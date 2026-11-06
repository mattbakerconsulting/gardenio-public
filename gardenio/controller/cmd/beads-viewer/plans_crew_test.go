package main

// plans_crew_test.go — the plan CREW roster (ka-0000000150): "show crew the
// gardener has created and how it's going". Covers the batched assigned-edge
// walk + agent hydration (planCrew), the per-member state/outcome projection,
// the live-first ordering, and the SSE interest set (crewIDs).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// crewFixtureNow is the shared pinned clock every crew test derives seed
// timestamps against. seedCrew stamps an alive crew's UpdatedAt at
// crewFixtureNow − 5m — comfortably inside the 1h agentStaleThreshold — so the
// crew reads FRESH (live) under the iter-42 freshness gate. A render test that
// exercises the crew chip through a handler MUST pin the server clock to this
// same instant (mkServerWithNow(t, f, crewFixtureNowFn) or crewTestServer);
// otherwise the wall clock reads the fixed seed stamp as years-stale and the
// gate correctly demotes a "working" crew to stuck — the exact behavior iter-42
// adds, but not what those legacy fixtures mean to assert.
var crewFixtureNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

func crewFixtureNowFn() time.Time { return crewFixtureNow }

// seedCrew wires a crew agent bead assigned to a step: SpawnAgent writes an
// `assigned` edge FROM the crew agent TO the step, so the step's REVERSE deps
// surface its crew. Mirrors the live linkage planCrew walks. The alive-crew
// UpdatedAt is stamped fresh (crewFixtureNow − 5m) so a working/spawning/yielding
// crew reads live under the freshness gate; seedStaleCrew overrides it to test a
// crashed-mid-task crew.
func seedCrew(f *fakeDaemon, agentID, name, stepID, state, status string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     agentID,
		Title:  name,
		Kind:   "config",
		Type:   "agent",
		Status: status,
		Fields: map[string]string{
			"agent":       name,
			"role":        "crew",
			"agent_state": state,
			"task_id":     stepID,
		},
		CreatedAt: crewFixtureNow.Add(-time.Hour),
		UpdatedAt: crewFixtureNow.Add(-5 * time.Minute),
	}
	f.beads[agentID] = b
	if f.revDeps == nil {
		f.revDeps = map[string][]beadsapi.Dependency{}
	}
	// crew --assigned--> step : the reverse dep of the STEP points at the crew.
	f.revDeps[stepID] = append(f.revDeps[stepID], beadsapi.Dependency{
		BeadID: agentID, DependsOnID: stepID, Type: "assigned",
	})
	return b
}

// seedStaleCrew wires a crew agent bead whose alive agent_state carries a STALE
// heartbeat (UpdatedAt past agentStaleThreshold before crewFixtureNow) — the
// crashed-mid-task crew the iter-42 freshness gate demotes from "working" to a
// reachable "stuck" so the crew chip agrees with the ⚠ stall badge on the same
// card. ageBeforeNow is how long ago the crew last pushed an update.
func seedStaleCrew(f *fakeDaemon, agentID, name, stepID, state string, ageBeforeNow time.Duration) *beadsapi.BeadDetail {
	b := seedCrew(f, agentID, name, stepID, state, "in_progress")
	b.UpdatedAt = crewFixtureNow.Add(-ageBeforeNow)
	return b
}

func crewTestServer(f *fakeDaemon) *Server {
	s := testServerWithDaemon(f)
	s.SetNow(crewFixtureNowFn)
	return s
}

func TestPlanCrew_WorkingAndDoneMembers(t *testing.T) {
	f := seedPlansFake()
	// A working crew (recent update → alive) and a finished one.
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	done := seedCrew(f, "gd-crewB", "gardenio-wire-route-xyz", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"Wired the route; MR merged."}`
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)

	if info.Total != 2 {
		t.Fatalf("Total = %d, want 2", info.Total)
	}
	if info.Working != 1 || info.Done != 1 || info.Failed != 0 {
		t.Errorf("tally working/done/failed = %d/%d/%d, want 1/1/0", info.Working, info.Done, info.Failed)
	}
	// Live crew leads the roster.
	if info.Members[0].ID != "gd-crewA" || !info.Members[0].Live {
		t.Errorf("first member = %+v, want live gd-crewA first", info.Members[0])
	}
	// Step context propagates from the timeline.
	if info.Members[0].StepTitle != "Wire the route" || info.Members[0].StepNumber != 1 {
		t.Errorf("member step = %q #%d, want Wire the route #1", info.Members[0].StepTitle, info.Members[0].StepNumber)
	}
	// The finished crew carries its wrapup tldr as the outcome.
	var b planCrewMember
	for _, m := range info.Members {
		if m.ID == "gd-crewB" {
			b = m
		}
	}
	if !strings.Contains(b.Outcome, "Wired the route") {
		t.Errorf("done crew Outcome = %q, want it to carry the wrapup tldr", b.Outcome)
	}
	if b.Failed {
		t.Errorf("clean done crew must not read failed")
	}
}

func TestPlanCrew_FailedCrewReadsRed(t *testing.T) {
	f := seedPlansFake()
	fail := seedCrew(f, "gd-crewF", "gardenio-wire-route-fail", "gd-step1", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"Hit a wall","blockers":"needs an IAM grant"}`
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", info.Failed)
	}
	if !info.Members[0].Failed {
		t.Errorf("failed crew member.Failed = false, want true")
	}
}

func TestPlanCrew_StuckOpenCrewReadsFailed(t *testing.T) {
	f := seedPlansFake()
	// A still-open crew in a rate_limited state is stuck, not making progress.
	seedCrew(f, "gd-crewR", "gardenio-wire-route-rl", "gd-step1", "rate_limited", "in_progress")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Failed != 1 || info.Working != 0 {
		t.Errorf("stuck open crew tally working/failed = %d/%d, want 0/1", info.Working, info.Failed)
	}
	if !info.Members[0].Failed {
		t.Errorf("stuck open crew must read failed")
	}
}

