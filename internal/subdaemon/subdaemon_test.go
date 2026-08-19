package subdaemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Config parsing ---

func TestSplitFields(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "", want: nil},
		{in: "name /bin/true", want: []string{"name", "/bin/true"}},
		{in: "  spaced   /bin/true   --repo  x  ", want: []string{"spaced", "/bin/true", "--repo", "x"}},
		{in: `name "/path with space/bin" daemon`, want: []string{"name", "/path with space/bin", "daemon"}},
		{in: `name ""`, want: []string{"name", ""}},
		{in: `name "/unterminated`, wantErr: true},
	}
	for _, c := range cases {
		got, err := splitFields(c.in)
		if c.wantErr {
			assert.Error(t, err, "splitFields(%q) should error", c.in)
			continue
		}
		require.NoError(t, err, "splitFields(%q)", c.in)
		assert.Equal(t, c.want, got, "splitFields(%q)", c.in)
	}
}

func TestParse(t *testing.T) {
	in := strings.NewReader(`
# a comment, ignored

broker-subscriber /usr/local/bin/broker-subscriber daemon --repo PrizmalAi/PrizmalSwitch

   # indented comment
other-daemon "/path with space/other" --flag value
`)
	entries, err := parse(in)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "broker-subscriber", entries[0].Name)
	assert.Equal(t, []string{"/usr/local/bin/broker-subscriber", "daemon", "--repo", "PrizmalAi/PrizmalSwitch"}, entries[0].Cmd)
	assert.Equal(t, "other-daemon", entries[1].Name)
	assert.Equal(t, []string{"/path with space/other", "--flag", "value"}, entries[1].Cmd)
}

func TestParse_TooFewFields(t *testing.T) {
	_, err := parse(strings.NewReader("lonely-name"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected at least")
}

func TestLoad_MissingFile(t *testing.T) {
	entries, ok, err := Load(filepath.Join(t.TempDir(), "nope.conf"))
	require.NoError(t, err)
	assert.False(t, ok, "a missing file is not an error")
	assert.Empty(t, entries)
}

func TestLoad_ParsesRealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemons.conf")
	require.NoError(t, os.WriteFile(path, []byte("a /bin/true\nb /bin/false\n"), 0o644))
	entries, ok, err := Load(path)
	require.NoError(t, err)
	assert.True(t, ok)
	require.Len(t, entries, 2)
	assert.Equal(t, "a", entries[0].Name)
}

func TestDefaultConfigPath_EnvOverride(t *testing.T) {
	t.Setenv(envConfigPath, "/custom/path.conf")
	assert.Equal(t, "/custom/path.conf", DefaultConfigPath())
}

func TestDefaultConfigPath_Default(t *testing.T) {
	t.Setenv(envConfigPath, "")
	base, err := os.UserConfigDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "gh-monitor", "daemons.conf"), DefaultConfigPath())
}

// --- Launcher / supervision ---
//
// fakeProcess and fakeSpawn let tests drive the restart loop deterministically
// without spawning real binaries. Each Wait() returns one scripted outcome.

type fakeProcess struct {
	wait     func() (time.Duration, error)
	sigCh    chan os.Signal
	signaled atomic.Bool
}

func (f *fakeProcess) Wait() (time.Duration, error) { return f.wait() }
func (f *fakeProcess) Signal(sig os.Signal) error {
	f.signaled.Store(true)
	if f.sigCh != nil {
		select {
		case f.sigCh <- sig:
		default:
		}
	}
	return nil
}

// scriptedSpawn returns a Spawn that hands each entry a queue of run scripts.
// Each script element is one (runtime, err) the child "experiences" on
// successive Waits, advancing a shared per-entry index so a restart sees the
// next scripted outcome rather than replaying the first. After the queue is
// exhausted the entry's process blocks until signalled (ctx cancel).
type scriptedSpawn struct {
	mu      sync.Mutex
	runs    map[string][]runOutcome
	cursors map[string]*int
}

type runOutcome struct {
	err     error
	runtime time.Duration
}

func newScriptedSpawn() *scriptedSpawn {
	return &scriptedSpawn{
		runs:    make(map[string][]runOutcome),
		cursors: make(map[string]*int),
	}
}

func (s *scriptedSpawn) spawn(_ context.Context, e Entry) (Process, error) {
	s.mu.Lock()
	if _, ok := s.cursors[e.Name]; !ok {
		c := 0
		s.cursors[e.Name] = &c
	}
	cursor := s.cursors[e.Name]
	queue := s.runs[e.Name]
	s.mu.Unlock()
	sigCh := make(chan os.Signal, 1)

	return &fakeProcess{
		sigCh: sigCh,
		wait: func() (time.Duration, error) {
			s.mu.Lock()
			idx := *cursor
			s.mu.Unlock()
			if idx < len(queue) {
				s.mu.Lock()
				*cursor = idx + 1
				s.mu.Unlock()
				return queue[idx].runtime, queue[idx].err
			}
			// Script exhausted: block until signalled (ctx cancel).
			<-sigCh
			return 0, nil
		},
	}, nil
}

func newTestLauncher(entries []Entry, spawn func(context.Context, Entry) (Process, error)) *Launcher {
	return &Launcher{
		Entries:       entries,
		Out:           &bytes.Buffer{},
		Spawn:         spawn,
		MinBackoff:    1 * time.Millisecond,
		MaxBackoff:    10 * time.Millisecond,
		StableRun:     50 * time.Millisecond,
		MaxRapidFails: 3,
		Sleep:         func(time.Duration) {}, // instant clock
	}
}

