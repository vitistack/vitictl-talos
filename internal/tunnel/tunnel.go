package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/config"
	"github.com/vitistack/vitictl-talos/internal/kube"
)

const (
	// DefaultTimeout bounds how long the tunnel pod has to become ready.
	// Generous enough for a cold image pull, short enough that a wedged pod
	// is reported rather than waited on.
	DefaultTimeout = 60 * time.Second

	// DefaultDeadline is the pod's own activeDeadlineSeconds: the backstop for
	// the one exit path no handler can catch, a kill -9.
	DefaultDeadline = 8 * time.Hour

	// deleteTimeout bounds the teardown delete, which runs on a context
	// deliberately detached from the cancelled one.
	deleteTimeout = 30 * time.Second

	// pollInterval is how often the pod is checked while waiting for Ready.
	pollInterval = 500 * time.Millisecond
)

// Options configures one tunnel.
type Options struct {
	Cluster     config.TunnelCluster
	Image       string
	LocalPort   int
	Via         string
	Timeout     time.Duration
	Deadline    time.Duration
	IncludeIPv6 bool
	// Warn reports non-fatal problems — a pre-existing tunnel pod, a delete
	// that did not land — without failing the command.
	Warn func(error)
	// Err receives the port-forwarder's own error output.
	Err io.Writer
}

func (o *Options) applyDefaults() {
	if strings.TrimSpace(o.Image) == "" {
		o.Image = DefaultImage
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Deadline <= 0 {
		o.Deadline = DefaultDeadline
	}
	if o.Warn == nil {
		o.Warn = func(error) {}
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
}

// Tunnel is an open path to a cluster's Talos API: a socat pod inside the
// cluster and a port-forward to it from here.
//
// It mirrors cluster.Session: opened once, torn down on every path out
// including the ones that fail, and Close is idempotent. The difference is
// that a Session leaves only a temp file behind, while an un-closed Tunnel
// leaves a pod running in somebody's cluster.
type Tunnel struct {
	// LocalAddr is what talosctl connects to, e.g. "127.0.0.1:54417".
	LocalAddr string
	// Via is the control-plane node the pod forwards to.
	Via cluster.Node
	// Nodes is every node of the cluster, as discovered from its Kubernetes
	// API — control planes first.
	Nodes []cluster.Node

	PodName   string
	Namespace string

	clientset kubernetes.Interface
	stop      chan struct{}
	warn      func(error)
}

// Open creates the tunnel pod and brings its Talos API to localhost.
//
// Every step that can fail tears down what the steps before it created, so a
// failure never leaves a pod behind.
func Open(ctx context.Context, o Options) (*Tunnel, error) {
	o.applyDefaults()

	rc, err := kube.RESTConfig(o.Cluster.Kubeconfig, o.Cluster.Context)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Cluster.Name, err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("%s: building a kubernetes client: %w", o.Cluster.Name, err)
	}

	nodeList, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%s: listing nodes through context %s: %w",
			o.Cluster.Name, o.Cluster.Context, err)
	}
	nodes := NodesFrom(nodeList, o.IncludeIPv6)
	via, err := SelectVia(nodes, o.Via)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Cluster.Name, err)
	}

	warnAboutOrphans(ctx, cs, o)

	name, err := PodName()
	if err != nil {
		return nil, err
	}
	pod := Pod(name, o.Cluster.Namespace, o.Image, via.IP, o.Cluster.Name, o.Deadline)
	if _, err := cs.CoreV1().Pods(o.Cluster.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("%s: creating tunnel pod %s/%s: %w",
			o.Cluster.Name, o.Cluster.Namespace, name, err)
	}

	t := &Tunnel{
		Via:       via,
		Nodes:     nodes,
		PodName:   name,
		Namespace: o.Cluster.Namespace,
		clientset: cs,
		stop:      make(chan struct{}),
		warn:      o.Warn,
	}
	if err := t.waitReady(ctx, o.Timeout); err != nil {
		t.Close()
		return nil, err
	}
	if err := t.forward(ctx, rc, cs, o); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

