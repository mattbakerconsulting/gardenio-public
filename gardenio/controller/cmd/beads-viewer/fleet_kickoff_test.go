package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// fleet_kickoff_test.go — task ka-0000006826. Operators could not kick off a
// ready plan or fire a gardener iteration from the FLEET views (/plans, /gardeners):
// "Approve & build" lived only on the plan cockpit, and "Run now" only on the
// /gardener/{id} control bar. This surfaces both existing verbs on the index/list,
// reusing the SAME handlers (handlePlanBuild, spawnGardenerRunNowCore).
//
// Two acceptance surfaces, each proved twice — the button RENDERS on the fleet
// card, and the click DISPATCHES the existing handler correctly.

// --- (1) /plans index card: "Approve & build" ---------------------------------

// TestPlansIndex_ApproveBuildButtonOnReadyPlan proves a ready plan with a bound
// gardener renders the "Approve & build" button on the /plans INDEX row — wired
// to the existing POST /plans/{id}/build handler with row=1 (so it re-renders the
// whole row, not the cockpit card the index has no slot for) — INSTEAD of the
// pause/resume toggle that would refuse to resume a still-authoring plan.
func TestPlansIndex_ApproveBuildButtonOnReadyPlan(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{"plan_state": "ready"}
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")

	// Key on the button markup (class attr) — the bare class name also appears in
	// the page's <style> block, so match a string that only the button carries.
	if !strings.Contains(body, `class="bead-action plan-card-approve-btn"`) {
		t.Errorf("a ready plan with a bound gardener should render the Approve & build button on the index; body=%s", snippetN(body, 1200))
	}
	if !strings.Contains(body, `hx-post="/plans/gd-plan1/build"`) {
		t.Errorf("the Approve & build button should POST to the existing /plans/{id}/build handler; body=%s", snippetN(body, 1200))
	}
	// row=1 so the handler re-renders the whole /plans row in place.
	if !strings.Contains(body, `hx-vals='{"row":"1"}'`) {
		t.Errorf("the index Approve & build button should carry row=1 so it re-renders the row; body=%s", snippetN(body, 1200))
	}
	if !strings.Contains(body, `hx-target="closest .plan-card"`) {
		t.Errorf("the index Approve & build button should target the whole row; body=%s", snippetN(body, 1200))
	}
	// It REPLACES the pause toggle for a ready plan — a resume would only be
	// refused behind the R6 build gate, so the toggle must not render here.
	if strings.Contains(body, `id="gardener-pause-gd-cap1"`) {
		t.Errorf("a ready plan should show Approve & build INSTEAD of the pause toggle; body=%s", snippetN(body, 1200))
	}
}

// TestPlansIndex_NoApproveBuildWhenBuilding proves the Approve & build button is
// gated to the ready state: a plan already building shows the pause/resume toggle
// (the loop is live and can be held), NOT the approve button.
func TestPlansIndex_NoApproveBuildWhenBuilding(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{"plan_state": "building"}
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")

	if strings.Contains(body, `class="bead-action plan-card-approve-btn"`) {
		t.Errorf("a building plan should NOT render the Approve & build button; body=%s", snippetN(body, 1200))
	}
	if !strings.Contains(body, `id="gardener-pause-gd-cap1"`) {
		t.Errorf("a building plan should show the pause toggle; body=%s", snippetN(body, 1200))
	}
}

