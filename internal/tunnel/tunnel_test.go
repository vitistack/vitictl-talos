package tunnel

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

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

func TestOptionsApplyDefaults(t *testing.T) {
	var o Options
	o.applyDefaults()
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
