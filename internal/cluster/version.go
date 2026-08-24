package cluster

import "regexp"

// A Talos cluster reports its version from three places, and they routinely
// disagree. Which one a command reads decides whether it is looking at
// intent or at reality:
//
//   - Machine spec.os.imageID — the factory image URL the operator drives
//     provisioning and upgrades from, e.g.
//     https://factory.talos.dev/image/<schematic>/v1.13.7/nocloud-amd64.iso
//     This is DECLARED (desired) state. The operator bumps it to the newest
//     available image ahead of any actual upgrade, so it runs ahead of the
//     nodes — vitictl's own notes record it observed 112 days ahead.
//   - machine.install.image on the node — what the config says to install,
//     reconciled from the cluster's pinned install_image. Also desired state,
//     and it goes stale the moment a pin moves without the nodes following.
//   - Node status.nodeInfo.osImage in the guest cluster, e.g.
//     "Talos (v1.13.7)" — what the kubelet reports, which is the RUNNING
//     version and the only one of the three that is not a wish.
//
// Anything deciding whether a node needs an upgrade has to read the third.
var (
	imageIDVersionRe     = regexp.MustCompile(`/v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)/`)
	osImageVersionRe     = regexp.MustCompile(`(?i)talos.*?v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)`)
	enforcementVersionRe = regexp.MustCompile(`(?i)all nodes run talos v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)`)
)

// VersionFromImageID extracts the declared Talos version from a Machine's
// spec.os.imageID factory URL. Returns "" when no version is recognisable.
func VersionFromImageID(imageID string) string {
	return firstSubmatch(imageIDVersionRe, imageID)
}

// VersionFromOSImage extracts the running Talos version from a Node's
// status.nodeInfo.osImage. Returns "" for a non-Talos or unrecognisable image.
func VersionFromOSImage(osImage string) string {
	return firstSubmatch(osImageVersionRe, osImage)
}

// VersionFromEnforcement extracts the operator-verified running version from a
// TalosVersionEnforcement condition message ("All nodes run Talos v1.12.7").
//
// It is the cheapest truthful source there is — a condition on the
// KubernetesCluster the caller already holds, no second cluster to reach — but
// it is cluster-wide and only speaks when every node agrees. Mid-upgrade, or
// when the operator publishes no such condition, the message has a different
// shape and this returns "".
func VersionFromEnforcement(message string) string {
	return firstSubmatch(enforcementVersionRe, message)
}

func firstSubmatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
