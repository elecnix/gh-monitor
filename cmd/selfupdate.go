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
