// Package testrec provides a concurrency-safe recorder for the observations
// a test collects from a callback.
//
// It exists so the tests that watch a callback — a hub's fetch observer, a
// router's failover observer — do not each write the same "a mutex, a slice,
// an append, and a copy-on-read accessor" helper. The recorder is generic
// over the observation type, so each test still names its own shape.
//
// It lives in its own package so several packages' test suites can share it
// without an import cycle, the same reason internal/mux/muxtest exists.
package testrec

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Recorder collects observations in the order they arrive. It is safe for
// concurrent use: several pollers or several watches report at once.
type Recorder[T any] struct {
	mu    sync.Mutex
	items []T
}

// Add records one observation.
func (r *Recorder[T]) Add(item T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, item)
}

// All returns the recorded observations, oldest first. The caller gets its
// own copy, so it can read it while more observations arrive.
func (r *Recorder[T]) All() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]T(nil), r.items...)
}

// Len returns how many observations have been recorded.
func (r *Recorder[T]) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

// GraphQLHeaders builds the X-RateLimit-* headers a GraphQL response
// carries, with the given remaining count against a limit, and a reset that
// far in the future.
//
// It exists because a test that drives a rate-limit reading writes these
// five headers by hand, and two packages' test suites do it: one for a
// budget guard, one for a hub poller. The spelling is identical, so it
// lives here rather than twice.
func GraphQLHeaders(remaining, limit int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Resource", "graphql")
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Used", strconv.Itoa(limit-remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}
