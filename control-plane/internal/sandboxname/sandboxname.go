// Package sandboxname owns Docker/Podman-safe sandbox container references.
package sandboxname

import "strings"

const prefix = "s-"

// Container returns the canonical container name for a public sandbox ID.
// Public IDs remain uppercase ULIDs; names are lowercased for Podman compatibility.
func Container(id string) string {
	return prefix + strings.ToLower(id)
}

// Reference prefers a persisted container ID, which keeps operations compatible
// with containers created before canonical names were introduced.
func Reference(id, containerID string) string {
	if containerID != "" {
		return containerID
	}
	return Container(id)
}

// IDFromContainer converts a canonical sandbox container name back to the
// uppercase identifier used by persisted sandbox rows.
func IDFromContainer(name string) (string, bool) {
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
		return "", false
	}
	return strings.ToUpper(strings.TrimPrefix(name, prefix)), true
}
