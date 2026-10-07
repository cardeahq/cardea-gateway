package trace

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

// Compile-time checks that every sink the gateway holds is closeable.
var (
	_ CloseableSink = (*LogSink)(nil)
	_ CloseableSink = (*CardeaSink)(nil)
	_ CloseableSink = (*MultiSink)(nil)
)

const (
	csKeyEnv  = "CARDEA_SINK_TEST_API_KEY"
	csKey     = "sk-cardea-test-key-0123456789abcdef"
	csPath    = "/secret-ingest-path/v1/batches"
	csQuery   = "region=eu"
	csSentinl = "SENTINEL"
)

// --- fake platform ---------------------------------------------------------------------

type csEnvelope struct {
	Seq    uint64          `json:"seq"`
	Record json.RawMessage `json:"record"`
}

type csPlatformReply struct {
	status int
	header http.Header
	delay  time.Duration
	hang   bool // never answer, until the request is cancelled or the test ends
}

type csPlatformRequest struct {
	at        time.Time
	method    string
	uri       string
	header    http.Header
	raw       []byte
	plain     []byte
	lines     []string
	entries   []csEnvelope
	decodeErr error
}

type csPlatform struct {
	srv   *httptest.Server
	reply func(n int) csPlatformReply // n is the 1-based request number; nil means 202

	mu    sync.Mutex
	reqs  []csPlatformRequest
	block chan struct{}
	once  sync.Once
}

func newCSPlatform(t *testing.T, reply func(n int) csPlatformReply) *csPlatform {
	t.Helper()
	p := &csPlatform{reply: reply, block: make(chan struct{})}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	t.Cleanup(p.release) // runs first: unblocks hung handlers so srv.Close returns
	return p
}

func (p *csPlatform) release() { p.once.Do(func() { close(p.block) }) }

func (p *csPlatform) endpoint() string { return p.srv.URL + csPath + "?" + csQuery }

func (p *csPlatform) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	pr := csPlatformRequest{at: time.Now(), method: r.Method, uri: r.URL.RequestURI(), header: r.Header.Clone(), raw: raw}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		pr.decodeErr = err
	} else if plain, err := io.ReadAll(zr); err != nil {
		pr.decodeErr = err
	} else {
		pr.plain = plain
		if len(plain) > 0 {
			pr.lines = strings.Split(strings.TrimSuffix(string(plain), "\n"), "\n")
		}
		for _, l := range pr.lines {
			var e csEnvelope
			if err := json.Unmarshal([]byte(l), &e); err != nil {
				pr.decodeErr = err
			}
			pr.entries = append(pr.entries, e)
		}
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, pr)
	n := len(p.reqs)
	p.mu.Unlock()

	rep := csPlatformReply{status: http.StatusAccepted}
	if p.reply != nil {
		rep = p.reply(n)
	}
	if rep.hang {
		select {
		case <-p.block:
			rep = csPlatformReply{status: http.StatusAccepted} // the platform recovered
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			return
		}
	}
	if rep.delay > 0 {
		time.Sleep(rep.delay)
	}
	for k, vs := range rep.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rep.status)
}

func (p *csPlatform) requests() []csPlatformRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.reqs)
}

func (p *csPlatform) count() int { return len(p.requests()) }

// seqs returns every delivered seq in arrival order.
func (p *csPlatform) seqs() []uint64 {
	var out []uint64
	for _, r := range p.requests() {
		for _, e := range r.entries {
			out = append(out, e.Seq)
		}
	}
	return out
}

func (p *csPlatform) batchLens() []int {
	var out []int
	for _, r := range p.requests() {
		out = append(out, len(r.entries))
	}
	return out
}

func csWaitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *csPlatform) waitSeqs(t *testing.T, n int) {
	t.Helper()
	csWaitUntil(t, fmt.Sprintf("%d delivered records", n), 10*time.Second, func() bool { return len(p.seqs()) >= n })
}

// --- sink construction helpers -----------------------------------------------------------

func csConfig(endpoint string) config.CardeaSinkConfig {
	return config.CardeaSinkConfig{
		Endpoint:        endpoint,
		APIKeyEnv:       csKeyEnv,
		QueueSize:       1000,
		QueueMaxBytes:   64 << 20,
		BatchMaxRecords: 10,
		BatchMaxBytes:   1 << 20,
		BatchMaxDelay:   20 * time.Millisecond,
		RequestTimeout:  2 * time.Second,
		ShutdownTimeout: 500 * time.Millisecond,
	}
}

