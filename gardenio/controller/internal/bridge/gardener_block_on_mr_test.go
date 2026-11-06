package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mrBead builds a type=merge-request BeadDetail with the given mr_outcome.
func mrBead(id, outcome string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "merge-request",
		Status: "open",
		Fields: map[string]string{"mr_outcome": outcome},
	}
}

// blockOnMRStop is a bounded stop blob carrying a declared mr_merged loop_end
// entry scoped to the given parent epic, plus the mandatory hard_stop wall_clock
// so it passes the bounded-gardener guard if that ever runs.
func blockOnMRStop(parent string) json.RawMessage {
	return json.RawMessage(`{"loop_end":{"any":[{"kind":"mr_merged","parent":"` + parent + `"}]},` +
		`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`)
}

// TestGardener_BlockOnMR_UnmergedMR_GoesDormant: a gardener that DECLARES
// {"kind":"mr_merged","parent":<epic>} and whose tracked MR under that epic is
// not yet merged parks dormant — it does NOT spawn, does NOT close, and records
// DormantBlockerKind="mr_merged" so the wake predicate re-checks the MRs. This
// is the first-class "block until MR merged" facility (ka-0000007387).
func TestGardener_BlockOnMR_UnmergedMR_GoesDormant(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		Stop:        blockOnMRStop("gd-epic-1"),
	})
	// Open epic with one open child that produces an UNMERGED MR.
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "green MR, awaiting human merge"},
	)
	daemon.parentOf = map[string]string{"gd-child-1": "gd-epic-1"}
	daemon.mrBeadsByTask = map[string]*beadsapi.BeadDetail{
		"gd-child-1": mrBead("gd-mr-1", "open"),
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawns := len(daemon.spawnedNames)
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()
	if spawns != 0 {
		t.Errorf("gardener blocked on unmerged MR must not spawn, got %d", spawns)
	}
	if closed {
		t.Errorf("gardener blocked on unmerged MR must park, not close; got closedIDs=%v", daemon.closedIDs)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if !cap.DormantOnEscalation {
		t.Errorf("expected DormantOnEscalation=true, got %+v", cap)
	}
	if cap.DormantBlockerKind != gardenerDormantKindMRMerged {
		t.Errorf("expected DormantBlockerKind=%q, got %q", gardenerDormantKindMRMerged, cap.DormantBlockerKind)
	}
	if cap.DormantBlocker != "gd-epic-1" {
		t.Errorf("expected DormantBlocker=gd-epic-1 (the tracked-MR parent), got %q", cap.DormantBlocker)
	}
}

// TestGardener_BlockOnMR_AllMerged_DoesNotPark: with the declared mr_merged entry
// but every tracked MR merged, the gardener is NOT parked by the block-on-MR gate
// — it proceeds to normal firing. (Zero tracked unmerged MRs ⇒ nothing to sleep
// on.)
func TestGardener_BlockOnMR_AllMerged_DoesNotPark(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		Stop:        blockOnMRStop("gd-epic-1"),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "merged"},
	)
	daemon.parentOf = map[string]string{"gd-child-1": "gd-epic-1"}
	daemon.mrBeadsByTask = map[string]*beadsapi.BeadDetail{
		"gd-child-1": mrBead("gd-mr-1", "merged"),
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("all-MRs-merged gardener must proceed to fire, got %d spawns", len(daemon.spawnedNames))
	}
}

