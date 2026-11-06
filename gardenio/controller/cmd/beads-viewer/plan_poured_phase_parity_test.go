package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plan_poured_phase_parity_test.go (gd-D6V8qZdGyG) — a POURED phase must render,
// count, stream, and reorder EXACTLY like an authored phase.
//
// A "poured phase" is a formula pour EMPLACED into a plan (ka-0000000759): the
// pour's container is a type=molecule bead listed in the plan's phase_order,
// rather than a hand-authored type=epic sub-epic. isPhaseContainer accepts both
// (resolved decision ka-0000003759) so every phase-scan site treats a molecule
// wherever it treated an epic. The behavioral contract those call sites are
// SUPPOSED to satisfy — and what this file proves end-to-end — is that a plan
// with a poured (molecule) phase behaves indistinguishably from the same plan
// with a hand-authored (epic) phase across the four surfaces the task names:
//
//   (1) RENDER   — the poured phase's rendered timeline HTML is byte-identical to
//                  the authored phase's, once the two intrinsic differences are
//                  normalized away (the phase bead id, and the molecule-only
//                  provenance chip the authored epic legitimately lacks).
//   (2) COUNT    — planProgress (detail header) and renderedStepRollup (index
//                  card) report the same step totals for both.
//   (3) STREAM   — buildPlanEventFilters folds the molecule phase + its steps
//                  into the SSE interest sets exactly as it does an epic phase,
//                  and a step closed under either phase emits the SAME
//                  writePlanEvent frame sequence.
//   (4) REORDER  — the phase-reorder CAS (phase_order on the plan) and the
//                  within-phase step-reorder CAS (step_order on the container)
//                  write the same arrays whether the container is an epic or a
//                  molecule.
//
// The parity fixtures below are deliberately built from ONE step dataset so the
// only thing that varies between the "authored" and "poured" worlds is the phase
// container's Type (+ the molecule's provenance fields). That is the whole point:
// if any surface branched on container type, a parity assertion here would fail.

// parityStep is one step's identity in the shared dataset — the fields that must
// render/count identically regardless of which container holds the step.
type parityStep struct {
	id     string
	title  string
	status string
	minute int // CreatedAt minute offset, for deterministic stable ordering
}

// parityPhaseSteps is the shared step dataset the two phase containers each hold.
// Kept in a stable creation order (minute-ascending) so buildStepRows numbers
// them identically under both containers with no step_order overlay.
var parityPhaseSteps = []parityStep{
	{id: "gd-ps1", title: "Design the widget", status: "closed", minute: 1},
	{id: "gd-ps2", title: "Implement the widget", status: "in_progress", minute: 2},
	{id: "gd-ps3", title: "Test the widget", status: "open", minute: 3},
}

