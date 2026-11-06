package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plans_closeout_test.go — the /plans EXIT RAMP (Matt's ask, Slack thread
// ka-0000001785: "we need a better way to close out plans so we dont have to
// look at every plan we've every made"; decision ka-0000001273 chose build-now —
// tasks ka-0000004467 surfacing + ka-0000005361 bulk).
//
// The load-bearing cases, in order of how badly getting them wrong would hurt:
//
//  1. THE LEGALITY GATE. The daemon admits only building→complete (plus any edge
//     from unset), so a drained draft/ready plan CANNOT reach complete. One of
//     the two live flagged plans is exactly that. A one-tap lever there posts a
//     guaranteed 400, so the gate must hold in BOTH the card render and the bulk
//     handler.
//  2. The daemon's `autoclose-ready` label actually reaching the UI — the signal
//     that was computed, published, and thrown away.
//  3. Terminal reconcile: a CLOSED plan must not read "Done?" beside "closed".
//  4. Precedence: an open blocking escalation still outranks the close-out lane.

// planCloseoutRow builds a minimal enriched row for the pure-projection tests.
func planCloseoutRow(planState string, flagged bool, stepsAllDone bool) planRowView {
	return planRowView{
		ID: "gd-plan1", Status: "open",
		PlanState:       planState,
		CloseoutFlagged: flagged,
		StepsAllDone:    stepsAllDone,
	}
}

// --- 1. the legality gate (pure) ---------------------------------------------

// TestPlanCompleteReachable mirrors the daemon's legalPlanStateEdges: unset and
// building can reach complete; draft and ready cannot. This table IS the contract
// with kbeads plan_state_validation.go — if the daemon's edge set changes, this
// is the test that should fail.
func TestPlanCompleteReachable(t *testing.T) {
	for _, c := range []struct {
		st   planState
		want bool
		why  string
	}{
		{planStateUnset, true, "the backward-compat opt-in edge: any transition FROM unset is legal (39 of 56 live open plans are unset)"},
		{planStateBuilding, true, "the cockpit's own Mark-complete edge"},
		{planStateDraft, false, "the daemon rejects draft→complete"},
		{planStateReady, false, "the daemon rejects ready→complete — and a live flagged plan (ka-0000005352) is exactly this"},
		{planStateComplete, false, "already terminal; the caller renders Reopen instead"},
	} {
		if got := planCompleteReachable(c.st); got != c.want {
			t.Errorf("planCompleteReachable(%q) = %v, want %v — %s", c.st, got, c.want, c.why)
		}
	}
}

// TestPlanCloseoutBadge_GatesLeverOnLegality is the headline of the surfacing
// task: a DRAINED plan whose lifecycle cannot legally reach complete gets the
// badge (honest "this looks finished") but NOT the one-tap lever, plus a reason
// naming the blocked state. The alternative — rendering a lever that posts a
// guaranteed daemon 400 — is worse than rendering nothing.
func TestPlanCloseoutBadge_GatesLeverOnLegality(t *testing.T) {
	// A drained + daemon-flagged plan sitting in `ready` — the live ka-0000005352
	// shape.
	b := planCloseoutBadgeFor(planCloseoutRow("ready", true, true))
	if !b.Show {
		t.Fatalf("a drained plan must still show the badge, got %+v", b)
	}
	if b.CanComplete {
		t.Errorf("a ready plan cannot legally reach complete — the lever must not render")
	}
	if b.BlockedReason == "" {
		t.Errorf("a blocked plan needs a reason naming its lifecycle, got empty")
	}
	if !strings.Contains(b.BlockedReason, "ready") {
		t.Errorf("the reason should name the blocking state, got %q", b.BlockedReason)
	}

	// The dominant live case: plan_state UNSET (39 of 56). The lever DOES render —
	// the daemon's backward-compat carve-out makes unset→complete legal.
	b = planCloseoutBadgeFor(planCloseoutRow("", false, true))
	if !b.Show || !b.CanComplete {
		t.Errorf("an UNSET drained plan is the dominant case and must offer the lever, got %+v", b)
	}
	if b.BlockedReason != "" {
		t.Errorf("an unblocked plan needs no reason, got %q", b.BlockedReason)
	}
}

// TestPlanCloseoutBadge_HiddenOnHealthyPlan proves the badge is not just always
// on: a plan with open steps and no daemon flag renders nothing, so the signal
// stays meaningful.
func TestPlanCloseoutBadge_HiddenOnHealthyPlan(t *testing.T) {
	if b := planCloseoutBadgeFor(planCloseoutRow("building", false, false)); b.Show {
		t.Errorf("a plan with open work must show no close-out badge, got %+v", b)
	}
}

// TestPlanCloseoutBadge_FlaggedOutranksDerived proves the two roads are
// distinguished: the DAEMON-flagged case is marked Flagged (it verified every
// child closed AND MR work merged), the viewer-derived one is not — the card tiers
// them (solid vs dashed) on this bit.
func TestPlanCloseoutBadge_FlaggedOutranksDerived(t *testing.T) {
	// Flagged but NOT all-steps-done: the daemon's flag alone must surface the
	// badge. This is the case a StepsAllDone-only implementation would miss —
	// a flagged plan can have no rendered steps at all.
	b := planCloseoutBadgeFor(planCloseoutRow("building", true, false))
	if !b.Show || !b.Flagged {
		t.Errorf("the daemon flag alone must surface a flagged badge, got %+v", b)
	}
	// Drained per the viewer but unflagged: still shown, not marked Flagged.
	// Measured live: 5 open plans had zero open children while only 2 carried the
	// label, because flagPlanEpicReady fires only from the auto-close cascade.
	b = planCloseoutBadgeFor(planCloseoutRow("building", false, true))
	if !b.Show {
		t.Errorf("a drained-but-unflagged plan must still surface, got %+v", b)
	}
	if b.Flagged {
		t.Errorf("an unflagged plan must not claim the daemon's higher-confidence flag")
	}
}

// --- 2 + 3. the enrich seam: label read + terminal reconcile ------------------

// TestPlansIndex_SurfacesDaemonAutocloseFlag is the "stop throwing the signal
// away" test: an `autoclose-ready` plan epic renders the drained badge AND the
// one-tap Mark complete on the index. Before this work, `grep autoclose` across
// the whole viewer returned one hit — a code comment.
func TestPlansIndex_SurfacesDaemonAutocloseFlag(t *testing.T) {
	f := seedPlansFake()
	// The daemon's flag, plus plan_state=building so completion is legally
	// reachable and the lever renders.
	f.beads["gd-plan1"].Labels = append(f.beads["gd-plan1"].Labels, planCloseoutReadyLabel)
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateBuilding)}

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	if !strings.Contains(body, `<span class="chip plan-closeout-chip plan-closeout-chip-flagged`) {
		t.Errorf("an autoclose-ready plan must render the flagged close-out badge\nbody=%s", snippetN(body, 1600))
	}
	if !strings.Contains(body, `hx-post="/plans/gd-plan1/complete"`) {
		t.Errorf("a legally-completable drained plan must offer one-tap Mark complete\nbody=%s", snippetN(body, 1600))
	}
}

