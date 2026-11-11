package broker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transitionLog collects OnTransition calls. Run delivers them from its own
// goroutine and the outage timer from another, so it locks.
type transitionLog struct {
	mu   sync.Mutex
	list []Transition
}

func (l *transitionLog) record(t Transition) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, t)
}

func (l *transitionLog) snapshot() []Transition {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Transition, len(l.list))
	copy(out, l.list)
	return out
}

func (l *transitionLog) count(kind TransitionKind) int {
	n := 0
	for _, t := range l.snapshot() {
		if t.Kind == kind {
			n++
		}
	}
	return n
}

// fakeTimer stands in for the outage timer. The test fires it by hand, so
// the threshold is a decision the test makes, never a wait it sits through.
type fakeTimer struct {
	mu      sync.Mutex
	armed   int
	stopped int
	fn      func()
	delay   time.Duration
}

func (f *fakeTimer) start(d time.Duration, fn func()) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed++
	f.fn = fn
	f.delay = d
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped++
	}
}

func (f *fakeTimer) fire() {
	f.mu.Lock()
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// fakeClock returns a fixed time, advanced only by the test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out: %s", msg)
}

// TestWatcher_TransitionsRecordEveryAttempt is the record the daemon's
// transport log is built from: a connect that fails produces a connecting
// record for the attempt, a degraded record with the error and the delay
// Run is about to sleep, and a connecting record for the next attempt. The
// fake clock stamps each record, so the test checks the timestamps too.
func TestWatcher_TransitionsRecordEveryAttempt(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 10, 17, 4, 57, 0, time.UTC)}
	timer := &fakeTimer{}
	log := &transitionLog{}
	w := &Watcher{
		cfg:            Config{Endpoint: "abc123.iot.us-east-1.amazonaws.com"},
		initialBackoff: time.Millisecond,
		maxBackoff:     4 * time.Millisecond,
		stableAfter:    time.Hour,
		now:            clock.now,
		startTimer:     timer.start,
	}
	w.OnTransition = log.record

	var mu sync.Mutex
	calls := 0
	w.connect = func(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			clock.advance(time.Second)
			return errors.New("connect: network Error : dial tcp: no route to host")
		}
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	waitFor(t, func() bool { return len(log.snapshot()) >= 3 }, "three transitions")
	cancel()
	<-done

	got := log.snapshot()[:3]
	assert.Equal(t, TransitionConnecting, got[0].Kind)
	assert.Equal(t, 1, got[0].Attempt, "the first attempt is numbered 1")
	assert.Equal(t, "abc123.iot.us-east-1.amazonaws.com", got[0].Host)
	assert.Equal(t, time.Date(2026, 11, 10, 17, 4, 57, 0, time.UTC), got[0].At)

	assert.Equal(t, TransitionDegraded, got[1].Kind)
	assert.Equal(t, 1, got[1].Attempt)
	require.Error(t, got[1].Err)
	assert.Equal(t, 2*time.Millisecond, got[1].RetryIn, "the record carries the delay Run sleeps before the next attempt")
	assert.Equal(t, time.Date(2026, 11, 10, 17, 4, 58, 0, time.UTC), got[1].At, "the fake clock stamps the record")
	assert.Equal(t, got[1].At, got[1].Since, "the first failure opens the episode")

	assert.Equal(t, TransitionConnecting, got[2].Kind)
	assert.Equal(t, 2, got[2].Attempt, "the retry is the episode's second attempt")
}

