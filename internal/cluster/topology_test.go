package cluster

import (
	"strings"
	"testing"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

func machine(name string, ifaces ...vitiv1alpha1.NetworkInterfaceStatus) *vitiv1alpha1.Machine {
	m := &vitiv1alpha1.Machine{}
	m.Name = name
	m.Status.NetworkInterfaces = ifaces
	return m
}

func iface(name, ifaceType string, ips ...string) vitiv1alpha1.NetworkInterfaceStatus {
	return vitiv1alpha1.NetworkInterfaceStatus{Name: name, Type: ifaceType, IPAddresses: ips}
}

// Pointing talosctl at a CNI address fails with "x509: certificate is valid
// for X, not Y", which reads as a broken PKI rather than the wrong address —
// so the filtering is worth pinning down.
func TestMachineNodeIPsDropsVirtualInterfaces(t *testing.T) {
	m := machine("node",
		iface("cilium_host", "ethernet", "10.244.0.1"),
		iface("eth0", "ethernet", "192.168.1.10"),
		iface("lxc1234", "ethernet", "10.244.0.7"),
		iface("lo", "loopback", "127.0.0.1"),
	)
	got := MachineNodeIPs(m)
	if len(got) != 1 || got[0] != "192.168.1.10" {
		t.Errorf("MachineNodeIPs() = %v, want [192.168.1.10]", got)
	}
}

// KubeVirt guests report their CNI interfaces through the guest agent with no
// Name at all, so the name filter cannot catch them; the infoSource marker is
// what distinguishes a real NIC there.
func TestMachineNodeIPsPrefersKubevirtAttachedInterfaces(t *testing.T) {
	m := machine("node",
		iface("", "guest-agent", "10.244.0.5"),
		iface("", "domain, guest-agent", "192.168.1.11"),
		iface("", "multus-status", "192.168.2.11"),
	)
	got := MachineNodeIPs(m)
	if len(got) != 2 || got[0] != "192.168.1.11" || got[1] != "192.168.2.11" {
		t.Errorf("MachineNodeIPs() = %v, want the two attached addresses", got)
	}
}

func TestMachineNodeIPsFallsBackToStatusAddresses(t *testing.T) {
	m := &vitiv1alpha1.Machine{}
	m.Status.IPAddresses = []string{"127.0.0.1", "192.168.1.12", "192.168.1.12"}
	got := MachineNodeIPs(m)
	if len(got) != 1 || got[0] != "192.168.1.12" {
		t.Errorf("MachineNodeIPs() = %v, want [192.168.1.12] (loopback dropped, duplicate deduped)", got)
	}
}

func TestMachineNodeIPsIsNilSafe(t *testing.T) {
	if got := MachineNodeIPs(nil); got != nil {
		t.Errorf("MachineNodeIPs(nil) = %v, want nil", got)
	}
}

// v6 is off by default because an unreachable v6 entry hangs talosctl until
// its dial timeout rather than failing fast.
func TestFilterIPFamily(t *testing.T) {
	in := []string{"192.168.1.1", "fd00::1", "not-an-ip"}
	if got := filterIPFamily(in, false); len(got) != 1 || got[0] != "192.168.1.1" {
		t.Errorf("filterIPFamily(v4 only) = %v", got)
	}
	if got := filterIPFamily(in, true); len(got) != 3 {
		t.Errorf("filterIPFamily(includeIPv6) = %v, want the input unchanged", got)
	}
}

func topo() *Topology {
	return &Topology{
		Nodes: []Node{
			{Name: "c1-ctp0", IP: "10.0.0.1", Role: RoleControlPlane},
			{Name: "c1-ctp1", IP: "10.0.0.2", Role: RoleControlPlane},
			{Name: "c1-wrk0", IP: "10.0.0.3", Role: RoleWorker},
		},
	}
}

func TestSelectDefaultsToEveryNodeOfTheRole(t *testing.T) {
	got, err := topo().Select(nil, RoleControlPlane)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Select(nil, controlplane) = %v, want both control planes", got)
	}
	all, err := topo().Select(nil, "")
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("Select(nil, \"\") = %d nodes, want 3", len(all))
	}
}

// A human types the machine name from a listing; a script pastes back the
// address it was given. Both have to work.
func TestSelectMatchesNameOrAddress(t *testing.T) {
	got, err := topo().Select([]string{"c1-wrk0", "10.0.0.1"}, "")
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if len(got) != 2 || got[0].Name != "c1-wrk0" || got[1].Name != "c1-ctp0" {
		t.Errorf("Select() = %v, want the worker then the first control plane", got)
	}
}

func TestSelectReportsUnknownNodes(t *testing.T) {
	_, err := topo().Select([]string{"c1-wrk9"}, "")
	if err == nil {
		t.Fatal("Select() accepted a node that does not exist")
	}
	if !strings.Contains(err.Error(), "c1-wrk9") || !strings.Contains(err.Error(), "have:") {
		t.Errorf("error does not say what was asked for or what exists: %v", err)
	}
}

// --role and --node must intersect, not shadow each other: naming a worker
// while asking for control planes is a mistake worth reporting.
func TestSelectAppliesRoleBeforeNames(t *testing.T) {
	if _, err := topo().Select([]string{"c1-wrk0"}, RoleControlPlane); err == nil {
		t.Error("Select() returned a worker under --role controlplane")
	}
}

// Handing talosctl an empty node list silently falls back to the talosconfig's
// own nodes, which is not what was asked for.
func TestSelectRefusesNodesWithoutAddresses(t *testing.T) {
	tp := &Topology{Nodes: []Node{{Name: "c1-ctp0", Role: RoleControlPlane}}}
	_, err := tp.Select(nil, "")
	if err == nil {
		t.Fatal("Select() returned a node with no address")
	}
	if !strings.Contains(err.Error(), "c1-ctp0") {
		t.Errorf("error does not name the addressless node: %v", err)
	}
}

func TestNodeIPsFiltersByRole(t *testing.T) {
	tp := topo()
	if got := tp.NodeIPs(RoleWorker); len(got) != 1 || got[0] != "10.0.0.3" {
		t.Errorf("NodeIPs(worker) = %v", got)
	}
	if got := tp.NodeIPs(""); len(got) != 3 {
		t.Errorf("NodeIPs(all) = %v, want 3", got)
	}
}

func TestVersionFromImageID(t *testing.T) {
	cases := map[string]string{
		"https://factory.talos.dev/image/b0f2a8b5/v1.13.7/nocloud-amd64.iso": "1.13.7",
		"https://factory.talos.dev/image/abc/v1.14.0-beta.1/metal-arm64.iso": "1.14.0-beta.1",
		"":                     "",
		"not-a-url":            "",
		"https://x/no/ver.iso": "",
	}
	for in, want := range cases {
		if got := VersionFromImageID(in); got != want {
			t.Errorf("VersionFromImageID(%q) = %q, want %q", in, got, want)
		}
	}
}
