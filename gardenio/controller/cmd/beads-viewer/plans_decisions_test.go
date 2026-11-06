package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// seedPlanInlineDecisionFake extends seedPlansFake so the plan's clarifying
// question (gd-dec1) carries real options + a prompt AND is present in f.beads
// (so the resolve/dismiss handlers' GetBead succeeds). The base seed only
// stashes the decision in decisionResp for the list query; the inline
// answer-in-place routes (task ka-0000003157) additionally GetBead it.
func seedPlanInlineDecisionFake() *fakeDaemon {
	f := seedPlansFake()
	dec := &beadsapi.BeadDetail{
		ID:     "gd-dec1",
		Title:  "Which identity marker?",
		Type:   "decision",
		Status: "open",
		Fields: map[string]string{
			"prompt":       "Label the epic 'plan' or use a title prefix?",
			"requested_by": "gardenio-plan-gardener-1",
			"options": `[
				{"id":"label","short":"Use label","label":"Add a plan label","artifact_type":"plan"},
				{"id":"prefix","short":"Use prefix","label":"Prefix the title","artifact_type":"plan"}
			]`,
		},
		UpdatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.beads["gd-dec1"] = dec
	// The list query (planDecisions / planDecisionIDs) returns the same bead.
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{dec},
		Total: 1,
	}
	// The decision is a DIRECT parent-child child of the plan epic. The
	// resolve/dismiss membership check (loadPlanDecision) reads the decision's
	// FORWARD parent-child edge and requires it to point at the plan.
	if f.deps == nil {
		f.deps = map[string][]beadsapi.Dependency{}
	}
	f.deps["gd-dec1"] = []beadsapi.Dependency{
		{BeadID: "gd-dec1", DependsOnID: "gd-plan1", Type: "parent-child"},
	}
	return f
}

// seedPinnedDecisionFake extends seedPlansFake with an OPEN decision homed to
// the plan ONLY via fields.requesting_plan (Layer 2, ka-0000007801) — it is NOT
// a parent-child child of the epic, so the transitive Parent=epicID query does
// not reach it. This is the orphan class: born inside the plan, its origin step
// closed, but the create-time pin re-homes it.
func seedPinnedDecisionFake() *fakeDaemon {
	f := seedPlansFake()
	pinned := &beadsapi.BeadDetail{
		ID:     "gd-decpin",
		Title:  "Which rollout order?",
		Type:   "decision",
		Status: "open",
		Fields: map[string]string{
			"prompt":          "Ship A before B, or B before A?",
			"requesting_plan": "gd-plan1",
		},
		UpdatedAt: time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC),
	}
	f.beads["gd-decpin"] = pinned
	// Deliberately NO parent-child edge to gd-plan1 and NOT in decisionResp — the
	// pin is its only path to the plan.
	return f
}

