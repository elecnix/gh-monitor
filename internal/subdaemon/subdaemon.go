// Package subdaemon launches and supervises optional sub-daemon processes
// configured by the operator. It is the generic process-supervision layer
// gh-monitor's daemon uses to start backends that speak the backend/remote
// protocol — most notably a proprietary notification subscriber — without
// carrying any proprietary code in this public repo.
//
// The launcher reads a line-delimited config file listing sub-daemons to
// launch, starts each as a child process, and keeps each alive with
// exponential backoff (reset after a stable run). It is deliberately dumb:
// it does not know what a sub-daemon does, which socket it binds, or which
// protocol it speaks. It just reads a list and launches processes.
//
// # Config file
//
// The path is resolved in order: $GH_MONITOR_SUBDAEMONS if set, else
// <cwd>/.gh-monitor.conf if it exists, else <user config dir>/gh-monitor/daemons.conf.
// Precedence is replacement, not merge — a project file that exists is used whole.
// The format is one sub-daemon per line:
//
//	# optional comment
//	<name> <executable> [args...]
//
// Blank lines and comments (first non-space character #) are ignored. A
// field may be double-quoted to keep a path or argument that contains
// spaces. There is no escape sequence.
//
// # Fallback
//
// If the config file does not exist, the launcher is a no-op and the daemon
// works exactly as it does today (pure polling). If a sub-daemon fails to
// start — binary not found, bad config — the launcher logs a warning and
// continues with the rest; it never brings down the daemon.
package subdaemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// envConfigPath names the environment variable overriding the config path.
const envConfigPath = "GH_MONITOR_SUBDAEMONS"

// projectConfigFile is the per-project sub-daemon config file resolved from
// the current working directory ahead of the global user config. A repository
// pins it by dropping this file in its root; the daemon then uses those
// entries instead of the operator's machine-wide daemons.conf.
const projectConfigFile = ".gh-monitor.conf"

// DefaultConfigPath returns the global config path: $GH_MONITOR_SUBDAEMONS if
// set, otherwise <user config dir>/gh-monitor/daemons.conf.
func DefaultConfigPath() string {
	if p := strings.TrimSpace(os.Getenv(envConfigPath)); p != "" {
		return p
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "gh-monitor", "daemons.conf")
}

// ResolveConfigPath returns the first existing daemon config path in
// precedence order:
//
//  1. $GH_MONITOR_SUBDAEMONS, if set (an explicit admin override)
//  2. <cwd>/.gh-monitor.conf, the per-project file, if it exists
//  3. <user config dir>/gh-monitor/daemons.conf, the global config (default)
//
// Precedence is replacement, not merge: a project file that exists is used
// whole, so a repository pins its own sub-daemons without inheriting the
// operator's. When cwd is empty or the project file is absent, the call falls
// through to the global config. The final (global) path is returned even when
// it does not exist — the caller's Load treats a missing file as pure polling.
func ResolveConfigPath(cwd string) string {
	if p := strings.TrimSpace(os.Getenv(envConfigPath)); p != "" {
		return p
	}
	if cwd != "" {
		if _, err := os.Stat(filepath.Join(cwd, projectConfigFile)); err == nil {
			return filepath.Join(cwd, projectConfigFile)
		}
	}
	return DefaultConfigPath()
}

// Entry is one sub-daemon to launch.
type Entry struct {
	// Name is a human-readable label used in logs. It is the first field on
	// a config line and carries no protocol meaning.
	Name string
	// Cmd is the executable and its arguments. Cmd[0] is the binary path.
	Cmd []string
}

// Load reads and parses the sub-daemon config at path. A missing file is not
// an error: it returns an empty slice and ok=false so the caller keeps the
// daemon in pure-polling mode.
func Load(path string) (entries []Entry, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open sub-daemon config: %w", err)
	}
	defer func() { _ = f.Close() }()
	entries, err = parse(f)
	if err != nil {
		return nil, false, fmt.Errorf("parse sub-daemon config %s: %w", path, err)
	}
	return entries, true, nil
}

// parse reads a line-delimited config from r.
func parse(r io.Reader) ([]Entry, error) {
	var entries []Entry
	s := bufio.NewScanner(r)
	// Allow long lines (paths can be long); the default 64K token limit is
	// already generous, so leave it.
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields, err := splitFields(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: expected at least <name> and <executable>, got %d field(s)", lineNo, len(fields))
		}
		entries = append(entries, Entry{Name: fields[0], Cmd: fields[1:]})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// splitFields splits on whitespace, honoring double-quoted segments so a
// path or argument may contain spaces. There is no escape sequence: a
// backslash is literal. This keeps the parser tiny and the format obvious.
func splitFields(line string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	inField := false // true once a field has started (saw non-space or a quote)
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			inField = true
		case !inQuote && (r == ' ' || r == '\t'):
			if inField {
				fields = append(fields, cur.String())
				cur.Reset()
				inField = false
			}
		default:
			cur.WriteRune(r)
			inField = true
		}
	}
	if inQuote {
		return nil, errors.New("unterminated double-quoted field")
	}
	if inField {
		fields = append(fields, cur.String())
	}
	return fields, nil
}

