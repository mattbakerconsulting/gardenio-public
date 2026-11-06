package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// formulasFake routes ListBeadsFiltered by query shape so the /formulas handler
// can be seeded with a formula-recipe list and a molecule list independently.
// Embeds *fakeDaemon for the rest of the beadAPI surface (inboxCountForViewer,
// viewerVibeMode user-bead lookup, etc.), all of which default to empty.
type formulasFake struct {
	*fakeDaemon

	formulasResp  *beadsapi.ListBeadsResult // Types=[formula]
	moleculesResp *beadsapi.ListBeadsResult // Types=[molecule]

	formulasErr  error
	moleculesErr error

	// lastFormulaQ / lastMoleculeQ capture the queries for assertion.
	lastFormulaQ  *beadsapi.ListBeadsQuery
	lastMoleculeQ *beadsapi.ListBeadsQuery
}

func (f *formulasFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	switch {
	case slices.Contains(q.Types, "formula"):
		qq := q
		f.lastFormulaQ = &qq
		return orEmpty(f.formulasResp), f.formulasErr
	case slices.Contains(q.Types, "molecule"):
		qq := q
		f.lastMoleculeQ = &qq
		return orEmpty(f.moleculesResp), f.moleculesErr
	}
	return &beadsapi.ListBeadsResult{}, nil
}

func formulasServer(f *formulasFake) *Server {
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) })
	return srv
}

func formulaBeadFixture(id, title, project string, fields map[string]string) *beadsapi.BeadDetail {
	labels := []string{}
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "formula",
		Status: "open",
		Title:  title,
		Labels: labels,
		Fields: fields,
	}
}

func moleculeBeadFixture(id, formulaID, status, project string) *beadsapi.BeadDetail {
	labels := []string{}
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "molecule",
		Status: status,
		Title:  "molecule " + id,
		Labels: labels,
		Fields: map[string]string{"formula_id": formulaID},
	}
}

// stepsJSON / varsJSON build minimal JSON-array field values with n elements,
// mirroring how the daemon re-marshals the formula's steps/vars arrays.
func stepsJSON(n int) string { return jsonArrayOfN(n) }
func varsJSON(n int) string  { return jsonArrayOfN(n) }