func csObserved() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// csNewSink builds a sink with the key set, short backoff, and a Close at
// the end of the test.
func csNewSink(t *testing.T, cfg config.CardeaSinkConfig, logger *zap.Logger) *CardeaSink {
	t.Helper()
	t.Setenv(csKeyEnv, csKey)
	s, err := NewCardeaSink(cfg, logger)
	if err != nil {
		t.Fatalf("NewCardeaSink: %v", err)
	}
	s.backoffBase = 5 * time.Millisecond
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func csWrite(t *testing.T, s *CardeaSink, recs ...Record) {
	t.Helper()
	for i, r := range recs {
		if err := s.Write(r); err != nil {
			t.Fatalf("Write #%d: %v", i+1, err)
		}
	}
}

func csCallID(i int) string { return fmt.Sprintf("ak_%016x", i) }

// csEnvelopeLine is the line the platform must receive for seq and rec.
func csEnvelopeLine(t *testing.T, seq int, rec Record) string {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return fmt.Sprintf(`{"seq":%d,"record":%s}`, seq, b)
}

// csSized returns a request record whose envelope line, newline included,
// is exactly entryBytes long for a one-digit seq.
func csSized(t *testing.T, callID string, entryBytes int) RequestRecord {
	t.Helper()
	r := minimalRequestRecord(callID)
	empty := ""
	r.Model = &empty
	base := len(csEnvelopeLine(t, 1, r)) + 1
	if entryBytes < base {
		t.Fatalf("cannot build a %d-byte entry: the smallest is %d", entryBytes, base)
	}
	m := strings.Repeat("m", entryBytes-base)
	r.Model = &m
	return r
}

func csSizedN(t *testing.T, n, entryBytes int) []Record {
	t.Helper()
	out := make([]Record, n)
	for i := range out {
		out[i] = csSized(t, csCallID(i+1), entryBytes)
	}
	return out
}

// csSentinelRecord carries a sentinel in every prompt-adjacent field.
func csSentinelRecord(i int) RequestRecord {
	r := minimalRequestRecord(csCallID(i))
	agent, model, reason, session := csSentinl+"-AGENT", csSentinl+"-MODEL", csSentinl+"-REASON", csSentinl+"-SESSION"
	fp := csSentinl + "-FINGERPRINT"
	r.AgentID = &agent
	r.Model = &model
	r.Outcome.Reason = &reason
	r.Session = Session{ID: &session, Status: "claimed"}
	r.ToolResults = []ToolResult{{ToolCallID: csSentinl + "-TOOLCALL", Fingerprint: &fp}}
	return r
}

// csFailingRecord cannot be encoded.
type csFailingRecord struct{ RequestRecord }

func (csFailingRecord) MarshalJSON() ([]byte, error) { return nil, errors.New("cannot encode") }

// --- log helpers -------------------------------------------------------------------------

func csEntryText(e observer.LoggedEntry) string {
	return fmt.Sprintf("%s %v", e.Message, e.ContextMap())
}

func csLogText(logs *observer.ObservedLogs) string {
	var b strings.Builder
	for _, e := range logs.All() {
		fmt.Fprintf(&b, "%s: %s\n", e.Level, csEntryText(e))
	}
	return b.String()
}

func csIntFields(e observer.LoggedEntry) []int64 {
	var out []int64
	for _, f := range e.Context {
		switch f.Type {
		case zapcore.Int64Type, zapcore.Int32Type, zapcore.Int16Type, zapcore.Int8Type,
			zapcore.Uint64Type, zapcore.Uint32Type, zapcore.Uint16Type, zapcore.Uint8Type:
			out = append(out, f.Integer)
		}
	}
	return out
}

// csCanSumTo reports whether the entries can be read as reporting n records
// in total, each entry contributing at most one of its integer fields. The
// field names are the implementation's choice (the spec fixes only that
// counts are logged), and a rate limiter may split a count across lines.
func csCanSumTo(entries []observer.LoggedEntry, n int64) bool {
	reach := map[int64]bool{0: true}
	for _, e := range entries {
		next := maps2clone(reach)
		for r := range reach {
			for _, v := range csIntFields(e) {
				next[r+v] = true
			}
		}
		reach = next
	}
	return n > 0 && reach[n]
}

func maps2clone(m map[int64]bool) map[int64]bool {
	out := make(map[int64]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func csErrors(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterLevelExact(zapcore.ErrorLevel).All()
}

func csAssertNoLeak(t *testing.T, logs *observer.ObservedLogs, forbidden ...string) {
	t.Helper()
	for _, e := range logs.All() {
		text := csEntryText(e)
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Fatalf("log entry contains %q: %s: %s", f, e.Level, text)
			}
		}
	}
}

// csDefaultForbidden is what no log line may ever contain.
var csDefaultForbidden = []string{csKey, "Bearer", "secret-ingest-path", csQuery, csSentinl}

// --- delivery ---------------------------------------------------------------------------

func TestCardeaSink_DeliversBothRecordTypesAsEnvelopeLines(t *testing.T) {
	p := newCSPlatform(t, nil)
	s := csNewSink(t, csConfig(p.endpoint()), zap.NewNop())

	req := minimalRequestRecord(csCallID(1))
	resp := minimalResponseRecord(csCallID(1))
	csWrite(t, s, req, resp)
	p.waitSeqs(t, 2)

	var got []string
	for _, r := range p.requests() {
		if r.decodeErr != nil {
			t.Fatalf("request body is not gzip of NDJSON envelope lines: %v", r.decodeErr)
		}
		if !bytes.HasSuffix(r.plain, []byte("\n")) {
			t.Fatalf("body does not end with a newline: %q", r.plain)
		}
		got = append(got, r.lines...)
	}
	want := []string{csEnvelopeLine(t, 1, req), csEnvelopeLine(t, 2, resp)}
	if !slices.Equal(got, want) {
		t.Fatalf("lines =\n%s\nwant (record bytes equal json.Marshal of the record, seq from 1, nothing else added)\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCardeaSink_RequestCarriesTheDeliveryContract(t *testing.T) {
	p := newCSPlatform(t, nil)
	s := csNewSink(t, csConfig(p.endpoint()), zap.NewNop())
	csWrite(t, s, csSizedN(t, 25, 600)...)
	p.waitSeqs(t, 25)

	instance := regexp.MustCompile(`^[0-9a-f]{32}$`)
	if !instance.MatchString(s.InstanceID()) {
		t.Fatalf("InstanceID() = %q, want 32 hex characters", s.InstanceID())
	}
	next := uint64(1)
	for i, r := range p.requests() {
		if r.method != http.MethodPost {
			t.Fatalf("request %d method = %s, want POST", i, r.method)
		}
		if want := csPath + "?" + csQuery; r.uri != want {
			t.Fatalf("request %d URI = %q, want the configured endpoint as given: %q", i, r.uri, want)
		}
		checks := map[string]string{
			"Authorization":     "Bearer " + csKey,
			"Content-Type":      "application/x-ndjson",
			"Content-Encoding":  "gzip",
			"X-Cardea-Instance": s.InstanceID(),
			"X-Cardea-Schema":   "cardea-gateway.trace/v1",
			"User-Agent":        "cardea-gateway",
		}
		for h, want := range checks {
			if got := r.header.Get(h); got != want {
				t.Fatalf("request %d header %s = %q, want %q", i, h, got, want)
			}
		}
		if len(r.entries) == 0 || r.decodeErr != nil {
			t.Fatalf("request %d has no decodable entries: %v", i, r.decodeErr)
		}
		first, last := r.entries[0].Seq, r.entries[len(r.entries)-1].Seq
		if got := r.header.Get("X-Cardea-First-Seq"); got != strconv.FormatUint(first, 10) {
			t.Fatalf("request %d First-Seq = %q, want %d", i, got, first)
		}
		if got := r.header.Get("X-Cardea-Last-Seq"); got != strconv.FormatUint(last, 10) {
			t.Fatalf("request %d Last-Seq = %q, want %d", i, got, last)
		}
		for _, e := range r.entries {
			if e.Seq != next {
				t.Fatalf("request %d: seq %d, want %d (increasing, no gap, starting at 1)", i, e.Seq, next)
			}
			next++
		}
		if bytes.Contains(r.plain, []byte(csKey)) {
			t.Fatalf("request %d body contains the API key", i)
		}
		for name := range r.header {
			if strings.Contains(strings.ToLower(name), "tenant") {
				t.Fatalf("request %d sends a header naming a tenant: %s", i, name)
			}
		}
	}
}

func TestCardeaSink_NoTenantInAnyDeliveredLine(t *testing.T) {
	p := newCSPlatform(t, nil)
	s := csNewSink(t, csConfig(p.endpoint()), zap.NewNop())
	csWrite(t, s, minimalRequestRecord(csCallID(1)), minimalResponseRecord(csCallID(1)))
	p.waitSeqs(t, 2)

	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				if strings.Contains(strings.ToLower(k), "tenant") {
					t.Fatalf("delivered JSON has a key naming a tenant: %q", k)
				}
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	for _, r := range p.requests() {
		for _, l := range r.lines {
			var v any
			if err := json.Unmarshal([]byte(l), &v); err != nil {
				t.Fatalf("line is not JSON: %v", err)
			}
			walk(v)
		}
	}
}

func TestCardeaSink_ConcurrentWritersKeepSeqOrderWithoutGaps(t *testing.T) {
	p := newCSPlatform(t, nil)
	cfg := csConfig(p.endpoint())
	cfg.BatchMaxRecords = 50
	cfg.ShutdownTimeout = 5 * time.Second
	s := csNewSink(t, cfg, zap.NewNop())

	const writers, per = 8, 50
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range per {
				if err := s.Write(minimalRequestRecord(csCallID(w*per + i))); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := p.seqs()
	if len(got) != writers*per {
		t.Fatalf("delivered %d records, want %d", len(got), writers*per)
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("arrival %d has seq %d, want %d: seq order must equal queue order", i, seq, i+1)
		}
	}
}

// --- batching ---------------------------------------------------------------------------

func TestCardeaSink_BatchingThresholds(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(c *config.CardeaSinkConfig)
		writes          int
		entryBytes      int
		wantBeforeClose []int // batch sizes sent while the sink is open
		wantAfterClose  []int // batch sizes once Close has drained
	}{
		{
			name:            "record count sends full batches and holds the remainder",
			mutate:          func(c *config.CardeaSinkConfig) { c.BatchMaxRecords = 3; c.BatchMaxDelay = time.Hour },
			writes:          7,
			entryBytes:      600,
			wantBeforeClose: []int{3, 3},
			wantAfterClose:  []int{3, 3, 1},
		},
		{
			name:            "batch of one record sends every record on its own",
			mutate:          func(c *config.CardeaSinkConfig) { c.BatchMaxRecords = 1; c.BatchMaxDelay = time.Hour },
			writes:          4,
			entryBytes:      600,
			wantBeforeClose: []int{1, 1, 1, 1},
			wantAfterClose:  []int{1, 1, 1, 1},
		},
		{
			name: "byte size sends what fits and holds the rest",
			mutate: func(c *config.CardeaSinkConfig) {
				c.BatchMaxRecords = 500
				c.BatchMaxBytes = 1500
				c.BatchMaxDelay = time.Hour
			},
			writes:          3,
			entryBytes:      600,
			wantBeforeClose: []int{2},
			wantAfterClose:  []int{2, 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newCSPlatform(t, nil)
			cfg := csConfig(p.endpoint())
			tt.mutate(&cfg)
			s := csNewSink(t, cfg, zap.NewNop())
			csWrite(t, s, csSizedN(t, tt.writes, tt.entryBytes)...)

			want := 0
			for _, n := range tt.wantBeforeClose {
				want += n
			}
			p.waitSeqs(t, want)
			time.Sleep(150 * time.Millisecond)
			if got := p.batchLens(); !slices.Equal(got, tt.wantBeforeClose) {
				t.Fatalf("batches before Close = %v, want %v", got, tt.wantBeforeClose)
			}

			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if got := p.batchLens(); !slices.Equal(got, tt.wantAfterClose) {
				t.Fatalf("batches after Close = %v, want %v (Close drains without waiting for batch_max_delay)", got, tt.wantAfterClose)
			}
		})
	}
}

func TestCardeaSink_BatchDelaySendsAPartialBatchNotBeforeTheDelay(t *testing.T) {
	p := newCSPlatform(t, nil)
	cfg := csConfig(p.endpoint())
	cfg.BatchMaxRecords = 100
	cfg.BatchMaxDelay = 300 * time.Millisecond
	s := csNewSink(t, cfg, zap.NewNop())

	start := time.Now()
	csWrite(t, s, csSizedN(t, 2, 600)...)
	time.Sleep(150 * time.Millisecond)
	if n := p.count(); n != 0 {
		t.Fatalf("%d requests after 150ms, want none before batch_max_delay (300ms)", n)
	}
	p.waitSeqs(t, 2)
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("delivered after %v, before batch_max_delay", elapsed)
	}
	if got := p.batchLens(); !slices.Equal(got, []int{2}) {
		t.Fatalf("batches = %v, want one batch of both records", got)
	}
}

func TestCardeaSink_BatchSizeLimitAndOversizedRecord(t *testing.T) {
	t.Run("600-byte records under a 1500-byte batch size never exceed two per request", func(t *testing.T) {
		p := newCSPlatform(t, nil)
		cfg := csConfig(p.endpoint())
		cfg.BatchMaxRecords = 500
		cfg.BatchMaxBytes = 1500
		s := csNewSink(t, cfg, zap.NewNop())
		csWrite(t, s, csSizedN(t, 5, 600)...)
		p.waitSeqs(t, 5)

		for i, r := range p.requests() {
			if len(r.entries) > 2 {
				t.Fatalf("request %d carries %d records, want at most 2", i, len(r.entries))
			}
			if len(r.plain) > 1500 {
				t.Fatalf("request %d body is %d bytes uncompressed, want at most 1500", i, len(r.plain))
			}
		}
		if got := p.seqs(); !slices.Equal(got, []uint64{1, 2, 3, 4, 5}) {
			t.Fatalf("delivered seqs = %v, want 1..5", got)
		}
	})

	t.Run("one 4500-byte record (three times the batch size) is sent alone", func(t *testing.T) {
		p := newCSPlatform(t, nil)
		cfg := csConfig(p.endpoint())
		cfg.BatchMaxRecords = 500
		cfg.BatchMaxBytes = 1500
		s := csNewSink(t, cfg, zap.NewNop())
		csWrite(t, s, csSized(t, csCallID(1), 600), csSized(t, csCallID(2), 4500), csSized(t, csCallID(3), 600))
		p.waitSeqs(t, 3)

		for i, r := range p.requests() {
			for _, e := range r.entries {
				if e.Seq == 2 && len(r.entries) != 1 {
					t.Fatalf("request %d: the oversized record travels with %d records, want alone", i, len(r.entries))
				}
			}
		}
		if got := p.seqs(); !slices.Equal(got, []uint64{1, 2, 3}) {
			t.Fatalf("delivered seqs = %v, want 1..3", got)
		}
	})
}

// --- classification, backoff, Retry-After -------------------------------------------------

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int
		want     verdict
	}{
		{"2xx is delivered", []int{200, 201, 202, 204, 299}, delivered},
		{"401 and 403 refuse the credential", []int{401, 403}, refusedCredential},
		{"400, 413 and 422 refuse the batch", []int{400, 413, 422}, refusedBatch},
		{"other 4xx refuse the batch", []int{404, 405, 409, 410, 418, 451}, refusedBatch},
		{"3xx refuse the batch (redirects are never followed)", []int{300, 301, 302, 303, 307, 308}, refusedBatch},
		{"408 and 429 retry", []int{408, 429}, retry},
		{"5xx retry", []int{500, 502, 503, 504, 599}, retry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, status := range tt.statuses {
				if got := classify(status); got != tt.want {
					t.Errorf("classify(%d) = %v, want %v", status, got, tt.want)
				}
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	h := func(v string) http.Header { return http.Header{"Retry-After": {v}} }

	t.Run("delta-seconds", func(t *testing.T) {
		d, ok := retryAfter(h("5"), now)
		if !ok || d != 5*time.Second {
			t.Fatalf("retryAfter(5) = %v, %v; want 5s, true", d, ok)
		}
	})
	t.Run("HTTP-date", func(t *testing.T) {
		d, ok := retryAfter(h(now.Add(10*time.Second).Format(http.TimeFormat)), now)
		if !ok || d != 10*time.Second {
			t.Fatalf("retryAfter(date now+10s) = %v, %v; want 10s, true", d, ok)
		}
	})
	t.Run("HTTP-date in the past asks for no wait", func(t *testing.T) {
		d, _ := retryAfter(h(now.Add(-time.Minute).Format(http.TimeFormat)), now)
		if d > 0 {
			t.Fatalf("retryAfter(past date) = %v, want no wait", d)
		}
	})
	t.Run("absent or unparseable is not an instruction", func(t *testing.T) {
		for _, hdr := range []http.Header{{}, h(""), h("soon"), h("1.5x")} {
			if d, ok := retryAfter(hdr, now); ok && d > 0 {
				t.Fatalf("retryAfter(%v) = %v, true; want ok=false", hdr, d)
			}
		}
	})
	t.Run("negative seconds never wait", func(t *testing.T) {
		if d, ok := retryAfter(h("-5"), now); ok && d > 0 {
			t.Fatalf("retryAfter(-5) = %v, true", d)
		}
	})
}

func TestBackoff_FloorFromRetryAfterIsHonoredAndCappedAtFiveMinutes(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		retryAfter string
		wantMin    time.Duration
		wantMax    time.Duration
	}{
		{"Retry-After 5 waits at least 5 seconds", "5", 5 * time.Second, 6 * time.Second},
		{"Retry-After 3600 waits 5 minutes, not an hour", "3600", 5 * time.Minute, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			floor, ok := retryAfter(http.Header{"Retry-After": {tt.retryAfter}}, now)
			if !ok {
				t.Fatalf("retryAfter(%q) not ok", tt.retryAfter)
			}
			for range 50 {
				if got := backoff(0, floor); got < tt.wantMin || got > tt.wantMax {
					t.Fatalf("backoff(0, retryAfter(%q)) = %v, want within [%v, %v]", tt.retryAfter, got, tt.wantMin, tt.wantMax)
				}
			}
		})
	}
}