// TestGardener_BlockOnMR_NotDeclared_DoesNotPark: a gardener that does NOT declare
// mr_merged is never parked by the block-on-MR gate even if unmerged MRs exist —
// the facility is strictly opt-in.
func TestGardener_BlockOnMR_NotDeclared_DoesNotPark(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		AssignBead:  "gd-epic-1",
		// No mr_merged entry — a plain bounded stop.
		Stop: json.RawMessage(`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "unmerged"},
	)
	daemon.parentOf = map[string]string{"gd-child-1": "gd-epic-1"}
	daemon.mrBeadsByTask = map[string]*beadsapi.BeadDetail{
		"gd-child-1": mrBead("gd-mr-1", "open"),
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("non-declaring gardener must fire normally, got %d spawns", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("non-declaring gardener must not be parked/closed by block-on-MR gate, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_BlockOnMR_ClosedParent_DoesNotPark: a declared mr_merged gardener
// whose parent epic is CLOSED does not park — the plan is done, there is nothing
// to wait on. (Fail-safe: a closed parent yields zero tracked unmerged MRs.)
func TestGardener_BlockOnMR_ClosedParent_DoesNotPark(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		Stop:        blockOnMRStop("gd-epic-1"),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "closed", Title: "done epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "orphan unmerged"},
	)
	daemon.parentOf = map[string]string{"gd-child-1": "gd-epic-1"}
	daemon.mrBeadsByTask = map[string]*beadsapi.BeadDetail{
		"gd-child-1": mrBead("gd-mr-1", "open"),
	}

	engine.Evaluate(context.Background())

	// The definitive assertion: the block-on-MR gate must not have parked it.
	cap := lastGardenerOrZero(daemon, "gd-cap-1")
	if cap.DormantBlockerKind == gardenerDormantKindMRMerged {
		t.Errorf("closed-parent gardener must not park on block-on-MR, got DormantBlockerKind=%q", cap.DormantBlockerKind)
	}
}

// TestGardener_BlockOnMR_DormantWakesWhenAllMerged: an already-dormant
// mr_merged-kind gardener wakes once every tracked MR under its recorded parent is
// merged — DormantOnEscalation clears and history resets.
func TestGardener_BlockOnMR_DormantWakesWhenAllMerged(t *testing.T) {
	staleProgress := `{"runs":[{"agent_id":"a","progress_score":0}]}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:              50,
		RunsSpawned:            5,
		ResumesWithoutProgress: 3,
		Stop:                   blockOnMRStop("gd-epic-1"),
		DormantOnEscalation:    true,
		DormantBlocker:         "gd-epic-1",
		DormantBlockerKind:     gardenerDormantKindMRMerged,
		ProgressState:          json.RawMessage(staleProgress),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "now merged"},
	)
	daemon.parentOf = map[string]string{"gd-child-1": "gd-epic-1"}
	daemon.mrBeadsByTask = map[string]*beadsapi.BeadDetail{
		"gd-child-1": mrBead("gd-mr-1", "merged"),
	}

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.DormantOnEscalation {
		t.Errorf("all MRs merged — gardener must wake (DormantOnEscalation=false), got true")
	}
	if cap.DormantBlockerKind != "" {
		t.Errorf("expected DormantBlockerKind cleared on wake, got %q", cap.DormantBlockerKind)
	}
	if cap.ResumesWithoutProgress != 0 {
		t.Errorf("expected ResumesWithoutProgress reset to 0 on wake, got %d", cap.ResumesWithoutProgress)
	}
}

// TestGardener_BlockOnMR_DormantStaysWhileUnmerged: an already-dormant
// mr_merged-kind gardener whose tracked MR is still unmerged stays parked
// silently — no spawn, no close, no gardener-block churn.
func TestGardener_BlockOnMR_DormantStaysWhileUnmerged(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:           50,
		RunsSpawned:         5,
		Stop:                blockOnMRStop("gd-epic-1"),
		DormantOnEscalation: true,
		DormantBlocker:      "gd-epic-1",
		DormantBlockerKind:  gardenerDormantKindMRMerged,
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "still unmerged"},
	)
	daemon.parentOf = map[string]string{"gd-child-1": "gd-epic-1"}
	daemon.mrBeadsByTask = map[string]*beadsapi.BeadDetail{
		"gd-child-1": mrBead("gd-mr-1", "open"),
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("still-blocked dormant gardener must not spawn, got %d", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("still-blocked dormant gardener must not close, got %v", daemon.closedIDs)
	}
	for _, id := range daemon.updatedIDs {
		if id == "gd-cap-1" {
			t.Errorf("still-blocked dormant gardener must not churn its gardener block, got update for %s", id)
		}
	}
}

// ── Prong 2: proactive operator-wedge dormancy ──────────────────────────────

