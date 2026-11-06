package main

// plan_transcript_render_test.go — task 2b (ka-0000004607), Phase 2 of the
// from-scratch-plan-authoring + persistent-in-viewer-chat plan (ka-0000002918).
//
// The READ+RENDER side of the plan-chat transcript: proves the assistant lane
// now feeds the durable turn beads task 2a (ka-0000004125) mints through the
// SHARED bead_chat direction-bubble renderer, on the page load, the standalone
// assistant fragment, AND the SSE re-render. The pure projector is already
// covered by plan_transcript_spike_test.go (buildPlanTranscript); these tests
// exercise the daemon-read + template-embed wiring this task added.

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// seedTranscriptTurn seeds one durable transcript-turn bead bound to the plan by
// fields.plan_id — the exact shape planTranscriptTurnBead mints (task 2a) and
// planTranscriptView reads (task 2b). role is planTurnRoleOperator/Assistant.
func seedTranscriptTurn(f *fakeDaemon, id, epicID, author, role, text string, created time.Time) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     id,
		Title:  text,
		Type:   "message",
		Kind:   "data",
		Status: "closed",
		Labels: []string{"plan-transcript", "from:" + author},
		Fields: map[string]string{
			planTranscriptPlanIDField:   epicID,
			planTranscriptTurnRoleField: role,
			"source_agent":              author,
			"text":                      text,
		},
		CreatedBy: author,
		CreatedAt: created,
	}
	f.beads[id] = b
	return b
}

// TestPlanAssistantView_RendersTranscript is the headline test: a plan with
// persisted operator + assistant turns renders the direction-bubble chat log in
// the assistant lane, with operator turns on the own-side (dir-from-user) and
// assistant turns on the other-side (dir-from-agent).
func TestPlanAssistantView_RendersTranscript(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	base := planAssistantTestNow.Add(-10 * time.Minute)
	seedTranscriptTurn(f, "gd-turn0", "gd-plan1", "matt", planTurnRoleOperator,
		"Draft me a plan to add a New-plan button.", base)
	seedTranscriptTurn(f, "gd-turn1", "gd-plan1", "gardenio-plan-asst-1", planTurnRoleAssistant,
		"On it — researching the existing /plans routes now.", base.Add(time.Minute))
	mux := plansAssistantMux(t, f, nil)

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="bead-chat"`,            // the shared chat renderer painted
		"Draft me a plan to add a New", // the operator turn text
		"researching the existing",     // the assistant turn text
		"dir-from-user",                // operator bubble side
		"dir-from-agent",               // assistant bubble side
	} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript render missing %q; body=%s", want, body)
		}
	}
	// The operator turn must precede the assistant turn (oldest→newest chat-log
	// order); assert the byte offsets to pin ordering through the real template.
	if op, as := strings.Index(body, "Draft me a plan"), strings.Index(body, "researching the existing"); op < 0 || as < 0 || op > as {
		t.Errorf("expected operator turn before assistant turn in the log (op=%d as=%d)", op, as)
	}
}

// seedToolTurn seeds one durable TOOL-CALL transcript-turn bead (Phase A,
// ka-0000004601) — turn_role=tool plus the structured tool fields the render side
// (decorateToolTurn / bead_chat) reads.
func seedToolTurn(f *fakeDaemon, id, epicID, author, toolName, args, result string, created time.Time) *beadsapi.BeadDetail {
	fields := map[string]string{
		planTranscriptPlanIDField:   epicID,
		planTranscriptTurnRoleField: planTurnRoleTool,
		planTranscriptToolNameField: toolName,
		"source_agent":              author,
		"text":                      toolName + ": " + args,
	}
	if args != "" {
		fields[planTranscriptToolArgsField] = args
	}
	if result != "" {
		fields[planTranscriptToolResultField] = result
	}
	b := &beadsapi.BeadDetail{
		ID:        id,
		Title:     toolName + ": " + args,
		Type:      "message",
		Kind:      "data",
		Status:    "closed",
		Labels:    []string{"plan-transcript", "from:" + author},
		Fields:    fields,
		CreatedBy: author,
		CreatedAt: created,
	}
	f.beads[id] = b
	return b
}

// TestPlanAssistantView_RendersToolTurn is the render-side headline for Phase A:
// a persisted tool-call turn paints a distinct, collapsible tool entry inline in
// the transcript (tool name + args + result), through the real template — not a
// plain prose bubble.
func TestPlanAssistantView_RendersToolTurn(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	base := planAssistantTestNow.Add(-10 * time.Minute)
	seedTranscriptTurn(f, "gd-turn0", "gd-plan1", "matt", planTurnRoleOperator,
		"run the tests please", base)
	seedToolTurn(f, "gd-tool0", "gd-plan1", "gardenio-plan-asst-1", "Bash",
		"go test ./...", "ok — 42 passed", base.Add(time.Minute))
	seedTranscriptTurn(f, "gd-turn1", "gd-plan1", "gardenio-plan-asst-1", planTurnRoleAssistant,
		"All green.", base.Add(2*time.Minute))
	mux := plansAssistantMux(t, f, nil)

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"bead-chat-msg is-tool", // the distinct tool-entry container
		"bead-chat-tool-name",   // the tool-name badge
		"Bash",                  // the tool name
		"go test ./...",         // the args summary
		"ok — 42 passed",        // the result summary (in the expandable pane)
		"bead-chat-tool-result", // the result pane class
		"run the tests please",  // the operator prose turn still renders
		"All green.",            // the assistant prose turn still renders
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tool-turn render missing %q; body=%s", want, body)
		}
	}
	// Ordering: operator prose → tool entry → assistant prose.
	op := strings.Index(body, "run the tests please")
	tool := strings.Index(body, "go test ./...")
	as := strings.Index(body, "All green.")
	if op < 0 || tool < 0 || as < 0 || !(op < tool && tool < as) {
		t.Errorf("expected op<tool<assistant order (op=%d tool=%d as=%d)", op, tool, as)
	}
}

// TestPlanAssistantView_NoTranscriptWhenEmpty proves a plan with no persisted
// turns paints NO chat panel — the bead_chat partial self-gates on
// HasMessages, so a brand-new plan shows only the compose box, not an empty
// chat frame.
func TestPlanAssistantView_NoTranscriptWhenEmpty(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	mux := plansAssistantMux(t, f, nil)

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="bead-chat"`) {
		t.Errorf("empty transcript should not paint a chat panel; body=%s", body)
	}
	// The lane still renders its compose box.
	if !strings.Contains(body, `hx-post="/plans/gd-plan1/assistant/prompt"`) {
		t.Errorf("lane should still render the compose form; body=%s", body)
	}
}

