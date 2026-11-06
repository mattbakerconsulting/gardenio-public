package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- Step 1 (ka-0000008064): broaden 'mine' to involvement --------------------
//
// These tests pin the reusable "plans this operator is INVOLVED in" query:
// creator ∪ step-owner ∪ decision-resolver ∪ gardener-owner. Facet A (creator)
// rides the existing hotEpicsResp epic route; facets B/C/D are seeded through
// dedicated fake routes (involveStepScanResp / involveDecisionScanResp /
// triggerScheduleResp) and mapped to their parent plan via the fake's forward
// deps.

// involvementBaseQuery mirrors the base query handlePlans assembles for the
// plan-epics universe (type=epic, plan label, active statuses), so a unit test
// of plansInvolvingOperator exercises the same constraint set the handler uses.
func involvementBaseQuery() beadsapi.ListBeadsQuery {
	return beadsapi.ListBeadsQuery{
		Types:            []string{"epic"},
		Labels:           []string{planLabel},
		Statuses:         activeEnumStatuses,
		Sort:             "-created_at",
		ExcludeScheduled: true,
		Limit:            plansListLimit,
	}
}

// seedInvolvementExtraPlan seeds a plan epic that the viewer did NOT create (so
// facet A misses it) but is reachable via a B/C/D facet. It is added to f.beads
// (so BatchGetBeads materializes it) but NOT to hotEpicsResp (the creator route).
func seedInvolvementExtraPlan(f *fakeDaemon, id, project string, created time.Time) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:        id,
		Title:     "Extra plan " + id,
		Kind:      "issue",
		Type:      "epic",
		Status:    "open",
		Labels:    []string{"project:" + project, "plan"},
		CreatedAt: created,
		UpdatedAt: created,
	}
	f.beads[id] = b
	return b
}

// TestPlansInvolving_CreatorFacetOnly proves that with no B/C/D hits the result
// is exactly the creator set (the historical behaviour) — the involvement query
// is a strict superset that adds nothing when the operator only created plans.
func TestPlansInvolving_CreatorFacetOnly(t *testing.T) {
	f := seedPlansFake() // creator route (hotEpicsResp) serves gd-plan1
	s := planAssistantTestServer(f)

	res, err := s.plansInvolvingOperator(context.Background(), involvementBaseQuery(), "oidc:matt@acme.example")
	if err != nil {
		t.Fatalf("plansInvolvingOperator: %v", err)
	}
	if len(res.Beads) != 1 || res.Beads[0].ID != "gd-plan1" {
		t.Fatalf("creator-only involvement should be [gd-plan1], got %+v", beadIDs(res.Beads))
	}
	// The creator facet MUST be issued as a scoped epic query so the header-chip
	// default-scoping assertion (TestPlansIndex_DefaultScopesToViewer) still holds.
	if q := planEpicQuery(f); q == nil || !slices.Contains(q.OperatedByAliases, "oidc:matt@acme.example") {
		t.Errorf("creator facet must carry the operator alias on the epic query; got %+v", q)
	}
}

// TestPlansInvolving_GardenerFacetAddsPlan proves facet D: a plan the viewer did
// not create but whose gardener they own is folded in.
func TestPlansInvolving_GardenerFacetAddsPlan(t *testing.T) {
	f := seedPlansFake()
	extra := seedInvolvementExtraPlan(f, "gd-capplan", "gardenio", time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC))
	// A gardener the viewer owns, pinned to the extra plan. seedPlanGardener routes
	// it through the role:gardener list (triggerScheduleResp), which the gardener
	// facet reads. The fake's gardener list ignores operated_by, so ownership is
	// implied by seeding it into the viewer's facet response.
	seedPlanGardener(f, "gd-cap-extra", extra.ID, "true")
	s := planAssistantTestServer(f)

	res, err := s.plansInvolvingOperator(context.Background(), involvementBaseQuery(), "oidc:matt@acme.example")
	if err != nil {
		t.Fatalf("plansInvolvingOperator: %v", err)
	}
	got := beadIDs(res.Beads)
	if !slices.Contains(got, "gd-plan1") || !slices.Contains(got, "gd-capplan") {
		t.Fatalf("gardener facet should fold in gd-capplan alongside creator gd-plan1; got %v", got)
	}
}

