package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
)

func newReviewViewCommand(bo *backendOptions) *cobra.Command {
	opts := &reviewViewOptions{}

	cmd := &cobra.Command{
		Use:   "view [<number> | <url>]",
		Short: "View a structured review summary (GraphQL)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.Selector = args[0]
			}
			return runReviewView(cmd, bo, opts)
		},
	}

	addTargetFlags(cmd.Flags(), &opts.targetSelector)
	cmd.Flags().StringVar(&opts.Reviewer, "reviewer", "", "Filter to a specific reviewer (login)")
	cmd.Flags().StringSliceVar(&opts.States, "states", nil, "Comma-separated review states (APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING)")
	cmd.Flags().BoolVar(&opts.Unresolved, "unresolved", false, "Only include unresolved threads")
	cmd.Flags().BoolVar(&opts.NotOutdated, "not_outdated", false, "Exclude outdated threads")
	cmd.Flags().IntVar(&opts.TailReplies, "tail", 0, "Limit to the last N replies per thread (0 = all)")
	cmd.Flags().BoolVar(&opts.IncludeCommentNodeID, "include-comment-node-id", false, "Include comment_node_id fields for parent comments and replies")
	cmd.Flags().StringVar(&opts.Author, "author", "", "Filter threads to those containing a comment by this author login (case-insensitive)")
	cmd.Flags().BoolVar(&opts.IncludeResolved, "include-resolved", false, "Include resolved threads (overrides --unresolved)")
	addBackendFlags(cmd, bo)

	return cmd
}

type reviewViewOptions struct {
	targetSelector

	Reviewer             string
	States               []string
	Unresolved           bool
	NotOutdated          bool
	TailReplies          int
	IncludeCommentNodeID bool
	Author               string
	IncludeResolved      bool
}

func runReviewView(cmd *cobra.Command, bo *backendOptions, opts *reviewViewOptions) error {
	if opts.TailReplies < 0 {
		return fmt.Errorf("invalid --tail value %d: must be non-negative", opts.TailReplies)
	}

	states, statesProvided, err := parseStateFilters(opts.States)
	if err != nil {
		return err
	}

	_, target, err := resolveTarget(&opts.targetSelector)
	if err != nil {
		return err
	}

	reg, err := actorRegistry(cmd.Context(), bo)
	if err != nil {
		return err
	}
	actor, _, err := reg.ReportFor(target)
	if err != nil {
		return err
	}

	output, err := actor.ViewReport(cmd.Context(), target, backend.ReportOptions{
		Reviewer:             strings.TrimSpace(opts.Reviewer),
		States:               states,
		StatesProvided:       statesProvided,
		RequireUnresolved:    opts.Unresolved,
		RequireNotOutdated:   opts.NotOutdated,
		TailReplies:          opts.TailReplies,
		IncludeCommentNodeID: opts.IncludeCommentNodeID,
		Author:               strings.TrimSpace(opts.Author),
		IncludeResolved:      opts.IncludeResolved,
	})
	if err != nil {
		return err
	}

	return encodeJSON(cmd, output)
}

func parseStateFilters(raw []string) ([]backend.ReportState, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}

	valid := map[string]backend.ReportState{
		"APPROVED":          backend.ReportStateApproved,
		"CHANGES_REQUESTED": backend.ReportStateChangesRequested,
		"COMMENTED":         backend.ReportStateCommented,
		"DISMISSED":         backend.ReportStateDismissed,
		"PENDING":           backend.ReportStatePending,
	}
	allowed := make([]string, 0, len(valid))
	for key := range valid {
		allowed = append(allowed, key)
	}
	sort.Strings(allowed)

	temp := make(map[backend.ReportState]struct{})
	states := make([]backend.ReportState, 0, len(raw))
	for _, entry := range raw {
		parts := strings.Split(entry, ",")
		for _, part := range parts {
			candidate := strings.ToUpper(strings.TrimSpace(part))
			if candidate == "" {
				continue
			}
			state, ok := valid[candidate]
			if !ok {
				return nil, false, fmt.Errorf("invalid review state %q (allowed: %s)", part, strings.Join(allowed, ", "))
			}
			if _, seen := temp[state]; seen {
				continue
			}
			temp[state] = struct{}{}
			states = append(states, state)
		}
	}

	if len(states) == 0 {
		return nil, false, fmt.Errorf("no valid states provided")
	}

	return states, true, nil
}
