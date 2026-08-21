// Resident daemon self-upgrade (issue #73).
//
// The runtime-copy launcher (internal/reexec) keeps the installed binary's
// image unmapped, so `gh extension upgrade` can rewrite it while the daemon
// runs. This file closes the loop: the daemon watches the installed binary,
// and when an upgrade lands it spawns it. The new daemon performs the
// in-memory handoff — adopting the watching state and the listening socket —
// and this process exits. The upgrade is therefore seamless end to end: the
// installed file is never busy, no watcher restarts, no polling state is
// lost, and nothing but the binary itself is written.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/reexec"
	"github.com/spf13/cobra"
)

// upgradeCheckInterval is how often a resident daemon checks whether the
// installed binary has been replaced by an upgrade. The check is one stat.
var upgradeCheckInterval = 15 * time.Second

// upgradeTakeoverTimeout bounds how long a daemon waits for its successor to
// complete the handoff before concluding the upgrade failed and announcing
// that it is still serving. A variable so tests can shorten it.
var upgradeTakeoverTimeout = 30 * time.Second

// spawnUpgradedDaemonFn spawns the upgraded binary as a replacement daemon.
// Package variable so tests can observe the spawn instead of exec'ing. It
// returns the successor's process handle so callers can check whether it
// stayed up (nil process means it could not be tracked).
var spawnUpgradedDaemonFn = spawnUpgradedDaemon

// maybeReexecFn relaunches a resident command from a runtime copy of the
// binary (issue #73). Package variable so tests run in place.
var maybeReexecFn = reexec.MaybeReexec

// startUpgradeWatcher launches the background check that hands the daemon off
// to an upgraded binary. It only runs when the daemon was launched through
// the runtime-copy launcher: without it, the running image maps the installed
// file, an upgrade could not have landed anyway, and replacing a mapped
// executable is exactly what cannot be done.
func startUpgradeWatcher(ctx context.Context, cmd *cobra.Command, socket string, interval time.Duration) {
	installed := os.Getenv(reexec.InstalledBinEnv)
	if installed == "" {
		return
	}
	go watchInstalledBinary(ctx, installed, socket, interval, cmd.ErrOrStderr())
}

// watchInstalledBinary stats the installed binary on every tick and, when it
// has changed, spawns it as a successor daemon. Success is silent from this
// side: the successor's handoff shuts this process down. Failure is loud:
// if no takeover happens within upgradeTakeoverTimeout, the daemon says so
// and keeps serving the old version until the next detected change.
func watchInstalledBinary(ctx context.Context, installed, socket string, interval time.Duration, stderr io.Writer) {
	base, err := os.Stat(installed)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gh-monitor daemon: cannot watch %s for upgrades (%v)\n", installed, err)
		return
	}
	ticker := time.NewTicker(upgradeCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cur, err := os.Stat(installed)
		if err != nil || (cur.Size() == base.Size() && cur.ModTime().Equal(base.ModTime())) {
			continue
		}
		_, _ = fmt.Fprintf(stderr, "gh-monitor daemon: upgrade detected (%s changed); handing off to the new binary\n", installed)
		if _, err := spawnUpgradedDaemonFn(installed, socket, interval); err != nil {
			_, _ = fmt.Fprintf(stderr, "gh-monitor daemon: spawning the upgraded daemon failed (%v); still serving\n", err)
			base = cur // retried only when the binary changes again
			continue
		}
		select {
		case <-ctx.Done():
			return // the successor took over; this process exits
		case <-time.After(upgradeTakeoverTimeout):
			_, _ = fmt.Fprintf(stderr,
				"gh-monitor daemon: the upgraded daemon did not take over within %s; still serving the old version\n",
				upgradeTakeoverTimeout)
			base = cur
		}
	}
}

// buildUpgradeCommand assembles the successor daemon's invocation.
func buildUpgradeCommand(installed, socket string, interval time.Duration) *exec.Cmd {
	return exec.Command(installed, "daemon", "--socket", socket, "--interval", strconv.Itoa(int(interval.Seconds())))
}

// ---------------------------------------------------------------------------
// Self-update (issue #69): the daemon pulls new releases itself
// ---------------------------------------------------------------------------

// selfUpdateRemovedEnv is the removed environment variable that used to gate
// self-update (issue #82 moved the setting into preferences.json). It is no
// longer honored anywhere, but a daemon that sees it set announces loudly
// where the setting lives now — an operator relying on the old spelling must
// never be silently ignored.
const selfUpdateRemovedEnv = "GH_MONITOR_SELFUPDATE"

