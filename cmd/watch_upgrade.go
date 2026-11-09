// Resident watch self-replacement (issue #216).
//
// The daemon has watched the installed binary since issue #73/#74: when an
// upgrade lands it spawns the new binary and hands its state over. A resident
// *watch client* had no such watcher, so the process that polls, diffs and
// prints the first-poll ack kept executing the image it started with. An
// upgrade that landed on disk therefore reached the daemon and never reached
// the watchers — and `gh monitor reload` restarts the daemon, not them.
//
// This closes that half. A continuous watch checks the installed binary on a
// slow tick, and when a *different* binary has settled there, replaces its own
// process image with it (replaceProcess). Because it execs rather than spawns,
// the PID and the parent's pipes are unchanged: the session sees no gap, no new
// process, and no restart. The one thing the new image must not lose is where
// its stream was, so the resume ID travels in the environment
// (watchResumeIDEnv) and the daemon hands back that watcher's baseline — the
// same continuity mechanism a daemon handoff uses, no second one.
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/reexec"
	"github.com/elecnix/gh-monitor/internal/upgradewatch"
)

const (
	// watchResumeIDEnv carries a watch's resume ID across its own
	// replacement, so the reconnected watcher claims the baseline its
	// predecessor's stream had already delivered.
	watchResumeIDEnv = "GH_MONITOR_WATCH_RESUME_ID"

	// watchReexecCountEnv counts consecutive self-replacements of one watch.
	// The tracker's content comparison already makes an unchanged binary a
	// no-op; this bounds the pathological case where the installed file keeps
	// changing, so a watch can never relaunch itself forever.
	watchReexecCountEnv = "GH_MONITOR_WATCH_REEXEC_COUNT"

	// maxWatchReexecs is how many consecutive replacements one watch will
	// perform before concluding something is wrong and keeping its image.
	maxWatchReexecs = 3
)

// watchUpgradeInterval is how often a continuous watch stats the installed
// binary. One stat per tick. A variable so tests can shorten it.
var watchUpgradeInterval = upgradewatch.DefaultInterval

// replaceProcessFn replaces the current process image with the binary at path,
// keeping argv and environment. Package variable so tests observe the decision
// instead of exec'ing.
var replaceProcessFn = replaceProcess

// watchContinuitySafe reports whether replacing this watch preserves its stream
// exactly. PR, issue, ref and commit watches resume from the baseline the
// daemon holds for their resume ID; a named instance resumes from its persisted
// cursor whatever the kind. A repo watch without an instance keeps its cursor
// only in memory, so replacing it would either replay events the operator has
// seen or skip the ones that arrived while it was replaced — it keeps its
// binary until the session restarts it.
func watchContinuitySafe(kind backend.Kind, instance string) bool {
	return kind != backend.KindRepo || instance != ""
}

// resumeIDFromEnv returns the resume ID inherited from a replaced watch, if
// any.
func resumeIDFromEnv() string {
	return os.Getenv(watchResumeIDEnv)
}

// startWatchUpgradeWatch launches the tick that replaces this watch when the
// installed binary changes. It runs only when the watch was launched through
// the runtime-copy launcher (otherwise the running image maps the installed
// file and an upgrade could not have landed) and only for a continuous watch
// that has a resume ID to reconnect with — replacing a watch that cannot resume
// would replay its first poll, which is worse than running a version behind.
func startWatchUpgradeWatch(ctx context.Context, resumeID string, continuitySafe bool, stderr io.Writer) {
	installed := os.Getenv(reexec.InstalledBinEnv)
	if installed == "" || resumeID == "" || !continuitySafe {
		return
	}
	go watchInstalledForWatch(ctx, installed, resumeID, stderr)
}

// watchInstalledForWatch is the tick loop. A change it cannot act on is
// reported once and then abandoned: a watch that keeps a working stream is
// better than one that spins on a file it cannot exec.
func watchInstalledForWatch(ctx context.Context, installed, resumeID string, stderr io.Writer) {
	tracker, err := upgradewatch.New(installed)
	if err != nil {
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor: cannot watch %s for upgrades (%v); this watch keeps its current binary\n",
			installed, err)
		return
	}
	watchInstalledWithTracker(ctx, tracker, installed, resumeID, stderr)
}

// watchInstalledWithTracker is the tick loop proper, split out so tests can
// hand it a tracker built against the binary the process is already running
// and then change the file — no sleeps, no race about when the baseline was
// sampled.
func watchInstalledWithTracker(ctx context.Context, tracker *upgradewatch.Tracker, installed, resumeID string, stderr io.Writer) {
	ticker := time.NewTicker(watchUpgradeInterval)
	defer ticker.Stop()
	reported := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		changed, err := tracker.Poll()
		if err != nil {
			if !reported {
				_, _ = fmt.Fprintf(stderr,
					"gh-monitor: checking %s for upgrades failed (%v); keeping the current binary\n",
					installed, err)
				reported = true
			}
			continue
		}
		reported = false
		if !changed {
			continue
		}

		count, _ := strconv.Atoi(os.Getenv(watchReexecCountEnv))
		if count >= maxWatchReexecs {
			_, _ = fmt.Fprintf(stderr,
				"gh-monitor: the installed binary changed again after %d restarts of this watch; "+
					"keeping the current one rather than relaunching in a loop\n", count)
			return
		}
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor: installed binary changed; restarting this watch on the new one (restart %d of %d)\n",
			count+1, maxWatchReexecs)
		_ = os.Setenv(watchResumeIDEnv, resumeID)
		_ = os.Setenv(watchReexecCountEnv, strconv.Itoa(count+1))
		if err := replaceProcessFn(installed); err != nil {
			_, _ = fmt.Fprintf(stderr,
				"gh-monitor: could not restart this watch on the new binary (%v); still serving the old one\n", err)
			return
		}
		return
	}
}
