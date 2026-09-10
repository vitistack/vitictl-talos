package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/config"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
	"github.com/vitistack/vitictl-talos/internal/tunnel"
	"github.com/vitistack/vitictl/pkg/plugin/picker"
)

const tunnelLong = `Reach a cluster's Talos API through a pod inside its Kubernetes cluster.

Some Talos clusters answer on the Kubernetes API but not on tcp/50000 from
where you are sitting — the Vitistack management clusters and the KubeVirt
hypervisor clusters are the usual case. This runs a small socat pod inside the
cluster, forwards one control-plane node's Talos API to it, and brings that to
localhost. apid on that endpoint proxies to every other node, so one control
plane reaches all of them.

These clusters are not KubernetesCluster resources, so nothing in the
management cluster knows where they are or how to authenticate to them. Name
them in talos.yaml beside vitictl's own config ("viti talos config path"):

  clusters:
    - context: admin@pos1-kv-cl01
      talosconfig: ~/.talos/kvosltalos

Their nodes are not configured — they are read from the Kubernetes API each
time, so a rebuilt cluster needs no edit here.

With a -- , a talosctl command is run through the tunnel and everything is torn
down afterwards. Without one, the tunnel is held open and the talosctl line to
paste is printed, until Ctrl-C.

The pod is deleted on every exit path, and carries an activeDeadlineSeconds so
that even a killed process cannot leave one running forever.`

const tunnelExample = `  viti talos tunnel pos1-kv-cl01
  viti talos tunnel pos1-kv-cl01 -- get members
  viti talos tunnel pos1-kv-cl01 --role controlplane -- dmesg
  viti talos tunnel pos1-kv-cl01 --local-port 50000`

// tunnelOpts holds the flags specific to opening a tunnel.
type tunnelOpts struct {
	via       string
	namespace string
	image     string
	localPort int
	timeout   time.Duration
	deadline  time.Duration
}

func (o *tunnelOpts) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.via, "via-node", "",
		"control-plane node to forward through, by name or address (default: the first Ready one)")
	// Deliberately not -n/--namespace: that is the root's "limit to this
	// namespace", and one flag with two meanings on one command is a trap.
	cmd.Flags().StringVar(&o.namespace, "tunnel-namespace", "",
		"namespace to create the tunnel pod in (default: from talos.yaml, else "+config.DefaultTunnelNamespace+")")
	cmd.Flags().StringVar(&o.image, "image", tunnel.DefaultImage,
		"socat image for the tunnel pod")
	cmd.Flags().IntVar(&o.localPort, "local-port", 0,
		"local port to bind (default: a free one, printed once bound)")
	cmd.Flags().DurationVar(&o.timeout, "timeout", tunnel.DefaultTimeout,
		"how long to wait for the tunnel pod to become ready")
	cmd.Flags().DurationVar(&o.deadline, "deadline", tunnel.DefaultDeadline,
		"pod lifetime cap, so a killed process cannot leave one running")
}

func newTunnelCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}
	o := &tunnelOpts{}

	cmd := &cobra.Command{
		Use:     "tunnel [cluster] [-- talosctl command]",
		Aliases: []string{"portfwd"},
		Short:   "🛣️  Reach a Talos API that is not routable from here",
		Long:    tunnelLong,
		Example: tunnelExample,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTunnel(cmd, s, n, o, args)
		},
	}
	n.register(cmd)
	o.register(cmd)
	return cmd
}

