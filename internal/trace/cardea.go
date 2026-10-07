package trace

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
	"unicode"

	"go.uber.org/zap"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("cardea sink is closed")

const (
	dropLogEvery   = 10 * time.Second
	retryLogEvery  = 60 * time.Second
	closingBackoff = 250 * time.Millisecond
	drainLimit     = 4 << 10
)

// CardeaSink delivers records to the Cardea platform. Write only enqueues;
// one background goroutine delivers.
type CardeaSink struct {
	cfg      config.CardeaSinkConfig
	key      string
	instance string
	host     string
	logger   *zap.Logger
	client   *http.Client

	// Test seams.
	sendBatch   func(ctx context.Context, b *batch) (status int, header http.Header, err error)
	backoffBase time.Duration
	now         func() time.Time

	ctx     context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	closing chan struct{}
	done    chan struct{}

	closeOnce sync.Once
	closeErr  error

	dropLim  limiter
	credLim  limiter
	retryLim limiter
	failures int // worker goroutine only

	mu         sync.Mutex
	queue      []entry
	queueBytes int
	seq        uint64
	closed     bool
	delivered  int
	lost       int
	lostAtExit int
}

type entry struct {
	seq  uint64
	line []byte
	at   time.Time
}

type batch struct {
	first, last uint64
	n, size     int
	body        []byte
}

// NewCardeaSink validates cfg, reads the API key from cfg.APIKeyEnv, logs
// the startup line, and starts the worker. It makes no network call. It
// fails when the key variable is unset, empty, or contains whitespace or
// control characters (header injection), naming the variable but never its
// value.
func NewCardeaSink(cfg config.CardeaSinkConfig, logger *zap.Logger) (*CardeaSink, error) {
	key, err := readAPIKey(cfg.APIKeyEnv)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, errors.New("cardea sink endpoint is not a valid URL")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("generating cardea sink instance identity: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	s := &CardeaSink{
		cfg:      cfg,
		key:      key,
		instance: hex.EncodeToString(id[:]),
		host:     u.Host,
		logger:   logger,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		backoffBase: time.Second,
		now:         time.Now,
		wake:        make(chan struct{}, 1),
		closing:     make(chan struct{}),
		done:        make(chan struct{}),
		dropLim:     limiter{every: dropLogEvery},
		credLim:     limiter{every: dropLogEvery},
		retryLim:    limiter{every: retryLogEvery},
	}
	s.sendBatch = s.send
	s.ctx, s.cancel = context.WithCancel(context.Background())

	logger.Info("cardea sink started", zap.String("host", s.host), zap.String("instance", s.instance))
	go s.run()
	return s, nil
}

func readAPIKey(env string) (string, error) {
	key, ok := os.LookupEnv(env)
	if !ok || key == "" {
		return "", fmt.Errorf("environment variable %s is unset or empty", env)
	}
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("environment variable %s holds a key with whitespace or control characters", env)
		}
	}
	return key, nil
}

// InstanceID returns the identity sent with every batch.
func (s *CardeaSink) InstanceID() string { return s.instance }

// Write marshals the record and enqueues it. It never touches the network.
// A full queue discards the record, consumes its seq and returns nil.
func (s *CardeaSink) Write(r Record) error {
	line, marshalErr := json.Marshal(r)
	dropped, err := s.enqueue(r, line, marshalErr)
	if dropped {
		if n, ok := s.dropLim.hit(); ok {
			s.logger.Error("cardea sink queue is full; records dropped",
				zap.Int("count", n), zap.String("host", s.host), zap.String("instance", s.instance))
		}
	}
	return err
}