// TestPlanCrew_StuckTallySplitsOpenVsClosed proves the cockpit roster splits its
// stuck tally by REACHABILITY (ka-0000005352 iter-33), exactly as iter-29 split
// the index card's crew chip: an OPEN (reachable) stuck crew counts toward the
// LOUD "⚠ K stuck" chip (OpenStuck / HasOpenStuck), a CLOSED-failed crew toward
// the CALM "✕ K failed" historical chip (ClosedFailed / HasClosedFailed). Before
// the fix the roster rendered one loud "⚠ N stuck" chip conflating both, so a
// plan whose failures were all closed history read a calm "✕ 1" on the index
// board beside a loud red "⚠ 1 stuck" on its own cockpit — a card-vs-truth
// contradiction. FAILS pre-fix (OpenStuck/ClosedFailed do not exist).
func TestPlanCrew_StuckTallySplitsOpenVsClosed(t *testing.T) {
	f := seedPlansFake()
	// One OPEN stuck crew (rate-limited, reachable) and one CLOSED-failed crew
	// (dead terminal, history) — the exact mix the live board carries.
	seedCrew(f, "gd-crewOpen", "gardenio-open-stuck", "gd-step1", "rate_limited", "in_progress")
	seedCrew(f, "gd-aSdZpwhQxR", "gardenio-closed-fail", "gd-step1", "failed", "closed")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)

	if info.Failed != 2 {
		t.Fatalf("Failed = %d, want 2 (open + closed)", info.Failed)
	}
	if info.OpenStuck() != 1 || !info.HasOpenStuck() {
		t.Errorf("OpenStuck = %d (has=%v), want 1/true — the loud reachable count", info.OpenStuck(), info.HasOpenStuck())
	}
	if info.ClosedFailed() != 1 || !info.HasClosedFailed() {
		t.Errorf("ClosedFailed = %d (has=%v), want 1/true — the calm historical count", info.ClosedFailed(), info.HasClosedFailed())
	}
}

// TestPlanCrew_OnlyClosedFailedIsCalm proves a plan whose failures are ALL closed
// history renders NO loud open-stuck chip — only the calm "✕ K failed" marker —
// so the cockpit no longer screams a false "act now" for crew with nothing to
// click through to. This is the exact contradiction measured on 4 live plans
// (index calm "✕ 1", cockpit loud "⚠ 1 stuck"). FAILS pre-fix.
func TestPlanCrew_OnlyClosedFailedIsCalm(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-aSdZpwhQxR", "gardenio-closed-fail", "gd-step1", "failed", "closed")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)

	if info.HasOpenStuck() {
		t.Errorf("a plan with only closed-failed crew must render NO loud ⚠ stuck chip (HasOpenStuck=true)")
	}
	if info.ClosedFailed() != 1 || !info.HasClosedFailed() {
		t.Errorf("ClosedFailed = %d (has=%v), want 1/true", info.ClosedFailed(), info.HasClosedFailed())
	}
}

// TestPlanCrew_OnlyOpenStuckIsLoud proves a plan whose stuck crew are all OPEN
// (reachable) renders the loud "⚠ K stuck" chip and NO calm historical marker —
// the count still reads as one loud "act now" exactly as before the split.
func TestPlanCrew_OnlyOpenStuckIsLoud(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewOpen", "gardenio-open-stuck", "gd-step1", "budget_exceeded", "in_progress")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)

	if info.OpenStuck() != 1 || !info.HasOpenStuck() {
		t.Errorf("OpenStuck = %d (has=%v), want 1/true", info.OpenStuck(), info.HasOpenStuck())
	}
	if info.HasClosedFailed() {
		t.Errorf("a plan with only open stuck crew must render NO calm ✕ failed marker (HasClosedFailed=true)")
	}
}

func TestPlanCrew_DedupesCrewAcrossEdges(t *testing.T) {
	f := seedPlansFake()
	// One crew, two assigned edges to the same step (a re-claim writes a second).
	seedCrew(f, "gd-crewA", "gardenio-dup", "gd-step1", "working", "in_progress")
	f.revDeps["gd-step1"] = append(f.revDeps["gd-step1"], beadsapi.Dependency{
		BeadID: "gd-crewA", DependsOnID: "gd-step1", Type: "assigned",
	})
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Total != 1 {
		t.Errorf("Total = %d, want 1 (deduped by agent id)", info.Total)
	}
}