func runTunnel(cmd *cobra.Command, s *scope, n *nodeSelector, o *tunnelOpts, args []string) error {
	name, passthrough, err := splitArgs(cmd, args)
	if err != nil {
		return err
	}
	if err := checkPassthrough(passthrough); err != nil {
		return err
	}
	role, err := parseRole(n.role)
	if err != nil {
		return err
	}
	tc, err := selectTunnelCluster(cmd, name)
	if err != nil {
		return err
	}
	if o.namespace != "" {
		tc.Namespace = o.namespace
	}
	raw, err := os.ReadFile(tc.Talosconfig) // #nosec G304 -- the path is this plugin's own config value
	if err != nil {
		return fmt.Errorf("reading the talosconfig configured for %s: %w", tc.Name, err)
	}

	// Ctrl-C must unwind through the deferred Close rather than killing the
	// process, which would leave the pod running in someone's cluster.
	ctx, stopSignals := signal.NotifyContext(contextOrBackground(cmd), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	tun, err := tunnel.Open(ctx, tunnel.Options{
		Cluster:     tc,
		Image:       o.image,
		LocalPort:   o.localPort,
		Via:         o.via,
		Timeout:     o.timeout,
		Deadline:    o.deadline,
		IncludeIPv6: s.ipv6,
		Warn:        func(e error) { warn(cmd, e) },
		Err:         cmd.ErrOrStderr(),
	})
	if err != nil {
		return err
	}
	defer tun.Close()

	nodes, err := (&cluster.Topology{Nodes: tun.Nodes}).Select(n.nodes, role)
	if err != nil {
		return fmt.Errorf("%s: %w", tc.Name, err)
	}

	path, cleanup, err := cluster.WriteTempTalosconfigBytes(raw, tc.Name, []string{tun.LocalAddr})
	if err != nil {
		return fmt.Errorf("talosconfig %s: %w", tc.Talosconfig, err)
	}
	defer cleanup()

	echo(cmd, fmt.Sprintf("%s — tunnel via %s/%s → %s:%d",
		tc.Name, tun.Namespace, tun.PodName, tun.Via.IP, cluster.APIPort))
	echo(cmd, "ready on "+tun.LocalAddr)

	target := talosctl.Target{
		Talosconfig: path,
		// Passed explicitly even though the temp config carries them, per this
		// package's rule that a target is never left to ambient state.
		Endpoints: []string{tun.LocalAddr},
		Nodes:     nodeAddresses(nodes),
	}
	if len(passthrough) > 0 {
		if runErr := talosctl.Run(ctx, streams(cmd), target, passthrough[0], passthrough[1:]...); runErr != nil {
			return fmt.Errorf("%w\n%s", runErr, credentialHint(tc))
		}
		return nil
	}

	_, _ = fmt.Fprint(cmd.ErrOrStderr(), tunnelUsage(path, nodes))
	<-ctx.Done()
	echo(cmd, "tearing down")
	return nil
}

// checkPassthrough rejects a talosctl invocation that leads with a flag.
//
// build() puts the subcommand first, so "-- --follow" would produce
// "talosctl --follow --talosconfig …" and fail with a message about the flag
// rather than about the mistake.
func checkPassthrough(passthrough []string) error {
	if len(passthrough) == 0 || !strings.HasPrefix(passthrough[0], "-") {
		return nil
	}
	return fmt.Errorf(
		"the first word after -- must be a talosctl subcommand, got %q "+
			"(e.g. 'viti talos tunnel my-cluster -- dmesg --follow')", passthrough[0])
}

// credentialHint follows a failed talosctl run through a tunnel.
//
// talosctl's output streams straight to the terminal rather than through this
// process, so an exit code is all that comes back here — there is nothing to
// pattern-match on. The one failure worth naming in advance is a talosconfig
// for the wrong cluster: it surfaces as "x509: certificate signed by unknown
// authority", which reads as a broken cluster rather than as a mismatched
// file, and the tunnel makes it likelier by decoupling the credentials from
// the cluster they were fetched for.
func credentialHint(tc config.TunnelCluster) string {
	return fmt.Sprintf(
		"   if that was \"x509: certificate signed by unknown authority\", then %s holds "+
			"credentials for a different cluster than %s", tc.Talosconfig, tc.Context)
}

// tunnelUsage renders the block printed when the tunnel is held open.
//
// No -e: the temp talosconfig already names the local endpoint, so what is
// printed is the shortest line that actually works.
func tunnelUsage(talosconfig string, nodes []cluster.Node) string {
	var b strings.Builder
	b.WriteString("\n  export TALOSCONFIG=" + talosconfig + "\n")
	b.WriteString("  talosctl -n " + strings.Join(nodeAddresses(nodes), ",") + " <command>\n")
	b.WriteString("\n(Ctrl-C to tear down)\n")
	return b.String()
}

// selectTunnelCluster resolves the configured cluster to open a tunnel to.
func selectTunnelCluster(cmd *cobra.Command, name string) (config.TunnelCluster, error) {
	clusters, err := config.TunnelClusters()
	if err != nil {
		return config.TunnelCluster{}, err
	}
	if name != "" {
		found, ok := config.FindTunnelCluster(clusters, name)
		if !ok {
			return config.TunnelCluster{}, fmt.Errorf(
				"no tunnel cluster named %q — configured: %s",
				name, strings.Join(tunnelClusterNames(clusters), ", "))
		}
		return found, nil
	}
	if len(clusters) == 1 {
		return clusters[0], nil
	}
	if !picker.Interactive() {
		return config.TunnelCluster{}, fmt.Errorf(
			"no cluster given — pass one (configured: %s), or run in a terminal to pick one interactively",
			strings.Join(tunnelClusterNames(clusters), ", "))
	}
	return pickTunnelCluster(cmd, clusters)
}

func pickTunnelCluster(cmd *cobra.Command, clusters []config.TunnelCluster) (config.TunnelCluster, error) {
	items := make([]picker.Item, 0, len(clusters))
	for _, c := range clusters {
		columns := []string{c.Name, c.Context, c.Namespace, c.Talosconfig}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   c,
		})
	}
	chosen, err := picker.Select(" Select a cluster to tunnel to ",
		[]string{"NAME", "CONTEXT", "NAMESPACE", "TALOSCONFIG"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return config.TunnelCluster{}, errCancelled
		}
		return config.TunnelCluster{}, err
	}
	got, ok := chosen.Value.(config.TunnelCluster)
	if !ok {
		return config.TunnelCluster{}, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	echo(cmd, got.Name)
	return got, nil
}

func tunnelClusterNames(in []config.TunnelCluster) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, c.Name)
	}
	return out
}
