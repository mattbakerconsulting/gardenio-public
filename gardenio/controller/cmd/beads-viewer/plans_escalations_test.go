package main

// plans_escalations_test.go — the blocked-by-escalation signal
// (epic ka-0000005480): WS2 aggregation, WS3 index badge, WS4 cockpit banner
// + SSE live-clear, WS5 per-step escalation chip.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedPlanEscalation wires an open escalation that `blocks` the given work
// bead (a plan step or the epic itself): the forward blocks-edge WS1's
// `gb escalation create --requesting-task` writes.
func seedPlanEscalation(f *fakeDaemon, escID, blockedID, title, opType, sensitivity string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     escID,
		Title:  title,
		Kind:   "data",
		Type:   "escalation",
		Status: "open",
		Fields: map[string]string{
			"operation_type":  opType,
			"sensitivity":     sensitivity,
			"requesting_task": blockedID,
		},
		CreatedAt: time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC),
	}
	f.beads[escID] = b
	if f.deps == nil {
		f.deps = map[string][]beadsapi.Dependency{}
	}
	f.deps[blockedID] = append(f.deps[blockedID], beadsapi.Dependency{
		BeadID: blockedID, DependsOnID: escID, Type: "blocks",
	})
	return b
}

// seedPinnedEscalation wires an OPEN escalation homed to a plan ONLY via the
// requesting_plan pin (Layer 2, ka-0000007801) — no blocks-edge to any rendered
// step. This is the orphan case: the escalation's origin step closed (or it was
// born from non-step work), so the blocks-edge path finds nothing, but the
// create-time pin still re-homes it to the plan.
func seedPinnedEscalation(f *fakeDaemon, escID, planID, title, opType, sensitivity string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     escID,
		Title:  title,
		Kind:   "data",
		Type:   "escalation",
		Status: "open",
		Fields: map[string]string{
			"operation_type":  opType,
			"sensitivity":     sensitivity,
			"requesting_plan": planID,
		},
		CreatedAt: time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC),
	}
	f.beads[escID] = b
	return b
}

// --- Layer 2: the requesting_plan escalation pin (ka-0000007801) --------------

// TestPlanEscalationsBulk_PinCollectsOrphan proves the load-bearing case: an
// open escalation with fields.requesting_plan == epicID and NO blocks-edge to a
// rendered step is still collected under that plan. This is the exact class the
// bug (ka-0000003246) describes — the blocks-origin closed, so the walk points
// at nothing, but the durable create-time pin re-homes it.
func TestPlanEscalationsBulk_PinCollectsOrphan(t *testing.T) {
	f := seedPlansFake()
	seedPinnedEscalation(f, "gd-escpin", "gd-plan1", "Rotate the prod secret", "credential_rotation", "secret")
	s := testServerWithDaemon(f)

	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"}, // no blocks-edge to either
	})
	info := got["gd-plan1"]
	if info.Count != 1 {
		t.Fatalf("Count = %d, want 1 (the pinned orphan must surface)", info.Count)
	}
	ref := info.Escalations[0]
	if ref.ID != "gd-escpin" || ref.Title != "Rotate the prod secret" {
		t.Errorf("ref = %+v, want gd-escpin / Rotate the prod secret", ref)
	}
	if ref.OperationType != "credential_rotation" || ref.Sensitivity != "secret" {
		t.Errorf("ref op/sens = %q/%q, want credential_rotation/secret", ref.OperationType, ref.Sensitivity)
	}
}

// TestPlanEscalationsBulk_PinDedupesWithBlocksEdge proves an escalation reached
// by BOTH paths (an open blocks-edge from a step AND the requesting_plan pin)
// renders EXACTLY ONCE — the regression the task calls out (no double-render).
func TestPlanEscalationsBulk_PinDedupesWithBlocksEdge(t *testing.T) {
	f := seedPlansFake()
	// Blocks-edge from the step...
	esc := seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "internal")
	// ...AND pinned to the same plan.
	esc.Fields["requesting_plan"] = "gd-plan1"
	s := testServerWithDaemon(f)

	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
	})
	if info := got["gd-plan1"]; info.Count != 1 {
		t.Fatalf("an escalation on both paths must count once, got Count = %d", info.Count)
	}
}

