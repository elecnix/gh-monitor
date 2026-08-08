package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/internal/cursor"
	"github.com/elecnix/gh-monitor/internal/prefs"
)

// newInstancesCommand builds `gh monitor instances`: list and reset named
// monitor instance cursors (issue #32).
func newInstancesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "instances",
		Short: "Manage named monitor instance cursors",
		Long: `List and reset named monitor instance cursors.

Named instances (--instance) keep an independent cursor so a restart resumes
from where it left off rather than replaying the entire backlog. Each instance
owns its own cursor — advancing one never affects another.

  gh monitor instances list          Show every stored cursor.
  gh monitor instances reset <name>  Delete a cursor so the next run replays
                                     from the beginning.
  gh monitor instances reset --all   Delete every cursor.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(newInstancesListCommand())
	cmd.AddCommand(newInstancesResetCommand())
	return cmd
}

func newInstancesListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all named instance cursors",
		Long:  "Show every stored cursor: instance name, repository, cursor position, and last-seen time.",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := cursorStore()
			if err != nil {
				return err
			}

			cursors, err := store.List()
			if err != nil {
				return fmt.Errorf("list cursors: %w", err)
			}
			if len(cursors) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No named instance cursors found.")
				return nil
			}

			for _, c := range cursors {
				lastSeen := c.LastSeen.Format(time.RFC3339)
				if c.LastSeen.IsZero() {
					lastSeen = "-"
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-24s %-32s %-24s %s\n",
					c.Instance,
					fmt.Sprintf("%s/%s", c.Owner, c.Repo),
					c.Position,
					lastSeen,
				)
			}
			return nil
		},
	}
}

func newInstancesResetCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "reset [<name>]",
		Short: "Reset a named instance cursor so the next run replays from the beginning",
		Long: `Delete the cursor for a named instance. The next run with --instance <name>
will replay the full backlog as if it were the first invocation.

With --all, deletes every stored cursor.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := cursorStore()
			if err != nil {
				return err
			}

			if all {
				cursors, err := store.List()
				if err != nil {
					return fmt.Errorf("list cursors: %w", err)
				}
				for _, c := range cursors {
					if err := store.Delete(c.Instance); err != nil {
						return fmt.Errorf("delete cursor %q: %w", c.Instance, err)
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Reset: %s\n", c.Instance)
				}
				if len(cursors) == 0 {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No cursors to reset.")
				}
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("specify an instance name to reset, or use --all")
			}
			name := args[0]
			if err := store.Delete(name); err != nil {
				return fmt.Errorf("delete cursor %q: %w", name, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Reset: %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Reset every stored cursor")
	return cmd
}

// cursorStore creates a DiskStore under the gh-monitor config directory.
func cursorStore() (*cursor.DiskStore, error) {
	prefsPath, err := prefs.ConfigPath("")
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfgDir := strings.TrimSuffix(prefsPath, string(filepath.Separator)+"preferences.json")
	return cursor.NewDiskStore(cfgDir)
}