// TestPlansInvolving_StepOwnerFacetAddsPlan proves facet B: a plan the viewer did
// not create but where they own a rendered step is folded in. The step maps to
// its parent plan via the forward-dep (child→parent) walk.
func TestPlansInvolving_StepOwnerFacetAddsPlan(t *testing.T) {
	f := seedPlansFake()
	extra := seedInvolvementExtraPlan(f, "gd-stepplan", "gardenio", time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC))
	// A step the viewer owns, whose parent-child parent is the extra plan.
	step := &beadsapi.BeadDetail{
		ID: "gd-ownedstep", Title: "Owned step", Kind: "issue", Type: "task", Status: "open",
	}
	f.beads[step.ID] = step
	f.deps[step.ID] = []beadsapi.Dependency{{BeadID: step.ID, DependsOnID: extra.ID, Type: "parent-child"}}
	f.involveStepScanResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{step}, Total: 1}
	s := planAssistantTestServer(f)

	res, err := s.plansInvolvingOperator(context.Background(), involvementBaseQuery(), "oidc:matt@acme.example")
	if err != nil {
		t.Fatalf("plansInvolvingOperator: %v", err)
	}
	got := beadIDs(res.Beads)
	if !slices.Contains(got, "gd-plan1") || !slices.Contains(got, "gd-stepplan") {
		t.Fatalf("step-owner facet should fold in gd-stepplan; got %v", got)
	}
}

// TestPlansInvolving_DecisionResolverFacetAddsPlan proves facet C: a plan the
// viewer did not create but whose clarifying-question decision they resolved is
// folded in. The resolver identity is the decision's canonical responded_by.
func TestPlansInvolving_DecisionResolverFacetAddsPlan(t *testing.T) {
	f := seedPlansFake()
	extra := seedInvolvementExtraPlan(f, "gd-decplan", "gardenio", time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC))
	dec := &beadsapi.BeadDetail{
		ID: "gd-resolveddec", Title: "Resolved Q", Type: "decision", Status: "closed",
		Fields: map[string]string{"responded_by": "oidc:matt@acme.example"},
	}
	f.beads[dec.ID] = dec
	f.deps[dec.ID] = []beadsapi.Dependency{{BeadID: dec.ID, DependsOnID: extra.ID, Type: "parent-child"}}
	f.involveDecisionScanResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{dec}, Total: 1}
	s := planAssistantTestServer(f)

	res, err := s.plansInvolvingOperator(context.Background(), involvementBaseQuery(), "oidc:matt@acme.example")
	if err != nil {
		t.Fatalf("plansInvolvingOperator: %v", err)
	}
	got := beadIDs(res.Beads)
	if !slices.Contains(got, "gd-plan1") || !slices.Contains(got, "gd-decplan") {
		t.Fatalf("decision-resolver facet should fold in gd-decplan; got %v", got)
	}
}