func (l *Launcher) out() string {
	return l.Out.(*bytes.Buffer).String()
}

// TestLauncher_RapidFailureGivesUp verifies an entry that crashes under the
// stable-run threshold stops after MaxRapidFails crashes — the visible-failure
// path that keeps a broken sub-daemon from churning forever.
func TestLauncher_RapidFailureGivesUp(t *testing.T) {
	s := newScriptedSpawn()
	s.runs["broken"] = []runOutcome{
		{nil, 1 * time.Millisecond}, // rapid fail 1
		{nil, 1 * time.Millisecond}, // rapid fail 2
		{nil, 1 * time.Millisecond}, // rapid fail 3 → give up
	}
	l := newTestLauncher([]Entry{{Name: "broken", Cmd: []string{"x"}}}, s.spawn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = l.Run(ctx)

	out := l.out()
	assert.Contains(t, out, "giving up after 3 rapid crashes")
	// A "rapid failure" line is logged only for crashes that do NOT give up,
	// so three crashes produce two such lines (the third gives up instead).
	assert.Equal(t, 2, strings.Count(out, "rapid failure"), "logged each non-final rapid failure")
}

// TestLauncher_StableRunResetsBackoff verifies a run that lasts past
// StableRun resets the rapid-failure counter so a long-lived daemon that
// died once is restarted fresh, not penalized.
func TestLauncher_StableRunResetsBackoff(t *testing.T) {
	s := newScriptedSpawn()
	s.runs["flaky"] = []runOutcome{
		{nil, 100 * time.Millisecond}, // stable → reset
		{nil, 1 * time.Millisecond},   // rapid fail 1 (not 2)
		{nil, 1 * time.Millisecond},   // rapid fail 2
		{nil, 1 * time.Millisecond},   // rapid fail 3 → give up
	}
	l := newTestLauncher([]Entry{{Name: "flaky", Cmd: []string{"x"}}}, s.spawn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = l.Run(ctx)

	out := l.out()
	// The stable run must have reset the counter: three rapid crashes after it
	// are required to give up (not two).
	assert.Contains(t, out, "giving up after 3 rapid crashes")
}

// TestLauncher_NotFoundStopsImmediately verifies a missing binary is logged
// once and the entry is not retried — retrying a binary that does not exist
// only adds noise to the log.
func TestLauncher_NotFoundStopsImmediately(t *testing.T) {
	var spawns int64
	l := newTestLauncher(
		[]Entry{{Name: "ghost", Cmd: []string{"/no/such/binary"}}},
		func(context.Context, Entry) (Process, error) {
			atomic.AddInt64(&spawns, 1)
			return nil, fmt.Errorf("start: %w", exec.ErrNotFound)
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = l.Run(ctx)

	assert.Equal(t, int64(1), atomic.LoadInt64(&spawns), "a missing binary must not be retried")
	assert.Contains(t, l.out(), "not started")
}

// TestLauncher_MultipleEntriesIndependent verifies one entry giving up does
// not stop another — supervision is per-entry.
func TestLauncher_MultipleEntriesIndependent(t *testing.T) {
	s := newScriptedSpawn()
	s.runs["broken"] = []runOutcome{
		{nil, 1 * time.Millisecond},
		{nil, 1 * time.Millisecond},
		{nil, 1 * time.Millisecond}, // give up after 3
	}
	s.runs["healthy"] = []runOutcome{
		{nil, 100 * time.Millisecond}, // stable, loops forever after script
	}
	l := newTestLauncher(
		[]Entry{{Name: "broken", Cmd: []string{"x"}}, {Name: "healthy", Cmd: []string{"y"}}},
		s.spawn,
	)

	ctx, cancel := context.WithCancel(context.Background())
	// Let broken give up, then cancel to release healthy.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_ = l.Run(ctx)

	out := l.out()
	assert.Contains(t, out, `"broken": giving up after 3 rapid crashes`)
	// healthy must still have been launched and run its stable pass.
	assert.Contains(t, out, `"healthy" ran`)
}

// TestLauncher_CancellationStopsChildren verifies cancelling the launcher's
// context signals running children and Run returns.
func TestLauncher_CancellationStopsChildren(t *testing.T) {
	var procMu sync.Mutex
	var child *fakeProcess
	l := newTestLauncher(
		[]Entry{{Name: "long", Cmd: []string{"x"}}},
		func(context.Context, Entry) (Process, error) {
			sigCh := make(chan os.Signal, 1)
			p := &fakeProcess{
				sigCh: sigCh,
				wait: func() (time.Duration, error) {
					<-sigCh // block until signalled
					return 0, nil
				},
			}
			procMu.Lock()
			child = p
			procMu.Unlock()
			return p, nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	// Give the launcher a moment to start the child, then cancel.
	require.Eventually(t, func() bool {
		procMu.Lock()
		defer procMu.Unlock()
		return child != nil
	}, time.Second, 5*time.Millisecond, "child should be started")

	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// The child must have been interrupted by the cancellation propagation —
	// the only way Run can return against a Wait that blocks on sigCh.
	procMu.Lock()
	c := child
	procMu.Unlock()
	require.NotNil(t, c)
	assert.True(t, c.signaled.Load(), "child must be signalled on cancel")
}

// TestLoad_OpenError verifies an open error that is not "missing" is reported
// rather than silently treated as an absent config.
func TestLoad_OpenError(t *testing.T) {
	// A NUL byte in the path is rejected by the OS as an invalid argument —
	// a hard open error that is not os.ErrNotExist.
	_, _, err := Load("invalid\x00path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sub-daemon config")
}
