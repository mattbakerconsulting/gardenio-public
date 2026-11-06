package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPlanAgentStateFrom truth-tables the unified loop×agent derivation: the two
// facets are orthogonal, so every (loop, agent) pair must round-trip verbatim,
// and an empty facet must normalise to its "none" sentinel.
func TestPlanAgentStateFrom(t *testing.T) {
	cases := []struct {
		name      string
		loop      planGardenerState
		agent     planAssistantState
		wantLoop  planGardenerState
		wantAgent planAssistantState
	}{
		{"running-live", planGardenerStateRunning, planAssistantStateLive, planGardenerStateRunning, planAssistantStateLive},
		{"running-parked", planGardenerStateRunning, planAssistantStateParked, planGardenerStateRunning, planAssistantStateParked},
		{"paused-live", planGardenerStatePaused, planAssistantStateLive, planGardenerStatePaused, planAssistantStateLive},
		{"paused-parked", planGardenerStatePaused, planAssistantStateParked, planGardenerStatePaused, planAssistantStateParked},
		{"retired-none", planGardenerStateRetired, planAssistantStateNone, planGardenerStateRetired, planAssistantStateNone},
		{"none-waking", planGardenerStateNone, planAssistantStateWaking, planGardenerStateNone, planAssistantStateWaking},
		// Queued (woken spawn deferred on an exhausted pool, ka-0000003525) is a
		// first-class agent facet that must round-trip verbatim under any loop.
		{"running-queued", planGardenerStateRunning, planAssistantStateQueued, planGardenerStateRunning, planAssistantStateQueued},
		{"none-queued", planGardenerStateNone, planAssistantStateQueued, planGardenerStateNone, planAssistantStateQueued},
		// Empty facets normalise to their none sentinel — a zero-value view (the
		// composer's degrade path) must never leak an empty string into the chip.
		{"empty-loop", "", planAssistantStateLive, planGardenerStateNone, planAssistantStateLive},
		{"empty-agent", planGardenerStateRunning, "", planGardenerStateRunning, planAssistantStateNone},
		{"both-empty", "", "", planGardenerStateNone, planAssistantStateNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planAgentStateFrom(planGardenerView{State: tc.loop}, tc.agent)
			if got.Loop != tc.wantLoop {
				t.Errorf("Loop = %q, want %q", got.Loop, tc.wantLoop)
			}
			if got.Agent != tc.wantAgent {
				t.Errorf("Agent = %q, want %q", got.Agent, tc.wantAgent)
			}
		})
	}
}

// TestPlanAgentChip truth-tables the unified chip derivation (4 loop × 4 agent).
// The chip is loop-primary (tone follows the loop when bound; the agent liveness
// folds in as a "· live"/"· waking" sub-signal + a pulsing dot), and falls back
// to the agent liveness for a gardener-less plan — so a plan can never show two
// contradictory chips.
func TestPlanAgentChip(t *testing.T) {
	cases := []struct {
		loop      planGardenerState
		agent     planAssistantState
		wantLabel string
		wantTone  string
		wantDot   string
	}{
		// Loop bound: tone = loop; live/waking fold in as a sub-signal + dot.
		{planGardenerStateRunning, planAssistantStateLive, "running · live", "running", "live"},
		{planGardenerStateRunning, planAssistantStateWaking, "running · waking", "running", "waking"},
		// Running loop, NO active iteration → "idle" (Matt's ask, thread
		// ka-0000001290): the bare "running" word over-claims activity when
		// nothing is executing. Green "running" now means exactly "an agent is
		// live"; an armed-but-quiet loop reads calm "idle".
		{planGardenerStateRunning, planAssistantStateParked, "idle", "idle", ""},
		{planGardenerStateRunning, planAssistantStateNone, "idle", "idle", ""},
		// Queued folds in as a "· rate-limited" sub-signal + violet dot — unlike
		// parked, it adds info the loop word lacks (the loop is driving but the
		// wake is blocked on capacity), so it is NOT dropped (ka-0000003525).
		{planGardenerStateRunning, planAssistantStateQueued, "running · rate-limited", "running", "queued"},
		{planGardenerStatePaused, planAssistantStateQueued, "paused · rate-limited", "paused", "queued"},
		{planGardenerStatePaused, planAssistantStateLive, "paused · live", "paused", "live"},
		{planGardenerStatePaused, planAssistantStateParked, "paused", "paused", ""},
		{planGardenerStatePaused, planAssistantStateNone, "paused", "paused", ""},
		{planGardenerStateRetired, planAssistantStateLive, "retired · live", "retired", "live"},
		{planGardenerStateRetired, planAssistantStateNone, "retired", "retired", ""},
		// Gardener-less plan: the chip reflects the assistant alone; no dot.
		{planGardenerStateNone, planAssistantStateLive, "live", "live", ""},
		{planGardenerStateNone, planAssistantStateWaking, "waking", "waking", ""},
		{planGardenerStateNone, planAssistantStateQueued, "rate-limited", "queued", ""},
		{planGardenerStateNone, planAssistantStateParked, "parked", "parked", ""},
		{planGardenerStateNone, planAssistantStateNone, "not started", "none", ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.loop)+"-"+string(tc.agent), func(t *testing.T) {
			v := planAgentChip(planAgentState{Loop: tc.loop, Agent: tc.agent})
			if v.Label != tc.wantLabel {
				t.Errorf("Label = %q, want %q", v.Label, tc.wantLabel)
			}
			if v.Tone != tc.wantTone {
				t.Errorf("Tone = %q, want %q", v.Tone, tc.wantTone)
			}
			if v.Dot != tc.wantDot {
				t.Errorf("Dot = %q, want %q", v.Dot, tc.wantDot)
			}
			if v.Title == "" {
				t.Errorf("Title should never be empty (loop=%q agent=%q)", tc.loop, tc.agent)
			}
		})
	}
}

