// Resident watch self-replacement (issue #216).
//
// The runtime-copy launcher (internal/reexec) lets `gh extension upgrade`
// rewrite the installed binary while a watch runs, but until this file existed
// the watch kept executing the image it started from: the daemon adopted the
// upgrade and the sessions did not. This file closes the client-side half. A
// resident watch stats the installed binary on the same slow tick the daemon
// uses, and when an upgrade has settled on disk it re-execs into it between
// polls, carrying its ResumeID so the daemon hands its baseline back.
//
// Content decides whether the binary changed, and metadata only pre-tests it.
// Every tick costs one stat. A stat change that stays stable for two ticks is
// a candidate, and only then does the watch hash the candidate and compare it
// with the image it runs. A touch, a chmod or a reinstall of the same build
// moves the stat and keeps the watch where it is.
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

	"github.com/elecnix/gh-monitor/backend"
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

	// watchDeadlineEnv carries a watch's --timeout deadline across an in-place
	// replacement, as UnixNano. The successor re-runs the same command line,
	// and recomputing the deadline from its own start would hand the caller
	// more watch time than --timeout promised (issue #216).
	watchDeadlineEnv = "GH_MONITOR_WATCH_DEADLINE"

	// maxClientReplacements caps one chain's self-replacements. The number is a
	// fuse, not a budget: a working watch replaces itself once per upgrade, so
	// a chain anywhere near this count is churning.
	maxClientReplacements = 4
)

// execIntoFn performs the in-place replacement. Package variable so tests
// observe the exec instead of replacing the test process.
var execIntoFn = reexec.ExecInto

// binaryFingerprintFn reads the installed binary's cheap stat fingerprint.
// It includes the inode change time, so an in-place rewrite that keeps size
// and mtime still makes a candidate. Package variable so tests can drive ticks
// without touching the filesystem.
var binaryFingerprintFn = reexec.ChangeFingerprint

// binarySumFn hashes a binary's content. It runs only for a candidate that
// settled over two ticks, never on a tick where the stat did not move.
// Package variable so tests can make a read fail.
var binarySumFn = reexec.ContentSum

// runningImageFn returns the path of the image this process runs. Package
// variable so tests can stand in a file whose content they choose.
var runningImageFn = os.Executable

// contentVerdict is what the content check concludes about a candidate.
type contentVerdict int

const (
	// contentUnconfirmed: a read or hash failed. The watch keeps serving and
	// checks the candidate again on the next tick.
	contentUnconfirmed contentVerdict = iota
	// contentSame: the candidate holds the bytes this process already runs.
	contentSame
	// contentDiffers: the candidate is another build.
	contentDiffers
)

// clientUpgrader watches the installed binary for a resident watch and records
// when the watch should replace itself. It requests a replacement and never
// performs one: the watch loop performs it between polls, after the in-flight
// batch is handled and its state written (decision 4 on issue #216).
type clientUpgrader struct {
	installed string
	stderr    io.Writer

	// running is the path of the image this process runs. In the normal case
	// it is the content-addressed runtime copy (internal/reexec), written once
	// and never rewritten, so its bytes are the running image's bytes. With
	// the relaunch opted out it is the installed file itself, which an upgrade
	// rewrites; runningSum is therefore taken when the watch starts.
	running string

	mu         sync.Mutex
	runningSum string // SHA-256 of the running image, "" until read
	// startRaced records that the installed file changed while the start
	// hash read it, and that the hash is not trusted as this image's content.
	// The file changed after this process started, so the next settled
	// candidate counts as another build.
	startRaced bool
	baseline   string // the fingerprint this chain has already acted on
	pending    string // a changed fingerprint seen once, not yet stable
	unreadable string // the candidate whose read failure was reported
	requested  bool
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
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor: cannot tell which file to watch for upgrades; this watch keeps serving the image it started with\n")
		return nil
	}
	if count := clientReplaceCount(); count >= maxClientReplacements {
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor: this watch has already replaced itself %d times; it keeps serving the current image and will not replace itself again\n",
			count)
		return nil
	}
	u := &clientUpgrader{installed: installed, stderr: stderr}
	if running, err := runningImageFn(); err == nil {
		u.running = running
	}
	if err := u.snapshot(); err != nil {
		_, _ = fmt.Fprintf(stderr, "gh-monitor: cannot watch %s for upgrades (%v); this watch keeps serving the image it started with\n", installed, err)
		return nil
	}
	go u.run(ctx)
	return u
}

// snapshot records the baseline fingerprint and the running image's hash,
// with the hash read between two stats of the installed file. When the
// running image is the installed file itself, an upgrade that lands during the
// hash would otherwise be recorded as this image's content, and the upgrade
// would later compare as "same content".
func (u *clientUpgrader) snapshot() error {
	before, err := binaryFingerprintFn(u.installed)
	if err != nil {
		return err
	}
	u.baseline = before
	if u.running == "" {
		return nil
	}
	sum, sumErr := binarySumFn(u.running)
	after, err := binaryFingerprintFn(u.installed)
	if err != nil {
		return err
	}
	switch {
	case sumErr != nil:
		// Unknown content: compareContent retries a runtime copy later and
		// reports any other path as unconfirmed.
	case after == before || reexec.IsRuntimeCopy(u.running):
		// A runtime copy is never rewritten, so its hash is good even when
		// the installed file moved during the read.
		u.runningSum = sum
	default:
		u.startRaced = true
	}
	return nil
}

