package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// openDec builds an OPEN decision bead for the enumeration fixture.
func openDec(id, title string, created time.Time, fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     title,
		Type:      "decision",
		Status:    "open",
		Labels:    []string{"project:gardenio"},
		Fields:    fields,
		CreatedAt: created,
	}
}

// TestBuildUnhomedForks_SubtractCorrectness is the load-bearing unit test the
// bead asks for (ka-0000004983 VERIFY). It drives buildUnhomedForks over an
// enumerated set that mixes every surfacing class and proves the subtract keeps
// EXACTLY the forks that belong to no plan and no operator surface:
//
//   - a parent-child subtree decision (id ∈ a rendered row's openDecisionIDs) is
//     subtracted;
//   - a blocks-edge / requesting_plan-pin escalation (id ∈ a row's Escalations) is
//     subtracted;
//   - a decision pinned to a KNOWN plan (rendered OR universe) via
//     fields.requesting_plan is subtracted (Layer-2 durable homing);
//   - a genuinely orphaned open decision AND open escalation (no membership at all)
//     survive — the exact reproduction class (ephemeral origin, no plan ancestor,
//     no requesting-task) that made ka-0000004749 invisible.
func TestBuildUnhomedForks_SubtractCorrectness(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{},
		decisionResp: &beadsapi.ListBeadsResult{
			Beads: []*beadsapi.BeadDetail{
				openDec("gd-dec-subtree", "Homed via parent-child", base.Add(1*time.Hour), nil),
				openDec("gd-dec-pin-rendered", "Homed via pin to a rendered plan", base.Add(2*time.Hour), map[string]string{"requesting_plan": "gd-plan1"}),
				openDec("gd-dec-pin-universe", "Homed via pin to an off-view plan", base.Add(3*time.Hour), map[string]string{"requesting_plan": "gd-plan-universe"}),
				// The reproduction: no parent-child, no requesting_plan — orphaned.
				openDec("gd-dec-orphan", "Orphaned clarifying question", base, map[string]string{"requested_by": "gardenio-cycle-agent"}),
			},
			Total: 4,
		},
		escalationsResp: []beadsapi.EscalationDetail{
			{Issue: makeEscBead("gd-esc-surfaced", "Blocks a rendered step", "aws_iam", "internal", "open", base.Add(1*time.Hour), nil)},
			{Issue: makeEscBead("gd-esc-pin-universe", "Pinned to an off-view plan", "deploy", "secret", "open", base.Add(2*time.Hour), map[string]string{"requesting_plan": "gd-plan-universe"})},
			{Issue: makeEscBead("gd-esc-orphan", "Orphaned escalation", "credential_provisioning", "secret", "open", base, nil)},
		},
	}
	s := newTestServerForEsc(t, f)

	// One rendered plan carries the surfaced sets the enrich pass produces:
	// a subtree-homed decision id + a blocks-edge-homed escalation id.
	rows := []planRowView{{
		ID:              "gd-plan1",
		openDecisionIDs: []string{"gd-dec-subtree"},
		Escalations:     planEscalationInfo{Count: 1, Escalations: []planEscalationRef{{ID: "gd-esc-surfaced"}}},
	}}
	// Known plans = rendered (gd-plan1) ∪ unscoped universe (gd-plan-universe).
	planIDs := map[string]struct{}{"gd-plan1": {}, "gd-plan-universe": {}}

	view := s.buildUnhomedForks(context.Background(), rows, planIDs, "")

	// Only the two genuine orphans survive.
	if view.Count != 2 {
		t.Fatalf("Count = %d, want 2 (one orphan decision + one orphan escalation); got %+v", view.Count, view)
	}
	if len(view.Decisions) != 1 || view.Decisions[0].ID != "gd-dec-orphan" {
		t.Errorf("Decisions = %+v, want exactly [gd-dec-orphan]", view.Decisions)
	}
	if len(view.Escalations) != 1 || view.Escalations[0].ID != "gd-esc-orphan" {
		t.Errorf("Escalations = %+v, want exactly [gd-esc-orphan]", view.Escalations)
	}
	// The orphan decision carries its requested_by detail + a resolve deep-link.
	if d := view.Decisions[0]; d.Detail != "gardenio-cycle-agent" || d.URL != "/gd-dec-orphan" {
		t.Errorf("orphan decision detail/url = %q/%q, want requested_by + /{id}", d.Detail, d.URL)
	}
}