// TestPlanEscalationsBulk_PinIgnoresOtherPlan proves the page-wide pin query is
// bucketed strictly by membership: an escalation pinned to a plan NOT on this
// page is dropped, never mis-attributed.
func TestPlanEscalationsBulk_PinIgnoresOtherPlan(t *testing.T) {
	f := seedPlansFake()
	seedPinnedEscalation(f, "gd-escpin", "gd-otherplan", "Not this plan's fork", "general", "")
	s := testServerWithDaemon(f)

	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
	})
	if info := got["gd-plan1"]; info.Count != 0 {
		t.Errorf("escalation pinned to a different plan must not surface here, got Count = %d", info.Count)
	}
}

// TestPlanEscalationsBulk_PinIgnoresClosed proves a CLOSED escalation pinned to
// the plan is not collected — a resolved fork needs no re-homing.
func TestPlanEscalationsBulk_PinIgnoresClosed(t *testing.T) {
	f := seedPlansFake()
	esc := seedPinnedEscalation(f, "gd-escpin", "gd-plan1", "Already approved", "aws_iam", "")
	esc.Status = "closed"
	s := testServerWithDaemon(f)

	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
	})
	if info := got["gd-plan1"]; info.Count != 0 {
		t.Errorf("closed pinned escalation must not count, got Count = %d", info.Count)
	}
}

// TestPlansIndex_PinnedEscalationBadge proves the /plans index badge shouts a
// pinned-only escalation (no blocks-edge) — the surface operators actually
// watch, per the bug's remediation.
func TestPlansIndex_PinnedEscalationBadge(t *testing.T) {
	f := seedPlansFake()
	seedPinnedEscalation(f, "gd-escpin", "gd-plan1", "Rotate the prod secret", "credential_rotation", "secret")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "blocked by escalation") {
		t.Error("index card missing the ⛔ badge for the pinned escalation")
	}
	if !strings.Contains(body, "Rotate the prod secret") {
		t.Error("badge tooltip should enumerate the pinned escalation title")
	}
}

