package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Execute sets up the root command tree and executes it.
func Execute() error {
	root := newRootCommand()
	return root.Execute()
}

func newRootCommand() *cobra.Command {
	monitorOpts := &monitorOptions{}

	cmd := &cobra.Command{
		Use:           "gh monitor [<number> | <url>]",
		Short:         "Watch GitHub PRs, issues, workflow runs, refs, and repos",
		Long:          "Default command: continuously watch a pull request, emitting one event per genuinely-new change.\n\nWith --repo alone, watches the entire repository for new PRs and issues.\n\nRun 'gh monitor --help' for subcommands.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Setting Version is what wires up --version (and -v, which is free).
		Version: versionString(readBuildDetails()),
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				monitorOpts.Selector = args[0]
			}
			return runMonitor(cmd, monitorOpts)
		},
	}

	// Cobra's default template prefixes the command name and the word
	// "version"; the string is already self-describing.
	cmd.SetVersionTemplate("{{.Version}}\n")

	addMonitorFlags(cmd, monitorOpts)

	cmd.AddCommand(newVersionCommand())
	cmd.AddCommand(newCommentsCommand())
	cmd.AddCommand(newDraftCommand())
	cmd.AddCommand(newReviewCommand())
	cmd.AddCommand(newThreadsCommand())

	cmd.AddCommand(newReactCommand())
	cmd.AddCommand(newPrefsCommand())
	cmd.AddCommand(newDaemonCommand())
	cmd.AddCommand(newReloadCommand())
	cmd.AddCommand(newInstancesCommand())
	cmd.AddCommand(newBackendsCommand())
	return cmd
}

// ExecuteOrExit runs the command tree and exits with a non-zero status on error.
func ExecuteOrExit() {
	if err := Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