func TestBackoff_ScheduleAndJitter(t *testing.T) {
	for attempt := range 12 {
		sched := min(time.Second<<attempt, 30*time.Second)
		seen := map[time.Duration]bool{}
		for range 300 {
			d := backoff(attempt, 0)
			if d < sched/2 || d > sched {
				t.Fatalf("backoff(%d) = %v, want within [%v, %v] (1s * 2^n capped at 30s, times a factor in [0.5, 1])", attempt, d, sched/2, sched)
			}
			seen[d] = true
		}
		if len(seen) < 2 {
			t.Fatalf("backoff(%d) returned one value 300 times, want random jitter", attempt)
		}
	}
	for _, attempt := range []int{40, 64, 1000} {
		if d := backoff(attempt, 0); d < 15*time.Second || d > 30*time.Second {
			t.Fatalf("backoff(%d) = %v, want the capped schedule [15s, 30s] without overflow", attempt, d)
		}
	}
}

type csTimeoutErr struct{}

func (csTimeoutErr) Error() string {
	return "i/o timeout for https://platform.example.com/secret-ingest-path"
}
func (csTimeoutErr) Timeout() bool   { return true }
func (csTimeoutErr) Temporary() bool { return true }

func TestReason_MapsTransportErrorsToAFixedVocabularyWithoutTheURL(t *testing.T) {
	const u = "https://platform.example.com/secret-ingest-path?region=eu"
	wrap := func(err error) error { return &url.Error{Op: "Post", URL: u, Err: err} }
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"deadline exceeded", wrap(context.DeadlineExceeded), "timeout"},
		{"network timeout", wrap(csTimeoutErr{}), "timeout"},
		{"connection refused", wrap(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), "connection refused"},
		{"dns failure", wrap(&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "platform.example.com", IsNotFound: true}}), "dns"},
		{"tls verification", wrap(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), "tls"},
		{"canceled", wrap(context.Canceled), "canceled"},
		{"anything else", wrap(errors.New("something odd at " + u)), "network error"},
		{"not a url error", errors.New("plain"), "network error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reason(tt.err)
			if got != tt.want {
				t.Fatalf("reason(%v) = %q, want %q", tt.err, got, tt.want)
			}
			if strings.Contains(got, "platform.example.com") || strings.Contains(got, "secret") {
				t.Fatalf("reason leaks the URL: %q", got)
			}
		})
	}
}

