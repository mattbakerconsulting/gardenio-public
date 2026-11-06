package main

// plans_transcript_durability_test.go — task 4c (gd-y2kf6hNrNa), Phase 4 of the
// from-scratch-plan-authoring + persistent-in-viewer-chat plan (ka-0000005794).
//
// This is the DOGFOOD/VALIDATION guard for decision D1=hybrid: it confirms the
// rendered transcript RECONSTRUCTS after a cold respawn — the ka-0000002360
// failure class ("Wake cold-starts and loses the prior conversation") — through
// the PRODUCTION read path, not the pure builder.
//
// Why a new test when the tree already covers transcripts:
//   - plan_transcript_spike_test.go (D1 spike) proves the pure builder survives a
//     respawn, but it models "respawn" by hand-feeding buildPlanTranscript an
//     empty vs. full slice. It never exercises planTranscriptView, the daemon
//     read, or the assistant-bead re-mint that IS the ka-0000002360 root cause.
//   - plan_transcript_render_test.go (2b) renders seeded turns ONCE; no respawn.
//   - test-plans-wake-button.sh (e2e) proves the wake MECHANICS (revive-in-place,
//     stable bead id) but asserts nothing about the transcript surviving.
//
// The gap this closes: an integration-level assertion that walks the real
// lifecycle — assistant live → park (terminal agent_state) → COLD RESPAWN (the
// coop S3 session is gone AND, in the pre-fix re-mint shape, the assistant bead
// itself is a NEW id) → wake — and proves GET /plans/{id}/agent still paints the
// full prior conversation. The load-bearing D1 invariant is that the transcript
// is keyed on the PLAN EPIC (fields.plan_id), never on the assistant agent bead
// id or the coop session, so neither losing the session nor re-minting the bead
// can lose the conversation.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/oidcauth"
)

// seedDurableConversation seeds a two-turn operator↔assistant conversation as
// durable fields.plan_id-bound message beads (the exact shape task 2a mints) and
// returns the turn texts in chat order. These beads are the SOURCE OF TRUTH — by
// D1=hybrid they outlive the coop session and any assistant-bead re-mint.
func seedDurableConversation(f *fakeDaemon, epicID string, base time.Time) (opText, asstText string) {
	opText = "Draft me a plan to add a New-plan button to /plans."
	asstText = "On it — researching the existing /plans routes now."
	seedTranscriptTurn(f, "gd-turn-op", epicID, "matt", planTurnRoleOperator, opText, base)
	seedTranscriptTurn(f, "gd-turn-asst", epicID, "gardenio-plan-asst-old", planTurnRoleAssistant,
		asstText, base.Add(time.Minute))
	return opText, asstText
}

