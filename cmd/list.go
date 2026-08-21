package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl/pkg/plugin/output"
)

func newClustersCmd(s *scope) *cobra.Command {
	var outputFlag string

	cmd := &cobra.Command{
		Use:     "clusters",
		Aliases: []string{"cluster", "list", "ls"},
		Short:   "📋 List the Talos clusters viti knows about",
		Long: `List the Talos KubernetesClusters across viti's availability zones.

This is the target list every other command picks from, so it is the first
thing to run when a cluster you expected is missing from a picker. Non-Talos
clusters are filtered out — they have no Talos API to talk to — and
--all-providers shows them anyway to make that visible.

Nothing here contacts a Talos cluster: it reads the management clusters only,
so it works even when the clusters themselves are unreachable.`,
		Example: `  viti talos clusters
  viti talos clusters -o wide
  viti talos clusters -z prod -o name`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := output.Parse(outputFlag)
			if err != nil {
				return err
			}
			found, err := listClusters(cmd, s)
			if err != nil {
				return err
			}
			return renderClusters(cmd, found, format)
		},
	}
	cmd.Flags().StringVarP(&outputFlag, "output", "o", "",
		fmt.Sprintf("output format: %s", strings.Join(output.ValidFormats, ", ")))
	return cmd
}

func renderClusters(cmd *cobra.Command, clusters []cluster.Cluster, format output.Format) error {
	out := cmd.OutOrStdout()
	switch format {
	case output.FormatJSON:
		return output.WriteJSON(out, clusterJSON(clusters))
	case output.FormatYAML:
		return output.WriteYAML(out, clusterJSON(clusters))
	case output.FormatName:
		for _, c := range clusters {
			_, _ = fmt.Fprintf(out, "kubernetescluster/%s/%s\n", c.Namespace(), c.Name())
		}
		return nil
	}

	now := time.Now()
	header := strings.Join(clusterHeader(), "\t")
	rows := make([]string, 0, len(clusters))
	for _, c := range clusters {
		cols := clusterColumns(c, now)
		if format != output.FormatWide {
			// The default view drops the columns that repeat across a fleet
			// (provider is Talos by definition here) in favour of what
			// distinguishes one cluster from another.
			cols = []string{cols[0], cols[1], cols[2], cols[3], cols[5], cols[6], cols[7], cols[8]}
			header = "AZ\tNAMESPACE\tNAME\tCLUSTER ID\tENV\tPHASE\tK8S\tAGE"
		}
		rows = append(rows, strings.Join(cols, "\t"))
	}
	return output.WriteTable(out, header, rows)
}

// clusterRow is the machine-readable shape of a listing: the identifying
// fields flattened, with the whole resource alongside so a consumer can reach
// anything else it carries.
type clusterRow struct {
	AvailabilityZone string `json:"availabilityZone"`
	Namespace        string `json:"namespace"`
	Name             string `json:"name"`
	ClusterID        string `json:"clusterId"`
	Provider         string `json:"provider"`
	Environment      string `json:"environment"`
	Phase            string `json:"phase"`
	KubernetesVer    string `json:"kubernetesVersion,omitempty"`
	Cluster          any    `json:"kubernetesCluster"`
}

func clusterJSON(clusters []cluster.Cluster) []clusterRow {
	out := make([]clusterRow, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, clusterRow{
			AvailabilityZone: c.Zone(),
			Namespace:        c.Namespace(),
			Name:             c.Name(),
			ClusterID:        c.ID(),
			Provider:         string(c.KC.Spec.Cluster.Provider),
			Environment:      c.KC.Spec.Cluster.Environment,
			Phase:            c.KC.Status.Phase,
			KubernetesVer:    c.KC.Spec.Topology.Version,
			Cluster:          c.KC,
		})
	}
	return out
}

