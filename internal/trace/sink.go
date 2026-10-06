package trace

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Sink carries records downstream. It must be safe for concurrent use. A
// non-nil error means the record was not accepted and is lost; the caller
// must not refuse a call because of it.
type Sink interface {
	Write(r Record) error
}

// CloseableSink is a Sink that holds resources until closed.
type CloseableSink interface {
	Sink
	Close() error
}

// LogSink writes each record as one line of JSON.
type LogSink struct {
	mu sync.Mutex
	w  io.Writer
	f  *os.File
}

// NewLogSink writes to stdout when path is "", else opens path for
// appending, creating it if needed.
func NewLogSink(path string) (*LogSink, error) {
	if path == "" {
		return &LogSink{w: os.Stdout}, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return nil, fmt.Errorf("opening trace sink file: %w", err)
	}
	return &LogSink{w: f, f: f}, nil
}

func (s *LogSink) Write(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding %s record: %w", r.recordType(), err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(line)
	return err
}

// Close closes the file, if any. It never closes stdout.
func (s *LogSink) Close() error {
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}