func TestPlanCrew_IgnoresNonAssignedAndNonAgentEdges(t *testing.T) {
	f := seedPlansFake()
	// A parent-child reverse dep (not a crew) and an assigned edge to a
	// non-agent bead must both be ignored.
	f.revDeps["gd-step1"] = append(f.revDeps["gd-step1"],
		beadsapi.Dependency{BeadID: "gd-doc1", DependsOnID: "gd-step1", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-notagent", DependsOnID: "gd-step1", Type: "assigned"},
	)
	f.beads["gd-notagent"] = &beadsapi.BeadDetail{ID: "gd-notagent", Type: "task", Status: "open"}
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Total != 0 {
		t.Errorf("Total = %d, want 0 (no real crew)", info.Total)
	}
}

func TestPlanCrew_EmptyWhenNoSteps(t *testing.T) {
	f := seedPlansFake()
	s := crewTestServer(f)
	if info := s.planCrew(context.Background(), "", nil, nil); info.Total != 0 {
		t.Errorf("no steps → Total = %d, want 0", info.Total)
	}
}

func TestPlanCrew_RenderCapNotesRemainder(t *testing.T) {
	f := seedPlansFake()
	// Seed more crew than the render cap so HasMore/MoreCount surface.
	for i := 0; i < planCrewMaxRender+3; i++ {
		id := "gd-crew" + string(rune('a'+i))
		seedCrew(f, id, "gardenio-crew-"+id, "gd-step1", "done", "closed")
	}
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Total != planCrewMaxRender+3 {
		t.Fatalf("Total = %d, want %d", info.Total, planCrewMaxRender+3)
	}
	if len(info.Members) != planCrewMaxRender {
		t.Errorf("rendered members = %d, want capped at %d", len(info.Members), planCrewMaxRender)
	}
	if !info.HasMore() || info.MoreCount() != 3 {
		t.Errorf("HasMore=%v MoreCount=%d, want true/3", info.HasMore(), info.MoreCount())
	}
}

// --- planCrewBulk: the page-wide index rollup (ka-0000004801) ----------------

// TestPlanCrewBulk_TalliesAcrossPlans proves the page-wide crew rollup counts
// working / done / stuck per plan from the SAME batched assigned-edge walk, and
// classifies each crew identically to the cockpit roster.
func TestPlanCrewBulk_TalliesAcrossPlans(t *testing.T) {
	f := seedPlansFake()
	// Plan 1's step (gd-step1): one working + one clean-done crew.
	seedCrew(f, "gd-crewA", "gardenio-crew-a", "gd-step1", "working", "in_progress")
	done := seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"done"}`
	// Plan 2 (gd-plan2) has its own step (gd-step9) with a failed crew.
	fail := seedCrew(f, "gd-crewF", "gardenio-crew-f", "gd-step9", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"hit a wall","blockers":"needs IAM"}`
	s := crewTestServer(f)

	rolls := s.planCrewBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1"},
		"gd-plan2": {"gd-step9"},
	}, nil)
	p1 := rolls["gd-plan1"]
	if p1.Total != 2 || p1.Working != 1 || p1.Done != 1 || p1.Failed != 0 {
		t.Errorf("plan1 rollup total/working/done/failed = %d/%d/%d/%d, want 2/1/1/0", p1.Total, p1.Working, p1.Done, p1.Failed)
	}
	if !p1.Any() || !p1.HasLive() {
		t.Errorf("plan1 Any=%v HasLive=%v, want true/true", p1.Any(), p1.HasLive())
	}
	p2 := rolls["gd-plan2"]
	if p2.Total != 1 || p2.Failed != 1 || p2.Working != 0 {
		t.Errorf("plan2 rollup total/failed/working = %d/%d/%d, want 1/1/0", p2.Total, p2.Failed, p2.Working)
	}
	if p2.HasLive() {
		t.Errorf("plan2 has no working crew — HasLive must be false")
	}
}

// TestPlanCrewBulk_NoCrewOmitsPlan proves a plan with no dispatched crew is
// absent from the rollup map (its card then renders no crew chip).
func TestPlanCrewBulk_NoCrewOmitsPlan(t *testing.T) {
	f := seedPlansFake()
	s := crewTestServer(f)
	rolls := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)
	if roll, ok := rolls["gd-plan1"]; ok {
		t.Errorf("crew-less plan should be omitted, got %+v", roll)
	}
}

// TestPlanCrewBulk_DedupesCrewAcrossEdges proves a re-claim (two assigned edges
// for one crew) counts the crew once, matching planCrew's dedupe.
func TestPlanCrewBulk_DedupesCrewAcrossEdges(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewA", "gardenio-dup", "gd-step1", "working", "in_progress")
	f.revDeps["gd-step1"] = append(f.revDeps["gd-step1"], beadsapi.Dependency{
		BeadID: "gd-crewA", DependsOnID: "gd-step1", Type: "assigned",
	})
	s := crewTestServer(f)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Total != 1 {
		t.Errorf("Total = %d, want 1 (deduped by agent id)", roll.Total)
	}
}

// TestPlanCrewBulk_MatchesCockpitTally proves the page-wide rollup and the
// cockpit roster (planCrew) count the same crew the same way — the anti-drift
// guarantee: the index chip can never disagree with the /plans/{id} crew card.
func TestPlanCrewBulk_MatchesCockpitTally(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewA", "gardenio-crew-a", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "done", "closed")
	seedCrew(f, "gd-crewR", "gardenio-crew-r", "gd-step1", "rate_limited", "in_progress")
	s := crewTestServer(f)

	// A CLOSED-failed crew too, so the open/closed stuck split (iter-33) is
	// exercised — the index chip and cockpit tally must agree on it by construction.
	seedCrew(f, "gd-crewC", "gardenio-crew-c", "gd-step1", "failed", "closed")
	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1}}
	cockpit := s.planCrew(context.Background(), "", steps, nil)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Total != cockpit.Total || roll.Working != cockpit.Working ||
		roll.Done != cockpit.Done || roll.Failed != cockpit.Failed {
		t.Errorf("rollup %+v disagrees with cockpit tally working/done/failed %d/%d/%d",
			roll, cockpit.Working, cockpit.Done, cockpit.Failed)
	}
	// The open/closed stuck split must match between the two surfaces (iter-33) —
	// otherwise the index chip and the cockpit tally contradict each other on which
	// failures are reachable "act now" vs closed history.
	if roll.OpenStuck() != cockpit.OpenStuck() || roll.ClosedFailed() != cockpit.ClosedFailed() {
		t.Errorf("open/closed stuck split disagrees: index open/closed = %d/%d, cockpit = %d/%d",
			roll.OpenStuck(), roll.ClosedFailed(), cockpit.OpenStuck(), cockpit.ClosedFailed())
	}
}

// --- planCrew(Bulk) cumulative SPEND (ka-0000004218, iter-12) ----------------