// --- retry ------------------------------------------------------------------------------

func TestCardeaSink_Retry503ThenAcceptedDeliversOnceWithNoMissingSeq(t *testing.T) {
	p := newCSPlatform(t, func(n int) csPlatformReply {
		if n == 1 {
			return csPlatformReply{status: 503}
		}
		return csPlatformReply{status: 202}
	})
	s := csNewSink(t, csConfig(p.endpoint()), zap.NewNop())
	csWrite(t, s, csSizedN(t, 3, 600)...)

	csWaitUntil(t, "two requests", 10*time.Second, func() bool { return p.count() >= 2 })
	time.Sleep(150 * time.Millisecond)
	reqs := p.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2 (one refused, one retry that succeeds)", len(reqs))
	}
	if !bytes.Equal(reqs[0].plain, reqs[1].plain) {
		t.Fatalf("the retry carries a different body:\n%s\nvs\n%s", reqs[0].plain, reqs[1].plain)
	}
	if got := p.seqs(); !slices.Equal(got[len(reqs[0].entries):], []uint64{1, 2, 3}) {
		t.Fatalf("delivered seqs after the failure = %v, want 1, 2, 3 once", got)
	}
}

func TestCardeaSink_RetriedHeadBatchIsNeverOvertaken(t *testing.T) {
	p := newCSPlatform(t, func(n int) csPlatformReply {
		if n <= 2 {
			return csPlatformReply{status: 503}
		}
		return csPlatformReply{status: 202}
	})
	cfg := csConfig(p.endpoint())
	cfg.BatchMaxRecords = 1
	s := csNewSink(t, cfg, zap.NewNop())
	csWrite(t, s, csSizedN(t, 3, 600)...)

	csWaitUntil(t, "five requests", 10*time.Second, func() bool { return p.count() >= 5 })
	var first []string
	for _, r := range p.requests() {
		first = append(first, r.header.Get("X-Cardea-First-Seq"))
	}
	if want := []string{"1", "1", "1", "2", "3"}; !slices.Equal(first, want) {
		t.Fatalf("First-Seq of successive requests = %v, want %v: nothing is reordered behind a batch being retried", first, want)
	}
}

