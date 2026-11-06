package main

// plans_queued_faces_test.go — iter-53 (ka-0000005352): QUEUED crew surface as
// calm click-through avatar faces on the /plans index card. QUEUED was the LAST
// crew disposition that carried a COUNT facet ("· N queued · Xage", iter-31/32)
// but NO click-through faces — the working strip (iter-14/15) and the stuck strip
// (iter-29, ka-0000003793) both let an operator jump from the index straight to a
// specific WHO, but a queued crew was only ever a bare count. These tests prove
// the queued strip is projected from the SAME queued candidate set the wait-age
// reads, capped + overflowed like the sibling strips, toned CALM (not the stuck
// strip's red), and folded away on a terminal plan (reconcileTerminal zeroes Idle).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPlanCrewBulk_QueuedFacesSurfaceOpenQueuedCrew proves an OPEN queued crew
// (dispatched onto a step, pod-deferred in the pool) surfaces as a click-through
// avatar on the index — the WHO the "· N queued" count facet never carried —
// while a genuinely WORKING crew stays a green working face and a CLOSED (done)
// crew never gets a queued avatar.
func TestPlanCrewBulk_QueuedFacesSurfaceOpenQueuedCrew(t *testing.T) {
	f := seedPlansFake()
	// Two open queued crew (dispatched, pod-deferred) + a working crew + a
	// cleanly-finished closed crew.
	q1 := seedCrew(f, "gd-queA", "gardenio-prefix-cache-one", "gd-step1", "queued", "in_progress")
	q1.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC)
	q2 := seedCrew(f, "gd-queB", "gardenio-prefix-cache-two", "gd-step1", "idle", "in_progress")
	q2.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC)
	seedCrew(f, "gd-workC", "gardenio-working-three", "gd-step1", "working", "in_progress")
	done := seedCrew(f, "gd-doneD", "gardenio-closed-done", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"finished."}`
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]

	// Idle tally counts both open queued crew — and IS the queued-faces candidate set.
	if roll.Idle != 2 {
		t.Fatalf("Idle = %d, want 2 (both open queued crew)", roll.Idle)
	}
	// Both open queued crew get avatars.
	if len(roll.QueuedFaces) != 2 || !roll.HasQueuedFaces() {
		t.Fatalf("QueuedFaces=%d HasQueuedFaces=%v, want 2 queued avatars", len(roll.QueuedFaces), roll.HasQueuedFaces())
	}
	if roll.QueuedFacesHidden() != 0 {
		t.Errorf("QueuedFacesHidden = %d, want 0 (both open queued fit under the cap)", roll.QueuedFacesHidden())
	}
	// Newest-active queued crew leads the strip, and every queued face carries a
	// click-through url and is NOT marked Stuck (so the template tones it calm).
	if roll.QueuedFaces[0].ID != "gd-queB" {
		t.Errorf("first queued face = %s, want gd-queB (most-recently-active first)", roll.QueuedFaces[0].ID)
	}
	for _, fc := range roll.QueuedFaces {
		if fc.Stuck {
			t.Errorf("queued face %s must NOT be marked Stuck — it reads calm neutral, not red", fc.ID)
		}
		if fc.URL == "" {
			t.Errorf("queued face %s has empty url — should fall back to the bead page", fc.ID)
		}
		if fc.ID == "gd-workC" {
			t.Errorf("working crew %s leaked into the queued avatars", fc.ID)
		}
		if fc.ID == "gd-doneD" {
			t.Errorf("closed-done crew %s leaked into the queued avatars", fc.ID)
		}
	}
	// The working crew is still a (green) working face, never a queued one.
	if len(roll.Faces) != 1 || roll.Faces[0].ID != "gd-workC" {
		t.Errorf("working faces = %+v, want exactly gd-workC", roll.Faces)
	}
}

// TestPlanCrewBulk_QueuedFacesCapAndOverflow proves the queued strip is capped at
// planCrewFacesMax and the remaining open queued crew report via QueuedFacesHidden
// (the "+N" overflow), so a many-queued plan never blows out the card.
func TestPlanCrewBulk_QueuedFacesCapAndOverflow(t *testing.T) {
	f := seedPlansFake()
	total := planCrewFacesMax + 2
	for i := range total {
		id := "gd-que" + string(rune('A'+i))
		b := seedCrew(f, id, "gardenio-queued-"+string(rune('a'+i)), "gd-step1", "queued", "in_progress")
		b.UpdatedAt = time.Date(2026, 7, 5, 11, i, 0, 0, time.UTC)
	}
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Idle != total {
		t.Fatalf("Idle = %d, want %d", roll.Idle, total)
	}
	if len(roll.QueuedFaces) != planCrewFacesMax {
		t.Errorf("rendered queued faces = %d, want the cap %d", len(roll.QueuedFaces), planCrewFacesMax)
	}
	if roll.QueuedFacesHidden() != total-planCrewFacesMax {
		t.Errorf("QueuedFacesHidden = %d, want %d (open queued beyond the cap)", roll.QueuedFacesHidden(), total-planCrewFacesMax)
	}
}

// TestPlanCrewBulk_NoQueuedNoQueuedFaces proves a plan with no OPEN queued crew
// carries no queued avatars — HasQueuedFaces is false and QueuedFacesHidden never
// goes negative.
func TestPlanCrewBulk_NoQueuedNoQueuedFaces(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-workC", "gardenio-working-only", "gd-step1", "working", "in_progress")
	s := crewTestServer(f)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.HasQueuedFaces() || len(roll.QueuedFaces) != 0 {
		t.Errorf("a plan with no queued crew must not surface a queued avatar: HasQueuedFaces=%v QueuedFaces=%d", roll.HasQueuedFaces(), len(roll.QueuedFaces))
	}
	if roll.QueuedFacesHidden() != 0 {
		t.Errorf("QueuedFacesHidden = %d, want 0 (no open queued crew)", roll.QueuedFacesHidden())
	}
}

// TestPlanCrewRollup_ReconcileTerminalClearsQueuedFaces proves a TERMINAL plan
// folds Idle→Done AND drops the queued faces — a finished plan cannot have a crew
// still queued to start (the crew-faces twin of reconcileTerminal's zombie-working
// fold).
func TestPlanCrewRollup_ReconcileTerminalClearsQueuedFaces(t *testing.T) {
	r := planCrewRollup{
		Total:       2,
		Idle:        1,
		Done:        1,
		QueuedFaces: []planCrewFace{{ID: "gd-que1", Name: "gardenio-queued-one"}},
	}
	got := r.reconcileTerminal("closed")
	if got.Idle != 0 {
		t.Errorf("terminal Idle = %d, want 0 (folded into Done)", got.Idle)
	}
	if got.Done != 2 {
		t.Errorf("terminal Done = %d, want 2 (1 done + 1 folded queued)", got.Done)
	}
	if len(got.QueuedFaces) != 0 || got.HasQueuedFaces() {
		t.Errorf("terminal plan must drop queued faces: QueuedFaces=%d HasQueuedFaces=%v", len(got.QueuedFaces), got.HasQueuedFaces())
	}
	// A non-terminal plan is untouched (idempotent floor).
	nonTerm := r.reconcileTerminal("in_progress")
	if len(nonTerm.QueuedFaces) != 1 {
		t.Errorf("a non-terminal plan must keep its queued faces; got %d", len(nonTerm.QueuedFaces))
	}
}

// --- render regressions ------------------------------------------------------

// TestPlansIndex_QueuedCrewRendersQueuedFaces proves the index card renders the
// calm queued-face strip (its own container class) beside the working crew's
// green face, and does NOT tint it with the red stuck-face class.
func TestPlansIndex_QueuedCrewRendersQueuedFaces(t *testing.T) {
	f := seedIdlePlanCrew("in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "plan-card-crew-faces-queued") {
		t.Errorf("an active plan with a queued crew must render the queued-faces strip; card:\n%s", card)
	}
	if !strings.Contains(card, "plan-card-crew-face-queued") {
		t.Errorf("the queued crew must render a queued-face avatar; card:\n%s", card)
	}
	// The queued face must NOT reuse the red stuck class (it reads calm, not alarm).
	if strings.Contains(card, "plan-card-crew-face-stuck") {
		t.Errorf("a queued crew must not render the loud red stuck-face class; card:\n%s", card)
	}
	// The working crew's green face still renders alongside (anti-over-reach).
	if !strings.Contains(card, "plan-card-crew-faces\"") && !strings.Contains(card, `class="plan-card-crew-faces"`) {
		t.Errorf("the working crew's green face strip must still render alongside the queued strip; card:\n%s", card)
	}
}

// TestPlansIndex_NoQueuedCrewNoQueuedFaces proves a plan with no queued crew
// renders no queued-face strip — add signal, not ink.
func TestPlansIndex_NoQueuedCrewNoQueuedFaces(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-working", "gardenio-prefix-cache-www", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if strings.Contains(card, "plan-card-crew-faces-queued") {
		t.Errorf("a plan with no queued crew must not render the queued-faces strip")
	}
}

// TestPlansIndex_TerminalPlanNoQueuedFaces proves a TERMINAL plan folds idle→done
// so its card renders no queued-face strip even with a leftover queued crew bead
// (the crew-faces twin of iter-30's zombie fold + iter-53's reconcileTerminal drop).
func TestPlansIndex_TerminalPlanNoQueuedFaces(t *testing.T) {
	f := seedIdlePlanCrew("closed")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if strings.Contains(card, "plan-card-crew-faces-queued") {
		t.Errorf("a closed plan must fold idle→done and render no queued-faces strip; card:\n%s", card)
	}
}
