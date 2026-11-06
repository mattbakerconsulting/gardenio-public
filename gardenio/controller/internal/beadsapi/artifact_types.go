package beadsapi

import "strings"

// ArtifactTypes is the single source of truth for the artifact_type values a
// decision option may declare. It was previously copy-pasted in three places
// that drifted (the `gb decision` allowlist, the Slack "Custom Response" modal
// dropdown, and two error strings) — export it once so every surface reads the
// same ordered list. Order is display order (the modal dropdown and help text
// follow it).
//
//   - report        flat single-bead report (the historical default)
//   - plan          a plan document
//   - checklist     an actionable checklist
//   - diff-summary  a summary of a diff / change
//   - epic          an epic (multi-task) breakdown
//   - bug           a filed bug
//   - tldr          a length-bounded one-paragraph summary
//   - bundle        a multi-doc bundle deliverable (ka-0000007047)
var ArtifactTypes = []string{
	"report", "plan", "checklist", "diff-summary", "epic", "bug", "tldr", "bundle",
}

// validArtifactTypeSet is ArtifactTypes as a set, built once for O(1) lookups.
var validArtifactTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(ArtifactTypes))
	for _, at := range ArtifactTypes {
		m[at] = true
	}
	return m
}()

// IsValidArtifactType reports whether at is a recognized artifact_type.
func IsValidArtifactType(at string) bool {
	return validArtifactTypeSet[at]
}

// ArtifactTypesList renders ArtifactTypes as a comma-separated string for use
// in help text and error messages (e.g. "report, plan, ..., bundle").
func ArtifactTypesList() string {
	return strings.Join(ArtifactTypes, ", ")
}