func TestCardeaSink_FirstRetryWaitsWithinTheJitteredBackoff(t *testing.T) {
	p := newCSPlatform(t, func(n int) csPlatformReply {
		if n == 1 {
			return csPlatformReply{status: 500}
		}
		return csPlatformReply{status: 202}
	})
	s := csNewSink(t, csConfig(p.endpoint()), zap.NewNop())
	s.backoffBase = 200 * time.Millisecond
	csWrite(t, s, csSizedN(t, 1, 600)...)

	csWaitUntil(t, "two requests", 10*time.Second, func() bool { return p.count() >= 2 })
	reqs := p.requests()
	gap := reqs[1].at.Sub(reqs[0].at)
	if gap < 90*time.Millisecond || gap > 200*time.Millisecond+time.Second {
		t.Fatalf("retry after %v, want about backoffBase * [0.5, 1] = [100ms, 200ms]", gap)
	}
}

func TestCardeaSink_429WithRetryAfterIsNotRetriedEarlier(t *testing.T) {
	p := newCSPlatform(t, func(n int) csPlatformReply {
		if n == 1 {
			return csPlatformReply{status: 429, header: http.Header{"Retry-After": {"1"}}}
		}
		return csPlatformReply{status: 202}
	})
	s := csNewSink(t, csConfig(p.endpoint()), zap.NewNop()) // backoffBase is 5ms: only Retry-After explains a long wait
	csWrite(t, s, csSizedN(t, 1, 600)...)

	csWaitUntil(t, "two requests", 10*time.Second, func() bool { return p.count() >= 2 })
	reqs := p.requests()
	if gap := reqs[1].at.Sub(reqs[0].at); gap < 950*time.Millisecond {
		t.Fatalf("retried after %v, want at least the 1s the platform asked for", gap)
	}
}

func TestCardeaSink_TimedOutDeliveryIsRetried(t *testing.T) {
	p := newCSPlatform(t, func(n int) csPlatformReply {
		if n == 1 {
			return csPlatformReply{hang: true}
		}
		return csPlatformReply{status: 202}
	})
	logger, logs := csObserved()
	cfg := csConfig(p.endpoint())
	cfg.RequestTimeout = 150 * time.Millisecond
	s := csNewSink(t, cfg, logger)
	csWrite(t, s, csSizedN(t, 1, 600)...)

	csWaitUntil(t, "retry after timeout", 10*time.Second, func() bool { return p.count() >= 2 })
	csWaitUntil(t, "a warning", 5*time.Second, func() bool { return logs.FilterLevelExact(zapcore.WarnLevel).Len() >= 1 })
	warn := logs.FilterLevelExact(zapcore.WarnLevel).All()[0]
	if !strings.Contains(csEntryText(warn), "timeout") {
		t.Fatalf("warning does not give the reason (timeout): %s", csEntryText(warn))
	}
	csAssertNoLeak(t, logs, csDefaultForbidden...)
}

func TestCardeaSink_RetryFailuresWarnOnceThenInfoOnRecovery(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	p := newCSPlatform(t, func(n int) csPlatformReply {
		if fail.Load() {
			return csPlatformReply{status: 503}
		}
		return csPlatformReply{status: 202}
	})
	logger, logs := csObserved()
	s := csNewSink(t, csConfig(p.endpoint()), logger)
	csWrite(t, s, csSizedN(t, 1, 600)...)

	csWaitUntil(t, "many failed attempts", 10*time.Second, func() bool { return p.count() >= 5 })
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("%d warnings over %d failed attempts, want one (the next is due a minute later):\n%s", len(warns), p.count(), csLogText(logs))
	}
	if !strings.Contains(csEntryText(warns[0]), "503") {
		t.Fatalf("warning does not give the status: %s", csEntryText(warns[0]))
	}

	infosBefore := logs.FilterLevelExact(zapcore.InfoLevel).Len()
	fail.Store(false)
	p.waitSeqs(t, 1)
	csWaitUntil(t, "an info line on recovery", 5*time.Second, func() bool {
		return logs.FilterLevelExact(zapcore.InfoLevel).Len() > infosBefore
	})
	csAssertNoLeak(t, logs, csDefaultForbidden...)
}

func TestCardeaSink_UnreachablePlatformWarnsWithAFixedReasonAndNoURL(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens here any more

	logger, logs := csObserved()
	s := csNewSink(t, csConfig("http://"+addr+csPath+"?"+csQuery), logger)
	csWrite(t, s, csSizedN(t, 1, 600)...)

	csWaitUntil(t, "a warning", 10*time.Second, func() bool { return logs.FilterLevelExact(zapcore.WarnLevel).Len() >= 1 })
	time.Sleep(300 * time.Millisecond)
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("%d warnings, want one at the first failure", len(warns))
	}
	if !strings.Contains(csEntryText(warns[0]), "connection refused") {
		t.Fatalf("warning = %s, want the fixed reason \"connection refused\"", csEntryText(warns[0]))
	}
	csAssertNoLeak(t, logs, csDefaultForbidden...)
}

// --- refusals ---------------------------------------------------------------------------

func TestCardeaSink_RefusedBatchIsDroppedAndTheNextOneIsSent(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantInText string
	}{
		{"401 refuses the credential", 401, "credential"},
		{"403 refuses the credential", 403, "credential"},
		{"400 refuses the batch", 400, "400"},
		{"413 refuses the batch", 413, "413"},
		{"422 refuses the batch", 422, "422"},
		{"other 4xx refuses the batch", 404, "404"},
		{"a redirect status refuses the batch", 302, "302"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newCSPlatform(t, func(n int) csPlatformReply {
				if n == 1 {
					return csPlatformReply{status: tt.status}
				}
				return csPlatformReply{status: 202}
			})
			logger, logs := csObserved()
			cfg := csConfig(p.endpoint())
			cfg.BatchMaxRecords = 1
			s := csNewSink(t, cfg, logger)
			csWrite(t, s, csSentinelRecord(1), csSentinelRecord(2))

			p.waitSeqs(t, 2)
			time.Sleep(150 * time.Millisecond)
			if got := p.seqs(); !slices.Equal(got, []uint64{1, 2}) {
				t.Fatalf("seqs received = %v, want [1 2]: the refused batch is sent once and never again, the next batch follows", got)
			}

			errs := csErrors(logs)
			if len(errs) == 0 {
				t.Fatalf("no error-level line for a dropped batch:\n%s", csLogText(logs))
			}
			if !strings.Contains(strings.ToLower(csEntryText(errs[0])), tt.wantInText) {
				t.Fatalf("error line %q does not mention %q", csEntryText(errs[0]), tt.wantInText)
			}
			if !csCanSumTo(errs, 1) {
				t.Fatalf("error lines do not report the 1 record dropped:\n%s", csLogText(logs))
			}
			csAssertNoLeak(t, logs, csDefaultForbidden...)
		})
	}
}