func (s *CardeaSink) enqueue(r Record, line []byte, marshalErr error) (dropped bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ErrClosed
	}
	s.seq++
	if marshalErr != nil {
		s.lost++
		return false, fmt.Errorf("encoding %s record: %w", r.recordType(), marshalErr)
	}
	e := make([]byte, 0, len(line)+32)
	e = append(e, `{"seq":`...)
	e = strconv.AppendUint(e, s.seq, 10)
	e = append(e, `,"record":`...)
	e = append(e, line...)
	e = append(e, '}', '\n')

	if len(s.queue) >= s.cfg.QueueSize || int64(s.queueBytes+len(e)) > s.cfg.QueueMaxBytes {
		s.lost++
		return true, nil
	}
	s.queue = append(s.queue, entry{seq: s.seq, line: e, at: time.Now()})
	s.queueBytes += len(e)
	if len(s.queue) == 1 || len(s.queue) >= s.cfg.BatchMaxRecords || s.queueBytes >= s.cfg.BatchMaxBytes {
		s.signal()
	}
	return false, nil
}

func (s *CardeaSink) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Close drains the queue for up to shutdown_timeout, then abandons what is
// left. It is idempotent and returns after the worker has exited.
func (s *CardeaSink) Close() error {
	s.closeOnce.Do(s.shutdown)
	return s.closeErr
}

