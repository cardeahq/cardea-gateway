package pipeline

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cardeahq/cardea-gateway/internal/trace"
)

// bodyRecorder writes every byte through to the real ResponseWriter
// immediately, so streaming responses keep streaming, while also
// retaining up to maxBody bytes for stage observation and feeding the
// relayed bytes to the tracing tap, if any.
type bodyRecorder struct {
	http.ResponseWriter
	maxBody     int64
	buf         bytes.Buffer
	truncated   bool
	status      int
	wroteHeader bool

	tap            *trace.ResponseTap
	firstByte      time.Time
	upstreamFailed bool
	disconnected   bool
	tapStarted     time.Time
	tapTime        time.Duration
	tapFailed      bool
	tapPanic       any
}

func newBodyRecorder(w http.ResponseWriter, maxBody int64, tap *trace.ResponseTap) *bodyRecorder {
	return &bodyRecorder{ResponseWriter: w, maxBody: maxBody, tap: tap}
}

func (r *bodyRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.firstByte = time.Now()
	r.tapCall(func() { r.tap.Start(status, r.Header()) })
	r.ResponseWriter.WriteHeader(status)
}

func (r *bodyRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}

	remaining := r.maxBody - int64(r.buf.Len())
	switch {
	case remaining <= 0:
		if len(b) > 0 {
			r.truncated = true
		}
	case int64(len(b)) > remaining:
		r.buf.Write(b[:remaining])
		r.truncated = true
	default:
		r.buf.Write(b)
	}

	n, err := r.ResponseWriter.Write(b)
	if n > 0 {
		relayedAt := time.Now()
		r.tapCall(func() { r.tap.Write(b[:n], relayedAt) })
	}
	if err != nil && !r.disconnected {
		r.disconnected = true
		r.tapCall(func() { r.tap.Disconnected() })
	}
	return n, err
}

func (r *bodyRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// UpstreamFailed is called by the provider's proxy when the provider never
// answered, before it writes its own 502.
func (r *bodyRecorder) UpstreamFailed() {
	r.upstreamFailed = true
}

// statusSent is what the agent received; a handler that never wrote a
// status sent 200.
func (r *bodyRecorder) statusSent() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// tapCall runs f against the tap, timing it and containing any panic: the
// relay must never be affected by tracing. After a panic the tap is not
// called again.
func (r *bodyRecorder) tapCall(f func()) {
	if r.tap == nil || r.tapFailed {
		return
	}
	started := time.Now()
	if r.tapStarted.IsZero() {
		r.tapStarted = started
	}
	defer func() {
		r.tapTime += time.Since(started)
		if p := recover(); p != nil {
			r.tapFailed = true
			r.tapPanic = p
		}
	}()
	f()
}

// tapResult closes the tap and returns what it read, with tracing's entry
// for the response record's stages. The entry is nil when there is no tap.
// Result is called even after a tap panic, since it also stops the gzip
// parser goroutine.
func (r *bodyRecorder) tapResult(stage string, forwardedAt time.Time) (trace.ResponseInfo, *observation) {
	info := trace.ResponseInfo{ToolCalls: []trace.ToolCall{}}
	if r.tap == nil {
		return info, nil
	}
	started := time.Now()
	if r.tapStarted.IsZero() {
		r.tapStarted = started
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				r.tapFailed = true
				if r.tapPanic == nil {
					r.tapPanic = p
				}
				info = trace.ResponseInfo{ToolCalls: []trace.ToolCall{}}
			}
		}()
		info = r.tap.Result()
	}()
	r.tapTime += time.Since(started)

	return info, &observation{
		name:      stage,
		started:   r.tapStarted,
		duration:  r.tapTime,
		completed: !info.Failed && !r.tapFailed,
	}
}

func (r *bodyRecorder) tapError(info trace.ResponseInfo) error {
	if r.tapPanic != nil {
		return errors.Join(fmt.Errorf("response tap panicked: %v", r.tapPanic), info.Err)
	}
	return info.Err
}

func (r *bodyRecorder) decodedBody(contentEncoding string) ([]byte, error) {
	if contentEncoding != "gzip" {
		return r.buf.Bytes(), nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(r.buf.Bytes()))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}
