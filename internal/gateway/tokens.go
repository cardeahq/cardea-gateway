package gateway

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/agentkeeper/agentkeeper/internal/provider"
)

// tokenCounter tracks a running, in-memory total of tokens forwarded
// through the gateway. It resets on process restart.
type tokenCounter struct {
	sent     atomic.Int64
	received atomic.Int64
}

func (c *tokenCounter) add(sent, received int64) {
	c.sent.Add(sent)
	c.received.Add(received)
}

// countTokensStage parses the provider's response body via
// provider.ParseUsage and adds the resulting sent/received tokens to
// counter.
func countTokensStage(logger *zap.Logger, counter *tokenCounter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &bodyRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			body := rec.body.Bytes()
			if rec.Header().Get("Content-Encoding") == "gzip" {
				gr, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					logger.Warn(fmt.Sprintf("could not gunzip response body: %s", err))
					return
				}
				defer gr.Close()
				if body, err = io.ReadAll(gr); err != nil {
					logger.Warn(fmt.Sprintf("could not gunzip response body: %s", err))
					return
				}
			}

			if sent, received, ok := provider.ParseUsage(body); ok {
				counter.add(sent, received)
			} else {
				logger.Warn("could not find token usage in payload")
			}
		})
	}
}

// bodyRecorder copies everything written to it into body while still
// writing through to the underlying ResponseWriter, so streaming clients
// keep working.
type bodyRecorder struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (r *bodyRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *bodyRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
