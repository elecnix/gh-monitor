package ghcli

import (
	"bufio"
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitReading is the rate-limit state GitHub reported in the
// X-RateLimit-* headers of one real response. GET /rate_limit can disagree
// with these headers (issue #123), so the headers of the calls a watcher
// actually makes are the better source.
type RateLimitReading struct {
	Resource   string // "core", "graphql", "search", ...
	Limit      int
	Remaining  int
	Used       int
	Reset      time.Time // when the window resets
	ObservedAt time.Time // when the response arrived
}

// RateLimitStore records the latest reading per host and resource. It is safe
// for concurrent use.
type RateLimitStore struct {
	mu       sync.Mutex
	readings map[string][]RateLimitReading // newest last; one reset window per slice
}

// ReadingFreshness is how far back Latest looks for a higher counter within
// the newest reading's window. GitHub answers one token with two counters
// that disagree: one can read 0 while another reads thousands. A single
// exhausted or stale response must not drive the pace, so when the counters
// interleave Latest reports the highest remaining among readings no older
// than this. Real consumption in that span is small next to the discrepancy.
const ReadingFreshness = 30 * time.Second

// maxReadingHistory bounds the readings kept per host and resource.
const maxReadingHistory = 16

// GraphQLHeaders builds the X-RateLimit-* headers a GraphQL response
// carries, with the given remaining count against a limit, and a reset at
// the given time.
//
// It is the inverse of what Observe parses, and it exists so a test that
// drives a rate-limit reading does not spell out those five headers by
// hand: several packages' test suites need them, and they are always the
// same five.
func GraphQLHeaders(remaining, limit int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Resource", "graphql")
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Used", strconv.Itoa(limit-remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

// NewRateLimitStore returns an empty store.
func NewRateLimitStore() *RateLimitStore {
	return &RateLimitStore{readings: make(map[string][]RateLimitReading)}
}

// Observed is the process-wide store that clients without their own Limits
// record into. The daemon's budget guard reads it.
var Observed = NewRateLimitStore()

func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "github.com"
	}
	return host
}

func storeKey(host, resource string) string {
	return normalizeHost(host) + "\x00" + resource
}

// Observe records the reading in headers, if they carry one. Responses
// without a resource or remaining count leave the store unchanged.
func (s *RateLimitStore) Observe(host string, headers http.Header, now time.Time) {
	if s == nil || headers == nil {
		return
	}
	resource := headers.Get("X-RateLimit-Resource")
	remaining, err := strconv.Atoi(headers.Get("X-RateLimit-Remaining"))
	if resource == "" || err != nil {
		return
	}
	r := RateLimitReading{Resource: resource, Remaining: remaining, ObservedAt: now}
	r.Limit, _ = strconv.Atoi(headers.Get("X-RateLimit-Limit"))
	r.Used, _ = strconv.Atoi(headers.Get("X-RateLimit-Used"))
	if reset, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		r.Reset = time.Unix(reset, 0)
	}
	key := storeKey(host, resource)
	s.mu.Lock()
	hist := s.readings[key]
	if n := len(hist); n > 0 && !hist[n-1].Reset.Equal(r.Reset) {
		hist = nil // a new window: the old counters describe a spent budget
	}
	hist = append(hist, r)
	if len(hist) > maxReadingHistory {
		hist = hist[len(hist)-maxReadingHistory:]
	}
	s.readings[key] = hist
	s.mu.Unlock()
}

// Latest returns the freshest reading for host and resource. When readings
// of one reset window interleave within ReadingFreshness (a rise in
// remaining), it carries the highest remaining count among them.
func (s *RateLimitStore) Latest(host, resource string) (RateLimitReading, bool) {
	if s == nil {
		return RateLimitReading{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hist := s.readings[storeKey(host, resource)]
	if len(hist) == 0 {
		return RateLimitReading{}, false
	}
	r := hist[len(hist)-1]
	// Real consumption only lowers remaining. A rise inside the fresh span
	// proves two counters are interleaving, so the highest one stands in for
	// the account. A span that only falls reports its newest reading.
	best, rose, prev := r.Remaining, false, -1
	for _, h := range hist {
		if r.ObservedAt.Sub(h.ObservedAt) > ReadingFreshness {
			continue
		}
		if prev >= 0 && h.Remaining > prev {
			rose = true
		}
		prev = h.Remaining
		if h.Remaining > best {
			best = h.Remaining
		}
	}
	if rose {
		r.Remaining = best
		if r.Limit > 0 {
			r.Used = r.Limit - best
		}
	}
	return r, true
}

// splitIncluded splits the output of `gh api --include` into the response
// headers and the body. gh ends the status line with \n and header lines with
// \r\n, then prints a blank line before the body. Output that does not start
// with a status line is returned unchanged as the body.
func splitIncluded(out []byte) (http.Header, []byte) {
	if !bytes.HasPrefix(out, []byte("HTTP/")) {
		return nil, out
	}
	headers := http.Header{}
	r := bufio.NewReader(bytes.NewReader(out))
	consumed := 0
	first := true
	for {
		line, err := r.ReadBytes('\n')
		consumed += len(line)
		text := strings.TrimRight(string(line), "\r\n")
		if first {
			first = false
		} else if text == "" {
			break
		} else if k, v, ok := strings.Cut(text, ":"); ok {
			headers.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
		if err != nil {
			break
		}
	}
	return headers, out[consumed:]
}
