package broker

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// TransitionKind says what a Transition records.
type TransitionKind string

const (
	// TransitionConnecting records one connect attempt starting.
	TransitionConnecting TransitionKind = "connecting"
	// TransitionHealthy records a session that connected and subscribed.
	TransitionHealthy TransitionKind = "healthy"
	// TransitionDegraded records a session that ended, or a connect attempt
	// that failed, and the delay before the next attempt.
	TransitionDegraded TransitionKind = "degraded"
	// TransitionOutage records the moment a degraded transport passed the
	// outage threshold. It is raised once per episode.
	TransitionOutage TransitionKind = "outage"
	// TransitionRecovered records a session connecting after an outage
	// notice. It closes the episode the outage opened.
	TransitionRecovered TransitionKind = "recovered"
)

// Transition is one observed change of the broker connection, in the form
// the daemon's transport log records (issue #223). It carries the endpoint
// host and the raw error. It never carries the connection URL, a header, or
// an event payload: the watcher builds it from the config's host and the
// connect error alone, so a log built from it cannot contain a credential
// unless the error message quotes one, and Redact strips those.
type Transition struct {
	Kind TransitionKind
	// At is when the transition happened, from the watcher's clock.
	At time.Time
	// Host is the endpoint's host name, with no scheme, path, or query.
	Host string
	// Attempt counts connect attempts in the current episode. On a
	// connecting transition it is the attempt about to start, from 1. On a
	// degraded transition it is the attempt that failed. On an outage or a
	// recovered transition it is the number of failed attempts so far.
	Attempt int
	// Err is the error that ended the session. Degraded only.
	Err error
	// RetryIn is the delay before the next attempt. Degraded only.
	RetryIn time.Duration
	// Since is when the current episode began: the first failure after the
	// last healthy session. Degraded, outage, and recovered only.
	Since time.Time
}

// Host returns the endpoint's host name. A bare IoT Core host name is
// returned as is. A ws:// or wss:// URL is reduced to its host, so a log
// record built from it can never carry the URL's path or query, which is
// where a presigned URL keeps its signature.
func (c Config) Host() string {
	if !strings.Contains(c.Endpoint, "://") {
		return c.Endpoint
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}

// redactMaxLen caps a redacted message, in bytes. A connect error that
// quotes a whole handshake response has nothing past this point a reader
// needs, and a log line is bounded either way.
const redactMaxLen = 512

var (
	// redactURL matches any URL and keeps the scheme and host. The userinfo
	// goes, because user:password@ is a credential. The path and query go:
	// a SigV4-presigned URL carries its credential, signature and session
	// token in the query.
	redactURL = regexp.MustCompile(`(?i)\b(wss?|https?)://(?:[^\s/?#@]*@)?([^\s/?#@]+)[^\s]*`)
	// redactHeader matches an Authorization header with its value.
	redactHeader = regexp.MustCompile(`(?i)\bauthorization:\s*\S+(\s+\S+)?`)
	// redactScheme matches a bearer, basic or token scheme with its value.
	redactScheme = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// redactPair matches a key=value pair whose key names a secret.
	redactPair = regexp.MustCompile(`(?i)\b(x-amz-[a-z-]+|access[_-]?token|token|signature|secret|password|credential|api[_-]?key|authorization)=[^\s&;,"']+`)
)

// Redact strips credentials from an error message before it is written
// anywhere a reader can see it: the transport log and the degraded notice
// sent to watching clients. Any URL keeps its scheme and host only, with
// no userinfo, path or query, an Authorization header or bearer token
// loses its value, and a key=value pair
// whose key is a token, signature, secret or AWS signing parameter loses
// its value. The result is cut at redactMaxLen bytes.
func Redact(msg string) string {
	msg = redactURL.ReplaceAllString(msg, "$1://$2/[redacted]")
	msg = redactHeader.ReplaceAllString(msg, "authorization: [redacted]")
	msg = redactScheme.ReplaceAllString(msg, "$1 [redacted]")
	msg = redactPair.ReplaceAllString(msg, "$1=[redacted]")
	if len(msg) > redactMaxLen {
		// Step back only far enough not to split the rune at the boundary.
		// An invalid byte earlier in the message is kept as it is: the cap
		// is a length, not a validity check.
		cut := redactMaxLen
		for cut > redactMaxLen-utf8.UTFMax && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "…"
	}
	return msg
}

var websocketClose = regexp.MustCompile(`websocket: close (\d+)`)

// Classify names the class of a connect or session error, for a log record
// that must not depend on the message text: "websocket-close-<code>" for a
// WebSocket close frame, "timeout" for a deadline, "dns" for a lookup
// failure, "resolve-url" for a presign or endpoint problem, "connect" for a
// failed dial or handshake, "subscribe" for a refused subscription,
// "connection-lost" for any other session end, and "other" for the rest. A
// nil error has no class.
func Classify(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if m := websocketClose.FindStringSubmatch(msg); m != nil {
		return "websocket-close-" + m[1]
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	switch {
	case strings.HasPrefix(msg, "resolve websocket URL"):
		return "resolve-url"
	case strings.HasPrefix(msg, "connect:"):
		return "connect"
	case strings.HasPrefix(msg, "subscribe:"):
		return "subscribe"
	case strings.HasPrefix(msg, "connection lost"):
		return "connection-lost"
	}
	return "other"
}