// Process is the handle to a running sub-daemon child. The launcher waits on
// it and may signal it.
type Process interface {
	// Wait blocks until the child exits and returns how long it ran and its
	// exit error (nil for a clean exit code 0).
	Wait() (runtime time.Duration, err error)
	// Signal sends a signal to the child.
	Signal(sig os.Signal) error
}

// Launcher supervises a set of sub-daemon processes. Run blocks until ctx is
// cancelled; on cancel it signals every child and waits for them.
type Launcher struct {
	Entries []Entry
	// Out receives log lines (warnings, lifecycle). Defaults to os.Stderr.
	Out io.Writer
	// ChildEnv, when set, builds the environment for one entry's child
	// process; whatever it returns becomes the child's env, whole. The daemon
	// uses it to point each sub-daemon at its own private socket via
	// GH_MONITOR_SOCK (issue #88) — the sub-daemon binary needs no knowledge
	// of the arrangement. Nil inherits the launcher's own environment.
	ChildEnv func(entry Entry) []string
	// Spawn starts one sub-daemon. Production re-execs the binary; tests
	// inject a fake. A Spawn that returns an error wrapping exec.ErrNotFound
	// stops that entry immediately (no retry) — a missing binary will never
	// start, so burning the rapid-failure budget on it only adds noise.
	Spawn func(ctx context.Context, entry Entry) (Process, error)

	// Tunables. Zero values fall back to the defaults below.
	MinBackoff    time.Duration
	MaxBackoff    time.Duration
	StableRun     time.Duration
	MaxRapidFails int
	// Sleep is the backoff sleep; tests inject an instant clock.
	Sleep func(time.Duration)
}

// Defaults. Exported so a caller or test can read them; mutating across
// goroutines is not safe.
const (
	defaultMinBackoff    = 1 * time.Second
	defaultMaxBackoff    = 60 * time.Second
	defaultStableRun     = 30 * time.Second
	defaultMaxRapidFails = 5
)

// Run launches every entry and keeps each alive until ctx is cancelled. Each
// entry is supervised independently: a crash restarts only that entry, and
// an entry that fails too many times in a row stops trying (logged) without
// affecting the others. Run returns ctx.Err() once every supervisor has
// stopped.
func (l *Launcher) Run(ctx context.Context) error {
	out := l.Out
	if out == nil {
		out = os.Stderr
	}
	minB := l.MinBackoff
	if minB <= 0 {
		minB = defaultMinBackoff
	}
	maxB := l.MaxBackoff
	if maxB <= 0 {
		maxB = defaultMaxBackoff
	}
	stable := l.StableRun
	if stable <= 0 {
		stable = defaultStableRun
	}
	maxFails := l.MaxRapidFails
	if maxFails <= 0 {
		maxFails = defaultMaxRapidFails
	}
	sleep := l.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	spawn := l.Spawn
	if spawn == nil {
		childEnv := l.ChildEnv
		spawn = func(ctx context.Context, e Entry) (Process, error) {
			return startChild(ctx, e, childEnv, out)
		}
	}

	var wg sync.WaitGroup
	for _, e := range l.Entries {
		e := e
		wg.Add(1)
		go func() {
			defer wg.Done()
			superviseOne(ctx, out, e, superviseConfig{
				spawn: spawn, minBackoff: minB, maxBackoff: maxB,
				stable: stable, maxFails: maxFails, sleep: sleep,
			})
		}()
	}
	wg.Wait()
	return ctx.Err()
}

type superviseConfig struct {
	spawn      func(ctx context.Context, entry Entry) (Process, error)
	minBackoff time.Duration
	maxBackoff time.Duration
	stable     time.Duration
	maxFails   int
	sleep      func(time.Duration)
}

