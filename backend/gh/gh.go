// Package gh is the built-in backend: it watches GitHub by polling its API,
// and reads a target's current state the same way.
//
// It registers both capabilities for every target kind, so it is the fallback
// under any backend that covers only part of the surface.
package gh

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
)

// Name is the backend name used by --backend and `gh monitor backends`.
const Name = "gh"

// Provider builds the built-in GitHub backend.
type Provider struct {
	// API returns the API client for a host. Defaults to the gh CLI client.
	API func(host string) ghcli.API

	// Base carries the run configuration the CLI resolved from flags and
	// preferences — templates, ignored bots, annotation levels, cursor
	// callbacks. Identity, Interval, and Timeout are filled per watch from
	// the Target and WatchOptions.
	Base monitor.RunOptions

	// Budget makes each watch consult the advisory GraphQL budget and stretch
	// its cadence as that budget runs low.
	Budget bool
}

// Name identifies the backend.
func (p *Provider) Name() string { return Name }

// Register adds every capability this backend provides, for every kind.
func (p *Provider) Register(r *backend.Registry) error {
	r.RegisterSource(Name, nil, backend.SourceFunc(p.watch))
	r.RegisterReader(Name, nil, backend.ReaderFunc(p.read))
	r.RegisterThreads(Name, nil, threadActor{p})
	r.RegisterReview(Name, nil, reviewActor{p})
	r.RegisterComments(Name, nil, commentActor{p})
	r.RegisterDraft(Name, nil, draftActor{p})
	r.RegisterReactions(Name, nil, reactionActor{p})
	return nil
}

// api returns the API client for a host, defaulting to the gh CLI client.
func (p *Provider) api(host string) ghcli.API {
	if p.API == nil {
		return &ghcli.Client{Host: host}
	}
	return p.API(host)
}

// service builds a monitor.Service for the target's host, wired with the
// failed-run log fetcher so a failed run's notification carries its log.
func (p *Provider) service(host string) *monitor.Service {
	svc := &monitor.Service{API: p.api(host)}
	if c, ok := svc.API.(*ghcli.Client); ok {
		svc.FailedRunLogsFn = c.FailedRunLogs
	}
	return svc
}

// runOptions merges the provider's base configuration with the per-watch
// target and options.
func (p *Provider) runOptions(t backend.Target, opts backend.WatchOptions) monitor.RunOptions {
	ro := p.Base
	ro.Identity = monitor.IdentityOf(t)
	if opts.Interval > 0 {
		ro.Interval = opts.Interval
	}
	if opts.Timeout > 0 {
		ro.Timeout = opts.Timeout
	}
	// The per-watch filters are what a caller on the far side of a backend
	// boundary can say about what it wants to hear; they override whatever
	// this process was configured with.
	if len(opts.IgnoredAuthors) > 0 {
		ro.Prefs.IgnoredBots = opts.IgnoredAuthors
	}
	if len(opts.AnnotationLevels) > 0 {
		// Parsed rather than constructed directly, so "none" keeps meaning
		// "report no annotations" instead of "report level none".
		if levels, err := monitor.ParseAnnotationLevels(strings.Join(opts.AnnotationLevels, ",")); err == nil {
			ro.AnnotationLevels = levels
		}
	}
	if opts.RepeatUnresolved {
		ro.Prefs.RetriggerComments = true
	}
	return ro
}

// watch polls the target, delivering one Update per genuinely-new change.
func (p *Provider) watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	svc := p.service(t.Host)
	ro := p.runOptions(t, opts)
	if p.Budget {
		ro.Budget = monitor.NewBudgetGuard(svc, ro.Interval)
	}

	out := make(chan backend.Update, 16)
	go func() {
		defer close(out)
		emit := func(u backend.Update) {
			select {
			case out <- u:
			case <-ctx.Done():
			}
		}
		var err error
		if opts.Once {
			err = monitor.WatchOnce(ctx, svc, ro, emit)
		} else {
			err = monitor.Watch(ctx, svc, ro, emit)
		}
		// A cancelled context is how a watch ends, not a failure. Anything
		// else is reported as a degraded update rather than swallowed: a
		// watcher that stops without saying so reads as all-clear.
		if err != nil && ctx.Err() == nil {
			emit(backend.Update{
				Target: t,
				Event: backend.Event{
					Type:            backend.EventDegraded,
					DegradedSurface: "graphql",
					DegradedMessage: err.Error(),
				},
				At: time.Now(),
			})
		}
	}()
	return out, nil
}

// read returns the target's current distilled state.
func (p *Provider) read(ctx context.Context, t backend.Target) (backend.Status, error) {
	svc := p.service(t.Host)
	id := monitor.IdentityOf(t)

	switch t.Kind {
	case backend.KindIssue:
		resp, err := svc.FetchIssue(t.Owner, t.Repo, t.Number)
		if err != nil {
			return nil, err
		}
		return monitor.SnapshotIssue(resp.Repository.Issue,
			monitor.SnapshotOptions{IgnoredBots: p.Base.Prefs.IgnoredBots}), nil

	case backend.KindRun:
		run, err := svc.FetchRun(t.Owner, t.Repo, t.RunID)
		if err != nil {
			return nil, err
		}
		return monitor.SnapshotRun(run), nil

	case backend.KindRef:
		resp, err := svc.FetchRef(t.Owner, t.Repo, t.Ref)
		if err != nil {
			return nil, err
		}
		return monitor.SnapshotRef(resp.Repository.Ref), nil

	case backend.KindCommit:
		resp, err := svc.FetchCommit(t.Owner, t.Repo, t.SHA)
		if err != nil {
			return nil, err
		}
		return monitor.SnapshotCommit(resp.Repository.Object), nil

	case backend.KindRepo:
		resp, err := svc.FetchRepo(t.Owner, t.Repo)
		if err != nil {
			return nil, err
		}
		return monitor.SnapshotRepo(resp), nil

	case backend.KindPR, "":
		resp, err := svc.Fetch(&id, t.Number)
		if err != nil {
			return nil, err
		}
		snapOpts := monitor.SnapshotOptions{
			IgnoredBots:      p.Base.Prefs.IgnoredBots,
			AnnotationLevels: p.Base.AnnotationLevels,
		}
		// A ruleset that cannot be read must not silently become "nothing is
		// required", so the error rides along on the snapshot rather than
		// failing the read.
		if rs, rsErr := svc.FetchRequiredChecks(t.Owner, t.Repo); rsErr == nil && rs != nil && rs.Error == "" {
			snapOpts.RulesetChecks = rs
		}
		return monitor.Snapshot(resp.Repository.PullRequest, snapOpts), nil

	default:
		return nil, fmt.Errorf("gh backend cannot read %s targets", t.Kind)
	}
}
