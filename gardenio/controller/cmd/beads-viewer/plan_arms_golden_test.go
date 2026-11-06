package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The "no arms yet" chip (badges-from-arms plan gd-pQZO9ie11D, step 3
// gd-YvWv4mR4fu): on /plans a plan or phase epic without arms shows a small chip
// in the arms slot, so the gap the plan gardener closes is visible. A poured
// molecule phase cannot bear arms and shows neither arms nor chip.

// goldenArmsPath is the checked-in #plan-steps render of seedArmsPlanFake.
// Regenerate after an intentional template change with
// `GB_UPDATE_GOLDEN=1 go test ./cmd/beads-viewer/ -run TestPlanArms_Golden`.
const goldenArmsPath = "testdata/plan_arms_steps.golden.html"

// armsNoneChip is the chip's class attribute, its render fingerprint.
const armsNoneChip = `class="chip arms-none"`

// seedArmsPlanFake builds an armless plan with three phases: an epic without
// arms, an epic with arms, and a poured molecule. Steps carry no timestamps so
// the render is clock-independent (as in plan_no_pours_golden_test.go).
func seedArmsPlanFake() *fakeDaemon {
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan: give the widget arms", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Fields:    map[string]string{"phase_order": `["gd-phA","gd-phB","gd-molP"]`},
		UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	phA := &beadsapi.BeadDetail{ID: "gd-phA", Title: "Armless phase", Kind: "issue", Type: "epic", Status: "open"}
	phB := &beadsapi.BeadDetail{ID: "gd-phB", Title: "Armed phase", Kind: "issue", Type: "epic", Status: "open",
		Heraldry: testArms}
	molP := &beadsapi.BeadDetail{ID: "gd-molP", Title: "Poured phase", Kind: "issue", Type: "molecule", Status: "open"}
	a1 := &beadsapi.BeadDetail{ID: "gd-a1", Title: "Choose the arms", Kind: "issue", Type: "task", Status: "open"}
	b1 := &beadsapi.BeadDetail{ID: "gd-b1", Title: "Draw the arms", Kind: "issue", Type: "task", Status: "closed"}
	m1 := &beadsapi.BeadDetail{ID: "gd-m1", Title: "Pour the arms", Kind: "issue", Type: "task", Status: "open"}
	for _, b := range []*beadsapi.BeadDetail{plan, phA, phB, molP, a1, b1, m1} {
		f.beads[b.ID] = b
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {
			{BeadID: "gd-phA", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-phB", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-molP", DependsOnID: "gd-plan1", Type: "parent-child"},
		},
		"gd-phA":  {{BeadID: "gd-a1", DependsOnID: "gd-phA", Type: "parent-child"}},
		"gd-phB":  {{BeadID: "gd-b1", DependsOnID: "gd-phB", Type: "parent-child"}},
		"gd-molP": {{BeadID: "gd-m1", DependsOnID: "gd-molP", Type: "parent-child"}},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	f.epicSubtreeCount = map[string]int{"gd-plan1": 6, "gd-plan1|closed": 1}
	return f
}

// TestPlanArms_Golden pins the #plan-steps fragment of a plan with an armless,
// an armed and a molecule phase: the chip renders once (the armless epic), the
// armed phase draws its arms, the molecule draws neither.
func TestPlanArms_Golden(t *testing.T) {
	srv := NewServer(seedArmsPlanFake(), slog.Default(), "", "", "", nil)
	got := srv.renderPlanStepsFragment(t.Context(), nil, "gd-plan1", false)

	if os.Getenv("GB_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenArmsPath, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("updated golden %s (%d bytes)", goldenArmsPath, len(got))
		return
	}
	wantBytes, err := os.ReadFile(goldenArmsPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", goldenArmsPath, err)
	}
	if want := string(wantBytes); got != want {
		t.Errorf("arms #plan-steps render drifted from %s; regenerate with GB_UPDATE_GOLDEN=1 if intended.\n%s",
			goldenArmsPath, firstDiff(want, got))
	}

	if n := strings.Count(got, armsNoneChip); n != 1 {
		t.Errorf("want exactly one no-arms chip (the armless epic phase), got %d", n)
	}
	phaseHead := func(id string) string {
		i := strings.Index(got, `data-phase-id="`+id+`"`)
		if i < 0 {
			t.Fatalf("phase %s not rendered", id)
		}
		head := got[i:]
		if j := strings.Index(head, "</header>"); j >= 0 {
			head = head[:j]
		}
		return head
	}
	if !strings.Contains(phaseHead("gd-phA"), armsNoneChip) {
		t.Error("the armless epic phase must show the no-arms chip")
	}
	if h := phaseHead("gd-phB"); !strings.Contains(h, `class="arms arms-24"`) || strings.Contains(h, armsNoneChip) {
		t.Errorf("the armed phase must draw its arms and no chip; head:\n%s", h)
	}
	if h := phaseHead("gd-molP"); strings.Contains(h, armsNoneChip) || strings.Contains(h, `class="arms `) {
		t.Errorf("a molecule phase cannot bear arms and must show neither; head:\n%s", h)
	}
}

// TestPlanArms_HeaderAndCardChip: the plan page header and the /plans index card
// show the chip for an armless plan, and drop it once the plan bears arms.
func TestPlanArms_HeaderAndCardChip(t *testing.T) {
	get := func(f *fakeDaemon, path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		mkServer(t, f).ServeHTTP(w, plansIndexReq(path, "matt@acme.example"))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	header := func(body string) string {
		i := strings.Index(body, `<div class="plan-title-row">`)
		if i < 0 {
			t.Fatal("plan header not rendered")
		}
		return body[i : i+strings.Index(body[i:], "</h1>")]
	}
	card := func(body string) string {
		i := strings.Index(body, `<li class="plan-card"`)
		if i < 0 {
			t.Fatal("plan card not rendered")
		}
		return body[i : i+strings.Index(body[i:], "</li>")]
	}

	f := seedArmsPlanFake()
	if !strings.Contains(header(get(f, "/plans/gd-plan1")), armsNoneChip) {
		t.Error("an armless plan's header must show the no-arms chip")
	}
	if !strings.Contains(card(get(f, "/plans")), armsNoneChip) {
		t.Error("an armless plan's card must show the no-arms chip")
	}

	f = seedArmsPlanFake()
	f.beads["gd-plan1"].Heraldry = "red 🏅 . 🖌️ . 🔎"
	if h := header(get(f, "/plans/gd-plan1")); strings.Contains(h, armsNoneChip) || !strings.Contains(h, `class="arms arms-64"`) {
		t.Errorf("an armed plan's header draws its arms and no chip; header:\n%s", h)
	}
	if c := card(get(f, "/plans")); strings.Contains(c, armsNoneChip) || !strings.Contains(c, `class="arms arms-40"`) {
		t.Errorf("an armed plan's card draws its arms and no chip; card:\n%s", c)
	}
}

// TestPlanArms_HeaderSlotInsideH1 (gd-29Ou0yBzPj): on /plans/<id> the arms slot
// (or the no-arms chip) sits INSIDE h1.plan-title, as in title_block.html, so
// .arms-64's 1em is the title size. As a flex sibling of the h1 it took the
// row's body size and reserved an empty column beside the title. The only
// thing before the h1 is the 64 px badge picture (gd-5qEMPflr4z), armed or not.
func TestPlanArms_HeaderSlotInsideH1(t *testing.T) {
	h1 := func(f *fakeDaemon) (row, title string) {
		t.Helper()
		w := httptest.NewRecorder()
		mkServer(t, f).ServeHTTP(w, plansIndexReq("/plans/gd-plan1", "matt@acme.example"))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /plans/gd-plan1: %d", w.Code)
		}
		body := w.Body.String()
		i := strings.Index(body, `<div class="plan-title-row">`)
		if i < 0 {
			t.Fatal("plan header not rendered")
		}
		row = body[i : i+strings.Index(body[i:], "</h1>")]
		j := strings.Index(row, `<h1 class="plan-title">`)
		if j < 0 {
			t.Fatalf("plan title h1 not rendered; row:\n%s", row)
		}
		return row[:j], row[j:]
	}

	const badgeImg = `<img class="badge-64" src="/image/plans/badge.svg" alt="" width="64" height="64">`
	f := seedArmsPlanFake()
	f.beads["gd-plan1"].Heraldry = testArms
	f.beads["gd-plan1"].Fields["icon_svg_url"] = "/image/plans/badge.svg"
	before, title := h1(f)
	if strings.Contains(before, "heraldry-slot") || strings.Contains(before, "plan-header-icon") || !strings.Contains(before, badgeImg) {
		t.Errorf("an armed plan with a badge puts only its badge-64 picture before its h1; got:\n%s", before)
	}
	if !strings.HasPrefix(title, `<h1 class="plan-title"><span class="heraldry-slot"><span class="arms arms-64"`) {
		t.Errorf("an armed plan's h1 must open with its 64 arms slot; got:\n%s", title)
	}

	f = seedArmsPlanFake()
	f.beads["gd-plan1"].Heraldry = testArms
	if before, _ = h1(f); strings.Contains(before, "<img") || strings.Contains(before, "heraldry-slot") {
		t.Errorf("an armed plan with no badge puts nothing before its h1; got:\n%s", before)
	}

	before, title = h1(seedArmsPlanFake())
	if strings.Contains(before, "heraldry-slot") {
		t.Errorf("the no-arms chip must sit inside the h1, not beside it; got:\n%s", before)
	}
	if !strings.HasPrefix(title, `<h1 class="plan-title"><span class="heraldry-slot"><span `+armsNoneChip) {
		t.Errorf("an armless plan's h1 must open with the no-arms chip; got:\n%s", title)
	}

	f = seedArmsPlanFake()
	f.beads["gd-plan1"].Fields["icon_svg_url"] = "/image/plans/badge.svg"
	before, title = h1(f)
	if strings.Contains(before, "plan-header-icon") || !strings.Contains(before, badgeImg) {
		t.Errorf("an armless plan with a badge draws it as the badge-64 picture before the h1; got:\n%s", before)
	}
	if !strings.HasPrefix(title, `<h1 class="plan-title"><span class="heraldry-slot"><span `+armsNoneChip) {
		t.Errorf("an armless plan with a badge still opens its h1 with the no-arms chip; got:\n%s", title)
	}
}
