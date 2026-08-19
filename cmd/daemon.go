package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/broker"
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
notifications from the shared poller instead of each polling GitHub. When no
daemon is running, ` + "`gh monitor`" + ` falls back to its usual in-process polling, so
existing behaviour is unchanged.

The daemon honours $GH_MONITOR_SOCK, $XDG_RUNTIME_DIR, and a per-user cache
dir for the socket path. Send SIGTERM/SIGINT to stop it cleanly.

Set $GH_MONITOR_BROKER_ENDPOINT to also subscribe to a GitHub-webhook fan-out
broker: matching events wake the affected PR's fetch immediately instead of
waiting for the next tick, so a quiet PR polls far less often. It is purely
additive — a lost broker connection reports itself loudly and falls back to
normal interval polling within one cycle, never silence. See the README's
"Broker transport" section.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	// daemon works exactly as it does today (pure polling).
	entries, cfgOK, subErr := subdaemon.Load(subdaemon.DefaultConfigPath())
	if subErr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: sub-daemon config: %v\n", subErr)
	}
	if cfgOK && len(entries) > 0 {
		return runSubdaemonMode(cmd, entries)
	}

	listener, err := ipc.Listen(socket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	// Remove the socket on clean shutdown so clients fall back promptly.
	defer func() { _ = os.Remove(socket) }()

	// One fetch function per identity. Each call goes through the real gh CLI
	// client at the poller's current query tier (shedding low-priority
	// surfaces as the GraphQL budget runs low); the hub fans the single result
	// out to every subscribed client.
	fetch := func(ctx context.Context, id resolver.Identity, tier monitor.QueryTier) (*monitor.PullRequest, error) {
		svc := &monitor.Service{API: apiClientFactory(id.Host)}
		resp, err := svc.FetchWithTier(&id, id.Number, tier)
		if err != nil {
			return nil, err
		}
		return resp.Repository.PullRequest, nil
	}

	// Ruleset function is called once per new poller to read the branch
	// ruleset and determine required status checks.
	rulesetFn := func(owner, repo string) (*monitor.RulesetChecks, error) {
		svc := &monitor.Service{API: apiClientFactory("")}
		return svc.FetchRequiredChecks(owner, repo)
	}

	// Budget guard: every poller stretches its cadence as the shared GraphQL
	// budget runs low. Advisory only — rate-limit errors keep their hard
	// backoff, and the rate_limit endpoint is read over REST (the two budgets
	// exhaust independently).
	budgetSvc := &monitor.Service{API: apiClientFactory("")}
	budget := monitor.NewBudgetGuard(budgetSvc, interval)

	h := hub.New(fetch, rulesetFn, interval, budget)
	defer h.Stop()

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

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon listening on %s (interval %s)\n", socket, interval)
	startBrokerTransport(ctx, cmd, h)

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
				serveClient(ctx, h, c)
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
func runSubdaemonMode(cmd *cobra.Command, entries []subdaemon.Entry) error {
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

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"gh-monitor daemon: sub-daemon mode — launching %d sub-daemon(s) (polling hub off; sub-daemons own the socket)\n",
		len(entries))
	return subdaemonLauncherFn(entries, cmd.ErrOrStderr()).Run(ctx)
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
	if t.Kind != backend.KindPR {
		return nil, fmt.Errorf("the shared poller only serves pull requests, not %s", t.Kind)
	}
	ch, unsub := s.hub.SubscribePR(ctx, t, opts)
	go func() {
		<-ctx.Done()
		unsub()
	}()
	return ch, nil
}

// serveClient handles one client connection with the shared backend protocol.
func serveClient(ctx context.Context, h *hub.Hub, conn net.Conn) {
	// The daemon shares a fetch; it does not read, mutate, or render. Declaring
	// only what it does is what lets a client fall back to the built-in backend
	// for everything else.
	cfg := remote.ServerConfig{
		Name:   DaemonBackendName,
		Kinds:  []backend.Kind{backend.KindPR},
		Source: hubSource{hub: h},
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
