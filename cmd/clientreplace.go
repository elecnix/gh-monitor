// Resident watch self-replacement (issue #216).
//
// The runtime-copy launcher (internal/reexec) lets `gh extension upgrade`
// rewrite the installed binary while a watch runs, but until this file existed
// the watch kept executing the image it started from: the daemon adopted the
// upgrade and the sessions did not. This file closes the client-side half. A
// resident watch stats the installed binary on the same slow tick the daemon
// uses, and when an upgrade has settled on disk it re-execs into it between
// polls, carrying its ResumeID so the daemon hands its baseline back.
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/internal/reexec"
)

const (
	// watchResumeIDEnv carries a watch's ResumeID across an in-place
	// replacement. The successor reconnects to the daemon under the same ID,
	// so the daemon seeds it with this watcher's baseline: no second
	// first-poll, no replayed event (issue #216).
	watchResumeIDEnv = "GH_MONITOR_WATCH_RESUME_ID"

	// clientReplaceCountEnv counts how many times one watch chain has replaced
	// itself, across each exec. A broken upgrade script that rewrites the
	// binary every few seconds would otherwise relaunch the watch forever.
	clientReplaceCountEnv = "GH_MONITOR_CLIENT_REPLACE_COUNT"

	// maxClientReplacements caps one chain's self-replacements. The number is a
	// fuse, not a budget: a working watch replaces itself once per upgrade, so
	// a chain anywhere near this count is churning.
	maxClientReplacements = 4
)

// execIntoFn performs the in-place replacement. Package variable so tests
// observe the exec instead of replacing the test process.
var execIntoFn = reexec.ExecInto

// binaryFingerprintFn reads the installed binary's cheap stat fingerprint.
// Package variable so tests can drive ticks without touching the filesystem.
var binaryFingerprintFn = reexec.Fingerprint

// clientUpgrader watches the installed binary for a resident watch and records
// when the watch should replace itself. It requests a replacement and never
// performs one: the watch loop performs it between polls, after the in-flight
// batch is handled and its state written (decision 4 on issue #216).
type clientUpgrader struct {
	installed string
	stderr    io.Writer

	mu        sync.Mutex
	baseline  string // the fingerprint this chain has already acted on
	pending   string // a changed fingerprint seen once, not yet stable
	requested bool
}

// watchResumeID returns this watch's ResumeID. A successor reuses the one its
// predecessor exported, which is what makes the replacement seamless from the
// daemon's side; a fresh watch generates and exports one.
func watchResumeID() string {
	if id := strings.TrimSpace(os.Getenv(watchResumeIDEnv)); id != "" {
		return id
	}
	id := newResumeID()
	if id != "" {
		_ = os.Setenv(watchResumeIDEnv, id)
	}
	return id
}

// clientReplaceCount reads how many replacements this watch chain has taken.
func clientReplaceCount() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(clientReplaceCountEnv)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// startClientUpgrade returns the upgrader for a resident watch, or nil when no
// replacement is possible: the platform cannot exec in place, the chain reached
// its replacement cap, or there is nothing meaningful to watch. Every nil answer
// comes with one line saying so, because a watch that keeps serving the old
// image must not pretend it adopted the upgrade.
func startClientUpgrade(ctx context.Context, stderr io.Writer) *clientUpgrader {
	if !reexec.Supported() {
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor: this platform cannot replace a running process in place; this watch keeps serving the image it started with\n")
		return nil
	}
	installed := upgradeTarget()
	if installed == "" {
		return nil
	}
	if count := clientReplaceCount(); count >= maxClientReplacements {
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor: this watch has already replaced itself %d times; it keeps serving the current image and will not replace itself again\n",
			count)
		return nil
	}
	baseline, err := binaryFingerprintFn(installed)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gh-monitor: cannot watch %s for upgrades (%v); this watch keeps serving the image it started with\n", installed, err)
		return nil
	}
	u := &clientUpgrader{installed: installed, stderr: stderr, baseline: baseline}
	go u.run(ctx)
	return u
}

// run stats the installed binary on every tick. A new fingerprint must be seen
// on two consecutive ticks before it requests a replacement, so a file that is
// still being written is never executed.
func (u *clientUpgrader) run(ctx context.Context) {
	ticker := time.NewTicker(upgradeCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cur, err := binaryFingerprintFn(u.installed)
		if err != nil {
			continue
		}
		if u.observe(cur) {
			_, _ = fmt.Fprintf(u.stderr,
				"gh-monitor: the installed binary changed (%s); replacing this watch between polls\n", u.installed)
		}
	}
}

// observe folds one tick's fingerprint into the state machine and reports
// whether it requested a replacement. The recorded baseline advances on the
// request, so a replacement that fails to exec retries only when the binary
// changes again.
func (u *clientUpgrader) observe(cur string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if cur == u.baseline {
		u.pending = ""
		return false
	}
	if cur != u.pending {
		// First sighting of this change: a half-written file looks exactly
		// like this, so wait for a second tick to agree.
		u.pending = cur
		return false
	}
	u.pending = ""
	u.baseline = cur
	u.requested = true
	return true
}

// ReplaceRequested reports whether a replacement is waiting to be performed at
// the next quiet point in the watch.
func (u *clientUpgrader) ReplaceRequested() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requested
}

// clientReplaceEnv is the environment for the successor image: this process's
// own, minus the re-exec depth, with the replacement count advanced by one.
//
// The successor is a new launch, not another hop: it starts from the installed
// binary and takes its own single hop into a runtime copy, so an inherited
// depth count would trip the re-exec fuse on a healthy chain. The count is
// advanced only in the slice handed to the exec, never in this process, so an
// exec that fails does not spend the chain's budget.
func clientReplaceEnv() []string {
	env := reexec.EnvWithout(reexec.ChildEnv(), clientReplaceCountEnv)
	return append(env, clientReplaceCountEnv+"="+strconv.Itoa(clientReplaceCount()+1))
}

// Replace replaces this process with the installed binary, passing this watch's
// command line and environment. It does not return on success. On failure the
// watch keeps serving and the recorded baseline stays advanced, so the same
// change is not retried.
func (u *clientUpgrader) Replace() {
	u.mu.Lock()
	u.requested = false
	u.mu.Unlock()
	if err := execIntoFn(u.installed, os.Args, clientReplaceEnv()); err != nil {
		_, _ = fmt.Fprintf(u.stderr,
			"gh-monitor: replacing this watch with %s failed (%v); still serving, and it will not retry until the binary changes again\n",
			u.installed, err)
	}
}