// selfUpdateDefaultInterval is the release-check cadence when self-update is
// enabled without an explicit duration. The issue asks for roughly hourly —
// slow enough to be invisible, fast enough that agents never run long against
// a stale binary.
var selfUpdateDefaultInterval = time.Hour

// extensionUpgradeFn runs `gh extension upgrade` for this extension. Package
// variable so tests observe the call instead of shelling out. An upgrade
// that finds nothing newer succeeds quietly; one that lands rewrites the
// installed binary in place — which is exactly what the stat watcher above
// is waiting to notice, closing the loop: check → upgrade → handoff.
var extensionUpgradeFn = func() error {
	out, err := exec.Command("gh", "extension", "upgrade", "gh-monitor").CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh extension upgrade gh-monitor: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// selfUpdatePrefsLoadFn loads the global preferences document — the only
// config location selfUpdate is read from (issue #82). Package variable so
// tests can substitute a value without touching the filesystem.
var selfUpdatePrefsLoadFn = func() (prefs.Preferences, error) {
	return prefs.Load("") // empty baseDir: the XDG global path, never cwd-relative
}

// selfUpdateIntervalFromPrefs resolves the configured self-update cadence.
// Zero means off. A preferences file that cannot be read or parsed degrades
// to off with a logged reason — config trouble must never kill the daemon,
// and must never silently flip a resident process into auto-upgrading either.
func selfUpdateIntervalFromPrefs(stderr io.Writer) time.Duration {
	if v := strings.TrimSpace(os.Getenv(selfUpdateRemovedEnv)); v != "" {
		path, pathErr := prefs.ConfigPath("")
		if pathErr != nil {
			path = "~/.config/gh-monitor/preferences.json"
		}
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor daemon: %s=%s has been removed; set \"selfUpdate\" in %s instead (gh monitor prefs set) — issue #82\n",
			selfUpdateRemovedEnv, v, path)
	}
	p, err := selfUpdatePrefsLoadFn()
	if err != nil {
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor daemon: reading preferences (%v); self-update defaults to off\n", err)
	}
	return prefs.SelfUpdateInterval(p.SelfUpdate, selfUpdateDefaultInterval)
}

// startSelfUpdate launches the optional release-check loop. It only runs when
// two things are true: the operator asked for it via the global
// preferences file's selfUpdate value (issue #82), and the daemon was
// launched through the runtime-copy launcher — otherwise the running image
// maps the installed file and an upgrade could not land anyway (the write
// would fail with ETXTBSY).
func startSelfUpdate(ctx context.Context, cmd *cobra.Command) {
	every := selfUpdateIntervalFromPrefs(cmd.ErrOrStderr())
	if every <= 0 {
		return
	}
	if os.Getenv(reexec.InstalledBinEnv) == "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"gh-monitor daemon: selfUpdate is enabled in preferences but the daemon was not launched from a runtime copy; self-update disabled\n")
		return
	}
	go checkForReleases(ctx, every, cmd.ErrOrStderr())
}

// checkForReleases runs the extension upgrade on the configured cadence. A
// check that finds nothing newer is a quiet no-op; a landed upgrade rewrites
// the installed binary and the stat watcher hands the daemon off; a failed
// check is logged and retried on the next tick — never fatal, never
// disruptive to serving.
func checkForReleases(ctx context.Context, every time.Duration, stderr io.Writer) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := extensionUpgradeFn(); err != nil {
				_, _ = fmt.Fprintf(stderr, "gh-monitor daemon: self-update check failed (%v); retrying on the next tick\n", err)
			}
		}
	}
}

// spawnUpgradedDaemon starts the successor detached, so it outlives this
// process exiting after the handoff. The returned handle lets the caller
// verify the successor stayed up; it remains valid across the successor's
// own runtime-copy re-exec (same PID).
func spawnUpgradedDaemon(installed, socket string, interval time.Duration) (*os.Process, error) {
	cmd := buildUpgradeCommand(installed, socket, interval)
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn upgraded daemon: %w", err)
	}
	p := cmd.Process
	_ = p.Release()
	return p, nil
}

// ---------------------------------------------------------------------------
// Sub-daemon-mode upgrades (issue #84): the launcher self-updates too
// ---------------------------------------------------------------------------