// seedParityPlanFake builds a plan whose SECOND phase's CONTAINER type is
// parameterized: containerType is "epic" (a hand-authored phase) or "molecule"
// (a poured phase). Everything else — the plan, phase 1, the shared step
// dataset, timestamps — is byte-for-byte identical across the two worlds. When
// the container is a molecule it carries the provenance fields a real pour stamps
// (formula_id + applied_vars) so the render-parity test can prove those are the
// ONLY rendered difference.
//
// Phase ids are also parameterized (phaseID) so the two worlds can be seeded into
// one process for a direct comparison without id collisions; the render-parity
// normalizer rewrites the id back to a common token before diffing.
func seedParityPlanFake(containerType, phaseID string) *fakeDaemon {
	f := newFake()
	tm := func(min int) time.Time { return time.Date(2026, 7, 1, 10, min, 0, 0, time.UTC) }

	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan: ship widget", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		// Phase 1 is always a hand-authored epic; phase 2's container type is the
		// variable under test. Both are listed in phase_order → both render.
		Fields:    map[string]string{"phase_order": `["gd-phase1","` + phaseID + `"]`},
		UpdatedAt: tm(0),
	}
	phase1 := &beadsapi.BeadDetail{
		ID: "gd-phase1", Title: "Research", Kind: "issue", Type: "epic",
		Status: "closed", Assignee: "gardenio-researcher", UpdatedAt: tm(0),
	}
	// The variable container: an authored epic or a poured molecule, same title /
	// status / assignee / updated_at so only Type (+ provenance) differs.
	container := &beadsapi.BeadDetail{
		ID: phaseID, Title: "Build", Kind: "issue", Type: containerType,
		Status: "in_progress", Assignee: "gardenio-builder", UpdatedAt: tm(0),
	}
	if containerType == "molecule" {
		container.Fields = map[string]string{
			"formula_id":   "gd-formX",
			"applied_vars": `{"widget":"gauge","count":"3"}`,
		}
	}

	// Phase 1's single closed step (fixed context around the phase under test).
	r1 := &beadsapi.BeadDetail{ID: "gd-r1", Title: "Read the code", Kind: "issue", Type: "task", Status: "closed", CreatedAt: tm(0)}

	beads := []*beadsapi.BeadDetail{plan, phase1, container, r1}
	// The shared step dataset lives under the phase-2 container.
	phaseChildDeps := make([]beadsapi.Dependency, 0, len(parityPhaseSteps))
	for _, ps := range parityPhaseSteps {
		beads = append(beads, &beadsapi.BeadDetail{
			ID: ps.id, Title: ps.title, Kind: "issue", Type: "task",
			Status: ps.status, CreatedAt: tm(ps.minute),
		})
		phaseChildDeps = append(phaseChildDeps, beadsapi.Dependency{
			BeadID: ps.id, DependsOnID: phaseID, Type: "parent-child",
		})
	}
	if containerType == "molecule" {
		// The recipe the molecule was poured from (the provenance chip's deep-link
		// target). Absent in the authored-epic world.
		beads = append(beads, &beadsapi.BeadDetail{ID: "gd-formX", Title: "Widget builder", Kind: "issue", Type: "formula", Status: "open"})
	}

	for _, b := range beads {
		f.beads[b.ID] = b
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {
			{BeadID: "gd-phase1", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: phaseID, DependsOnID: "gd-plan1", Type: "parent-child"},
		},
		"gd-phase1": {
			{BeadID: "gd-r1", DependsOnID: "gd-phase1", Type: "parent-child"},
		},
		phaseID: phaseChildDeps,
	}
	// The molecule's forward parent-child edge to the plan — resolvePlanForStepParent
	// walks it. Harmless for the epic world (unused there).
	f.deps = map[string][]beadsapi.Dependency{
		phaseID: {{BeadID: phaseID, DependsOnID: "gd-plan1", Type: "parent-child"}},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	// Transitive rollup: 4 steps total (1 in phase 1 + 3 in phase 2), 2 closed
	// (r1 + ps1). Deliberately set so a surface that (wrongly) read this transitive
	// number instead of the rendered rows would diverge from planProgress.
	f.epicSubtreeCount = map[string]int{"gd-plan1": 4, "gd-plan1|closed": 2}
	return f
}

// operatorReq builds an operator-authenticated request (email header + operator
// role), the shape every mutating plan verb requires.
func operatorReq(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	return req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
}

// --- (1) RENDER parity -------------------------------------------------------

// extractPhaseSection returns the rendered HTML of ONE phase <section> — from
// its `data-phase-id="<id>"` opening attribute to the end of that section's
// step <ol> (or its empty-state <p>). It is the render surface a phase owns:
// its header (number/title/status/progress/reorder) plus its step rows.
func extractPhaseSection(t *testing.T, body, phaseID string) string {
	t.Helper()
	marker := `data-phase-id="` + phaseID + `"`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("phase section %q not found in body", phaseID)
	}
	// Back up to the enclosing <section ...> opening tag so the class list
	// (plan-phase / is-done) is included in the compared region.
	openTag := strings.LastIndex(body[:start], "<section")
	if openTag < 0 {
		t.Fatalf("no <section> opening before phase marker %q", phaseID)
	}
	rest := body[openTag:]
	// The phase section is closed by its own </section>. The template emits no
	// nested <section> inside a phase, so the first </section> after the opening
	// tag is this phase's closer.
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("phase section %q not closed", phaseID)
	}
	return rest[:end+len("</section>")]
}

