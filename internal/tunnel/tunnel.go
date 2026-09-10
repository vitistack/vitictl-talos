package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

// ErrLostConnection reports a port-forward that ended without an error of its
// own. client-go's forwarder returns nil both when it is stopped deliberately
// and when the stream is lost — pod evicted, node drained,
// activeDeadlineSeconds reached — so the second case needs an error of its
// own, or a tunnel that died would look like one that was never open.
var ErrLostConnection = errors.New("the connection to the tunnel pod was lost")

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

// applyDefaults fills in what was left unset and rejects a combination that
// cannot produce a working tunnel.
func (o *Options) applyDefaults() error {
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
	// activeDeadlineSeconds below the readiness wait guarantees the pod is
	// killed before it can carry anything, and the failure it produces is a
	// readiness timeout that says nothing about the deadline that caused it.
	if o.Deadline < o.Timeout {
		return fmt.Errorf(
			"deadline %s is shorter than timeout %s: the tunnel pod would be killed before it was ready",
			o.Deadline, o.Timeout)
	}
	// activeDeadlineSeconds is whole seconds, so anything under one rounds
	// down to zero and is refused by the apiserver's own validation — a
	// message about a field this tool set, not about the flag that set it.
	if int64(o.Deadline.Seconds()) == 0 {
		return fmt.Errorf("deadline %s rounds down to zero seconds; use at least 1s", o.Deadline)
	}
	return nil
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

	// done closes when the port-forwarder returns and fwdErr says why, so a
	// tunnel that dies on its own can be selected on. Without that the
	// command sits on <-ctx.Done() still advertising a local port that
	// stopped accepting, and the next talosctl gets "connection refused"
	// from a CLI claiming the tunnel is up.
	done chan struct{}

	// mu guards fwdErr and closing, which the forwarder goroutine writes and
	// Err reads.
	mu sync.Mutex
	// closing records that this process asked for the teardown, so the nil
	// the forwarder then returns is not reported as a failure.
	closing bool
	fwdErr  error

	// closeOnce guards the body of Close, which is reached from a defer, a
	// signal handler, and the defer that runs after the signal — three
	// goroutines is common, not hypothetical. Without it, two concurrent
	// callers can both observe stop != nil and both close() it, panicking.
	closeOnce sync.Once
}

// Done closes when the port-forward stops carrying traffic, whether the pod
// died, the deadline fired, or Close tore it down.
//
// A tunnel that was never forwarded returns nil, which blocks forever in a
// select: it has nothing that can stop.
func (t *Tunnel) Done() <-chan struct{} {
	if t == nil {
		return nil
	}
	return t.done
}

// Err reports why the port-forward stopped. It is meaningful once Done has
// closed, and is nil for a teardown this process asked for.
func (t *Tunnel) Err() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fwdErr
}

// setForwardErr records why the forwarder returned, translating the two ways
// it can say nothing: a teardown this process asked for is not a failure, and
// a nil from anything else means the stream was lost.
func (t *Tunnel) setForwardErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return
	}
	if err == nil {
		err = ErrLostConnection
	}
	t.fwdErr = err
}

// Open creates the tunnel pod and brings its Talos API to localhost.
//
// Every step that can fail tears down what the steps before it created, so a
// failure never leaves a pod behind.
func Open(ctx context.Context, o Options) (*Tunnel, error) {
	if err := o.applyDefaults(); err != nil {
		return nil, fmt.Errorf("%s: %w", o.Cluster.Name, err)
	}

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
	if err := t.forward(ctx, rc, o); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

// Close stops the port-forward and deletes the pod. It is idempotent, safe on
// a nil receiver, and safe for concurrent callers, because it is reached from
// a defer, from a signal, and from the defer that runs after the signal —
// those are separate goroutines, not separate sequential calls.
func (t *Tunnel) Close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(t.close)
}

// close is Close's body, run at most once via closeOnce.
func (t *Tunnel) close() {
	// Recorded before the forwarder is stopped, so the nil it returns on the
	// way out is attributed to this teardown rather than reported as a tunnel
	// that died: Done fires either way, and only Err can tell them apart.
	t.mu.Lock()
	t.closing = true
	t.mu.Unlock()

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
	// wait.Interrupted is true exactly for the timeout/cancellation cases —
	// the loop ran out of time or the caller's context was cancelled — and
	// false when the condition itself errored (a failed Get: a 403, a torn
	// connection). Discriminating on that, not on whether a Get ever
	// succeeded, is what keeps an API error from being reported as a timeout.
	if !wait.Interrupted(err) {
		return fmt.Errorf("waiting for tunnel pod %s/%s: %w", t.Namespace, t.PodName, err)
	}
	// wait.Interrupted is true for a cancelled caller too, and the poll runs
	// on its own derived deadline — so only the caller's context being
	// cancelled distinguishes Ctrl-C from a pod that really was too slow.
	// Reporting the first as the second sends the reader hunting a cluster
	// problem they do not have.
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("waiting for tunnel pod %s/%s: %w", t.Namespace, t.PodName, ctx.Err())
	}
	return fmt.Errorf("tunnel pod %s/%s did not become ready within %s (%w): %s",
		t.Namespace, t.PodName, timeout, err, NotReadyReason(last))
}

// forward brings the pod's Talos API port to localhost.
func (t *Tunnel) forward(ctx context.Context, rc *rest.Config, o Options) error {
	rt, upgrader, err := spdy.RoundTripperFor(rc)
	if err != nil {
		return fmt.Errorf("building the port-forward transport: %w", err)
	}
	u := t.clientset.CoreV1().RESTClient().Post().
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
	// done outlives this function: ForwardPorts blocks for the life of the
	// tunnel, and Done is what lets the caller notice when it stops.
	t.done = make(chan struct{})
	go func() {
		t.setForwardErr(pf.ForwardPorts())
		close(t.done)
	}()

	select {
	case <-ready:
	case <-t.done:
		return fmt.Errorf("port-forward to %s/%s failed: %w", t.Namespace, t.PodName, t.Err())
	case <-ctx.Done():
		return fmt.Errorf("port-forward to %s/%s: %w", t.Namespace, t.PodName, ctx.Err())
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
