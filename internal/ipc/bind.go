package ipc

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// BindResultEnv names the environment variable through which a client tells
// the daemon it spawned where to report whether it ended up serving the
// socket (issue #170).
//
// The daemon is spawned detached with its stdio on /dev/null, so stdout and
// stderr cannot carry the answer back; a file path can. The client creates
// the path, passes it here, and reads the outcome once the socket answers.
//
// The daemon is the only process that knows the answer: ipc.Listen binds the
// socket, and its liveness probe is what makes concurrent cold starts safe —
// exactly one daemon owns the socket, and every client's WaitReady succeeds
// against that owner.
const BindResultEnv = "GH_MONITOR_BIND_RESULT"

// The outcomes a daemon reports through the bind-result file.
const (
	// BindWon means this process owns the socket and is the daemon serving
	// it.
	BindWon = "won"
	// BindLost means this process does not serve the socket: another daemon
	// owns it, or this daemon was replaced by a successor that took the
	// socket over.
	BindLost = "lost"
)

// ErrBindResult reports a bind-result that could not be read or does not
// carry a known outcome. It is never fatal: an unreadable result means the
// answer is unknown, and the caller falls back to its previous behaviour.
var ErrBindResult = errors.New("bind result unavailable")

// bindResultPollInterval is how often a waiting client re-reads the
// bind-result file. The daemon writes it within milliseconds of its bind
// decision, so this only has to close the gap between the socket becoming
// answerable and the file landing on disk.
const bindResultPollInterval = 10 * time.Millisecond

// NewBindResultPath returns a unique path for a bind-result file. Each spawn
// gets its own, so two clients cold-starting the same socket never read each
// other's answer.
//
// The file is created and removed, so the path is reserved but empty until a
// daemon writes to it. An empty file means "not reported yet", which is not
// the same as "not reported at all".
func NewBindResultPath() (string, error) {
	f, err := os.CreateTemp("", "gh-monitor-bind-*.result")
	if err != nil {
		return "", fmt.Errorf("create bind result file: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return name, nil
}

// BindResultPathFromEnv returns the bind-result path the client asked for,
// or "" when this daemon was not spawned by a client waiting for an answer.
func BindResultPathFromEnv() string {
	return os.Getenv(BindResultEnv)
}

// EnvWithBindResult returns env with the bind-result variable set to path,
// replacing any value it already carries.
//
// Replacing rather than appending matters: a process that already has the
// variable set — a daemon spawning a successor, say — would otherwise leave
// two entries in the child's environment, and the child would read the first
// one it finds, which is not the answer the client is waiting for.
func EnvWithBindResult(env []string, path string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, BindResultEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	if path != "" {
		out = append(out, BindResultEnv+"="+path)
	}
	return out
}

// WriteBindResultTo reports the bind outcome to path.
//
// A failure to write is deliberately swallowed: the answer only sharpens the
// client's certainty, and a daemon that cannot report it must still serve. The
// client treats a missing result as unknown.
func WriteBindResultTo(path, outcome string) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(outcome), 0o600)
}

// ReadBindResult reads the outcome a spawned daemon reported. It returns
// BindWon or BindLost when the answer is known, and an error wrapping
// ErrBindResult when the file is missing, unreadable, empty, or carries an
// unrecognised value.
func ReadBindResult(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBindResult, err)
	}
	switch outcome := strings.TrimSpace(string(raw)); outcome {
	case BindWon:
		return BindWon, nil
	case BindLost:
		return BindLost, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrBindResult, outcome)
	}
}

// WaitForBindResult reads the bind outcome, waiting up to timeout for the
// daemon to report it.
//
// The wait exists because the socket can answer before the file lands: the
// daemon binds, and only then writes down that it won. Reading once would
// mistake that gap for an unknown answer and fall back to the old behaviour —
// which is exactly the bug this mechanism exists to fix. The wait is bounded:
// a daemon that never reports (an older build, or one that died before it
// could) must not hold the client, and an unknown answer is a safe fallback.
func WaitForBindResult(path string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		outcome, err := ReadBindResult(path)
		if err == nil {
			return outcome, nil
		}
		if time.Now().After(deadline) {
			return "", err
		}
		time.Sleep(bindResultPollInterval)
	}
}

// RemoveBindResult deletes a bind-result file. The client calls it once it
// has read the answer, so the temporary files do not accumulate.
func RemoveBindResult(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}
