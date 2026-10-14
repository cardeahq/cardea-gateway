package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cardeahq/cardea-gateway/internal/config"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

// stubProvider is a minimal provider.Provider for tests that need to
// observe whether the provider was reached.
type stubProvider struct {
	handler http.Handler
}

func (stubProvider) Name() string            { return "stub" }
func (p stubProvider) Handler() http.Handler { return p.handler }

// fileSinkTracing is a tracing config whose records go to a temp file, so
// tests that enable tracing never write to stdout.
func fileSinkTracing(t *testing.T) config.TracingConfig {
	t.Helper()
	return config.TracingConfig{
		FingerprintKeyEnv: "AK_GW_TEST_UNSET_FINGERPRINT_KEY",
		MaxReadBytes:      1 << 20,
		Sink:              config.TraceSinkConfig{Type: "logging", Path: filepath.Join(t.TempDir(), "records.jsonl")},
	}
}

func TestGateway_HealthzBypassesPipelineRegardlessOfStageConfig(t *testing.T) {
	tests := []struct {
		name    string
		tracing bool
	}{
		{"pipeline stage enabled", true},
		{"pipeline stage disabled", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := stubProvider{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			})}
			cfg := config.Config{
				Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
				Pipeline: config.PipelineConfig{
					MaxBodyBytes: 1 << 20,
					Stages:       config.StagesConfig{Tracing: tt.tracing},
				},
				Tracing: fileSinkTracing(t),
			}
			g, err := New(cfg, zaptest.NewLogger(t), prov)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			g.server.Handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("/healthz status = %d, want %d: it must bypass the pipeline and the provider entirely", rec.Code, http.StatusOK)
			}
		})
	}
}

func TestGateway_ForwardsAllowedCallsThroughPipelineToProvider(t *testing.T) {
	var providerCalled bool
	prov := stubProvider{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		w.WriteHeader(http.StatusOK)
	})}

	cfg := config.Config{
		Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Tracing: true},
		},
		Tracing: fileSinkTracing(t),
	}
	g, err := New(cfg, zaptest.NewLogger(t), prov)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(rec, req)

	if !providerCalled {
		t.Fatal("provider was never invoked for an allowed call routed through the pipeline")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestBuildStages_RespectsConfigBooleans(t *testing.T) {
	tests := []struct {
		name    string
		tracing bool
		wantLen int
	}{
		{"tracing enabled", true, 1},
		{"tracing disabled", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				Pipeline: config.PipelineConfig{
					MaxBodyBytes: 1 << 20,
					Stages:       config.StagesConfig{Tracing: tt.tracing},
				},
				Tracing: fileSinkTracing(t),
			}
			stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
			if err != nil {
				t.Fatalf("buildStages: %v", err)
			}
			if sink != nil {
				t.Cleanup(func() { _ = sink.Close() })
			}
			if len(stages) != tt.wantLen {
				t.Fatalf("len(buildStages(...)) = %d, want %d", len(stages), tt.wantLen)
			}
		})
	}
}

func TestBuildStages_TracingStageIsNamedAfterItsConfigKey(t *testing.T) {
	cfg := config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Tracing: true},
		},
		Tracing: fileSinkTracing(t),
	}
	stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	if len(stages) != 1 {
		t.Fatalf("len(buildStages(...)) = %d, want 1", len(stages))
	}
	if got := stages[0].Name(); got != "tracing" {
		t.Fatalf("stages[0].Name() = %q, want %q (matching its pipeline.stages config key)", got, "tracing")
	}
}

// --- cardea sink (specs/0005) --------------------------------------------------

const (
	gwCardeaKeyEnv = "AK_GW_TEST_CARDEA_API_KEY"
	gwCardeaKey    = "sk-cardea-gateway-test-key-0123456789"
)

// gwPlatform is a fake Cardea platform: it keeps the record bytes of every
// line it receives, and can be told never to answer.
type gwPlatform struct {
	srv      *httptest.Server
	hang     bool
	mu       sync.Mutex
	requests int
	records  []string
	block    chan struct{}
	once     sync.Once
}

func newGWPlatform(t *testing.T, hang bool) *gwPlatform {
	t.Helper()
	p := &gwPlatform{hang: hang, block: make(chan struct{})}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.requests++
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			plain, _ := io.ReadAll(zr)
			for _, l := range strings.Split(strings.TrimSuffix(string(plain), "\n"), "\n") {
				var e struct {
					Seq    uint64          `json:"seq"`
					Record json.RawMessage `json:"record"`
				}
				if json.Unmarshal([]byte(l), &e) == nil {
					p.records = append(p.records, string(e.Record))
				}
			}
		}
		p.mu.Unlock()
		if p.hang {
			select {
			case <-p.block:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(p.srv.Close)
	t.Cleanup(func() { p.once.Do(func() { close(p.block) }) })
	return p
}

func (p *gwPlatform) received() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.records...)
}

