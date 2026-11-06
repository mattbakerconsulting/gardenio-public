package main

import (
	"context"
	"maps"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// plans_enrich_budget_test.go — Phase 1 CONFIRMATION of the /plans index enrich
// round-trip budget (plan ka-0000002224, task gd-iZdjOhAVtk). Where the Phase-4
// re-measure (plans_enrich_measure_test.go) proves N-INVARIANCE on the MINIMAL
// plan shape — one step + one decision per plan, which lands at 8 round-trips —
// this file pins the CEILING: it drives a MAXIMAL plan shape where every bulk
// join fires BOTH of its round-trips (the dep-batch AND the hydration batch it
// gates), so the exact per-method budget shape is asserted, not just its
// invariance. Together the two files bracket the real budget: 9 (floor) ≤ b ≤ 15
// (ceiling), a small bounded constant, never O(plans).
//
// ── The confirmed budget ────────────────────────────────────────────────────
//
// Per /plans render the enrich pass (planIndexEnrichTimed) issues, at the
// UNSCOPED page (no operator involvement fan-out) with N plans on the page:
//
//	WAVE 1 — four INDEPENDENT joins, fanned out under one errgroup:
//	  1. populateOperatedBySlugs   — GetOperatorSlug, read-through cached, so ONE
//	                                 per DISTINCT human operator on the page (0 if
//	                                 no plan carries an OperatedByID FK). NOT keyed
//	                                 by plan → never O(plans) for a shared operator.
//	  2. planAssistantBulk         — 1 ListBeadsFiltered  (role:plan-gardener agents)
//	  3. planIndexStepData         — 2 batched calls for the flat tier:
//	                                   BatchGetDependencies(reverse)  [epics→children]
//	                                   BatchGetBeads                  [hydrate children]
//	                                 + 2 MORE only when some plan is PHASED (tier-2
//	                                   descent over the phase sub-epics):
//	                                   BatchGetDependencies(reverse)  [phases→steps]
//	                                   BatchGetBeads                  [hydrate steps]
//	  4. planGardenersByEpic        — 1 ListBeadsFiltered  (role:gardener triggers)
//
//	── BARRIER (wave1.Wait) ── wave 2's candidate sets (escWalk / stepWalk) are
//	   derived from wave 1's stepData, so wave 2 cannot begin until wave 1 ends.
//
//	WAVE 2 — four joins that DEPEND on wave 1, fanned out under a SECOND errgroup:
//	  5. planStallBulk       — 1 FindAgentBeadsByNames (liveness of in_progress
//	                            step assignees; 0 calls when no live-agent names)
//	  6. planEscalationsBulk — BatchGetDependencies(forward)  [steps+epic → blocks]
//	                         + BatchGetBeads                  [hydrate blockers]  (2)
//	                         + ListBeadsFiltered              [requesting_plan pin] (1)
//	  7. planCrewBulk        — BatchGetDependencies(reverse)  [steps → assigned]
//	                         + BatchGetBeads                  [hydrate crew agents](2)
//	  8. planOpenMRsBulk     — BatchGetDependencies(forward)  [steps → produces]
//	                         + BatchGetBeads                  [hydrate MR beads]   (2)
//
// The SECOND call of joins 6/7/8 is GATED: it fires only if the first surfaced a
// non-empty target/agent/MR set. The stall liveness call is likewise gated on a
// non-empty live-agent-name set. planEscalationsBulk's pin query (Layer 2,
// ka-0000007801) is UNGATED — it is one page-wide open-escalation query bucketed
// client-side by fields.requesting_plan, so it fires on every render but exactly
// ONCE regardless of plan count. This is why the minimal shape measures 9 and the
// maximal shape measures 13 (unphased) / 15 (phased):
//
//	                       │ minimal (measure) │ maximal-unphased │ maximal-phased
//	  ListBeadsFiltered    │        3          │        3         │      3
//	  GetOperatorSlug      │        0          │        1†        │      1†
//	  BatchGetDependencies │        4          │        4         │      5‡
//	  BatchGetBeads        │        1          │        4         │      5‡
//	  FindAgentBeadsByNames│        1          │        1         │      1
//	  ─────────────────────┼───────────────────┼──────────────────┼───────────────
//	  TOTAL                │        9          │       13         │     15
//
//	† one per DISTINCT operator, not per plan (all plans here share one operator).
//	‡ the +2 (one dep-batch + one hydrate) is the tier-2 phase descent, taking
//	  BatchGetDependencies 4→5 and BatchGetBeads 4→5.
//	  ListBeadsFiltered is 3 everywhere: planAssistantBulk + planGardenersByEpic +
//	  the requesting_plan escalation pin (one page-wide query, plan-count invariant).
//
// The headline invariant this task set out to CONFIRM: none of these is issued
// once-per-plan. Every count above is O(1) in the plan count (a fixed number of
// tiers/waves), O(operators) only for the slug populate, and O(1) again for the
// per-join batches — so doubling the plans leaves the whole table unchanged.
//
// ── Reconciling with the WHOLE-PAGE budget the task title quotes ────────────
//
// The title frames the pre-fix cost as "1 list + N decision-count calls (N up to
// plansListLimit=200) + ~12 serial bulk calls". Those three terms map to:
//
//   - "1 list": handlePlans' own top-level ListBeadsFiltered for the plan epics
//     (plans.go), which happens OUTSIDE this enrich pass. On the operator-scoped
//     path it fans into the involvement query (creator list + 3 facet scans + 1
//     batch materialize) and the board Done-lane / browse-by-person strip each
//     add one more bounded list — all page-shell calls, none per-plan, and the
//     browse-by-person universe list is now OVERLAPPED with enrich (ka-0000007509).
//   - "N decision-count calls": the ELIMINATED per-row N+1 — one CountOnly decision
//     query per plan, up to plansListLimit=200. Phase 2 (ka-0000004522 / ka-0000006056)
//     folded that count into the batched child walk; it is now ZERO extra calls,
//     guarded by TestPlansEnrich_NoPerRowDecisionCountCall. The "N" term is gone.
//   - "~12 serial bulk calls": the enrich-pass joins this file confirms — now 8..14
//     depending on shape, run as a TWO-WAVE DAG (below) instead of ~12 serial hops.
//
// So the confirmed post-fix whole-page budget is: a small constant of page-shell
// list calls + the 8..14 bulk enrich calls charted above — with the O(plans) N+1
// term removed entirely. This file pins the enrich-pass half; the page-shell
// list calls are exercised by plans_index_test.go / plans_involvement_test.go.
//
// ── The serial dependency graph among the bulk joins ────────────────────────
//
// The joins are NOT a flat list of 12 serial hops (the pre-Phase-3 shape the
// title quotes as "~12 serial bulk calls"). They form a two-layer DAG with ONE
// barrier:
//
//	           ┌─────────────────── WAVE 1 (concurrent) ───────────────────┐
//	           │  slugs        assistants      stepData        gardeners     │
//	           │ (operator)   (ListBeads)   (revdeps+get,     (ListBeads)   │
//	           │                             ×2 tiers)                       │
//	           └───────────────────────────┬──────────────────────────────┘
//	                                        │  BARRIER: wave 2 reads stepData
//	                                        │  (escWalk = epic+rendered steps,
//	                                        │   stepWalk = rendered steps)
//	           ┌───────────────────────────┴──────────────── WAVE 2 (concurrent) ┐
//	           │  stall            escalations        crew            openMRs      │
//	           │ (liveness    (fwd blocks→hydrate) (rev assigned→ (fwd produces→   │
//	           │  batch;       ×2, 2nd gated)       hydrate ×2,    hydrate ×2,     │
//	           │  reads                             2nd gated)     2nd gated)      │
//	           │  stepData+                                                        │
//	           │  assistants+                                                      │
//	           │  gardeners)                                                        │
//	           └──────────────────────────────────────────────────────────────────┘
//
// Edges (what depends on what):
//   - WITHIN a join, the hydration BatchGetBeads depends on its own dep-batch
//     (you cannot hydrate targets you have not yet discovered) — a 2-deep serial
//     chain per join, the only serialism left inside a wave.
//   - stall/escalations/crew/openMRs ALL depend on stepData.RenderedStepIDs
//     (escWalk/stepWalk are built from it after the barrier). stall ALSO reads
//     assistants and gardeners. That cross-wave read is the barrier's reason to
//     exist; without wave 1's stepData there is no candidate set to walk.
//   - wave-2 joins do NOT depend on each other — they read wave 1's maps
//     concurrently and each writes a DISTINCT result var, so they fan out freely.
//
// Critical path (serial DEPTH, the wall-clock floor at uniform RTT): wave 1's
// deepest branch is stepData at 2 tiers × 2 calls = 4 serial RTTs (phased) or 2
// (unphased); wave 2's deepest branch is any hydrating join at 2 serial RTTs.
// So depth = 4 (wave1) + 2 (wave2) = 6 RTTs phased / 2 + 2 = 4 unphased — versus
// the pre-Phase-3 sum of ~12+ serial hops. The COUNT is unchanged by the
// parallelization (same round-trips); the DEPTH is what fell.

// seedMaximalPlansFake builds a fake serving n PHASED plan epics, each wired so
// EVERY bulk join fires both of its round-trips:
//
//   - phase_order with one phase sub-epic → tier-2 descent (the +2 in stepData),
//   - the step lives UNDER the phase (a grandchild) and is in_progress with a
//     live agent assignee → planStallBulk's FindAgentBeadsByNames fires,
//   - the step --blocks--> an OPEN escalation → planEscalationsBulk hydrates,
//   - a crew agent --assigned--> the step → planCrewBulk hydrates,
//   - the step --produces--> an OPEN merge-request → planOpenMRsBulk hydrates,
//   - the epic carries an OperatedByID FK → populateOperatedBySlugs resolves one
//     slug (SHARED across all plans, so exactly ONE GetOperatorSlug regardless
//     of n — the property that proves the slug populate is O(operators)),
//   - a bound plan-gardener agent + a role:gardener trigger → the assistant and
//     gardener list joins bind a real row.
//
// Returns the fake and the epic slice in index order. Deliberately standalone
// (its own seeder + its own operator/edges) so it never perturbs the shared
// seedPlansFake or the Phase-4 seedManyPlansFake fixtures.
func seedMaximalPlansFake(n int) (*fakeDaemon, []*beadsapi.BeadDetail) {
	f := newFake()
	if f.revDeps == nil {
		f.revDeps = map[string][]beadsapi.Dependency{}
	}
	if f.deps == nil {
		f.deps = map[string][]beadsapi.Dependency{}
	}
	// One SHARED human operator across every plan — the slug populate must resolve
	// it exactly ONCE (read-through cache), proving it is O(distinct operators),
	// not O(plans).
	const operatorID int64 = 4242
	f.opSlugResp = map[int64]*beadsapi.GetOperatorSlugResponse{
		operatorID: {Found: true, Slug: "matt"},
	}

	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	epics := make([]*beadsapi.BeadDetail, 0, n)
	for i := range n {
		opID := operatorID
		epicID := "gd-mplan-" + itoa(i)
		phaseID := "gd-mphase-" + itoa(i)
		stepID := "gd-mstep-" + itoa(i)
		decID := "gd-mdec-" + itoa(i)
		escID := "gd-mesc-" + itoa(i)
		crewID := "gd-mcrew-" + itoa(i)
		mrID := "gd-mmr-" + itoa(i)
		gardenerID := "gd-mcap-" + itoa(i)
		assistantID := "gd-masst-" + itoa(i)

		epic := &beadsapi.BeadDetail{
			ID:           epicID,
			Title:        "Maximal Plan " + itoa(i),
			Kind:         "issue",
			Type:         "epic",
			Status:       "in_progress",
			Labels:       []string{"project:gardenio", "plan"},
			OperatedByID: &opID,
			// One phase sub-epic → the tier-2 descent path is exercised.
			Fields:    map[string]string{"phase_order": `["` + phaseID + `"]`},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		phase := &beadsapi.BeadDetail{
			ID:        phaseID,
			Title:     "Phase 1",
			Kind:      "issue",
			Type:      "epic",
			Status:    "in_progress",
			Labels:    []string{"project:gardenio"},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		// The step is a GRANDCHILD (under the phase), in_progress with a live-looking
		// agent name → the stall liveness batch fires off a non-empty name set.
		step := &beadsapi.BeadDetail{
			ID:        stepID,
			Title:     "Step " + itoa(i),
			Kind:      "issue",
			Type:      "task",
			Status:    "in_progress",
			Assignee:  "gardenio-worker-" + itoa(i),
			Labels:    []string{"project:gardenio"},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		// A DIRECT open clarifying-question decision (tier-1) → folded open-decision
		// count off the batched walk, zero extra round-trip.
		decision := &beadsapi.BeadDetail{
			ID:        decID,
			Title:     "Question " + itoa(i),
			Type:      "decision",
			Status:    "open",
			Fields:    map[string]string{"prompt": "pick a path"},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		// An OPEN escalation the step BLOCKS on → escalation hydration batch fires.
		esc := &beadsapi.BeadDetail{
			ID:     escID,
			Title:  "Escalation " + itoa(i),
			Type:   "escalation",
			Status: "open",
			Fields: map[string]string{"operation_type": "secret", "sensitivity": "high"},
		}
		// A live crew agent ASSIGNED to the step → crew hydration batch fires.
		crew := &beadsapi.BeadDetail{
			ID:        crewID,
			Title:     "crew " + itoa(i),
			Type:      "agent",
			Status:    "in_progress",
			Fields:    map[string]string{"agent": "gardenio-crew-" + itoa(i), "agent_state": "working"},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		// An OPEN merge-request the step PRODUCES → MR hydration batch fires.
		mr := &beadsapi.BeadDetail{
			ID:     mrID,
			Title:  "MR " + itoa(i),
			Type:   "merge-request",
			Status: "open",
			Fields: map[string]string{"mr_url": "https://gitlab/mr/" + itoa(i), "mr_state": "opened"},
		}
		// A role:gardener trigger bound to the epic → the gardeners list join binds it.
		gardener := &beadsapi.BeadDetail{
			ID:        gardenerID,
			Title:     "gardener " + itoa(i),
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"role:gardener", "gardener-epic:" + epicID},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		// A role:plan-gardener agent bound to the epic via plan_id → the assistant
		// list join binds it.
		assistant := &beadsapi.BeadDetail{
			ID:        assistantID,
			Title:     "assistant " + itoa(i),
			Type:      "agent",
			Status:    "in_progress",
			Labels:    []string{"role:plan-gardener"},
			Fields:    map[string]string{"plan_id": epicID, "agent_state": "working", "agent": "gardenio-assistant-" + itoa(i)},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}

		for _, b := range []*beadsapi.BeadDetail{epic, phase, step, decision, esc, crew, mr, gardener, assistant} {
			f.beads[b.ID] = b
		}
		// Reverse deps (child --parent-child--> parent), as the reverse-dep walk reads:
		//   epic  ← phase (tier-1 direct child, an epic)  AND  epic ← decision (tier-1)
		//   phase ← step  (tier-2 grandchild)
		f.revDeps[epicID] = []beadsapi.Dependency{
			{BeadID: phaseID, DependsOnID: epicID, Type: "parent-child"},
			{BeadID: decID, DependsOnID: epicID, Type: "parent-child"},
		}
		f.revDeps[phaseID] = []beadsapi.Dependency{
			{BeadID: stepID, DependsOnID: phaseID, Type: "parent-child"},
		}
		// Crew --assigned--> step surfaces on the step's REVERSE deps.
		f.revDeps[stepID] = []beadsapi.Dependency{
			{BeadID: crewID, DependsOnID: stepID, Type: "assigned"},
		}
		// Step --blocks--> escalation AND step --produces--> MR surface on the step's
		// FORWARD deps.
		f.deps[stepID] = []beadsapi.Dependency{
			{BeadID: stepID, DependsOnID: escID, Type: "blocks"},
			{BeadID: stepID, DependsOnID: mrID, Type: "produces"},
		}
		epics = append(epics, epic)
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: epics, Total: len(epics)}
	return f, epics
}

// maximalEnrichRoundTrips runs the real timed enrich over n maximal-shape plans
// and returns the total round-trip count plus the per-method breakdown.
func maximalEnrichRoundTrips(t *testing.T, n int) (int64, map[string]int) {
	t.Helper()
	f, epics := seedMaximalPlansFake(n)
	cd := newCountingDaemon(f, 0)
	s := testServerWithDaemon(cd)
	s.SetNow(func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) })
	rows, _ := s.planIndexEnrichTimed(context.Background(), epics)
	if len(rows) != n {
		t.Fatalf("enriched %d rows, want %d", len(rows), n)
	}
	cd.mu.Lock()
	pm := maps.Clone(cd.perMethod)
	cd.mu.Unlock()
	return cd.total(), pm
}

// TestPlansEnrich_MaximalBudgetShape CONFIRMS the exact per-method round-trip
// budget when every bulk join fires BOTH of its hops (the ceiling). This is the
// companion to the measure test's floor: it proves the budget's SHAPE, not just
// its N-invariance, and it exercises the hydration halves + tier-2 phase descent
// the minimal shape short-circuits.
//
// If a future change adds a per-row call inside any bulk join, or reintroduces a
// per-plan round-trip, one of these exact-count assertions breaks with a clear
// per-method diff — a far sharper failure than a drifting total.
func TestPlansEnrich_MaximalBudgetShape(t *testing.T) {
	const n = 24
	total, pm := maximalEnrichRoundTrips(t, n)

	// The confirmed maximal-phased budget: 15 round-trips.
	//   ListBeadsFiltered    = 3  (planAssistantBulk + planGardenersByEpic +
	//                              the requesting_plan escalation pin — ONE
	//                              page-wide open-escalation query, ka-0000007801)
	//   GetOperatorSlug      = 1  (ONE shared operator, read-through cached)
	//   BatchGetDependencies = 5  (stepData tier1 revdeps + tier2 revdeps = 2;
	//                              escalations fwd = 1; crew rev = 1; openMRs fwd = 1)
	//   BatchGetBeads        = 5  (stepData tier1 hydrate + tier2 hydrate = 2;
	//                              escalation blockers = 1; crew agents = 1; MR beads = 1)
	//   FindAgentBeadsByNames= 1  (stall liveness of the in_progress step assignees)
	//
	// The pin query is a SINGLE call regardless of plan count (bucketed
	// client-side by fields.requesting_plan), so the N-invariance the companion
	// tests pin is preserved — the budget rose by a constant 1, not by O(plans).
	want := map[string]int{
		"ListBeadsFiltered":     3,
		"GetOperatorSlug":       1,
		"BatchGetDependencies":  5,
		"BatchGetBeads":         5,
		"FindAgentBeadsByNames": 1,
	}
	for method, wantN := range want {
		if pm[method] != wantN {
			t.Errorf("method %s: got %d round-trips, want %d (full breakdown: %v)", method, pm[method], wantN, pm)
		}
	}
	// No stray method the budget doesn't account for.
	for method, gotN := range pm {
		if _, accounted := want[method]; !accounted {
			t.Errorf("unexpected daemon method %s called %d times — not in the confirmed budget (%v)", method, gotN, pm)
		}
	}

	const wantTotal = 15
	if total != wantTotal {
		t.Fatalf("maximal-phased enrich total = %d round-trips, want %d; breakdown %v", total, wantTotal, pm)
	}

	// Sanity floor: the maximal shape must cost MORE than the minimal shape (the
	// measure test's 9) — otherwise the hydration halves / tier-2 descent were not
	// actually exercised and this test proves nothing about the ceiling.
	if total <= 9 {
		t.Fatalf("maximal shape cost %d round-trips — expected strictly more than the minimal-shape 9; the hydration/phase paths did not fire (breakdown %v)", total, pm)
	}
	t.Logf("CONFIRMED maximal-phased budget @%d plans: %d round-trips; per-method %v", n, total, pm)
}

// TestPlansEnrich_MaximalBudgetIsPlanCountInvariant re-runs the N-invariance
// proof on the MAXIMAL shape (the measure test proves it on the minimal shape).
// Firing every hydration half + the tier-2 descent is exactly where a per-row
// regression would most plausibly hide, so pinning invariance HERE closes that
// gap: enriching 20 vs 50 maximal plans must issue the IDENTICAL budget.
func TestPlansEnrich_MaximalBudgetIsPlanCountInvariant(t *testing.T) {
	total20, pm20 := maximalEnrichRoundTrips(t, 20)
	total50, pm50 := maximalEnrichRoundTrips(t, 50)

	if total20 != total50 {
		t.Fatalf("maximal-shape round-trips must be plan-count INVARIANT; got %d for 20 plans vs %d for 50 plans (a per-row fan-out survives). @20 %v; @50 %v",
			total20, total50, pm20, pm50)
	}
	// And no single method may scale with plan count: at 50 plans, any method at
	// >= 50 calls is a per-row fan-out. Every bulk join is O(1) tiers.
	for method, count := range pm50 {
		if count >= 50 {
			t.Fatalf("method %s called %d times for 50 maximal plans — a per-row fan-out, not a bulk join", method, count)
		}
	}
	t.Logf("CONFIRMED maximal-shape N-invariance: 20 plans => %d round-trips; 50 plans => %d; INVARIANT. Per-method @50: %v",
		total50, total50, pm50)
}

// TestPlansEnrich_WaveDependencyIsSerialized confirms the ONE serial edge in the
// dependency graph: wave 2 cannot begin until wave 1 finishes, because wave 2's
// candidate sets (escWalk/stepWalk) are derived from wave 1's stepData. We prove
// it by injecting a per-round-trip RTT and asserting the measured wall clock is
// at least the two-wave serial-depth floor (wave 1 hits the daemon, THEN wave 2
// hits it after the barrier). A collapsed single-wave design (no barrier) could
// finish inside one wave's depth; the barrier forbids that.
//
// This is the wall-clock complement to the count assertions above: the COUNT is
// invariant to the parallelization, but the DEPTH — the property the Phase-3
// two-wave split actually bought — is what this pins.
func TestPlansEnrich_WaveDependencyIsSerialized(t *testing.T) {
	const (
		n   = 12
		rtt = 2 * time.Millisecond
	)
	f, epics := seedMaximalPlansFake(n)
	cd := newCountingDaemon(f, rtt)
	s := testServerWithDaemon(cd)
	s.SetNow(func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) })

	start := time.Now()
	rows, _ := s.planIndexEnrichTimed(context.Background(), epics)
	wall := time.Since(start)
	if len(rows) != n {
		t.Fatalf("enriched %d rows, want %d", len(rows), n)
	}

	// Serial-depth floor: wave 1's stepData chain is 2 tiers (revdeps→get,
	// revdeps→get) = 4 serial RTTs (phased); wave 2 then runs AFTER the barrier and
	// its hydrating joins are 2 serial RTTs deep. So the wall clock is provably
	// >= (wave1 barrier + wave2 first hop) worth of real sleeps. We assert a
	// conservative floor of 3×rtt: wave 1 must complete at least one dep→get chain
	// (2 RTTs) before the barrier releases wave 2, which then pays >= 1 RTT. A
	// no-barrier collapse would let wave-2 calls overlap wave 1 and could dip below
	// this floor.
	floor := 3 * rtt
	if wall < floor {
		t.Fatalf("enrich wall clock %v < two-wave serial floor %v — the barrier between waves was not honored (or the injected rtt did not take effect)", wall, floor)
	}
	t.Logf("CONFIRMED two-wave serialization @%d plans, rtt=%v: wall=%v (>= %v two-wave floor)", n, rtt, wall, floor)
}
