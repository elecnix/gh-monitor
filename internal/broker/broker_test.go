package broker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stateLog collects OnState transitions with a mutex, since Run calls it
// from its own goroutine.
type stateLog struct {
	mu     sync.Mutex
	states []State
	errs   []error
}

func (l *stateLog) record(s State, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.states = append(l.states, s)
	l.errs = append(l.errs, err)
}

func (l *stateLog) snapshot() []State {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]State, len(l.states))
	copy(out, l.states)
	return out
}

// TestWatcher_BreaksToDegradedWhenConnectionDrops is the literal "kill the
// subscription and confirm the watcher reports DEGRADED" test the module's
// well-behaved-subscriber guidance calls for. The fake session connects
// successfully (onConnect fires, matching a live subscribe), then the
// connection drops — exactly what happens to a real MQTT session across a
// network blip or the SigV4-presigned URL's expiry. The watcher must report
// Degraded, never stay silently Healthy and never simply stop reporting.
func TestWatcher_BreaksToDegradedWhenConnectionDrops(t *testing.T) {
	log := &stateLog{}
	w := &Watcher{
		initialBackoff: time.Millisecond,
		maxBackoff:     5 * time.Millisecond,
		stableAfter:    time.Hour, // never "stable" in this short test
	}
	w.OnState = log.record

	var calls int
	var mu sync.Mutex
	w.connect = func(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			onConnect() // subscription goes live
			return errors.New("connection lost: EOF")
		}
		// Subsequent reconnect attempts: the broker stays unreachable, so
		// the watcher must keep reporting Degraded, not recover on its own.
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	states := log.snapshot()
	require.GreaterOrEqual(t, len(states), 3, "expected at least connecting, healthy, degraded")
	assert.Equal(t, StateConnecting, states[0])
	assert.Equal(t, StateHealthy, states[1], "the fake session subscribed successfully; the watcher must report Healthy")
	assert.Equal(t, StateDegraded, states[2], "the dropped connection must be reported as Degraded, not silence")
}