func (p *gwPlatform) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}

func gwWaitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// gwCountingListener accepts and immediately drops every connection, and
// counts them.
func gwCountingListener(t *testing.T) (addr string, accepts *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepts = new(atomic.Int32)
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
	return ln.Addr().String(), accepts
}

func gwCardeaSettings(endpoint string) config.CardeaSinkConfig {
	return config.CardeaSinkConfig{
		Endpoint:        endpoint,
		APIKeyEnv:       gwCardeaKeyEnv,
		QueueSize:       1000,
		QueueMaxBytes:   64 << 20,
		BatchMaxRecords: 10,
		BatchMaxBytes:   1 << 20,
		BatchMaxDelay:   20 * time.Millisecond,
		RequestTimeout:  2 * time.Second,
		ShutdownTimeout: 500 * time.Millisecond,
	}
}

const gwCallBody = `{"model":"claude-sonnet-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`

// gwCardeaGateway builds a gateway in front of a fake provider that streams
// events, with the sink the type selects. The key variable is set.
func gwCardeaGateway(t *testing.T, sinkType, endpoint string, tune func(*config.CardeaSinkConfig), logger *zap.Logger, events []string) (*Gateway, config.Config) {
	t.Helper()
	_, url := newFakeAnthropic(t, streamResponder(events))
	cfg := tracingGatewayConfig(t, url)
	t.Setenv(gwCardeaKeyEnv, gwCardeaKey)
	c := gwCardeaSettings(endpoint)
	if tune != nil {
		tune(&c)
	}
	cfg.Tracing.Sink.Type = sinkType
	cfg.Tracing.Sink.Cardea = c
	g := newTracingGateway(t, cfg, logger)
	t.Cleanup(func() { _ = g.sink.Close() })
	return g, cfg
}

// gwUnwritableSink returns a path the local sink can open but not write to:
// a FIFO whose only reader has gone away by the time breakIt is called.
func gwUnwritableSink(t *testing.T) (path string, breakIt func(), hasWriter func() bool) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "records.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("opening the FIFO's reading end: %v", err)
	}
	var once sync.Once
	closeReader := func() { once.Do(func() { _ = syscall.Close(fd) }) }
	t.Cleanup(closeReader)
	hasWriter = func() bool {
		buf := make([]byte, 1)
		n, err := syscall.Read(fd, buf)
		return n != 0 || err != nil // EOF means no writer holds the FIFO open
	}
	return path, closeReader, hasWriter
}

func gwWantBody(events []string) string { return strings.Join(events, "") }

func TestGateway_DefaultSettingsBuildNoCardeaSinkAndSendNothing(t *testing.T) {
	addr, accepts := gwCountingListener(t)
	_, url := newFakeAnthropic(t, streamResponder(endTurnStream("msg_1")))
	cfg := tracingGatewayConfig(t, url) // type logging, the default
	cfg.Tracing.Sink.Cardea = gwCardeaSettings("http://" + addr + "/in")
	// No key variable: the default must not need one.
	t.Setenv(gwCardeaKeyEnv, "")

	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
	t.Cleanup(func() { _ = g.sink.Close() })
	if _, ok := g.sink.(*trace.CardeaSink); ok {
		t.Fatal("the default configuration built a CardeaSink")
	}
	if _, ok := g.sink.(*trace.MultiSink); ok {
		t.Fatal("the default configuration built a MultiSink")
	}
	if rec := serve(g, post(gwCallBody)); rec.Code != http.StatusOK {
		t.Fatalf("call status = %d, want 200", rec.Code)
	}
	time.Sleep(200 * time.Millisecond)
	if n := accepts.Load(); n != 0 {
		t.Fatalf("%d connections to the configured address with the default sink type, want none", n)
	}
}