// TestPlanBoardLanes_CloseoutColumn proves the assembled board grows a "Done?"
// column when a plan drains — and, per planBoardCoreLanes, does NOT render an
// empty one when nothing is drained (an empty "nothing looks finished" column is
// noise, unlike an empty In progress / Stalled where the absence IS the signal).
func TestPlanBoardLanes_CloseoutColumn(t *testing.T) {
	drained := planRowView{ID: "gd-plan1", Status: "open", PlanState: "building", StepsAllDone: true}
	lanes := planBoardLanes([]planRowView{drained}, nil, planStatusActive, false)

	var found *planBoardLane
	for i := range lanes {
		if lanes[i].Key == planCloseoutBoardLane {
			found = &lanes[i]
		}
	}
	if found == nil {
		t.Fatalf("a drained plan must produce a Done? lane, got lanes %+v", laneKeys(lanes))
	}
	if found.Label != "Done?" {
		t.Errorf("lane label = %q, want %q — the question mark is deliberate (these plans are still OPEN)", found.Label, "Done?")
	}
	if len(found.Rows) != 1 || found.Rows[0].ID != "gd-plan1" {
		t.Errorf("the drained plan should be the lane's only row, got %+v", found.Rows)
	}
	// It sits immediately BEFORE Closed — the last stop on the ramp.
	keys := laneKeys(lanes)
	closeoutAt, closedAt := -1, -1
	for i, k := range keys {
		switch k {
		case planCloseoutBoardLane:
			closeoutAt = i
		case "closed":
			closedAt = i
		}
	}
	if closeoutAt < 0 || closedAt < 0 || closeoutAt > closedAt {
		t.Errorf("Done? must come before Closed, got order %v", keys)
	}

	// Nothing drained → no empty Done? column.
	healthy := planRowView{ID: "gd-plan2", Status: "open", PlanState: "building", StepsClosed: 1}
	for _, l := range planBoardLanes([]planRowView{healthy}, nil, planStatusActive, false) {
		if l.Key == planCloseoutBoardLane {
			t.Errorf("no plan is drained — the Done? column must not render empty")
		}
	}
}

