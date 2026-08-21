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

Self-update works in both daemon modes: in polling mode an upgrade hands off
seamlessly over the socket; in sub-daemon mode the launcher stops its children
(releasing their sockets), starts the successor, and exits once the successor
proves it stays up — relaunching the old children if the successor fails, so
service always converges.

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
				interval = 60
			}
			return runDaemon(cmd, socket, time.Duration(interval)*time.Second)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "Unix socket path (default: $GH_MONITOR_SOCK or $XDG_RUNTIME_DIR/gh-monitor.sock)")
	cmd.Flags().IntVar(&interval, "interval", 60, "Polling interval in seconds (min 10)")
	return cmd
}

func runDaemon(cmd *cobra.Command, socket string, interval time.Duration) error {
	// Sub-daemon mode: if the operator has configured sub-daemons, the daemon
	// launches and supervises them instead of running the polling hub. The
	// sub-daemons are expected to bind the socket themselves (they are
	// backends that speak the backend/remote protocol) — so in this mode the
	// daemon does not bind it, avoiding contention. With no config file, the
	// daemon works exactly as it does today (pure polling). The config path
	// resolves per-project first: <cwd>/.gh-monitor.conf if it exists, then
	// the operator's <user config dir>/gh-monitor/daemons.conf.
	esc, _ := os.Getwd()
	entries, cfgOK, subErr := subdaemon.Load(subdaemon.ResolveConfigPath(esc))
	if subErr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: sub-daemon config: %v\n", subErr)
	}
	if cfgOK && len(entries) > 0 {
		return runSubdaemonMode(cmd.Context(), cmd, entries, socket, interval)
	}

	listener, adopted, err := listenOrAdopt(cmd.Context(), socket)
	if err != nil {
		return err
	}
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
		hub.WithFailedRunLogFetcher(gh.FailedRunLogs(apiClientFactory)))
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
			_ = listener.Close()
		case <-ctx.Done():
		}
	}()

	// The daemon is persistent once started (issue #69): it has no idle
	// timeout and never exits for lack of attached clients. Auto-start (the
	// first client bootstraps it) only works if the daemon outlives that
	// client — a fleet of short-lived watchers must not bootstrap a fresh
	// daemon on every invocation. It serves until SIGTERM/SIGINT, or until a
	// successor daemon completes an upgrade handoff.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon listening on %s (interval %s)\n", socket, interval)
	startBrokerTransport(ctx, cmd, h)
	startUpgradeWatcher(ctx, cmd, socket, interval)
	startSelfUpdate(ctx, cmd)

	srv := &daemonServer{
		hub:       h,
		listener:  listener,
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
	return nil
}

// subdaemonLauncherFn builds the launcher runSubdaemonMode runs. It is a
// package variable so tests can substitute a launcher that does not spawn
// real processes.
var subdaemonLauncherFn = func(entries []subdaemon.Entry, out io.Writer) *subdaemon.Launcher {
	return &subdaemon.Launcher{Entries: entries, Out: out}
}

// runSubdaemonMode launches and supervises the configured sub-daemons and
// nothing else — it does not bind the polling socket, so the sub-daemons own
// it. On SIGTERM/SIGINT it signals every child and returns.
//
// It also arms self-update (issue #84): the launcher is as resident as the
// polling hub, so it runs the release checker and, when the installed binary
// changes, hands the whole fleet to a successor — stopping its children first
// so they release their sockets, then exiting once the successor proves it
// stays up. A successor that fails to spawn or dies within the grace window
// is answered by relaunching the children on the old binary: service always
// converges to some running generation.
func runSubdaemonMode(ctx context.Context, cmd *cobra.Command, entries []subdaemon.Entry, socket string, interval time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
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

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"gh-monitor daemon: sub-daemon mode — launching %d sub-daemon(s) (polling hub off; sub-daemons own the socket)\n",
		len(entries))

	var takingOver atomic.Bool
	for {
		takingOver.Store(false)
		genCtx, genCancel := context.WithCancel(ctx)
		startSelfUpdate(genCtx, cmd)
		decision := startSubdaemonUpgradeWatcher(genCtx, cmd, socket, interval, genCancel, &takingOver)

		_ = subdaemonLauncherFn(entries, cmd.ErrOrStderr()).Run(genCtx)
		genCancel()
		if ctx.Err() != nil {
			return nil // operator SIGTERM/SIGINT: done
		}
		if !takingOver.Load() {
			return nil // every entry gave up on its own; nothing to hand off
		}
		select {
		case d := <-decision:
			if d == upgradeTakeover {
				return nil // the successor owns the service now
			}
			// upgradeRelaunch: loop into the next generation on the old binary.
		case <-time.After(2 * upgradeTakeoverTimeout):
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"gh-monitor daemon: upgrade watcher never reported a decision; relaunching sub-daemons\n")
		}
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
// listener (for the upgrade handoff's fd pass) and the shutdown hook that
// lets a successor daemon take over mid-flight (issue #73).
type daemonServer struct {
	hub       *hub.Hub
	listener  net.Listener
	handedOff *atomic.Bool
	shutdown  func()
}

// serveClient handles one client connection with the shared backend protocol.
func serveClient(ctx context.Context, srv *daemonServer, conn net.Conn) {
	// The daemon shares a fetch; it does not read, mutate, or render. Declaring
	// only what it does is what lets a client fall back to the built-in backend
	// for everything else.
	cfg := remote.ServerConfig{
		Name:   DaemonBackendName,
		Kinds:  backend.AllKinds(),
		Source: hubSource{hub: srv.hub},
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