func TestCardeaSink_CloseReportsDeliveredAndLostTotals(t *testing.T) {
	statuses := []int{400, 202, 202, 401, 202} // 2 batches lost, 3 delivered
	p := newCSPlatform(t, func(n int) csPlatformReply { return csPlatformReply{status: statuses[n-1]} })
	logger, logs := csObserved()
	cfg := csConfig(p.endpoint())
	cfg.BatchMaxRecords = 1
	s := csNewSink(t, cfg, logger)
	csWrite(t, s, csSizedN(t, 5, 600)...)
	csWaitUntil(t, "five requests", 10*time.Second, func() bool { return p.count() >= 5 })
	time.Sleep(100 * time.Millisecond)

	before := logs.Len()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var found bool
	for _, e := range logs.All()[before:] {
		if e.Level != zapcore.InfoLevel {
			continue
		}
		ints := csIntFields(e)
		if slices.Contains(ints, 3) && slices.Contains(ints, 2) {
			found = true
		}
	}
	if !found {
		t.Fatalf("Close logged no info line carrying delivered=3 and lost=2:\n%s", csLogText(logs))
	}
}

func TestCardeaSink_RedirectIsNotFollowedAndTheKeyNeverLeavesTheEndpoint(t *testing.T) {
	other := newCSPlatform(t, nil)
	p := newCSPlatform(t, func(n int) csPlatformReply {
		if n == 1 {
			return csPlatformReply{status: http.StatusTemporaryRedirect, header: http.Header{"Location": {other.srv.URL + "/stolen"}}}
		}
		return csPlatformReply{status: 202}
	})
	logger, logs := csObserved()
	cfg := csConfig(p.endpoint())
	cfg.BatchMaxRecords = 1
	s := csNewSink(t, cfg, logger)
	csWrite(t, s, csSizedN(t, 2, 600)...)

	p.waitSeqs(t, 2)
	time.Sleep(200 * time.Millisecond)
	if n := other.count(); n != 0 {
		t.Fatalf("the redirect target received %d requests, want none", n)
	}
	if got := p.seqs(); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("seqs = %v, want [1 2]: the redirected batch is dropped, not resent", got)
	}
	if len(csErrors(logs)) == 0 {
		t.Fatal("no error-level line for the batch dropped as refused")
	}
}

func TestCardeaSink_PanicWhilePreparingABatchDropsOnlyThatBatch(t *testing.T) {
	p := newCSPlatform(t, nil)
	logger, logs := csObserved()
	cfg := csConfig(p.endpoint())
	cfg.BatchMaxRecords = 1
	s := csNewSink(t, cfg, logger)
	csPanicOnFirstSend(t, s)

	csWrite(t, s, csSentinelRecord(1))
	csWaitUntil(t, "an error line for the dropped batch", 5*time.Second, func() bool { return len(csErrors(logs)) > 0 })
	csWrite(t, s, csSentinelRecord(2))
	p.waitSeqs(t, 1)

	time.Sleep(100 * time.Millisecond)
	if got := p.seqs(); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("delivered seqs = %v, want [2]: the batch in hand when the panic hit is dropped and counted, the next one is delivered", got)
	}
	if !csCanSumTo(csErrors(logs), 1) {
		t.Fatalf("error lines do not report the 1 record lost:\n%s", csLogText(logs))
	}
	csAssertNoLeak(t, logs, csDefaultForbidden...)
}

// csPanicOnFirstSend replaces the sendBatch seam so its first call panics
// and every later call goes to the original. It works through reflection so
// that the test does not depend on the seam's exact signature.
func csPanicOnFirstSend(t *testing.T, s *CardeaSink) {
	t.Helper()
	f := reflect.ValueOf(s).Elem().FieldByName("sendBatch")
	if !f.IsValid() || f.Kind() != reflect.Func {
		t.Fatal("CardeaSink must have a func-typed sendBatch field (the spec's test seam)")
	}
	slot := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	if slot.IsNil() {
		t.Fatal("sendBatch must default to the real sender so a test can wrap it")
	}
	orig := reflect.ValueOf(slot.Interface())
	var calls atomic.Int32
	slot.Set(reflect.MakeFunc(f.Type(), func(args []reflect.Value) []reflect.Value {
		if calls.Add(1) == 1 {
			panic("injected failure while sending")
		}
		return orig.Call(args)
	}))
}

// --- capacity ---------------------------------------------------------------------------

func TestCardeaSink_CapacityDropsNewRecordsAndLeavesAGapOfExactlyTheDropped(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *config.CardeaSinkConfig)
	}{
		{
			name:   "queue_size of 2 records",
			mutate: func(c *config.CardeaSinkConfig) { c.QueueSize = 2 },
		},
		{
			name: "queue_max_bytes smaller than three entries",
			mutate: func(c *config.CardeaSinkConfig) {
				c.BatchMaxBytes = 1500
				c.QueueMaxBytes = 1500 // two 600-byte entries fit, a third does not
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var recovered atomic.Bool
			p := newCSPlatform(t, func(n int) csPlatformReply {
				if recovered.Load() {
					return csPlatformReply{status: 202}
				}
				return csPlatformReply{hang: true}
			})
			logger, logs := csObserved()
			cfg := csConfig(p.endpoint())
			tt.mutate(&cfg)
			cfg.ShutdownTimeout = time.Second
			s := csNewSink(t, cfg, logger)

			// The platform does not answer, so nothing leaves the queue.
			for i, r := range csSizedN(t, 5, 600) {
				if err := s.Write(r); err != nil {
					t.Fatalf("Write #%d = %v, want nil: a full queue discards, it never rejects", i+1, err)
				}
			}
			csWaitUntil(t, "an error line for the dropped records", 5*time.Second, func() bool { return len(csErrors(logs)) > 0 })

			recovered.Store(true)
			p.release()
			p.waitSeqs(t, 2)
			// The platform records a request on arrival; the sink frees its
			// capacity only once the reply is in, so wait for the queue to drain.
			csWaitUntil(t, "the delivered records to leave the queue", 5*time.Second, func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return len(s.queue) == 0
			})
			csWrite(t, s, csSized(t, csCallID(6), 600))
			p.waitSeqs(t, 3)
			time.Sleep(100 * time.Millisecond)

			if got := p.seqs(); !slices.Equal(got, []uint64{1, 2, 6}) {
				t.Fatalf("delivered seqs = %v, want [1 2 6]: seqs 3, 4 and 5 were dropped and consumed, a gap of exactly 3", got)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !csCanSumTo(csErrors(logs), 3) {
				t.Fatalf("error lines do not report the 3 records dropped:\n%s", csLogText(logs))
			}
			csAssertNoLeak(t, logs, csDefaultForbidden...)
		})
	}
}