func jsonArrayOfN(n int) string {
	if n <= 0 {
		return "[]"
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "{}"
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestBuildFormulaRow_CountsBadgesAndRollup covers the core row projection:
// step/var counts from the JSON fields, formula_type default + phase badge, and
// the live-molecule rollup (count, pips, frontier).
func TestBuildFormulaRow_CountsBadgesAndRollup(t *testing.T) {
	f := formulaBeadFixture("gd-f1", "Release Checklist", "gardenio", map[string]string{
		"steps": stepsJSON(6),
		"vars":  varsJSON(3),
		"phase": "liquid",
		// formula_type omitted → defaults to workflow.
	})
	mols := []*beadsapi.BeadDetail{
		moleculeBeadFixture("gd-m1", "gd-f1", "open", "gardenio"),
		moleculeBeadFixture("gd-m2", "gd-f1", "in_progress", "gardenio"),
		moleculeBeadFixture("gd-m3", "gd-f1", "open", "gardenio"),
	}

	row := buildFormulaRow(f, mols)

	if row.StepCount != 6 {
		t.Errorf("StepCount = %d, want 6", row.StepCount)
	}
	if row.VarCount != 3 {
		t.Errorf("VarCount = %d, want 3", row.VarCount)
	}
	if row.FormulaType != "workflow" {
		t.Errorf("FormulaType = %q, want workflow (default)", row.FormulaType)
	}
	if row.Phase != "liquid" {
		t.Errorf("Phase = %q, want liquid", row.Phase)
	}
	if row.Project != "gardenio" {
		t.Errorf("Project = %q, want gardenio", row.Project)
	}
	if row.LiveCount != 3 {
		t.Errorf("LiveCount = %d, want 3", row.LiveCount)
	}
	if len(row.Pips) != 3 {
		t.Fatalf("len(Pips) = %d, want 3", len(row.Pips))
	}
	// in_progress ranks first in the pip strip.
	if row.Pips[0].Status != "in_progress" {
		t.Errorf("first pip status = %q, want in_progress (rank order)", row.Pips[0].Status)
	}
	// The strip is not truncated (3 pips ≤ cap), so every molecule's status is
	// already shown by a colour-coded pip. The frontier tally would only restate
	// them (and the "3 live" count) — it is suppressed to keep the row dense.
	if row.Frontier != "" {
		t.Errorf("Frontier = %q, want empty (un-truncated strip already shows every pip)", row.Frontier)
	}
	if row.PipOverflow != 0 {
		t.Errorf("PipOverflow = %d, want 0", row.PipOverflow)
	}
}

// TestBuildFormulaRow_PipOverflow verifies the pip strip caps at
// formulaStatusPipCap and reports the remainder via PipOverflow. On a truncated
// strip the frontier tally IS rendered — it accounts for the whole live set,
// including the molecules the +N chip hides.
func TestBuildFormulaRow_PipOverflow(t *testing.T) {
	f := formulaBeadFixture("gd-f1", "Busy Formula", "gardenio", map[string]string{"steps": stepsJSON(2)})
	mols := make([]*beadsapi.BeadDetail, 0, formulaStatusPipCap+5)
	for i := range formulaStatusPipCap + 5 {
		mols = append(mols, moleculeBeadFixture("gd-m"+itoa(i), "gd-f1", "open", "gardenio"))
	}

	row := buildFormulaRow(f, mols)

	if row.LiveCount != formulaStatusPipCap+5 {
		t.Errorf("LiveCount = %d, want %d", row.LiveCount, formulaStatusPipCap+5)
	}
	if len(row.Pips) != formulaStatusPipCap {
		t.Errorf("len(Pips) = %d, want cap %d", len(row.Pips), formulaStatusPipCap)
	}
	if row.PipOverflow != 5 {
		t.Errorf("PipOverflow = %d, want 5", row.PipOverflow)
	}
	// The tally counts ALL live molecules (cap+5 open), not just the visible pips.
	if want := itoa(formulaStatusPipCap+5) + " open"; row.Frontier != want {
		t.Errorf("Frontier = %q, want %q (whole live set, incl. the +5 hidden)", row.Frontier, want)
	}
}

// TestBuildFormulaRow_NoLiveMolecules keeps zero-live rows clean: no pips, no
// frontier, LiveCount 0.
func TestBuildFormulaRow_NoLiveMolecules(t *testing.T) {
	f := formulaBeadFixture("gd-f1", "Dormant", "gardenio", map[string]string{
		"steps":        stepsJSON(4),
		"formula_type": "aspect",
	})
	row := buildFormulaRow(f, nil)
	if row.LiveCount != 0 {
		t.Errorf("LiveCount = %d, want 0", row.LiveCount)
	}
	if len(row.Pips) != 0 || row.Frontier != "" || row.PipOverflow != 0 {
		t.Errorf("expected empty rollup, got Pips=%d Frontier=%q Overflow=%d", len(row.Pips), row.Frontier, row.PipOverflow)
	}
	if row.FormulaType != "aspect" {
		t.Errorf("FormulaType = %q, want aspect", row.FormulaType)
	}
}

// TestHandleFormulas_RendersCatalogWithRollup drives the full handler + template
// render: seeds two formulas (one with live molecules, one dormant) and asserts
// the page shows titles, counts, the live rollup, and the summary bar.
func TestHandleFormulas_RendersCatalogWithRollup(t *testing.T) {
	f := &formulasFake{fakeDaemon: newFake()}
	f.formulasResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			formulaBeadFixture("gd-live", "Golden Release Path", "gardenio", map[string]string{
				"steps": stepsJSON(9), "vars": varsJSON(2), "phase": "liquid",
			}),
			formulaBeadFixture("gd-dormant", "Bug Triage Workflow", "gardenio", map[string]string{
				"steps": stepsJSON(3), "vars": varsJSON(3),
			}),
		},
		Total: 2,
	}
	f.moleculesResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			moleculeBeadFixture("gd-mol1", "gd-live", "in_progress", "gardenio"),
			moleculeBeadFixture("gd-mol2", "gd-live", "open", "gardenio"),
		},
		Total: 2,
	}

	srv := formulasServer(f)
	req := httptest.NewRequest(http.MethodGet, "/formulas", nil)
	w := httptest.NewRecorder()
	srv.handleFormulas(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"Golden Release Path",
		"Bug Triage Workflow",
		"9 steps",
		"liquid",
		"2 live",
		"no live molecules", // dormant recipe
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Summary bar: 2 formulas, 1 with live molecules, 2 live molecules total.
	if !strings.Contains(body, ">2<") {
		t.Errorf("expected a summary count of 2 in the stats banner")
	}
	// The live formula sorts ahead of the dormant one.
	if strings.Index(body, "Golden Release Path") > strings.Index(body, "Bug Triage Workflow") {
		t.Errorf("expected the live formula to sort before the dormant one")
	}

	// ka-0000003920: the catalog banner carries a scoped modifier class and a
	// narrow-viewport media rule so it collapses to a compact grid on phones
	// (instead of ~200px of stacked full-width chrome) without touching the
	// shared .stats-banner used on other pages.
	if !strings.Contains(body, `class="stats-banner stats-banner-formulas"`) {
		t.Errorf("expected the formula catalog banner to carry the scoped stats-banner-formulas class")
	}
	if !strings.Contains(body, ".stats-banner-formulas { display: grid;") {
		t.Errorf("expected a narrow-viewport grid rule scoped to .stats-banner-formulas")
	}

	// ka-0000004426: on desktop the shared block-level .stats-banner stretched to
	// the full content width, so its three KPIs huddled in the left ~40% and left
	// a ~746px (61%) near-white band to their right. A desktop-scoped rule makes
	// the catalog banner hug its content (width:max-content) so it stops being a
	// full-width card with an empty right half. Scoped to >=481px so the narrow
	// compact-grid rule (ka-0000003920) above is left untouched. The right-of-KPI
	// pixel-band outcome (no >400px all-white run) is measured on real geometry in
	// the headless Playwright suite (formulas_banner_density.spec.js).
	if !strings.Contains(body, "@media (min-width: 481px)") ||
		!strings.Contains(body, ".stats-banner-formulas { width: max-content; max-width: 100%; }") {
		t.Errorf("expected a desktop-scoped (>=481px) width:max-content hug rule on .stats-banner-formulas so the banner stops leaving a wide empty right band at 1280px (ka-0000004426)")
	}

	// ka-0000000836: the >=481px hug rule shrinks the banner CARD but leaves the
	// ~768px STRIP to its right (x[512-1280]) as dead page-background. The KPI
	// banner and the filter form are wrapped in one .formula-catalog-header and
	// laid out as a single flex row at >=768px (filters pushed right via
	// margin-left:auto) so the filters fill that band instead of stacking as a
	// second sparse strip below. The right-band pixel outcome is measured on real
	// geometry in the headless Playwright suite (formulas_banner_density.spec.js).
	if !strings.Contains(body, `class="formula-catalog-header"`) {
		t.Errorf("expected the KPI banner + filter form to be wrapped in .formula-catalog-header so they share one dense header row (ka-0000000836)")
	}
	if !strings.Contains(body, "@media (min-width: 768px)") ||
		!strings.Contains(body, ".formula-catalog-header { display: flex;") ||
		!strings.Contains(body, ".formula-catalog-header .formula-catalog-filters { margin-left: auto;") {
		t.Errorf("expected a desktop-scoped (>=768px) flex rule laying the banner + filters out as one row with the filters pushed right, filling the dead band right of the hugged banner (ka-0000000836)")
	}

	// ka-0000003457: the catalog list carries a scoped modifier class and a
	// desktop (>=1024px) two-column grid rule so the empty middle-right ~40% of a
	// 1280px viewport carries a second column of recipes instead of near-white.
	// The pixel-band + side-by-side outcome is measured on real geometry in the
	// headless Playwright suite (formulas_catalog_density.spec.js).
	if !strings.Contains(body, `class="bead-list formula-catalog-list"`) {
		t.Errorf("expected the catalog <ul> to carry the scoped formula-catalog-list class (ka-0000003457)")
	}
	if !strings.Contains(body, "@media (min-width: 1024px)") ||
		!strings.Contains(body, ".formula-catalog-list { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr));") {
		t.Errorf("expected a desktop-scoped (>=1024px) two-column grid rule on .formula-catalog-list so the catalog fills the right ~40 pct band at 1280px (ka-0000003457)")
	}
}