// laneKeys projects the lane keys in render order for readable assertions.
func laneKeys(lanes []planBoardLane) []string {
	out := make([]string, 0, len(lanes))
	for _, l := range lanes {
		out = append(out, l.Key)
	}
	return out
}

// TestPlansIndex_CloseoutLeverHiddenOnUnreachableLifecycle is the render-side
// half of the legality gate: a drained + flagged plan in `ready` shows the badge
// but NO Mark-complete button — the live ka-0000005352 case. A dead button here
// would post a request the daemon is guaranteed to reject.
func TestPlansIndex_CloseoutLeverHiddenOnUnreachableLifecycle(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Labels = append(f.beads["gd-plan1"].Labels, planCloseoutReadyLabel)
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateReady)}

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	if !strings.Contains(body, `<span class="chip plan-closeout-chip`) {
		t.Errorf("the badge must still render on a blocked-lifecycle drained plan\nbody=%s", snippetN(body, 1600))
	}
	if strings.Contains(body, `hx-post="/plans/gd-plan1/complete"`) {
		t.Errorf("a ready plan cannot reach complete — no one-tap lever may render\nbody=%s", snippetN(body, 1600))
	}
}

// TestPlansIndex_NoCloseoutBadgeOnClosedPlan pins the terminal reconcile (the
// iter-25/27/38/41 family): the daemon stamps `autoclose-ready` at DRAIN time and
// never removes it on the later close, so a genuinely-closed plan still carries
// the label. It must not render "Done?" beside "closed" — that card-vs-truth
// contradiction is exactly what those iterations killed for every sibling signal.
func TestPlansIndex_NoCloseoutBadgeOnClosedPlan(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Labels = append(f.beads["gd-plan1"].Labels, planCloseoutReadyLabel)
	f.beads["gd-plan1"].Status = "closed"

	body := plansIndexBodyOperator(t, f, "/plans?status=all&operated_by=")
	// Match the rendered ELEMENT, not the bare class name: the page embeds
	// `.plan-closeout-chip { … }` in its own <style> block, so a substring match on
	// the class alone passes/fails on the stylesheet regardless of the card.
	if strings.Contains(body, `<span class="chip plan-closeout-chip`) {
		t.Errorf("a CLOSED plan is already done — it must render no close-out badge\nbody=%s", snippetN(body, 1600))
	}
	if strings.Contains(body, `hx-post="/plans/gd-plan1/complete"`) {
		t.Errorf("a CLOSED plan must offer no one-tap Mark complete\nbody=%s", snippetN(body, 1600))
	}
}

