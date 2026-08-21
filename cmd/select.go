package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/config"
	"github.com/vitistack/vitictl-talos/internal/kube"
	"github.com/vitistack/vitictl/pkg/plugin/picker"
)

// scope holds the flags every command shares: what to look at, and how to
// reach it.
type scope struct {
	namespace string
	az        string
	endpoints []string
	ipv6      bool
	// allProviders lists clusters of every provider rather than Talos only.
	// It exists for diagnosis — "why is my cluster not in the picker" — not
	// for acting: a non-Talos cluster has no Talos API to talk to.
	allProviders bool
}

func (s *scope) register(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVarP(&s.namespace, "namespace", "n", "",
		"limit to this namespace")
	cmd.PersistentFlags().StringVarP(&s.az, "availabilityzone", "z", "",
		"limit to a single Vitistack availability zone")
	cmd.PersistentFlags().StringArrayVar(&s.endpoints, "endpoint", nil,
		"override the resolved Talos API endpoint(s) (repeatable)")
	cmd.PersistentFlags().BoolVar(&s.ipv6, "ipv6", false,
		"include IPv6 addresses when resolving endpoints and nodes (default: IPv4 only)")
	cmd.PersistentFlags().BoolVar(&s.allProviders, "all-providers", false,
		"do not filter to Talos clusters (diagnostic; non-Talos clusters have no Talos API)")
}

// clients connects to the availability zones in scope.
func (s *scope) clients(cmd *cobra.Command) ([]*kube.Client, error) {
	zones, err := config.AvailabilityZones(s.az)
	if err != nil {
		return nil, err
	}
	return kube.ConnectAll(contextOrBackground(cmd), zones, func(e error) { warn(cmd, e) })
}

// listClusters returns the clusters in scope.
func listClusters(cmd *cobra.Command, s *scope) ([]cluster.Cluster, error) {
	clients, err := s.clients(cmd)
	if err != nil {
		return nil, err
	}
	found := cluster.List(contextOrBackground(cmd), clients, s.namespace, !s.allProviders,
		func(e error) { warn(cmd, e) })
	if len(found) == 0 {
		return nil, fmt.Errorf("no Talos kubernetesclusters found%s%s",
			inNamespace(s.namespace), inZone(s.az))
	}
	return found, nil
}

// selectCluster resolves the single cluster a command should act on: the one
// named when a name is given and unambiguous, otherwise an interactive pick.
func selectCluster(cmd *cobra.Command, s *scope, name string) (cluster.Cluster, error) {
	// Checked before listing anything: with no name and no terminal there is
	// no way to choose, so every zone listing would be wasted work.
	if name == "" && !picker.Interactive() {
		return cluster.Cluster{}, errors.New(
			"no cluster given — pass one (e.g. 'viti talos dmesg my-cluster'), " +
				"or run in a terminal to pick one interactively")
	}

	found, err := listClusters(cmd, s)
	if err != nil {
		return cluster.Cluster{}, err
	}
	if name != "" {
		matches := cluster.Find(found, name)
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return cluster.Cluster{}, fmt.Errorf(
				"no Talos kubernetescluster named %q found in any availability zone%s "+
					"(the name may be either the resource name or its clusterId)",
				name, inNamespace(s.namespace))
		default:
			if !picker.Interactive() {
				return cluster.Cluster{}, cluster.Ambiguous(name, matches)
			}
			found = matches
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	return pickCluster(cmd, found)
}

// selectClusters resolves the set a fleet command should act on.
//
// Named clusters win; --all takes everything in scope; anything else opens the
// marking picker. There is deliberately no "no names and no --all means all"
// rule: a command that patches machine configs must never default to the whole
// fleet because an argument was forgotten.
func selectClusters(cmd *cobra.Command, s *scope, names []string, all bool) ([]cluster.Cluster, error) {
	if len(names) > 0 && all {
		return nil, errors.New("--cluster and --all are mutually exclusive")
	}
	if len(names) == 0 && !all && !picker.Interactive() {
		return nil, errors.New(
			"no clusters given — pass --cluster <name> (repeatable) or --all, " +
				"or run in a terminal to pick them interactively")
	}

	found, err := listClusters(cmd, s)
	if err != nil {
		return nil, err
	}
	if all {
		return found, nil
	}
	if len(names) > 0 {
		var out []cluster.Cluster
		var missing []string
		for _, n := range names {
			matches := cluster.Find(found, n)
			switch len(matches) {
			case 1:
				out = append(out, matches[0])
			case 0:
				missing = append(missing, n)
			default:
				return nil, cluster.Ambiguous(n, matches)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("no Talos kubernetescluster named %s found in any availability zone%s",
				strings.Join(missing, ", "), inNamespace(s.namespace))
		}
		return dedupeClusters(out), nil
	}
	return pickClusters(cmd, found)
}

// clusterColumns renders one cluster as the picker's and the list command's
// shared row, so what you pick looks like what you listed.
func clusterColumns(c cluster.Cluster, now time.Time) []string {
	return []string{
		c.Zone(), c.Namespace(), c.Name(), dash(c.ID()),
		dash(string(c.KC.Spec.Cluster.Provider)),
		dash(c.KC.Spec.Cluster.Environment),
		dash(c.KC.Status.Phase),
		dash(c.KC.Spec.Topology.Version),
		age(c.KC.CreationTimestamp, now),
	}
}

// clusterHeader titles clusterColumns.
func clusterHeader() []string {
	return []string{"AZ", "NAMESPACE", "NAME", "CLUSTER ID", "PROVIDER", "ENV", "PHASE", "K8S", "AGE"}
}

func clusterItems(clusters []cluster.Cluster) []picker.Item {
	now := time.Now()
	items := make([]picker.Item, 0, len(clusters))
	for _, c := range clusters {
		columns := clusterColumns(c, now)
		items = append(items, picker.Item{
			// Matched on every column, so an environment, a region or a
			// clusterId narrows the list as readily as a name does.
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   c,
		})
	}
	return items
}

// pickCluster shows the candidates and returns the chosen one.
func pickCluster(cmd *cobra.Command, clusters []cluster.Cluster) (cluster.Cluster, error) {
	chosen, err := picker.Select(" Select a Talos cluster ", clusterHeader(), clusterItems(clusters))
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return cluster.Cluster{}, errCancelled
		}
		return cluster.Cluster{}, err
	}
	got, ok := chosen.Value.(cluster.Cluster)
	if !ok {
		return cluster.Cluster{}, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	echo(cmd, got.Describe())
	return got, nil
}

// pickClusters shows the candidates with marking and returns every chosen one.
func pickClusters(cmd *cobra.Command, clusters []cluster.Cluster) ([]cluster.Cluster, error) {
	chosen, err := picker.SelectMulti(
		" Select Talos clusters — [Tab] mark, [Ctrl-A] mark all shown ",
		clusterHeader(), clusterItems(clusters))
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return nil, errCancelled
		}
		return nil, err
	}
	out := make([]cluster.Cluster, 0, len(chosen))
	for _, it := range chosen {
		got, ok := it.Value.(cluster.Cluster)
		if !ok {
			return nil, fmt.Errorf("picker returned an unexpected item %T", it.Value)
		}
		out = append(out, got)
	}
	if len(out) == 0 {
		return nil, errCancelled
	}
	echo(cmd, fmt.Sprintf("%d cluster(s) selected", len(out)))
	return out, nil
}