// subdaemonUpgradeDecision tells runSubdaemonMode's generation loop what to do
// after the upgrade watcher stopped the current children.
type subdaemonUpgradeDecision int

const (
	// upgradeTakeover: the successor spawned and stayed up through the grace
	// window; this process should exit and let it own the service.
	upgradeTakeover subdaemonUpgradeDecision = iota
	// upgradeRelaunch: the successor failed to spawn or died within the grace
	// window; relaunch the children on the old binary so service converges.
	upgradeRelaunch
)

// subdaemonSuccessorAliveFn reports whether the freshly spawned successor is
// still running. Package variable so tests stub liveness.
var subdaemonSuccessorAliveFn = processAlive

// processAlive reports whether p is still running. A nil process is dead. An
// error other than os.ErrProcessDone (e.g. signals unsupported on Windows)
// optimistically reports alive: a false "dead" would tear down working
// children, while a false "alive" at worst leaves the successor in charge.
func processAlive(p *os.Process) bool {
	if p == nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return !errors.Is(err, os.ErrProcessDone)
	}
	return true
}

// startSubdaemonUpgradeWatcher arms the binary-change watcher for sub-daemon
// mode. There is no socket handoff here — the children own their sockets and
// their state — so a detected upgrade stops the children (releasing those
// sockets), spawns the successor, and reports one decision on the returned
// channel: upgradeTakeover (exit; the successor owns the service) or
// upgradeRelaunch (bring the children back on the old binary). The channel is
// only written when the watcher actually takes over, which the takingOver
// flag records for the generation loop.
func startSubdaemonUpgradeWatcher(ctx context.Context, cmd *cobra.Command, socket string, interval time.Duration, stopChildren func(), takingOver *atomic.Bool) <-chan subdaemonUpgradeDecision {
	decision := make(chan subdaemonUpgradeDecision, 1)
	installed := os.Getenv(reexec.InstalledBinEnv)
	if installed == "" {
		return decision // not launched from a runtime copy: nothing to watch
	}
	go watchSubdaemonBinary(ctx, installed, socket, interval, cmd.ErrOrStderr(), stopChildren, takingOver, decision)
	return decision
}

// watchSubdaemonBinary stats the installed binary on every tick and, when it
// has changed, walks the sub-daemon upgrade sequence: stop children, spawn
// the successor, hold a grace window to confirm it stays up, then report the
// decision. Failure is never fatal to convergence — every failure path ends
// in upgradeRelaunch, so some generation of children is always running.
func watchSubdaemonBinary(ctx context.Context, installed, socket string, interval time.Duration, stderr io.Writer, stopChildren func(), takingOver *atomic.Bool, decision chan<- subdaemonUpgradeDecision) {
	base, err := os.Stat(installed)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gh-monitor daemon: cannot watch %s for upgrades (%v)\n", installed, err)
		return
	}
	ticker := time.NewTicker(upgradeCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cur, err := os.Stat(installed)
		if err != nil || (cur.Size() == base.Size() && cur.ModTime().Equal(base.ModTime())) {
			continue
		}
		_, _ = fmt.Fprintf(stderr, "gh-monitor daemon: upgrade detected (%s changed); restarting sub-daemons on the new binary\n", installed)
		takingOver.Store(true)
		stopChildren() // children exit, releasing the sockets they own
		proc, spawnErr := spawnUpgradedDaemonFn(installed, socket, interval)
		if spawnErr == nil {
			// Grace: confirm the successor stays up before committing. A
			// successor that dies here would otherwise become an outage.
			deadline := time.Now().Add(upgradeTakeoverTimeout)
			for time.Now().Before(deadline) {
				if !subdaemonSuccessorAliveFn(proc) {
					spawnErr = errors.New("the upgraded daemon exited during the takeover grace window")
					break
				}
				select {
				case <-ctx.Done():
					return // root shutdown while waiting; nothing left to decide
				case <-time.After(50 * time.Millisecond):
				}
			}
		}
		if spawnErr != nil {
			_, _ = fmt.Fprintf(stderr,
				"gh-monitor daemon: upgrade handoff failed (%v); relaunching sub-daemons on the old version\n", spawnErr)
			base = cur // retried only when the binary changes again
			select {
			case decision <- upgradeRelaunch:
			default: // the loop also recovers via its decision timeout
			}
			continue
		}
		decision <- upgradeTakeover
		return
	}
}
