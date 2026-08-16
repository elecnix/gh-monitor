package monitor

import (
	"fmt"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/prefs"
)

// update builds the Update for one observation of the target.
func (o *RunOptions) update(status backend.Status, ev Event) backend.Update {
	// A typed-nil status pointer would satisfy the interface while being nil
	// underneath, so callers with nothing to report pass an untyped nil.
	return backend.Update{
		Target:   TargetOf(o.Identity),
		Event:    ev,
		Status:   status,
		At:       o.now(),
		Terminal: isTerminalEvent(ev.Type),
	}
}

// notice builds an Update carrying a diagnostic the loop raises about itself
// rather than about the target — an API surface that could not be read, or a
// surface it stopped watching to stay inside a tight budget.
func (o *RunOptions) notice(msg string) backend.Update {
	return backend.Update{
		Target: TargetOf(o.Identity),
		Event:  Event{Type: EventDegraded, Notice: msg},
		At:     o.now(),
	}
}

// renderTo adapts a Notification consumer to the Update-emitting loops.
func (o RunOptions) renderTo(emit func(Notification)) func(backend.Update) {
	return func(u backend.Update) {
		emit(Render(u, o.Prefs, o.Interval))
	}
}

// isTerminalEvent reports whether an event marks the target as finished, so
// nothing further will be observed about it.
func isTerminalEvent(t EventType) bool {
	switch t {
	case EventMerged, EventClosed, EventIssueClosed, EventRunCompleted:
		return true
	}
	return false
}

// Render turns an Update into the Notification a consumer sees: it selects the
// template for the event kind, interpolates it against the target's state, and
// fills in the structured fields relevant to that kind.
//
// It is the whole presentation layer, and it is deliberately outside the watch
// loop: every backend's updates render the same way, against the same user
// templates, whether they were discovered by polling or handed to us.
func Render(u backend.Update, p prefs.Preferences, interval time.Duration) Notification {
	opts := RunOptions{
		Identity: IdentityOf(u.Target),
		Prefs:    p,
		Interval: interval,
		Now:      func() time.Time { return u.At },
	}

	// A notice carries its own fully-rendered message: there is no target
	// state to interpolate, so no template applies.
	if u.Event.Notice != "" {
		return Notification{
			Type:      string(u.Event.Type),
			PRLabel:   degradedLabel(opts),
			Message:   u.Event.Notice,
			Timestamp: u.At,
		}
	}
	if u.Event.Type == EventDegraded {
		label := degradedLabel(opts)
		return Notification{
			Type:    string(EventDegraded),
			PRLabel: label,
			Message: fmt.Sprintf("⚠️ API degraded (%s) on %s: %s",
				u.Event.DegradedSurface, label, u.Event.DegradedMessage),
			Timestamp: u.At,
		}
	}

	typ := string(u.Event.Type)
	var n Notification
	switch u.Target.Kind {
	case backend.KindIssue:
		st, _ := u.Status.(*IssueStatus)
		n = renderNotificationIssue(opts, st, typ, u.Event)
	case backend.KindRun:
		st, _ := u.Status.(*RunStatus)
		n = renderNotificationRun(opts, st, typ, u.Event)
	case backend.KindRef, backend.KindCommit:
		st, _ := u.Status.(*RefStatus)
		n = renderNotificationRef(opts, st, typ, u.Event)
	case backend.KindRepo:
		st, _ := u.Status.(*RepoStatus)
		n = renderNotificationRepo(opts, st, typ, u.Event)
	default:
		st, _ := u.Status.(*PRStatus)
		n = renderNotificationPR(opts, st, typ, u.Event)
	}

	// A backend that already has a richer body than the renderer can build
	// keeps it; this is how the failed-run log snippet reaches the consumer.
	if u.Event.Detail != "" {
		n.Detail = u.Event.Detail
	}
	return n
}
