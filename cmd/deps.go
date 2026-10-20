package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

var apiClientFactory = func(host string) ghcli.API {
	return &ghcli.Client{Host: host}
}

// inferRepo fills in repo from the current git context when not explicitly provided.
func inferRepo(repo *string) {
	if *repo == "" {
		if r, err := ghcli.CurrentRepo(); err == nil {
			*repo = r
		}
	}
}

// inferPR fills in the PR number from the current branch when not explicitly provided.
func inferPR(selector string, pr *int) {
	if selector == "" && *pr == 0 {
		if n, err := ghcli.CurrentPR(); err == nil {
			*pr = n
		}
	}
}

// targetSelector is what every pull-request-scoped verb takes: the positional
// selector, the --repo flag, and the --pr flag. Each command declares its own
// flags into its own instance of one of these, but resolution happens here so
// every verb infers the git context, reads GH_HOST, and derives its backend
// target the same way.
type targetSelector struct {
	Selector string
	Repo     string
	Pull     int
}

// addTargetFlags declares the --repo and --pr flags into a selector. Commands
// with a parent that also declares them bind to the same instance, so the
// flag can be given at either level and means the same thing.
func addTargetFlags(fs *pflag.FlagSet, sel *targetSelector) {
	fs.StringVarP(&sel.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	fs.IntVar(&sel.Pull, "pr", 0, "Pull request number")
}

// addPersistentTargetFlags puts --repo and --pr on a command group, so every
// subcommand under it accepts them.
func addPersistentTargetFlags(cmd *cobra.Command, sel *targetSelector) {
	addTargetFlags(cmd.PersistentFlags(), sel)
}

// resolveTarget turns a selector, --repo and --pr into a concrete identity and
// the backend target for it. This is the one place a verb decides what it is
// talking to: inferring the repo and pull request from the git context when
// they were not given, and sanitizing GH_HOST. Two verbs that did this
// differently ended up disagreeing about whether a GH_HOST like
// "https://ghe.example.com" was a host or a URL.
func resolveTarget(sel *targetSelector) (resolver.Identity, backend.Target, error) {
	inferRepo(&sel.Repo)
	inferPR(sel.Selector, &sel.Pull)

	selector, err := resolver.NormalizeSelector(sel.Selector, sel.Pull)
	if err != nil {
		return resolver.Identity{}, backend.Target{}, err
	}

	identity, err := resolver.Resolve(selector, sel.Repo, os.Getenv("GH_HOST"))
	if err != nil {
		return resolver.Identity{}, backend.Target{}, err
	}
	return identity, monitor.TargetOf(identity), nil
}
