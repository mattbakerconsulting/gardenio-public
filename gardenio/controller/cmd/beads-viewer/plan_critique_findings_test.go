package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// helper: find a finding by category+title substring.
func findFinding(findings []critiqueFinding, category, titleContains string) *critiqueFinding {
	for i := range findings {
		f := &findings[i]
		if f.Category == category && strings.Contains(f.Title, titleContains) {
			return f
		}
	}
	return nil
}

// TestProofGap_MergedMROpenStep — a step whose MR merged but the step is still
// open is the classic "claimed done, not attested" gap.
func TestProofGap_MergedMROpenStep(t *testing.T) {
	steps := []planStepView{
		{ID: "gd-a", Number: 1, Title: "Ship the handler", MRURL: "https://x/mr/1", MRState: "merged", Closed: false},
		{ID: "gd-b", Number: 2, Title: "Ship the UI", MRURL: "https://x/mr/2", MRState: "merged", Closed: true},
	}
	findings := computePlanCritiqueFindings(nil, steps, nil, nil, nil, planEscalationInfo{}, planStateBuilding)

	f := findFinding(findings, "proof-gap", "merged but step still open")
	if f == nil {
		t.Fatalf("expected proof-gap finding for open step with merged MR; got %d findings", len(findings))
	}
	if f.Severity != critiqueWarn {
		t.Errorf("proof-gap severity = %v, want warn", f.Severity)
	}
	if f.AnchorHref != "/gd-a" {
		t.Errorf("proof-gap anchor href = %q, want /gd-a", f.AnchorHref)
	}
	// The closed step (gd-b) must NOT produce a finding.
	if findFinding(findings, "proof-gap", "gd-b") != nil {
		t.Errorf("closed step should not raise a proof-gap finding")
	}
}

// TestCoverageGap_BuildingNoSteps — building plan with zero steps is a blocker.
func TestCoverageGap_BuildingNoSteps(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-plan", Description: "Do the thing."}
	findings := computePlanCritiqueFindings(bead, nil, nil, nil, nil, planEscalationInfo{}, planStateBuilding)

	f := findFinding(findings, "coverage-gap", "no steps")
	if f == nil {
		t.Fatalf("expected coverage-gap for building plan with no steps")
	}
	if f.Severity != critiqueBlock {
		t.Errorf("severity = %v, want block", f.Severity)
	}
}

// TestCoverageGap_EmptyPhase — a phase with no steps is flagged.
func TestCoverageGap_EmptyPhase(t *testing.T) {
	phases := []planPhaseView{
		{ID: "gd-ph1", Number: 1, Title: "Groundwork", Steps: nil},
		{ID: "gd-ph2", Number: 2, Title: "Build", Steps: []planStepView{{ID: "gd-s", Title: "s"}}},
	}
	findings := computePlanCritiqueFindings(nil, nil, phases, nil, nil, planEscalationInfo{}, planStateBuilding)

	f := findFinding(findings, "coverage-gap", "Phase has no steps")
	if f == nil {
		t.Fatalf("expected coverage-gap for empty phase")
	}
	if f.AnchorHref != "/gd-ph1" {
		t.Errorf("anchor href = %q, want /gd-ph1", f.AnchorHref)
	}
}

// TestCoverageGap_UnlinkedSteps — when most open steps carry no section ref and
// the plan HAS a narrative bundle, raise the traceability gap.
func TestCoverageGap_UnlinkedSteps(t *testing.T) {
	prose := &bundleView{Docs: []bundleDocCard{{BeadID: "gd-doc", Format: "md"}}}
	steps := []planStepView{
		{ID: "gd-a", Title: "a"}, // no SectionRef
		{ID: "gd-b", Title: "b"}, // no SectionRef
		{ID: "gd-c", Title: "c", SectionRef: planSectionRefView{Label: "C1"}},
	}
	findings := computePlanCritiqueFindings(nil, steps, nil, nil, prose, planEscalationInfo{}, planStateBuilding)

	if findFinding(findings, "coverage-gap", "don't reference the narrative") == nil {
		t.Fatalf("expected traceability coverage-gap when most steps unlinked")
	}

	// With no bundle, the traceability rule must NOT fire (nothing to link to).
	findings2 := computePlanCritiqueFindings(nil, steps, nil, nil, nil, planEscalationInfo{}, planStateBuilding)
	if findFinding(findings2, "coverage-gap", "don't reference the narrative") != nil {
		t.Errorf("traceability rule must not fire without a narrative bundle")
	}
}

