package cluster

import (
	"context"
	"fmt"
)

// Session is one cluster, opened: its topology resolved and its credentials
// materialised as a talosconfig on disk that talosctl can be handed.
//
// It exists because every command in this plugin needs exactly the same three
// things — a talosconfig path, endpoints, and node addresses — and because the
// temp config must be cleaned up on every path out, including the ones that
// fail. Close is idempotent.
type Session struct {
	Topology *Topology
	// Talosconfig is the path to a temporary, single-context config file
	// holding this cluster's admin credentials.
	Talosconfig string

	cleanup func()
}

// Open resolves a cluster and writes its temporary talosconfig.
//
// endpointOverride replaces the resolved control-plane addresses when given,
// which is the escape hatch for a cluster reached through a tunnel or a jump
// host whose addresses the management cluster does not know.
func Open(ctx context.Context, c Cluster, endpointOverride []string, includeIPv6 bool) (*Session, error) {
	topo, err := Resolve(ctx, c, includeIPv6)
	if err != nil {
		return nil, err
	}
	if len(endpointOverride) > 0 {
		topo.Endpoints = dedupeKeepOrder(endpointOverride)
		topo.Source = SourceOverride
	}
	if len(topo.Endpoints) == 0 {
		return nil, fmt.Errorf(
			"no Talos API endpoints resolved for cluster %s — no CPVIP publishes pool members and no "+
				"control-plane machine reports a usable address; pass --endpoint to name one",
			c.Describe())
	}

	secret, err := FindSecret(ctx, c)
	if err != nil {
		return nil, err
	}
	// The context is named after the clusterId rather than the resource name:
	// it is what the Talos PKI was issued for, and what every message about
	// this cluster elsewhere in viti refers to.
	path, cleanup, err := WriteTempTalosconfig(secret, c.ID(), topo.Endpoints)
	if err != nil {
		return nil, err
	}
	return &Session{Topology: topo, Talosconfig: path, cleanup: cleanup}, nil
}

// Close removes the temporary talosconfig.
func (s *Session) Close() {
	if s == nil || s.cleanup == nil {
		return
	}
	s.cleanup()
	s.cleanup = nil
}

// Cluster is the cluster this session was opened for.
func (s *Session) Cluster() Cluster { return s.Topology.Cluster }

// Endpoints are the Talos API addresses talosctl connects through.
func (s *Session) Endpoints() []string { return s.Topology.Endpoints }
