package cmd

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/internal/comments"
)

type commentsOptions struct {
	targetSelector
	Backend backendOptions
}

func newCommentsCommand() *cobra.Command {
	opts := &commentsOptions{}

	cmd := &cobra.Command{
		Use:   "comments",
		Short: "Reply to pull request review threads",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Help(); err != nil {
				return err
			}
			return errors.New("use 'gh monitor comments reply' to respond to a review thread; run 'gh monitor review view' to locate thread IDs")
		},
	}

	addPersistentTargetFlags(cmd, &opts.targetSelector)
	addPersistentBackendFlags(cmd, &opts.Backend)

	cmd.AddCommand(newCommentsReplyCommand(opts))

	return cmd
}

func newCommentsReplyCommand(parent *commentsOptions) *cobra.Command {
	opts := &commentsReplyOptions{}

	cmd := &cobra.Command{
		Use:   "reply [<number> | <url>]",
		Short: "Reply to a pull request review thread",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				parent.Selector = args[0]
			}
			return runCommentsReply(cmd, parent, opts)
		},
	}

	cmd.Flags().StringVar(&opts.ThreadID, "thread-id", "", "Review thread identifier to reply to")
	cmd.Flags().StringVar(&opts.ReviewID, "review-id", "", "GraphQL review identifier when replying inside a pending review")
	cmd.Flags().StringVar(&opts.Body, "body", "", "Reply text")
	cmd.Flags().StringVar(&opts.BodyFile, "body-file", "", "Read reply text from file (use \"-\" for stdin)")
	_ = cmd.MarkFlagRequired("thread-id")
	cmd.MarkFlagsMutuallyExclusive("body", "body-file")

	return cmd
}

type commentsReplyOptions struct {
	ThreadID string
	ReviewID string
	Body     string
	BodyFile string
}

func runCommentsReply(cmd *cobra.Command, parent *commentsOptions, opts *commentsReplyOptions) error {
	body, err := resolveBody(opts.Body, opts.BodyFile)
	if err != nil {
		return err
	}
	if body == "" {
		return errors.New("--body or --body-file is required")
	}
	opts.Body = body

	// The selector, --repo and --pr are declared once on the parent, so they
	// mean the same thing whichever level they are given at.
	_, target, err := resolveTarget(&parent.targetSelector)
	if err != nil {
		return err
	}

	reg, err := actorRegistry(cmd.Context(), &parent.Backend)
	if err != nil {
		return err
	}
	actor, _, err := reg.CommentsFor(target)
	if err != nil {
		return err
	}

	reply, err := actor.ReplyToThread(cmd.Context(), target, comments.ReplyOptions{
		ThreadID: opts.ThreadID,
		ReviewID: opts.ReviewID,
		Body:     opts.Body,
	})
	if err != nil {
		return err
	}
	if reply.CommentNodeID == "" {
		return errors.New("reply response missing comment node id")
	}
	return encodeJSON(cmd, map[string]string{"comment_node_id": reply.CommentNodeID})
}