// TestHandlePlanDetail_PinnedEscalationBanner proves the cockpit banner renders a
// pinned-only escalation on the detail page (plans_detail.go path).
func TestHandlePlanDetail_PinnedEscalationBanner(t *testing.T) {
	f := seedPlansFake()
	seedPinnedEscalation(f, "gd-escpin", "gd-plan1", "Rotate the prod secret", "credential_rotation", "secret")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-escalations"`,
		"blocked by",
		"Rotate the prod secret",
		"credential_rotation",
		`href="/gd-escpin"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail banner missing %q (pinned escalation)", want)
		}
	}
}

// TestHandlePlanEvents_PinnedEscalationCloseEmitsBannerFrame proves the SSE
// interest set includes the pinned escalation (resolvePlanEscalationInterest
// PATH 2), so the operator approving it clears the banner LIVE — without this,
// the render would show the pinned fork but its close would only clear on
// reload, re-introducing the same stale-surface class the fix removes.
func TestHandlePlanEvents_PinnedEscalationCloseEmitsBannerFrame(t *testing.T) {
	f := seedPlansFake()
	seedPinnedEscalation(f, "gd-escpin", "gd-plan1", "Rotate the prod secret", "credential_rotation", "secret")
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// The operator approves: the pinned escalation bead closes.
	enqueuePlanEvent(t, ch, "beads.bead.closed", "gd-escpin")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-escalations-updated") {
		t.Errorf("expected a plan-escalations-updated frame for the pinned escalation close, got: %s", got)
	}
	if !strings.Contains(got, `id="plan-escalations"`) {
		t.Errorf("the frame should carry the re-rendered banner section, got: %s", got)
	}
}

// --- WS2: the aggregation seam ------------------------------------------------

func TestPlanEscalationsBulk_OpenEscalationBlocksStep(t *testing.T) {
	f := seedPlansFake()
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "internal")
	s := testServerWithDaemon(f)

	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
	})
	info := got["gd-plan1"]
	if info.Count != 1 {
		t.Fatalf("Count = %d, want 1", info.Count)
	}
	ref := info.Escalations[0]
	if ref.ID != "gd-esc1" || ref.Title != "Grant IAM for deploy" {
		t.Errorf("ref = %+v, want gd-esc1 / Grant IAM for deploy", ref)
	}
	if ref.OperationType != "aws_iam" || ref.Sensitivity != "internal" {
		t.Errorf("ref op/sens = %q/%q, want aws_iam/internal", ref.OperationType, ref.Sensitivity)
	}
}

func TestPlanEscalationsBulk_ClosedEscalationDoesNotCount(t *testing.T) {
	f := seedPlansFake()
	esc := seedPlanEscalation(f, "gd-esc1", "gd-step1", "Already approved", "k8s_rbac", "")
	esc.Status = "closed"
	s := testServerWithDaemon(f)

	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
	})
	if info := got["gd-plan1"]; info.Count != 0 {
		t.Errorf("closed escalation must not count, got Count = %d", info.Count)
	}
}

func TestPlanEscalationsBulk_OrdinaryBlockerDoesNotCount(t *testing.T) {
	f := seedPlansFake()
	// An ordinary open task blocking the step — a blocker, but not an escalation.
	f.beads["gd-blk"] = &beadsapi.BeadDetail{
		ID: "gd-blk", Title: "Ordinary blocker", Kind: "issue", Type: "task", Status: "open",
	}
	if f.deps == nil {
		f.deps = map[string][]beadsapi.Dependency{}
	}
	f.deps["gd-step1"] = []beadsapi.Dependency{
		{BeadID: "gd-step1", DependsOnID: "gd-blk", Type: "blocks"},
	}
	s := testServerWithDaemon(f)

	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
	})
	if info := got["gd-plan1"]; info.Count != 0 {
		t.Errorf("non-escalation blocker must not count, got Count = %d", info.Count)
	}
}

func TestPlanEscalationsBulk_DedupesAcrossSteps(t *testing.T) {
	f := seedPlansFake()
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "One escalation, two steps", "general", "")
	// The same escalation also blocks a second step.
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Second step", Kind: "issue", Type: "task", Status: "open",
	}
	f.deps["gd-step2"] = []beadsapi.Dependency{
		{BeadID: "gd-step2", DependsOnID: "gd-esc1", Type: "blocks"},
	}
	s := testServerWithDaemon(f)

	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1", "gd-step2"},
	})
	if info := got["gd-plan1"]; info.Count != 1 {
		t.Errorf("one escalation blocking two steps must count once, got %d", info.Count)
	}
}

func TestPlanEscalationsBulk_BoundedCalls(t *testing.T) {
	f := seedPlansFake()
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "esc for plan1", "aws_iam", "")
	// A second plan with its own step + escalation — the bulk pass must still
	// issue exactly ONE forward dep batch (and one bead hydration batch).
	f.beads["gd-plan2"] = &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "open", Labels: []string{"project:gardenio", "plan"},
	}
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Second plan step", Kind: "issue", Type: "task", Status: "open",
	}
	seedPlanEscalation(f, "gd-esc2", "gd-step2", "esc for plan2", "k8s_rbac", "")
	s := testServerWithDaemon(f)

	before := f.batchGetDepsCalls
	got := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
		"gd-plan2": {"gd-plan2", "gd-step2"},
	})
	if calls := f.batchGetDepsCalls - before; calls != 1 {
		t.Errorf("expected exactly 1 batched dep call across all plans, got %d", calls)
	}
	if got["gd-plan1"].Count != 1 || got["gd-plan2"].Count != 1 {
		t.Errorf("per-plan counts = %d/%d, want 1/1", got["gd-plan1"].Count, got["gd-plan2"].Count)
	}
}

// --- WS3: the /plans index badge ---------------------------------------------

func TestPlansIndex_EscalationBadgeWhenBlocked(t *testing.T) {
	f := seedPlansFake()
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "internal")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "blocked by escalation") {
		t.Error("index card missing the ⛔ blocked-by-escalation badge")
	}
	if !strings.Contains(body, "Grant IAM for deploy") {
		t.Error("badge tooltip should enumerate the escalation title")
	}
	if !strings.Contains(body, "/plans/gd-plan1#plan-escalations") {
		t.Error("badge should deep-link to the cockpit escalation banner")
	}
}

func TestPlansIndex_NoEscalationBadgeWhenClear(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "blocked by escalation") {
		t.Error("unblocked plan must not show the escalation badge")
	}
}

// --- WS4: the cockpit banner ---------------------------------------------------

func TestHandlePlanDetail_EscalationBanner(t *testing.T) {
	f := seedPlansFake()
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "secret")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-escalations"`,   // the SSE swap anchor
		"blocked by",              // the banner headline
		"an open escalation",      // singular phrasing
		"Grant IAM for deploy",    // WHAT it's blocked by
		"aws_iam",                 // operation type chip
		"secret",                  // sensitivity tag
		`href="/gd-esc1"`,         // link to the escalation bead
		"plan-escalations-banner", // banner styling hook
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
}