func TestCardeaSink_WriteNeverWaitsForThePlatform(t *testing.T) {
	p := newCSPlatform(t, func(n int) csPlatformReply { return csPlatformReply{hang: true} })
	cfg := csConfig(p.endpoint())
	cfg.QueueSize = 5000
	cfg.ShutdownTimeout = 100 * time.Millisecond
	s := csNewSink(t, cfg, zap.NewNop())

	recs := csSizedN(t, 1000, 600)
	start := time.Now()
	for i, r := range recs {
		if err := s.Write(r); err != nil {
			t.Fatalf("Write #%d: %v", i+1, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("1000 Writes took %v against a platform that never answers, want them to cost an enqueue each", elapsed)
	}
}

// --- marshal failure ---------------------------------------------------------------------

func TestCardeaSink_RecordThatCannotBeEncodedConsumesASeqAndReturnsTheError(t *testing.T) {
	p := newCSPlatform(t, nil)
	logger, logs := csObserved()
	s := csNewSink(t, csConfig(p.endpoint()), logger)

	if err := s.Write(minimalRequestRecord(csCallID(1))); err != nil {
		t.Fatalf("Write #1: %v", err)
	}
	err := s.Write(csFailingRecord{csSentinelRecord(2)})
	if err == nil {
		t.Fatal("Write of a record that cannot be encoded = nil, want the marshal error")
	}
	if errors.Is(err, ErrClosed) {
		t.Fatalf("Write error = %v, want the marshal error, not ErrClosed", err)
	}
	if err := s.Write(minimalRequestRecord(csCallID(3))); err != nil {
		t.Fatalf("Write #3: %v", err)
	}

	p.waitSeqs(t, 2)
	if got := p.seqs(); !slices.Equal(got, []uint64{1, 3}) {
		t.Fatalf("delivered seqs = %v, want [1 3]: the failed record's seq is consumed so the platform sees the gap", got)
	}
	csAssertNoLeak(t, logs, csDefaultForbidden...)
}

// --- close ------------------------------------------------------------------------------

func TestCardeaSink_CloseDrainsAgainstASlowButWorkingPlatform(t *testing.T) {
	p := newCSPlatform(t, func(n int) csPlatformReply { return csPlatformReply{status: 202, delay: 100 * time.Millisecond} })
	cfg := csConfig(p.endpoint())
	cfg.BatchMaxRecords = 1
	cfg.BatchMaxDelay = time.Hour
	cfg.ShutdownTimeout = 10 * time.Second
	s := csNewSink(t, cfg, zap.NewNop())
	csWrite(t, s, csSizedN(t, 5, 600)...)

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("Close took %v, want it to return as soon as the queue is drained", elapsed)
	}
	if got := p.seqs(); !slices.Equal(got, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("delivered seqs = %v after Close, want 1..5: Close returns after the worker exits", got)
	}
}

func TestCardeaSink_CloseAbandonsAtTheTimeoutAgainstADeadPlatformAndReportsTheLoss(t *testing.T) {
	p := newCSPlatform(t, func(n int) csPlatformReply { return csPlatformReply{hang: true} })
	logger, logs := csObserved()
	cfg := csConfig(p.endpoint())
	cfg.ShutdownTimeout = 200 * time.Millisecond
	s := csNewSink(t, cfg, logger)
	csWrite(t, s, csSentinelRecord(1), csSentinelRecord(2), csSentinelRecord(3))
	csWaitUntil(t, "the first attempt", 5*time.Second, func() bool { return p.count() >= 1 })

	start := time.Now()
	_ = s.Close()
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("Close took %v, want about shutdown_timeout (200ms)", elapsed)
	}
	if !csCanSumTo(csErrors(logs), 3) {
		t.Fatalf("no error line reports the 3 records lost at shutdown:\n%s", csLogText(logs))
	}
	var summary bool
	for _, e := range logs.FilterLevelExact(zapcore.InfoLevel).All() {
		if slices.Contains(csIntFields(e), 3) {
			summary = true
		}
	}
	if !summary {
		t.Fatalf("Close logged no info line with the lost total 3:\n%s", csLogText(logs))
	}
	csAssertNoLeak(t, logs, csDefaultForbidden...)
}

func TestCardeaSink_ZeroShutdownTimeoutSkipsDraining(t *testing.T) {
	p := newCSPlatform(t, nil)
	logger, logs := csObserved()
	cfg := csConfig(p.endpoint())
	cfg.BatchMaxDelay = time.Hour
	cfg.ShutdownTimeout = 0
	s := csNewSink(t, cfg, logger)
	csWrite(t, s, csSizedN(t, 3, 600)...)

	_ = s.Close()
	if n := p.count(); n != 0 {
		t.Fatalf("platform received %d requests, want none: shutdown_timeout 0 skips draining", n)
	}
	if !csCanSumTo(csErrors(logs), 3) {
		t.Fatalf("no error line reports the 3 records lost:\n%s", csLogText(logs))
	}
}

func TestCardeaSink_WriteAfterCloseIsErrClosedAndCloseIsIdempotent(t *testing.T) {
	p := newCSPlatform(t, nil)
	s := csNewSink(t, csConfig(p.endpoint()), zap.NewNop())
	csWrite(t, s, minimalRequestRecord(csCallID(1)))

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requests := p.count()
	if err := s.Write(minimalRequestRecord(csCallID(2))); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}

	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second Close did not return")
	}
	if p.count() != requests {
		t.Fatal("the second Close delivered something")
	}
}

// --- identity and construction -------------------------------------------------------------

func TestCardeaSink_EachSinkHasItsOwnIdentityAndNumbersFromOne(t *testing.T) {
	p1, p2 := newCSPlatform(t, nil), newCSPlatform(t, nil)
	s1 := csNewSink(t, csConfig(p1.endpoint()), zap.NewNop())
	s2 := csNewSink(t, csConfig(p2.endpoint()), zap.NewNop())
	if s1.InstanceID() == s2.InstanceID() {
		t.Fatalf("two sinks share the identity %q", s1.InstanceID())
	}
	csWrite(t, s1, minimalRequestRecord(csCallID(1)))
	csWrite(t, s2, minimalRequestRecord(csCallID(1)))
	p1.waitSeqs(t, 1)
	p2.waitSeqs(t, 1)
	for i, p := range []*csPlatform{p1, p2} {
		s := []*CardeaSink{s1, s2}[i]
		r := p.requests()[0]
		if r.entries[0].Seq != 1 || r.header.Get("X-Cardea-First-Seq") != "1" {
			t.Fatalf("sink %d starts at seq %d, want 1", i+1, r.entries[0].Seq)
		}
		if got := r.header.Get("X-Cardea-Instance"); got != s.InstanceID() {
			t.Fatalf("sink %d sends instance %q, InstanceID() = %q", i+1, got, s.InstanceID())
		}
	}
}

