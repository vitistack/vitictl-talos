package tunnel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// watchedTunnel is a forwarded tunnel as the watcher sees it: a stop channel
// the forwarder listens on and a done channel the forwarder closes. The fake
// forwarder here returns nil when stopped, as client-go's does.
func watchedTunnel(cs *fake.Clientset) (*Tunnel, chan struct{}) {
	tun := &Tunnel{
		PodName:    testPodName,
		Namespace:  testNamespace,
		clientset:  cs,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		watchEvery: 10 * time.Millisecond,
	}
	watcherDone := make(chan struct{})
	go func() {
		<-tun.stop
		tun.setForwardErr(nil)
		close(tun.done)
	}()
	go func() {
		tun.watchPod(context.Background(), cs)
		close(watcherDone)
	}()
	return tun, watcherDone
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not happen within 2s", what)
	}
}

// The live failure: a pod past its activeDeadlineSeconds is Failed, and the
// port-forward does not return on its own. The watcher must stop it and say
// why, naming the deadline so the reader knows which flag to raise.
func TestWatchPodEndsTheTunnelWhenThePodReachesItsDeadline(t *testing.T) {
	cs := fake.NewSimpleClientset(tunnelPod(corev1.PodRunning))
	tun, watcherDone := watchedTunnel(cs)

	failed := tunnelPod(corev1.PodFailed)
	failed.Status.Reason = "DeadlineExceeded"
	failed.Status.Message = "Pod was active on the node longer than the specified deadline"
	if _, err := cs.CoreV1().Pods(testNamespace).UpdateStatus(context.Background(), failed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	waitClosed(t, tun.Done(), "the tunnel ending")
	waitClosed(t, watcherDone, "the watcher returning")
	err := tun.Err()
	if !errors.Is(err, ErrPodEnded) {
		t.Fatalf("Err() = %v, want it to wrap ErrPodEnded", err)
	}
	for _, want := range []string{testPodName, "DeadlineExceeded", "--deadline"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Err() = %q, want it to mention %q", err, want)
		}
	}
}

// A pod deleted from under the tunnel — by a colleague's cleanup — ends it too.
func TestWatchPodEndsTheTunnelWhenThePodIsDeleted(t *testing.T) {
	cs := fake.NewSimpleClientset(tunnelPod(corev1.PodRunning))
	tun, _ := watchedTunnel(cs)

	if err := cs.CoreV1().Pods(testNamespace).Delete(context.Background(), testPodName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	waitClosed(t, tun.Done(), "the tunnel ending")
	if err := tun.Err(); !errors.Is(err, ErrPodEnded) || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("Err() = %v, want ErrPodEnded saying the pod was deleted", err)
	}
}

// One failed check is an API hiccup, not an ended pod: dropping a working
// tunnel for it would be worse than the bug this watcher fixes.
func TestWatchPodKeepsTheTunnelWhenACheckFails(t *testing.T) {
	cs := fake.NewSimpleClientset(tunnelPod(corev1.PodRunning))
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection reset by peer")
	})
	tun, _ := watchedTunnel(cs)

	select {
	case <-tun.Done():
		t.Fatalf("the tunnel ended on a failed check: %v", tun.Err())
	case <-time.After(150 * time.Millisecond):
	}
	tun.Close()
	waitClosed(t, tun.Done(), "the tunnel closing")
}

// A pod that is still running, ready or not, keeps its tunnel.
func TestWatchPodKeepsATunnelWhosePodRuns(t *testing.T) {
	cs := fake.NewSimpleClientset(tunnelPod(corev1.PodRunning))
	tun, watcherDone := watchedTunnel(cs)

	select {
	case <-tun.Done():
		t.Fatalf("the tunnel ended while its pod was running: %v", tun.Err())
	case <-time.After(100 * time.Millisecond):
	}
	tun.Close()
	waitClosed(t, watcherDone, "the watcher returning after Close")
	if err := tun.Err(); err != nil {
		t.Errorf("Err() = %v after Close, want nil: a teardown we asked for is not a failure", err)
	}
}

// Close deletes the pod, and the watcher may see that delete before it sees
// the forwarder stop. It must not then report our own teardown as an ended
// pod.
func TestWatchPodDoesNotReportOurOwnTeardown(t *testing.T) {
	cs := fake.NewSimpleClientset(tunnelPod(corev1.PodRunning))
	tun := &Tunnel{
		PodName: testPodName, Namespace: testNamespace, clientset: cs,
		stop: make(chan struct{}), done: make(chan struct{}), watchEvery: 10 * time.Millisecond,
	}
	tun.Close() // marks closing, stops the forwarder, deletes the pod
	go tun.watchPod(context.Background(), cs)
	time.Sleep(50 * time.Millisecond)
	tun.setForwardErr(nil)
	close(tun.done)
	if err := tun.Err(); err != nil {
		t.Errorf("Err() = %v, want nil: the pod is gone because we deleted it", err)
	}
}

// A cancelled caller context — Ctrl-C — stops the watcher without ending the
// tunnel on its own: the command's teardown does that.
func TestWatchPodStopsWithItsContext(t *testing.T) {
	cs := fake.NewSimpleClientset(tunnelPod(corev1.PodRunning))
	tun := &Tunnel{
		PodName: testPodName, Namespace: testNamespace, clientset: cs,
		stop: make(chan struct{}), done: make(chan struct{}), watchEvery: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		tun.watchPod(ctx, cs)
		close(returned)
	}()
	cancel()
	waitClosed(t, returned, "the watcher returning after its context was cancelled")
	if err := tun.Err(); err != nil {
		t.Errorf("Err() = %v, want nil: a cancelled caller is not an ended pod", err)
	}
}