// TestPlanCrewBulk_SumsSpend proves the rollup sums every crew's dollar cost into
// the plan total using the shared agentSpendUSD semantics: a CLOSED crew counts
// its reconciled total_cost_usd only (its live_cost_usd is stale and must NOT be
// added), while a LIVE crew counts total + the in-pod live estimate.
func TestPlanCrewBulk_SumsSpend(t *testing.T) {
	f := seedPlansFake()
	// Closed crew: reconciled $2.00; a stale live_cost_usd that must be IGNORED.
	done := seedCrew(f, "gd-crewD", "gardenio-crew-d", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"done"}`
	done.Fields["total_cost_usd"] = "2.00"
	done.Fields["live_cost_usd"] = "9.99" // stale — closed crew: excluded
	// Live crew: reconciled $1.00 from prior sessions + $0.50 live estimate = $1.50.
	live := seedCrew(f, "gd-crewL", "gardenio-crew-l", "gd-step1", "working", "in_progress")
	live.Fields["total_cost_usd"] = "1.00"
	live.Fields["live_cost_usd"] = "0.50"
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	// 2.00 (closed, no live) + 1.50 (live total+estimate) = 3.50.
	if !roll.HasSpend() {
		t.Fatalf("HasSpend = false, want true")
	}
	if got := roll.SpendLabel(); got != "$3.50" {
		t.Errorf("SpendLabel = %q, want $3.50 (closed 2.00 + live 1.50; stale live_cost on closed excluded)", got)
	}
}

// TestPlanCrewBulk_NoSpendNoFacet proves a plan whose crew have reported no cost
// yet renders no spend facet (HasSpend false, empty SpendLabel) — the zero-ink
// gate every facet on the card follows.
func TestPlanCrewBulk_NoSpendNoFacet(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewA", "gardenio-crew-a", "gd-step1", "working", "in_progress")
	s := crewTestServer(f)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.HasSpend() || roll.SpendLabel() != "" {
		t.Errorf("no-cost crew: HasSpend=%v SpendLabel=%q, want false/empty", roll.HasSpend(), roll.SpendLabel())
	}
}

// TestPlanCrewSpend_MatchesCockpit proves the index rollup and the cockpit roster
// derive the SAME dollar figure for the same crew — the anti-drift guarantee for
// spend, mirroring TestPlanCrewBulk_MatchesCockpitTally for the counts.
func TestPlanCrewSpend_MatchesCockpit(t *testing.T) {
	f := seedPlansFake()
	a := seedCrew(f, "gd-crewA", "gardenio-crew-a", "gd-step1", "working", "in_progress")
	a.Fields["total_cost_usd"] = "1.25"
	a.Fields["live_cost_usd"] = "0.25"
	b := seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "done", "closed")
	b.Fields["wrapup"] = `{"tldr":"done"}`
	b.Fields["total_cost_usd"] = "3.00"
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1}}
	cockpit := s.planCrew(context.Background(), "", steps, nil)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if cockpit.SpendLabel() != roll.SpendLabel() {
		t.Errorf("cockpit spend %q disagrees with index rollup spend %q", cockpit.SpendLabel(), roll.SpendLabel())
	}
	// 1.25 + 0.25 (live) + 3.00 (closed) = 4.50.
	if roll.SpendLabel() != "$4.50" {
		t.Errorf("SpendLabel = %q, want $4.50", roll.SpendLabel())
	}
}

// --- planCrewBulk live-crew faces (ka-0000002189) ----------------------------

// TestPlanCrewBulk_FacesSurfaceLiveCrew proves the rollup carries a click-through
// avatar per LIVE crew (initials + terminal/bead url), ordered newest-first, and
// that only working crew get a face — a done/failed crew never appears as an
// avatar even though it's counted.
func TestPlanCrewBulk_FacesSurfaceLiveCrew(t *testing.T) {
	f := seedPlansFake()
	// Two working crew (different update times) + one done + one failed.
	older := seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	older.UpdatedAt = time.Date(2026, 7, 5, 11, 30, 0, 0, time.UTC)
	older.Labels = []string{"project:gardenio", "role:crew"}
	newer := seedCrew(f, "gd-crewB", "gardenio-paint-grid-xyz", "gd-step1", "working", "in_progress")
	newer.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC)
	newer.Labels = []string{"project:gardenio", "role:crew"}
	done := seedCrew(f, "gd-crewD", "gardenio-done-one", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"done"}`
	fail := seedCrew(f, "gd-crewF", "gardenio-stuck-one", "gd-step1", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"hit a wall","blockers":"needs IAM"}`
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Working != 2 || len(roll.Faces) != 2 {
		t.Fatalf("Working=%d Faces=%d, want 2 working with 2 faces (only live crew get avatars)", roll.Working, len(roll.Faces))
	}
	if roll.FacesHidden() != 0 {
		t.Errorf("FacesHidden = %d, want 0 (both live crew fit under the cap)", roll.FacesHidden())
	}
	// Newest-active crew leads the strip.
	if roll.Faces[0].ID != "gd-crewB" {
		t.Errorf("first face = %s, want gd-crewB (most-recently-active first)", roll.Faces[0].ID)
	}
	// Initials + name + a click-through url are projected from the hydrated bead.
	// The avatar DROPS the redundant leading project token (iter-35): the crew
	// carries project:gardenio, so "gardenio-paint-grid-xyz" → "PGX", not "GPGX" —
	// the leading G (constant across every gardenio crew) no longer steals a glyph.
	// The full name is unchanged (only the avatar collapses).
	if roll.Faces[0].Initials != "PGX" || roll.Faces[0].Name != "gardenio-paint-grid-xyz" {
		t.Errorf("face = %+v, want initials PGX / name gardenio-paint-grid-xyz", roll.Faces[0])
	}
	if roll.Faces[0].URL == "" {
		t.Errorf("face url is empty — should fall back to the crew's bead page")
	}
	// A done/failed crew is never an avatar.
	for _, fc := range roll.Faces {
		if fc.ID == "gd-crewD" || fc.ID == "gd-crewF" {
			t.Errorf("non-live crew %s leaked into the faces strip", fc.ID)
		}
	}
}

// TestPlanCrewBulk_FacesCarryStepContext proves each live/stuck crew face carries
// the STEP number + title the crew is working (ka-0000004037, iter-15), resolved
// from the stepMeta map (the per-plan Grid the handler folds in), matching the
// cockpit roster's "#N: title". A crew whose step is absent from the map reads
// StepNumber 0 / StepTitle "" — the face then renders initials-only, as before.
func TestPlanCrewBulk_FacesCarryStepContext(t *testing.T) {
	f := seedPlansFake()
	// A working crew on step 2 and an OPEN stuck crew on step 5.
	work := seedCrew(f, "gd-crewW", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	work.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC)
	stuck := seedCrew(f, "gd-crewS", "gardenio-stuck-one", "gd-step2", "rate_limited", "in_progress")
	stuck.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC)
	s := crewTestServer(f)

	// stepMeta names the two steps the way the card's mini-grid would (per-group #N
	// + title). gd-step1 is #2 "Wire the route"; gd-step2 is #5 "Ship it".
	stepMeta := map[string]crewStep{
		"gd-step1": {Number: 2, Title: "Wire the route"},
		"gd-step2": {Number: 5, Title: "Ship it"},
	}
	roll := s.planCrewBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1", "gd-step2"},
	}, stepMeta)["gd-plan1"]

	if len(roll.Faces) != 1 || len(roll.StuckFaces) != 1 {
		t.Fatalf("Faces=%d StuckFaces=%d, want 1 working + 1 stuck", len(roll.Faces), len(roll.StuckFaces))
	}
	if roll.Faces[0].StepNumber != 2 || roll.Faces[0].StepTitle != "Wire the route" {
		t.Errorf("working face step = #%d %q, want #2 \"Wire the route\"", roll.Faces[0].StepNumber, roll.Faces[0].StepTitle)
	}
	if roll.StuckFaces[0].StepNumber != 5 || roll.StuckFaces[0].StepTitle != "Ship it" {
		t.Errorf("stuck face step = #%d %q, want #5 \"Ship it\"", roll.StuckFaces[0].StepNumber, roll.StuckFaces[0].StepTitle)
	}
}

// TestPlanCrewBulk_FacesNoStepMetaDegradeGracefully proves that with no stepMeta
// (nil map, or a step missing from it) the crew faces still render — StepNumber 0
// / StepTitle "" — so the enrichment never regresses the initials-only baseline.
func TestPlanCrewBulk_FacesNoStepMetaDegradeGracefully(t *testing.T) {
	f := seedPlansFake()
	work := seedCrew(f, "gd-crewW", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	work.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC)
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if len(roll.Faces) != 1 {
		t.Fatalf("Faces = %d, want 1", len(roll.Faces))
	}
	if roll.Faces[0].StepNumber != 0 || roll.Faces[0].StepTitle != "" {
		t.Errorf("face step = #%d %q, want unresolved (#0, empty) with no stepMeta", roll.Faces[0].StepNumber, roll.Faces[0].StepTitle)
	}
	// The face must still carry its identity (initials + url) — the baseline.
	if roll.Faces[0].Initials == "" || roll.Faces[0].URL == "" {
		t.Errorf("face lost its identity without stepMeta: %+v", roll.Faces[0])
	}
}

// TestPlanCrewBulk_FacesCapAndOverflow proves the avatar strip is capped at
// planCrewFacesMax and the remaining live crew are reported via FacesHidden (the
// "+N" overflow), so a many-crew plan never blows out the card.
func TestPlanCrewBulk_FacesCapAndOverflow(t *testing.T) {
	f := seedPlansFake()
	total := planCrewFacesMax + 3
	for i := range total {
		id := "gd-crew" + string(rune('A'+i))
		b := seedCrew(f, id, "gardenio-crew-"+string(rune('a'+i)), "gd-step1", "working", "in_progress")
		// Stagger update times so ordering is deterministic (not update-time ties).
		// Keep every stamp inside the 1h freshness bar (crewFixtureNow − i min) so
		// all stay LIVE — this test asserts the FACES cap, not the freshness gate.
		b.UpdatedAt = crewFixtureNow.Add(-time.Duration(i) * time.Minute)
	}
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Working != total {
		t.Fatalf("Working = %d, want %d", roll.Working, total)
	}
	if len(roll.Faces) != planCrewFacesMax {
		t.Errorf("rendered faces = %d, want the cap %d", len(roll.Faces), planCrewFacesMax)
	}
	if roll.FacesHidden() != total-planCrewFacesMax {
		t.Errorf("FacesHidden = %d, want %d (live crew beyond the cap)", roll.FacesHidden(), total-planCrewFacesMax)
	}
}

// TestPlanCrewBulk_NoLiveNoFaces proves a plan whose crew are all finished/stuck
// (none working) carries no WORKING avatars — the card then shows only the count
// facets (a closed-failed crew never surfaces as a green working face; whether it
// surfaces as a stuck face is covered by the stuck-faces tests below).
func TestPlanCrewBulk_NoLiveNoFaces(t *testing.T) {
	f := seedPlansFake()
	done := seedCrew(f, "gd-crewD", "gardenio-done-one", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"done"}`
	s := crewTestServer(f)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Total != 1 || roll.Working != 0 {
		t.Fatalf("Total/Working = %d/%d, want 1/0", roll.Total, roll.Working)
	}
	if len(roll.Faces) != 0 || roll.FacesHidden() != 0 {
		t.Errorf("no live crew must yield no faces: Faces=%d FacesHidden=%d", len(roll.Faces), roll.FacesHidden())
	}
}