// TestPlansInvolving_ConstraintsDropNonMatchingExtras proves a facet candidate
// that is NOT a plan, is in the wrong project, or is in a filtered-out status is
// dropped when the extra epics are materialized and constraint-checked — so the
// involvement set never leaks a bead the pure creator query could not return.
func TestPlansInvolving_ConstraintsDropNonMatchingExtras(t *testing.T) {
	f := seedPlansFake()
	// (1) A closed plan — dropped by the active-status base query.
	closedPlan := seedInvolvementExtraPlan(f, "gd-qtkwkqh8tn", "gardenio", time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	closedPlan.Status = "closed"
	// (2) A wrong-project plan — dropped when the base query carries project:gardenio.
	seedInvolvementExtraPlan(f, "gd-coopplan", "coop", time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	// (3) A non-plan epic (no plan label) — dropped by the plan-label constraint.
	nonPlan := seedInvolvementExtraPlan(f, "gd-nonplan", "gardenio", time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	nonPlan.Labels = []string{"project:gardenio"} // strip the plan label
	// Point all three at gardeners the viewer owns so facet D surfaces them.
	seedPlanGardener(f, "gd-cap-closed", "gd-qtkwkqh8tn", "true")
	seedPlanGardener(f, "gd-cap-coop", "gd-coopplan", "true")
	seedPlanGardener(f, "gd-cap-nonplan", "gd-nonplan", "true")
	s := planAssistantTestServer(f)

	// Base query scoped to project:gardenio + active statuses (as handlePlans builds
	// it for /plans?project=gardenio).
	base := involvementBaseQuery()
	base.Labels = append(base.Labels, "project:gardenio")
	res, err := s.plansInvolvingOperator(context.Background(), base, "oidc:matt@acme.example")
	if err != nil {
		t.Fatalf("plansInvolvingOperator: %v", err)
	}
	got := beadIDs(res.Beads)
	for _, drop := range []string{"gd-qtkwkqh8tn", "gd-coopplan", "gd-nonplan"} {
		if slices.Contains(got, drop) {
			t.Errorf("constraint check should have dropped %s; got %v", drop, got)
		}
	}
}

// TestPlansInvolving_CreatorErrorPropagates proves the creator facet is
// authoritative: its daemon error fails the whole query (mirrors the historical
// single-query error path), while a best-effort facet error would not.
func TestPlansInvolving_CreatorErrorPropagates(t *testing.T) {
	f := seedPlansFake()
	f.listErr = errFakeList
	s := planAssistantTestServer(f)

	if _, err := s.plansInvolvingOperator(context.Background(), involvementBaseQuery(), "oidc:matt@acme.example"); err == nil {
		t.Fatal("a creator-facet daemon error must propagate, got nil")
	}
}

// TestPlansIndex_DefaultIncludesInvolvedPlan is the end-to-end wiring proof: the
// cold-load /plans default renders a plan the viewer did not create but is
// involved in (here via the gardener facet), so "Showing your work" now means
// plans they are part of, not just plans they authored.
func TestPlansIndex_DefaultIncludesInvolvedPlan(t *testing.T) {
	f := seedPlansFake()
	extra := seedInvolvementExtraPlan(f, "gd-capplan", "gardenio", time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC))
	extra.Title = "Plan I gardener but did not create"
	f.epicSubtreeCount[extra.ID] = 0
	seedPlanGardener(f, "gd-cap-extra", extra.ID, "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Plan: ship the /plans page") {
		t.Errorf("creator plan should still render on the default view")
	}
	if !strings.Contains(body, "/plans/gd-capplan") {
		t.Errorf("the involved (gardenered-but-not-created) plan should render on the default view; body=%s", snippetN(body, 1500))
	}
}

// TestPlansIndex_ShowAllSkipsInvolvementQuery proves the unscoped views
// (explicit show-all / anonymous / unowned) keep the plain full-population query
// and do NOT issue the involvement facet scans — involvement is a
// scoped-to-an-operator concern only.
func TestPlansIndex_ShowAllSkipsInvolvementQuery(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// No facet scans: the step-owner scan (Kinds=[issue] + OperatedByAliases) and
	// the decision-resolver scan (responded_by field) must be absent.
	for _, q := range f.listAllQ {
		if len(q.Types) == 0 && slices.Contains(q.Kinds, "issue") && len(q.OperatedByAliases) > 0 {
			t.Errorf("show-all must not issue the step-owner involvement scan; got %+v", q)
		}
		if q.Fields["responded_by"] != "" {
			t.Errorf("show-all must not issue the decision-resolver involvement scan; got %+v", q)
		}
	}
}

// beadIDs projects a bead slice to its ids for terse assertions.
func beadIDs(beads []*beadsapi.BeadDetail) []string {
	out := make([]string, 0, len(beads))
	for _, b := range beads {
		if b != nil {
			out = append(out, b.ID)
		}
	}
	return out
}
