package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plans_transcript_test.go — task 2c (ka-0000000311). 2c streams the durable
// plan-chat transcript live and adds the "assistant is typing…" indicator. The
// transcript RENDER lives in the #plan-assistant lane (2b, ka-0000004607); 2c
// owns (a) the missing SSE trigger — a new type=message turn bead with
// fields.plan_id must re-render that lane, which it did not before 2c — and
// (b) the typing bubble off the bound assistant's agent_state.

// seedTranscriptTurn is provided by plan_transcript_render_test.go (2b):
//   seedTranscriptTurn(f, id, epicID, author, role, text, created)

// TestPlanTranscriptTyping pins the typing predicate to the alive agent_state
// bucket (working / spawning / yielding), independent of freshness — a
// still-thawing spawn must read as "typing", not "stuck" (§10-R1).
func TestPlanTranscriptTyping(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"nil assistant", nil, false},
		{"nil fields", &beadsapi.BeadDetail{}, false},
		{"empty state", &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": ""}}, false},
		{"working", &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": "working"}}, true},
		{"spawning", &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": "spawning"}}, true},
		{"yielding", &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": "yielding"}}, true},
		{"idle", &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": "idle"}}, false},
		{"done", &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": "done"}}, false},
		// Stale but alive: a spawn stamped long ago is still "coming online" — the
		// typing bubble must show through the cold-thaw window, so no freshness gate.
		{"stale-but-working", &beadsapi.BeadDetail{
			Fields:    map[string]string{"agent_state": "working"},
			UpdatedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planTranscriptTyping(tc.bead); got != tc.want {
				t.Errorf("planTranscriptTyping = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEventFieldEquals guards the SSE-payload field extractor: it must match the
// wanted key, ignore siblings, and — critically — NOT choke on a fields blob
// whose OTHER keys hold non-string values (arrays/numbers), which is exactly
// what would break the shared writePlanEvent decode if the whole blob were typed.
func TestEventFieldEquals(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		key  string
		want string
		ok   bool
	}{
		{"match", `{"plan_id":"gd-plan1"}`, "plan_id", "gd-plan1", true},
		{"mismatch value", `{"plan_id":"gd-plan2"}`, "plan_id", "gd-plan1", false},
		{"missing key", `{"turn_role":"operator"}`, "plan_id", "gd-plan1", false},
		{"empty blob", ``, "plan_id", "gd-plan1", false},
		{"empty key", `{"plan_id":"gd-plan1"}`, "", "gd-plan1", false},
		// A sibling array/number must NOT fail the whole decode — only the ONE key
		// is coerced. This is the load-bearing robustness (phase_order is an array).
		{"non-string sibling array", `{"plan_id":"gd-plan1","phase_order":["a","b"]}`, "plan_id", "gd-plan1", true},
		{"non-string sibling number", `{"plan_id":"gd-plan1","n":3}`, "plan_id", "gd-plan1", true},
		// The wanted key itself being non-string → no match (coerce fails), not a panic.
		{"target key is array", `{"plan_id":["gd-plan1"]}`, "plan_id", "gd-plan1", false},
		{"malformed json", `{not json`, "plan_id", "gd-plan1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventFieldEquals([]byte(tc.raw), tc.key, tc.want); got != tc.ok {
				t.Errorf("eventFieldEquals(%q,%q,%q) = %v, want %v", tc.raw, tc.key, tc.want, got, tc.ok)
			}
		})
	}
}

// TestPlanAssistantLaneData_TypingFromAssistantState pins that the lane bag
// carries Typing derived from the bound assistant's agent_state.
func TestPlanAssistantLaneData_TypingFromAssistantState(t *testing.T) {
	t.Run("working assistant → typing", func(t *testing.T) {
		f := seedPlansFake()
		seedAssistantBead(f, "gd-asst", "gardenio-plan-asst", "gd-plan1", "working", planAssistantTestNow)
		s := planAssistantTestServer(f)
		data := s.planAssistantLaneData(context.Background(), "gd-plan1", false, "", planDeliveryNone)
		if data["Typing"] != true {
			t.Errorf("Typing = %v, want true (assistant working)", data["Typing"])
		}
	})
	t.Run("no assistant → not typing", func(t *testing.T) {
		f := seedPlansFake()
		s := planAssistantTestServer(f)
		data := s.planAssistantLaneData(context.Background(), "gd-plan1", false, "", planDeliveryNone)
		if data["Typing"] != false {
			t.Errorf("Typing = %v, want false (no assistant)", data["Typing"])
		}
	})
}

// TestPlanAssistantLane_RendersTypingBubble exercises the lane fragment: a
// working assistant renders the "assistant is typing…" bubble; a parked one
// does not.
func TestPlanAssistantLane_RendersTypingBubble(t *testing.T) {
	t.Run("typing", func(t *testing.T) {
		f := seedPlansFake()
		seedAssistantBead(f, "gd-asst", "gardenio-plan-asst", "gd-plan1", "working", planAssistantTestNow)
		mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })
		req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		for _, want := range []string{"plan agent is typing", "plan-assistant-typing"} {
			if !strings.Contains(body, want) {
				t.Errorf("live assistant lane missing typing bubble %q; body=%s", want, body)
			}
		}
	})
	t.Run("parked → no bubble", func(t *testing.T) {
		f := seedPlansFake()
		seedAssistantBead(f, "gd-asstp", "gardenio-plan-asst-p", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
		mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })
		req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if strings.Contains(w.Body.String(), "plan agent is typing") {
			t.Errorf("parked assistant must not show the typing bubble; body=%s", w.Body.String())
		}
	})
}

// TestHandlePlanEvents_TurnMintReRendersAssistantLane is the load-bearing 2c
// wiring test: a type=message turn bead bound to the plan by fields.plan_id
// (NOT a step/narrative interest member) must re-render the #plan-assistant lane
// (which carries the transcript) via the plan-agent-updated frame — before
// 2c it fell through the interest gate and never streamed.
func TestHandlePlanEvents_TurnMintReRendersAssistantLane(t *testing.T) {
	f := seedPlansFake()
	seedTranscriptTurn(f, "gd-turn1", "gd-plan1", "gardenio-plan-asst", planTurnRoleAssistant, "researched the siblings",
		time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC))
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// A newly-minted assistant turn bead for THIS plan — the daemon streams the
	// full bead incl. fields, so the payload carries fields.plan_id.
	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.created",
		Data:  []byte(`{"bead":{"id":"gd-turn1","type":"message","fields":{"plan_id":"gd-plan1","turn_role":"assistant"}}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-agent-updated") {
		t.Errorf("a turn mint should re-render the assistant lane; got: %s", got)
	}
	if !strings.Contains(got, `id="plan-agent"`) {
		t.Errorf("expected the assistant lane fragment in the SSE payload; got: %s", got)
	}
	// A turn must NOT flash the step timeline (routed + returned before the gate).
	if strings.Contains(got, "event: plan-updated") {
		t.Errorf("a transcript turn must not emit plan-updated; got: %s", got)
	}
}

// TestHandlePlanEvents_TurnRendersInLane confirms the streamed lane fragment
// actually contains the new turn's text (the transcript rides the frame).
func TestHandlePlanEvents_TurnRendersInLane(t *testing.T) {
	f := seedPlansFake()
	seedTranscriptTurn(f, "gd-turn1", "gd-plan1", "matt", planTurnRoleOperator, "draft the plan please",
		time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC))
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.created",
		Data:  []byte(`{"bead":{"id":"gd-turn1","type":"message","fields":{"plan_id":"gd-plan1","turn_role":"operator"}}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "draft the plan please") {
		t.Errorf("streamed lane should carry the new turn text; got: %s", got)
	}
}

// TestHandlePlanEvents_DropsTurnForOtherPlan guards the fields.plan_id scoping:
// a turn bead bound to a DIFFERENT plan must not stream on this plan's cockpit.
func TestHandlePlanEvents_DropsTurnForOtherPlan(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.created",
		Data:  []byte(`{"bead":{"id":"gd-turnX","type":"message","fields":{"plan_id":"gd-OTHER","turn_role":"operator"}}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if strings.Contains(got, "event: plan-agent-updated") {
		t.Errorf("a turn bound to another plan must not re-render this lane; got: %s", got)
	}
}

// TestHandlePlanEvents_AssistantStateFlipsTyping verifies the typing bubble
// flips live: an agent_state change on the bound assistant re-renders the lane
// (which recomputes Typing).
func TestHandlePlanEvents_AssistantStateFlipsTyping(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asst", "gardenio-plan-asst", "gd-plan1", "working", planAssistantTestNow)
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-asst","type":"agent"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-agent-updated") {
		t.Errorf("an assistant agent_state change should re-render the lane; got: %s", got)
	}
	// The working assistant → the re-rendered lane carries the typing bubble.
	if !strings.Contains(got, "plan agent is typing") {
		t.Errorf("re-rendered lane for a working assistant should carry the typing bubble; got: %s", got)
	}
}

// TestHandlePlanEvents_StepChangeDoesNotReRenderLane guards that a non-turn,
// non-assistant lifecycle event (a step close) does NOT re-render the assistant
// lane (only the step timeline).
func TestHandlePlanEvents_StepChangeDoesNotReRenderLane(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-step1","type":"task"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if strings.Contains(got, "event: plan-agent-updated") {
		t.Errorf("a step change must not re-render the assistant lane; got: %s", got)
	}
	if !strings.Contains(got, "event: plan-updated") {
		t.Errorf("a step change should still re-render the step timeline; got: %s", got)
	}
}

// TestHandlePlanDetail_EmbedsTranscriptAndTyping pins that the plan cockpit page
// embeds the transcript turn (via 2b's in-lane render) and the typing bubble on
// first paint.
func TestHandlePlanDetail_EmbedsTranscriptAndTyping(t *testing.T) {
	f := seedPlansFake()
	seedTranscriptTurn(f, "gd-turn1", "gd-plan1", "matt", planTurnRoleOperator, "kick it off",
		time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC))
	seedAssistantBead(f, "gd-asst", "gardenio-plan-asst", "gd-plan1", "working", planAssistantTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"kick it off", "plan agent is typing"} {
		if !strings.Contains(body, want) {
			t.Errorf("plan detail should embed transcript + typing; missing %q", want)
		}
	}
	if strings.Contains(body, "<no value>") {
		t.Errorf("lane embed leaked a <no value> (missing data-bag key); body=%s", body)
	}
}
