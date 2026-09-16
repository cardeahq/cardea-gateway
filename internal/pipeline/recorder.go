package pipeline

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
)

// bodyRecorder writes every byte through to the real ResponseWriter
// immediately, so streaming responses keep streaming, while also
// retaining up to maxBody bytes for stage observation.
type bodyRecorder struct {
	http.ResponseWriter
	maxBody     int64
	buf         bytes.Buffer
	truncated   bool
	status      int
	wroteHeader bool
}

func newBodyRecorder(w http.ResponseWriter, maxBody int64) *bodyRecorder {
	return &bodyRecorder{ResponseWriter: w, maxBody: maxBody}
}

func (r *bodyRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
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

	return r.ResponseWriter.Write(b)
}

func (r *bodyRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
