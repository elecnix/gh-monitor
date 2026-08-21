// Package gh is the built-in backend: it reads GitHub's API, and — since the
// shared-poller daemon became the single watch code path (issue #76) — it is
// also where the daemon's per-kind fetch implementation lives. `gh monitor
// daemon` wraps gh.Fetch in its hub; watching without a running daemon is a
// hard error, not a silent in-process fallback.
//
// It registers the reader capability and the mutation actors for every target
// kind. It deliberately registers no Source: watching goes through the hub,
// which shares one fetch across every watcher.
package gh

import (
	"context"
	"fmt"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// Name is the backend name used by --backend and `gh monitor backends`.
const Name = "gh"

// Provider builds the built-in GitHub backend.
type Provider struct {
	// API returns the API client for a host. Defaults to the gh CLI client.
	API func(host string) ghcli.API

	// Base carries the read configuration the CLI resolved from flags and
	// preferences — ignored bots and annotation levels. Identity is filled
	// per read from the Target.
	Base monitor.RunOptions
}

// Name identifies the backend.
func (p *Provider) Name() string { return Name }

// Register adds every capability this backend provides, for every kind.
// The Source it registers serves one-shot reads only (a single fetch through
// an in-process hub — the same hub.Once the daemon serves); continuous
// watching goes through the shared-poller daemon.
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

// ---------------------------------------------------------------------------
// The daemon's fetch implementation
// ---------------------------------------------------------------------------

// service builds a monitor.Service for the target's host, wired with the
// failed-run log fetcher so a failed run's notification carries its log.
func service(api func(host string) ghcli.API, host string) *monitor.Service {
	var client ghcli.API
	if api == nil {
		client = &ghcli.Client{Host: host}
	} else {
		client = api(host)
	}
	svc := &monitor.Service{API: client}
	if c, ok := svc.API.(*ghcli.Client); ok {
		svc.FailedRunLogsFn = c.FailedRunLogs
	}
	return svc
}

// Fetch returns the per-kind fetch function the daemon's hub polls with. Each
// call goes through the real gh CLI client — at the poller's current query
// tier for the kinds whose queries have tiers (pr, ref, commit); untiered for
// the rest. The hub fans each single result out to every subscribed client.
func Fetch(api func(host string) ghcli.API) hub.FetchFunc {
	return func(ctx context.Context, id resolver.Identity, tier monitor.QueryTier) (any, error) {
		svc := service(api, id.Host)
		switch id.Target {
		case "ref":
			return svc.FetchRefWithTier(id.Owner, id.Repo, id.Ref, tier)
		case "commit":
			return svc.FetchCommitWithTier(id.Owner, id.Repo, id.CommitSHA, tier)
		case "issue":
			return svc.FetchIssue(id.Owner, id.Repo, id.Number)
		case "run":
			return svc.FetchRun(id.Owner, id.Repo, id.RunID)
		case "repo":
			return svc.FetchRepo(id.Owner, id.Repo)
		default:
			resp, err := svc.FetchWithTier(&id, id.Number, tier)
			if err != nil {
				return nil, err
			}
			return resp.Repository.PullRequest, nil
		}
	}
}

// Ruleset returns the ruleset function the daemon's hub calls once per PR
// poller to read the branch ruleset and determine required status checks.
func Ruleset(api func(host string) ghcli.API) hub.RulesetFunc {
	return func(owner, repo string) (*monitor.RulesetChecks, error) {
		return service(api, "").FetchRequiredChecks(owner, repo)
	}
}

// FailedRunLogs returns the failed-run log fetcher the daemon's hub injects,
// so run-target notifications carry their log snippet exactly as the
// in-process loops' did.
func FailedRunLogs(api func(host string) ghcli.API) hub.FailedRunLogFetcher {
	return func(owner, repo string, runID int) (string, error) {
		return service(api, "").FailedRunLogs(owner, repo, runID)
	}
}

// watch serves one-shot reads (WatchOptions.Once): a single fetch + emit
// through an in-process hub, so `--once` works without a daemon. Continuous
// watching requires the shared-poller daemon — a non-once Watch is a hard
// error pointing at it, never a silent in-process poll loop.
func (p *Provider) watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	if !opts.Once {
		return nil, fmt.Errorf("watching requires the shared-poller daemon; start one with 'gh monitor daemon'")
	}
	h := hub.New(Fetch(p.API), Ruleset(p.API), 0, nil,
		hub.WithFailedRunLogFetcher(FailedRunLogs(p.API)))
	return h.Once(ctx, t, opts), nil
}

// read returns the target's current distilled state.
func (p *Provider) read(ctx context.Context, t backend.Target) (backend.Status, error) {
	svc := service(p.API, t.Host)
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