// --- planCrewBulk stuck-crew faces (ka-0000003793) ---------------------------

// TestPlanCrewBulk_StuckFacesSurfaceOpenStuckCrew proves an OPEN stuck crew
// (rate-limited / failing, not closed) surfaces as a red click-through avatar on
// the index — the one an operator most needs to reach — while a CLOSED-failed
// crew stays in the Failed tally only (its terminal is gone, nothing to jump to).
func TestPlanCrewBulk_StuckFacesSurfaceOpenStuckCrew(t *testing.T) {
	f := seedPlansFake()
	// An open rate-limited crew (stuck, reachable) + an open budget-exceeded crew,
	// plus a working crew and a historical closed-failed crew.
	rl := seedCrew(f, "gd-crewR", "gardenio-rate-limited-one", "gd-step1", "rate_limited", "in_progress")
	rl.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC)
	be := seedCrew(f, "gd-crewE", "gardenio-budget-two", "gd-step1", "budget_exceeded", "in_progress")
	be.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC)
	seedCrew(f, "gd-crewW", "gardenio-working-three", "gd-step1", "working", "in_progress")
	fail := seedCrew(f, "gd-crewF", "gardenio-closed-fail", "gd-step1", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"hit a wall","blockers":"needs IAM"}`
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	// Failed tally counts all three failed/stuck crew (2 open + 1 closed).
	if roll.Failed != 3 {
		t.Fatalf("Failed = %d, want 3 (2 open stuck + 1 closed-failed)", roll.Failed)
	}
	// iter-29 two-tier facet split: OpenStuck is the LOUD reachable subset (2), and
	// ClosedFailed is the CALM historical remainder (1). Failed = OpenStuck +
	// ClosedFailed always holds.
	if roll.OpenStuck() != 2 || !roll.HasOpenStuck() {
		t.Errorf("OpenStuck=%d HasOpenStuck=%v, want 2/true (the reachable stuck subset)", roll.OpenStuck(), roll.HasOpenStuck())
	}
	if roll.ClosedFailed() != 1 || !roll.HasClosedFailed() {
		t.Errorf("ClosedFailed=%d HasClosedFailed=%v, want 1/true (the historical remainder)", roll.ClosedFailed(), roll.HasClosedFailed())
	}
	if roll.OpenStuck()+roll.ClosedFailed() != roll.Failed {
		t.Errorf("OpenStuck(%d) + ClosedFailed(%d) != Failed(%d) — the split must partition Failed", roll.OpenStuck(), roll.ClosedFailed(), roll.Failed)
	}
	// Only the two OPEN stuck crew get avatars.
	if len(roll.StuckFaces) != 2 || !roll.HasStuck() {
		t.Fatalf("StuckFaces=%d HasStuck=%v, want 2 open stuck avatars", len(roll.StuckFaces), roll.HasStuck())
	}
	if roll.StuckFacesHidden() != 0 {
		t.Errorf("StuckFacesHidden = %d, want 0 (both open stuck fit under the cap)", roll.StuckFacesHidden())
	}
	// Newest-active stuck crew leads the strip, and every stuck face is marked +
	// carries a click-through url.
	if roll.StuckFaces[0].ID != "gd-crewE" {
		t.Errorf("first stuck face = %s, want gd-crewE (most-recently-active first)", roll.StuckFaces[0].ID)
	}
	for _, fc := range roll.StuckFaces {
		if !fc.Stuck {
			t.Errorf("stuck face %s must be marked Stuck for the red tint", fc.ID)
		}
		if fc.URL == "" {
			t.Errorf("stuck face %s has empty url — should fall back to the bead page", fc.ID)
		}
		if fc.ID == "gd-crewF" {
			t.Errorf("closed-failed crew %s leaked into the stuck avatars — it has no reachable terminal", fc.ID)
		}
		if fc.ID == "gd-crewW" {
			t.Errorf("working crew %s leaked into the stuck avatars", fc.ID)
		}
	}
	// The working crew is still a (green) working face, never a stuck one.
	if len(roll.Faces) != 1 || roll.Faces[0].ID != "gd-crewW" || roll.Faces[0].Stuck {
		t.Errorf("working faces = %+v, want exactly gd-crewW (not stuck)", roll.Faces)
	}
}

// TestPlanCrewBulk_StuckFacesCapAndOverflow proves the stuck strip is capped at
// planCrewFacesMax and the remaining open stuck crew report via StuckFacesHidden
// (the "+N" overflow), so a many-stuck plan never blows out the card.
func TestPlanCrewBulk_StuckFacesCapAndOverflow(t *testing.T) {
	f := seedPlansFake()
	total := planCrewFacesMax + 2
	for i := range total {
		id := "gd-stuck" + string(rune('A'+i))
		b := seedCrew(f, id, "gardenio-stuck-"+string(rune('a'+i)), "gd-step1", "rate_limited", "in_progress")
		b.UpdatedAt = time.Date(2026, 7, 5, 11, i, 0, 0, time.UTC)
	}
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Failed != total {
		t.Fatalf("Failed = %d, want %d", roll.Failed, total)
	}
	if len(roll.StuckFaces) != planCrewFacesMax {
		t.Errorf("rendered stuck faces = %d, want the cap %d", len(roll.StuckFaces), planCrewFacesMax)
	}
	if roll.StuckFacesHidden() != total-planCrewFacesMax {
		t.Errorf("StuckFacesHidden = %d, want %d (open stuck beyond the cap)", roll.StuckFacesHidden(), total-planCrewFacesMax)
	}
}

// TestPlanCrewBulk_NoOpenStuckNoStuckFaces proves a plan whose only failed crew
// are historical (closed) carries no stuck avatars — HasStuck is false and
// StuckFacesHidden does NOT miscount the closed-failed crew as hidden (it counts
// off the OPEN-stuck set, not Failed).
func TestPlanCrewBulk_NoOpenStuckNoStuckFaces(t *testing.T) {
	f := seedPlansFake()
	fail := seedCrew(f, "gd-crewF", "gardenio-closed-fail", "gd-step1", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"hit a wall","blockers":"needs IAM"}`
	s := crewTestServer(f)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", roll.Failed)
	}
	if roll.HasStuck() || len(roll.StuckFaces) != 0 {
		t.Errorf("a closed-failed crew must not surface as a stuck avatar: HasStuck=%v StuckFaces=%d", roll.HasStuck(), len(roll.StuckFaces))
	}
	if roll.StuckFacesHidden() != 0 {
		t.Errorf("StuckFacesHidden = %d, want 0 (no OPEN stuck crew; closed-failed is not hidden)", roll.StuckFacesHidden())
	}
}