// watchContinuitySafe reports whether a watch of this kind can replace itself
// without losing its place. A repo watch reads events from a cursor, and
// without --instance that cursor exists only in this process's memory: the
// successor would replay events the consumer already saw, or skip the ones
// that arrived during the exec. Other kinds resume through their ResumeID.
func watchContinuitySafe(kind backend.Kind, instance string) bool {
	return kind != backend.KindRepo || instance != ""
}

// startWatchUpgrade starts the upgrade watcher for a continuous target watch,
// or returns nil with one line on stderr when the watch cannot replace itself
// without losing its place.
func startWatchUpgrade(ctx context.Context, stderr io.Writer, kind backend.Kind, instance string) *clientUpgrader {
	if !watchContinuitySafe(kind, instance) {
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor: a repo watch without --instance keeps its cursor in memory only, so it keeps serving the image it started with after an upgrade; restart it, or start it with --instance NAME to let it replace itself\n")
		return nil
	}
	return startClientUpgrade(ctx, stderr)
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
// whether it requested a replacement. The recorded baseline advances once the
// content check reaches a verdict, so a replacement that fails to exec
// retries only when the binary changes again. A candidate that cannot be read
// stays pending, and the next tick checks it again.
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
	verdict, err := u.compareContent()
	switch verdict {
	case contentDiffers:
		u.pending = ""
		u.baseline = cur
		u.requested = true
		return true
	case contentSame:
		u.pending = ""
		u.baseline = cur
		u.logf("gh-monitor: %s changed on disk but has the same content as this watch's image; this watch keeps serving\n", u.installed)
		return false
	default:
		if u.unreadable != cur {
			u.unreadable = cur
			u.logf("gh-monitor: %s changed on disk, but its content could not be compared (%v); this watch keeps serving and checks again on the next tick\n", u.installed, err)
		}
		return false
	}
}

// compareContent hashes the candidate and compares it with the running image.
// Callers hold u.mu. The running image is hashed first, so a watch that
// cannot read its own image does not read the candidate on every tick.
func (u *clientUpgrader) compareContent() (contentVerdict, error) {
	if u.runningSum == "" && u.startRaced {
		// The installed file changed after this process started, so the
		// candidate is not the image this process runs.
		return contentDiffers, nil
	}
	if u.runningSum == "" {
		// Only a runtime copy can be read again safely: it is never rewritten.
		// Any other path may now hold the candidate's bytes.
		if u.running == "" || !reexec.IsRuntimeCopy(u.running) {
			return contentUnconfirmed, fmt.Errorf("the running image was not hashed at start")
		}
		sum, err := binarySumFn(u.running)
		if err != nil {
			return contentUnconfirmed, err
		}
		u.runningSum = sum
	}
	candidate, err := binarySumFn(u.installed)
	if err != nil {
		return contentUnconfirmed, err
	}
	if candidate == u.runningSum {
		return contentSame, nil
	}
	return contentDiffers, nil
}

// logf writes one line to stderr when the upgrader has one.
func (u *clientUpgrader) logf(format string, args ...any) {
	if u.stderr != nil {
		_, _ = fmt.Fprintf(u.stderr, format, args...)
	}
}

// ReplaceRequested reports whether a replacement is waiting to be performed at
// the next quiet point in the watch.
func (u *clientUpgrader) ReplaceRequested() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requested
}

// carriedWatchDeadline returns the --timeout deadline an earlier image of this
// watch set, or the zero time when there is none. A value that cannot be
// parsed is ignored: an unreadable timestamp must not end or extend a watch.
func carriedWatchDeadline() time.Time {
	raw := strings.TrimSpace(os.Getenv(watchDeadlineEnv))
	if raw == "" {
		return time.Time{}
	}
	ns, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// clientReplaceEnv is the environment for the successor image: this process's
// own, minus the re-exec depth, with the replacement count advanced by one.
//
// The successor is a new launch, not another hop: it starts from the installed
// binary and takes its own single hop into a runtime copy, so an inherited
// depth count would trip the re-exec fuse on a healthy chain. The count is
// advanced only in the slice handed to the exec, never in this process, so an
// exec that fails does not spend the chain's budget.
func clientReplaceEnv(deadline time.Time) []string {
	env := reexec.EnvWithout(reexec.ChildEnv(), clientReplaceCountEnv, watchDeadlineEnv)
	env = append(env, clientReplaceCountEnv+"="+strconv.Itoa(clientReplaceCount()+1))
	if !deadline.IsZero() {
		env = append(env, watchDeadlineEnv+"="+strconv.FormatInt(deadline.UnixNano(), 10))
	}
	return env
}

// Replace replaces this process with the installed binary, passing this watch's
// command line and environment, and the deadline the caller was promised. It
// does not return on success. On failure the watch keeps serving and the
// recorded baseline stays advanced, so the same change is not retried.
func (u *clientUpgrader) Replace(deadline time.Time) {
	u.mu.Lock()
	u.requested = false
	u.mu.Unlock()
	if err := execIntoFn(u.installed, os.Args, clientReplaceEnv(deadline)); err != nil {
		_, _ = fmt.Fprintf(u.stderr,
			"gh-monitor: replacing this watch with %s failed (%v); still serving, and it will not retry until the binary changes again\n",
			u.installed, err)
	}
}