func TestBuildStages_SinkTypeSelectsTheImplementation(t *testing.T) {
	tests := []struct {
		sinkType string
		check    func(s trace.CloseableSink) bool
		want     string
	}{
		{"logging", func(s trace.CloseableSink) bool { _, ok := s.(*trace.LogSink); return ok }, "*trace.LogSink"},
		{"cardea", func(s trace.CloseableSink) bool { _, ok := s.(*trace.CardeaSink); return ok }, "*trace.CardeaSink"},
		{"logging+cardea", func(s trace.CloseableSink) bool { _, ok := s.(*trace.MultiSink); return ok }, "*trace.MultiSink"},
	}
	for _, tt := range tests {
		t.Run(tt.sinkType, func(t *testing.T) {
			p := newGWPlatform(t, false)
			t.Setenv(gwCardeaKeyEnv, gwCardeaKey)
			cfg := config.Config{
				Pipeline: config.PipelineConfig{MaxBodyBytes: 1 << 20, Stages: config.StagesConfig{Tracing: true}},
				Tracing:  fileSinkTracing(t),
			}
			cfg.Tracing.Sink.Type = tt.sinkType
			cfg.Tracing.Sink.Cardea = gwCardeaSettings(p.srv.URL + "/in")
			_, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
			if err != nil {
				t.Fatalf("buildStages: %v", err)
			}
			t.Cleanup(func() { _ = sink.Close() })
			if !tt.check(sink) {
				t.Fatalf("buildStages returned %T for type %q, want %s", sink, tt.sinkType, tt.want)
			}
		})
	}
}

func TestGatewayNew_CardeaWithoutTheKeyVariableFailsNamingItAndLeavesNoOpenFile(t *testing.T) {
	tests := []struct {
		name  string
		value string
		unset bool
	}{
		{"unset", "", true},
		{"empty", "", false},
		{"contains a space", "sk-secret with space", false},
	}
	for _, tt := range tests {
		for _, sinkType := range []string{"cardea", "logging+cardea"} {
			t.Run(tt.name+"/"+sinkType, func(t *testing.T) {
				path, _, hasWriter := gwUnwritableSink(t)
				_, url := newFakeAnthropic(t, streamResponder(endTurnStream("msg_1")))
				cfg := tracingGatewayConfig(t, url)
				cfg.Tracing.Sink.Type = sinkType
				cfg.Tracing.Sink.Path = path
				cfg.Tracing.Sink.Cardea = gwCardeaSettings("https://platform.example.com/in")
				t.Setenv(gwCardeaKeyEnv, tt.value)
				if tt.unset {
					t.Setenv(gwCardeaKeyEnv, "x")
					_ = syscall.Unsetenv(gwCardeaKeyEnv)
				}
				t.Setenv(gwAnthropicKeyEnv, "sk-test-upstream")
				prov := stubProvider{handler: http.NotFoundHandler()}

				g, err := New(cfg, zaptest.NewLogger(t), prov)
				if err == nil {
					t.Fatal("gateway.New = nil error, want it to fail")
				}
				if g != nil {
					t.Fatal("gateway.New returned a *Gateway alongside an error")
				}
				if !strings.Contains(err.Error(), gwCardeaKeyEnv) {
					t.Fatalf("error %q does not name %s", err, gwCardeaKeyEnv)
				}
				if tt.value != "" && strings.Contains(err.Error(), tt.value) {
					t.Fatalf("error %q contains the key value", err)
				}
				if hasWriter() {
					t.Fatal("the local sink's file was left open after gateway.New failed")
				}
			})
		}
	}
}

func TestGateway_LoggingPlusCardeaWritesTheLocalFileAndReachesThePlatform(t *testing.T) {
	p := newGWPlatform(t, false)
	events := toolCallStream("msg_1", "toolu_1", "bash", `{"command":"ls"}`)
	g, cfg := gwCardeaGateway(t, "logging+cardea", p.srv.URL+"/in", nil, zaptest.NewLogger(t), events)

	rec := serve(g, post(gwCallBody))
	if rec.Code != http.StatusOK || rec.Body.String() != gwWantBody(events) {
		t.Fatalf("call: status %d, %d bytes, want 200 and the provider's response unchanged", rec.Code, rec.Body.Len())
	}

	gwWaitUntil(t, "both records at the platform", func() bool { return len(p.received()) >= 2 })
	local := readRecordLines(t, cfg.Tracing.Sink.Path)
	if len(local) != 2 {
		t.Fatalf("local file has %d records, want 2", len(local))
	}
	got := p.received()
	if len(got) != 2 || got[0] != local[0] || got[1] != local[1] {
		t.Fatalf("platform records differ from the local ones, want each exactly as 0003 writes it:\nplatform: %v\nlocal:    %v", got, local)
	}
	for i, kind := range []string{"request", "response"} {
		var m map[string]any
		_ = json.Unmarshal([]byte(got[i]), &m)
		if m["record"] != kind {
			t.Fatalf("record %d is %v, want %s", i, m["record"], kind)
		}
	}
}