// TestPlanChipAgentState_Queued: a woken assistant whose spawn was declined on
// an exhausted account pool (agent_state=queued) resolves to the distinct
// queued state through BOTH the gardener-bar chip resolver (planChipAgentState)
// and the lane bag (planAssistantLaneData) — NOT collapsed to parked
// (ka-0000003525). Uses a stale timestamp to prove queued is checked before the
// parked fallthrough (queued is not freshness-gated).
func TestPlanChipAgentState_Queued(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstq", "gardenio-plan-asst-q", "gd-plan1", "queued", planGardenerTestNow.Add(-2*time.Hour))
	s := mkPlanGardenerServer(f)

	if got := s.planChipAgentState(context.Background(), "gd-plan1"); got != planAssistantStateQueued {
		t.Errorf("planChipAgentState = %q, want %q", got, planAssistantStateQueued)
	}

	lane := s.planAssistantLaneData(context.Background(), "gd-plan1", true, "", planDeliveryNone)
	if lane["State"] != string(planAssistantStateQueued) {
		t.Errorf("lane State = %v, want %q", lane["State"], planAssistantStateQueued)
	}
}

// TestPlanAgentPanelData_CarriesChip: the merged page-load bag carries the
// unified chip derived off the FULL panel state (both halves).
func TestPlanAgentPanelData_CarriesChip(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "") // paused loop
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planGardenerTestNow)
	s := mkPlanGardenerServer(f)

	panel := s.planAgentPanelData(context.Background(), "gd-plan1", true, "", planDeliveryNone, "")
	chip, ok := panel["Chip"].(planAgentChipView)
	if !ok {
		t.Fatalf("Chip missing or wrong type: %T", panel["Chip"])
	}
	// Paused loop + live iteration → "paused · live" (the exact contradiction the
	// old two-chip surface would have shown as "paused" + "live" side by side).
	if chip.Label != "paused · live" {
		t.Errorf("chip Label = %q, want \"paused · live\"", chip.Label)
	}
	if chip.Tone != "paused" || chip.Dot != "live" {
		t.Errorf("chip Tone/Dot = %q/%q, want paused/live", chip.Tone, chip.Dot)
	}
}

// TestPlanGardenerBarData_CarriesChip: the gardener bar's OWN data (the standalone
// fragment + POST + SSE re-render path) also carries the unified chip, resolving
// the agent liveness so a gardener-only re-render stays honest ("paused · live").
func TestPlanGardenerBarData_CarriesChip(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planGardenerTestNow)
	s := mkPlanGardenerServer(f)

	data := s.planGardenerBarData(context.Background(), "gd-plan1", true, "")
	chip, ok := data["Chip"].(planAgentChipView)
	if !ok {
		t.Fatalf("gardener bar Chip missing or wrong type: %T", data["Chip"])
	}
	if chip.Label != "paused · live" {
		t.Errorf("gardener-bar chip Label = %q, want \"paused · live\"", chip.Label)
	}
}