// TestPlanCrewBulk_StuckOldestAge proves the rollup carries the wait-age of the
// OLDEST OPEN stuck crew (the earliest last-activity stamp), NOT the newest — the
// age the chip's stuck facet surfaces (ka-0000002016, iter-13). With the test
// clock at 12:00, an open crew last active at 11:40 has waited 20m; one at 11:50
// has waited 10m. The oldest wait (20m) is the one reported, and it is under the
// 1h stale bar so StuckOldestStale is false.
func TestPlanCrewBulk_StuckOldestAge(t *testing.T) {
	f := seedPlansFake()
	newer := seedCrew(f, "gd-crewN", "gardenio-newer-stuck", "gd-step1", "rate_limited", "in_progress")
	newer.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC) // 10m ago
	older := seedCrew(f, "gd-crewO", "gardenio-older-stuck", "gd-step1", "budget_exceeded", "in_progress")
	older.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC) // 20m ago — the oldest wait
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Failed != 2 {
		t.Fatalf("Failed=%d, want 2 (2 open stuck crew)", roll.Failed)
	}
	if got := roll.StuckOldestAge(); got != "20m" {
		t.Errorf("StuckOldestAge = %q, want %q (the OLDER stuck crew's wait, not the newer)", got, "20m")
	}
	if roll.StuckOldestStale() {
		t.Errorf("StuckOldestStale = true, want false (20m is under the 1h stale bar)")
	}
	// No-new-fan-out budget: the wait-age is a fold over beads already in hand, so
	// the rollup still pays exactly ONE reverse assigned-edge batch + ONE agent
	// hydration batch — no per-crew round-trip for the age.
	if f.batchGetDepsCalls != 1 || f.batchGetBeadsCalls != 1 {
		t.Errorf("fan-out = %d deps + %d beads, want 1 + 1 (the age adds no daemon call)", f.batchGetDepsCalls, f.batchGetBeadsCalls)
	}
}