// TestDesignReview_BuildingWithOpenQuestions — building past open questions is a
// blocker; open questions on a draft plan are just info.
func TestDesignReview_OpenQuestions(t *testing.T) {
	decisions := []planDecisionView{
		{ID: "gd-q1", Resolved: false},
		{ID: "gd-q2", Resolved: true},
	}
	building := computePlanCritiqueFindings(nil, []planStepView{{ID: "s"}}, nil, decisions, nil, planEscalationInfo{}, planStateBuilding)
	f := findFinding(building, "design-review", "open clarifying questions")
	if f == nil || f.Severity != critiqueBlock {
		t.Fatalf("expected BLOCK design-review for building-with-open-questions; got %+v", f)
	}

	draft := computePlanCritiqueFindings(nil, []planStepView{{ID: "s"}}, nil, decisions, nil, planEscalationInfo{}, planStateDraft)
	f2 := findFinding(draft, "design-review", "Open clarifying questions")
	if f2 == nil || f2.Severity != critiqueInfo {
		t.Fatalf("expected INFO design-review for draft-with-open-questions; got %+v", f2)
	}
}

// TestDesignReview_NoNarrative — steps but no narrative at all is a warn.
func TestDesignReview_NoNarrative(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-plan", Description: "   "} // blank
	steps := []planStepView{{ID: "gd-a", Title: "a"}}
	findings := computePlanCritiqueFindings(bead, steps, nil, nil, nil, planEscalationInfo{}, planStateBuilding)
	if findFinding(findings, "design-review", "no narrative") == nil {
		t.Fatalf("expected design-review for steps-without-narrative")
	}

	// With a description present, the rule must not fire.
	bead2 := &beadsapi.BeadDetail{ID: "gd-plan", Description: "A real plan summary."}
	findings2 := computePlanCritiqueFindings(bead2, steps, nil, nil, nil, planEscalationInfo{}, planStateBuilding)
	if findFinding(findings2, "design-review", "no narrative") != nil {
		t.Errorf("no-narrative rule must not fire when a description exists")
	}
}

// TestDesignReview_BlockedByEscalation — an open escalation is a blocker.
func TestDesignReview_BlockedByEscalation(t *testing.T) {
	esc := planEscalationInfo{Count: 2}
	findings := computePlanCritiqueFindings(nil, []planStepView{{ID: "s"}}, nil, nil, nil, esc, planStateBuilding)
	f := findFinding(findings, "design-review", "blocked by escalation")
	if f == nil || f.Severity != critiqueBlock {
		t.Fatalf("expected BLOCK for escalation-blocked plan; got %+v", f)
	}
}

// TestStructure_BlockedSteps — open steps waiting on blockers are info.
func TestStructure_BlockedSteps(t *testing.T) {
	steps := []planStepView{
		{ID: "gd-a", Title: "a", BlockedBy: []planStepBlockerView{{ID: "gd-x", Title: "x"}}},
		{ID: "gd-b", Title: "b"},
	}
	findings := computePlanCritiqueFindings(nil, steps, nil, nil, nil, planEscalationInfo{}, planStateBuilding)
	if findFinding(findings, "structure", "waiting on blockers") == nil {
		t.Fatalf("expected structure finding for blocked steps")
	}
}

// TestFindings_SortedBySeverity — blockers first, then warns, then info.
func TestFindings_SortedBySeverity(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-plan"} // no description
	steps := []planStepView{
		{ID: "gd-a", Title: "a", MRURL: "x", MRState: "merged", Closed: false}, // proof-gap warn
		{ID: "gd-b", Title: "b", BlockedBy: []planStepBlockerView{{ID: "z"}}},  // structure info
	}
	esc := planEscalationInfo{Count: 1} // design-review block
	findings := computePlanCritiqueFindings(bead, steps, nil, nil, nil, esc, planStateBuilding)
	if len(findings) < 3 {
		t.Fatalf("expected >=3 findings, got %d", len(findings))
	}
	// First must be a blocker, last must be info (non-increasing severity).
	for i := 1; i < len(findings); i++ {
		if findings[i-1].Severity < findings[i].Severity {
			t.Errorf("findings not sorted most-severe-first at %d: %v then %v", i, findings[i-1].Severity, findings[i].Severity)
		}
	}
}

