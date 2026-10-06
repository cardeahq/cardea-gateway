package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	cliAnthropicKeyEnv = "AK_CLI_TEST_ANTHROPIC_KEY"
	regoHead           = "package cardea.policy\n\n"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// runServe starts `agentkeeper serve --config ...` in the background and
// returns once /healthz answers. The returned stop function cancels the
// command and reports what serve returned.
func runServe(t *testing.T, policyPath string) (base string, stop func() error) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv(cliAnthropicKeyEnv, "sk-test-upstream")

	addr := freeAddr(t)
	cfg := fmt.Sprintf(`server:
  listen_addr: %q
logging:
  level: error
provider:
  name: anthropic
  base_url: %q
  api_key_env: %q
pipeline:
  stages:
    identification: false
    tracing: false
    policies: true
`, addr, upstream.URL, cliAnthropicKeyEnv)
	if policyPath != "" {
		cfg += fmt.Sprintf("policies:\n  path: %q\n", policyPath)
	}
	cfgPath := filepath.Join(t.TempDir(), "agentkeeper.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	root := NewRootCmd()
	root.SetArgs([]string{"serve", "--config", cfgPath})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	base = "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("serve exited before listening: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve never started listening: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var stopped bool
	stop = func() error {
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			return fmt.Errorf("serve did not return after its context ended")
		}
	}
	t.Cleanup(func() {
		if !stopped {
			_ = stop()
		}
	})
	return base, stop
}

func postModel(t *testing.T, base, model string) int {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func sighup(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestServe_SIGHUPReloadsThePolicyAndDoesNotEndTheProcess(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.rego")
	write := func(rules string) {
		if err := os.WriteFile(policy, []byte(regoHead+rules), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`deny contains "no-a" if { input.model == "m-a" }`)
	base, stop := runServe(t, policy)
	time.Sleep(100 * time.Millisecond)

	if got := postModel(t, base, "m-a"); got != http.StatusForbidden {
		t.Fatalf("m-a status = %d before the reload, want 403", got)
	}
	if got := postModel(t, base, "m-b"); got != http.StatusOK {
		t.Fatalf("m-b status = %d before the reload, want 200", got)
	}

	write(`deny contains "no-b" if { input.model == "m-b" }`)
	// Editing alone changes nothing: policy files are read on reload only.
	if got := postModel(t, base, "m-b"); got != http.StatusOK {
		t.Fatalf("m-b status = %d after the edit but before SIGHUP, want 200", got)
	}

	sighup(t)
	eventually(t, "SIGHUP to apply the edited policy", func() bool { return postModel(t, base, "m-b") == http.StatusForbidden })
	if got := postModel(t, base, "m-a"); got != http.StatusOK {
		t.Fatalf("m-a status = %d after the reload, want 200", got)
	}

	// An invalid edit followed by SIGHUP keeps the previous policy and the process alive.
	write(regoHead + "deny contains \"x\" if { input.a == }\n")
	sighup(t)
	time.Sleep(300 * time.Millisecond)
	if got := postModel(t, base, "m-b"); got != http.StatusForbidden {
		t.Fatalf("m-b status = %d after a failed reload, want the previous policy still enforced", got)
	}

	if err := stop(); err != nil {
		t.Fatalf("serve returned %v after its context ended, want nil", err)
	}
}

func TestServe_SIGHUPWithNoPolicyDoesNotEndTheProcess(t *testing.T) {
	base, stop := runServe(t, "")
	time.Sleep(100 * time.Millisecond)

	sighup(t)
	time.Sleep(300 * time.Millisecond)

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("the gateway stopped answering after SIGHUP with no policy: %v", err)
	}
	_ = resp.Body.Close()
	if got := postModel(t, base, "any-model"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200: with no policy everything is allowed", got)
	}
	if err := stop(); err != nil {
		t.Fatalf("serve returned %v, want nil", err)
	}
}

func TestServe_ExitsBeforeListeningOnAnInvalidPolicy(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.rego")
	if err := os.WriteFile(bad, []byte(regoHead+"deny contains \"x\" if { input.a == }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	t.Setenv(cliAnthropicKeyEnv, "sk-test-upstream")
	addr := freeAddr(t)
	cfg := fmt.Sprintf("server:\n  listen_addr: %q\nlogging:\n  level: error\nprovider:\n  base_url: %q\n  api_key_env: %q\npipeline:\n  stages:\n    tracing: false\npolicies:\n  path: %q\n",
		addr, upstream.URL, cliAnthropicKeyEnv, bad)
	cfgPath := filepath.Join(dir, "agentkeeper.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"serve", "--config", cfgPath})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := root.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("serve with an invalid policy returned nil, want an error naming the file")
	}
	if !strings.Contains(err.Error(), "bad.rego") {
		t.Fatalf("error %q does not name the policy file", err)
	}
	if conn, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
		_ = conn.Close()
		t.Fatal("something is listening: serve must exit before listening")
	}
}