// pickNode chooses one node of a cluster, for the commands that address
// exactly one — editing a machine config being the obvious case.
func pickNode(cmd *cobra.Command, topo *cluster.Topology) (cluster.Node, error) {
	if !picker.Interactive() {
		return cluster.Node{}, errors.New(
			"no node given — pass --node <name|address>, or run in a terminal to pick one interactively")
	}
	items := make([]picker.Item, 0, len(topo.Nodes))
	for _, n := range topo.Nodes {
		columns := []string{n.Name, dash(n.IP), n.Role, dash(n.Phase), dash(n.TalosVersion)}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   n,
		})
	}
	chosen, err := picker.Select(
		" Select a node in "+topo.Cluster.Name()+" ",
		[]string{"NAME", "ADDRESS", "ROLE", "PHASE", "TALOS"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return cluster.Node{}, errCancelled
		}
		return cluster.Node{}, err
	}
	got, ok := chosen.Value.(cluster.Node)
	if !ok {
		return cluster.Node{}, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	if got.IP == "" {
		return cluster.Node{}, fmt.Errorf("machine %s reports no usable address — it cannot be reached over the Talos API", got.Name)
	}
	echo(cmd, got.Name+" ("+got.IP+")")
	return got, nil
}

// openSession resolves a cluster and mints its temporary talosconfig. Callers
// must Close the session.
func openSession(cmd *cobra.Command, s *scope, c cluster.Cluster) (*cluster.Session, error) {
	sess, err := cluster.Open(contextOrBackground(cmd), c, s.endpoints, s.ipv6)
	if err != nil {
		return nil, err
	}
	for _, w := range sess.Topology.Warnings {
		warn(cmd, errors.New(w))
	}
	return sess, nil
}

func dedupeClusters(in []cluster.Cluster) []cluster.Cluster {
	seen := make(map[string]struct{}, len(in))
	out := make([]cluster.Cluster, 0, len(in))
	for _, c := range in {
		key := c.Describe()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}
	return out
}

func inNamespace(ns string) string {
	if ns == "" {
		return ""
	}
	return " (namespace " + ns + ")"
}

func inZone(az string) string {
	if az == "" {
		return ""
	}
	return " (availability zone " + az + ")"
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// age renders a timestamp as a short duration, matching viti's column style.
func age(t metav1.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t.Time)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	default:
		return fmt.Sprintf("%dy", int(d.Hours())/(24*365))
	}
}