// TestPlansIndex_NoApproveBuildForViewer proves the fleet-level Approve & build
// lever is operator-gated: a read-only viewer sees the state chip but NEITHER the
// approve button NOR the toggle (parity with the pause-toggle gate).
func TestPlansIndex_NoApproveBuildForViewer(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{"plan_state": "ready"}
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	req := plansIndexReq("/plans?operated_by=", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="bead-action plan-card-approve-btn"`) {
		t.Errorf("a read-only viewer must not see the Approve & build lever; body=%s", snippetN(body, 1200))
	}
}

// TestHandlePlanBuild_FromRowApprovesAndReRendersRow proves the DISPATCH half:
// clicking Approve & build from the index (row=1) flips the plan ready→building,
// enables the bound gardener via the SAME setGardenerEnabledCore path, and
// re-renders the whole /plans row (plan_card) — not the cockpit #plan-agent card.
func TestHandlePlanBuild_FromRowApprovesAndReRendersRow(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{"plan_state": "ready"}
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")
	mux := plansAssistantMux(t, f, &fakeScreenClient{})

	req := operatorPost("/plans/gd-plan1/build", "row=1")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, snippetN(w.Body.String(), 600))
	}
	// The plan flipped to building.
	if got := f.beads["gd-plan1"].Fields["plan_state"]; got != "building" {
		t.Errorf("plan_state = %q, want building", got)
	}
	// The bound gardener was enabled (setGardenerEnabledCore writes the JSON bool
	// true onto the gardener bead, NOT the plan epic).
	enabledWrite := false
	var writeIDs []string
	for _, c := range f.updateFieldsHistory {
		writeIDs = append(writeIDs, c.ID)
		if c.ID == "gd-cap1" {
			if v, ok := c.Fields["enabled"].(bool); ok && v {
				enabledWrite = true
			}
		}
	}
	if !enabledWrite {
		t.Errorf("Approve & build should enable the bound gardener (write enabled=true to gd-cap1); writes went to %v", writeIDs)
	}
	// The response is the re-rendered ROW (plan_card), not the cockpit card.
	body := w.Body.String()
	if !strings.Contains(body, `id="plan-card-gd-plan1"`) {
		t.Errorf("Approve & build from the row should re-render the plan_card row; body=%s", snippetN(body, 1200))
	}
}

// TestHandlePlanBuild_FromRowNoGardenerReRendersRow proves a stale/misconfigured
// click (a ready plan whose gardener vanished) re-renders the row with the inline
// "no gardener bound" note rather than a bare HTTP error — the fleet-view failure
// surface degrades gracefully in place.
func TestHandlePlanBuild_FromRowNoGardenerReRendersRow(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{"plan_state": "ready"}
	// No gardener bound.
	mux := plansAssistantMux(t, f, &fakeScreenClient{})

	req := operatorPost("/plans/gd-plan1/build", "row=1")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// row=1 re-renders the row at 200 (HTMX swaps it) even on the no-gardener path.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (row re-render), got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="plan-card-gd-plan1"`) {
		t.Errorf("a no-gardener Approve & build from the row should re-render the row; body=%s", snippetN(body, 1200))
	}
	if !strings.Contains(body, "No gardener is bound") {
		t.Errorf("the row should carry the no-gardener note; body=%s", snippetN(body, 1200))
	}
	// The plan_state must NOT have advanced (no gardener → no build).
	if got := f.beads["gd-plan1"].Fields["plan_state"]; got != "ready" {
		t.Errorf("plan_state = %q, want ready (build refused without a gardener)", got)
	}
}

// --- (2) /gardeners list: "Run now" --------------------------------------------

// TestGardenersList_RunNowButtonOnRows proves each non-closed /gardeners row
// renders the inline "Run now" action wired to the existing POST
// /gardener/{id}/run-now handler (with row=1 so it re-renders the compact action
// in place).
func TestGardenersList_RunNowButtonOnRows(t *testing.T) {
	f := seedGardenersIndexFake(t)
	mux := mkServerWithNow(t, f, gardenersNow)

	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, `class="cap-runnow-btn"`) {
		t.Errorf("each gardener row should render the Run now button; body=%s", snippetN(body, 1500))
	}
	// Both gardeners (gd-cap1, gd-cap2) are open, so both carry the action wired to
	// the existing run-now route.
	for _, id := range []string{"gd-cap1", "gd-cap2"} {
		if !strings.Contains(body, `hx-post="/gardener/`+id+`/run-now"`) {
			t.Errorf("Run now on %s should POST to the existing /gardener/{id}/run-now handler; body=%s", id, snippetN(body, 1500))
		}
		if !strings.Contains(body, `id="gardener-run-now-`+id+`"`) {
			t.Errorf("Run now on %s should carry the stable swap id; body=%s", id, snippetN(body, 1500))
		}
	}
	// The action carries row=1 so the handler re-renders the compact action, not
	// the cockpit control bar the list has no slot for.
	if !strings.Contains(body, `hx-vals='{"row":"1"}'`) {
		t.Errorf("the list Run now action should carry row=1; body=%s", snippetN(body, 1500))
	}
}