// TestPlanAgentPanelData_MergesBothHalves: the one merged bag carries BOTH the
// gardener view (loop-state half) AND the assistant lane's flat keys (agent-state
// half), plus the derived unified PanelState — so a plan with a running gardener
// and a live assistant yields ONE coherent structure, not two.
func TestPlanAgentPanelData_MergesBothHalves(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	// Assistant fresh at the gardener test clock so assistantIsLive → live.
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planGardenerTestNow)
	s := mkPlanGardenerServer(f)

	panel := s.planAgentPanelData(context.Background(), "gd-plan1", true, "", planDeliveryNone, "")

	// Loop-state half: the gardener view is folded in under "Gardener".
	cv, ok := panel["Gardener"].(planGardenerView)
	if !ok {
		t.Fatalf("Gardener key missing or wrong type: %T", panel["Gardener"])
	}
	if cv.State != planGardenerStateRunning {
		t.Errorf("gardener State = %q, want running", cv.State)
	}
	if cv.GardenerID != "gd-cap1" {
		t.Errorf("gardener GardenerID = %q, want gd-cap1", cv.GardenerID)
	}

	// Agent-state half: the flat lane keys survive verbatim onto the merged bag.
	if panel["State"] != string(planAssistantStateLive) {
		t.Errorf("agent State = %v, want live", panel["State"])
	}
	for _, k := range []string{"PlanID", "Assistant", "Transcript", "Activity", "MaxLen", "BasePath"} {
		if _, present := panel[k]; !present {
			t.Errorf("merged bag missing assistant-lane key %q", k)
		}
	}

	// Derived unified state: both facets present and consistent with the halves.
	ps, ok := panel["PanelState"].(planAgentState)
	if !ok {
		t.Fatalf("PanelState missing or wrong type: %T", panel["PanelState"])
	}
	if ps.Loop != planGardenerStateRunning || ps.Agent != planAssistantStateLive {
		t.Errorf("PanelState = %+v, want {running live}", ps)
	}
}

// TestPlanAgentPanelData_UnboundPlan: a plan with neither gardener nor assistant
// still yields a well-formed bag — the gardener view is the "none" state and the
// unified state normalises to none/none, so the panel renders the not-started
// prompts rather than erroring.
func TestPlanAgentPanelData_UnboundPlan(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	s := mkPlanGardenerServer(f)

	panel := s.planAgentPanelData(context.Background(), "gd-plan1", true, "", planDeliveryNone, "")

	cv := panel["Gardener"].(planGardenerView)
	if cv.State != planGardenerStateNone {
		t.Errorf("gardener State = %q, want none", cv.State)
	}
	if cv.GardenerID != "" {
		t.Errorf("gardener GardenerID = %q, want empty", cv.GardenerID)
	}
	if panel["State"] != string(planAssistantStateNone) {
		t.Errorf("agent State = %v, want none", panel["State"])
	}
	ps := panel["PanelState"].(planAgentState)
	if ps.Loop != planGardenerStateNone || ps.Agent != planAssistantStateNone {
		t.Errorf("PanelState = %+v, want {none none}", ps)
	}
}

// TestPlanAgentPanelData_GardenerErrorPropagates: the gardenerErr argument reaches
// the folded gardener view's Error field (the compose path used by a POST
// re-render that surfaces an inline gardener-bar alert on the unified panel).
func TestPlanAgentPanelData_GardenerErrorPropagates(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	s := mkPlanGardenerServer(f)

	panel := s.planAgentPanelData(context.Background(), "gd-plan1", true, "", planDeliveryNone, "boom")
	cv := panel["Gardener"].(planGardenerView)
	if cv.Error != "boom" {
		t.Errorf("gardener Error = %q, want boom", cv.Error)
	}
}

// TestPlanDetail_RendersUnifiedPanel: the full plan cockpit renders BOTH halves
// off the ONE merged bag — the gardener levers and the compose box both appear
// inside the single #plan-agent card (step 6, ka-0000000116), proving the
// template's single .Agent pass-through feeds the merged partial.
func TestPlanDetail_RendersUnifiedPanel(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planGardenerTestNow)
	mux := planGardenerMux(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-agent"`,                  // the ONE merged card
		"/plans/gd-plan1/gardener/wake",    // gardener lever
		"/plans/gd-plan1/assistant/prompt", // compose box
		// The ONE unified plan-agent chip (step 5, ka-0000001332): a running
		// gardener + a live iteration reads "running · live" in a single chip —
		// tone follows the loop, a pulsing dot conveys the agent liveness.
		`plan-agent-chip-running`,
		`plan-agent-chip-dot-live`,
		"running · live",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unified panel missing %q", want)
		}
	}
	// Never two contradictory chips: the old per-box chips are gone.
	if strings.Contains(body, "plan-gardener-chip") || strings.Contains(body, "plan-assistant-chip") {
		t.Errorf("unified panel must not render the old per-box chips; body=%s", body)
	}
}
