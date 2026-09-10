package tunnel

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func testPod() *corev1.Pod {
	return Pod("talos-tunnel-3f9a2c1d", "viti-center", DefaultImage,
		"100.64.0.4", "admin@pos1-kv-cl01", 8*time.Hour)
}

// The namespaces this runs in enforce the restricted PodSecurity profile. A
// pod missing any one of these fields is rejected at admission, which is a
// slow and confusing way to learn about a typo.
func TestPodSatisfiesRestrictedPodSecurity(t *testing.T) {
	p := testPod()

	sc := p.Spec.SecurityContext
	if sc == nil {
		t.Fatal("pod has no securityContext")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot is not true")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 65534 {
		t.Errorf("runAsUser = %v, want 65534", sc.RunAsUser)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != 65534 {
		t.Errorf("runAsGroup = %v, want 65534", sc.RunAsGroup)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccompProfile is not RuntimeDefault")
	}

	if len(p.Spec.Containers) != 1 {
		t.Fatalf("pod has %d containers, want 1", len(p.Spec.Containers))
	}
	csc := p.Spec.Containers[0].SecurityContext
	if csc == nil {
		t.Fatal("container has no securityContext")
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation is not false")
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", csc.Capabilities)
	}
}

func TestPodForwardsToTheTargetNode(t *testing.T) {
	want := []string{"TCP-LISTEN:50000,fork,reuseaddr", "TCP:100.64.0.4:50000"}
	got := testPod().Spec.Containers[0].Args
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A kill -9 leaves no handler to run, so the pod has to be able to expire on
// its own or it forwards to a control plane forever.
func TestPodExpiresOnItsOwn(t *testing.T) {
	p := testPod()
	if p.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("pod has no activeDeadlineSeconds")
	}
	if *p.Spec.ActiveDeadlineSeconds != 28800 {
		t.Errorf("activeDeadlineSeconds = %d, want 28800", *p.Spec.ActiveDeadlineSeconds)
	}
	if p.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", p.Spec.RestartPolicy)
	}
}

// The label is how an orphaned pod is found again, so it has to be on there
// even when the cluster name cannot be a label value.
func TestPodIsLabelledForCleanup(t *testing.T) {
	p := testPod()
	if p.Labels[ManagedByLabel] != ManagedByLabelValue {
		t.Errorf("%s = %q, want %q", ManagedByLabel, p.Labels[ManagedByLabel], ManagedByLabelValue)
	}
	if p.Labels[NameLabel] != NameLabelValue {
		t.Errorf("%s = %q, want %q", NameLabel, p.Labels[NameLabel], NameLabelValue)
	}
	if p.Labels[TunnelForKey] != "admin-pos1-kv-cl01" {
		t.Errorf("%s = %q, want the sanitised cluster name", TunnelForKey, p.Labels[TunnelForKey])
	}
	if p.Annotations[TunnelForKey] != "admin@pos1-kv-cl01" {
		t.Errorf("annotation %s = %q, want the name verbatim", TunnelForKey, p.Annotations[TunnelForKey])
	}
}

// A namespace with a LimitRange or a quota rejects a pod that asks for
// nothing, and socat needs almost nothing.
func TestPodDeclaresResources(t *testing.T) {
	r := testPod().Spec.Containers[0].Resources
	if r.Requests.Cpu().IsZero() || r.Requests.Memory().IsZero() {
		t.Errorf("requests = %v, want cpu and memory set", r.Requests)
	}
	if r.Limits.Cpu().IsZero() || r.Limits.Memory().IsZero() {
		t.Errorf("limits = %v, want cpu and memory set", r.Limits)
	}
}

// Cluster names here are kube contexts like admin@pos1-kv-cl01, and "@" is not
// legal in a label value — the pod would be rejected outright.
func TestLabelValueSanitises(t *testing.T) {
	for in, want := range map[string]string{
		"admin@pos1-kv-cl01": "admin-pos1-kv-cl01",
		"bgo-virt-001":       "bgo-virt-001",
		"-leading":           "leading",
		"trailing-":          "trailing",
		"":                   "unnamed",
		"@@@":                "unnamed",
	} {
		if got := LabelValue(in); got != want {
			t.Errorf("LabelValue(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 100)
	if got := LabelValue(long); len(got) != 63 {
		t.Errorf("LabelValue(<100 chars>) is %d chars, want 63", len(got))
	}
}

// Two windows, or two engineers, must not collide on one pod name.
func TestPodNameIsUniquePerCall(t *testing.T) {
	a, err := PodName()
	if err != nil {
		t.Fatalf("PodName() error: %v", err)
	}
	b, err := PodName()
	if err != nil {
		t.Fatalf("PodName() error: %v", err)
	}
	if a == b {
		t.Errorf("PodName() returned %q twice", a)
	}
	if !strings.HasPrefix(a, PodNamePrefix) {
		t.Errorf("PodName() = %q, want the %q prefix", a, PodNamePrefix)
	}
}