// TestBuildFindingsData_TalliesAndBasePath — the render projection folds the
// per-severity tallies and prefixes anchor hrefs with the base path.
func TestBuildFindingsData_TalliesAndBasePath(t *testing.T) {
	findings := []critiqueFinding{
		{Severity: critiqueBlock, Category: "design-review", Title: "b", AnchorHref: "/gd-x", AnchorLabel: "Step 1"},
		{Severity: critiqueWarn, Category: "proof-gap", Title: "w"},
		{Severity: critiqueInfo, Category: "structure", Title: "i"},
	}
	data := buildPlanCritiqueFindingsData("gd-plan", "/beads", findings)
	if data.BlockerCount != 1 || data.WarnCount != 1 || data.InfoCount != 1 {
		t.Errorf("tallies = %d/%d/%d, want 1/1/1", data.BlockerCount, data.WarnCount, data.InfoCount)
	}
	if data.Findings[0].AnchorHref != "/beads/gd-x" {
		t.Errorf("anchor href = %q, want /beads/gd-x", data.Findings[0].AnchorHref)
	}
	if data.Findings[0].SeveritySlug != "block" {
		t.Errorf("severity slug = %q, want block", data.Findings[0].SeveritySlug)
	}
}

// TestBuildFindingsData_Empty — no findings yields a nil Findings slice so the
// template suppresses the whole panel.
func TestBuildFindingsData_Empty(t *testing.T) {
	data := buildPlanCritiqueFindingsData("gd-plan", "/beads", nil)
	if data.Findings != nil {
		t.Errorf("empty findings should yield nil Findings, got %v", data.Findings)
	}
}

// TestCleanPlan_NoFindings — a well-formed plan (narrative, closed steps, no
// escalations, no open questions) produces no findings.
func TestCleanPlan_NoFindings(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-plan", Description: "A crisp, well-scoped plan."}
	prose := &bundleView{Docs: []bundleDocCard{{BeadID: "gd-doc", Format: "md"}}}
	steps := []planStepView{
		{ID: "gd-a", Title: "a", Closed: true, MRURL: "x", MRState: "merged", SectionRef: planSectionRefView{Label: "C1"}},
		{ID: "gd-b", Title: "b", Closed: true, SectionRef: planSectionRefView{Label: "C2"}},
	}
	findings := computePlanCritiqueFindings(bead, steps, nil, nil, prose, planEscalationInfo{}, planStateBuilding)
	if len(findings) != 0 {
		t.Fatalf("expected no findings on a clean plan, got %d: %+v", len(findings), findings)
	}
}

// TestPlanCritiqueFindings_Renders executes the partial through the parsed
// template set and asserts the panel renders findings with collapsible detail —
// and self-suppresses on an empty bag (byte-identical-to-today guarantee).
func TestPlanCritiqueFindings_Renders(t *testing.T) {
	s := newTestServer(t)
	tmpl, ok := s.partials["templates/partials/plan_critique_findings.html"]
	if !ok {
		t.Fatalf("plan_critique_findings partial not registered")
	}

	findings := []critiqueFinding{
		{Severity: critiqueBlock, Category: "design-review", Title: "Plan blocked by escalation", Detail: "An escalation blocks this plan.", Remediation: "Approve it."},
		{Severity: critiqueWarn, Category: "proof-gap", Title: "Step's MR merged but step still open", Detail: "MR merged, step open.", Remediation: "Close the step.", AnchorLabel: "Step 1", AnchorHref: "/gd-a"},
	}
	data := buildPlanCritiqueFindingsData("gd-plan", "/beads", findings)

	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "plan_critique_findings", data); err != nil {
		t.Fatalf("execute plan_critique_findings: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Automated critique",
		"1 blocker",
		"1 warn",
		"pcf-sev-block",
		"Plan blocked by escalation",
		"<details",              // collapsible detail sections
		"/beads/gd-a",           // base-path-prefixed anchor
		"→ Step 1",              // deep-link label
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered panel missing %q\n---\n%s", want, out)
		}
	}
	// Blocker findings render open by default; warns stay collapsed.
	if !strings.Contains(out, `<details class="pcf-details" open>`) {
		t.Errorf("blocker finding should render <details open>")
	}

	// Empty bag => empty render (panel fully suppressed).
	empty := buildPlanCritiqueFindingsData("gd-plan", "/beads", nil)
	var eb strings.Builder
	if err := tmpl.ExecuteTemplate(&eb, "plan_critique_findings", empty); err != nil {
		t.Fatalf("execute empty: %v", err)
	}
	if strings.Contains(eb.String(), "Automated critique") {
		t.Errorf("empty findings bag must suppress the whole panel, got:\n%s", eb.String())
	}
}