// TestBuildFormulaRow_SingleLineNoStackedLiveDiv is a structural guard for the
// ka-0000003457 single-dense-line fix: the recipe row must NOT re-introduce the
// old flex-COLUMN layout that stacked a separate live/pip line under the meta
// line (doubling every row's height). The row itself is a wrap-flex line and the
// live-status cluster is pulled right (margin-left:auto), not rendered as a
// block beneath the row.
func TestFormulasTemplate_SingleDenseLineRow(t *testing.T) {
	f := &formulasFake{fakeDaemon: newFake()}
	f.formulasResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			formulaBeadFixture("gd-live", "Golden Release Path", "gardenio", map[string]string{
				"steps": stepsJSON(9), "vars": varsJSON(2), "phase": "liquid",
			}),
		},
		Total: 1,
	}
	f.moleculesResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			moleculeBeadFixture("gd-mol1", "gd-live", "in_progress", "gardenio"),
		},
		Total: 1,
	}

	srv := formulasServer(f)
	req := httptest.NewRequest(http.MethodGet, "/formulas", nil)
	w := httptest.NewRecorder()
	srv.handleFormulas(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// The old two-line layout used a `.formula-row-head` wrapper div plus a
	// separate `.formula-live` block below it. The fix flattens these into one
	// wrap-flex line, so the head wrapper must be gone.
	if strings.Contains(body, "formula-row-head") {
		t.Errorf("row still uses the old .formula-row-head wrapper — the two-line layout must be flattened to one dense line (ka-0000003457)")
	}
	// The single-line row layout pulls the live-status cluster to the right with
	// margin-left:auto (in the scoped <style>).
	if !strings.Contains(body, ".formula-live { display: flex;") ||
		!strings.Contains(body, "margin-left: auto;") {
		t.Errorf("expected .formula-live to be pulled right (margin-left:auto) so the recipe fills the row width (ka-0000003457)")
	}
	// The row itself must be a wrap-flex line (not flex-direction:column).
	if !strings.Contains(body, ".formula-row { display: flex; flex-wrap: wrap;") {
		t.Errorf("expected .formula-row to be a wrap-flex single line, not a flex column (ka-0000003457)")
	}
	if strings.Contains(body, ".formula-row { display: flex; flex-direction: column;") {
		t.Errorf("row must not re-introduce the flex-COLUMN layout that stacked a separate live line (ka-0000003457)")
	}
}

