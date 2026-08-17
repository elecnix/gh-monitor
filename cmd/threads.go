package cmd

import (
	"errors"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/elecnix/gh-monitor/internal/threads"
)

func newThreadsCommand() *cobra.Command {
	bo := &backendOptions{}
	cmd := &cobra.Command{
		Use:   "threads",
		Short: "Inspect and resolve pull request review threads",
	}
	addPersistentBackendFlags(cmd, bo)

	cmd.AddCommand(newThreadsListCommand(bo))
	cmd.AddCommand(newThreadsResolveCommand(bo))
	cmd.AddCommand(newThreadsUnresolveCommand(bo))
	cmd.AddCommand(newThreadsViewCommand(bo))

	return cmd
}

// threads view [<thread-id> ...]
func newThreadsViewCommand(bo *backendOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "view <thread-id> [<thread-id> ...]",
		Short: "View one or more review threads with comments",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runThreadsView(cmd, bo, args)
		},
	}
	return cmd
}

func runThreadsView(cmd *cobra.Command, bo *backendOptions, threadIDs []string) error {
	// Viewing threads by ID needs no pull request context, only a host.
	target := backend.Target{Kind: backend.KindPR, Host: os.Getenv("GH_HOST")}
	actor, err := threadActorFor(cmd, bo, target)
	if err != nil {
		return err
	}
	threadsWithComments, err := actor.ViewThreads(cmd.Context(), target, threadIDs)
	if err != nil {
		return err
	}
	return encodeJSON(cmd, threadsWithComments)
}

// threadActorFor resolves the review-thread capability for a target.
func threadActorFor(cmd *cobra.Command, bo *backendOptions, target backend.Target) (backend.ThreadActor, error) {
	reg, err := actorRegistry(cmd.Context(), bo)
	if err != nil {
		return nil, err
	}
	actor, _, err := reg.ThreadsFor(target)
	return actor, err
}

func newThreadsListCommand(bo *backendOptions) *cobra.Command {
	opts := &threadsListOptions{}

	cmd := &cobra.Command{
		Use:   "list [<number> | <url>]",
		Short: "List review threads for a pull request",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.Selector = args[0]
			}
			return runThreadsList(cmd, bo, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.UnresolvedOnly, "unresolved", false, "Filter to unresolved threads only")
	cmd.Flags().BoolVar(&opts.MineOnly, "mine", false, "Show only threads involving or resolvable by the viewer")
	cmd.PersistentFlags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	cmd.PersistentFlags().IntVar(&opts.Pull, "pr", 0, "Pull request number")

	return cmd
}

type threadsListOptions struct {
	Repo           string
	Pull           int
	Selector       string
	UnresolvedOnly bool
	MineOnly       bool
}

func runThreadsList(cmd *cobra.Command, bo *backendOptions, opts *threadsListOptions) error {
	inferPR(opts.Selector, &opts.Pull)
	selector, err := resolver.NormalizeSelector(opts.Selector, opts.Pull)
	if err != nil {
		return err
	}

	inferRepo(&opts.Repo)
	hostEnv := os.Getenv("GH_HOST")
	identity, err := resolver.Resolve(selector, opts.Repo, hostEnv)
	if err != nil {
		return err
	}

	target := monitor.TargetOf(identity)
	actor, err := threadActorFor(cmd, bo, target)
	if err != nil {
		return err
	}
	payload, err := actor.ListThreads(cmd.Context(), target, threads.ListOptions{
		OnlyUnresolved: opts.UnresolvedOnly,
		MineOnly:       opts.MineOnly,
	})
	if err != nil {
		return err
	}

	return encodeJSON(cmd, payload)
}

func newThreadsResolveCommand(bo *backendOptions) *cobra.Command {
	return newThreadsMutationCommand(bo, true)
}

func newThreadsUnresolveCommand(bo *backendOptions) *cobra.Command {
	return newThreadsMutationCommand(bo, false)
}

func newThreadsMutationCommand(bo *backendOptions, resolve bool) *cobra.Command {
	opts := &threadsMutationOptions{}

	use := "resolve"
	short := "Resolve a review thread"
	if !resolve {
		use = "unresolve"
		short = "Reopen a review thread"
	}

	cmd := &cobra.Command{
		Use:   use + " [<number> | <url>]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.Selector = args[0]
			}
			if err := opts.Validate(); err != nil {
				return err
			}
			if resolve {
				return runThreadsResolve(cmd, bo, opts)
			}
			return runThreadsUnresolve(cmd, bo, opts)
		},
	}

	cmd.Flags().StringVar(&opts.ThreadID, "thread-id", "", "GraphQL node ID for the review thread")
	cmd.PersistentFlags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	cmd.PersistentFlags().IntVar(&opts.Pull, "pr", 0, "Pull request number")

	return cmd
}

type threadsMutationOptions struct {
	Repo     string
	Pull     int
	Selector string
	ThreadID string
}

func (o *threadsMutationOptions) Validate() error {
	if strings.TrimSpace(o.ThreadID) == "" {
		return errors.New("--thread-id is required")
	}
	return nil
}

func runThreadsResolve(cmd *cobra.Command, bo *backendOptions, opts *threadsMutationOptions) error {
	return runThreadsMutation(cmd, bo, opts, true)
}

func runThreadsUnresolve(cmd *cobra.Command, bo *backendOptions, opts *threadsMutationOptions) error {
	return runThreadsMutation(cmd, bo, opts, false)
}

func runThreadsMutation(cmd *cobra.Command, bo *backendOptions, opts *threadsMutationOptions, resolve bool) error {
	inferPR(opts.Selector, &opts.Pull)
	selector, err := resolver.NormalizeSelector(opts.Selector, opts.Pull)
	if err != nil {
		return err
	}

	inferRepo(&opts.Repo)
	hostEnv := os.Getenv("GH_HOST")
	identity, err := resolver.Resolve(selector, opts.Repo, hostEnv)
	if err != nil {
		return err
	}

	target := monitor.TargetOf(identity)
	actor, err := threadActorFor(cmd, bo, target)
	if err != nil {
		return err
	}
	action := threads.ActionOptions{ThreadID: strings.TrimSpace(opts.ThreadID)}

	var result threads.ActionResult
	if resolve {
		result, err = actor.ResolveThread(cmd.Context(), target, action)
	} else {
		result, err = actor.UnresolveThread(cmd.Context(), target, action)
	}
	if err != nil {
		return err
	}
	return encodeJSON(cmd, result)
}
