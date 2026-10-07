package policy

import (
	"context"
	"os"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

const watchSettle = 150 * time.Millisecond

func rule(id string) string {
	return regoHeader + `deny contains "` + id + `" if { input.model == "m" }`
}

// startWatch loads an engine with watching on and runs Watch until the
// test ends. The returned logs hold every reload attempt.
func startWatch(t *testing.T, policyPath string, data ...string) (*Engine, *observer.ObservedLogs, context.CancelFunc, <-chan error) {
	t.Helper()
	logger, logs := observedLogger()
	cfg := cfgFor(policyPath, data...)
	cfg.Watch = true
	cfg.WatchSettle = watchSettle
	e, err := New(cfg, "anthropic", logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() { errCh <- e.Watch(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Watch did not return after its context ended")
		}
	})
	time.Sleep(300 * time.Millisecond) // let the watcher arm
	logs.TakeAll()
	return e, logs, cancel, errCh
}

// reloadAttempts counts log entries that report a reload triggered by a
// file change, at any level.
func reloadAttempts(logs *observer.ObservedLogs) int {
	n := 0
	for _, e := range logs.All() {
		for _, v := range e.ContextMap() {
			if s, ok := v.(string); ok && s == "watch" {
				n++
				break
			}
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatch_ReturnsImmediatelyWhenWatchIsOff(t *testing.T) {
	path := writeFile(t, t.TempDir(), "policy.rego", rule("a"))
	e, err := New(cfgFor(path), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Watch(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch with watch off = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch with watch off did not return immediately")
	}
}

func TestWatch_EndsWhenContextIsCancelled(t *testing.T) {
	path := writeFile(t, t.TempDir(), "policy.rego", rule("a"))
	_, _, cancel, errCh := startWatch(t, path)
	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not return after its context was cancelled")
	}
}

func TestWatch_PolicyChangeReloadsOnceAfterTheSettleTime(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", rule("v1"))
	e, logs, _, _ := startWatch(t, path)
	rev := e.Revision()

	writeFile(t, dir, "policy.rego", rule("v2"))
	waitFor(t, "the revision to change", func() bool { return e.Revision() != rev })
	time.Sleep(3 * watchSettle)

	if n := reloadAttempts(logs); n != 1 {
		t.Fatalf("%d reload attempts for one change, want exactly 1; logs:\n%s", n, allLogText(logs))
	}
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil || len(res.Rules) != 1 || res.Rules[0] != "v2" {
		t.Fatalf("rules=%v err=%v, want [v2]", res.Rules, err)
	}
}

func TestWatch_NothingHappensBeforeTheSettleTimeEnds(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", rule("v1"))
	e, _, _, _ := startWatch(t, path)
	rev := e.Revision()

	written := time.Now()
	writeFile(t, dir, "policy.rego", rule("v2"))
	time.Sleep(watchSettle / 3)
	// The reload timer starts after the write, so a reload seen less than
	// the settle time after `written` is early. A slow machine may overshoot
	// the sleep; a reload seen after the settle time is then legitimate.
	changed := e.Revision() != rev
	if elapsed := time.Since(written); changed && elapsed < watchSettle {
		t.Fatalf("the policy reloaded %v after the change, before the settle time (%v) elapsed", elapsed, watchSettle)
	}
	waitFor(t, "the reload after settling", func() bool { return e.Revision() != rev })
}

func TestWatch_ManyChangesWithinTheSettleTimeCauseOneReload(t *testing.T) {
	policyDir := t.TempDir()
	path := writeFile(t, policyDir, "policy.rego", regoHeader+`deny contains "r" if { input.model in data.blocked }`)
	dataDir := t.TempDir()
	writeFile(t, dataDir, "d0.json", `{"k0":0}`)
	e, logs, _, _ := startWatch(t, path, dataDir)
	rev := e.Revision()

	for i := 1; i <= 5; i++ {
		writeFile(t, dataDir, "d"+string(rune('0'+i))+".json", `{"k`+string(rune('0'+i))+`":1}`)
		time.Sleep(10 * time.Millisecond)
	}
	// "blocked" is absent from data: the policy stays valid, rules just never fire.
	waitFor(t, "the revision to change", func() bool { return e.Revision() != rev })
	time.Sleep(3 * watchSettle)

	if n := reloadAttempts(logs); n != 1 {
		t.Fatalf("%d reload attempts for five quick changes, want exactly 1; logs:\n%s", n, allLogText(logs))
	}
}

func TestWatch_NewFileInWatchedDataDirectoryReloads(t *testing.T) {
	policyDir := t.TempDir()
	path := writeFile(t, policyDir, "policy.rego", rule("r"))
	dataDir := t.TempDir()
	writeFile(t, dataDir, "a.json", `{"a":1}`)
	e, _, _, _ := startWatch(t, path, dataDir)
	rev := e.Revision()

	writeFile(t, dataDir, "nested/deeper/b.json", `{"b":1}`)
	waitFor(t, "a file added in a new subdirectory to be picked up", func() bool { return e.Revision() != rev })
}

func TestWatch_UnrelatedFileInThePolicyDirectoryDoesNotReload(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", rule("r"))
	_, logs, _, _ := startWatch(t, path)

	writeFile(t, dir, "notes.txt", "hello")
	time.Sleep(4 * watchSettle)
	if n := reloadAttempts(logs); n != 0 {
		t.Fatalf("%d reload attempts for a .txt file next to the policy, want 0", n)
	}
}

func TestWatch_SaveByRenameAndReplaceReloads(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", rule("v1"))
	e, _, _, _ := startWatch(t, path)

	for i, id := range []string{"v2", "v3"} { // twice: the watch must survive the first replace
		before := e.Revision()
		tmp := writeFile(t, dir, ".policy.rego.swp"+string(rune('0'+i)), rule(id))
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "a reload after rename-and-replace "+id, func() bool { return e.Revision() != before })
		res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
		if err != nil || len(res.Rules) != 1 || res.Rules[0] != id {
			t.Fatalf("after replace %s: rules=%v err=%v", id, res.Rules, err)
		}
	}
}

func TestWatch_InvalidChangeIsRejectedAndTheWatchContinues(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", rule("v1"))
	e, logs, _, _ := startWatch(t, path)
	rev := e.Revision()

	writeFile(t, dir, "policy.rego", regoHeader+"deny contains \"x\" if { input.a == }\n")
	waitFor(t, "the failed reload to be logged", func() bool { return reloadAttempts(logs) >= 1 })
	if e.Revision() != rev {
		t.Fatalf("Revision = %q after an invalid change, want the old %q", e.Revision(), rev)
	}
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil || len(res.Rules) != 1 || res.Rules[0] != "v1" {
		t.Fatalf("rules=%v err=%v, want the old policy [v1] still enforced", res.Rules, err)
	}

	writeFile(t, dir, "policy.rego", rule("v3"))
	waitFor(t, "the next valid change to apply", func() bool { return e.Revision() != rev })
	res, _ = e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if len(res.Rules) != 1 || res.Rules[0] != "v3" {
		t.Fatalf("rules=%v, want [v3]", res.Rules)
	}
}

func TestWatch_DeletedPolicyFailsTheReloadAndKeepsEnforcing(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", rule("v1"))
	e, logs, _, _ := startWatch(t, path)
	rev := e.Revision()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed reload to be logged", func() bool { return reloadAttempts(logs) >= 1 })
	if e.Revision() != rev {
		t.Fatalf("Revision = %q after deleting the policy, want %q", e.Revision(), rev)
	}
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil || len(res.Rules) != 1 {
		t.Fatalf("rules=%v err=%v, want enforcement to continue", res.Rules, err)
	}
}

func TestWatch_UnwatchablePathIsAnErrorNotAPanic(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", rule("v1"))
	cfg := config.PoliciesConfig{Path: path, Watch: true, WatchSettle: watchSettle, EvalTimeout: time.Second}
	e, err := New(cfg, "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	// The parent directory vanishes after load: the watcher cannot arm.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Watch(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Watch on a path that cannot be watched = nil, want an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not report that it could not watch")
	}
}