func TestNewCardeaSink_LogsOneStartupLineWithHostAndIdentityAndMakesNoConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepts atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = c.Close()
		}
	}()

	logger, logs := csObserved()
	t.Setenv(csKeyEnv, csKey)
	s, err := NewCardeaSink(csConfig("http://"+ln.Addr().String()+csPath+"?"+csQuery), logger)
	if err != nil {
		t.Fatalf("NewCardeaSink: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if n := logs.Len(); n != 1 {
		t.Fatalf("construction logged %d lines, want exactly one startup line:\n%s", n, csLogText(logs))
	}
	e := logs.All()[0]
	if e.Level != zapcore.InfoLevel {
		t.Fatalf("startup line level = %s, want info", e.Level)
	}
	text := csEntryText(e)
	if !strings.Contains(text, "127.0.0.1") {
		t.Fatalf("startup line does not name the platform host: %s", text)
	}
	if !strings.Contains(text, s.InstanceID()) {
		t.Fatalf("startup line does not carry InstanceID() %q: %s", s.InstanceID(), text)
	}
	for _, f := range []string{csKey, "secret-ingest-path", csQuery} {
		if strings.Contains(text, f) {
			t.Fatalf("startup line contains %q: %s", f, text)
		}
	}

	_ = s.Close()
	if n := accepts.Load(); n != 0 {
		t.Fatalf("the platform saw %d connections, want none before there is a record to deliver", n)
	}
}

func TestNewCardeaSink_RejectsAnUnsafeOrMissingKeyNamingTheVariableNotTheValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		unset bool
	}{
		{name: "unset", unset: true},
		{name: "empty", value: ""},
		{name: "contains a space", value: "sk-secret-with space"},
		{name: "contains a newline", value: "sk-secret\nX-Injected: 1"},
		{name: "contains a carriage return", value: "sk-secret\rinjected"},
		{name: "contains a tab", value: "sk-secret\tvalue"},
		{name: "contains a control character", value: "sk-secret\x01value"},
		{name: "trailing space", value: "sk-secret "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(csKeyEnv, tt.value)
			if tt.unset {
				if err := os.Unsetenv(csKeyEnv); err != nil {
					t.Fatal(err)
				}
			}
			s, err := NewCardeaSink(csConfig("https://platform.example.com/ingest"), zap.NewNop())
			if err == nil {
				_ = s.Close()
				t.Fatal("NewCardeaSink = nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), csKeyEnv) {
				t.Fatalf("error %q does not name the variable %s", err, csKeyEnv)
			}
			if tt.value != "" && strings.Contains(err.Error(), strings.TrimSpace(tt.value)[:9]) {
				t.Fatalf("error %q contains the key value", err)
			}
		})
	}
}

// --- no content in logs --------------------------------------------------------------------

func TestCardeaSink_NoFailurePathLogsRecordContentTheKeyOrTheEndpointPath(t *testing.T) {
	closedAddr := func(t *testing.T) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}
	tests := []struct {
		name         string
		wantErrorLog bool
		run          func(t *testing.T, logger *zap.Logger)
	}{
		{"marshal error", false, func(t *testing.T, logger *zap.Logger) {
			p := newCSPlatform(t, nil)
			s := csNewSink(t, csConfig(p.endpoint()), logger)
			_ = s.Write(csFailingRecord{csSentinelRecord(1)})
			_ = s.Close()
		}},
		{"400 refusal", true, func(t *testing.T, logger *zap.Logger) {
			p := newCSPlatform(t, func(int) csPlatformReply { return csPlatformReply{status: 400} })
			s := csNewSink(t, csConfig(p.endpoint()), logger)
			csWrite(t, s, csSentinelRecord(1))
			csWaitUntil(t, "a request", 5*time.Second, func() bool { return p.count() >= 1 })
			_ = s.Close()
		}},
		{"401 refusal", true, func(t *testing.T, logger *zap.Logger) {
			p := newCSPlatform(t, func(int) csPlatformReply { return csPlatformReply{status: 401} })
			s := csNewSink(t, csConfig(p.endpoint()), logger)
			csWrite(t, s, csSentinelRecord(1))
			csWaitUntil(t, "a request", 5*time.Second, func() bool { return p.count() >= 1 })
			_ = s.Close()
		}},
		{"capacity drop", true, func(t *testing.T, logger *zap.Logger) {
			p := newCSPlatform(t, func(int) csPlatformReply { return csPlatformReply{hang: true} })
			cfg := csConfig(p.endpoint())
			cfg.QueueSize = 1
			cfg.BatchMaxRecords = 1
			cfg.ShutdownTimeout = 50 * time.Millisecond
			s := csNewSink(t, cfg, logger)
			for i := 1; i <= 3; i++ {
				_ = s.Write(csSentinelRecord(i))
			}
			_ = s.Close()
		}},
		{"shutdown loss", true, func(t *testing.T, logger *zap.Logger) {
			p := newCSPlatform(t, func(int) csPlatformReply { return csPlatformReply{hang: true} })
			cfg := csConfig(p.endpoint())
			cfg.ShutdownTimeout = 50 * time.Millisecond
			s := csNewSink(t, cfg, logger)
			csWrite(t, s, csSentinelRecord(1), csSentinelRecord(2))
			_ = s.Close()
		}},
		{"panic", true, func(t *testing.T, logger *zap.Logger) {
			p := newCSPlatform(t, nil)
			cfg := csConfig(p.endpoint())
			cfg.BatchMaxRecords = 1
			s := csNewSink(t, cfg, logger)
			csPanicOnFirstSend(t, s)
			csWrite(t, s, csSentinelRecord(1))
			time.Sleep(200 * time.Millisecond)
			_ = s.Close()
		}},
		{"retries against a failing platform", false, func(t *testing.T, logger *zap.Logger) {
			p := newCSPlatform(t, func(int) csPlatformReply { return csPlatformReply{status: 503} })
			cfg := csConfig(p.endpoint())
			cfg.ShutdownTimeout = 100 * time.Millisecond
			s := csNewSink(t, cfg, logger)
			csWrite(t, s, csSentinelRecord(1))
			csWaitUntil(t, "retries", 5*time.Second, func() bool { return p.count() >= 3 })
			_ = s.Close()
		}},
		{"unreachable platform", false, func(t *testing.T, logger *zap.Logger) {
			cfg := csConfig("http://" + closedAddr(t) + csPath + "?" + csQuery)
			cfg.ShutdownTimeout = 100 * time.Millisecond
			s := csNewSink(t, cfg, logger)
			csWrite(t, s, csSentinelRecord(1))
			time.Sleep(200 * time.Millisecond)
			_ = s.Close()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := csObserved()
			tt.run(t, logger)
			if tt.wantErrorLog && len(csErrors(logs)) == 0 {
				t.Fatalf("expected an error-level line for the loss, got:\n%s", csLogText(logs))
			}
			csAssertNoLeak(t, logs, csDefaultForbidden...)
		})
	}
}
