package cluster

import "regexp"

// The declared Talos version lives in a Machine's spec.os.imageID — the
// factory image URL the operator drives provisioning and upgrades from, e.g.
// https://factory.talos.dev/image/<schematic>/v1.13.7/nocloud-amd64.iso
//
// It is the DESIRED version, not necessarily the running one: the operator
// bumps it to the newest available image ahead of any actual upgrade. What a
// node actually runs is what `viti talos version` asks the node itself.
var imageIDVersionRe = regexp.MustCompile(`/v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)/`)

// VersionFromImageID extracts the Talos version from a factory image URL.
// Returns "" when no version is recognisable.
func VersionFromImageID(imageID string) string {
	m := imageIDVersionRe.FindStringSubmatch(imageID)
	if m == nil {
		return ""
	}
	return m[1]
}