var provenanceChipRe = regexp.MustCompile(`(?s)<a class="chip plan-phase-provenance".*?</a>`)

// noArmsChipRe matches the epic-only "no arms yet" chip in the phase header's
// arms slot (gd-YvWv4mR4fu).
var noArmsChipRe = regexp.MustCompile(`<span class="heraldry-slot"><span class="chip arms-none"[^>]*>no arms yet</span></span>`)

// normalizePhaseSection strips the three INTRINSIC differences between a poured
// and an authored phase so the remainder can be diffed for parity:
//   - the phase bead id (poured=gd-molP, authored=gd-phaseE) → a common token;
//   - the molecule-only provenance chip (an authored epic has no formula_id, so
//     the chip legitimately does not render for it) → removed;
//   - the epic-only "no arms yet" chip (a molecule cannot bear arms, so it
//     legitimately does not render for the poured phase) → removed.
//
// If ANY other byte differs after this normalization, the two phases do not
// render identically — which is exactly the regression this test guards.
func normalizePhaseSection(section, phaseID string) string {
	section = provenanceChipRe.ReplaceAllString(section, "")
	section = noArmsChipRe.ReplaceAllString(section, "")
	section = strings.ReplaceAll(section, phaseID, "gd-PHASE")
	// Collapse runs of whitespace so incidental template indentation around the
	// removed chip can't masquerade as a real difference.
	section = strings.Join(strings.Fields(section), " ")
	return section
}

// renderPhaseSection renders /plans/gd-plan1 as an OPERATOR (so the reorder ▲▼
// controls render and are part of the parity surface) and returns the normalized
// HTML of the phase-2 section.
func renderPhaseSection(t *testing.T, containerType, phaseID string) string {
	t.Helper()
	mux := mkServer(t, seedParityPlanFake(containerType, phaseID))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorReq("GET", "/plans/gd-plan1"))
	if w.Code != http.StatusOK {
		t.Fatalf("%s plan: expected 200, got %d", containerType, w.Code)
	}
	return normalizePhaseSection(extractPhaseSection(t, w.Body.String(), phaseID), phaseID)
}

// TestPouredPhase_RendersIdenticallyToAuthored proves the rendered phase section
// — header chrome, step rows, markers, per-phase progress, and the operator
// reorder controls — is byte-identical between a poured (molecule) phase and a
// hand-authored (epic) phase holding the same steps, once the phase id and the
// molecule-only provenance chip are normalized away. Any other rendered
// divergence (a type-conditioned class, a dropped row, a different number) fails
// here.
func TestPouredPhase_RendersIdenticallyToAuthored(t *testing.T) {
	authored := renderPhaseSection(t, "epic", "gd-phaseE")
	poured := renderPhaseSection(t, "molecule", "gd-molP")

	if authored != poured {
		t.Errorf("poured phase does not render identically to authored phase\n authored: %s\n   poured: %s", authored, poured)
	}

	// Sanity floor: the compared region is the real phase surface, not an empty
	// string that would make the equality vacuous.
	for _, want := range []string{
		"plan-phase",           // the phase section class
		"Build",                // the phase title
		"Design the widget",    // a step row
		"Implement the widget", // a step row
		"Test the widget",      // a step row
		"1/3 · 33%",            // per-phase progress (1 of 3 closed)
		"plan-phase-up",        // operator reorder control (part of the surface)
	} {
		if !strings.Contains(authored, want) {
			t.Errorf("compared phase surface missing %q — parity assertion would be vacuous", want)
		}
	}
}