// TestWatcher_NeverConnectsStaysDegraded covers the case where the broker is
// entirely unreachable (bad endpoint, missing AWS credentials, network
// outage) — a daemon running on a machine with no broker configured, or a
// misconfigured one, must never claim Healthy.
func TestWatcher_NeverConnectsStaysDegraded(t *testing.T) {
	log := &stateLog{}
	w := &Watcher{
		initialBackoff: time.Millisecond,
		maxBackoff:     2 * time.Millisecond,
		stableAfter:    time.Hour,
	}
	w.OnState = log.record
	w.connect = func(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error {
		return errors.New("connect: no route to host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	states := log.snapshot()
	require.NotEmpty(t, states)
	for _, s := range states {
		assert.NotEqual(t, StateHealthy, s, "a connectFunc that never calls onConnect must never be reported Healthy")
	}
}

// TestWatcher_ValidEventWakesCaller verifies a well-formed event reaches
// OnWake with its owner/repo/pr_number, and an event failing the shape
// check (guideline #2: validate before trusting) is dropped instead of
// waking anything.
func TestWatcher_ValidEventWakesCaller(t *testing.T) {
	var mu sync.Mutex
	var woken []string

	w := &Watcher{initialBackoff: time.Hour, maxBackoff: time.Hour, stableAfter: time.Hour}
	w.OnState = func(State, error) {}
	w.OnWake = func(owner, repo string, pr int) {
		mu.Lock()
		defer mu.Unlock()
		woken = append(woken, owner+"/"+repo)
		_ = pr
	}
	w.connect = func(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error {
		onConnect()
		onEvent(Event{Source: "github", RepositoryOwner: "acme", RepositoryName: "widgets", EventType: "pull_request", PRNumber: 42})
		onEvent(Event{RepositoryOwner: "acme"}) // missing source/repo name/event_type — must be dropped
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"acme/widgets"}, woken, "only the valid event should wake the caller")
}

// TestWatcher_StableSessionResetsBackoff verifies a session that ran past
// stableAfter resets the reconnect delay to initialBackoff on its next
// disconnect — a broker that was working and dropped once (e.g. a routine
// presigned-URL expiry) should reconnect fast, not inherit a long backoff
// from a completely different, earlier outage.
func TestWatcher_StableSessionResetsBackoff(t *testing.T) {
	log := &stateLog{}
	w := &Watcher{
		initialBackoff: 2 * time.Millisecond,
		maxBackoff:     time.Second, // large: a bug that fails to reset would blow past the test window
		stableAfter:    5 * time.Millisecond,
	}
	w.OnState = log.record

	var mu sync.Mutex
	calls := 0
	w.connect = func(ctx context.Context, cfg Config, onConnect func(), onEvent func(Event)) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		onConnect()
		if n == 1 {
			time.Sleep(10 * time.Millisecond) // stay "connected" past stableAfter
			return errors.New("connection lost: EOF")
		}
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	mu.Lock()
	n := calls
	mu.Unlock()
	assert.GreaterOrEqual(t, n, 2, "a fast-reset backoff must allow a second connect attempt within the test window")
}

func TestEvent_Valid(t *testing.T) {
	cases := []struct {
		name string
		evt  Event
		want bool
	}{
		{"fully populated", Event{Source: "github", RepositoryOwner: "o", RepositoryName: "r", EventType: "pull_request", PRNumber: 1}, true},
		{"no PR number (check_run)", Event{Source: "github", RepositoryOwner: "o", RepositoryName: "r", EventType: "check_run"}, true},
		{"missing source", Event{RepositoryOwner: "o", RepositoryName: "r", EventType: "pull_request"}, false},
		{"missing owner", Event{Source: "github", RepositoryName: "r", EventType: "pull_request"}, false},
		{"missing repo", Event{Source: "github", RepositoryOwner: "o", EventType: "pull_request"}, false},
		{"missing event type", Event{Source: "github", RepositoryOwner: "o", RepositoryName: "r"}, false},
		{"zero value", Event{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.evt.Valid())
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Run("unset endpoint disables the transport", func(t *testing.T) {
		t.Setenv(envEndpoint, "")
		t.Setenv(envRegion, "")
		t.Setenv(envTopic, "")
		_, ok := ConfigFromEnv()
		assert.False(t, ok, "an unset endpoint must read as not-configured, never as configured-but-degraded")
	})

	t.Run("endpoint set applies defaults for region and topic", func(t *testing.T) {
		t.Setenv(envEndpoint, "abc123.iot.us-east-1.amazonaws.com")
		t.Setenv(envRegion, "")
		t.Setenv(envTopic, "")
		cfg, ok := ConfigFromEnv()
		require.True(t, ok)
		assert.Equal(t, "abc123.iot.us-east-1.amazonaws.com", cfg.Endpoint)
		assert.Equal(t, defaultRegion, cfg.Region)
		assert.Equal(t, defaultTopic, cfg.Topic)
	})

	t.Run("explicit region and topic override the defaults", func(t *testing.T) {
		t.Setenv(envEndpoint, "abc123.iot.ca-central-1.amazonaws.com")
		t.Setenv(envRegion, "ca-central-1")
		t.Setenv(envTopic, "github/my-org/my-repo/+")
		cfg, ok := ConfigFromEnv()
		require.True(t, ok)
		assert.Equal(t, "ca-central-1", cfg.Region)
		assert.Equal(t, "github/my-org/my-repo/+", cfg.Topic)
	})
}

func TestIdleCapFromEnv(t *testing.T) {
	t.Run("unset falls back to the default", func(t *testing.T) {
		t.Setenv(envIdleCap, "")
		assert.Equal(t, 30*time.Minute, IdleCapFromEnv(30*time.Minute))
	})
	t.Run("valid seconds value is honoured", func(t *testing.T) {
		t.Setenv(envIdleCap, "120")
		assert.Equal(t, 120*time.Second, IdleCapFromEnv(30*time.Minute))
	})
	t.Run("garbage falls back rather than disabling the safety net", func(t *testing.T) {
		t.Setenv(envIdleCap, "not-a-number")
		assert.Equal(t, 30*time.Minute, IdleCapFromEnv(30*time.Minute))
	})
	t.Run("zero or negative falls back rather than disabling the safety net", func(t *testing.T) {
		t.Setenv(envIdleCap, "0")
		assert.Equal(t, 30*time.Minute, IdleCapFromEnv(30*time.Minute))
		t.Setenv(envIdleCap, "-5")
		assert.Equal(t, 30*time.Minute, IdleCapFromEnv(30*time.Minute))
	})
}
