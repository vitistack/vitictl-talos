package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

func newDashboardCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}

	cmd := &cobra.Command{
		Use:     "dashboard [cluster] [-- talosctl flags]",
		Aliases: []string{"dash", "top"},
		Short:   "📊 Open the Talos dashboard for a cluster",
		Long: `Open talosctl's full-screen dashboard against a cluster's nodes.

This is the closest thing Talos has to a console. Talos runs no getty, no
login shell and no SSH on any TTY, so attaching to a node's serial line
connects and then shows nothing at all; the dashboard is where its state,
logs and network actually are.

Leave [cluster] out to pick one from an interactive, fuzzy-searchable list.
Every node is shown unless --node or --role narrows it; inside the dashboard,
the left and right arrow keys move between them.`,
		Example: `  viti talos dashboard
  viti talos dashboard my-cluster
  viti talos dashboard my-cluster --role controlplane`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, passthrough, err := splitArgs(cmd, args)
			if err != nil {
				return err
			}
			sess, nodes, err := resolveNodes(cmd, s, n, name)
			if err != nil {
				return err
			}
			defer sess.Close()

			target := talosctl.Target{
				Talosconfig: sess.Talosconfig,
				Endpoints:   sess.Endpoints(),
				Nodes:       nodeAddresses(nodes),
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"📊 talosctl dashboard — cluster %s, endpoints %s, nodes %s\n",
				sess.Cluster().ID(), strings.Join(sess.Endpoints(), ","),
				strings.Join(nodeNames(nodes), ", "))
			return talosctl.Run(contextOrBackground(cmd), streams(cmd), target, "dashboard", passthrough...)
		},
	}
	n.register(cmd)
	return cmd
}