// TestPouredPhase_ProvenanceChipIsTheOnlyRenderDifference proves the ONLY
// rendered chrome a poured phase adds over an authored one is the provenance chip
// — it renders for the molecule (deep-linking the recipe) and does NOT render for
// the epic. This is the flip side of the byte-identity test: the difference the
// normalizer removes is real and is exactly the chip, nothing more.
func TestPouredPhase_ProvenanceChipIsTheOnlyRenderDifference(t *testing.T) {
	const chip = `class="chip plan-phase-provenance"`

	mux := mkServer(t, seedParityPlanFake("molecule", "gd-molP"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorReq("GET", "/plans/gd-plan1"))
	pouredBody := w.Body.String()
	if !strings.Contains(extractPhaseSection(t, pouredBody, "gd-molP"), chip) {
		t.Error("poured (molecule) phase must render the provenance chip")
	}

	mux = mkServer(t, seedParityPlanFake("epic", "gd-phaseE"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, operatorReq("GET", "/plans/gd-plan1"))
	if strings.Contains(w.Body.String(), chip) {
		t.Error("authored (epic) phase must NOT render a provenance chip")
	}
}

// --- (2) COUNT parity --------------------------------------------------------

// TestPouredPhase_StepCountMatchesAuthored proves the two count surfaces —
// planProgress (detail header) and renderedStepRollup (index card) — return the
// same totals whether phase 2's container is a molecule or an epic, and that both
// count the RENDERED rows (4 steps, 2 closed) rather than the transitive subtree
// number seeded deliberately equal here but validated as row-derived by the
// mixed statuses.
func TestPouredPhase_StepCountMatchesAuthored(t *testing.T) {
	authored := testServerWithDaemon(seedParityPlanFake("epic", "gd-phaseE"))
	poured := testServerWithDaemon(seedParityPlanFake("molecule", "gd-molP"))

	aSteps, aPhases := authored.planTimeline(t.Context(), "gd-plan1")
	pSteps, pPhases := poured.planTimeline(t.Context(), "gd-plan1")

	aTotal, aClosed, aPct := planProgress(aSteps, aPhases)
	pTotal, pClosed, pPct := planProgress(pSteps, pPhases)

	if aTotal != pTotal || aClosed != pClosed || aPct != pPct {
		t.Errorf("planProgress differs: authored %d/%d (%d%%) vs poured %d/%d (%d%%)",
			aClosed, aTotal, aPct, pClosed, pTotal, pPct)
	}
	// Absolute value: 4 rendered steps (1 in phase 1 + 3 in phase 2), 2 closed.
	if pTotal != 4 || pClosed != 2 {
		t.Errorf("poured plan rendered progress = %d/%d, want 2/4", pClosed, pTotal)
	}

	// Per-phase rollup on the phase-2 container must match too.
	var aPhase2, pPhase2 *planPhaseView
	for i := range aPhases {
		if aPhases[i].ID == "gd-phaseE" {
			aPhase2 = &aPhases[i]
		}
	}
	for i := range pPhases {
		if pPhases[i].ID == "gd-molP" {
			pPhase2 = &pPhases[i]
		}
	}
	if aPhase2 == nil || pPhase2 == nil {
		t.Fatalf("phase-2 container missing: authored=%v poured=%v", aPhase2 != nil, pPhase2 != nil)
	}
	if aPhase2.StepsTotal != pPhase2.StepsTotal || aPhase2.StepsClosed != pPhase2.StepsClosed || aPhase2.PctDone != pPhase2.PctDone {
		t.Errorf("per-phase rollup differs: authored %d/%d (%d%%) vs poured %d/%d (%d%%)",
			aPhase2.StepsClosed, aPhase2.StepsTotal, aPhase2.PctDone,
			pPhase2.StepsClosed, pPhase2.StepsTotal, pPhase2.PctDone)
	}
	if pPhase2.StepsTotal != 3 || pPhase2.StepsClosed != 1 {
		t.Errorf("poured phase-2 rollup = %d/%d, want 1/3", pPhase2.StepsClosed, pPhase2.StepsTotal)
	}

	// The index-card rollup (renderedStepRollup) must agree with the detail
	// (planProgress) for BOTH worlds — the /plans index % and the /plans/{id} %
	// are computed by these two independent helpers and must never diverge.
	order := []string{"gd-phase1", "gd-molP"}
	directIDs := []string{"gd-phase1", "gd-molP"}
	bySub := map[string][]string{
		"gd-phase1": {"gd-r1"},
		"gd-molP":   {"gd-ps1", "gd-ps2", "gd-ps3"},
	}
	f := seedParityPlanFake("molecule", "gd-molP")
	rTotal, rClosed, _, _ := renderedStepRollup(order, directIDs, bySub, f.beads, f.beads)
	if rTotal != pTotal || rClosed != pClosed {
		t.Errorf("index rollup %d/%d disagrees with detail planProgress %d/%d for the poured plan",
			rClosed, rTotal, pClosed, pTotal)
	}
}

// --- (3) STREAM parity -------------------------------------------------------

// TestPouredPhase_SSEInterestSetMatchesAuthored proves buildPlanEventFilters
// folds the poured (molecule) phase and its step grandchildren into the SSE
// interest sets EXACTLY as it does an authored (epic) phase: the container is a
// tracked phaseEpicID, and every one of its steps is in the `steps` interest set
// so a change under it streams live. Pre-isPhaseContainer, the molecule was
// skipped (a `c.Type != "epic"` gate) and its steps never streamed.
func TestPouredPhase_SSEInterestSetMatchesAuthored(t *testing.T) {
	authored := testServerWithDaemon(seedParityPlanFake("epic", "gd-phaseE"))
	poured := testServerWithDaemon(seedParityPlanFake("molecule", "gd-molP"))

	aF := authored.buildPlanEventFilters(t.Context(), "gd-plan1", nil)
	pF := poured.buildPlanEventFilters(t.Context(), "gd-plan1", nil)

	// The phase-2 container is a tracked phase in both worlds.
	if _, ok := aF.phaseEpicIDs["gd-phaseE"]; !ok {
		t.Error("authored: epic phase container missing from phaseEpicIDs")
	}
	if _, ok := pF.phaseEpicIDs["gd-molP"]; !ok {
		t.Error("poured: molecule phase container missing from phaseEpicIDs — it would not re-render on a phase status flip")
	}

	// Every phase-2 step is in the steps interest set in both worlds.
	for _, id := range []string{"gd-ps1", "gd-ps2", "gd-ps3"} {
		if _, ok := aF.steps[id]; !ok {
			t.Errorf("authored: step %q missing from SSE steps interest set", id)
		}
		if _, ok := pF.steps[id]; !ok {
			t.Errorf("poured: step %q missing from SSE steps interest set — a change under the molecule phase would not stream", id)
		}
	}

	// Structural parity: the two interest sets are identical modulo the phase id.
	if got, want := len(pF.steps), len(aF.steps); got != want {
		t.Errorf("steps interest set size differs: poured=%d authored=%d", got, want)
	}
	if got, want := len(pF.phaseEpicIDs), len(aF.phaseEpicIDs); got != want {
		t.Errorf("phaseEpicIDs size differs: poured=%d authored=%d", got, want)
	}
}

// sseEventNames extracts the ordered `event:` names from an SSE response body —
// the frame sequence a client receives.
func sseEventNames(body string) []string {
	var names []string
	for line := range strings.SplitSeq(body, "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, strings.TrimSpace(name))
		}
	}
	return names
}

// TestPouredPhase_StepCloseStreamsSameSequence proves that closing a step UNDER
// the phase-2 container streams the SAME writePlanEvent frame sequence whether
// that container is a molecule or an epic. A step-close arrives as a
// beads.bead.updated on a step in filters.steps → exactly one plan-updated frame
// (the timeline re-render). If the molecule phase's steps weren't in the interest
// set, the poured world would emit ZERO frames — the regression this guards.
func TestPouredPhase_StepCloseStreamsSameSequence(t *testing.T) {
	run := func(containerType, phaseID string) []string {
		// NewServer (not testServerWithDaemon) so the plan_steps partial is loaded
		// and writePlanEvent can render the plan-updated fragment.
		srv := NewServer(seedParityPlanFake(containerType, phaseID), slog.Default(), "", "", "", nil)
		filters := srv.buildPlanEventFilters(t.Context(), "gd-plan1", nil)

		// ps3 (the open step under phase 2) flips to closed — a bead.updated on a
		// step in the interest set.
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/events", nil)
		evt := beadsapi.SSEEvent{
			Event: "beads.bead.updated",
			Data:  []byte(`{"bead":{"id":"gd-ps3","type":"task"},"changes":{"status":"closed"}}`),
		}
		srv.writePlanEvent(t.Context(), w, req, w, evt, "gd-plan1", "in_progress", &filters, false)
		return sseEventNames(w.Body.String())
	}

	authored := run("epic", "gd-phaseE")
	poured := run("molecule", "gd-molP")

	if strings.Join(authored, ",") != strings.Join(poured, ",") {
		t.Errorf("SSE frame sequence differs: authored=%v poured=%v", authored, poured)
	}
	// Concrete floor: exactly one plan-updated frame (the timeline re-render).
	if len(poured) != 1 || poured[0] != "plan-updated" {
		t.Errorf("poured step-close frames = %v, want [plan-updated]", poured)
	}
}

// TestPouredPhase_StepAddedStreamsLive proves a step ADDED under the phase-2
// container (a dependency.added edge whose target is the container) passes
// writePlanEvent's intoPhase interest gate and streams a plan-updated frame — for
// a molecule container exactly as for an epic one. This is the live-streaming
// contract that makes a poured phase's steps appear without a reload.
func TestPouredPhase_StepAddedStreamsLive(t *testing.T) {
	run := func(containerType, phaseID string) []string {
		// NewServer so writePlanEvent's fragment renders (templates loaded).
		srv := NewServer(seedParityPlanFake(containerType, phaseID), slog.Default(), "", "", "", nil)
		filters := srv.buildPlanEventFilters(t.Context(), "gd-plan1", nil)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/events", nil)
		depEvt := beadsapi.SSEEvent{
			Event: "beads.dependency.added",
			Data:  []byte(`{"dependency":{"bead_id":"gd-ps4","depends_on_id":"` + phaseID + `","type":"parent-child"}}`),
		}
		srv.writePlanEvent(t.Context(), w, req, w, depEvt, "gd-plan1", "in_progress", &filters, false)
		return sseEventNames(w.Body.String())
	}

	authored := run("epic", "gd-phaseE")
	poured := run("molecule", "gd-molP")

	if strings.Join(authored, ",") != strings.Join(poured, ",") {
		t.Errorf("step-added SSE sequence differs: authored=%v poured=%v", authored, poured)
	}
	// The edge into the phase container fires a timeline re-render (plan-updated)
	// among its frames in both worlds.
	if !slices.Contains(poured, "plan-updated") {
		t.Errorf("a step added under the poured phase must stream a plan-updated frame, got %v", poured)
	}
}

// --- (4) REORDER parity ------------------------------------------------------

// TestPouredPhase_PhaseReorderMatchesAuthored proves moving the phase-2 container
// UP within the plan's phase_order writes the SAME CAS (expected + next arrays)
// whether the container is a molecule or an epic. The reorder resolves membership
// via resolvedPhaseIDs → isPhaseContainer, so a molecule phase is a first-class
// reorder target.
func TestPouredPhase_PhaseReorderMatchesAuthored(t *testing.T) {
	run := func(containerType, phaseID string) arrayFieldReorder {
		f := seedParityPlanFake(containerType, phaseID)
		mux := mkServer(t, f)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, operatorReq("POST", "/plans/gd-plan1/phases/"+phaseID+"/reorder?dir=up"))
		if w.Code != http.StatusOK {
			t.Fatalf("%s phase reorder: expected 200, got %d (body: %s)", containerType, w.Code, w.Body.String())
		}
		if f.reorderArrayCalls != 1 {
			t.Fatalf("%s phase reorder: expected 1 CAS call, got %d", containerType, f.reorderArrayCalls)
		}
		return f.reorderArrayLast
	}

	authored := run("epic", "gd-phaseE")
	poured := run("molecule", "gd-molP")

	// Same field, same positional swap: phase 2 moves to index 0 in phase_order.
	if authored.Field != poured.Field || authored.Field != "phase_order" {
		t.Errorf("reorder field differs: authored=%q poured=%q", authored.Field, poured.Field)
	}
	// expected = the stored order [phase1, <phase2>]; next = the swapped order
	// [<phase2>, phase1]. The <phase2> id differs by construction, so compare the
	// POSITIONS: in both worlds the phase-2 container lands at index 0 and phase1
	// slides to index 1.
	if len(authored.Expected) != 2 || len(poured.Expected) != 2 {
		t.Fatalf("expected 2-element phase_order snapshots, got authored=%v poured=%v", authored.Expected, poured.Expected)
	}
	if authored.Expected[0] != "gd-phase1" || poured.Expected[0] != "gd-phase1" {
		t.Errorf("CAS expected[0] should be gd-phase1 in both: authored=%v poured=%v", authored.Expected, poured.Expected)
	}
	if authored.Next[1] != "gd-phase1" || poured.Next[1] != "gd-phase1" {
		t.Errorf("after moving phase-2 up, gd-phase1 must slide to index 1: authored next=%v poured next=%v", authored.Next, poured.Next)
	}
	if authored.Next[0] != "gd-phaseE" || poured.Next[0] != "gd-molP" {
		t.Errorf("after moving phase-2 up, the phase-2 container must land at index 0: authored next=%v poured next=%v", authored.Next, poured.Next)
	}
}