func newNodesCmd(s *scope) *cobra.Command {
	var outputFlag string
	var role string
	var probe bool

	cmd := &cobra.Command{
		Use:     "nodes [cluster]",
		Aliases: []string{"node", "machines"},
		Short:   "🖥️  List a cluster's Talos nodes and the addresses used to reach them",
		Long: `Show the nodes of one Talos cluster: their machine names, the addresses
this plugin points talosctl at, their roles, and the Talos version each one's
image declares.

This is the command to run before anything that acts. The address column is
the one that matters: Talos issues its certificates for the node's own
addresses, so a node listed without one cannot be reached at all, and a
machine reporting a CNI address instead of a real NIC is what produces
"x509: certificate is valid for X, not Y" later on.

--probe additionally dials each address on the Talos API port. That answers
the other half of the question: whether this machine can actually reach the
cluster right now. When it cannot — a VPN or tunnel that is down — talosctl
reports a gRPC dial timeout that reads as a broken cluster, and the probe says
plainly that it is a broken network path instead.

Leave [cluster] out to pick one from an interactive, fuzzy-searchable list.`,
		Example: `  viti talos nodes
  viti talos nodes my-cluster
  viti talos nodes my-cluster --probe
  viti talos nodes my-cluster --role controlplane -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := output.Parse(outputFlag)
			if err != nil {
				return err
			}
			wantRole, err := parseRole(role)
			if err != nil {
				return err
			}
			c, err := selectCluster(cmd, s, firstArg(args))
			if err != nil {
				return err
			}
			topo, err := cluster.Resolve(contextOrBackground(cmd), c, s.ipv6)
			if err != nil {
				return err
			}
			for _, w := range topo.Warnings {
				warn(cmd, fmt.Errorf("%s", w))
			}
			return renderNodes(cmd, topo, wantRole, probe, format)
		},
	}
	cmd.Flags().StringVarP(&outputFlag, "output", "o", "",
		fmt.Sprintf("output format: %s", strings.Join(output.ValidFormats, ", ")))
	cmd.Flags().StringVar(&role, "role", "",
		"limit to a role: controlplane, worker")
	cmd.Flags().BoolVar(&probe, "probe", false,
		fmt.Sprintf("also dial each address on the Talos API port (tcp/%d) to check it answers", cluster.APIPort))
	return cmd
}

func renderNodes(cmd *cobra.Command, topo *cluster.Topology, role string, probe bool, format output.Format) error {
	out := cmd.OutOrStdout()

	nodes := make([]cluster.Node, 0, len(topo.Nodes))
	for _, n := range topo.Nodes {
		if role != "" && n.Role != role {
			continue
		}
		nodes = append(nodes, n)
	}

	var reachable map[string]bool
	if probe {
		reachable = probeNodes(nodes)
	}

	switch format {
	case output.FormatJSON:
		return output.WriteJSON(out, nodeReport(topo, nodes, reachable))
	case output.FormatYAML:
		return output.WriteYAML(out, nodeReport(topo, nodes, reachable))
	case output.FormatName:
		for _, n := range nodes {
			_, _ = fmt.Fprintln(out, n.Name)
		}
		return nil
	}

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "🐢 %s — endpoints %s (from %s)\n",
		topo.Cluster.Describe(), strings.Join(topo.Endpoints, ","), topo.Source)

	header := "NAME\tADDRESS\tROLE\tPHASE\tTALOS"
	if probe {
		header += "\tTALOS API"
	}
	rows := make([]string, 0, len(nodes))
	for _, n := range nodes {
		cells := []string{n.Name, dash(n.IP), n.Role, dash(n.Phase), dash(n.TalosVersion)}
		if probe {
			cells = append(cells, reachabilityLabel(reachable[n.IP]))
		}
		rows = append(rows, strings.Join(cells, "\t"))
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "🤷 no nodes matched")
		return err
	}
	return output.WriteTable(out, header, rows)
}

// nodeReportShape is the machine-readable rendering of a resolved cluster:
// the nodes plus the connection details they were resolved with, because
// "which endpoint did you use" is the first question after any failure.
type nodeReportShape struct {
	AvailabilityZone string    `json:"availabilityZone"`
	Namespace        string    `json:"namespace"`
	Cluster          string    `json:"cluster"`
	ClusterID        string    `json:"clusterId"`
	Endpoints        []string  `json:"endpoints"`
	EndpointSource   string    `json:"endpointSource"`
	Nodes            []nodeRow `json:"nodes"`
	Warnings         []string  `json:"warnings,omitempty"`
}

// nodeRow is a node plus, when it was probed, whether its Talos API answered.
// Reachable is a pointer so "not probed" and "probed and unreachable" are
// distinguishable in the JSON rather than both rendering as false.
type nodeRow struct {
	cluster.Node
	Reachable *bool `json:"talosApiReachable,omitempty"`
}

func nodeReport(topo *cluster.Topology, nodes []cluster.Node, reachable map[string]bool) nodeReportShape {
	rows := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		row := nodeRow{Node: n}
		if reachable != nil {
			ok := reachable[n.IP]
			row.Reachable = &ok
		}
		rows = append(rows, row)
	}
	return nodeReportShape{
		AvailabilityZone: topo.Cluster.Zone(),
		Namespace:        topo.Cluster.Namespace(),
		Cluster:          topo.Cluster.Name(),
		ClusterID:        topo.Cluster.ID(),
		Endpoints:        topo.Endpoints,
		EndpointSource:   string(topo.Source),
		Nodes:            rows,
		Warnings:         topo.Warnings,
	}
}

// probeNodes dials every node's Talos API port, in parallel.
func probeNodes(nodes []cluster.Node) map[string]bool {
	addrs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.IP != "" {
			addrs = append(addrs, n.IP)
		}
	}
	out := make(map[string]bool, len(addrs))
	for _, a := range cluster.Reachable(addrs, cluster.DefaultProbeTimeout) {
		out[a] = true
	}
	return out
}

func reachabilityLabel(ok bool) string {
	if ok {
		return "✅ open"
	}
	return "❌ no answer"
}

// parseRole validates the --role flag. An empty value means every role.
func parseRole(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "":
		return "", nil
	case cluster.RoleControlPlane, "cp", "control-plane", "master":
		return cluster.RoleControlPlane, nil
	case cluster.RoleWorker, "workers", "wrk":
		return cluster.RoleWorker, nil
	default:
		return "", fmt.Errorf("unknown role %q (valid: controlplane, worker)", role)
	}
}