func TestGateway_CardeaOnlySendsBothRecordsToThePlatform(t *testing.T) {
	p := newGWPlatform(t, false)
	events := endTurnStream("msg_1")
	g, _ := gwCardeaGateway(t, "cardea", p.srv.URL+"/in", nil, zaptest.NewLogger(t), events)

	if rec := serve(g, post(gwCallBody)); rec.Code != http.StatusOK || rec.Body.String() != gwWantBody(events) {
		t.Fatalf("call: status %d, want 200 and the provider's response unchanged", rec.Code)
	}
	gwWaitUntil(t, "both records at the platform", func() bool { return len(p.received()) >= 2 })
}

func TestGateway_LoggingPlusCardeaWithTheLocalFileUnwritableStillReachesThePlatform(t *testing.T) {
	path, breakIt, _ := gwUnwritableSink(t)
	p := newGWPlatform(t, false)
	logger, logs := observed()
	events := endTurnStream("msg_1")
	_, url := newFakeAnthropic(t, streamResponder(events))
	cfg := tracingGatewayConfig(t, url)
	t.Setenv(gwCardeaKeyEnv, gwCardeaKey)
	cfg.Tracing.Sink.Type = "logging+cardea"
	cfg.Tracing.Sink.Path = path
	cfg.Tracing.Sink.Cardea = gwCardeaSettings(p.srv.URL + "/in")
	g := newTracingGateway(t, cfg, logger)
	t.Cleanup(func() { _ = g.sink.Close() })
	breakIt()

	rec := serve(g, post(gwCallBody))
	if rec.Code != http.StatusOK || rec.Body.String() != gwWantBody(events) {
		t.Fatalf("call: status %d, want 200 and the response unchanged: the local sink failing never denies a call", rec.Code)
	}
	gwWaitUntil(t, "both records at the platform", func() bool { return len(p.received()) >= 2 })
	gwAssertLostRecordLogged(t, logs, "request")
}

func TestGateway_UnwritableLocalSinkDoesNotDenyTheCallAndNamesTheLostRecord(t *testing.T) {
	path, breakIt, _ := gwUnwritableSink(t)
	logger, logs := observed()
	events := endTurnStream("msg_1")
	_, url := newFakeAnthropic(t, streamResponder(events))
	cfg := tracingGatewayConfig(t, url)
	cfg.Tracing.Sink.Path = path
	g := newTracingGateway(t, cfg, logger)
	t.Cleanup(func() { _ = g.sink.Close() })
	breakIt()

	rec := serve(g, post(gwCallBody))
	if rec.Code != http.StatusOK || rec.Body.String() != gwWantBody(events) {
		t.Fatalf("call: status %d, %d bytes, want 200 and the response unchanged", rec.Code, rec.Body.Len())
	}
	gwAssertLostRecordLogged(t, logs, "request")
	gwAssertLostRecordLogged(t, logs, "response")
}

// gwAssertLostRecordLogged requires an error-level line naming a call and
// the record type it lost.
func gwAssertLostRecordLogged(t *testing.T, logs *observer.ObservedLogs, recordType string) {
	t.Helper()
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		ctx := e.ContextMap()
		if id, _ := ctx["call_id"].(string); id != "" && ctx["record"] == recordType {
			return
		}
	}
	t.Fatalf("no error-level line naming a call_id and the lost %s record:\n%s", recordType, logText(logs))
}