// TestPlanAssistantView_TranscriptForNonOperator proves the transcript is
// read-only and renders for a NON-operator viewer too (it's a conversation
// record, not a mutation) — the compose form stays operator-gated, but the log
// is visible to anyone who can see the plan.
func TestPlanAssistantView_TranscriptForNonOperator(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedTranscriptTurn(f, "gd-turn0", "gd-plan1", "matt", planTurnRoleOperator,
		"a visible operator turn", planAssistantTestNow.Add(-time.Minute))
	mux := plansAssistantMux(t, f, nil)

	// An authenticated NON-operator (viewer role): sees the plan but cannot
	// mutate. (An empty role set is the dev/unauthenticated fallback that
	// viewerCanOperate treats as operator, so pin an explicit viewer role.)
	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "a visible operator turn") {
		t.Errorf("transcript should render for a non-operator viewer; body=%s", body)
	}
	// The compose form must NOT render for a non-operator.
	if strings.Contains(body, `hx-post="/plans/gd-plan1/assistant/prompt"`) {
		t.Errorf("non-operator must not see the compose form; body=%s", body)
	}
}

// TestHandlePlanDetail_EmbedsTranscript proves the transcript renders on the
// FULL plan-detail page load (not only via the HTMX fragment round-trip) — the
// first-paint parity the lane's data-bag pass-through guarantees.
func TestHandlePlanDetail_EmbedsTranscript(t *testing.T) {
	f := seedPlansFake()
	seedTranscriptTurn(f, "gd-turnA", "gd-plan1", "matt", planTurnRoleOperator,
		"first-paint transcript line", planAssistantTestNow.Add(-time.Minute))
	mux := plansAssistantMux(t, f, nil)

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`class="bead-chat"`, "first-paint transcript line"} {
		if !strings.Contains(body, want) {
			t.Errorf("plan detail should embed the transcript on first paint; missing %q", want)
		}
	}
}

// TestPlanAssistantView_ActivityFeedDemoted proves the lifecycle activity feed
// is now a collapsed <details> secondary disclosure (task 2b) rather than the
// always-open primary block — the transcript is the primary surface.
func TestPlanAssistantView_ActivityFeedDemoted(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	mux := plansAssistantMux(t, f, nil)

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The activity feed still renders (the assistant bead is an activity row),
	// but inside a collapsible <details class="plan-assistant-activity">.
	if !strings.Contains(body, `<details class="plan-assistant-activity">`) {
		t.Errorf("activity feed should be demoted to a <details> disclosure; body=%s", body)
	}
	if !strings.Contains(body, "Recent activity") {
		t.Errorf("activity feed heading should still render; body=%s", body)
	}
}

// TestHandlePlanEvents_AssistantFrameCarriesTranscript proves the SSE
// plan-agent-updated frame re-renders the transcript too — the fragment
// render path (renderPlanAssistantFragment) resolves the nested bead_chat
// template (registered as a standalone partialDep) and includes the chat log.
// Guards the live-swap seam so a persisted turn shows up without a page reload.
func TestHandlePlanEvents_AssistantFrameCarriesTranscript(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	seedTranscriptTurn(f, "gd-turnSSE", "gd-plan1", "matt", planTurnRoleOperator,
		"a live-swapped transcript line", planAssistantTestNow.Add(-time.Minute))
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// An update to the assistant bead emits the plan-agent-updated frame.
	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-asstlive","type":"agent"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-agent-updated") {
		t.Fatalf("expected a plan-agent-updated frame; got: %s", got)
	}
	if !strings.Contains(got, "a live-swapped transcript line") {
		t.Errorf("SSE assistant frame should carry the transcript; got: %s", got)
	}
	if !strings.Contains(got, "bead-chat") {
		t.Errorf("SSE assistant frame should render the chat panel; got: %s", got)
	}
}

// TestPlanTranscriptView_DaemonErrorDegradesEmpty proves a daemon error on the
// transcript fetch degrades to an empty view (the lane still renders) rather
// than failing the whole page.
func TestPlanTranscriptView_DaemonErrorDegradesEmpty(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	f.listErr = errors.New("daemon down")
	mux := plansAssistantMux(t, f, nil)

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 despite the transcript list error, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="bead-chat"`) {
		t.Errorf("a transcript list error should degrade to no chat panel; body=%s", body)
	}
	if !strings.Contains(body, `id="plan-agent"`) {
		t.Errorf("the lane must still render on a transcript error; body=%s", body)
	}
}
