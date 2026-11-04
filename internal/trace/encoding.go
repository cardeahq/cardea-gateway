package trace

import (
	"compress/gzip"
	"compress/zlib"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// decoders holds the response content encodings tracing reads. The tap
// decodes only for itself: the caller always receives the bytes the
// provider sent.
var decoders = map[string]func(io.Reader) (io.ReadCloser, error){
	"gzip": func(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) },
	"br":   func(r io.Reader) (io.ReadCloser, error) { return io.NopCloser(brotli.NewReader(r)), nil },
	"zstd": func(r io.Reader) (io.ReadCloser, error) {
		// One goroutine-free decoder per call, closed with the reader. HTTP
		// caps the window at 8 MB (RFC 9659); a larger one is refused.
		d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(8<<20))
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	},
	// HTTP's "deflate" is the zlib format (RFC 9110, 8.4.1.2).
	"deflate": func(r io.Reader) (io.ReadCloser, error) { return zlib.NewReader(r) },
}
