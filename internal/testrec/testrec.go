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

import "sync"

// Recorder collects observations in the order they arrive. It is safe for
// concurrent use: several pollers or several watches report at once.
//
// A Recorder holds a mutex, so it is used through a pointer: a test that
// embeds one embeds a *Recorder.
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

// New returns a recorder and the callback that feeds it.
//
// The callback takes the arguments the watched callback takes and turns
// them into an observation with build. A test hands the callback straight
// to the thing it watches, so it never writes a one-line wrapper of its
// own — and two tests that watch different callbacks do not write the
// same wrapper twice.
func New[T any, A, B, C, D any](build func(A, B, C, D) T) (*Recorder[T], func(A, B, C, D)) {
	r := &Recorder[T]{}
	return r, func(a A, b B, c C, d D) {
		r.Add(build(a, b, c, d))
	}
}
