// Daemon identity and connected clients, asked over the daemon socket
// (issue #193).
//
// Two callers need this. A handoff successor asks before it takes the socket,
// so it can refuse a takeover that the service manager would not follow — a
// successor spawned outside the unit's cgroup is killed with the predecessor,
// and both lose. `gh monitor doctor` asks to compare what the daemon serves
// against what is actually running on the machine, which is how a watcher
// polling outside the daemon becomes visible instead of accumulating.

package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elecnix/gh-monitor/backend"
)

// OpInfo returns the daemon's Info. OpReload asks the resident daemon to
// spawn its own successor, which is the only way a reload can stay inside a
// service unit's cgroup.
const (
	OpInfo   = "daemon-info"
	OpReload = "daemon-reload"
)

// Info identifies a resident daemon and what it is doing.
type DaemonInfo struct {
	// PID is the daemon's process id. A successor compares it against its own
	// to tell "I am the daemon" from "somebody else is".
	PID int `json:"pid"`
	// Socket is the path it serves on.
	Socket string `json:"socket"`
	// Version is the daemon's own report, for diagnostics only.
	Version string `json:"version,omitempty"`
	// InvocationID is the service manager's invocation id ($INVOCATION_ID).
	// Two processes with the same non-empty value are the same service
	// instance; a successor with a different one was started outside the
	// unit, and taking the socket would leave a daemon that the unit's
	// teardown kills.
	InvocationID string `json:"invocationId,omitempty"`
	// Supervised reports that the daemon can notify a service manager, so a
	// handoff can be announced rather than survived by luck.
	Supervised bool `json:"supervised,omitempty"`
	// SocketActivated reports that a service manager holds the listening
	// socket, in which case no handout is needed or possible.
	SocketActivated bool `json:"socketActivated,omitempty"`
	// StartedAt is when the daemon began serving.
	StartedAt time.Time `json:"startedAt,omitempty"`
	// Clients are the connections the daemon is serving right now.
	Clients []ClientInfo `json:"clients,omitempty"`
}

// ClientInfo is one connected client, as the daemon sees it.
type ClientInfo struct {
	// PID is the client's process id, read from the connection's peer
	// credentials. Zero when the platform does not report one.
	PID int `json:"pid,omitempty"`
	// Resumes are the resume ids the client watches under, which is how a
	// reconnecting client is recognised after a handoff.
	Resumes []string `json:"resumes,omitempty"`
	// Targets are what the client asked to watch or read.
	Targets []backend.Target `json:"targets,omitempty"`
	// Watch reports a streaming watch as opposed to a one-shot read.
	Watch bool `json:"watch,omitempty"`
	// Since is when the connection was accepted.
	Since time.Time `json:"since,omitempty"`
}

// ErrUnsupported reports that the resident daemon predates an operation.
// Callers fall back to the behaviour they had before it existed; it is never
// a fault of the daemon.
var ErrUnsupported = errors.New("the resident daemon predates this operation")

// isUnsupported reports the error text remote.Serve answers an op it does not
// know with.
func isUnsupported(msg string) bool { return strings.HasPrefix(msg, "unsupported op ") }

// Info asks the daemon holding socket to describe itself.
func Info(ctx context.Context, socket string) (DaemonInfo, error) {
	f, err := request(ctx, socket, OpInfo)
	if err != nil {
		return DaemonInfo{}, err
	}
	if f.Error != "" {
		if isUnsupported(f.Error) {
			return DaemonInfo{}, fmt.Errorf("handoff: %s: %w", OpInfo, ErrUnsupported)
		}
		return DaemonInfo{}, fmt.Errorf("handoff: %s: %s", OpInfo, f.Error)
	}
	var info DaemonInfo
	if len(f.Result) == 0 {
		return DaemonInfo{}, fmt.Errorf("handoff: empty info response")
	}
	if err := json.Unmarshal(f.Result, &info); err != nil {
		return DaemonInfo{}, fmt.Errorf("handoff: decode info: %w", err)
	}
	return info, nil
}

// Reload asks the daemon holding socket to spawn its own successor and
// returns the successor's process id, or zero when the daemon could not
// report one. The successor performs the usual handoff; the caller waits for
// the socket to answer under a new pid.
func Reload(ctx context.Context, socket string) (int, error) {
	f, err := request(ctx, socket, OpReload)
	if err != nil {
		return 0, err
	}
	if f.Error != "" {
		if isUnsupported(f.Error) {
			return 0, fmt.Errorf("handoff: %s: %w", OpReload, ErrUnsupported)
		}
		return 0, fmt.Errorf("handoff: %s: %s", OpReload, f.Error)
	}
	var out struct {
		PID int `json:"pid"`
	}
	if len(f.Result) > 0 {
		if err := json.Unmarshal(f.Result, &out); err != nil {
			return 0, fmt.Errorf("handoff: decode reload response: %w", err)
		}
	}
	return out.PID, nil
}
