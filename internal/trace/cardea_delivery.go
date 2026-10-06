package trace

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type verdict int

const (
	delivered verdict = iota
	retry
	refusedBatch
	refusedCredential
)

const (
	maxRetryAfter   = 5 * time.Minute
	maxBackoffSteps = 30 // the schedule caps at 30 times its base
)

func classify(status int) verdict {
	switch {
	case status >= 200 && status < 300:
		return delivered
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return refusedCredential
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return retry
	}
	return refusedBatch
}

// retryAfter reads Retry-After as delta-seconds or an HTTP-date. It reports
// no wait for an absent, malformed, negative or past value, and caps the
// rest at five minutes.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d <= 0 {
		return 0, false
	}
	return min(d, maxRetryAfter), true
}

// backoff is 1s * 2^attempt capped at 30s, times a random factor in
// [0.5, 1], and at least floor.
func backoff(attempt int, floor time.Duration) time.Duration {
	return backoffFrom(time.Second, attempt, floor)
}

func backoffFrom(base time.Duration, attempt int, floor time.Duration) time.Duration {
	limit := base * maxBackoffSteps
	d := base
	for i := 0; i < attempt && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	d = d/2 + rand.N(d/2+1)
	return max(d, floor)
}

// reason reduces a transport error to a fixed vocabulary: net/http errors
// embed the URL, which must not reach the logs.
func reason(err error) string {
	var dns *net.DNSError
	var ne net.Error
	var verify *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.As(err, &verify), errors.As(err, &unknownAuth), errors.As(err, &hostname), errors.As(err, &invalid):
		return "tls"
	}
	return "network error"
}

// limiter collapses a repeating event into at most one line per interval,
// carrying the count accumulated since the last line.
type limiter struct {
	every time.Duration

	mu      sync.Mutex
	last    time.Time
	pending int
}

// hit records one event and reports whether to log now, with the count.
func (l *limiter) hit() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending++
	if !l.last.IsZero() && time.Since(l.last) < l.every {
		return 0, false
	}
	l.last = time.Now()
	n := l.pending
	l.pending = 0
	return n, true
}

// flush returns the count not yet logged.
func (l *limiter) flush() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.pending
	l.pending = 0
	return n
}

func (l *limiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = time.Time{}
	l.pending = 0
}
