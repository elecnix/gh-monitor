package cmd

import (
	"bufio"
	"context"
	"encoding/json"
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

	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

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
dir for the socket path. Send SIGTERM/SIGINT to stop it cleanly.`,
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

// serveClient handles one client connection: read its Subscribe request, register
// a hub subscription, and stream notifications back as newline-delimited JSON
// until the client disconnects or the PR reaches a terminal state.
func serveClient(ctx context.Context, h *hub.Hub, conn net.Conn) {
	// Read the subscribe request, honouring daemon shutdown.
	req, err := ipc.ReadSubscribeContext(ctx, conn)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gh-monitor: read subscribe: %v\n", err)
		return
	}
	if req.Target != "pr" {
		_, _ = fmt.Fprintf(os.Stderr, "gh-monitor: unsupported target %q\n", req.Target)
		return
	}

	interval := time.Duration(req.Interval) * time.Second
	if interval < 10*time.Second {
		interval = 60 * time.Second
	}
	opts := monitor.RunOptions{
		Identity: req.Identity,
		Prefs:    req.Prefs,
		Interval: interval,
		Timeout:  time.Duration(req.Timeout) * time.Second,
		Now:      time.Now,
	}

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, unsub := h.SubscribePR(subCtx, opts)
	defer unsub()

	// Stop streaming when the client disconnects.
	go func() {
		_, _ = io.Copy(io.Discard, conn) // block until EOF/error
		cancel()
	}()

	w := bufio.NewWriter(conn)
	defer func() { _ = w.Flush() }()
	for n := range ch {
		if err := ipc.WriteNotification(w, n); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// streamFromDaemon is the client side: connect to a running daemon, send the
// subscribe request, and pipe newline-delimited JSON notifications to out
// until the daemon closes the stream or ctx is cancelled. It returns
// os.ErrNotExist (via ipc.IsAbsent) when no daemon socket is present, so the
// caller can fall back to in-process polling.
func streamFromDaemon(ctx context.Context, socket string, req ipc.Subscribe, out io.Writer) error {
	conn, err := ipc.Dial(socket)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// Close the connection when ctx is cancelled so a blocked read (e.g. an
	// open PR that produces no new events) unblocks instead of ignoring the
	// cancellation — the same way the in-process loop honours SIGINT.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	if err := ipc.SendSubscribe(conn, req); err != nil {
		return fmt.Errorf("send subscribe: %w", err)
	}

	r := bufio.NewReader(conn)
	dec := newNDJSONDecoder(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := dec.Decode()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := writeNDJSON(out, n); err != nil {
			return err
		}
	}
}

// ndJSONDecoder reads one JSON object per line.
type ndJSONDecoder struct {
	r *bufio.Reader
}

func newNDJSONDecoder(r *bufio.Reader) *ndJSONDecoder { return &ndJSONDecoder{r: r} }

func (d *ndJSONDecoder) Decode() (monitor.Notification, error) {
	line, err := d.r.ReadBytes('\n')
	if err != nil {
		return monitor.Notification{}, err
	}
	var n monitor.Notification
	if err := json.Unmarshal(line, &n); err != nil {
		return n, err
	}
	return n, nil
}

func writeNDJSON(w io.Writer, n monitor.Notification) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
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
