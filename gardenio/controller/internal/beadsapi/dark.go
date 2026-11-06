package beadsapi

import (
	"slices"
	"strings"
)

// DarkMarkerLabels are the bead labels that mark an agent (or gardener, or any
// bead) as belonging to the in-gardenio monorepo-dark universe. Matching any one
// is sufficient. Kept in sync with the convention used by live dark beads (bare
// `monorepo-dark`) and the canonical guard label (`dark:true`).
//
// This is the SINGLE source of truth for the dark discriminator. bridge's
// IsDarkGardener (dark-only MR-cap + per-agent token-budget controls) and the
// controller's dark-Jira write-token fence (ka-0000005242) both key off IsDark,
// so the two can never drift on what "dark" means.
var DarkMarkerLabels = []string{
	"monorepo-dark",
	"dark:true",
	"project:monorepo-dark",
}

// DarkProject is the project field value that marks a bead as dark. No live
// dark agent uses this yet (they stay project=gardenio + a `monorepo-dark`
// label), but accepting it keeps the discriminator correct if a dedicated dark
// project is ever split out.
const DarkProject = "monorepo-dark"

// MetaDarkPod is the agent-bead metadata key the controller stamps ("true")
// when the spawning agent belongs to the monorepo-dark universe (IsDark). The
// pod-spec builder reads it to fence the privileged Jira write token out of
// dark pods and to label them for the dark-agent egress deny (ka-0000005242).
//
// It lives here — in the lowest package — so both stamp sites can share it
// without an import cycle: the events path stamps it in subscriber.buildEvent,
// the reconciler path stamps it in ListAgentBeads. It rides in the metadata map
// (not a dedicated struct field) so it flows through both spec builders with no
// signature change.
const MetaDarkPod = "dark_pod"

// EnvGardenioDark is the pod environment variable the controller sets ("true")
// inside every dark agent pod. It is the in-pod signal the gb CLI reads to
// construct a READ-ONLY Jira client — the write methods fail closed — as the
// client-layer leg of the dark-Jira write-token fence (ka-0000005242),
// belt-and-suspenders over the fenced JIRA_API_TOKEN and the denied Atlassian
// egress.
const EnvGardenioDark = "GARDENIO_DARK"

// IsDark reports whether a bead's labels / project mark it as belonging to the
// monorepo-dark universe. It is intentionally permissive on the OR side (any
// dark signal ⇒ dark) so a dark agent can never dodge the dark-only controls —
// including the write-token fence — by carrying only one of the dark markers.
func IsDark(labels []string, project string) bool {
	if strings.TrimSpace(project) == DarkProject {
		return true
	}
	for _, l := range labels {
		if slices.Contains(DarkMarkerLabels, l) {
			return true
		}
	}
	return false
}
