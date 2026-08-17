package backend

// defaultDedupWindow is how many recent update IDs a Deduper remembers. A
// redelivery arrives close behind the original, so a bounded window catches it
// while keeping memory flat on a watch that runs for weeks.
const defaultDedupWindow = 4096

// Deduper drops updates it has already delivered, identified by Update.ID.
//
// A backend with at-least-once delivery will repeat itself, and a repeat that
// reaches the consumer is a second notification about one thing — noise that
// costs an operator real attention. An update with an empty ID is always
// delivered: a backend that cannot identify its updates has opted out rather
// than claimed uniqueness it does not have.
//
// A Deduper is not safe for concurrent use; give each stream its own.
type Deduper struct {
	window int
	seen   map[string]struct{}
	order  []string
}

// NewDeduper returns a Deduper remembering the last window IDs. A window of
// zero or less uses the default.
func NewDeduper(window int) *Deduper {
	if window <= 0 {
		window = defaultDedupWindow
	}
	return &Deduper{
		window: window,
		seen:   make(map[string]struct{}, window),
		order:  make([]string, 0, window),
	}
}

// Allow reports whether u should be delivered, recording its ID if so.
func (d *Deduper) Allow(u Update) bool {
	if u.ID == "" {
		return true
	}
	if _, dup := d.seen[u.ID]; dup {
		return false
	}
	if len(d.order) == d.window {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}
	d.seen[u.ID] = struct{}{}
	d.order = append(d.order, u.ID)
	return true
}
