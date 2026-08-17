package cmd

import (
	"errors"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/draft"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

func newDraftCommand() *cobra.Command {
	bo := &backendOptions{}
	cmd := &cobra.Command{
		Use:   "draft",
		Short: "Manage pull request draft status",
	}
	addPersistentBackendFlags(cmd, bo)

	cmd.AddCommand(newDraftMarkCommand(bo))
	cmd.AddCommand(newDraftReadyCommand(bo))
	cmd.AddCommand(newDraftStatusCommand(bo))
	cmd.AddCommand(newDraftListCommand(bo))

	return cmd
}

// draftActorFor resolves the draft capability for a target.
func draftActorFor(cmd *cobra.Command, bo *backendOptions, target backend.Target) (backend.DraftActor, error) {
	reg, err := actorRegistry(cmd.Context(), bo)
	if err != nil {
		return nil, err
	}
	actor, _, err := reg.DraftFor(target)
	return actor, err
}

// draft mark [<number>]
func newDraftMarkCommand(bo *backendOptions) *cobra.Command {
	opts := &draftActionOptions{}

	cmd := &cobra.Command{
		Use:   "mark [<number>]",
		Short: "Mark a pull request as draft",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				if prNum, err := strconv.Atoi(args[0]); err == nil {
					opts.PRNumber = prNum
				} else {
					opts.Selector = args[0]
				}
			}
			return runDraftMark(cmd, bo, opts)
		},
	}

	cmd.PersistentFlags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	cmd.PersistentFlags().IntVar(&opts.Pull, "pr", 0, "Pull request number")

	return cmd
}

// draft ready [<number>]
func newDraftReadyCommand(bo *backendOptions) *cobra.Command {
	opts := &draftActionOptions{}

	cmd := &cobra.Command{
		Use:   "ready [<number>]",
		Short: "Mark a pull request as ready for review",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				if prNum, err := strconv.Atoi(args[0]); err == nil {
					opts.PRNumber = prNum
				} else {
					opts.Selector = args[0]
				}
			}
			return runDraftReady(cmd, bo, opts)
		},
	}

	cmd.PersistentFlags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	cmd.PersistentFlags().IntVar(&opts.Pull, "pr", 0, "Pull request number")

	return cmd
}

// draft status [<number>]
func newDraftStatusCommand(bo *backendOptions) *cobra.Command {
	opts := &draftActionOptions{}

	cmd := &cobra.Command{
		Use:   "status [<number>]",
		Short: "Check if a pull request is a draft",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				if prNum, err := strconv.Atoi(args[0]); err == nil {
					opts.PRNumber = prNum
				} else {
					opts.Selector = args[0]
				}
			}
			return runDraftStatus(cmd, bo, opts)
		},
	}

	cmd.PersistentFlags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	cmd.PersistentFlags().IntVar(&opts.Pull, "pr", 0, "Pull request number")

	return cmd
}

// draft list
func newDraftListCommand(bo *backendOptions) *cobra.Command {
	opts := &draftListOptions{}

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all draft pull requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDraftList(cmd, bo, opts)
		},
	}

	cmd.PersistentFlags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")

	return cmd
}

type draftActionOptions struct {
	Repo     string
	Pull     int
	Selector string
	PRNumber int
}

type draftListOptions struct {
	Repo string
}

func runDraftMark(cmd *cobra.Command, bo *backendOptions, opts *draftActionOptions) error {
	return runDraftAction(cmd, bo, opts, true)
}

func runDraftReady(cmd *cobra.Command, bo *backendOptions, opts *draftActionOptions) error {
	return runDraftAction(cmd, bo, opts, false)
}

func runDraftAction(cmd *cobra.Command, bo *backendOptions, opts *draftActionOptions, markAsDraft bool) error {
	var err error
	var selector string

	if opts.PRNumber != 0 {
		// Use the provided PR number directly
		selector = strconv.Itoa(opts.PRNumber)
	} else if opts.Selector != "" {
		// Use the provided selector
		selector = opts.Selector
	} else if opts.Pull != 0 {
		// Use the --pr flag
		selector = strconv.Itoa(opts.Pull)
	} else {
		return errors.New("pull request number is required")
	}

	inferPR(selector, &opts.Pull)
	normalizedSelector, err := resolver.NormalizeSelector(selector, opts.Pull)
	if err != nil {
		return err
	}

	inferRepo(&opts.Repo)
	hostEnv := os.Getenv("GH_HOST")
	identity, err := resolver.Resolve(normalizedSelector, opts.Repo, hostEnv)
	if err != nil {
		return err
	}

	target := monitor.TargetOf(identity)
	actor, err := draftActorFor(cmd, bo, target)
	if err != nil {
		return err
	}
	result, err := actor.SetDraft(cmd.Context(), target, draft.ActionOptions{PRNumber: identity.Number}, markAsDraft)
	if err != nil {
		return err
	}

	return encodeJSON(cmd, result)
}

func runDraftStatus(cmd *cobra.Command, bo *backendOptions, opts *draftActionOptions) error {
	var err error
	var selector string

	if opts.PRNumber != 0 {
		selector = strconv.Itoa(opts.PRNumber)
	} else if opts.Selector != "" {
		selector = opts.Selector
	} else if opts.Pull != 0 {
		selector = strconv.Itoa(opts.Pull)
	} else {
		return errors.New("pull request number is required")
	}

	inferPR(selector, &opts.Pull)
	normalizedSelector, err := resolver.NormalizeSelector(selector, opts.Pull)
	if err != nil {
		return err
	}

	inferRepo(&opts.Repo)
	hostEnv := os.Getenv("GH_HOST")
	identity, err := resolver.Resolve(normalizedSelector, opts.Repo, hostEnv)
	if err != nil {
		return err
	}

	target := monitor.TargetOf(identity)
	actor, err := draftActorFor(cmd, bo, target)
	if err != nil {
		return err
	}
	result, err := actor.DraftStatus(cmd.Context(), target, draft.ActionOptions{PRNumber: identity.Number})
	if err != nil {
		return err
	}

	return encodeJSON(cmd, result)
}

func runDraftList(cmd *cobra.Command, bo *backendOptions, opts *draftListOptions) error {
	inferRepo(&opts.Repo)

	// Use a dummy selector for list operations
	selector := "1"
	normalizedSelector, err := resolver.NormalizeSelector(selector, 1)
	if err != nil {
		return err
	}

	hostEnv := os.Getenv("GH_HOST")
	identity, err := resolver.Resolve(normalizedSelector, opts.Repo, hostEnv)
	if err != nil {
		return err
	}

	target := monitor.TargetOf(identity)
	actor, err := draftActorFor(cmd, bo, target)
	if err != nil {
		return err
	}
	result, err := actor.ListDrafts(cmd.Context(), target)
	if err != nil {
		return err
	}

	return encodeJSON(cmd, result)
}
