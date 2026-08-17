package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/reactions"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

func newReactCommand() *cobra.Command {
	var reactionType string
	bo := &backendOptions{}

	cmd := &cobra.Command{
		Use:   "react <node-id>",
		Short: "Add a reaction to any reactable GitHub node",
		Long: `Add a reaction to any reactable GitHub node (review comment, issue comment, PR review body, etc.).

The node-id is the GraphQL node ID of the target object. Use --type to specify the reaction.

Valid reaction types: ` + strings.Join(reactions.ValidReactionNames(), ", "),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeID := strings.TrimSpace(args[0])
			if nodeID == "" {
				return fmt.Errorf("node-id is required")
			}

			reactionType = strings.TrimSpace(reactionType)
			if reactionType == "" {
				return fmt.Errorf("--type is required")
			}

			if err := reactions.Validate(reactionType); err != nil {
				return fmt.Errorf("--type: %w", err)
			}

			// A reaction targets a node id directly, so the only thing the
			// target carries is the host to reach it on.
			target := backend.Target{Kind: backend.KindPR, Host: resolver.SanitizeHost(os.Getenv("GH_HOST"))}
			reg, err := actorRegistry(cmd.Context(), bo)
			if err != nil {
				return err
			}
			actor, _, err := reg.ReactionsFor(target)
			if err != nil {
				return err
			}
			if err := actor.React(cmd.Context(), target, nodeID, reactionType); err != nil {
				return err
			}

			result := map[string]string{
				"node_id":  nodeID,
				"reaction": reactionType,
				"status":   "added",
			}
			return encodeJSON(cmd, result)
		},
	}

	cmd.Flags().StringVar(&reactionType, "type", "", "Reaction type (required): "+strings.Join(reactions.ValidReactionNames(), ", "))
	addBackendFlags(cmd, bo)
	_ = cmd.MarkFlagRequired("type")

	return cmd
}
