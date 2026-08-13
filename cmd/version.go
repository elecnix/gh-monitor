package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// version is stamped at build time with
// `-ldflags "-X github.com/elecnix/gh-monitor/cmd.version=v1.2.3"`; see
// script/build.sh. It stays empty for `go build` and `go install`, which fall
// back to the module version recorded in the binary's build info.
var version string

// devel is what Go itself reports for a binary built outside a released
// module, and what we print rather than an empty string.
const devel = "(devel)"

// buildDetails is the provenance of the running binary: what the build stamped
// in, plus what the Go toolchain embedded on its own.
type buildDetails struct {
	Version   string // release tag, or "(devel)" for a local build
	Revision  string // vcs.revision; empty when the build stripped VCS info
	Time      string // vcs.time
	Modified  bool   // vcs.modified: the working tree was dirty at build time
	GoVersion string
}

// readBuildDetails reads the running binary's own provenance. Every field is
// best-effort: a binary built with -buildvcs=false carries no revision at all,
// so the caller must be able to render a partial record.
func readBuildDetails() buildDetails {
	d := buildDetails{Version: version, GoVersion: runtime.Version()}

	if info, ok := debug.ReadBuildInfo(); ok {
		if d.Version == "" {
			d.Version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				d.Revision = s.Value
			case "vcs.time":
				d.Time = s.Value
			case "vcs.modified":
				d.Modified = s.Value == "true"
			}
		}
	}

	if d.Version == "" {
		d.Version = devel
	}
	return d
}

// versionString renders one line a human can compare in a glance against
// `gh extension list` and against a release.
func versionString(d buildDetails) string {
	v := d.Version
	if v == "" {
		v = devel
	}

	rev := "unknown revision"
	if d.Revision != "" {
		rev = shortRevision(d.Revision)
		if d.Modified {
			rev += "-dirty"
		}
	}

	parts := []string{rev}
	if d.Time != "" {
		parts = append(parts, d.Time)
	}
	if d.GoVersion != "" {
		parts = append(parts, d.GoVersion)
	}

	return fmt.Sprintf("gh monitor %s (%s)", v, strings.Join(parts, ", "))
}

func shortRevision(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

// newVersionCommand mirrors --version as a subcommand. It is safe alongside the
// default command's positional PR selector: cobra resolves only the first
// positional as a subcommand name, and "version" was never a valid selector.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the running binary's version and build revision",
		Long: "Print the version, VCS revision and build time of the binary that is actually running.\n\n" +
			"`gh extension list` reports the install manifest, which is not derived from the\n" +
			"executable and does not change if the executable is replaced.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), versionString(readBuildDetails()))
			return nil
		},
	}
}
