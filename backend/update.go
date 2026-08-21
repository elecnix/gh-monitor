package backend

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Status is the distilled state of a Target at one observation. The concrete
// types are per-kind (a pull request status, an issue status, and so on);
// this interface exists so an Update can carry any of them.
//
// A Status is optional on an Update. A backend that is told a specific thing
// changed, without re-reading the whole target, leaves it nil — the renderer
// then works from the Event alone.
type Status interface {
	// TargetKind reports which kind of target this status describes.
	TargetKind() Kind
}

// Update is one delivered observation: what changed, and the state it was
// observed in.
type Update struct {
	// ID is a stable identity for this update. When two Updates carry the
	// same non-empty ID the second is a redelivery and is dropped, so a
	// backend with at-least-once delivery does not notify twice. An empty ID
	// disables deduplication for that update.
	ID string `json:"id,omitempty"`

	// Target is the thing this update is about.
	Target Target `json:"target"`

	// Event is the change itself.
	Event Event `json:"event"`

	// Status is the target's state at observation time, or nil when the
	// backend does not have one.
	Status Status `json:"-"`

	// Cursor is an opaque resume token. When non-empty a caller may pass it
	// back as WatchOptions.Since to resume from this point.
	Cursor string `json:"cursor,omitempty"`

	// At is when the change was observed.
	At time.Time `json:"at"`

	// Terminal marks the target as finished — a merged or closed pull
	// request, a completed run, a closed issue. The Source closes its channel
	// after delivering a terminal update.
	Terminal bool `json:"terminal,omitempty"`

	// RawStatus holds the encoded Status when an Update arrives from an
	// external backend and no decoder is registered for its kind. It lets a
	// consumer pass the payload through rather than lose it silently.
	RawStatus json.RawMessage `json:"status,omitempty"`
}

// WatchOptions configures a Watch call. Every field is advisory: a backend
// that cannot honour one ignores it rather than failing.
type WatchOptions struct {
	// Since is an opaque cursor from a previous Update. Empty means "start
	// from now" — deliver changes from this moment on.
	Since string `json:"since,omitempty"`

	// Kinds narrows the event types the caller cares about. Empty means all.
	// A backend may deliver more than requested; the caller filters again.
	Kinds []EventType `json:"kinds,omitempty"`

	// Interval is the caller's preferred cadence. A backend that polls uses
	// it; one that does not ignores it.
	Interval time.Duration `json:"interval,omitempty"`

	// Timeout stops the watch after this duration. Zero means run until the
	// target is terminal or the context is cancelled.
	Timeout time.Duration `json:"timeout,omitempty"`

	// Once asks for the target's current actionable state rather than an
	// ongoing watch: deliver what is true now, then close the channel. A
	// Source that cannot distinguish the two should emit its current state
	// and close, which is the useful approximation.
	Once bool `json:"once,omitempty"`

	// IgnoredAuthors drops activity by these logins before it is reported.
	IgnoredAuthors []string `json:"ignored_authors,omitempty"`

	// AnnotationLevels limits which check-annotation severities are reported
	// (for example "warning", "failure"). Empty means the backend's default.
	AnnotationLevels []string `json:"annotation_levels,omitempty"`

	// RepeatUnresolved asks for still-open items to be re-reported on every
	// observation rather than only when they first appear.
	RepeatUnresolved bool `json:"repeat_unresolved,omitempty"`

	// ResumeID identifies one continuous watch across reconnects. A client
	// that re-establishes a dropped stream re-sends the same ID, and a backend
	// that kept the watcher's state (the shared-poller daemon, across an
	// upgrade handoff) resumes from the baseline it saw last instead of
	// replaying everything it currently knows. Empty means the watch has no
	// history to resume.
	ResumeID string `json:"resume_id,omitempty"`
}

// ---------------------------------------------------------------------------
// Status decoding for updates that arrive encoded
// ---------------------------------------------------------------------------

var (
	statusMu       sync.RWMutex
	statusDecoders = map[Kind]func([]byte) (Status, error){}
)

// RegisterStatusDecoder teaches this package how to decode a Status of the
// given kind from JSON, so Updates that arrive from an external backend carry
// a usable Status rather than opaque bytes. The built-in status types
// register themselves; an out-of-tree backend with its own status types may
// register those too.
func RegisterStatusDecoder(k Kind, decode func([]byte) (Status, error)) {
	statusMu.Lock()
	defer statusMu.Unlock()
	statusDecoders[k] = decode
}

// DecodeStatus decodes raw into the Status type registered for k. It returns
// a nil Status and no error when no decoder is registered — an unknown status
// shape is a reason to render from the Event alone, not to drop the update.
func DecodeStatus(k Kind, raw []byte) (Status, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	statusMu.RLock()
	decode := statusDecoders[k]
	statusMu.RUnlock()
	if decode == nil {
		return nil, nil
	}
	st, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s status: %w", k, err)
	}
	return st, nil
}

// MarshalJSON encodes the Update with its Status inlined, so the wire form is
// a single self-contained object.
func (u Update) MarshalJSON() ([]byte, error) {
	type alias Update // avoid recursing into this method
	out := alias(u)
	if u.Status != nil {
		raw, err := json.Marshal(u.Status)
		if err != nil {
			return nil, fmt.Errorf("encode status: %w", err)
		}
		out.RawStatus = raw
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes an Update, resolving RawStatus into a typed Status
// when a decoder is registered for the target's kind. When none is, RawStatus
// is preserved so the payload is passed through rather than lost.
func (u *Update) UnmarshalJSON(data []byte) error {
	type alias Update
	var in alias
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*u = Update(in)
	st, err := DecodeStatus(u.Target.Kind, u.RawStatus)
	if err != nil {
		return err
	}
	if st != nil {
		u.Status = st
		u.RawStatus = nil
	}
	return nil
}
