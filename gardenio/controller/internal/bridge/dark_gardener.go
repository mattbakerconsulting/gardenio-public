// Package bridge — monorepo-dark gardener discriminator.
//
// The bounded-stop guard is now GLOBAL (gardener_bounds_guard.go,
// ka-0000006744): every gardener — dark or not — is bounded the same way, so
// the dark-only bounded-stop fail-closed guard (and the dark-only
// `dark-gardeners-pause` kill-switch) it superseded are gone. The operator
// declined that dark special-casing as the wrong axis (decision
// ka-0000003439).
//
// What survives here is ONLY the dark identity discriminator, retained because
// two ORTHOGONAL dark blast-radius controls that postdate that MR still key on
// it — neither is about bounded-stop, so neither was in scope of the operator's
// objection:
//
//   - dark_mr_cap.go (R6-FU6c, ka-0000002822) — caps the MR blast radius of the
//     aggressively-merging dark universe (fleet + per-gardener), and
//   - dark_agent_budget.go (R6-FU6c, ka-0000001323) — caps per-agent/day token
//     spend for dark agents.
//
// Both are genuinely dark-specific (they exist because the dark universe merges
// continuously and far ahead of upstream) and apply only to dark gardeners —
// non-dark gardeners never reach them. `IsDarkGardener` is their shared
// discriminator.
package bridge

import (
	"gardenio/controller/internal/beadsapi"
)

// darkGardenerLabels are the labels that mark a gardener (or any bead) as
// belonging to the in-gardenio monorepo-dark universe. Aliased to the canonical
// beadsapi.DarkMarkerLabels so the discriminator lives in exactly one place —
// see beadsapi/dark.go. Retained here because a few in-package call sites
// (dark_mr_cap.go) iterate it directly.
var darkGardenerLabels = beadsapi.DarkMarkerLabels

// IsDarkGardener reports whether a bead's labels / project mark it as a
// monorepo-dark gardener subject to the dark-only blast-radius controls (MR
// caps + per-agent token budget). It is intentionally permissive on the OR
// side (any dark signal ⇒ dark) so a gardener can never dodge those controls by
// carrying only one of the dark markers. Delegates to the canonical
// beadsapi.IsDark so the dark-only controls and the dark-Jira write-token fence
// (ka-0000005242) can never disagree on what "dark" means.
func IsDarkGardener(labels []string, project string) bool {
	return beadsapi.IsDark(labels, project)
}