// TestPlanCrewBulk_StuckOldestStale proves an open stuck crew wedged past the
// staleness threshold (1h) flips StuckOldestStale — the mark that tints the facet
// loud so an egregiously-stuck crew pops out of the board.
func TestPlanCrewBulk_StuckOldestStale(t *testing.T) {
	f := seedPlansFake()
	// Last active at 09:30 — 2.5h before the 12:00 clock, well past the 1h bar.
	old := seedCrew(f, "gd-crewX", "gardenio-long-stuck", "gd-step1", "rate_limited", "in_progress")
	old.UpdatedAt = time.Date(2026, 7, 5, 9, 30, 0, 0, time.UTC)
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if got := roll.StuckOldestAge(); got == "" {
		t.Fatalf("StuckOldestAge is empty, want a datable age")
	}
	if !roll.StuckOldestStale() {
		t.Errorf("StuckOldestStale = false, want true (stuck 2.5h, past the 1h bar)")
	}
}

// TestPlanCrewBulk_ClosedFailedCarriesNoStuckAge proves a plan whose only failed
// crew are CLOSED reports no stuck wait-age — a closed-failed crew's "age" is its
// death, not an ongoing wait, so the facet stays count-only (its bare "⚠ K"),
// mirroring how a closed-failed crew never gets a red avatar.
func TestPlanCrewBulk_ClosedFailedCarriesNoStuckAge(t *testing.T) {
	f := seedPlansFake()
	fail := seedCrew(f, "gd-crewC", "gardenio-closed-fail", "gd-step1", "failed", "closed")
	fail.Fields["wrapup"] = `{"tldr":"hit a wall"}`
	fail.UpdatedAt = time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC) // long ago, but CLOSED
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Failed != 1 {
		t.Fatalf("Failed=%d, want 1 (the closed-failed crew still counts)", roll.Failed)
	}
	// iter-29: an all-closed plan carries ZERO open stuck (no loud act-now facet)
	// and its whole Failed count is the calm historical remainder.
	if roll.OpenStuck() != 0 || roll.HasOpenStuck() {
		t.Errorf("OpenStuck=%d HasOpenStuck=%v, want 0/false (no reachable stuck crew)", roll.OpenStuck(), roll.HasOpenStuck())
	}
	if roll.ClosedFailed() != 1 || !roll.HasClosedFailed() {
		t.Errorf("ClosedFailed=%d HasClosedFailed=%v, want 1/true (the closed failure is the calm historical count)", roll.ClosedFailed(), roll.HasClosedFailed())
	}
	if got := roll.StuckOldestAge(); got != "" {
		t.Errorf("StuckOldestAge = %q, want empty (closed-failed crew contribute no ongoing wait-age)", got)
	}
	if roll.StuckOldestStale() {
		t.Errorf("StuckOldestStale = true, want false (no OPEN stuck crew)")
	}
}

