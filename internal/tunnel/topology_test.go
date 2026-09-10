package tunnel

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vitistack/vitictl-talos/internal/cluster"
)

func node(name string, controlPlane bool, ready bool, osImage string, ips ...string) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if controlPlane {
		n.Labels = map[string]string{ControlPlaneLabel: ""}
	}
	for _, ip := range ips {
		n.Status.Addresses = append(n.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	n.Status.NodeInfo.OSImage = osImage
	return n
}

func list(nodes ...corev1.Node) *corev1.NodeList { return &corev1.NodeList{Items: nodes} }

// ptr1-kv-cl01's control planes carry Talos' default hostnames
// (talos-np6-59s), so the role cannot be inferred from the name the way the
// Vitistack -ctp<N> scheme allows. The label is the only reliable signal.
func TestNodesFromReadsRoleFromTheLabel(t *testing.T) {
	got := NodesFrom(list(
		node("talos-np6-59s", true, true, "Talos (v1.13.6)", "100.64.0.53"),
		node("ptr1-kv-cl01-wrk01", false, true, "Talos (v1.13.6)", "100.64.0.121"),
	), false)
	if len(got) != 2 {
		t.Fatalf("NodesFrom() returned %d nodes, want 2", len(got))
	}
	if got[0].Role != cluster.RoleControlPlane {
		t.Errorf("Role = %q, want %q", got[0].Role, cluster.RoleControlPlane)
	}
	if got[1].Role != cluster.RoleWorker {
		t.Errorf("Role = %q, want %q", got[1].Role, cluster.RoleWorker)
	}
}

// Control planes first, matching how cluster.Resolve orders a topology, so a
// listing reads the same however the cluster was resolved.
func TestNodesFromPutsControlPlanesFirst(t *testing.T) {
	got := NodesFrom(list(
		node("wrk02", false, true, "Talos (v1.13.6)", "10.0.0.4"),
		node("ctp01", true, true, "Talos (v1.13.6)", "10.0.0.1"),
		node("wrk01", false, true, "Talos (v1.13.6)", "10.0.0.3"),
	), false)
	want := []string{"ctp01", "wrk01", "wrk02"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("node %d = %q, want %q", i, got[i].Name, w)
		}
	}
}

// Operator workstations often cannot reach v6 endpoints, and a single
// unreachable v6 address hangs talosctl until its dial timeout fires — the
// same reason cluster.Resolve defaults to v4.
func TestNodesFromFiltersIPv6ByDefault(t *testing.T) {
	n := list(node("ctp01", true, true, "Talos (v1.13.6)",
		"2a05:ec0:1018:301:be24:11ff:fe6f:1fef", "10.122.6.174"))

	if got := NodesFrom(n, false); got[0].IP != "10.122.6.174" {
		t.Errorf("IP = %q, want the v4 address", got[0].IP)
	}
	if got := NodesFrom(n, true); got[0].IP != "2a05:ec0:1018:301:be24:11ff:fe6f:1fef" {
		t.Errorf("IP = %q, want the first address with --ipv6", got[0].IP)
	}
}

func TestNodesFromRecordsReadiness(t *testing.T) {
	got := NodesFrom(list(
		node("ctp01", true, true, "Talos (v1.13.6)", "10.0.0.1"),
		node("ctp02", true, false, "Talos (v1.13.6)", "10.0.0.2"),
	), false)
	if got[0].Phase != PhaseReady {
		t.Errorf("Phase = %q, want %q", got[0].Phase, PhaseReady)
	}
	if got[1].Phase != PhaseNotReady {
		t.Errorf("Phase = %q, want %q", got[1].Phase, PhaseNotReady)
	}
}

// A node with no InternalIP is kept in the listing — it is real, and saying
// so is more useful than hiding it — but with no address to dial.
func TestNodesFromKeepsNodesWithNoAddress(t *testing.T) {
	got := NodesFrom(list(node("ctp01", true, true, "Talos (v1.13.6)")), false)
	if len(got) != 1 {
		t.Fatalf("NodesFrom() returned %d nodes, want 1", len(got))
	}
	if got[0].IP != "" {
		t.Errorf("IP = %q, want empty", got[0].IP)
	}
}

func TestTalosVersionFromOSImage(t *testing.T) {
	for in, want := range map[string]string{
		"Talos (v1.13.6)":              "v1.13.6",
		"  Talos (v1.13.6)  ":          "v1.13.6",
		"Ubuntu 24.04.1 LTS":           "",
		"":                             "",
		"Talos v1.13.6":                "",
	} {
		if got := TalosVersionFromOSImage(in); got != want {
			t.Errorf("TalosVersionFromOSImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// apid on the endpoint proxies to any -n node, so one healthy control plane is
// all the tunnel needs — but it must be a healthy one, or the failure surfaces
// as a connection refused three layers down.
func TestSelectViaPicksTheFirstReadyControlPlane(t *testing.T) {
	nodes := NodesFrom(list(
		node("ctp01", true, false, "Talos (v1.13.6)", "10.0.0.1"),
		node("ctp02", true, true, "Talos (v1.13.6)", "10.0.0.2"),
		node("wrk01", false, true, "Talos (v1.13.6)", "10.0.0.3"),
	), false)
	got, err := SelectVia(nodes, "")
	if err != nil {
		t.Fatalf("SelectVia() error: %v", err)
	}
	if got.Name != "ctp02" {
		t.Errorf("SelectVia() = %q, want ctp02", got.Name)
	}
}

func TestSelectViaHonoursTheOverrideByNameOrAddress(t *testing.T) {
	nodes := NodesFrom(list(
		node("ctp01", true, true, "Talos (v1.13.6)", "10.0.0.1"),
		node("wrk01", false, true, "Talos (v1.13.6)", "10.0.0.3"),
	), false)
	for _, q := range []string{"wrk01", "10.0.0.3"} {
		got, err := SelectVia(nodes, q)
		if err != nil {
			t.Fatalf("SelectVia(%q) error: %v", q, err)
		}
		if got.Name != "wrk01" {
			t.Errorf("SelectVia(%q) = %q, want wrk01", q, got.Name)
		}
	}
}

// "no Ready control plane" has to say what it did find, because the next
// question is always "then what is there".
func TestSelectViaExplainsWhenNothingIsEligible(t *testing.T) {
	nodes := NodesFrom(list(
		node("ctp01", true, false, "Talos (v1.13.6)", "10.0.0.1"),
	), false)
	_, err := SelectVia(nodes, "")
	if err == nil {
		t.Fatal("SelectVia() succeeded, want an error")
	}
	for _, want := range []string{"ctp01", "NotReady", "--via-node"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestSelectViaRejectsAnUnknownOverride(t *testing.T) {
	nodes := NodesFrom(list(node("ctp01", true, true, "Talos (v1.13.6)", "10.0.0.1")), false)
	if _, err := SelectVia(nodes, "nope"); err == nil {
		t.Fatal("SelectVia(\"nope\") succeeded, want an error")
	}
}

// Forwarding to a node with no address would build "TCP::50000" and fail
// inside the pod, where nobody is looking.
func TestSelectViaRejectsAnOverrideWithNoAddress(t *testing.T) {
	nodes := NodesFrom(list(node("ctp01", true, true, "Talos (v1.13.6)")), false)
	if _, err := SelectVia(nodes, "ctp01"); err == nil {
		t.Fatal("SelectVia() succeeded on an addressless node, want an error")
	}
}
