package filtering

import (
	"context"
	"time"
)

// Category names a built-in PII category. Fixed in this version —
// operators enable or disable one, they cannot define a new one.
type Category string

const (
	CategoryEmail  Category = "email"
	CategorySSN    Category = "ssn"
	CategoryAPIKey Category = "api_key"
)

// Entry is one detected value, held by the cache under its placeholder's
// UUID. Value is the real PII text; nothing in the Gateway logs it.
type Entry struct {
	Category Category
	Value    string
}

// Cache is the pluggable store filtering writes detected values into, for
// a later version to read back. MemoryCache is the only implementation
// this version ships; a different one is added to this package and
// wired into buildStages, the way trace.Sink implementations are.
type Cache interface {
	// Set stores entry under key (the placeholder's UUID alone, not the
	// full "[category:uuid]" string) with the given time-to-live. An
	// error here fails the call closed — see Engine.Redact.
	Set(ctx context.Context, key string, entry Entry, ttl time.Duration) error
	// Get returns the entry for key. ok is false if it was never set, has
	// expired, or was evicted.
	Get(ctx context.Context, key string) (entry Entry, ok bool, err error)
	// Close releases any resources (goroutines, connections) the cache
	// holds.
	Close() error
}

// Request is what the pipeline hands the engine for one call.
type Request struct {
	CallID string
	Path   string
	Body   []byte
}

// Result is one Redact call's outcome. Counts is nil when nothing was
// redacted (not a target body, or nothing matched); Body equals the
// input body in that case.
type Result struct {
	Body   []byte
	Counts map[Category]int
}