func (s *CardeaSink) shutdown() {
	if s.cfg.ShutdownTimeout <= 0 {
		s.cancel()
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	close(s.closing)
	s.signal()

	if s.cfg.ShutdownTimeout > 0 {
		t := time.NewTimer(s.cfg.ShutdownTimeout)
		select {
		case <-s.done:
		case <-t.C:
			s.cancel()
		}
		t.Stop()
	}
	<-s.done
	s.cancel()
	s.client.CloseIdleConnections()

	if n := s.dropLim.flush(); n > 0 {
		s.logger.Error("cardea sink queue was full; records dropped",
			zap.Int("count", n), zap.String("host", s.host), zap.String("instance", s.instance))
	}
	if n := s.credLim.flush(); n > 0 {
		s.logger.Error("cardea platform refused the credential; records lost",
			zap.Int("count", n), zap.String("host", s.host), zap.String("instance", s.instance))
	}

	s.mu.Lock()
	delivered, lost := s.delivered, s.lost
	abandoned := s.lostAtExit
	s.mu.Unlock()
	s.logger.Info("cardea sink closed",
		zap.Int("delivered", delivered), zap.Int("lost", lost), zap.String("instance", s.instance))
	if abandoned > 0 {
		s.closeErr = fmt.Errorf("cardea sink closed with %d records undelivered", abandoned)
	}
}

func (s *CardeaSink) run() {
	defer close(s.done)
	defer s.abandon()
	for {
		b, ok := s.nextBatch()
		if !ok {
			return
		}
		s.handle(b)
	}
}

// abandon counts whatever is still queued when the worker exits as lost.
func (s *CardeaSink) abandon() {
	s.mu.Lock()
	n := len(s.queue)
	s.lost += n
	s.lostAtExit = n
	s.queue, s.queueBytes = nil, 0
	s.mu.Unlock()
	if n > 0 {
		s.logger.Error("cardea sink shut down with undelivered records; records lost",
			zap.Int("count", n), zap.String("host", s.host), zap.String("instance", s.instance))
	}
}

// nextBatch waits until a batch is due and returns it, still queued. It
// reports false when the worker must exit.
func (s *CardeaSink) nextBatch() (*batch, bool) {
	for {
		if s.ctx.Err() != nil {
			return nil, false
		}
		s.mu.Lock()
		n := len(s.queue)
		wait := time.Duration(-1)
		switch {
		case n == 0 && s.closed:
			s.mu.Unlock()
			return nil, false
		case n == 0:
		case s.closed || n >= s.cfg.BatchMaxRecords || s.queueBytes >= s.cfg.BatchMaxBytes:
			b := s.takeLocked()
			s.mu.Unlock()
			return b, true
		default:
			age := time.Since(s.queue[0].at)
			if age >= s.cfg.BatchMaxDelay {
				b := s.takeLocked()
				s.mu.Unlock()
				return b, true
			}
			wait = s.cfg.BatchMaxDelay - age
		}
		s.mu.Unlock()

		var t *time.Timer
		var timer <-chan time.Time
		if wait >= 0 {
			t = time.NewTimer(wait)
			timer = t.C
		}
		select {
		case <-s.wake:
		case <-timer:
		case <-s.ctx.Done():
		}
		if t != nil {
			t.Stop()
		}
	}
}

// takeLocked builds the head batch without removing its entries. The first
// entry always goes; each next one only while the batch stays within both
// limits.
func (s *CardeaSink) takeLocked() *batch {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	b := &batch{first: s.queue[0].seq}
	for _, e := range s.queue {
		if b.n > 0 && (b.n+1 > s.cfg.BatchMaxRecords || b.size+len(e.line) > s.cfg.BatchMaxBytes) {
			break
		}
		_, _ = zw.Write(e.line)
		b.n++
		b.size += len(e.line)
		b.last = e.seq
	}
	_ = zw.Close()
	b.body = buf.Bytes()
	return b
}

// handle delivers b, retrying until it is delivered, refused, or the sink
// is cancelled. A panic drops the batch in hand.
func (s *CardeaSink) handle(b *batch) {
	defer func() {
		if recover() == nil {
			return
		}
		s.pop(b, false)
		s.logger.Error("cardea sink failed while preparing a batch; records lost",
			zap.Int("count", b.n), zap.String("host", s.host), zap.String("instance", s.instance))
	}()

	for attempt := 0; s.ctx.Err() == nil; attempt++ {
		ctx, cancel := context.WithTimeout(s.ctx, s.cfg.RequestTimeout)
		status, header, err := s.sendBatch(ctx, b)
		cancel()
		if s.ctx.Err() != nil {
			return
		}

		var floor time.Duration
		if err != nil {
			s.retryFailed(zap.String("reason", reason(err)))
		} else {
			switch classify(status) {
			case delivered:
				s.pop(b, true)
				s.recovered()
				return
			case refusedCredential:
				s.pop(b, false)
				if n, ok := s.credLim.hit(); ok {
					s.logger.Error("cardea platform refused the credential; records lost",
						zap.Int("count", n), zap.Int("status", status), zap.String("host", s.host), zap.String("instance", s.instance))
				}
				return
			case refusedBatch:
				s.pop(b, false)
				s.logger.Error("cardea platform refused a batch; records lost",
					zap.Int("count", b.n), zap.Int("status", status), zap.String("host", s.host), zap.String("instance", s.instance))
				return
			}
			floor, _ = retryAfter(header, s.now())
			s.retryFailed(zap.Int("status", status))
		}
		s.sleep(max(backoffFrom(s.backoffBase, attempt, 0), floor))
	}
}

func (s *CardeaSink) retryFailed(why zap.Field) {
	s.failures++
	if n, ok := s.retryLim.hit(); ok {
		s.logger.Warn("cardea delivery failing; will retry",
			why, zap.Int("failures", n), zap.String("host", s.host), zap.String("instance", s.instance))
	}
}

func (s *CardeaSink) recovered() {
	if s.failures == 0 {
		return
	}
	s.logger.Info("cardea delivery recovered",
		zap.Int("failures", s.failures), zap.String("host", s.host), zap.String("instance", s.instance))
	s.failures = 0
	s.retryLim.reset()
}

// sleep waits d, capped at closingBackoff once the sink is closing so a drain
// does not sit out a long backoff but does not hammer the platform either,
// and cut short by cancellation.
func (s *CardeaSink) sleep(d time.Duration) {
	select {
	case <-s.closing:
		d = min(d, closingBackoff)
	default:
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-s.ctx.Done():
	}
}

// pop removes b's entries, which are still the head of the queue.
func (s *CardeaSink) pop(b *batch, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.queue[:b.n])
	s.queue = s.queue[b.n:]
	s.queueBytes -= b.size
	if ok {
		s.delivered += b.n
	} else {
		s.lost += b.n
	}
}

// send POSTs b. The response body is drained up to a bound and discarded.
func (s *CardeaSink) send(ctx context.Context, b *batch) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(b.body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Cardea-Instance", s.instance)
	req.Header.Set("X-Cardea-First-Seq", strconv.FormatUint(b.first, 10))
	req.Header.Set("X-Cardea-Last-Seq", strconv.FormatUint(b.last, 10))
	req.Header.Set("X-Cardea-Schema", Schema)
	req.Header.Set("User-Agent", "cardea-gateway")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header, nil
}
