package server

import (
	"slices"

	"gardenio/kbeads/internal/model"
)

// isDeliverableType reports whether t is a published-deliverable/artifact type:
// a finished, human-facing output rather than claimable work. The category
// trigger (derive_bead_category, migration 043) classifies these `artifact` so
// they never leak into the `gb ready` leaf-work default or the operator inbox.
// Keep in sync with that trigger and with readyDeliverableTypes in
// gardenio/controller/cmd/gb/ready.go (which the artifact category is intended
// to retire).
func isDeliverableType(t model.BeadType) bool {
	switch t {
	case model.TypeBundle, model.TypeRichReport, "dashboard", "mr", "merge-request":
		return true
	}
	return false
}

func hasLabel(labels []string, want string) bool {
	return slices.Contains(labels, want)
}
