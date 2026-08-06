package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

func addMonitorFlags(cmd *cobra.Command, opts *monitorOptions) {
	cmd.Flags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	cmd.Flags().IntVar(&opts.Pull, "pr", 0, "Pull request number")
	cmd.Flags().StringVar(&opts.Ref, "ref", "", "Branch or ref to monitor (CI checks only)")
	cmd.Flags().StringVar(&opts.Commit, "commit", "", "Commit SHA to monitor (CI checks only)")
	cmd.Flags().IntVar(&opts.Issue, "issue", 0, "Issue number to monitor")
	cmd.Flags().IntVar(&opts.RunID, "run-id", 0, "GitHub Actions workflow run id to monitor (watches a single run until it completes)")
	cmd.Flags().IntVarP(&opts.Interval, "interval", "i", 60, "Polling interval in seconds (min 10)")
	cmd.Flags().IntVarP(&opts.Timeout, "timeout", "t", 0, "Maximum watch time in seconds (0 = run until merged/closed)")
	cmd.Flags().StringVar(&opts.IgnoredBots, "ignored-bots", "", "Comma-separated author logins whose general comments are ignored")
	cmd.Flags().StringVar(&opts.Events, "events", "", "Comma-separated list of event kinds to emit (suppresses all others); omit to emit everything")
	cmd.Flags().StringVar(&opts.Events, "only-events", "", "Alias for --events")
	cmd.Flags().BoolVar(&opts.Once, "once", false, "Fetch once, emit the current actionable state, and exit")
	cmd.Flags().BoolVar(&opts.Text, "text", false, "Emit the rendered message per event instead of NDJSON")
}

type monitorOptions struct {
	Repo        string
	Pull        int
	Ref         string
	Commit      string
	Issue       int
	RunID       int
	Selector    string
	Interval    int
	Timeout     int
	IgnoredBots string
	Events      string
	Once        bool
	Text        bool
}

func (o *monitorOptions) Validate() error {
	if o.Interval < 10 {
		return errors.New("--interval must be at least 10 seconds")
	}
	if o.Timeout < 0 {
		return errors.New("--timeout must be a non-negative integer")
	}

	// Count how many target kinds are specified.
	targets := 0
	if o.Selector != "" || o.Pull > 0 {
		targets++
	}
	if o.Ref != "" {
		targets++
	}
	if o.Commit != "" {
		targets++
	}
	if o.Issue > 0 {
		targets++
	}
	if o.RunID > 0 {
		targets++
	}
	// Repo-only (--repo without any other target) is its own target kind.
	repoOnly := o.Repo != "" && o.Selector == "" && o.Pull == 0 && o.Ref == "" && o.Commit == "" && o.Issue == 0 && o.RunID == 0
	if repoOnly {
		// Repo monitoring: valid on its own; no conflict with other targets.
		return nil
	}
	if targets > 1 {
		return errors.New("--ref, --commit, --issue, --run-id, and a PR selector are mutually exclusive")
	}
	if targets == 0 {
		return errors.New("pull request number or URL is required")
	}

	return nil
}

func runMonitor(cmd *cobra.Command, opts *monitorOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	inferRepo(&opts.Repo)

	var identity resolver.Identity
	var err error

	if opts.Ref != "" {
		identity, err = resolver.ResolveRef(opts.Ref, opts.Repo, os.Getenv("GH_HOST"))
	} else if opts.Commit != "" {
		identity, err = resolver.ResolveCommit(opts.Commit, opts.Repo, os.Getenv("GH_HOST"))
	} else if opts.Issue > 0 {
		identity, err = resolver.ResolveIssue(opts.Issue, opts.Repo, os.Getenv("GH_HOST"))
	} else if opts.RunID > 0 {
		identity, err = resolver.ResolveRun(opts.RunID, opts.Repo, os.Getenv("GH_HOST"))
	} else if opts.Repo != "" && opts.Selector == "" && opts.Ref == "" && opts.Commit == "" && opts.Issue == 0 && opts.RunID == 0 {
		identity, err = resolver.ResolveRepo(opts.Repo, os.Getenv("GH_HOST"))
	} else {
		inferPR(opts.Selector, &opts.Pull)
		selector, normErr := resolver.NormalizeSelector(opts.Selector, opts.Pull)
		if normErr != nil {
			return normErr
		}
		identity, err = resolver.Resolve(selector, opts.Repo, os.Getenv("GH_HOST"))
	}
	if err != nil {
		return err
	}

	p, err := prefs.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh-monitor: using default templates (%v)\n", err)
	}
	for _, bot := range strings.Split(opts.IgnoredBots, ",") {
		if b := strings.TrimSpace(bot); b != "" {
			p.IgnoredBots = append(p.IgnoredBots, b)
		}
	}

	svc := &monitor.Service{API: apiClientFactory(identity.Host)}
	// Wire the failed-run log fetcher (issue #19) so run-completed
	// notifications for failed runs embed a `gh run view --log-failed` snippet.
	if c, ok := svc.API.(*ghcli.Client); ok {
		svc.FailedRunLogsFn = c.FailedRunLogs
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
		case <-ctx.Done():
		}
	}()

	runOpts := monitor.RunOptions{
		Identity: identity,
		Prefs:    p,
		Interval: time.Duration(opts.Interval) * time.Second,
		Timeout:  time.Duration(opts.Timeout) * time.Second,
	}

	// --events / --only-events: a per-event-kind allowlist. When set, only the
	// listed kinds are emitted; everything else is suppressed. An unknown kind
	// is rejected loudly so a typo doesn't silently mute what the caller wanted.
	if strings.TrimSpace(opts.Events) != "" {
		filter, err := monitor.ParseEventFilter(opts.Events)
		if err != nil {
			return err
		}
		runOpts.EventFilter = filter
	}

	emit := func(n monitor.Notification) {
		if opts.Text {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, monitor.LinkifyText(n))
			if n.Detail != "" {
				for _, line := range strings.Split(n.Detail, "\n") {
					fmt.Fprintf(out, "  %s\n", line)
				}
			}
			return
		}
		if err := encodeJSON(cmd, n); err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: %v\n", err)
		}
	}

	if opts.Once {
		return monitor.Once(ctx, svc, runOpts, emit)
	}

	if err := monitor.Run(ctx, svc, runOpts, emit); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil // clean shutdown on signal
		}
		return err
	}
	return nil
}