// superviseOne runs one entry's restart loop until ctx is cancelled. A
// configured sub-daemon is never abandoned: a rapid-crash burst backs off up
// to maxBackoff and keeps retrying — the event-driven wake path must
// self-heal the moment the transient condition clears (measured 2026-08-22:
// a broker-subscriber that gave up permanently after 5 rapid crashes left
// gh-monitor polling-only, the shared GraphQL burn source). The only
// permanent condition is a missing binary, which can never start and stops
// that entry immediately.
func superviseOne(ctx context.Context, out io.Writer, e Entry, cfg superviseConfig) {
	backoff := cfg.minBackoff
	consecutive := 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		proc, err := cfg.spawn(ctx, e)
		if err != nil {
			// A missing binary will never start, so do not burn the
			// rapid-failure budget retrying it. Log once and stop this entry.
			if isNotFound(err) {
				_, _ = fmt.Fprintf(out, "gh-monitor subdaemon: %q: %v (not started; check the path in the config)\n", e.Name, err)
				return
			}
			_, _ = fmt.Fprintf(out, "gh-monitor subdaemon: %q: spawn: %v\n", e.Name, err)
			consecutive++
			if consecutive >= cfg.maxFails {
				// The burst is spent: settle into a slow retry at the cap
				// instead of giving up — the condition may be transient
				// (a socket collision during an upgrade handoff), and a
				// permanent fall to polling-only is exactly the defect.
				_, _ = fmt.Fprintf(out, "gh-monitor subdaemon: %q: rapid failures exceed %d — retrying slowly every %s (the wake path stays armed)\n", e.Name, cfg.maxFails, cfg.maxBackoff)
				cfg.sleep(cfg.maxBackoff)
				continue
			}
			cfg.sleep(backoff)
			backoff = nextBackoff(backoff, cfg.maxBackoff)
			continue
		}

		// Propagate cancellation to the child so Wait returns promptly when
		// the daemon shuts down. Without this a cancelled launcher would
		// block on a live child until it died on its own.
		stopCh := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = proc.Signal(os.Interrupt)
			case <-stopCh:
			}
		}()
		runtime, exitErr := proc.Wait()
		close(stopCh)
		if cerr := ctx.Err(); cerr != nil {
			return
		}
		_, _ = fmt.Fprintf(out, "gh-monitor subdaemon: %q ran %s (exit=%v)\n", e.Name, runtime.Round(time.Second), exitErr)
		if runtime >= cfg.stable {
			// A stable run died normally — restart fresh, no penalty.
			consecutive = 0
			backoff = cfg.minBackoff
			continue
		}
		consecutive++
		if consecutive >= cfg.maxFails {
			_, _ = fmt.Fprintf(out, "gh-monitor subdaemon: %q: rapid failures exceed %d — retrying slowly every %s (the wake path stays armed)\n", e.Name, cfg.maxFails, cfg.maxBackoff)
			cfg.sleep(cfg.maxBackoff)
			continue
		}
		_, _ = fmt.Fprintf(out, "gh-monitor subdaemon: %q: rapid failure %d/%d — backing off %s\n", e.Name, consecutive, cfg.maxFails, backoff)
		cfg.sleep(backoff)
		backoff = nextBackoff(backoff, cfg.maxBackoff)
	}
}

// nextBackoff returns the next exponential backoff step, capped at max.
func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next <= 0 || next > max { // overflow or cap
		return max
	}
	return next
}

// isNotFound reports whether err is a "binary not found" start failure.
func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound)
}

// applyChildEnv builds a child's environment: the base env when no hook is
// set, whatever the hook returns otherwise.
func applyChildEnv(base []string, hook func(Entry) []string, e Entry) []string {
	if hook == nil {
		return base
	}
	return hook(e)
}

// startChild is the production spawn: it starts one sub-daemon as a child
// process. It applies the launcher's ChildEnv hook (nil inherits this
// process's environment) so the daemon can redirect $GH_MONITOR_SOCK to a
// private per-entry socket without the sub-daemon knowing (issue #88).
// Diagnostics pass through to out so a sub-daemon's logs stay visible
// alongside the daemon's.
func startChild(_ context.Context, e Entry, childEnv func(Entry) []string, out io.Writer) (Process, error) {
	if len(e.Cmd) == 0 {
		return nil, fmt.Errorf("empty command for %q", e.Name)
	}
	cmd := exec.Command(e.Cmd[0], e.Cmd[1:]...)
	cmd.Env = applyChildEnv(os.Environ(), childEnv, e)
	cmd.Stderr = out
	cmd.Stdout = out
	// Stdin is nil → /dev/null; sub-daemons are not interactive.
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &osProcess{cmd: cmd, started: time.Now()}, nil
}

// osProcess adapts exec.Cmd to the Process interface.
type osProcess struct {
	cmd     *exec.Cmd
	started time.Time
}

func (p *osProcess) Wait() (time.Duration, error) {
	err := p.cmd.Wait()
	return time.Since(p.started), err
}

func (p *osProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Signal(sig)
}
