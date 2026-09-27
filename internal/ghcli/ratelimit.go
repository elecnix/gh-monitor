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
	readings map[string]RateLimitReading
}

// NewRateLimitStore returns an empty store.
func NewRateLimitStore() *RateLimitStore {
	return &RateLimitStore{readings: make(map[string]RateLimitReading)}
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
	s.mu.Lock()
	s.readings[storeKey(host, resource)] = r
	s.mu.Unlock()
}

// Latest returns the most recent reading for host and resource.
func (s *RateLimitStore) Latest(host, resource string) (RateLimitReading, bool) {
	if s == nil {
		return RateLimitReading{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.readings[storeKey(host, resource)]
	return r, ok
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
