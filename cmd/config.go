package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/config"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

func newConfigCmd(s *scope) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Aliases: []string{"cfg"},
		Short:   "⚙️  Inspect what this plugin is configured to reach",
		Long: `viti-talos discovers almost everything it needs.

The availability zones come from vitictl's own ctl.config.yaml ("viti config
add"), and each Talos cluster's credentials come from the Secret its
management cluster holds — duplicating either here would only give the two
CLIs something to drift on.

The exception is talos.yaml, beside that file, which names the clusters that
are not KubernetesCluster resources — the management clusters and the KubeVirt
hypervisor clusters. Nothing in the management cluster knows their Talos
credentials, so "viti talos tunnel" is the one thing that has to be told. Its
nodes are still discovered rather than configured.

These subcommands are for finding out what that all resolves to when something
does not work.`,
	}
	cmd.AddCommand(newConfigPathCmd(), newConfigTestCmd(s))
	return cmd
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the Vitistack configuration file this plugin reads",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.VitistackConfigPath()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "vitistack (viti):  %s\n", path)

			tpath, err := config.TalosConfigPath()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "tunnel clusters:   %s%s\n", tpath, existsSuffix(tpath))
			_, err = fmt.Fprintf(out, "talosconfig:       minted per command from each cluster's Secret (never written to ~/.talos/config)\n")
			return err
		},
	}
}

func newConfigTestCmd(s *scope) *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Verify that talosctl and the Vitistack availability zones are reachable",
		Long: `Check every leg of the setup and report all of them.

Someone fixing their environment wants the whole picture, not one failure at a
time, so nothing short-circuits: talosctl, the availability zones, and how many
Talos clusters they hold are all reported before the command decides its exit
code.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			var failed bool
			report := func(label string, err error) {
				if err != nil {
					failed = true
					_, _ = fmt.Fprintf(out, "❌ %s: %v\n", label, err)
					return
				}
				_, _ = fmt.Fprintf(out, "✅ %s\n", label)
			}

			if path, err := talosctl.Path(); err != nil {
				report("talosctl", err)
			} else {
				report("talosctl ("+path+")", nil)
			}

			zones, err := config.AvailabilityZones(s.az)
			if err != nil {
				report("vitistack availability zones", err)
				if failed {
					return errConfigIncomplete
				}
				return nil
			}
			clients, err := s.clients(cmd)
			if err != nil {
				report("vitistack availability zones", err)
				return errConfigIncomplete
			}
			report(fmt.Sprintf("vitistack availability zones (%d/%d reachable)", len(clients), len(zones)), nil)

			found := cluster.List(contextOrBackground(cmd), clients, s.namespace, true, func(e error) { warn(cmd, e) })
			if len(found) == 0 {
				report("talos clusters", fmt.Errorf(
					"none found%s%s — 'viti talos clusters --all-providers' shows what is there instead",
					inNamespace(s.namespace), inZone(s.az)))
			} else {
				report(fmt.Sprintf("talos clusters (%d found)", len(found)), nil)
			}

			checkTunnelClusters(out, report)

			if failed {
				return errConfigIncomplete
			}
			return nil
		},
	}
}

// checkTunnelClusters reports the tunnel-cluster leg of "config test",
// calling report for anything that should make the command fail.
//
// A missing talos.yaml is never fatal: most people never open a tunnel, and
// not having the file is their normal state. A file that is *there* and wrong
// is the opposite. This is the command someone reaches for when a tunnel will
// not open, and rendering a YAML error as "none configured" tells them to
// create a file that already exists and that they have just broken — while
// exiting 0.
//
// It is separated from the RunE so it can be tested without a reachable
// availability zone, which the checks above it require.
func checkTunnelClusters(out io.Writer, report func(label string, err error)) {
	// Printed from the resolved path rather than from the error, whose
	// example block would be truncated to a dangling colon and would repeat
	// the reason the line already gives.
	tpath, _ := config.TalosConfigPath()
	switch tunnels, err := config.TunnelClusters(); {
	case errors.Is(err, config.ErrNoTalosConfig):
		_, _ = fmt.Fprintf(out, "➖ tunnel clusters: none configured (%s)\n", tpath)
	case err != nil:
		report("tunnel clusters", err)
	default:
		for _, tc := range tunnels {
			if _, statErr := os.Stat(tc.Talosconfig); statErr != nil {
				report("tunnel cluster "+tc.Name,
					fmt.Errorf("talosconfig %s is not readable: %w", tc.Talosconfig, statErr))
				continue
			}
			report(fmt.Sprintf("tunnel cluster %s (%s)", tc.Name, tc.Context), nil)
		}
	}
}

// errConfigIncomplete makes "config test" exit non-zero without printing a
// second error underneath the per-check report above.
var errConfigIncomplete = fmt.Errorf("one or more checks failed")

// existsSuffix marks a configured path that is not there, since "the file you
// are looking at is absent" is the answer to most questions that reach here.
func existsSuffix(path string) string {
	if _, err := os.Stat(path); err != nil {
		return " (not present)"
	}
	return ""
}