// TestGardenersList_NoRunNowForClosedGardener proves a retired (closed) gardener
// omits the Run now action — there is nothing to run, matching the pause toggle's
// own render gate.
func TestGardenersList_NoRunNowForClosedGardener(t *testing.T) {
	f := newFake()
	spec := map[string]any{"name": "retired-cap", "terminal_goal": "done", "runs_spawned": 3, "runs_completed": 3}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:     "gd-capdead",
			Title:  "retired-cap",
			Type:   "trigger",
			Status: "closed",
			Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields: map[string]string{"role": "gardener", "gardener": string(raw)},
		}},
		Total: 1,
	}
	mux := mkServerWithNow(t, f, gardenersNow)

	// A closed gardener is filtered out of the default Live view; the archived
	// filter surfaces it.
	req := httptest.NewRequest("GET", "/gardeners?filter=archived", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-capdead") {
		t.Fatalf("precondition: the retired gardener should render on the archived view; body=%s", snippetN(body, 1500))
	}
	if strings.Contains(body, `id="gardener-run-now-gd-capdead"`) {
		t.Errorf("a retired gardener must not render the Run now action; body=%s", snippetN(body, 1500))
	}
}

// TestHandleGardenerRunNow_FromRowSpawnsAndReRendersAction proves the DISPATCH
// half: clicking Run now from the list (row=1) spawns one iteration via the SAME
// spawnGardenerRunNowCore verb and re-renders the compact inline action (not the
// cockpit #gardener-controls bar) with a confirmation notice.
func TestHandleGardenerRunNow_FromRowSpawnsAndReRendersAction(t *testing.T) {
	f := seedGardenerOpsFake(t)
	// The seed spec has no one_spawn_at_a_time, so run-now spawns even with a
	// live last iteration. Carry row=1 in the form so the handler takes the
	// fleet-view branch (re-render the compact action, not the cockpit bar).
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := adminOpsReq("POST", "/gardener/gd-cap/run-now", url.Values{"row": {"1"}})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, snippetN(w.Body.String(), 600))
	}
	// It spawned one iteration bead (the run-now verb).
	if f.createCalls != 1 {
		t.Errorf("Run now should spawn exactly one iteration; got %d CreateBead calls", f.createCalls)
	}
	// The response is the compact inline action re-rendered with the notice — NOT
	// the cockpit control bar.
	body := w.Body.String()
	if !strings.Contains(body, `id="gardener-run-now-gd-cap"`) {
		t.Errorf("Run now from the list should re-render the compact action; body=%s", snippetN(body, 800))
	}
	if strings.Contains(body, `id="gardener-controls"`) {
		t.Errorf("Run now from the list must NOT re-render the cockpit control bar; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "cap-runnow-note") {
		t.Errorf("Run now from the list should carry a confirmation notice; body=%s", snippetN(body, 800))
	}
}

// TestHandleGardenerRunNow_FromRowConcurrentRunShowsInlineError proves a rejected
// run-now (a run already in flight under one_spawn_at_a_time) re-renders the
// compact action with the inline error at HTTP 200 (so HTMX swaps it) rather than
// a bare http.Error the list row could not surface.
func TestHandleGardenerRunNow_FromRowConcurrentRunShowsInlineError(t *testing.T) {
	f := seedGardenerOpsFake(t)
	// Turn on one_spawn_at_a_time; the seeded last iteration (gd-r3) is live, so
	// the concurrent-run guard trips.
	spec := map[string]any{
		"name":                "delivery",
		"terminal_goal":       "drain",
		"runs_spawned":        3,
		"last_agent_id":       "gd-r3",
		"one_spawn_at_a_time": true,
	}
	raw, _ := json.Marshal(spec)
	f.beads["gd-cap"].Fields["gardener"] = string(raw)
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := adminOpsReq("POST", "/gardener/gd-cap/run-now", url.Values{"row": {"1"}})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// row=1 keeps the response at 200 so HTMX swaps the inline error in.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error swap), got %d", w.Code)
	}
	// No iteration was spawned.
	if f.createCalls != 0 {
		t.Errorf("a rejected run-now must not spawn; got %d CreateBead calls", f.createCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "cap-runnow-note-err") {
		t.Errorf("a rejected run-now should render the inline error skin; body=%s", snippetN(body, 800))
	}
	if !strings.Contains(body, "already in flight") {
		t.Errorf("the inline error should explain the one-spawn rejection; body=%s", snippetN(body, 800))
	}
}
