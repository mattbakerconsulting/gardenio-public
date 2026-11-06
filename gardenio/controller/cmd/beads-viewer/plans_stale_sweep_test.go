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

// plans_stale_sweep_test.go — the ?status=stale SWEEP view + its bulk Abandon
// retire verb (task ka-0000004351, epic ka-0000007470). The sweep is the
// AGE-BASED half of the close-out predicate — "no activity in N days" — which is
// a genuinely different pile from the zero-open-children "Done?" lane MR !7208
// shipped: a plan can idle for a month with every step still open. The sweep
// subtracts the enriched rows to only those FreshnessStale already flagged, and
// pairs the existing bulk Mark complete with a bulk Abandon so a stale idle plan
// that was never approved (draft/ready — which cannot legally reach complete) has
// an honest one-gesture exit.

// --- the sweep post-filter (handler) -----------------------------------------

// seedStaleSweepFake serves two plans on the index: one FRESH (recent activity)
// and one STALE (last touched well past the 7d freshness bar). Both are open with
// live steps — neither is drained — so the ONLY thing that puts the stale one in
// the sweep is its age, exactly the signal the age-based half is for.
func seedStaleSweepFake() *fakeDaemon {
	f := seedPlansFake()
	// plan1 is the FRESH plan — touched moments before "now".
	fresh := f.beads["gd-plan1"]
	fresh.Status = "in_progress"
	fresh.UpdatedAt = planAssistantTestNow.Add(-1 * time.Hour)
	// plan2 is the STALE plan — no activity for 30 days.
	stale := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Idle plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: planAssistantTestNow.Add(-30 * 24 * time.Hour),
	}
	f.beads["gd-plan2"] = stale
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{fresh, stale}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 1 // has an open child — NOT drained
	return f
}

// TestPlansIndex_StaleSweepSubtractsFreshPlans is the headline of the age-based
// half: ?status=stale renders ONLY the plans past the freshness bar, dropping the
// fresh ones the default view would show. Neither plan is drained, so this is
// purely the age signal at work.
func TestPlansIndex_StaleSweepSubtractsFreshPlans(t *testing.T) {
	f := seedStaleSweepFake()
	body := plansIndexBodyOperator(t, f, "/plans?operated_by=&status=stale")

	if !strings.Contains(body, "gd-plan2") {
		t.Errorf("the stale (30d-idle) plan must appear in the sweep\nbody=%s", snippetN(body, 1600))
	}
	if strings.Contains(body, `value="gd-plan1"`) {
		t.Errorf("the FRESH plan must be subtracted from the stale sweep\nbody=%s", snippetN(body, 1600))
	}
}

// TestPlansIndex_StaleSweepLeadRenders proves the sweep carries its explanatory
// lead + the Abandon verb so an operator knows what the view is and how to clear
// it. The lead is gated to the stale filter with a non-empty result.
func TestPlansIndex_StaleSweepLeadRenders(t *testing.T) {
	f := seedStaleSweepFake()
	body := plansIndexBodyOperator(t, f, "/plans?operated_by=&status=stale")

	for _, want := range []string{
		"Stale sweep",                    // the lead
		`hx-post="/plans/bulk/abandon"`,  // the retire verb the sweep needs
		`hx-post="/plans/bulk/complete"`, // still offered for approved/legacy plans
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stale sweep missing %q\nbody=%s", want, snippetN(body, 2000))
		}
	}
}

// TestPlansIndex_StaleChipRenders proves the Stale filter chip is offered in the
// status control row so the sweep is discoverable, not URL-only.
func TestPlansIndex_StaleChipRenders(t *testing.T) {
	f := seedPlansFake()
	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	if !strings.Contains(body, "status=stale") {
		t.Errorf("the status control row should offer a Stale chip\nbody=%s", snippetN(body, 2000))
	}
}

// --- bulk Abandon (the sweep's retire verb) ----------------------------------

// TestPlansBulkAbandon_RetiresEverySelectedPlan proves the sweep's headline: a
// multi-select of stale plans is retired in ONE POST — each stamped
// plan_state=abandoned AND closed through the shared abandonPlanCore.
func TestPlansBulkAbandon_RetiresEverySelectedPlan(t *testing.T) {
	f := seedPlansFake()
	// Two never-approved plans — the exact pile the sweep surfaces that Mark
	// complete would refuse (draft/ready cannot reach complete).
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateDraft)}
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Fields:    map[string]string{planStateFieldKey: string(planStateReady)},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 1
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("abandon", "gd-plan1", "gd-plan2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	// Both epics were stamped plan_state=abandoned …
	var stamped []string
	for _, c := range f.updateFieldsHistory {
		if c.Fields[planStateFieldKey] == string(planStateAbandoned) {
			stamped = append(stamped, c.ID)
		}
	}
	if len(stamped) != 2 {
		t.Errorf("want both plans stamped abandoned, got %v (history=%+v)", stamped, f.updateFieldsHistory)
	}
	// … and both epics were CLOSED. This is what a draft/ready plan CANNOT reach
	// via complete — abandon is the honest exit that actually retires the sweep.
	for _, id := range []string{"gd-plan1", "gd-plan2"} {
		if got := f.beads[id].Status; got != "closed" {
			t.Errorf("plan %s status = %q, want closed", id, got)
		}
	}
	if f.closeCalls != 2 {
		t.Errorf("CloseBeadAs: want 2 calls, got %d", f.closeCalls)
	}
}

// TestPlansBulkAbandon_DisablesBoundGardener proves the bulk verb goes through the
// SAME abandonPlanCore composition as the single-plan lever — disable the gardener
// FIRST, then stamp abandoned, then close — rather than a second implementation
// that could drift on ordering.
func TestPlansBulkAbandon_DisablesBoundGardener(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateReady)}
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("abandon", "gd-plan1"))

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

// TestPlansBulkAbandon_AlreadyTerminalIsIdempotent proves a double-submit (or a
// plan another tab just retired) reads as success, not failure — matching
// handlePlanAbandon's own early return. Applies to a plan already abandoned OR
// already complete.
func TestPlansBulkAbandon_AlreadyTerminalIsIdempotent(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateAbandoned)}
	f.beads["gd-plan1"].Status = "closed"
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("abandon", "gd-plan1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if f.closeCalls != 0 {
		t.Errorf("an already-terminal plan must not be closed a second time, got %d close calls", f.closeCalls)
	}
}

// TestPlansBulkAbandon_RequiresOperator proves the retire verb carries the same
// route-level operator gate as every other /plans write.
func TestPlansBulkAbandon_RequiresOperator(t *testing.T) {
	f := seedPlansFake()
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := plansBulkReq("abandon", "gd-plan1")
	req.Header.Set("X-Auth-Request-Email", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("a read-only viewer must not reach the bulk abandon handler; got 200")
	}
	if f.closeCalls != 0 {
		t.Errorf("a rejected viewer bulk must retire nothing, got %d close calls", f.closeCalls)
	}
}