// TestGardener_ProactiveWedge_ZeroActionable_GoesDormant is the reachability fix
// (ka-0000007387): a gardener whose entire OPEN frontier is single-threaded
// behind an open operator escalation (every open child is blocked_by it, so
// actionable-count is 0) parks proactively — BEFORE any resume-budget
// bookkeeping — instead of spawning a pod that would re-derive "nothing to do."
func TestGardener_ProactiveWedge_ZeroActionable_GoesDormant(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		AssignBead:  "gd-epic-1",
		Stop:        json.RawMessage(`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`),
	})
	// Open epic; one open child, blocked_by an open escalation ⇒ actionable 0.
	// The child carries project:monorepo so it passes the now project-scoped
	// actionable-count bead_query (ka-0000003939) and is then subtracted by the
	// exclude_blocked_by_open_types filter — exercising the exclusion path, not
	// being silently dropped by the project scope.
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "blocked child", Labels: []string{"project:monorepo"}},
		&beadsapi.BeadDetail{ID: "gd-esc-1", Type: "escalation", Status: "open", Title: "operator: merge MRs"},
	)
	// Escalation lives under the epic (gardenerFrontierBlocker queries
	// escalations parented under the frontier epic); the actionable-count query
	// filters to task/bug/feature/epic types, so the escalation is not counted
	// as a child there.
	daemon.parentOf = map[string]string{"gd-child-1": "gd-epic-1", "gd-esc-1": "gd-epic-1"}
	// Declare the blocking edge child --blocks--> escalation so the
	// exclude_blocked_by_open_types filter proves the child externally blocked.
	daemon.addedDeps = []triggerMockDep{
		{BeadID: "gd-child-1", DependsOnID: "gd-esc-1", DepType: "blocks"},
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawns := len(daemon.spawnedNames)
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()
	if spawns != 0 {
		t.Errorf("fully-wedged gardener must not spawn, got %d", spawns)
	}
	if closed {
		t.Errorf("fully-wedged gardener must park, not close; got closedIDs=%v", daemon.closedIDs)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.DormantBlockerKind != gardenerDormantKindOperatorWedge {
		t.Errorf("expected DormantBlockerKind=%q, got %q", gardenerDormantKindOperatorWedge, cap.DormantBlockerKind)
	}
	if cap.DormantBlocker != "gd-esc-1" {
		t.Errorf("expected DormantBlocker=gd-esc-1, got %q", cap.DormantBlocker)
	}
}

// TestGardener_ProactiveWedge_ActionableChild_DoesNotPark: when the frontier has
// an actionable (not-blocked) open child, the gardener is NOT wedged and fires
// normally — the gate must never park a gardener with real work available.
func TestGardener_ProactiveWedge_ActionableChild_DoesNotPark(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		AssignBead:  "gd-epic-1",
		Stop:        json.RawMessage(`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`),
	})
	// One blocked child AND one actionable child ⇒ actionable-count 1 ⇒ not wedged.
	// Children carry project:monorepo (matching the trigger's lane) so the now
	// project-scoped actionable-count bead_query (ka-0000003939) counts them.
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "blocked", Labels: []string{"project:monorepo"}},
		&beadsapi.BeadDetail{ID: "gd-child-2", Type: "task", Status: "open", Title: "actionable", Labels: []string{"project:monorepo"}},
		&beadsapi.BeadDetail{ID: "gd-esc-1", Type: "escalation", Status: "open", Title: "operator: merge MRs"},
	)
	daemon.parentOf = map[string]string{
		"gd-child-1": "gd-epic-1", "gd-child-2": "gd-epic-1", "gd-esc-1": "gd-epic-1",
	}
	daemon.addedDeps = []triggerMockDep{
		{BeadID: "gd-child-1", DependsOnID: "gd-esc-1", DepType: "blocks"},
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawns := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if spawns != 1 {
		t.Errorf("gardener with an actionable child must fire, got %d spawns", spawns)
	}
	cap := lastGardenerOrZero(daemon, "gd-cap-1")
	if cap.DormantBlockerKind == gardenerDormantKindOperatorWedge {
		t.Errorf("gardener with an actionable child must not park on operator-wedge, got kind=%q", cap.DormantBlockerKind)
	}
}

