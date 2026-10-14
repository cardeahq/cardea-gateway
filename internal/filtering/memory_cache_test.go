package filtering

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestNewMemoryCache_RejectsNonPositiveCapacity(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
	}{
		{"zero", 0},
		{"negative", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewMemoryCache(tt.capacity); err == nil {
				t.Fatalf("NewMemoryCache(%d) = nil error, want error", tt.capacity)
			}
		})
	}
}

func TestMemoryCache_SetGetRoundTrip(t *testing.T) {
	c, err := NewMemoryCache(10)
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	entry := Entry{Category: CategoryEmail, Value: exEmail}
	if err := c.Set(ctx, "k1", entry, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, "k1")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v, %v, want a hit", got, ok, err)
	}
	if got != entry {
		t.Fatalf("Get = %+v, want %+v", got, entry)
	}
}

func TestMemoryCache_GetMissForUnknownKey(t *testing.T) {
	c, err := NewMemoryCache(10)
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, ok, err := c.Get(context.Background(), "never-set")
	if err != nil || ok {
		t.Fatalf("Get(never-set) = ok=%v err=%v, want a miss with no error", ok, err)
	}
}

func TestMemoryCache_EntryPastTTLIsAMissBeforeTheSweepRuns(t *testing.T) {
	c, err := NewMemoryCache(10)
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Set(ctx, "k1", Entry{Category: CategoryEmail, Value: exEmail}, 10*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // past the TTL, well under the 30s sweep interval

	_, ok, err := c.Get(ctx, "k1")
	if err != nil || ok {
		t.Fatalf("Get after TTL expiry = ok=%v err=%v, want a miss even though the sweep has not run yet", ok, err)
	}
}

func TestMemoryCache_PastCapacityEvictsTheOldestInsertedEntryNotTheLeastRecentlyUsed(t *testing.T) {
	c, err := NewMemoryCache(3)
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	set := func(key string) {
		if err := c.Set(ctx, key, Entry{Category: CategoryEmail, Value: key}, time.Hour); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
	}
	hit := func(key string) bool {
		_, ok, _ := c.Get(ctx, key)
		return ok
	}

	set("a") // oldest
	set("b")
	set("c") // cache full at capacity 3

	// Repeatedly accessing b must not protect it from eviction: this cache
	// is FIFO by insertion order, not LRU.
	for range 5 {
		hit("b")
	}

	set("d") // evicts "a", the oldest inserted
	if hit("a") {
		t.Fatal("a survived past capacity, want the oldest inserted entry evicted")
	}
	if !hit("b") || !hit("c") || !hit("d") {
		t.Fatal("b, c and d should all still be present after a single eviction")
	}

	set("e") // evicts "b" next, despite its Get accesses above
	if hit("b") {
		t.Fatal("b survived its turn for eviction because it was recently Get: this cache must be FIFO, not LRU")
	}
	if !hit("c") || !hit("d") || !hit("e") {
		t.Fatal("c, d and e should all still be present")
	}
}

func TestMemoryCache_CloseThenSetAndGetError(t *testing.T) {
	c, err := NewMemoryCache(10)
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	ctx := context.Background()
	if err := c.Set(ctx, "k1", Entry{Category: CategoryEmail, Value: exEmail}, time.Minute); err != nil {
		t.Fatalf("Set before Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := c.Set(ctx, "k2", Entry{Category: CategoryEmail, Value: exEmail}, time.Minute); err == nil {
		t.Fatal("Set after Close = nil error, want error")
	}
	if _, _, err := c.Get(ctx, "k1"); err == nil {
		t.Fatal("Get after Close = nil error, want error")
	}
}

func TestMemoryCache_CloseStopsTheSweepGoroutine(t *testing.T) {
	c, err := NewMemoryCache(10)
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	during := stableGoroutineCount(t)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := stableGoroutineCount(t)

	if after >= during {
		t.Fatalf("goroutines while open=%d, after Close=%d; want fewer after Close (the sweep goroutine should have stopped)", during, after)
	}
}

// stableGoroutineCount polls runtime.NumGoroutine until it reads the same
// value several times in a row (or a deadline passes), so a reading isn't
// taken mid-transition of some unrelated goroutine the test runner itself
// spins up or tears down.
func stableGoroutineCount(t *testing.T) int {
	t.Helper()
	const stableReadings = 5
	last, streak := -1, 0
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n == last {
			streak++
			if streak >= stableReadings {
				return n
			}
		} else {
			last, streak = n, 1
		}
		if time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}
