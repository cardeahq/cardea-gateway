package trace

import (
	"errors"
	"slices"
	"sync"
	"testing"
)

// stubSink records the order it is written and closed in, across sinks, and
// fails on demand.
type stubSink struct {
	name     string
	events   *stubEvents
	writeErr error
	closeErr error
}

type stubEvents struct {
	mu  sync.Mutex
	log []string
}

func (e *stubEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *stubEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

func (s *stubSink) Write(r Record) error {
	s.events.add("write:" + s.name + ":" + r.recordType())
	return s.writeErr
}

func (s *stubSink) Close() error {
	s.events.add("close:" + s.name)
	return s.closeErr
}

func TestMultiSink_WritesEveryRecordToEverySinkInOrder(t *testing.T) {
	ev := &stubEvents{}
	m := NewMultiSink(&stubSink{name: "a", events: ev}, &stubSink{name: "b", events: ev}, &stubSink{name: "c", events: ev})

	if err := m.Write(minimalRequestRecord("ak_0000000000000001")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := m.Write(minimalResponseRecord("ak_0000000000000001")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := []string{
		"write:a:request", "write:b:request", "write:c:request",
		"write:a:response", "write:b:response", "write:c:response",
	}
	if got := ev.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestMultiSink_AnErrorFromOneSinkDoesNotStopTheOthers(t *testing.T) {
	tests := []struct {
		name    string
		failing int // index of the sink that fails
	}{
		{"first sink fails", 0},
		{"middle sink fails", 1},
		{"last sink fails", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := &stubEvents{}
			errBoom := errors.New("boom")
			sinks := []*stubSink{{name: "a", events: ev}, {name: "b", events: ev}, {name: "c", events: ev}}
			sinks[tt.failing].writeErr = errBoom
			m := NewMultiSink(sinks[0], sinks[1], sinks[2])

			err := m.Write(minimalRequestRecord("ak_0000000000000001"))
			if !errors.Is(err, errBoom) {
				t.Fatalf("Write error = %v, want it to carry the failing sink's error", err)
			}
			want := []string{"write:a:request", "write:b:request", "write:c:request"}
			if got := ev.snapshot(); !slices.Equal(got, want) {
				t.Fatalf("events = %v, want every sink written despite the failure: %v", got, want)
			}
		})
	}
}

func TestMultiSink_JoinsTheErrorsOfEverySinkThatFailed(t *testing.T) {
	ev := &stubEvents{}
	errA, errC := errors.New("disk full"), errors.New("platform down")
	m := NewMultiSink(
		&stubSink{name: "a", events: ev, writeErr: errA},
		&stubSink{name: "b", events: ev},
		&stubSink{name: "c", events: ev, writeErr: errC},
	)
	err := m.Write(minimalResponseRecord("ak_0000000000000001"))
	if !errors.Is(err, errA) || !errors.Is(err, errC) {
		t.Fatalf("Write error = %v, want both %q and %q joined", err, errA, errC)
	}
}

func TestMultiSink_NoErrorWhenEverySinkAccepts(t *testing.T) {
	ev := &stubEvents{}
	m := NewMultiSink(&stubSink{name: "a", events: ev}, &stubSink{name: "b", events: ev})
	if err := m.Write(minimalRequestRecord("ak_0000000000000001")); err != nil {
		t.Fatalf("Write = %v, want nil", err)
	}
}

func TestMultiSink_CloseClosesEverySinkAndJoinsTheErrors(t *testing.T) {
	ev := &stubEvents{}
	errA, errB := errors.New("close a"), errors.New("close b")
	m := NewMultiSink(
		&stubSink{name: "a", events: ev, closeErr: errA},
		&stubSink{name: "b", events: ev, closeErr: errB},
		&stubSink{name: "c", events: ev},
	)

	err := m.Close()
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("Close error = %v, want both close errors joined", err)
	}
	want := []string{"close:a", "close:b", "close:c"}
	if got := ev.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want every sink closed even after one failed: %v", got, want)
	}
}

func TestMultiSink_CloseWithNoErrorsReturnsNil(t *testing.T) {
	ev := &stubEvents{}
	m := NewMultiSink(&stubSink{name: "a", events: ev}, &stubSink{name: "b", events: ev})
	if err := m.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}