// TestGardener_ProactiveWedge_NoOpenBlocker_DoesNotPark: an idle gardener (no open
// escalation/decision on its frontier) is NOT operator-wedged even if it has no
// open children — "idle" routes through the normal stop/resume gates, not the
// wedge park. Guards against over-parking a merely-quiet gardener.
func TestGardener_ProactiveWedge_NoOpenBlocker_DoesNotPark(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		AssignBead:  "gd-epic-1",
		Stop:        json.RawMessage(`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`),
	})
	// Open epic, no open escalation/decision. (No children at all.)
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
	)
	daemon.parentOf = map[string]string{} // opt into parent filtering (empty subtree)

	engine.Evaluate(context.Background())

	cap := lastGardenerOrZero(daemon, "gd-cap-1")
	if cap.DormantBlockerKind == gardenerDormantKindOperatorWedge {
		t.Errorf("idle gardener (no open blocker) must not park on operator-wedge, got kind=%q", cap.DormantBlockerKind)
	}
}

// TestGardener_BlockOnMR_DefaultedParent_GoesDormant: a declared mr_merged entry
// with NO explicit parent scopes tracked MRs to the gardener's frontier epic
// (AssignBead). An unmerged MR there parks the gardener, and DormantBlocker is
// stamped with the resolved frontier epic. Covers the documented "parent
// defaults to the frontier epic" behavior (ka-0000007387).
func TestGardener_BlockOnMR_DefaultedParent_GoesDormant(t *testing.T) {
	// mr_merged entry with no "parent" field — resolves to AssignBead.
	stop := json.RawMessage(`{"loop_end":{"any":[{"kind":"mr_merged"}]},` +
		`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`)
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		AssignBead:  "gd-epic-1", // frontier epic, used as the defaulted parent
		Stop:        stop,
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "unmerged"},
	)
	daemon.parentOf = map[string]string{"gd-child-1": "gd-epic-1"}
	daemon.mrBeadsByTask = map[string]*beadsapi.BeadDetail{
		"gd-child-1": mrBead("gd-mr-1", "open"),
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawns := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if spawns != 0 {
		t.Errorf("gardener blocked on unmerged MR (defaulted parent) must not spawn, got %d", spawns)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.DormantBlockerKind != gardenerDormantKindMRMerged {
		t.Errorf("expected DormantBlockerKind=%q, got %q", gardenerDormantKindMRMerged, cap.DormantBlockerKind)
	}
	if cap.DormantBlocker != "gd-epic-1" {
		t.Errorf("expected DormantBlocker=gd-epic-1 (resolved frontier epic), got %q", cap.DormantBlocker)
	}
}

// TestGardener_BlockOnMR_ParentReadError_DoesNotPark is the block-side fail-safety
// proof (invariant 2): when the daemon cannot resolve the declared parent epic
// (GetBead errors — here, the parent bead is simply absent from the fixture),
// gardenerHasUnmergedTrackedMR returns false and the gardener is NOT parked — it
// stays live for one more normal cycle rather than parking on unprovable data.
func TestGardener_BlockOnMR_ParentReadError_DoesNotPark(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		Stop:        blockOnMRStop("gd-epic-missing"),
	})
	// Deliberately do NOT seed gd-epic-missing — GetBead(parent) errors.
	daemon.parentOf = map[string]string{}

	engine.Evaluate(context.Background())

	cap := lastGardenerOrZero(daemon, "gd-cap-1")
	if cap.DormantBlockerKind == gardenerDormantKindMRMerged {
		t.Errorf("unresolvable parent must NOT park (fail-safe toward live), got kind=%q", cap.DormantBlockerKind)
	}
}