// getPlanAgentLane drives the production GET /plans/{id}/agent lane fragment
// (the standalone assistant-lane render that carries the transcript) as an
// operator and returns the response body. Fails the test on a non-200.
func getPlanAgentLane(t *testing.T, mux http.Handler, epicID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/plans/"+epicID+"/agent", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /plans/%s/agent: expected 200, got %d; body=%s", epicID, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestTranscriptSurvivesColdRespawnAndBeadRemint is the headline 4c guard. It
// walks the full ka-0000002360 lifecycle against the PRODUCTION read path:
//
//  1. LIVE — an assistant bead is bound to the plan and both turns are persisted
//     as durable message beads. GET /plans/{id}/agent paints the conversation.
//  2. PARK — the assistant goes terminal (agent_state=done). Nothing about the
//     transcript beads changes.
//  3. COLD RESPAWN — the coop S3 session is gone (modelled: it was never a
//     transcript input) AND the assistant is re-minted under a BRAND-NEW bead id
//     (the exact ka-0000002360 root cause: a new id defeats coop's S3-resume
//     key). The OLD assistant bead is removed; a NEW one takes its place.
//  4. WAKE — GET /plans/{id}/agent must STILL paint the full prior conversation,
//     because the transcript is keyed on fields.plan_id (the plan epic), not on
//     the assistant bead id or the coop session.
//
// Regression: before D1=hybrid the conversation lived only in the coop session
// keyed by the assistant bead id, so a re-mint cold-started an empty assistant.
func TestTranscriptSurvivesColdRespawnAndBeadRemint(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	base := planAssistantTestNow.Add(-30 * time.Minute)
	opText, asstText := seedDurableConversation(f, "gd-plan1", base)

	// (1) LIVE: a working assistant bound to the plan; both turns already
	// persisted. The lane paints the conversation with the compose box.
	seedAssistantBead(f, "gd-asst-live", "gardenio-plan-asst-old", "gd-plan1", "working", planAssistantTestNow)
	mux := plansAssistantMux(t, f, nil)

	live := getPlanAgentLane(t, mux, "gd-plan1")
	for _, want := range []string{`class="bead-chat"`, opText, asstText} {
		if !strings.Contains(live, want) {
			t.Fatalf("live lane missing %q; body=%s", want, live)
		}
	}

	// (2) PARK: the assistant goes terminal. This does not touch the durable
	// transcript beads. (agent_state=done → assistantIsLive is false → the lane
	// would render the wake/spawn composer, not the live one.)
	f.beads["gd-asst-live"].Fields["agent_state"] = "done"
	f.beads["gd-asst-live"].Status = "closed"

	parked := getPlanAgentLane(t, mux, "gd-plan1")
	if !strings.Contains(parked, opText) || !strings.Contains(parked, asstText) {
		t.Fatalf("parked lane lost the transcript; body=%s", parked)
	}

	// (3) COLD RESPAWN — the destructive step. The coop S3 session is gone (it
	// was never a transcript input under D1=hybrid, so nothing to remove) AND the
	// assistant bead is re-minted under a NEW id. Remove the old bead + its
	// assigned edge and mint a fresh assistant, exactly as the pre-fix wake path
	// (CreateBead) did — the NEW id is the ka-0000002360 root cause.
	delete(f.beads, "gd-asst-live")
	f.revDeps["gd-plan1"] = nil
	seedAssistantBead(f, "gd-asst-NEW", "gardenio-plan-asst-old", "gd-plan1", "spawning", planAssistantTestNow)

	// The re-minted assistant is a genuinely different bead id — pin that the
	// scenario actually reproduces the root cause (a stable-id fix would make
	// this test trivially pass without proving anything).
	if _, oldGone := f.beads["gd-asst-live"]; oldGone {
		t.Fatal("precondition: the old assistant bead should be gone after a re-mint respawn")
	}
	revived := planAssistantTestServer(f).planAssistant(context.Background(), "gd-plan1")
	if revived == nil || revived.ID != "gd-asst-NEW" {
		t.Fatalf("precondition: respawn should bind a NEW assistant bead id; got %+v", revived)
	}

	// (4) WAKE: GET /plans/{id}/agent must reconstruct the FULL prior
	// conversation from the durable fields.plan_id-bound beads — the coop session
	// is gone and the assistant bead id changed, yet neither turn is lost.
	woke := getPlanAgentLane(t, mux, "gd-plan1")
	for _, want := range []string{`class="bead-chat"`, opText, asstText} {
		if !strings.Contains(woke, want) {
			t.Errorf("D1 REGRESSION (ka-0000002360): woken lane lost %q after a cold "+
				"respawn + bead re-mint — the transcript must reconstruct from "+
				"fields.plan_id, independent of the coop session and the assistant "+
				"bead id; body=%s", want, woke)
		}
	}
	// Ordering must survive too: the operator turn precedes the assistant reply.
	if op, as := strings.Index(woke, opText), strings.Index(woke, asstText); op < 0 || as < 0 || op > as {
		t.Errorf("woken transcript lost chat order (op=%d as=%d)", op, as)
	}
}

// TestPlanTranscriptView_KeyedOnPlanNotAssistant is the tight unit-level
// companion: it proves planTranscriptView (the production daemon read) collects
// turns by the PLAN epic id, so turns minted under DIFFERENT assistant authors
// across a respawn all reconstruct into one plan transcript. This isolates the
// D1 invariant — plan-keyed, not assistant-keyed — from the full HTTP lane.
func TestPlanTranscriptView_KeyedOnPlanNotAssistant(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	base := planAssistantTestNow.Add(-time.Hour)

	// Turn from the FIRST assistant incarnation (pre-respawn).
	seedTranscriptTurn(f, "gd-t-pre", "gd-plan1", "gardenio-plan-asst-gen1", planTurnRoleAssistant,
		"reply from the first assistant pod", base)
	// Operator turn in between.
	seedTranscriptTurn(f, "gd-t-op", "gd-plan1", "matt", planTurnRoleOperator,
		"a question spanning the respawn", base.Add(time.Minute))
	// Turn from the SECOND assistant incarnation (post-respawn, different author
	// handle + different agent bead id — the re-mint). Same plan_id binding.
	seedTranscriptTurn(f, "gd-t-post", "gd-plan1", "gardenio-plan-asst-gen2", planTurnRoleAssistant,
		"reply from the respawned assistant pod", base.Add(2*time.Minute))

	// A turn bound to a DIFFERENT plan must never leak into this plan's view.
	seedTranscriptTurn(f, "gd-t-other", "gd-OTHERPLAN", "gardenio-plan-asst-gen2", planTurnRoleAssistant,
		"a turn for a different plan", base.Add(3*time.Minute))

	s := planAssistantTestServer(f)
	view := s.planTranscriptView(context.Background(), "gd-plan1")

	if !view.HasMessages || view.Shown != 3 {
		t.Fatalf("planTranscriptView Shown=%d HasMessages=%v, want 3 turns reconstructed "+
			"across the respawn (both assistant incarnations + the operator turn)", view.Shown, view.HasMessages)
	}
	// All three plan turns present, in chat order; the other-plan turn absent.
	wantOrder := []string{"gd-t-pre", "gd-t-op", "gd-t-post"}
	for i, id := range wantOrder {
		if view.Messages[i].ID != id {
			t.Errorf("Messages[%d].ID = %q, want %q (full: %v)", i, view.Messages[i].ID, id, msgIDs(view))
		}
	}
	for _, m := range view.Messages {
		if m.ID == "gd-t-other" {
			t.Errorf("a turn bound to a different plan leaked into the transcript")
		}
	}
}

// TestPlanTranscriptView_EmptyWhenOnlyCoopSessionHeld is the negative control
// for D1: it models the REJECTED view-over-coop option (D1 option (b), the
// ka-0000002360 failure class). If turns are NOT persisted as fields.plan_id
// beads — i.e. they lived only in the coop session, which a cold respawn drops —
// planTranscriptView reconstructs NOTHING. This pins that durability comes from
// the bead persistence, not the read path: the read is only as durable as what
// was written, and D1=hybrid is what guarantees the write.
func TestPlanTranscriptView_EmptyWhenOnlyCoopSessionHeld(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	// A bound assistant exists (as after a respawn), but NO transcript-turn beads
	// were ever minted — the conversation lived only in the now-gone coop session.
	seedAssistantBead(f, "gd-asst", "gardenio-plan-asst", "gd-plan1", "working", planAssistantTestNow)

	s := planAssistantTestServer(f)
	view := s.planTranscriptView(context.Background(), "gd-plan1")
	if view.HasMessages {
		t.Errorf("with no durable turn beads (view-over-coop, D1 option b), the transcript "+
			"must reconstruct EMPTY after respawn — got Shown=%d", view.Shown)
	}
}