// TestWatcher_OutageNoticeOncePerEpisode: a transport down past the
// threshold raises exactly one outage transition however many retries
// follow, and the connection coming back raises exactly one recovered
// transition. A stale timer firing after the recovery raises nothing.
func TestWatcher_OutageNoticeOncePerEpisode(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 10, 17, 4, 57, 0, time.UTC)}
	timer := &fakeTimer{}
	log := &transitionLog{}
	w := &Watcher{
		cfg:            Config{Endpoint: "ws://127.0.0.1:9001"},
		initialBackoff: time.Millisecond,
		maxBackoff:     2 * time.Millisecond,
		stableAfter:    time.Hour,
		outageAfter:    2 * time.Minute,
		now:            clock.now,
		startTimer:     timer.start,
	}
	w.OnTransition = log.record

	var mu sync.Mutex
	failing := true
	w.connect = func(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error {
		mu.Lock()
		f := failing
		mu.Unlock()
		if f {
			return errors.New("connect: network Error : dial tcp: connection refused")
		}
		onConnect()
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	waitFor(t, func() bool { return log.count(TransitionDegraded) >= 5 }, "five failed attempts")
	assert.Zero(t, log.count(TransitionOutage), "failures under the threshold are not an outage")
	timer.mu.Lock()
	assert.Equal(t, 1, timer.armed, "the first failure arms the outage timer once; later failures of the same episode do not re-arm it")
	assert.Equal(t, 2*time.Minute, timer.delay)
	timer.mu.Unlock()

	clock.advance(2 * time.Minute)
	timer.fire()
	timer.fire()
	waitFor(t, func() bool { return log.count(TransitionDegraded) >= 8 }, "retries keep going after the notice")
	assert.Equal(t, 1, log.count(TransitionOutage), "one outage notice per episode")

	var outage Transition
	for _, tr := range log.snapshot() {
		if tr.Kind == TransitionOutage {
			outage = tr
		}
	}
	assert.Equal(t, time.Date(2026, 11, 10, 17, 4, 57, 0, time.UTC), outage.Since)
	assert.GreaterOrEqual(t, outage.Attempt, 5, "the notice says how many attempts failed so far")

	mu.Lock()
	failing = false
	mu.Unlock()
	waitFor(t, func() bool { return log.count(TransitionRecovered) >= 1 }, "recovered")
	assert.Equal(t, 1, log.count(TransitionRecovered))
	assert.Equal(t, 1, log.count(TransitionHealthy))

	timer.fire() // stale: the episode is closed
	cancel()
	<-done
	assert.Equal(t, 1, log.count(TransitionOutage), "a stale timer must not reopen a closed episode")
	assert.Equal(t, 1, log.count(TransitionRecovered))

	var recovered Transition
	for _, tr := range log.snapshot() {
		if tr.Kind == TransitionRecovered {
			recovered = tr
		}
	}
	assert.Equal(t, outage.Since, recovered.Since, "the recovery names the episode it closes")
	assert.Equal(t, 2*time.Minute, recovered.At.Sub(recovered.Since))
}

// TestWatcher_QuickLossRaisesNoOutage: a session that drops once and
// reconnects on the next attempt is the normal case the existing degraded
// notice already covers. It must raise neither an outage nor a recovery.
func TestWatcher_QuickLossRaisesNoOutage(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 10, 17, 4, 57, 0, time.UTC)}
	timer := &fakeTimer{}
	log := &transitionLog{}
	w := &Watcher{
		initialBackoff: time.Millisecond,
		maxBackoff:     2 * time.Millisecond,
		stableAfter:    time.Hour,
		now:            clock.now,
		startTimer:     timer.start,
	}
	w.OnTransition = log.record

	var mu sync.Mutex
	calls := 0
	w.connect = func(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		onConnect()
		if n == 1 {
			return errors.New("connection lost: websocket: close 1006 (abnormal closure): unexpected EOF")
		}
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	waitFor(t, func() bool { return log.count(TransitionHealthy) >= 2 }, "reconnected")
	cancel()
	<-done

	assert.Equal(t, 1, log.count(TransitionDegraded))
	assert.Zero(t, log.count(TransitionOutage), "a single quick loss is not an outage")
	assert.Zero(t, log.count(TransitionRecovered), "no outage, so nothing to recover from")
	timer.mu.Lock()
	assert.Equal(t, 1, timer.armed)
	assert.GreaterOrEqual(t, timer.stopped, 1, "the reconnect disarms the outage timer")
	timer.mu.Unlock()
}

func TestConfig_Host(t *testing.T) {
	cases := []struct {
		endpoint string
		want     string
	}{
		{"abc123.iot.us-east-1.amazonaws.com", "abc123.iot.us-east-1.amazonaws.com"},
		{"ws://127.0.0.1:9001", "127.0.0.1:9001"},
		{"wss://relay.example.com/mqtt?X-Amz-Signature=abc", "relay.example.com"},
		{"", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, Config{Endpoint: tc.endpoint}.Host(), tc.endpoint)
	}
}