// TestGardener_BlockOnMR_WakeFailsOpenOnReadError is the wake-side fail-open proof
// (invariant 3): an already-dormant mr_merged gardener whose parent epic can no
// longer be read (GetBead errors) must WAKE rather than stay trapped — a flaky
// read can never imprison a gardener in dormancy.
func TestGardener_BlockOnMR_WakeFailsOpenOnReadError(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:           50,
		RunsSpawned:         5,
		Stop:                blockOnMRStop("gd-epic-gone"),
		DormantOnEscalation: true,
		DormantBlocker:      "gd-epic-gone",
		DormantBlockerKind:  gardenerDormantKindMRMerged,
	})
	// gd-epic-gone is absent ⇒ gardenerHasUnmergedTrackedMR's GetBead errors ⇒
	// wake predicate returns false ⇒ gardener wakes.
	daemon.parentOf = map[string]string{}

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.DormantOnEscalation {
		t.Errorf("dormant gardener with unreadable parent must WAKE (fail-open, invariant 3), stayed dormant")
	}
	if cap.DormantBlockerKind != "" {
		t.Errorf("expected DormantBlockerKind cleared on wake, got %q", cap.DormantBlockerKind)
	}
}

// TestGardener_BlockOnMR_SubtreeListError_DoesNotPark: the block-side subtree
// query (ListBeadsFiltered under the parent) erroring must fail toward NOT
// parking (invariant 2), even though the parent epic itself reads fine. Distinct
// from the parent-read-error case: here GetBead(parent) succeeds but the child
// scan fails, exercising gardenerHasUnmergedTrackedMR's list-error branch.
func TestGardener_BlockOnMR_SubtreeListError_DoesNotPark(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		Stop:        blockOnMRStop("gd-epic-1"),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
	)
	daemon.parentOf = map[string]string{}
	daemon.listBeadsErrForParent = map[string]error{"gd-epic-1": errTestDaemon}

	engine.Evaluate(context.Background())

	cap := lastGardenerOrZero(daemon, "gd-cap-1")
	if cap.DormantBlockerKind == gardenerDormantKindMRMerged {
		t.Errorf("subtree list error must NOT park (fail-safe toward live), got kind=%q", cap.DormantBlockerKind)
	}
}

// TestGardener_ProactiveWedge_DormantWakesOnQueryError: a dormant operator_wedge
// gardener whose wedge re-check (the actionable-count bead_query) errors must WAKE
// (invariant 3) — a flaky read can never trap it. gardenerFrontierFullyWedged
// fail-safes to not-wedged on query error, so gardenerDormantStillBlocked returns
// false and the gardener wakes.
func TestGardener_ProactiveWedge_DormantWakesOnQueryError(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:           50,
		RunsSpawned:         5,
		AssignBead:          "gd-epic-1",
		Stop:                json.RawMessage(`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`),
		DormantOnEscalation: true,
		DormantBlocker:      "gd-esc-1",
		DormantBlockerKind:  gardenerDormantKindOperatorWedge,
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-esc-1", Type: "escalation", Status: "open", Title: "operator: merge MRs"},
	)
	daemon.parentOf = map[string]string{"gd-esc-1": "gd-epic-1"}
	// The actionable-count query is scoped to the epic; make it error.
	daemon.listBeadsErrForParent = map[string]error{"gd-epic-1": errTestDaemon}

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.DormantOnEscalation {
		t.Errorf("dormant operator_wedge gardener whose re-check errors must WAKE (fail-open, invariant 3), stayed dormant")
	}
}

// errTestDaemon is a sentinel daemon error injected by fail-safety tests.
var errTestDaemon = errors.New("injected daemon read error")

// lastGardenerOrZero returns the latest gardener-block write for beadID, or a zero
// GardenerSpec when there was none (so a test can assert "was NOT parked" without
// requiring a write to exist).
func lastGardenerOrZero(daemon *mockTriggerDaemon, beadID string) GardenerSpec {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := range slices.Backward(daemon.updatedFields) {
		if daemon.updatedIDs[i] != beadID {
			continue
		}
		raw, ok := daemon.updatedFields[i]["gardener"]
		if !ok {
			continue
		}
		var capSpec GardenerSpec
		if err := json.Unmarshal([]byte(raw), &capSpec); err == nil {
			return capSpec
		}
	}
	return GardenerSpec{}
}
