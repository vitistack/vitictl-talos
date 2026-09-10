package tunnel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vitistack/vitictl-talos/internal/config"
)

const (
	testNamespace = "viti-center"
	testPodName   = "talos-tunnel-3f9a2c1d"
)

// tunnelPod is a tunnel pod as the apiserver would hand it back, in whatever
// phase the test needs.
func tunnelPod(phase corev1.PodPhase) *corev1.Pod {
	p := Pod(testPodName, testNamespace, DefaultImage, "100.64.0.4", "admin@pos1-kv-cl01", 8*time.Hour)
	p.Status.Phase = phase
	return p
}

// 50000 is frequently already bound — by a talosctl of your own, or a second
// tunnel — so the default asks the kernel for a free port instead of turning a
// second window into a confusing failure.
func TestPortSpecDefaultsToAnEphemeralLocalPort(t *testing.T) {
	if got := PortSpec(0); got != "0:50000" {
		t.Errorf("PortSpec(0) = %q, want 0:50000", got)
	}
	if got := PortSpec(50000); got != "50000:50000" {
		t.Errorf("PortSpec(50000) = %q, want 50000:50000", got)
	}
}

// "timed out waiting for the pod" is not a diagnosis. In an air-gapped zone
// the first failure will be an image pull, and the pod already knows that.
func TestNotReadyReasonReportsTheContainerState(t *testing.T) {
	p := &corev1.Pod{}
	p.Status.Phase = corev1.PodPending
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerName,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  "ImagePullBackOff",
			Message: "Back-off pulling image \"alpine/socat\"",
		}},
	}}
	got := NotReadyReason(p)
	for _, want := range []string{"Pending", "ImagePullBackOff", "Back-off pulling image"} {
		if !strings.Contains(got, want) {
			t.Errorf("NotReadyReason() = %q, does not mention %q", got, want)
		}
	}
}

func TestNotReadyReasonHandlesAPodThatWasNeverFetched(t *testing.T) {
	if got := NotReadyReason(nil); got == "" {
		t.Error("NotReadyReason(nil) = \"\", want something readable")
	}
}

// Close runs from a defer, from a signal handler, and again from the deferred
// call after the signal handler — it must survive all three.
func TestCloseIsIdempotentOnAnUnopenedTunnel(t *testing.T) {
	var tun *Tunnel
	tun.Close()

	tun = &Tunnel{}
	tun.Close()
	tun.Close()
}

// Close is reached from a defer, a signal handler, and the defer that runs
// after the signal — those are separate goroutines. This exercises exactly
// that: many callers racing to close the same tunnel. Run with -race; before
// the sync.Once fix, two goroutines could both observe stop != nil and both
// close() it, panicking with "close of closed channel".
func TestCloseIsSafeForConcurrentCallers(t *testing.T) {
	tun := &Tunnel{stop: make(chan struct{})}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tun.Close()
		}()
	}
	wg.Wait()
}

func TestOptionsApplyDefaults(t *testing.T) {
	var o Options
	if err := o.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults() on a zero Options: %v", err)
	}
	if o.Image != DefaultImage {
		t.Errorf("Image = %q, want %q", o.Image, DefaultImage)
	}
	if o.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", o.Timeout, DefaultTimeout)
	}
	if o.Deadline != DefaultDeadline {
		t.Errorf("Deadline = %v, want %v", o.Deadline, DefaultDeadline)
	}
}

// A deadline below the readiness timeout kills the pod before it can be used,
// and one under a second rounds down to activeDeadlineSeconds: 0, which the
// apiserver refuses with a message about a field this tool set rather than
// about the flag that set it. Both are worth catching here, where the message
// can name the flags.
func TestOptionsRejectADeadlineThatCannotWork(t *testing.T) {
	for name, o := range map[string]Options{
		"shorter than the timeout": {Timeout: time.Minute, Deadline: 10 * time.Second},
		"rounds down to zero":      {Timeout: 100 * time.Millisecond, Deadline: 200 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			err := o.applyDefaults()
			if err == nil {
				t.Fatal("applyDefaults() accepted the deadline, want an error")
			}
			if !strings.Contains(err.Error(), "deadline") {
				t.Errorf("error %q does not name the deadline", err)
			}
		})
	}
	// The boundary is allowed: a deadline equal to the timeout leaves the pod
	// exactly as long as it is given to become ready.
	o := Options{Timeout: time.Minute, Deadline: time.Minute}
	if err := o.applyDefaults(); err != nil {
		t.Errorf("applyDefaults() rejected deadline == timeout: %v", err)
	}
}

