package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

// readOnly is one talosctl subcommand that only reads from a node, exposed
// here with viti's cluster and node resolution in front of it.
//
// They are declared as data rather than written out one by one because they
// differ in nothing but their name and their help: same targeting flags, same
// pass-through, same "all nodes unless told otherwise" default. Adding the
// next one (services, logs, disks, …) is a line in this table.
type readOnly struct {
	name    string
	verb    string
	aliases []string
	short   string
	long    string
	example string
}

func readOnlyCommands() []readOnly {
	return []readOnly{
		{
			name:  "dmesg",
			verb:  "dmesg",
			short: "📜 Read the kernel log from a cluster's nodes",
			long: `Retrieve the kernel ring buffer from every node of a cluster.

Talos prints its own boot and runtime diagnostics here, so this is where a
node that failed to come up says why. Output is prefixed with the node
address, which is what makes reading a whole cluster at once useful.`,
			example: `  viti talos dmesg my-cluster
  viti talos dmesg my-cluster --role controlplane
  viti talos dmesg my-cluster -- --follow --tail`,
		},
		{
			name:    "netstat",
			verb:    "netstat",
			aliases: []string{"net"},
			short:   "🕸️  Show network connections on a cluster's nodes",
			long: `Show network connections and listening sockets on a cluster's nodes.

Defaults to what talosctl itself defaults to; pass talosctl's own flags after
a -- to change that, e.g. --listening --programs for "who is listening on
what".`,
			example: `  viti talos netstat my-cluster
  viti talos netstat my-cluster -N my-cluster-ctp0 -- --listening --programs`,
		},
		{
			name:    "get",
			verb:    "get",
			aliases: []string{"resource", "resources"},
			short:   "🔧 Read any Talos resource from a cluster's nodes",
			long: `Read any Talos resource, the way "talosctl get" does.

This is the general form of "show", which is just "get machineconfig". Name
the resource after a -- , since it is talosctl's argument rather than this
command's:

  viti talos get my-cluster -- operatorspecs -o yaml
  viti talos get my-cluster -- links
  viti talos get my-cluster -- addresses

It is the way to check what a config change actually did to the running
system, as opposed to what it did to the config text — "talosctl get
operatorspecs" shows the DHCP client Talos ended up configuring, for
instance, which a config diff cannot tell you.`,
			example: `  viti talos get my-cluster -- operatorspecs -o yaml
  viti talos get my-cluster -N my-cluster-wrk0 -- links
  viti talos get my-cluster --role controlplane -- services`,
		},
		{
			name:    "memory",
			verb:    "memory",
			aliases: []string{"mem"},
			short:   "🧠 Show memory usage on a cluster's nodes",
			long: `Show memory usage per node.

Pass -- --verbose for the full breakdown talosctl can print rather than the
summary line.`,
			example: `  viti talos memory my-cluster
  viti talos memory my-cluster -- --verbose`,
		},
	}
}

func newReadOnlyCmd(s *scope, r readOnly) *cobra.Command {
	n := &nodeSelector{}

	cmd := &cobra.Command{
		Use:     r.name + " [cluster] [-- talosctl flags]",
		Aliases: r.aliases,
		Short:   r.short,
		Long: r.long + `

Leave [cluster] out to pick one from an interactive, fuzzy-searchable list.
Every node is targeted unless --node or --role narrows it.

Anything after a -- is handed to talosctl unchanged, so its own flags remain
available without this command having to mirror them.`,
		Example: r.example,
		Args:    cobra.ArbitraryArgs,
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
			echo(cmd, fmt.Sprintf("%s — %s on %s",
				sess.Cluster().Describe(), r.verb, strings.Join(nodeNames(nodes), ", ")))
			return talosctl.Run(contextOrBackground(cmd), streams(cmd), target, r.verb, passthrough...)
		},
	}
	n.register(cmd)
	return cmd
}

// nodeSelector holds the node-targeting flags shared by every command that
// addresses nodes within a single cluster.
type nodeSelector struct {
	nodes []string
	role  string
}

func (n *nodeSelector) register(cmd *cobra.Command) {
	cmd.Flags().StringArrayVarP(&n.nodes, "node", "N", nil,
		"target a node by machine name or address (repeatable; default: all)")
	cmd.Flags().StringVar(&n.role, "role", "",
		"limit to a role: controlplane, worker")
}

// resolveNodes turns a cluster name and the node flags into an open session
// and the nodes to act on. The caller owns closing the session.
func resolveNodes(cmd *cobra.Command, s *scope, n *nodeSelector, name string) (*cluster.Session, []cluster.Node, error) {
	role, err := parseRole(n.role)
	if err != nil {
		return nil, nil, err
	}
	c, err := selectCluster(cmd, s, name)
	if err != nil {
		return nil, nil, err
	}
	sess, err := openSession(cmd, s, c)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := sess.Topology.Select(n.nodes, role)
	if err != nil {
		sess.Close()
		return nil, nil, err
	}
	return sess, nodes, nil
}

// splitArgs separates the optional cluster name from the talosctl flags after
// a --.
//
// cobra keeps both in args and only tells us where the -- was, so the split
// has to happen here. Without it, "dmesg my-cluster -- --follow" would look
// like two cluster names.
func splitArgs(cmd *cobra.Command, args []string) (name string, passthrough []string, err error) {
	at := cmd.ArgsLenAtDash()
	if at < 0 {
		at = len(args)
	} else {
		passthrough = args[at:]
	}
	before := args[:at]
	switch len(before) {
	case 0:
		return "", passthrough, nil
	case 1:
		return before[0], passthrough, nil
	default:
		return "", nil, fmt.Errorf(
			"expected at most one cluster name, got %d (%s) — talosctl flags go after a --",
			len(before), strings.Join(before, ", "))
	}
}

func nodeAddresses(nodes []cluster.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.IP)
	}
	return out
}

func nodeNames(nodes []cluster.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

func streams(cmd *cobra.Command) talosctl.Streams {
	return talosctl.Streams{In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
}