// Close stops the port-forward and deletes the pod. It is idempotent, and safe
// on a nil receiver, because it is reached from a defer, from a signal, and
// from the defer that runs after the signal.
func (t *Tunnel) Close() {
	if t == nil {
		return
	}
	if t.stop != nil {
		close(t.stop)
		t.stop = nil
	}
	if t.clientset == nil || t.PodName == "" {
		return
	}
	cs := t.clientset
	t.clientset = nil

	// A fresh context on purpose, not the caller's. Teardown is usually
	// triggered *by* the caller's context being cancelled — Ctrl-C — and
	// inheriting it would cancel the delete along with everything else,
	// leaving the pod running: the one outcome this function exists to
	// prevent. Nothing the delete needs travels in a context; the credentials
	// are in the clientset.
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()

	grace := int64(0)
	err := cs.CoreV1().Pods(t.Namespace).Delete(ctx, t.PodName,
		metav1.DeleteOptions{GracePeriodSeconds: &grace})
	if err != nil && !apierrors.IsNotFound(err) && t.warn != nil {
		t.warn(fmt.Errorf(
			"tunnel pod %s/%s was not deleted (%v) — remove it with: kubectl -n %s delete pod %s",
			t.Namespace, t.PodName, err, t.Namespace, t.PodName))
	}
}

// waitReady blocks until the tunnel pod can accept a connection.
func (t *Tunnel) waitReady(ctx context.Context, timeout time.Duration) error {
	var last *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			p, err := t.clientset.CoreV1().Pods(t.Namespace).Get(ctx, t.PodName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			last = p
			return podReady(p), nil
		})
	if err == nil {
		return nil
	}
	if last == nil {
		return fmt.Errorf("waiting for tunnel pod %s/%s: %w", t.Namespace, t.PodName, err)
	}
	return fmt.Errorf("tunnel pod %s/%s did not become ready within %s: %s",
		t.Namespace, t.PodName, timeout, NotReadyReason(last))
}

// forward brings the pod's Talos API port to localhost.
func (t *Tunnel) forward(ctx context.Context, rc *rest.Config, cs *kubernetes.Clientset, o Options) error {
	rt, upgrader, err := spdy.RoundTripperFor(rc)
	if err != nil {
		return fmt.Errorf("building the port-forward transport: %w", err)
	}
	u := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(t.Namespace).Name(t.PodName).
		SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)

	ready := make(chan struct{})
	// The forwarder's own stdout is discarded: it prints its own
	// "Forwarding from …" line, and this command prints a better one.
	pf, err := portforward.New(dialer, []string{PortSpec(o.LocalPort)}, t.stop, ready, io.Discard, o.Err)
	if err != nil {
		return fmt.Errorf("preparing the port-forward: %w", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- pf.ForwardPorts() }()

	select {
	case <-ready:
	case err := <-errCh:
		return fmt.Errorf("port-forward to %s/%s failed: %w", t.Namespace, t.PodName, err)
	case <-ctx.Done():
		return ctx.Err()
	}

	ports, err := pf.GetPorts()
	if err != nil {
		return fmt.Errorf("reading the forwarded port: %w", err)
	}
	if len(ports) == 0 {
		return fmt.Errorf("port-forward to %s/%s bound no port", t.Namespace, t.PodName)
	}
	t.LocalAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0].Local)))
	return nil
}

// PortSpec renders the port-forward mapping. A local port of 0 asks the kernel
// for a free one, which is the default because 50000 is so often taken.
func PortSpec(local int) string {
	return fmt.Sprintf("%d:%d", local, cluster.APIPort)
}

// podReady reports whether the pod will accept a connection.
func podReady(p *corev1.Pod) bool {
	if p == nil || p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// NotReadyReason explains why a pod is not ready yet, in the pod's own words.
//
// "timed out waiting for the pod" is not a diagnosis, and the first failure in
// a zone without a mirror for alpine/socat will be an image pull — which the
// pod's container status already says plainly.
func NotReadyReason(p *corev1.Pod) string {
	if p == nil {
		return "the pod could not be read back after it was created"
	}
	parts := []string{"phase " + string(p.Status.Phase)}
	for _, cs := range p.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		detail := w.Reason
		if w.Message != "" {
			detail += ": " + w.Message
		}
		parts = append(parts, fmt.Sprintf("container %s %s", cs.Name, detail))
	}
	return strings.Join(parts, ", ")
}

// warnAboutOrphans reports tunnel pods already in the namespace.
//
// Reported, not deleted: a colleague may be holding one open right now, and
// deleting it would drop their session with no warning. Reporting is enough —
// the label makes the cleanup a one-liner, which the message prints.
func warnAboutOrphans(ctx context.Context, cs kubernetes.Interface, o Options) {
	list, err := cs.CoreV1().Pods(o.Cluster.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: ManagedBySelector})
	if err != nil || len(list.Items) == 0 {
		return
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	o.Warn(fmt.Errorf(
		"%d tunnel pod(s) already in %s/%s (%s) — someone may be using them; "+
			"clean up with: kubectl -n %s delete pod -l %s",
		len(names), o.Cluster.Name, o.Cluster.Namespace, strings.Join(names, ", "),
		o.Cluster.Namespace, ManagedBySelector))
}