// client-go's forwarder returns nil both when it is stopped and when the
// stream is lost, so Err is the only thing that can tell "you pressed Ctrl-C"
// from "the pod is gone" — and getting that backwards is what makes a dead
// tunnel look like a healthy one.
func TestErrDistinguishesALostTunnelFromATeardown(t *testing.T) {
	lost := &Tunnel{done: make(chan struct{})}
	lost.setForwardErr(nil)
	if !errors.Is(lost.Err(), ErrLostConnection) {
		t.Errorf("Err() = %v, want it to wrap ErrLostConnection", lost.Err())
	}

	torn := &Tunnel{stop: make(chan struct{}), done: make(chan struct{})}
	torn.Close()
	torn.setForwardErr(nil)
	if torn.Err() != nil {
		t.Errorf("Err() = %v after Close, want nil: a teardown we asked for is not a failure", torn.Err())
	}
}

// A tunnel that was never forwarded has nothing that can stop, and a nil
// channel is what makes "case <-tun.Done():" block instead of firing at once.
func TestDoneOnATunnelThatWasNeverForwarded(t *testing.T) {
	var tun *Tunnel
	if tun.Done() != nil {
		t.Error("Done() on a nil Tunnel is not nil")
	}
	if (&Tunnel{}).Done() != nil {
		t.Error("Done() on an unforwarded Tunnel is not nil")
	}
}

// "timed out waiting for the pod" is not a diagnosis, so the timeout has to
// carry the pod's own account of why it is not ready.
func TestWaitReadyReportsWhyThePodIsNotReady(t *testing.T) {
	pod := tunnelPod(corev1.PodPending)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerName,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ImagePullBackOff",
		}},
	}}
	tun := &Tunnel{PodName: testPodName, Namespace: testNamespace, clientset: fake.NewClientset(pod)}

	err := tun.waitReady(context.Background(), 50*time.Millisecond)
	if err == nil {
		t.Fatal("waitReady() succeeded on a Pending pod, want a timeout")
	}
	for _, want := range []string{"did not become ready", testPodName, "ImagePullBackOff"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A 403 on the Get is not the cluster being slow, and reporting it as a
// timeout sends the reader off to wait longer for an answer that will never
// come. This is what discriminating on wait.Interrupted buys.
func TestWaitReadyReportsAnAPIErrorAsAnAPIError(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, testPodName, errors.New("no access to viti-center"))
	})
	tun := &Tunnel{PodName: testPodName, Namespace: testNamespace, clientset: cs}

	err := tun.waitReady(context.Background(), time.Minute)
	if err == nil {
		t.Fatal("waitReady() succeeded despite a forbidden Get, want an error")
	}
	if strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("error %q reports an API failure as a timeout", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error %q does not say the Get was refused", err)
	}
}

// Ctrl-C while the image pulls is not the cluster being slow either, and
// wait.Interrupted alone cannot tell the two apart.
func TestWaitReadyReportsCancellationAsCancellation(t *testing.T) {
	tun := &Tunnel{
		PodName:   testPodName,
		Namespace: testNamespace,
		clientset: fake.NewClientset(tunnelPod(corev1.PodPending)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := tun.waitReady(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReady() = %v, want it to wrap context.Canceled", err)
	}
	if strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("error %q blames the cluster for a cancelled caller", err)
	}
}

// The pod exists only for this session, and a graceful shutdown would leave
// it lingering in Terminating while the operator waits.
func TestCloseDeletesThePodImmediately(t *testing.T) {
	cs := fake.NewClientset(tunnelPod(corev1.PodRunning))
	var grace *int64
	var deleted bool
	cs.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		d, ok := a.(k8stesting.DeleteActionImpl)
		if !ok {
			return false, nil, nil
		}
		deleted, grace = true, d.DeleteOptions.GracePeriodSeconds
		return true, nil, nil
	})
	tun := &Tunnel{PodName: testPodName, Namespace: testNamespace, clientset: cs, stop: make(chan struct{})}

	tun.Close()

	if !deleted {
		t.Fatal("Close() did not delete the tunnel pod")
	}
	if grace == nil || *grace != 0 {
		t.Errorf("GracePeriodSeconds = %v, want 0", grace)
	}
}

