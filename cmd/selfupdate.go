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
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

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
// Package variable so tests can observe the spawn instead of exec'ing.
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
		if err := spawnUpgradedDaemonFn(installed, socket, interval); err != nil {
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

// selfUpdateEnv controls whether a resident daemon checks for new releases of
// gh-monitor and upgrades the installed binary itself. Unset or "0" disables
// it; "1" or "true" uses selfUpdateDefaultInterval; any Go duration ("30m",
// "2h") sets the cadence. Off by default: auto-upgrading a CLI under the
// operator's feet is an explicit choice, not a default.
const selfUpdateEnv = "GH_MONITOR_SELFUPDATE"

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

// selfUpdateIntervalFromEnv parses GH_MONITOR_SELFUPDATE. Zero means off.
func selfUpdateIntervalFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv(selfUpdateEnv))
	if v == "" || v == "0" || v == "false" {
		return 0
	}
	if v == "1" || v == "true" {
		return selfUpdateDefaultInterval
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return 0 // unparseable values disable rather than guess
}

// startSelfUpdate launches the optional release-check loop. It only runs when
// two things are true: the operator asked for it via GH_MONITOR_SELFUPDATE,
// and the daemon was launched through the runtime-copy launcher — otherwise
// the running image maps the installed file and an upgrade could not land
// anyway (the write would fail with ETXTBSY).
func startSelfUpdate(ctx context.Context, cmd *cobra.Command) {
	every := selfUpdateIntervalFromEnv()
	if every <= 0 {
		return
	}
	if os.Getenv(reexec.InstalledBinEnv) == "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"gh-monitor daemon: %s is set but the daemon was not launched from a runtime copy; self-update disabled\n", selfUpdateEnv)
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
// process exiting after the handoff.
func spawnUpgradedDaemon(installed, socket string, interval time.Duration) error {
	cmd := buildUpgradeCommand(installed, socket, interval)
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn upgraded daemon: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}
