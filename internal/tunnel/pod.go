package tunnel

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vitistack/vitictl-talos/internal/cluster"
)

const (
	// DefaultImage is the socat build the manual workaround was proven with.
	// Overridable because an air-gapped zone needs its own mirror.
	DefaultImage = "alpine/socat"

	// PodNamePrefix leads a per-run random suffix, so two windows — or two
	// engineers — never collide on one pod.
	PodNamePrefix = "talos-tunnel-"

	// containerName is what a kubectl logs/describe on the pod refers to.
	containerName = "tunnel"

	// nobodyUID is the uid and gid the container runs as. Any non-root value
	// satisfies the restricted profile; socat needs no identity of its own.
	nobodyUID = int64(65534)
)

// Labels and annotations a tunnel pod carries, so an orphan is findable:
//
//	kubectl -n viti-center delete pod -l app.kubernetes.io/managed-by=viti-talos
const (
	NameLabel           = "app.kubernetes.io/name"
	NameLabelValue      = "talos-tunnel"
	ManagedByLabel      = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "viti-talos"

	// TunnelForKey names the cluster the pod was opened for. It is both a
	// label and an annotation: the label is sanitised so it is selectable,
	// the annotation carries the name verbatim so it is readable.
	TunnelForKey = "vitistack.io/tunnel-for"
)

// ManagedBySelector finds every tunnel pod this plugin created.
const ManagedBySelector = ManagedByLabel + "=" + ManagedByLabelValue

// illegalLabelChars matches everything a Kubernetes label value may not hold.
var illegalLabelChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// PodName returns a fresh, unique tunnel pod name.
func PodName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a tunnel pod name: %w", err)
	}
	return PodNamePrefix + hex.EncodeToString(b[:]), nil
}

// Pod builds the socat pod that forwards to targetIP's Talos API.
//
// Every security field here is load-bearing: the namespaces this runs in
// enforce the restricted PodSecurity profile, which rejects a pod missing any
// one of them at admission — a slow and unhelpful way to find a typo. Hence
// the pure constructor, asserted field by field in the tests.
func Pod(name, namespace, image, targetIP, forCluster string, deadline time.Duration) *corev1.Pod {
	nonRoot := true
	noEscalation := false
	uid := nobodyUID
	gid := nobodyUID
	deadlineSeconds := int64(deadline.Seconds())

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				NameLabel:      NameLabelValue,
				ManagedByLabel: ManagedByLabelValue,
				TunnelForKey:   LabelValue(forCluster),
			},
			Annotations: map[string]string{TunnelForKey: forCluster},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadlineSeconds,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &nonRoot,
				RunAsUser:      &uid,
				RunAsGroup:     &gid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  containerName,
				Image: image,
				Args: []string{
					fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr", cluster.APIPort),
					fmt.Sprintf("TCP:%s:%d", targetIP, cluster.APIPort),
				},
				Ports: []corev1.ContainerPort{{ContainerPort: cluster.APIPort}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				// Declared because a namespace with a LimitRange or a quota
				// rejects a pod that asks for nothing. socat needs almost none
				// of it.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}},
		},
	}
}

// LabelValue makes s usable as a Kubernetes label value.
//
// Cluster names here are kube contexts — admin@pos1-kv-cl01 — and "@" is not
// legal in a label value, so the unsanitised name would have the pod rejected
// outright. The verbatim name survives as an annotation.
func LabelValue(s string) string {
	s = illegalLabelChars.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.Trim(s, "-_.")
	if s == "" {
		return "unnamed"
	}
	return s
}
