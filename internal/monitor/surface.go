package monitor

import "strings"

// Surface names one watched surface of a PR snapshot: a class of data the poll
// either fetches or leaves out. It is the vocabulary the degradation policy
// speaks — a tier says which surfaces it sheds, a snapshot records the ones it
// stopped fetching, and CarryForwardShed keys off the same values — so the
// three sites that used to hold their own copy of the literals now share one.
//
// The underlying kind is string: the values are stable identifiers that reach
// notifications and the `shed_surfaces` JSON field unchanged (issue #135).
type Surface string

const (
	// SurfaceAnnotations is check-run annotations.
	SurfaceAnnotations Surface = "annotations"
	// SurfaceReviews is the review decision (approved / changes requested /
	// dismissed) and its author.
	SurfaceReviews Surface = "reviews"
	// SurfaceReviewThreads is unresolved review threads with their comments.
	SurfaceReviewThreads Surface = "review threads"
	// SurfaceComments is general issue comments.
	SurfaceComments Surface = "comments"
)

// Surfaces is an ordered list of surfaces, in the operator's priority order
// (least valuable first) when it comes from a tier.
type Surfaces []Surface

// Has reports whether the list names x.
func (s Surfaces) Has(x Surface) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// Names projects the list onto plain strings — the shape the JSON field
// `shed_surfaces` and backend.Event.DegradedSurfaces carry. It is the single
// place that conversion happens, so a caller that needs a []string asks for
// one instead of relying on a []string having been threaded through.
func (s Surfaces) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = string(v)
	}
	return out
}

// String renders the list for a notice sentence, comma separated. An empty
// list renders empty, which keeps callers from emitting a dangling separator.
func (s Surfaces) String() string { return strings.Join(s.Names(), ", ") }