// TestRedact: a connect error can quote the presigned URL, and a presigned
// URL is a credential. Only the scheme and host of any URL may survive, and
// no header, token or key=value secret may.
func TestRedact(t *testing.T) {
	presigned := "wss://abc123.iot.us-east-1.amazonaws.com/mqtt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE%2F20261110%2Fus-east-1%2Fiotdevicegateway%2Faws4_request&X-Amz-Date=20261110T170457Z&X-Amz-Signature=deadbeef0123&X-Amz-Security-Token=FwoGZXIvYXdzEBYaDEXAMPLE"
	cases := []struct {
		name string
		in   string
		keep []string
		drop []string
	}{
		{
			name: "presigned URL keeps scheme and host only",
			in:   "connect: dial " + presigned + ": websocket: bad handshake",
			keep: []string{"wss://abc123.iot.us-east-1.amazonaws.com/", "websocket: bad handshake"},
			drop: []string{"X-Amz-Signature", "deadbeef0123", "AKIAEXAMPLE", "X-Amz-Security-Token", "FwoGZXIvYXdz", "/mqtt?"},
		},
		{
			name: "URL userinfo is a credential",
			in:   "connect: dial wss://ghmon:p4ssw0rd-secret@relay.example.com:9001/mqtt: connection refused",
			keep: []string{"wss://relay.example.com:9001/", "connection refused"},
			drop: []string{"ghmon", "p4ssw0rd-secret", "@", "/mqtt"},
		},
		{
			name: "authorization header value",
			in:   "connect: 401 with Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.secret.sig",
			keep: []string{"connect: 401"},
			drop: []string{"eyJhbGciOiJIUzI1NiJ9", "secret.sig"},
		},
		{
			name: "bare key=value token",
			in:   "subscribe: token=ghp_abcdefghijklmnop rejected",
			keep: []string{"subscribe:", "rejected"},
			drop: []string{"ghp_abcdefghijklmnop"},
		},
		{
			name: "plain error is unchanged",
			in:   "connection lost: websocket: close 1006 (abnormal closure): unexpected EOF",
			keep: []string{"connection lost: websocket: close 1006 (abnormal closure): unexpected EOF"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			for _, k := range tc.keep {
				assert.Contains(t, got, k)
			}
			for _, d := range tc.drop {
				assert.NotContains(t, got, d)
			}
		})
	}

	t.Run("long messages are cut", func(t *testing.T) {
		long := make([]byte, 5000)
		for i := range long {
			long[i] = 'x'
		}
		assert.LessOrEqual(t, len(Redact(string(long))), redactMaxLen+len("…"))
	})

	t.Run("an invalid byte before the cap does not move the cut", func(t *testing.T) {
		long := make([]byte, 600)
		for i := range long {
			long[i] = 'x'
		}
		long[10] = 0xFF
		got := Redact(string(long))
		assert.Equal(t, redactMaxLen+len("…"), len(got), "the cap is a length, so the cut stays at the cap")
		assert.Equal(t, string(long[:redactMaxLen]), got[:redactMaxLen], "everything up to the cap is kept, invalid byte included")
	})

	t.Run("the cut does not split a rune", func(t *testing.T) {
		long := []byte{}
		for len(long) < 600 {
			long = append(long, "é"...) // two bytes each
		}
		// redactMaxLen is even, so the boundary falls between runes; shift
		// it by one so the cap lands inside a rune.
		long = append([]byte{'a'}, long...)
		got := Redact(string(long))
		assert.True(t, utf8.ValidString(got), "the cut must not leave half a rune")
		assert.GreaterOrEqual(t, len(got), redactMaxLen-utf8.UTFMax+len("…"))
	})
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"websocket close code", errors.New("connection lost: websocket: close 1006 (abnormal closure): unexpected EOF"), "websocket-close-1006"},
		{"context deadline", context.DeadlineExceeded, "timeout"},
		{"presign failure", errors.New("resolve websocket URL: presign: retrieve AWS credentials: no EC2 IMDS role found"), "resolve-url"},
		{"connect refused", errors.New("connect: network Error : dial tcp 127.0.0.1:9001: connect: connection refused"), "connect"},
		{"subscribe", errors.New("subscribe: not authorized"), "subscribe"},
		{"connection lost other", errors.New("connection lost: EOF"), "connection-lost"},
		{"unknown", errors.New("something else"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Classify(tc.err))
		})
	}
}