// TestBuildUnhomedForks_EmptyWhenAllHomed proves a fully-homed scope yields an
// empty view (Count 0) — the gate-on-non-empty contract that keeps healthy
// cockpits noise-free (the handler sets the data key only when Count > 0).
func TestBuildUnhomedForks_EmptyWhenAllHomed(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{},
		decisionResp: &beadsapi.ListBeadsResult{
			Beads: []*beadsapi.BeadDetail{openDec("gd-dec1", "Homed", base, nil)},
			Total: 1,
		},
		escalationsResp: []beadsapi.EscalationDetail{
			{Issue: makeEscBead("gd-esc1", "Homed", "aws_iam", "internal", "open", base, nil)},
		},
	}
	s := newTestServerForEsc(t, f)
	rows := []planRowView{{
		ID:              "gd-plan1",
		openDecisionIDs: []string{"gd-dec1"},
		Escalations:     planEscalationInfo{Count: 1, Escalations: []planEscalationRef{{ID: "gd-esc1"}}},
	}}
	view := s.buildUnhomedForks(context.Background(), rows, map[string]struct{}{"gd-plan1": {}}, "")
	if view.Count != 0 || len(view.Decisions) != 0 || len(view.Escalations) != 0 {
		t.Errorf("fully-homed scope should yield an empty view, got %+v", view)
	}
}

// TestBuildUnhomedForks_ProjectFilterOnEscalations proves the ?project scope is
// honoured client-side for escalations (mirroring handleEscalations): an
// out-of-project orphan is dropped even though ListEscalations returned it.
func TestBuildUnhomedForks_ProjectFilterOnEscalations(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	otherProj := makeEscBead("gd-esc-other", "Other project orphan", "deploy", "secret", "open", base, nil)
	otherProj.Labels = []string{"project:kbeads"}
	f := &fakeDaemon{
		beads:        map[string]*beadsapi.BeadDetail{},
		decisionResp: &beadsapi.ListBeadsResult{},
		escalationsResp: []beadsapi.EscalationDetail{
			{Issue: makeEscBead("gd-esc-gardenio", "Gardenio orphan", "aws_iam", "internal", "open", base, nil)},
			{Issue: otherProj},
		},
	}
	s := newTestServerForEsc(t, f)
	view := s.buildUnhomedForks(context.Background(), nil, map[string]struct{}{}, "gardenio")
	if len(view.Escalations) != 1 || view.Escalations[0].ID != "gd-esc-gardenio" {
		t.Errorf("project=gardenio should keep only the gardenio orphan, got %+v", view.Escalations)
	}
}

// TestHandlePlans_UnhomedSectionSurfacesOrphan is the handler-level VERIFY: an
// open decision with NO parent-child to a plan epic and NO requesting_plan
// appears in the unhomed section on /plans.
func TestHandlePlans_UnhomedSectionSurfacesOrphan(t *testing.T) {
	f := seedPlansFake()
	// The seed's gd-dec1 IS a parent-child child of gd-plan1 (subtree-homed). Add a
	// second OPEN decision that is NOT parented to any plan and carries no
	// requesting_plan — the orphan. The enumeration query returns BOTH (decisionResp);
	// the subtract must keep only the orphan.
	orphan := openDec("gd-dec-orphan", "Orphaned P0 clarifying question", time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC), nil)
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-dec1"], orphan},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans?operated_by=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The section renders with the orphan...
	if !strings.Contains(body, `id="plans-unhomed"`) {
		t.Fatalf("expected the unhomed section to render for an orphaned decision; body:\n%s", body)
	}
	if !strings.Contains(body, "Orphaned P0 clarifying question") {
		t.Errorf("orphan decision title missing from the unhomed section")
	}
	if !strings.Contains(body, `href="/gd-dec-orphan"`) {
		t.Errorf("orphan decision should link to its resolve surface /{id}")
	}
	// ...and the normally-wired gd-dec1 still renders its "1 to answer" card chip
	// as today (Regression #1: unchanged surfacing).
	if !strings.Contains(body, "1 to answer") {
		t.Errorf("the parent-child-homed decision must still render its 'N to answer' card chip as today")
	}
}

// TestHandlePlans_NormallyWiredDecisionNotDuplicated is the regression the bead
// requires: a normally-wired decision (a parent-child descendant of a rendered
// plan) renders exactly as today and is NOT duplicated into the unhomed section.
// With only the subtree-homed decision in scope, the unhomed set is empty, so the
// section is hidden entirely.
func TestHandlePlans_NormallyWiredDecisionNotDuplicated(t *testing.T) {
	f := seedPlansFake() // gd-dec1 is a parent-child child of gd-plan1, and the sole open decision
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans?operated_by=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The card chip renders as today...
	if !strings.Contains(body, "1 to answer") {
		t.Errorf("the homed decision must still render its card chip")
	}
	// ...and the unhomed section is hidden (nothing orphaned → gate-on-non-empty).
	if strings.Contains(body, `id="plans-unhomed"`) {
		t.Errorf("a fully-homed cockpit must NOT render the unhomed section (no new noise); body:\n%s", body)
	}
	// No unhomed fork row renders (data-kind is a rendered-only marker; the inline
	// <style> block carries the class selectors but never a data-kind attribute).
	if strings.Contains(body, `data-kind="decision"`) || strings.Contains(body, `data-kind="escalation"`) {
		t.Errorf("no unhomed rows should render when every decision is homed")
	}
}