func TestHandlePlanDetail_NoBannerWhenClear(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the rendered element (class attr), not the bare class name —
	// the <style> block always carries `.plan-escalations-banner`.
	if strings.Contains(body, `plan-escalations-banner"`) {
		t.Error("unblocked plan must not render the escalation banner")
	}
	// The empty swap anchor still renders so SSE can clear INTO it later.
	if !strings.Contains(body, `id="plan-escalations"`) {
		t.Error("the #plan-escalations SSE anchor must render even when clear")
	}
}

func TestHandlePlanDetail_ReviewLinkWhenConfigured(t *testing.T) {
	f := seedPlansFake()
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Rotate creds", "credential_rotation", "")
	srv := newTestServerForEsc(t, f)
	srv.escalationReviewURL = "https://gardenio.example.test/escalations"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "bead_id=gd-esc1") {
		t.Error("banner should carry the bridge-SPA approve/reject deep link")
	}
}

// --- WS4 SSE: the banner clears live -------------------------------------------

func TestHandlePlanEvents_EscalationCloseEmitsBannerFrame(t *testing.T) {
	f := seedPlansFake()
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "")
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// The operator approves: the escalation bead closes.
	enqueuePlanEvent(t, ch, "beads.bead.closed", "gd-esc1")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-escalations-updated") {
		t.Errorf("expected a plan-escalations-updated frame, got: %s", got)
	}
	if !strings.Contains(got, "event: plan-updated") {
		t.Errorf("escalation close should also refresh the step timeline, got: %s", got)
	}
	if !strings.Contains(got, `id="plan-escalations"`) {
		t.Errorf("the frame should carry the re-rendered banner section, got: %s", got)
	}
}

// TestHandlePlanEvents_NewBlocksEdgeShowsBanner proves the inverse direction:
// an agent escalates mid-session (`gb escalation create --requesting-task`
// writes the step→escalation blocks edge) and the banner + timeline re-render
// immediately — the new escalation shouts without a reload.
func TestHandlePlanEvents_NewBlocksEdgeShowsBanner(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// Mid-session: the escalation + its blocks edge land...
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Fresh escalation", "aws_iam", "")
	// ...and the dependency event announces the edge from the step.
	ch <- beadsapi.SSEEvent{
		Event: "beads.dependency.added",
		Data:  []byte(`{"dependency":{"bead_id":"gd-step1","depends_on_id":"gd-esc1","type":"blocks"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-escalations-updated") {
		t.Errorf("expected a plan-escalations-updated frame for the new edge, got: %s", got)
	}
	if !strings.Contains(got, "Fresh escalation") {
		t.Errorf("the frame should carry the new escalation's title, got: %s", got)
	}
}

func TestHandlePlanEvents_UnrelatedEscalationIgnored(t *testing.T) {
	f := seedPlansFake()
	// An open escalation that blocks nothing in this plan.
	f.beads["gd-esc-other"] = &beadsapi.BeadDetail{
		ID: "gd-esc-other", Title: "Someone else's escalation", Kind: "data",
		Type: "escalation", Status: "open",
	}
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	enqueuePlanEvent(t, ch, "beads.bead.closed", "gd-esc-other")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if strings.Contains(got, "event: plan-escalations-updated") {
		t.Errorf("unrelated escalation must not emit a banner frame, got: %s", got)
	}
}

// --- WS5: the per-step escalation chip -------------------------------------------

func TestHandlePlanDetail_StepEscalationChipDistinct(t *testing.T) {
	f := seedPlansFake()
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-tl-blocked-esc") {
		t.Error("step blocked by an escalation should render the distinct escalation chip class")
	}
	if !strings.Contains(body, "⛔ escalation: aws_iam") {
		t.Error("escalation chip should carry the operation type")
	}
}

func TestHandlePlanDetail_OrdinaryBlockerChipUnchanged(t *testing.T) {
	// The full-row fixture has step2 blocked by an ordinary open bead — its
	// chip must keep the plain ⛔ {id} shape with no escalation styling.
	mux := mkServer(t, seedPlansFullRowFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⛔ gd-blk-open") {
		t.Error("ordinary blocker should keep the plain ⛔ {id} chip")
	}
	// Rendered class attr, not the bare name — the <style> block always
	// carries `.plan-tl-blocked-esc`.
	if strings.Contains(body, `plan-tl-blocked-esc"`) {
		t.Error("ordinary blocker must not get the escalation chip class")
	}
}