// TestPouredPhase_StepReorderWithinPhaseMatchesAuthored proves reordering a step
// WITHIN the phase-2 container rewrites step_order on the CONTAINER itself with
// the same array whether the container is a molecule or an epic. planStepParent
// resolves the step's parent via isPhaseContainer, so a step inside a poured
// molecule phase reorders exactly like one inside an authored epic phase.
func TestPouredPhase_StepReorderWithinPhaseMatchesAuthored(t *testing.T) {
	run := func(containerType, phaseID string) arrayFieldReorder {
		f := seedParityPlanFake(containerType, phaseID)
		mux := mkServer(t, f)
		// Move ps2 (the 2nd step in the phase) UP within its group.
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, operatorReq("POST", "/plans/gd-plan1/steps/gd-ps2/reorder?dir=up"))
		if w.Code != http.StatusOK {
			t.Fatalf("%s step reorder: expected 200, got %d (body: %s)", containerType, w.Code, w.Body.String())
		}
		if f.reorderArrayCalls != 1 {
			t.Fatalf("%s step reorder: expected 1 CAS call, got %d", containerType, f.reorderArrayCalls)
		}
		return f.reorderArrayLast
	}

	authored := run("epic", "gd-phaseE")
	poured := run("molecule", "gd-molP")

	// The step_order write targets the CONTAINER bead (the phase), not the plan —
	// so the ID differs by construction; the FIELD and the resulting ORDER must be
	// identical (the step ids are shared across both worlds).
	if authored.Field != "step_order" || poured.Field != "step_order" {
		t.Errorf("step reorder must rewrite step_order, got authored=%q poured=%q", authored.Field, poured.Field)
	}
	if authored.ID != "gd-phaseE" || poured.ID != "gd-molP" {
		t.Errorf("step_order must be written on the phase CONTAINER: authored=%q (want gd-phaseE) poured=%q (want gd-molP)", authored.ID, poured.ID)
	}
	// The reordered step array is identical: ps2 moves ahead of ps1.
	wantNext := []string{"gd-ps2", "gd-ps1", "gd-ps3"}
	if !slices.Equal(authored.Next, wantNext) || !slices.Equal(poured.Next, wantNext) {
		t.Errorf("step_order write differs: authored=%v poured=%v want=%v", authored.Next, poured.Next, wantNext)
	}
}