// TestPlanDecisions_PinCollectsOrphan proves an open decision reachable ONLY via
// the requesting_plan pin (no parent-child edge, absent from the subtree query)
// is still rendered under the plan — the load-bearing Layer 2 behavior.
func TestPlanDecisions_PinCollectsOrphan(t *testing.T) {
	f := seedPinnedDecisionFake()
	s := newTestServerForEsc(t, f) // NewServer wires the markdown renderer buildDecisionView needs

	views := s.planDecisions(context.Background(), "gd-plan1", true)
	var found *planDecisionView
	for i := range views {
		if views[i].ID == "gd-decpin" {
			found = &views[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("pinned decision gd-decpin missing from planDecisions; got %+v", views)
	}
	if found.Resolved {
		t.Errorf("pinned open decision should render as unresolved")
	}
	if found.Title != "Which rollout order?" {
		t.Errorf("pinned decision title = %q, want 'Which rollout order?'", found.Title)
	}
	// The parent-subtree decision (gd-dec1) is still collected alongside it.
	var haveSubtree bool
	for _, v := range views {
		if v.ID == "gd-dec1" {
			haveSubtree = true
		}
	}
	if !haveSubtree {
		t.Errorf("subtree decision gd-dec1 must still be collected alongside the pin; got %+v", views)
	}
}

// TestPlanDecisions_PinDedupesWithSubtree proves a decision reached by BOTH paths
// (a parent-child child that is ALSO pinned) renders exactly once — no
// double-render.
func TestPlanDecisions_PinDedupesWithSubtree(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	// The subtree decision gd-dec1 is ALSO pinned to the plan.
	f.beads["gd-dec1"].Fields["requesting_plan"] = "gd-plan1"
	s := newTestServerForEsc(t, f) // NewServer wires the markdown renderer

	views := s.planDecisions(context.Background(), "gd-plan1", true)
	count := 0
	for _, v := range views {
		if v.ID == "gd-dec1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("decision on both paths must render once, rendered %d times", count)
	}
}

// TestPlanDecisionIDs_PinInInterestSet proves the SSE interest set (planDecisionIDs)
// includes the pinned decision, so its answer/close streams live — the interest
// set stays == the rendered set (both route through planDecisionBeads).
func TestPlanDecisionIDs_PinInInterestSet(t *testing.T) {
	f := seedPinnedDecisionFake()
	s := testServerWithDaemon(f)

	ids := s.planDecisionIDs(context.Background(), "gd-plan1")
	var have bool
	for _, id := range ids {
		if id == "gd-decpin" {
			have = true
		}
	}
	if !have {
		t.Errorf("pinned decision gd-decpin missing from the SSE interest set; got %v", ids)
	}
}

// TestHandlePlanDetail_InlineDecisionOperatorChips proves the plan cockpit
// renders the inline answer surface for an operator: one option POST form per
// option (posting to the plan-scoped resolve route), the free-text "Other"
// answer form, and the Dismiss form — all targeting #plan-decisions so the
// answer lands in place (task ka-0000003157). The old out-link to /{id} for the
// question text is gone for operators (they read + answer it here).
func TestHandlePlanDetail_InlineDecisionOperatorChips(t *testing.T) {
	mux := mkServer(t, seedPlanInlineDecisionFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-decisions"`,
		`data-open-decisions="1"`, // header-sync attr
		"Label the epic",          // the markdown prompt, rendered in place
		`hx-post="/plans/gd-plan1/decisions/gd-dec1/resolve"`,  // option + other forms post here
		`hx-post="/plans/gd-plan1/decisions/gd-dec1/dismiss"`,  // dismiss form
		`hx-target="#plan-decisions"`,                          // swap the whole section
		`<input type="hidden" name="option_id" value="label">`, // one-click option form
		`<input type="hidden" name="option_id" value="prefix">`,
		`name="response_text"`, // free-text Other answer
		`name="reason"`,        // dismiss reason
		"Use label",            // option short label rendered
	} {
		if !strings.Contains(body, want) {
			t.Errorf("operator cockpit missing %q", want)
		}
	}
	// The header chip is present and NOT hidden (1 open).
	if strings.Contains(body, `id="plan-open-decisions-chip" class="chip chip-attention" href="#plan-decisions" hidden`) {
		t.Error("open-decisions header chip should not be hidden when 1 question is open")
	}
}

// TestHandlePlanDetail_InlineDecisionViewerReadonly proves a non-operator gets
// NO resolve/dismiss controls — only the read-only click-out link to the
// canonical decision card (the answer surface is operator-gated at the template
// AND the route layer).
func TestHandlePlanDetail_InlineDecisionViewerReadonly(t *testing.T) {
	mux := mkServer(t, seedPlanInlineDecisionFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	// Explicit viewer role: viewerCanOperate treats a role-less request as the
	// dev bypass (operator), so a genuine read-only viewer must carry RoleViewer.
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "/decisions/gd-dec1/resolve") || strings.Contains(body, "/decisions/gd-dec1/dismiss") {
		t.Error("read-only viewer must not see inline resolve/dismiss forms")
	}
	if !strings.Contains(body, `href="/gd-dec1"`) {
		t.Error("read-only viewer should keep the click-out link to the decision bead")
	}
}

// TestHandlePlanResolveDecision_OperatorResolvesInPlace proves the plan-scoped
// resolve route answers the decision via daemon.ResolveDecision and re-renders
// the #plan-decisions section (not one decision card), so the answer lands in
// place and the next question can appear.
func TestHandlePlanResolveDecision_OperatorResolvesInPlace(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/decisions/gd-dec1/resolve",
		strings.NewReader("option_id=label"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.resolveCalls != 1 || f.resolveLastID != "gd-dec1" {
		t.Errorf("expected ResolveDecision(gd-dec1); got calls=%d last=%s", f.resolveCalls, f.resolveLastID)
	}
	if f.resolveLastReq.SelectedOption != "label" {
		t.Errorf("expected SelectedOption=label; got %q", f.resolveLastReq.SelectedOption)
	}
	if f.resolveLastReq.RespondedBy != "op@acme.example" {
		t.Errorf("expected RespondedBy=op@acme.example; got %q", f.resolveLastReq.RespondedBy)
	}
	// The response is the re-rendered whole section, not a bare decision card.
	if !strings.Contains(w.Body.String(), `id="plan-decisions"`) {
		t.Errorf("resolve should re-render #plan-decisions; got: %s", w.Body.String())
	}
}

// TestHandlePlanResolveDecision_OtherFreeText proves the free-text "Other"
// answer resolves with an empty option and the typed response.
func TestHandlePlanResolveDecision_OtherFreeText(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/decisions/gd-dec1/resolve",
		strings.NewReader("option_id=&response_text=neither%2C+do+X+instead"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.resolveCalls != 1 || f.resolveLastReq.SelectedOption != "" {
		t.Errorf("expected a free-text resolve with empty option; got calls=%d option=%q", f.resolveCalls, f.resolveLastReq.SelectedOption)
	}
	if f.resolveLastReq.ResponseText != "neither, do X instead" {
		t.Errorf("expected the typed response; got %q", f.resolveLastReq.ResponseText)
	}
}

// TestHandlePlanResolveDecision_RejectsEmpty proves a resolve with neither an
// option nor a response is rejected (and never hits the daemon).
func TestHandlePlanResolveDecision_RejectsEmpty(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/decisions/gd-dec1/resolve",
		strings.NewReader("option_id=&response_text="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// The re-render carries the error banner in-band under 200 so htmx swaps it
	// (htmx discards 4xx/5xx bodies), and the daemon is never touched.
	if w.Code != http.StatusOK {
		t.Fatalf("empty resolve should re-render the section (200 + inline banner); got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Pick an option or write a response.") {
		t.Errorf("expected the inline error banner; got: %s", w.Body.String())
	}
	if f.resolveCalls != 0 {
		t.Errorf("empty resolve should not call the daemon; got %d calls", f.resolveCalls)
	}
}

// TestHandlePlanDismissDecision_OperatorDismisses proves the plan-scoped
// dismiss route cancels the decision via daemon.CancelDecision and re-renders
// the section.
func TestHandlePlanDismissDecision_OperatorDismisses(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/decisions/gd-dec1/dismiss",
		strings.NewReader("reason=not+relevant"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.cancelCalls != 1 || f.cancelLastID != "gd-dec1" {
		t.Errorf("expected CancelDecision(gd-dec1); got calls=%d last=%s", f.cancelCalls, f.cancelLastID)
	}
	if f.cancelLastReason != "not relevant" || f.cancelLastBy != "op@acme.example" {
		t.Errorf("expected reason+actor forwarded; got reason=%q by=%q", f.cancelLastReason, f.cancelLastBy)
	}
	if !strings.Contains(w.Body.String(), `id="plan-decisions"`) {
		t.Errorf("dismiss should re-render #plan-decisions; got: %s", w.Body.String())
	}
}

// TestHandlePlanResolveDecision_NonOperatorForbidden proves the resolve route
// is operator-gated: a viewer cannot answer a plan's question.
func TestHandlePlanResolveDecision_NonOperatorForbidden(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/decisions/gd-dec1/resolve",
		strings.NewReader("option_id=label"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("viewer must not resolve a decision; got 200")
	}
	if f.resolveCalls != 0 {
		t.Errorf("viewer resolve reached the daemon %d times, want 0", f.resolveCalls)
	}
}

// TestHandlePlanResolveDecision_AlreadyResolved proves a resolve against a
// closed decision is a no-op conflict (never double-resolves).
func TestHandlePlanResolveDecision_AlreadyResolved(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	f.beads["gd-dec1"].Status = "closed"
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/decisions/gd-dec1/resolve",
		strings.NewReader("option_id=label"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Re-render with the already-resolved banner in-band (200 so htmx swaps it);
	// the daemon is never asked to double-resolve.
	if w.Code != http.StatusOK {
		t.Fatalf("resolve of a closed decision should re-render (200 + banner); got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already resolved") {
		t.Errorf("expected the already-resolved banner; got: %s", w.Body.String())
	}
	if f.resolveCalls != 0 {
		t.Errorf("already-resolved decision should not reach the daemon; got %d calls", f.resolveCalls)
	}
}

// TestHandlePlanResolveDecision_RejectsCrossPlanDecision proves the plan-scoped
// resolve route refuses to act on an open decision that belongs to a DIFFERENT
// plan (its parent-child edge points elsewhere) — the scoping/attribution guard
// in loadPlanDecision. The daemon is never asked to resolve it.
func TestHandlePlanResolveDecision_RejectsCrossPlanDecision(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	// Re-point the decision's parent edge at some OTHER plan.
	f.deps["gd-dec1"] = []beadsapi.Dependency{
		{BeadID: "gd-dec1", DependsOnID: "gd-otherplan", Type: "parent-child"},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/decisions/gd-dec1/resolve",
		strings.NewReader("option_id=label"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "not part of this plan") {
		t.Errorf("expected the cross-plan rejection banner; got: %s", w.Body.String())
	}
	if f.resolveCalls != 0 {
		t.Errorf("a cross-plan decision must not reach the daemon; got %d calls", f.resolveCalls)
	}
}

// resolvePlanDecision drives the plan-scoped resolve route as an operator and
// returns the recorder, so the membership cases below differ only in the graph
// they seed.
func resolvePlanDecision(t *testing.T, f *fakeDaemon, planID, decisionID string) *httptest.ResponseRecorder {
	t.Helper()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/"+planID+"/decisions/"+decisionID+"/resolve",
		strings.NewReader("option_id=label"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestHandlePlanResolveDecision_AcceptsNestedDecision proves a decision nested
// DEEPER than a direct child of the plan epic — the epic → phase sub-epic →
// step → decision shape a real plan produces — can actually be resolved
// (ka-0000001446). The render path collects it via the TRANSITIVE Parent=epicID
// subtree, so the guard must reach it too; the old direct-parent-only check
// rejected it and left an Answer button that could only ever error.
func TestHandlePlanResolveDecision_AcceptsNestedDecision(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	// epic gd-plan1 → phase gd-phase1 → step gd-step1 → decision gd-dec1.
	f.deps["gd-dec1"] = []beadsapi.Dependency{
		{BeadID: "gd-dec1", DependsOnID: "gd-step1", Type: "parent-child"},
	}
	f.deps["gd-step1"] = []beadsapi.Dependency{
		{BeadID: "gd-step1", DependsOnID: "gd-phase1", Type: "parent-child"},
	}
	f.deps["gd-phase1"] = []beadsapi.Dependency{
		{BeadID: "gd-phase1", DependsOnID: "gd-plan1", Type: "parent-child"},
	}

	w := resolvePlanDecision(t, f, "gd-plan1", "gd-dec1")

	if strings.Contains(w.Body.String(), "not part of this plan") {
		t.Errorf("a nested decision must be resolvable; got the rejection banner: %s", w.Body.String())
	}
	if f.resolveCalls != 1 {
		t.Errorf("nested decision should reach the daemon exactly once; got %d calls", f.resolveCalls)
	}
}

// TestHandlePlanResolveDecision_AcceptsPinnedDecision proves a decision homed to
// the plan ONLY by fields.requesting_plan — no parent-child edge at all, the
// orphan class Layer 2 exists to re-home (ka-0000007801) — can be resolved
// (ka-0000001446). planDecisions RENDERS this decision under the plan, so the
// guard rejecting it made the rendered Answer chip permanently dead.
func TestHandlePlanResolveDecision_AcceptsPinnedDecision(t *testing.T) {
	f := seedPinnedDecisionFake()
	// Give the pinned decision resolvable options; it has no parent-child edge.
	f.beads["gd-decpin"].Fields["options"] = `[{"id":"label","short":"Use label","label":"Add a plan label"}]`

	w := resolvePlanDecision(t, f, "gd-plan1", "gd-decpin")

	if strings.Contains(w.Body.String(), "not part of this plan") {
		t.Errorf("a pin-homed decision must be resolvable; got the rejection banner: %s", w.Body.String())
	}
	if f.resolveCalls != 1 {
		t.Errorf("pinned decision should reach the daemon exactly once; got %d calls", f.resolveCalls)
	}
}

// TestHandlePlanResolveDecision_AcceptsSecondParentBranch proves the upward
// walk explores ALL parent edges, not just the first one it happens to see. A
// bead may carry more than one parent-child edge; descending a single chain
// would miss a plan reachable only through the second branch — a narrower
// instance of the same render/write divergence this fix is about.
func TestHandlePlanResolveDecision_AcceptsSecondParentBranch(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	// The decision has two parents. The FIRST leads away from the plan and dead-
	// ends; only the SECOND reaches gd-plan1, and only at depth 2.
	f.deps["gd-dec1"] = []beadsapi.Dependency{
		{BeadID: "gd-dec1", DependsOnID: "gd-deadend", Type: "parent-child"},
		{BeadID: "gd-dec1", DependsOnID: "gd-step2", Type: "parent-child"},
	}
	f.deps["gd-deadend"] = nil
	f.deps["gd-step2"] = []beadsapi.Dependency{
		{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"},
	}

	w := resolvePlanDecision(t, f, "gd-plan1", "gd-dec1")

	if strings.Contains(w.Body.String(), "not part of this plan") {
		t.Errorf("a plan reachable via the second parent branch must count; got: %s", w.Body.String())
	}
	if f.resolveCalls != 1 {
		t.Errorf("multi-parent decision should reach the daemon once; got %d calls", f.resolveCalls)
	}
}

// TestHandlePlanResolveDecision_RejectsNestedUnderOtherPlan proves widening the
// guard to a transitive walk did NOT widen it past the plan boundary: a
// decision nested under a DIFFERENT plan's subtree is still refused. This is the
// case that keeps the scoping guarantee honest — without it, "accept anything
// with a parent chain" would pass the two tests above.
func TestHandlePlanResolveDecision_RejectsNestedUnderOtherPlan(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	// gd-otherplan → gd-otherstep → gd-dec1. Nothing reaches gd-plan1.
	f.deps["gd-dec1"] = []beadsapi.Dependency{
		{BeadID: "gd-dec1", DependsOnID: "gd-otherstep", Type: "parent-child"},
	}
	f.deps["gd-otherstep"] = []beadsapi.Dependency{
		{BeadID: "gd-otherstep", DependsOnID: "gd-otherplan", Type: "parent-child"},
	}

	w := resolvePlanDecision(t, f, "gd-plan1", "gd-dec1")

	if !strings.Contains(w.Body.String(), "not part of this plan") {
		t.Errorf("expected the cross-plan rejection banner; got: %s", w.Body.String())
	}
	if f.resolveCalls != 0 {
		t.Errorf("a cross-plan decision must not reach the daemon; got %d calls", f.resolveCalls)
	}
}

// TestHandlePlanResolveDecision_ParentChainCycleTerminates proves a corrupted
// parent-child CYCLE that never reaches the plan terminates in the rejection
// banner instead of spinning the request. The daemon's subtree CTE carries the
// same concern (its UNION dedupes); the viewer-side walk relies on its seen-set
// plus the depth cap.
func TestHandlePlanResolveDecision_ParentChainCycleTerminates(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	// gd-dec1 → gd-a → gd-b → gd-a … a cycle with no path to gd-plan1.
	f.deps["gd-dec1"] = []beadsapi.Dependency{
		{BeadID: "gd-dec1", DependsOnID: "gd-a", Type: "parent-child"},
	}
	f.deps["gd-a"] = []beadsapi.Dependency{
		{BeadID: "gd-a", DependsOnID: "gd-b", Type: "parent-child"},
	}
	f.deps["gd-b"] = []beadsapi.Dependency{
		{BeadID: "gd-b", DependsOnID: "gd-a", Type: "parent-child"},
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- resolvePlanDecision(t, f, "gd-plan1", "gd-dec1") }()
	select {
	case w := <-done:
		if !strings.Contains(w.Body.String(), "not part of this plan") {
			t.Errorf("a cyclic chain that never reaches the plan must be refused; got: %s", w.Body.String())
		}
		if f.resolveCalls != 0 {
			t.Errorf("cyclic non-member must not reach the daemon; got %d calls", f.resolveCalls)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolve did not terminate on a cyclic parent chain")
	}
}

// TestHandlePlanResolveDecision_AcceptsLegacyParentEdge proves the walk honours
// the legacy `parent` dep-type alias the daemon's ParentChildDepTypes read set
// accepts (ka-0000008107). A decision joined by a legacy edge renders under the
// plan, so it must resolve under it too.
func TestHandlePlanResolveDecision_AcceptsLegacyParentEdge(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	f.deps["gd-dec1"] = []beadsapi.Dependency{
		{BeadID: "gd-dec1", DependsOnID: "gd-plan1", Type: "parent"},
	}

	w := resolvePlanDecision(t, f, "gd-plan1", "gd-dec1")

	if strings.Contains(w.Body.String(), "not part of this plan") {
		t.Errorf("a legacy `parent` edge must count as membership; got: %s", w.Body.String())
	}
	if f.resolveCalls != 1 {
		t.Errorf("legacy-edge decision should reach the daemon once; got %d calls", f.resolveCalls)
	}
}

// TestHandlePlanEvents_DecisionLifecycleEmitsDecisionsFrame proves a lifecycle
// event on the plan's clarifying-question decision re-renders the
// #plan-decisions section (plan-decisions-updated), and does NOT flash the step
// timeline (a resolve is not a step change).
func TestHandlePlanEvents_DecisionLifecycleEmitsDecisionsFrame(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// The gardener closes/answers the question → its decision bead gets a
	// lifecycle event.
	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.closed",
		Data:  []byte(`{"bead":{"id":"gd-dec1","type":"decision"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-decisions-updated") {
		t.Errorf("expected a plan-decisions-updated frame, got: %s", got)
	}
	if !strings.Contains(got, `id=\"plan-decisions\"`) && !strings.Contains(got, `id="plan-decisions"`) {
		t.Errorf("expected the re-rendered decisions section, got: %s", got)
	}
	if strings.Contains(got, "event: plan-updated") {
		t.Errorf("a decision resolve must not flash the step timeline, got: %s", got)
	}
}

// TestHandlePlanEvents_NewQuestionAppearsLive proves a brand-new clarifying
// question — auto-parented to the plan epic as a parent-child edge — surfaces
// via a plan-decisions-updated frame (the dependency.added rebuild branch), so
// the gardener's NEXT question appears inline without a reload.
func TestHandlePlanEvents_NewQuestionAppearsLive(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// A new decision is auto-parented to the plan epic (target == epicID).
	ch <- beadsapi.SSEEvent{
		Event: "beads.dependency.added",
		Data:  []byte(`{"dependency":{"bead_id":"gd-dec2","depends_on_id":"gd-plan1","type":"parent-child"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-decisions-updated") {
		t.Errorf("expected a plan-decisions-updated frame for the new question, got: %s", got)
	}
}