// An undeleted pod is somebody's cluster carrying this tool's litter, so the
// warning has to hand over the command that removes it.
func TestCloseWarnsWithTheCleanupCommandWhenTheDeleteFails(t *testing.T) {
	cs := fake.NewClientset(tunnelPod(corev1.PodRunning))
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd is unhappy"))
	})
	var warned []string
	tun := &Tunnel{
		PodName:   testPodName,
		Namespace: testNamespace,
		clientset: cs,
		stop:      make(chan struct{}),
		warn:      func(e error) { warned = append(warned, e.Error()) },
	}

	tun.Close()

	if len(warned) != 1 {
		t.Fatalf("Close() warned %d times, want 1: %v", len(warned), warned)
	}
	for _, want := range []string{"kubectl -n " + testNamespace + " delete pod " + testPodName, "etcd is unhappy"} {
		if !strings.Contains(warned[0], want) {
			t.Errorf("warning %q does not mention %q", warned[0], want)
		}
	}
}

// A pod that is already gone is the outcome Close wanted, not a problem.
func TestCloseIsQuietWhenThePodIsAlreadyGone(t *testing.T) {
	tun := &Tunnel{
		PodName:   testPodName,
		Namespace: testNamespace,
		clientset: fake.NewClientset(),
		stop:      make(chan struct{}),
		warn:      func(error) { t.Error("Close() warned about a pod that was already deleted") },
	}
	tun.Close()
}

// A colleague may be holding the other pod open right now, so it is reported
// rather than deleted — and the report has to be actionable on its own.
func TestWarnAboutOrphansNamesThePodAndTheCleanupCommand(t *testing.T) {
	var warned []string
	o := Options{
		Cluster: config.TunnelCluster{Name: "admin@pos1-kv-cl01", Namespace: testNamespace},
		Warn:    func(e error) { warned = append(warned, e.Error()) },
	}
	cs := fake.NewClientset(tunnelPod(corev1.PodRunning))

	warnAboutOrphans(context.Background(), cs, o)

	if len(warned) != 1 {
		t.Fatalf("warnAboutOrphans warned %d times, want 1: %v", len(warned), warned)
	}
	for _, want := range []string{testPodName, "kubectl -n " + testNamespace + " delete pod -l " + ManagedBySelector} {
		if !strings.Contains(warned[0], want) {
			t.Errorf("warning %q does not mention %q", warned[0], want)
		}
	}
}

// An empty namespace is the common case and must stay silent, or every tunnel
// would open under a warning.
func TestWarnAboutOrphansIsQuietWithNoTunnelPods(t *testing.T) {
	o := Options{
		Cluster: config.TunnelCluster{Name: "admin@pos1-kv-cl01", Namespace: testNamespace},
		Warn:    func(error) { t.Error("warnAboutOrphans warned with no tunnel pods in the namespace") },
	}
	// A pod in the namespace that this tool did not create must not match:
	// the label selector is the whole point.
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: testNamespace}}
	warnAboutOrphans(context.Background(), fake.NewClientset(other), o)
}

// podReady gates the whole readiness wait, so each way of not being ready has
// to be a "no" rather than a panic or a false "yes".
func TestPodReady(t *testing.T) {
	running := tunnelPod(corev1.PodRunning)
	running.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	notReady := tunnelPod(corev1.PodRunning)
	notReady.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse},
	}
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want bool
	}{
		"never fetched":              {nil, false},
		"still pending":              {tunnelPod(corev1.PodPending), false},
		"running with no condition":  {tunnelPod(corev1.PodRunning), false},
		"running but not ready":      {notReady, false},
		"running and ready":          {running, true},
		"succeeded after a deadline": {tunnelPod(corev1.PodSucceeded), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := podReady(tc.pod); got != tc.want {
				t.Errorf("podReady() = %v, want %v", got, tc.want)
			}
		})
	}
}