// TestOldestOpenCrewAge_UndatableAndEmpty proves the shared helper handles the
// edge sets: an empty set and a set whose only member carries no timestamp both
// read count-only (empty age, not stale) rather than a bogus "just now".
func TestOldestOpenCrewAge_UndatableAndEmpty(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	if age, stale := oldestOpenCrewAge(nil, now, planCrewStuckStaleThreshold); age != "" || stale {
		t.Errorf("empty set: got (%q,%v), want (\"\",false)", age, stale)
	}
	undatable := []*beadsapi.BeadDetail{{ID: "gd-z", Type: "agent"}} // zero UpdatedAt
	if age, stale := oldestOpenCrewAge(undatable, now, planCrewStuckStaleThreshold); age != "" || stale {
		t.Errorf("undatable set: got (%q,%v), want (\"\",false)", age, stale)
	}
}

// TestPlanCrewBulk_FaceInitialsFallback proves a crew whose name yields no
// initials (empty / pure punctuation) still gets a legible, clickable avatar — a
// bullet glyph — rather than an empty pill.
func TestPlanCrewBulk_FaceInitialsFallback(t *testing.T) {
	f := seedPlansFake()
	b := seedCrew(f, "gd-crewP", "(((", "gd-step1", "working", "in_progress")
	b.Title = "((("
	s := crewTestServer(f)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if len(roll.Faces) != 1 {
		t.Fatalf("Faces = %d, want 1", len(roll.Faces))
	}
	if roll.Faces[0].Initials != "•" {
		t.Errorf("Initials = %q, want the bullet fallback for a no-initials name", roll.Faces[0].Initials)
	}
}

// --- SSE interest set --------------------------------------------------------

func TestBuildPlanEventFilters_IncludesCrew(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	s := crewTestServer(f)

	filters := s.buildPlanEventFilters(context.Background(), "gd-plan1", f.beads["gd-plan1"])
	if _, ok := filters.crewIDs["gd-crewA"]; !ok {
		t.Errorf("crewIDs missing gd-crewA — a crew state change would not re-render the roster")
	}
}

// TestPlanDetail_RendersCrewRoster proves the cockpit renders the crew card
// with the gardener's dispatched agents + their state.
func TestPlanDetail_RendersCrewRoster(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	// Pin the server clock to the fixture clock so the seeded working crew reads
	// FRESH under the iter-42 freshness gate (seedCrew stamps it crewFixtureNow−5m).
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-crew"`,          // the SSE swap anchor
		"gardenio-wire-route-abc", // the crew name
		"plan-crew-member",        // a roster row rendered
		"1 working",               // the header tally
	} {
		if !strings.Contains(body, want) {
			t.Errorf("crew roster missing %q", want)
		}
	}
}

// TestPlanDetail_CrewRosterShowsSpend proves the cockpit crew-roster header
// renders the cumulative crew SPEND chip (ka-0000004218, iter-12) — the parity
// twin of the index chip's spend facet.
func TestPlanDetail_CrewRosterShowsSpend(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	c := seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	c.Fields["total_cost_usd"] = "2.34"
	// Fixture clock so the working crew reads fresh (live) under the iter-42 gate.
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-crew-count-spend") {
		t.Errorf("cockpit crew roster missing the spend chip")
	}
	if !strings.Contains(body, "$2.34 spent") {
		t.Errorf("cockpit crew roster missing the '$2.34 spent' figure")
	}
}

// TestPlanDetail_CrewEmptyState proves a plan with no crew renders the empty
// state (with the stable #plan-crew anchor so the SSE swap can populate it).
func TestPlanDetail_CrewEmptyState(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))

	body := w.Body.String()
	if !strings.Contains(body, `id="plan-crew"`) {
		t.Errorf("empty crew card should still render the #plan-crew anchor for SSE")
	}
	if !strings.Contains(body, "No crew yet") {
		t.Errorf("no-crew plan should render the empty state")
	}
}

// TestHandlePlanEvents_CrewStateFlipEmitsFrame proves a crew agent flipping
// state (working→done) re-renders the roster live.
func TestHandlePlanEvents_CrewStateFlipEmitsFrame(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	// The crew finishes: its agent bead updates.
	enqueuePlanEvent(t, ch, "beads.bead.updated", "gd-crewA")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-crew-updated") {
		t.Errorf("expected a plan-crew-updated frame, got: %s", got)
	}
	if !strings.Contains(got, `id="plan-crew"`) {
		t.Errorf("the frame should carry the re-rendered crew section, got: %s", got)
	}
	// A crew flip must NOT flash the step timeline.
	if strings.Contains(got, "event: plan-updated") {
		t.Errorf("a crew state flip should flash ONLY the crew roster, not the step timeline: %s", got)
	}
}

// TestHandlePlanEvents_NewCrewEdgeEmitsFrame proves a crew dispatched
// mid-session (its assigned edge lands) re-renders the roster so the new agent
// appears live.
func TestHandlePlanEvents_NewCrewEdgeEmitsFrame(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	// Mid-session: the gardener spawns a crew onto gd-step1...
	seedCrew(f, "gd-crewA", "gardenio-fresh-crew", "gd-step1", "spawning", "in_progress")
	// ...and the assigned-edge event announces it (crew → step).
	ch <- beadsapi.SSEEvent{
		Event: "beads.dependency.added",
		Data:  []byte(`{"dependency":{"bead_id":"gd-crewA","depends_on_id":"gd-step1","type":"assigned"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-crew-updated") {
		t.Errorf("expected a plan-crew-updated frame for the new crew edge, got: %s", got)
	}
	if !strings.Contains(got, "gardenio-fresh-crew") {
		t.Errorf("the frame should carry the freshly-spawned crew's name, got: %s", got)
	}
}
