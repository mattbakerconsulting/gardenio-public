package beadsapi

// plan_phase.go — the ONE shared /plans phase-membership predicate.
//
// A /plans plan is an epic; a plan PHASE is a direct-child container of that
// plan epic whose id is listed in the plan's fields.phase_order (membership +
// display order; design ka-0000001034). Two bead types render as a phase:
//
//   - type=epic     — a hand-authored / "+ New phase" sub-epic.
//   - type=molecule — a formula poured into the plan (epic ka-0000000759). The
//     poured container stays type=molecule to keep its /molecule execution view,
//     kd mol verbs, wisp GC, dedup guard, and formula lineage (resolved decision
//     ka-0000003759), so every phase-membership scan must accept a molecule
//     wherever it accepted an epic.
//
// IsPhaseContainer is that single membership test. It lives in beadsapi — the
// one package both the viewer (cmd/beads-viewer) and the CLI (cmd/gb) import and
// the owner of *BeadDetail — so the viewer's cockpit and `gb plan show` resolve
// phase membership through the SAME predicate and can never diverge on whether a
// poured molecule is a phase (the divergence ka-0000004781 closes: the CLI's
// ported resolvePlanPhaseIDs kept a raw `Type != "epic"` check that silently
// dropped poured molecule phases the cockpit rendered).
//
// A nil bead (a stale/deleted phase_order id) is never a container. This is a
// MEMBERSHIP test only — it does NOT match the plan epic itself: a plan's own
// `Type == "epic"` root gate (and the add-step phase-TARGET resolvers, which
// deliberately reject a molecule target) are distinct checks and stay as they
// are.
func IsPhaseContainer(c *BeadDetail) bool {
	return c != nil && (c.Type == "epic" || c.Type == "molecule")
}