// --- 4. lane precedence -------------------------------------------------------

// TestPlanBoardLaneKey_Closeout pins the derived lane, including the two
// precedence calls that are easy to get backwards.
func TestPlanBoardLaneKey_Closeout(t *testing.T) {
	for _, c := range []struct {
		name string
		row  planRowView
		want string
	}{
		{
			"drained plan lanes into Done?",
			planRowView{Status: "open", PlanState: "building", StepsAllDone: true},
			planCloseoutBoardLane,
		},
		{
			"daemon-flagged plan lanes into Done? even with no rendered steps",
			planRowView{Status: "open", PlanState: "building", CloseoutFlagged: true},
			planCloseoutBoardLane,
		},
		{
			// The precedence that matters: an open blocking escalation is a real
			// outstanding privileged human op. Laning it into Done? would hide the ⛔
			// badge behind a "retire me" invitation.
			"an open blocking escalation outranks Done?",
			planRowView{Status: "open", PlanState: "building", StepsAllDone: true,
				Escalations: planEscalationInfo{Count: 1}},
			planBoardLaneStalled,
		},
		{
			// A closed plan is terminal — it belongs in Closed, never in the
			// still-open Done? lane, whatever its leftover flag says.
			"a closed plan stays in Closed",
			planRowView{Status: "closed", PlanState: "complete", CloseoutFlagged: true},
			"closed",
		},
		{
			"a plan with open work is untouched by the new lane",
			planRowView{Status: "open", PlanState: "building", StepsClosed: 1},
			"in_progress",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := planBoardLaneKey(c.row); got != c.want {
				t.Errorf("planBoardLaneKey = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPlanAttention_CloseoutRanksAboveResidualBelowStalled pins the ranker tier:
// a drained plan must surface above the healthy feed (one tap permanently shrinks
// the pile — the highest-leverage action per unit of operator effort) but below a
// stalled plan (which needs real forward work, not a confirmation). Cheap ≠
// urgent, and the tier must agree with planBoardLaneKey's own precedence.
func TestPlanAttention_CloseoutRanksAboveResidualBelowStalled(t *testing.T) {
	drained := planRowView{Status: "open", StepsAllDone: true}
	if got := planAttention(drained); got != planAttentionCloseout {
		t.Fatalf("a drained plan should rank closeout, got %v", got)
	}
	healthy := planRowView{Status: "open"}
	if planAttention(drained) >= planAttention(healthy) {
		t.Errorf("a drained plan must outrank the residual feed")
	}
	stalled := planRowView{Status: "open", Stall: planStallInfo{Stalled: true}}
	if planAttention(stalled) >= planAttention(drained) {
		t.Errorf("a stalled plan must outrank a drained one — it needs real work, not a confirm")
	}
}

// --- 5. the bulk verb ---------------------------------------------------------

// TestPlansBulkComplete_RetiresEverySelectedPlan is the headline of the bulk task:
// "retire 20 plans in one POST instead of 20 clicks". Both selected plans are
// stamped complete AND closed through the shared completePlanCore.
func TestPlansBulkComplete_RetiresEverySelectedPlan(t *testing.T) {
	f := seedPlansFake()
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Fields:    map[string]string{planStateFieldKey: string(planStateBuilding)},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateBuilding)}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("complete", "gd-plan1", "gd-plan2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	// Both epics were stamped plan_state=complete …
	var stamped []string
	for _, c := range f.updateFieldsHistory {
		if c.Fields[planStateFieldKey] == string(planStateComplete) {
			stamped = append(stamped, c.ID)
		}
	}
	if len(stamped) != 2 {
		t.Errorf("want both plans stamped complete, got %v (history=%+v)", stamped, f.updateFieldsHistory)
	}
	// … and both epics were CLOSED (the observable "done" the board keys on).
	// The fake's CloseBeadAs flips the bead's own Status, so assert on that.
	for _, id := range []string{"gd-plan1", "gd-plan2"} {
		if got := f.beads[id].Status; got != "closed" {
			t.Errorf("plan %s status = %q, want closed", id, got)
		}
	}
	if f.closeCalls != 2 {
		t.Errorf("CloseBeadAs: want 2 calls, got %d", f.closeCalls)
	}
}

// TestPlansBulkComplete_RefusesUnreachableLifecyclePerPlan is the bulk half of the
// legality gate, and the reason it is enforced server-side rather than only in the
// card render: the card hides the lever on a draft/ready plan, but "Select all"
// can absolutely put one in the form. Such a plan is refused with a message naming
// its lifecycle while its legally-completable sibling still retires — partial
// success, not an aborted batch.
func TestPlansBulkComplete_RefusesUnreachableLifecyclePerPlan(t *testing.T) {
	f := seedPlansFake()
	// plan1 is a draft (cannot reach complete); plan2 is building (can).
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateDraft)}
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Fields:    map[string]string{planStateFieldKey: string(planStateBuilding)},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("complete", "gd-plan1", "gd-plan2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 (partial success), got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	// The draft plan was NEVER closed and never stamped …
	if got := f.beads["gd-plan1"].Status; got == "closed" {
		t.Errorf("a draft plan must not be closed — the daemon would reject the state write")
	}
	for _, c := range f.updateFieldsHistory {
		if c.ID == "gd-plan1" && c.Fields[planStateFieldKey] == string(planStateComplete) {
			t.Errorf("a draft plan must not be stamped complete (the daemon rejects draft→complete)")
		}
	}
	// … the building sibling still retired …
	if got := f.beads["gd-plan2"].Status; got != "closed" {
		t.Errorf("the legally-completable sibling should still retire, status = %q", got)
	}
	// … and the refusal names the blocking lifecycle inline.
	if !strings.Contains(w.Body.String(), "gd-plan1 is draft") {
		t.Errorf("expected an inline refusal naming the draft lifecycle\nbody=%s", snippetN(w.Body.String(), 800))
	}
}

// TestPlansBulkComplete_DisablesBoundGardener proves the bulk verb goes through the
// SAME completePlanCore composition as the single-plan lever — disable the gardener
// FIRST, then stamp, then close — rather than being a second implementation of
// "what it means to finish a plan" that could drift on ordering.
func TestPlansBulkComplete_DisablesBoundGardener(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateBuilding)}
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("complete", "gd-plan1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	var disabled bool
	for _, c := range f.updateFieldsHistory {
		if c.ID == "gd-cap1" && c.Fields["enabled"] == false {
			disabled = true
		}
	}
	if !disabled {
		t.Errorf("the bound gardener must be disabled so its loop stops firing, history=%+v", f.updateFieldsHistory)
	}
	if got := f.beads["gd-plan1"].Status; got != "closed" {
		t.Errorf("the plan should have been closed, status = %q", got)
	}
}

// TestPlansBulkComplete_AlreadyCompleteIsIdempotent proves a double-submit (or a
// plan another tab just retired) reads as success, not failure — matching
// handlePlanComplete's own early return.
func TestPlansBulkComplete_AlreadyCompleteIsIdempotent(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateComplete)}
	f.beads["gd-plan1"].Status = "closed"
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("complete", "gd-plan1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if f.closeCalls != 0 {
		t.Errorf("an already-complete plan must not be closed a second time, got %d close calls", f.closeCalls)
	}
}

// TestPlansBulkComplete_RequiresOperator proves the close-out verb carries the same
// route-level operator gate as every other /plans write: a read-only viewer never
// reaches the handler and nothing is retired.
func TestPlansBulkComplete_RequiresOperator(t *testing.T) {
	f := seedPlansFake()
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := plansBulkReq("complete", "gd-plan1")
	req.Header.Set("X-Auth-Request-Email", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("a read-only viewer must not reach the bulk complete handler; got 200")
	}
	if f.closeCalls != 0 {
		t.Errorf("a rejected viewer bulk must retire nothing, got %d close calls", f.closeCalls)
	}
}