// TestHandleFormulas_TypeFilter restricts the catalog to a single formula_type.
func TestHandleFormulas_TypeFilter(t *testing.T) {
	f := &formulasFake{fakeDaemon: newFake()}
	f.formulasResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			formulaBeadFixture("gd-wf", "A Workflow", "gardenio", map[string]string{"formula_type": "workflow"}),
			formulaBeadFixture("gd-asp", "An Aspect", "gardenio", map[string]string{"formula_type": "aspect"}),
		},
		Total: 2,
	}

	srv := formulasServer(f)
	req := httptest.NewRequest(http.MethodGet, "/formulas?formula_type=aspect", nil)
	w := httptest.NewRecorder()
	srv.handleFormulas(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "An Aspect") {
		t.Errorf("body should include the aspect formula")
	}
	if strings.Contains(body, "A Workflow") {
		t.Errorf("body should NOT include the workflow formula under formula_type=aspect")
	}
}

// TestHandleFormulas_HasLiveFilter hides recipes with zero live molecules when
// ?has_live=1 is set.
func TestHandleFormulas_HasLiveFilter(t *testing.T) {
	f := &formulasFake{fakeDaemon: newFake()}
	f.formulasResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			formulaBeadFixture("gd-live", "Has Instances", "gardenio", nil),
			formulaBeadFixture("gd-dormant", "No Instances", "gardenio", nil),
		},
		Total: 2,
	}
	f.moleculesResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			moleculeBeadFixture("gd-mol1", "gd-live", "open", "gardenio"),
		},
		Total: 1,
	}

	srv := formulasServer(f)
	req := httptest.NewRequest(http.MethodGet, "/formulas?has_live=1", nil)
	w := httptest.NewRecorder()
	srv.handleFormulas(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Has Instances") {
		t.Errorf("body should include the recipe with live molecules")
	}
	if strings.Contains(body, "No Instances") {
		t.Errorf("body should NOT include the dormant recipe under ?has_live=1")
	}
}