func TestGateway_UnreachablePlatformStartsServesAndConnectsOnlyOnceThereIsARecord(t *testing.T) {
	addr, accepts := gwCountingListener(t) // drops every connection: the platform is unreachable
	local := filepath.Join(t.TempDir(), "local.jsonl")
	events := endTurnStream("msg_1")
	_, url := newFakeAnthropic(t, streamResponder(events))
	cfg := tracingGatewayConfig(t, url)
	t.Setenv(gwCardeaKeyEnv, gwCardeaKey)
	cfg.Tracing.Sink.Type = "logging+cardea"
	cfg.Tracing.Sink.Path = local
	cfg.Tracing.Sink.Cardea = gwCardeaSettings("http://" + addr + "/in")
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
	t.Cleanup(func() { _ = g.sink.Close() })

	hz := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(hz, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	time.Sleep(300 * time.Millisecond)
	if hz.Code != http.StatusOK || accepts.Load() != 0 {
		t.Fatalf("after start: healthz %d, %d connections; want 200 and none before there is a record", hz.Code, accepts.Load())
	}

	for i := range 3 {
		rec := serve(g, post(gwCallBody))
		if rec.Code != http.StatusOK || rec.Body.String() != gwWantBody(events) {
			t.Fatalf("call %d: status %d, want 200 and the response unchanged", i+1, rec.Code)
		}
	}
	if got := len(readRecordLines(t, local)); got != 6 {
		t.Fatalf("local file has %d records, want 6 (both records of 3 calls) while the platform is unreachable", got)
	}
	gwWaitUntil(t, "an attempt to reach the platform", func() bool { return accepts.Load() > 0 })
}

func TestGateway_PlatformThatNeverAnswersDoesNotSlowOrAlterTheCall(t *testing.T) {
	p := newGWPlatform(t, true)
	events := toolCallStream("msg_1", "toolu_1", "bash", `{"command":"ls"}`)
	g, _ := gwCardeaGateway(t, "cardea", p.srv.URL+"/in", func(c *config.CardeaSinkConfig) {
		c.RequestTimeout = 30 * time.Second
		c.ShutdownTimeout = 100 * time.Millisecond
	}, zaptest.NewLogger(t), events)

	for i := range 3 {
		start := time.Now()
		rec := serve(g, post(gwCallBody))
		if rec.Code != http.StatusOK || rec.Body.String() != gwWantBody(events) {
			t.Fatalf("call %d: status %d, want 200 and the response unchanged", i+1, rec.Code)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("call %d took %v against a platform that never answers", i+1, elapsed)
		}
	}
}

func TestGateway_FullQueueDoesNotDenyOrAlterTheCall(t *testing.T) {
	p := newGWPlatform(t, true)
	logger, logs := observed()
	events := endTurnStream("msg_1")
	g, _ := gwCardeaGateway(t, "cardea", p.srv.URL+"/in", func(c *config.CardeaSinkConfig) {
		c.QueueSize = 1
		c.BatchMaxRecords = 1
		c.ShutdownTimeout = 100 * time.Millisecond
	}, logger, events)

	for i := range 3 {
		rec := serve(g, post(gwCallBody))
		if rec.Code != http.StatusOK || rec.Body.String() != gwWantBody(events) {
			t.Fatalf("call %d: status %d, want 200 and the response unchanged with a full queue", i+1, rec.Code)
		}
	}
	gwWaitUntil(t, "an error-level line for the records dropped", func() bool {
		return logs.FilterLevelExact(zapcore.ErrorLevel).Len() > 0
	})
}

func TestGateway_RunShutdownDeliversTheRecordsStillHeld(t *testing.T) {
	p := newGWPlatform(t, false)
	events := endTurnStream("msg_1")
	g, _ := gwCardeaGateway(t, "cardea", p.srv.URL+"/in", func(c *config.CardeaSinkConfig) {
		c.BatchMaxRecords = 500
		c.BatchMaxDelay = time.Hour // nothing leaves until the sink is closed
		c.ShutdownTimeout = 5 * time.Second
	}, zaptest.NewLogger(t), events)

	if rec := serve(g, post(gwCallBody)); rec.Code != http.StatusOK {
		t.Fatalf("call status = %d, want 200", rec.Code)
	}
	time.Sleep(150 * time.Millisecond)
	if n := p.requestCount(); n != 0 {
		t.Fatalf("platform received %d requests before shutdown, want none (records are held)", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
	if got := p.received(); len(got) != 2 {
		t.Fatalf("platform holds %d records after Run returned, want both held records delivered", len(got))
	}
}

func TestBuildStages_PolicyEngineFailureClosesTheSinkAlreadyBuilt(t *testing.T) {
	path, _, hasWriter := gwUnwritableSink(t)
	p := newGWPlatform(t, false)
	logger, logs := observed()
	t.Setenv(gwCardeaKeyEnv, gwCardeaKey)
	cfg := buildCfg(t, false, true, true, filepath.Join(t.TempDir(), "missing.rego"))
	cfg.Tracing.Sink.Type = "logging+cardea"
	cfg.Tracing.Sink.Path = path
	cfg.Tracing.Sink.Cardea = gwCardeaSettings(p.srv.URL + "/in")

	if _, _, _, _, err := buildStages(cfg, logger); err == nil {
		t.Fatal("buildStages = nil error, want the policy failure")
	}
	if hasWriter() {
		t.Fatal("the local sink's file was left open after the policy engine failed to build")
	}
	// The Cardea sink logs one line at startup and, when closed, its totals.
	if n := logs.FilterLevelExact(zapcore.InfoLevel).Len(); n < 2 {
		t.Fatalf("%d info lines, want the startup line and the close summary of a sink that was closed:\n%s", n, logText(logs))
	}
}
