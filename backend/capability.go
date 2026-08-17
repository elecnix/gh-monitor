package backend

import "context"

// Capability names one half of the backend surface. A backend registers the
// capabilities it implements and nothing else.
type Capability string

const (
	// CapSource means the backend delivers Updates (notifications).
	CapSource Capability = "source"
	// CapReader means the backend returns the current Status (queries).
	CapReader Capability = "reader"
)

// Source delivers Updates for a Target. It is the notification capability.
type Source interface {
	// Watch returns a channel of Updates for t. The channel is closed when
	// the target reaches a terminal state, the timeout elapses, or ctx is
	// cancelled. An error is returned only when the watch could not be
	// started at all; a failure afterwards is delivered as an EventDegraded
	// update instead, because a watcher that goes quiet must never read as
	// all-clear.
	Watch(ctx context.Context, t Target, opts WatchOptions) (<-chan Update, error)
}

// Reader returns the current Status of a Target. It is the query capability,
// used by `--once` and by any Source that discovers state by reading it.
type Reader interface {
	Read(ctx context.Context, t Target) (Status, error)
}

// Provider is what a backend implements to register itself. It registers only
// the capabilities it has, for only the kinds it covers.
type Provider interface {
	// Name identifies the backend in `gh monitor backends`, in --backend, and
	// in diagnostics.
	Name() string
	// Register adds this backend's capabilities to the registry.
	Register(*Registry) error
}

// SourceFunc adapts a plain function to the Source interface.
type SourceFunc func(ctx context.Context, t Target, opts WatchOptions) (<-chan Update, error)

// Watch implements Source.
func (f SourceFunc) Watch(ctx context.Context, t Target, opts WatchOptions) (<-chan Update, error) {
	return f(ctx, t, opts)
}

// ReaderFunc adapts a plain function to the Reader interface.
type ReaderFunc func(ctx context.Context, t Target) (Status, error)

// Read implements Reader.
func (f ReaderFunc) Read(ctx context.Context, t Target) (Status, error) { return f(ctx, t) }

// allCapabilities is every capability in canonical order. Listings and
// diagnostics iterate it so their output is stable.
var allCapabilities = []Capability{
	CapSource, CapReader,
	CapThreads, CapReview, CapComments, CapDraft, CapReactions,
}