// TestHandleFormulas_ProjectFilterScopesBothQueries verifies the project filter
// is forwarded to BOTH the formula list and the molecule rollup query, so the
// rollup counts stay scoped to the same project as the catalog.
func TestHandleFormulas_ProjectFilterScopesBothQueries(t *testing.T) {
	f := &formulasFake{fakeDaemon: newFake()}
	f.formulasResp = &beadsapi.ListBeadsResult{}

	srv := formulasServer(f)
	req := httptest.NewRequest(http.MethodGet, "/formulas?project=kbeads", nil)
	w := httptest.NewRecorder()
	srv.handleFormulas(w, req)

	if f.lastFormulaQ == nil || !slices.Contains(f.lastFormulaQ.Labels, "project:kbeads") {
		t.Errorf("formula query not scoped to project:kbeads: %+v", f.lastFormulaQ)
	}
	if f.lastMoleculeQ == nil || !slices.Contains(f.lastMoleculeQ.Labels, "project:kbeads") {
		t.Errorf("molecule rollup query not scoped to project:kbeads: %+v", f.lastMoleculeQ)
	}
	// The molecule rollup only counts live instances.
	if f.lastMoleculeQ != nil {
		if !slices.Contains(f.lastMoleculeQ.Statuses, "open") || !slices.Contains(f.lastMoleculeQ.Statuses, "in_progress") {
			t.Errorf("molecule rollup should query open + in_progress, got %v", f.lastMoleculeQ.Statuses)
		}
	}
}

// TestHandleFormulas_DaemonErrorRendersBanner ensures a formula-list error
// degrades to an error banner rather than a 500.
func TestHandleFormulas_DaemonErrorRendersBanner(t *testing.T) {
	f := &formulasFake{fakeDaemon: newFake()}
	f.formulasErr = errors.New("boom")

	srv := formulasServer(f)
	req := httptest.NewRequest(http.MethodGet, "/formulas", nil)
	w := httptest.NewRecorder()
	srv.handleFormulas(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (soft-fail)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load formulas") {
		t.Errorf("expected error banner in body")
	}
}

// TestHandleFormulas_MoleculeErrorStillRendersRecipes ensures a molecule-rollup
// error only drops the live counts — the recipe rows still render.
func TestHandleFormulas_MoleculeErrorStillRendersRecipes(t *testing.T) {
	f := &formulasFake{fakeDaemon: newFake()}
	f.formulasResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			formulaBeadFixture("gd-f1", "Still Here", "gardenio", map[string]string{"steps": stepsJSON(2)}),
		},
		Total: 1,
	}
	f.moleculesErr = errors.New("rollup boom")

	srv := formulasServer(f)
	req := httptest.NewRequest(http.MethodGet, "/formulas", nil)
	w := httptest.NewRecorder()
	srv.handleFormulas(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Still Here") {
		t.Errorf("recipe row should render despite molecule rollup error")
	}
	if !strings.Contains(body, "no live molecules") {
		t.Errorf("recipe should show zero live molecules when rollup errored")
	}
}
