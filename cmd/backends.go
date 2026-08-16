package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/gh"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/monitor"
)

// backendEndpointEnv names the environment variable holding an external
// backend endpoint, so a backend can be configured once for a shell rather
// than repeated on every invocation.
const backendEndpointEnv = "GH_MONITOR_BACKEND"

// backendOptions are the backend-selection flags, shared by the monitor
// command and `gh monitor backends`.
type backendOptions struct {
	// Name pins resolution to one registered backend.
	Name string
	// Endpoint is an external backend to connect to (unix:, tcp:, or exec:).
	Endpoint string
}

func addBackendFlags(cmd *cobra.Command, opts *backendOptions) {
	cmd.Flags().StringVar(&opts.Name, "backend", "", "Pin monitoring to a named backend (default: the most specific one registered for the target)")
	cmd.Flags().StringVar(&opts.Endpoint, "backend-endpoint", "", "Connect an external backend: unix:<path>, tcp:<host:port>, or exec:<command> (default: $"+backendEndpointEnv+")")
}

// endpoint returns the configured endpoint, falling back to the environment.
func (o *backendOptions) endpoint() string {
	if strings.TrimSpace(o.Endpoint) != "" {
		return strings.TrimSpace(o.Endpoint)
	}
	return strings.TrimSpace(os.Getenv(backendEndpointEnv))
}

// buildRegistry registers the built-in GitHub backend, then any external
// backend the caller configured.
//
// Order matters: the external backend registers last, so for the kinds and
// capabilities it declares it wins, and for everything else the built-in
// backend is still there. That is the whole point — a backend covers what it
// covers, and nothing silently goes unwatched because of what it does not.
func buildRegistry(ctx context.Context, opts *backendOptions, base monitor.RunOptions, budget bool) (*backend.Registry, error) {
	reg := backend.NewRegistry()

	builtin := &gh.Provider{
		API:    apiClientFactory,
		Base:   base,
		Budget: budget,
	}
	if err := reg.Use(builtin); err != nil {
		return nil, err
	}

	if ep := opts.endpoint(); ep != "" {
		transport, err := remote.ParseEndpoint(ep)
		if err != nil {
			return nil, err
		}
		provider, err := remote.Connect(ctx, transport)
		if err != nil {
			return nil, fmt.Errorf("connect backend %s: %w", ep, err)
		}
		if err := reg.Use(provider); err != nil {
			return nil, err
		}
	}

	if err := reg.Pin(opts.Name); err != nil {
		return nil, err
	}
	return reg, nil
}

// newBackendsCommand builds `gh monitor backends`: a listing of which backend
// serves which target kind. Resolution is otherwise invisible, and a backend
// quietly covering more or less than expected is exactly the sort of thing
// that turns a missing notification into an apparent all-clear.
func newBackendsCommand() *cobra.Command {
	opts := &backendOptions{}
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "backends",
		Short: "List the registered monitoring backends and the target kinds they cover",
		Long: `List the registered monitoring backends.

The built-in "gh" backend polls the GitHub API and covers every target kind.
An external backend configured with --backend-endpoint or $` + backendEndpointEnv + `
registers only the capabilities and target kinds it declares; everything it
does not cover stays with the built-in backend.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := buildRegistry(cmd.Context(), opts, monitor.RunOptions{}, false)
			if err != nil {
				return err
			}
			infos := reg.List()
			if asJSON {
				return encodeJSON(cmd, infos)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "BACKEND\tCAPABILITIES\tKINDS")
			for _, info := range infos {
				kinds := "all"
				if len(info.Kinds) > 0 {
					parts := make([]string, 0, len(info.Kinds))
					for _, k := range info.Kinds {
						parts = append(parts, string(k))
					}
					kinds = strings.Join(parts, ",")
				}
				caps := make([]string, 0, len(info.Capabilities))
				for _, c := range info.Capabilities {
					caps = append(caps, string(c))
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", info.Name, strings.Join(caps, ","), kinds)
			}
			return w.Flush()
		},
	}
	addBackendFlags(cmd, opts)
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the listing as JSON")
	return cmd
}
