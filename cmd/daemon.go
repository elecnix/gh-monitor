package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/gh"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/broker"
	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/mux"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/elecnix/gh-monitor/internal/subdaemon"
)

// DaemonBackendName is the name the daemon announces itself under, and the
// name to pass to --backend to pin monitoring to it.
const DaemonBackendName = "daemon"

// brokerIdleCapDefault is the daemon's poll-cadence ceiling while the broker
// transport reports healthy — see internal/hub.Hub.SetBrokerIdleCap. 30
// minutes turns polling into a rare safety net (event-driven wakes carry the
// real load) without ever making a watched PR go fully silent even if a
// wake is somehow missed. Override with GH_MONITOR_BROKER_IDLE_CAP (seconds)
// — see broker.IdleCapFromEnv.
const brokerIdleCapDefault = 30 * time.Minute

// newDaemonCommand builds `gh monitor daemon`: a long-lived process that
// multiplexes one fetch loop per PR identity across many `gh monitor` client
// processes, so N agents watching the same PR share a single GitHub fetch
// instead of polling independently (issue #34).
func newDaemonCommand() *cobra.Command {
	var (
		socket   string
		interval int
	)
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run a shared-poller daemon that `gh monitor` clients attach to",
		Long: `Run a long-lived daemon that multiplexes one fetch loop per PR identity.

Client ` + "`gh monitor`" + ` processes detect the daemon via its Unix socket and stream
notifications from the shared poller instead of each polling GitHub. The
shared poller multiplexes every target kind — pull requests, refs, commits,
issues, workflow runs, and whole repositories. Watching requires it: if no
daemon can be attached, the client fails with an error rather than polling
in-process.

The daemon is persistent once started: it has no idle timeout and never exits
for lack of attached clients — a fleet of short-lived watchers never has to
re-bootstrap it. It serves until SIGTERM/SIGINT, or until a successor daemon
completes an upgrade handoff.

The daemon honours $GH_MONITOR_SOCK, $XDG_RUNTIME_DIR, and a per-user cache
dir for the socket path. Send SIGTERM/SIGINT to stop it cleanly.

Set "selfUpdate" in the global preferences file to have the daemon run
"gh extension upgrade gh-monitor" on a cadence and hand off to the new binary
when one lands (issue #82):

  gh monitor prefs set '{"selfUpdate": "30m"}'

The value is a Go duration, or "1"/"true" for the default hourly cadence; it
is off by default. It is a global-only setting: it is read from the operator's
preferences.json and nowhere else, because self-upgrading the installed binary
is a machine-wide act.

The poll cadence is configurable from the same file (issue #90), so scheduled
API spend can be tuned without restart-forgetting-flag discipline:

  gh monitor prefs set '{"pollInterval": "10m", "idlePollCeiling": "6h",
                         "pollWhenBrokerHealthy": false}'

  - pollInterval — the poller's base cadence; overrides --interval. Go
    duration, or ""/"0"/"false" to keep the flag/default.
  - idlePollCeiling — caps the exponential idle backoff for every target,
    broker-healthy or not (replaces the built-in 300s ceiling).
  - pollWhenBrokerHealthy — false suspends timer-driven fetching entirely
    while the broker wake path reports healthy; a degrade resumes polling
    immediately. Default true.

All three are global-only settings beside selfUpdate, read at daemon start.

Self-update works in both daemon configurations, because there is only one:
the daemon always owns the socket and the polling hub. An upgrade hands off
seamlessly over the socket — watched targets and connected watchers carry
over (issue #73) — and the daemon relaunches its configured sub-daemons on
the new binary as it exits.

Sub-daemon backends (issue #88): a daemons.conf that lists entries makes the
daemon launch and supervise those processes as children. They no longer take
over the socket: each child is pointed at its own private socket next to the
daemon's ($GH_MONITOR_SOCK is set for it), gh-monitor discovers which target
kinds each live child serves from its protocol hello, and watches for those
kinds are routed to the child while every other kind — and every resumable
watch — is served by the polling hub on the public socket. A child that dies
is restarted by the supervisor; meanwhile its kinds fall back to hub polling,
so a target is degraded-but-covered instead of unmonitored.

Set $GH_MONITOR_BROKER_ENDPOINT to also subscribe to a GitHub-webhook fan-out
broker: matching events wake the affected PR's fetch immediately instead of
waiting for the next tick, so a quiet PR polls far less often. It is purely
additive — a lost broker connection reports itself loudly and falls back to
normal interval polling within one cycle, never silence. See the README's
"Broker transport" section.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The daemon is the most resident process gh-monitor runs: keep the
			// installed binary's image unmapped so upgrades can rewrite it, and
			// let an upgraded binary take over via handoff (issue #73).
			if err := maybeReexecFn(); err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"gh-monitor daemon: could not relaunch from a runtime copy (%v); running from the installed binary\n", err)
			}
			if socket == "" {
				socket = ipc.DefaultSocketPath()
			}
			if interval < 10 {
				// The built-in default (issue #90): a slow trickle that only
				// exists as insurance against event loss.
				interval = 300
			}
			return runDaemon(cmd, socket, time.Duration(interval)*time.Second)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "Unix socket path (default: $GH_MONITOR_SOCK or $XDG_RUNTIME_DIR/gh-monitor.sock)")
	cmd.Flags().IntVar(&interval, "interval", 300, "Polling interval in seconds (min 10)")
	return cmd
}

func runDaemon(cmd *cobra.Command, socket string, interval time.Duration) error {
	// Poll-cadence preferences (issue #90): the config file overrides what the
	// --interval flag selected — the base cadence (pollInterval), the idle
	// backoff ceiling (idlePollCeiling), and whether timer polling runs at all
	// while the broker wake path is healthy (pollWhenBrokerHealthy). A load
	// failure degrades to the flag/default values rather than refusing to
	// start: a broken preferences file must not take down watching.
	daemonPrefs, perr := prefs.Load("")
	if perr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: ignoring unreadable preferences (%v)\n", perr)
		daemonPrefs = prefs.DefaultPreferences()
	}
	interval, idleCeiling, pauseWhenHealthy := prefs.ResolveDaemonCadence(daemonPrefs, interval)

	// Cancellation and signal handling come first: everything below — the
	// socket bind, the hub, sub-daemon supervision — stops through this ctx,
	// and registering the handler before binding means a client that sees the
	// socket come up can always stop the daemon with SIGTERM.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Sub-daemon backends (issue #88): if the operator configured sub-daemons,
	// the daemon launches and supervises them as children — but it no longer
	// concedes the socket to them (the v1.19.0–v1.22.0 behaviour, which left
	// every non-sub-daemon target kind unservable). Each child is pointed at
	// its own private socket next to the daemon socket, and watches for the
	// kinds a live child serves are routed to it; everything else — and every
	// resumable watch — is served by the polling hub below. The config path
	// resolves per-project first: <cwd>/.gh-monitor.conf if it exists, then
	// the operator's <user config dir>/gh-monitor/daemons.conf.
	esc, _ := os.Getwd()
	entries, cfgOK, subErr := subdaemon.Load(subdaemon.ResolveConfigPath(esc))
	if subErr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: sub-daemon config: %v\n", subErr)
	}
	var routes *mux.Registry
	var launcherDone <-chan struct{}
	if cfgOK && len(entries) > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"gh-monitor daemon: launching %d sub-daemon backend(s); gh-monitor owns %s and routes to them\n",
			len(entries), socket)
		routes = mux.NewRegistry(cmd.ErrOrStderr())
		for _, e := range entries {
			tr, terr := remote.ParseEndpoint("unix:" + mux.SocketPath(filepath.Dir(socket), e.Name))
			if terr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: sub-daemon %q: %v\n", e.Name, terr)
				continue
			}
			routes.Track(e.Name, tr)
		}
		l := subdaemonLauncherFn(entries, cmd.ErrOrStderr(), filepath.Dir(socket))
		done := make(chan struct{})
		launcherDone = done
		go func() {
			defer close(done)
			_ = l.Run(ctx) // returns after every child has been signaled and waited
		}()
		go routes.Run(ctx, mux.ProbeInterval)
	}

	listener, adopted, err := listenOrAdopt(ctx, socket)
	if err != nil {
		return err
	}
	// Closing the listener is what unblocks Accept when the daemon stops —
	// the signal goroutine above only cancels, because it is registered
	// before the listener exists.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	defer func() { _ = listener.Close() }()
	// Remove the socket on clean shutdown so clients fall back promptly —
	// unless it was handed off to a successor daemon, which now owns the
	// path (issue #73).
	var handedOff atomic.Bool
	defer func() {
		if !handedOff.Load() {
			_ = os.Remove(socket)
		}
	}()

	// One fetch function per identity, dispatching on the identity's target
	// kind (see gh.Fetch). The hub fans the single result out to every
	// subscribed client.
	fetch := gh.Fetch(apiClientFactory)

	// Ruleset function is called once per new PR poller to read the branch
	// ruleset and determine required status checks.
	rulesetFn := gh.Ruleset(apiClientFactory)

	// Budget guard: every poller stretches its cadence as the shared GraphQL
	// budget runs low. Advisory only — rate-limit errors keep their hard
	// backoff, and the rate_limit endpoint is read over REST (the two budgets
	// exhaust independently).
	budgetSvc := &monitor.Service{API: apiClientFactory("")}
	budget := monitor.NewBudgetGuard(budgetSvc, interval)

	h := hub.New(fetch, rulesetFn, interval, budget,
		hub.WithFailedRunLogFetcher(gh.FailedRunLogs(apiClientFactory)),
		hub.WithIdleCeiling(idleCeiling),
		hub.WithPauseWhenBrokerHealthy(pauseWhenHealthy))
	defer h.Stop()
	if adopted != nil {
		if err := h.RestoreState(*adopted); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: ignoring unusable handoff state (%v)\n", err)
		} else {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"gh-monitor daemon: adopted %d watched PR(s) and %d connected watcher(s) from the previous daemon\n",
				len(adopted.Pollers), len(adopted.Resumes))
		}
	}

	// The daemon is persistent once started (issue #69): it has no idle
	// timeout and never exits for lack of attached clients. Auto-start (the
	// first client bootstraps it) only works if the daemon outlives that
	// client — a fleet of short-lived watchers must not bootstrap a fresh
	// daemon on every invocation. It serves until SIGTERM/SIGINT, or until a
	// successor daemon completes an upgrade handoff.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon listening on %s (interval %s, idle ceiling %s)",
		socket, interval, idleCeiling)
	if pauseWhenHealthy {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), ", timer polling pauses while the broker is healthy")
	} else {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	}
	startBrokerTransport(ctx, cmd, h)
	startUpgradeWatcher(ctx, cmd, socket, interval)
	startSelfUpdate(ctx, cmd)

	srv := &daemonServer{
		hub:       h,
		listener:  listener,
		routes:    routes,
		handedOff: &handedOff,
		shutdown:  func() { cancel(); _ = listener.Close() },
	}

	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor: accept: %v\n", err)
				continue
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				serveClient(ctx, srv, c)
			}(conn)
		}
	}()

	<-ctx.Done()
	wg.Wait()
	// Give the launcher a bounded window to signal and reap its children so a
	// successor (upgrade handoff) or the operator is not left with orphans
	// holding the private sockets. A child that ignores SIGINT for this long
	// is left to die on its own rather than stalling the daemon's exit.
	if launcherDone != nil {
		select {
		case <-launcherDone:
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

// subdaemonLauncherFn builds the launcher runDaemon runs for configured
// sub-daemon entries. It is a package variable so tests can substitute a
// launcher that does not spawn real processes. sockDir is where the daemon's
// own socket lives: each child's private socket is placed next to it.
var subdaemonLauncherFn = func(entries []subdaemon.Entry, out io.Writer, sockDir string) *subdaemon.Launcher {
	return &subdaemon.Launcher{
		Entries: entries,
		Out:     out,
		// Point the child at its private socket (issue #88): sub-daemons bind
		// $GH_MONITOR_SOCK wherever it points, so this redirects them without
		// any change in the sub-daemon binary. gh-monitor keeps the public
		// socket and routes to these.
		ChildEnv: func(e subdaemon.Entry) []string {
			return append(os.Environ(), "GH_MONITOR_SOCK="+mux.SocketPath(sockDir, e.Name))
		},
	}
}

// handleHandoffOp serves the upgrade handoff's two protocol extensions
// (issue #73). OpHandoff returns this daemon's transferable watching state to
// the successor. OpHandoffFD — which only arrives after the state has been
// delivered — passes the listening socket itself, then shuts this daemon down
// without removing the socket path, which the successor now owns. The
// successor is serving on the adopted fd before this process exits, so
// clients re-connecting see the same socket, and their watchers resume from
// the baselines the state carried.
func (srv *daemonServer) handleHandoffOp(ctx context.Context, conn io.ReadWriter, req remote.Request) (bool, error) {
	switch req.Op {
	case handoff.OpHandoff:
		state := srv.hub.ExportState()
		raw, err := json.Marshal(state)
		if err != nil {
			_ = remote.WriteFrame(conn, remote.Frame{Error: fmt.Sprintf("encode handoff state: %v", err)})
			return true, nil
		}
		return true, remote.WriteFrame(conn, remote.Frame{Result: raw})

	case handoff.OpHandoffFD:
		uconn, ok := conn.(*net.UnixConn)
		if !ok {
			_ = remote.WriteFrame(conn, remote.Frame{Error: "handoff fd pass requires a Unix socket"})
			return true, nil
		}
		srv.handedOff.Store(true)
		if err := handoff.SendListener(uconn, srv.listener); err != nil {
			_ = remote.WriteFrame(conn, remote.Frame{Error: err.Error()})
			return true, err
		}
		// The successor now holds the listening socket: stop serving. Closing
		// the listener and cancelling ctx ends the accept loop and every
		// client connection; runDaemon's defers skip the socket removal
		// because handedOff is set.
		srv.shutdown()
		return true, nil
	}
	return false, nil
}

// hubSource adapts the shared poller to backend.Source, so the daemon serves
// the same protocol as any other out-of-process backend.
type hubSource struct{ hub *hub.Hub }

// startBrokerTransport wires the optional broker-backed transport (see
// internal/broker) into the daemon's hub, if configured via
// GH_MONITOR_BROKER_ENDPOINT. It is a no-op — logged once, loudly, so a
// user can always tell which transport is answering — when the variable is
// unset, which is the default for every gh-monitor install that has never
// heard of this feature.
//
// Wiring is deliberately thin: a wake only ever triggers an existing
// RefreshPR/RefreshRepo fetch through the hub, never derives PR state
// itself, and every connection-state transition flows straight into
// h.SetBrokerHealth so a lost connection is loud and falls back to normal
// interval polling within one poll cycle — never silence, per the "absence
// is not success" guidance in the broker's own operator documentation.
func startBrokerTransport(ctx context.Context, cmd *cobra.Command, h *hub.Hub) {
	cfg, ok := broker.ConfigFromEnv()
	if !ok {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"gh-monitor daemon: broker transport not configured (set GH_MONITOR_BROKER_ENDPOINT to enable) — polling only")
		return
	}

	h.SetBrokerIdleCap(broker.IdleCapFromEnv(brokerIdleCapDefault))

	w := broker.NewWatcher(cfg)
	w.OnState = func(state broker.State, err error) {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		h.SetBrokerHealth(state == broker.StateHealthy, detail)
		switch state {
		case broker.StateHealthy:
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: broker transport connected (topic %s)\n", cfg.Topic)
		case broker.StateDegraded:
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: broker transport degraded (%v) — falling back to interval polling\n", err)
		}
	}
	w.OnWake = func(owner, repo string, prNumber int) {
		if prNumber > 0 {
			_ = h.RefreshPR(resolver.Identity{Owner: owner, Repo: repo, Number: prNumber})
			return
		}
		// No PR number on the event (e.g. check_run/check_suite, which key
		// off a commit SHA): refresh every PR this daemon currently watches
		// for that repository rather than guess which one changed.
		h.RefreshRepo(owner, repo)
	}

	go func() {
		if err := w.Run(ctx); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: broker watcher stopped: %v\n", err)
		}
	}()

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: broker transport enabled (endpoint %s, topic %s)\n", cfg.Endpoint, cfg.Topic)
}

func (s hubSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	var ch <-chan backend.Update
	if opts.Once {
		// One-shot read: a single fetch + emit through the hub, no poller.
		// The returned channel closes after the current state is delivered.
		ch = s.hub.Once(ctx, t, opts)
	} else {
		// Subscribe detaches the consumer on its own when ctx is cancelled,
		// so the cancel function is deliberately dropped here: calling it
		// when Watch returns would detach before the caller reads anything.
		ch, _ = s.hub.Subscribe(ctx, t, opts)
	}
	if opts.Timeout <= 0 {
		return ch, nil
	}
	// A watch stops after the caller's timeout. The hub itself has no notion
	// of one (a shared poller outlives any single subscriber), so the
	// boundary enforces it: relay until the source closes or the timeout
	// fires, then close — a client reading the channel sees a clean EOF
	// either way.
	out := make(chan backend.Update, 16)
	timer := time.NewTimer(opts.Timeout)
	go func() {
		defer close(out)
		defer timer.Stop()
		for {
			select {
			case u, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- u:
				case <-timer.C:
					return
				}
			case <-timer.C:
				return
			}
		}
	}()
	return out, nil
}

// listenOrAdopt binds the daemon socket. When a live daemon already owns it
// (the normal state after an upgrade: the old binary is still resident), the
// new daemon performs an in-memory handoff instead — it receives the old
// daemon's watching state and its listening socket, and serves in its place
// with no gap on the path and no file written (issue #73). A handoff that
// cannot be completed (predecessor too old to speak it, or it stopped
// answering) returns the original "in use" error, which is today's
// behaviour.
func listenOrAdopt(ctx context.Context, socket string) (net.Listener, *hub.State, error) {
	listener, err := ipc.Listen(socket)
	if err == nil {
		return listener, nil, nil
	}
	if !errors.Is(err, ipc.ErrLiveDaemon) || ctx == nil {
		return nil, nil, err
	}
	adopted, state, aerr := handoff.Adopt(ctx, socket)
	if aerr != nil {
		return nil, nil, fmt.Errorf("%w (upgrade handoff failed: %v)", err, aerr)
	}
	return adopted, &state, nil
}

// daemonServer carries what serving one connection needs beyond the hub: the
// listener (for the upgrade handoff's fd pass), the shutdown hook that lets a
// successor daemon take over mid-flight (issue #73), and — when sub-daemons
// are configured — the registry that routes kinds to them (issue #88).
type daemonServer struct {
	hub       *hub.Hub
	listener  net.Listener
	routes    *mux.Registry
	handedOff *atomic.Bool
	shutdown  func()
}

// serveClient handles one client connection with the shared backend protocol.
func serveClient(ctx context.Context, srv *daemonServer, conn net.Conn) {
	// The hub shares a fetch; it does not read, mutate, or render. Declaring
	// only what it does is what lets a client fall back to the built-in backend
	// for everything else.
	var src backend.Source = hubSource{hub: srv.hub}
	if srv.routes != nil {
		// Sub-daemon kinds go to their owning sub-daemon; everything else —
		// and every resumable watch — stays on the hub.
		src = mux.RoutingSource{Reg: srv.routes, Fallback: hubSource{hub: srv.hub}}
	}
	cfg := remote.ServerConfig{
		Name:   DaemonBackendName,
		Kinds:  backend.AllKinds(),
		Source: src,
		// Watchers survive a daemon upgrade: the hello announces that a
		// dropped stream can be re-established with the same ResumeID, and
		// the handoff ops below transfer this daemon's state to its
		// successor when an upgraded daemon starts (issue #73).
		Resumable: true,
		HandleOp: func(ctx context.Context, conn io.ReadWriter, req remote.Request) (bool, error) {
			return srv.handleHandoffOp(ctx, conn, req)
		},
	}
	// Reads inside Serve are not context-aware, so closing the connection is
	// what unblocks them when the daemon shuts down.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	if err := remote.Serve(ctx, conn, cfg); err != nil && ctx.Err() == nil && !isDisconnect(err) {
		_, _ = fmt.Fprintf(os.Stderr, "gh-monitor: serve client: %v\n", err)
	}
}

// isDisconnect reports whether err is a client hanging up. Clients probe the
// socket to see whether a daemon is live and then close, so this is the
// common case, not a fault — logging it would bury real errors in noise.
func isDisconnect(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// spawnDaemonFn spawns a detached daemon bound to socket. It is a package
// variable so tests can substitute an in-process server instead of re-exec'ing
// the real binary.
var spawnDaemonFn = spawnDaemon

// daemonAutostart reports whether `gh monitor` clients should spawn a daemon
// when none is running. Default true; opt out with GH_MONITOR_AUTOSTART=0.
func daemonAutostart() bool {
	return os.Getenv("GH_MONITOR_AUTOSTART") != "0"
}

// autostartDaemon spawns a daemon (if none is listening) and waits for it to
// accept connections. It is race-safe: if several clients autostart at once,
// ipc.Listen's liveness probe ensures only one daemon binds the socket, and
// every client's WaitReady still succeeds against the winner.
func autostartDaemon(ctx context.Context, socket string, interval time.Duration) error {
	if err := spawnDaemonFn(socket, interval); err != nil {
		return err
	}
	return ipc.WaitReady(ctx, socket, 8*time.Second)
}

// spawnDaemon re-executes the current binary as a detached `gh monitor daemon`
// that outlives the client. Stdio is redirected to /dev/null and the process
// is detached from the parent terminal (setsid on Unix, CREATE_NEW_PROCESS_GROUP
// | DETACHED_PROCESS on Windows) so it survives the client exiting.
func spawnDaemon(socket string, interval time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()

	cmd := exec.Command(exe, "daemon", "--socket", socket, "--interval", strconv.Itoa(int(interval.Seconds())))
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	// Release the child so it is not reaped when this client exits.
	_ = cmd.Process.Release()
	return nil
}
