package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/oidcauth"
)

// plansAssistantMux builds a mux over a fake daemon + optional fake coopmux,
// with the clock pinned to planAssistantTestNow so liveness is deterministic
// (S1's assistantIsLive uses agent_state + s.now()-relative recency).
func plansAssistantMux(t *testing.T, f *fakeDaemon, cm screenClient) http.Handler {
	t.Helper()
	srv := NewServer(f, slog.Default(), "", "", "", cm)
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux)
}

// operatorPost builds a POST request pre-populated with an operator role,
// operator email header, and form-urlencoded body — the canonical shape for
// every mutating plan-assistant test.
func operatorPost(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	return req
}

// operatorHxPost is operatorPost plus the HX-Request:true header htmx sets on
// its AJAX submit — the canonical shape for a browser composer send. The prompt
// handler branches on this header (ka-0000005067): an htmx submit gets the lane
// fragment / inline-error body, a NATIVE (headerless) submit gets a 303 PRG
// redirect. Delivery-path assertions (fragment classes, 502 error bodies) use
// this helper so they exercise the htmx contract the browser actually hits.
func operatorHxPost(target, body string) *http.Request {
	req := operatorPost(target, body)
	req.Header.Set("HX-Request", "true")
	return req
}

// TestPlanAssistantPrompt_NudgesLiveAssistant: when the plan has a LIVE
// assistant, the POST nudges coopmux (sub-second path) and does NOT spawn a
// new bead. The lane's chip stays "live".
func TestPlanAssistantPrompt_NudgesLiveAssistant(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	_ = epic
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	form := url.Values{"message": {"refine step 3 please"}}
	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 1 {
		t.Fatalf("expected 1 Nudge call, got %d", cm.nudgeCalls)
	}
	if cm.nudgeLastID != "sess-gd-asstlive" {
		t.Errorf("nudge sessionID = %q, want sess-gd-asstlive", cm.nudgeLastID)
	}
	if cm.nudgeLastText != "refine step 3 please" {
		t.Errorf("nudge message = %q", cm.nudgeLastText)
	}
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("live nudge must not spawn a new assistant; got %d agent CreateBead calls", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-state="live"`) {
		t.Errorf("expected the returned lane fragment to render state=live; body=%s", body)
	}
	if !strings.Contains(body, `id="plan-agent"`) {
		t.Errorf("expected the returned fragment to be the lane partial; body=%s", body)
	}
}

// TestPlanAssistantPrompt_MintsOperatorTurn proves the operator's literal message
// is persisted as a turn_role=operator transcript bead bound to the plan by
// fields.plan_id (task 2a) — the operator half of the durable transcript. This
// is minted in handlePlanAssistantPrompt (which holds the real message), NOT in
// the shared promptPlanAssistant that the synthetic add-step seed also flows
// through.
func TestPlanAssistantPrompt_MintsOperatorTurn(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	mux := plansAssistantMux(t, f, &fakeScreenClient{})

	req := operatorPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"fold D2 into step 3"}}.Encode())
	mux.ServeHTTP(httptest.NewRecorder(), req)

	// Exactly one message bead (the operator turn); the live nudge mints no agent.
	var opTurn *beadsapi.CreateBeadRequest
	for i := range f.createReqs {
		if f.createReqs[i].Type == "message" {
			opTurn = &f.createReqs[i]
		}
	}
	if opTurn == nil {
		t.Fatal("expected an operator-turn message bead to be minted")
	}
	fields := decodeFakeFields(t, opTurn.Fields)
	if fields[planTranscriptPlanIDField] != "gd-plan1" {
		t.Errorf("operator turn plan_id = %q, want gd-plan1", fields[planTranscriptPlanIDField])
	}
	if fields[planTranscriptTurnRoleField] != planTurnRoleOperator {
		t.Errorf("operator turn_role = %q, want operator", fields[planTranscriptTurnRoleField])
	}
	if fields["text"] != "fold D2 into step 3" {
		t.Errorf("operator turn text = %q, want the literal message", fields["text"])
	}
	if opTurn.IdempotencyKey == "" || !strings.HasPrefix(opTurn.IdempotencyKey, "plan-turn:gd-plan1:op:") {
		t.Errorf("operator turn IdempotencyKey = %q, want plan-turn:gd-plan1:op:<hash>", opTurn.IdempotencyKey)
	}
}

// TestPlanAssistantPrompt_OperatorDoubleSubmitCollapses proves an htmx
// double-submit of the SAME operator message collapses to ONE transcript bead
// via the process-independent IdempotencyKey (the fake models migration 029).
func TestPlanAssistantPrompt_OperatorDoubleSubmitCollapses(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	mux := plansAssistantMux(t, f, &fakeScreenClient{})

	form := url.Values{"message": {"go"}}.Encode()
	mux.ServeHTTP(httptest.NewRecorder(), operatorPost("/plans/gd-plan1/assistant/prompt", form))
	mux.ServeHTTP(httptest.NewRecorder(), operatorPost("/plans/gd-plan1/assistant/prompt", form))

	keys := map[string]struct{}{}
	for _, req := range f.createReqs {
		if req.Type == "message" {
			keys[req.IdempotencyKey] = struct{}{}
		}
	}
	if len(keys) != 1 {
		t.Errorf("identical operator messages must share ONE IdempotencyKey; got %d: %v", len(keys), keys)
	}
}

// TestPlanAssistantPrompt_WakesParkedAssistant: a plan with a PARKED assistant
// (agent_state=stopping, stale) is woken via the spawn path — spawnPlanAssistant
// re-mints the same name with session_resume=true. Coopmux is NOT called.
func TestPlanAssistantPrompt_WakesParkedAssistant(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	_ = epic
	// Parked assistant: alive-state=stopping, stale-timestamp.
	seedAssistantBead(f, "gd-asstpark", "gardenio-plan-asst-park", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	form := url.Values{"message": {"wake up and answer question 4"}}
	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("parked wake must not call coopmux Nudge; got %d", cm.nudgeCalls)
	}
	// Wake REVIVES the prior bead in place (bug ka-0000002360): no new bead is
	// minted, because the agent bead id IS the S3 session-resume key and a new
	// id would point the woken pod at an empty prefix and cold-start.
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Fatalf("wake must NOT CreateBead a new assistant (stable id = stable S3 resume key); got %d agent creates", got)
	}
	// The revive is ONE atomic write on the SAME bead: a status flip carrying the
	// refreshed fields (ka-0000001583). The fields ride on updateLastReq.Fields.
	if f.updateLastID != "gd-asstpark" {
		t.Errorf("wake updated %q, want the prior bead gd-asstpark", f.updateLastID)
	}
	if got := f.updateLastReq.Fields["session_resume"]; got != "true" {
		t.Errorf("wake should set session_resume=true on the revived bead; got %v", got)
	}
	if got := f.updateLastReq.Fields["prompt"]; got != "wake up and answer question 4" {
		t.Errorf("wake should refresh the prompt field to the operator message; got %v", got)
	}
	if got, ok := f.updateLastReq.Fields["stop_requested"]; !ok || got != "" {
		t.Errorf("wake should clear stop_requested on the revived bead; got %v (present=%v)", got, ok)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "in_progress" {
		t.Errorf("wake should flip the prior bead gd-asstpark to status=in_progress (the AgentSpawn re-spawn trigger); got status=%v",
			f.updateLastReq.Status)
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-state="waking"`) {
		t.Errorf("expected the returned lane fragment to render state=waking after wake; body=%s", body)
	}
}

// TestPlanAssistantPrompt_SpawnsFirstAssistant: a plan with NO assistant yet
// spawns a fresh one on first message (no session_resume flag).
func TestPlanAssistantPrompt_SpawnsFirstAssistant(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	form := url.Values{"message": {"draft the plan for me"}}
	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("first-spawn must not call coopmux; got %d", cm.nudgeCalls)
	}
	// Two creates: the operator-turn transcript bead (type=message, minted first)
	// and the fresh assistant agent bead (type=agent). createLastReq is the
	// agent bead because the operator mint runs BEFORE promptPlanAssistant.
	if got := f.createCallsOfType("agent"); got != 1 {
		t.Fatalf("expected 1 agent CreateBead for the fresh spawn, got %d", got)
	}
	if got := f.createCallsOfType("message"); got != 1 {
		t.Fatalf("expected the operator message to be minted as 1 transcript turn, got %d", got)
	}
	fields := decodeFakeFields(t, f.createLastReq.Fields)
	if fields["role"] != planGardenerRole {
		t.Errorf("spawn role = %q, want %q", fields["role"], planGardenerRole)
	}
	if fields[planIDField] != "gd-plan1" {
		t.Errorf("spawn plan_id = %q, want gd-plan1", fields[planIDField])
	}
	if fields["prompt"] != "draft the plan for me" {
		t.Errorf("spawn prompt = %q, want the operator's message", fields["prompt"])
	}
	// Fresh spawn must NOT flag session_resume — there's no prior conversation.
	if fields["session_resume"] == "true" {
		t.Error("first-time spawn must not set session_resume=true")
	}
	// The load-bearing assigned edge (S1).
	if f.addDepLastType != "assigned" || f.addDepLastDep != "gd-plan1" {
		t.Errorf("assigned edge = (%s -> %s), want assigned -> gd-plan1", f.addDepLastType, f.addDepLastDep)
	}
	// The operator must be stamped on the CreateBead ctx so the daemon audit
	// resolver doesn't fail-closed with a 400 that surfaces as the "Failed to
	// start the plan assistant" 502 (ka-0000000017). The handler re-stamps it
	// (matching quick_spawn.go) so this holds even without operatorMiddleware.
	if f.createLastOpBy != "oidc:op@acme.example" {
		t.Errorf("CreateBead operator = %q, want oidc:op@acme.example", f.createLastOpBy)
	}
}

// TestPlanAssistantPrompt_EmptyMessageRejected returns 400 without touching
// coopmux or the daemon (defense-in-depth for the placeholder-only submission).
func TestPlanAssistantPrompt_EmptyMessageRejected(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	form := url.Values{"message": {"   "}}
	req := operatorPost("/plans/gd-plan1/assistant/prompt", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty message, got %d", w.Code)
	}
	if f.createCalls != 0 || cm.nudgeCalls != 0 {
		t.Errorf("empty message must not touch anything; createCalls=%d nudgeCalls=%d", f.createCalls, cm.nudgeCalls)
	}
	if !strings.Contains(w.Body.String(), "Message is required") {
		t.Errorf("expected error message in rendered fragment; body=%s", w.Body.String())
	}
}

// TestPlanAssistantPrompt_NonOperatorForbidden: viewer roles cannot POST — the
// operatorOnly gate on the route rejects with 4xx and NEVER touches coopmux
// or the daemon.
func TestPlanAssistantPrompt_NonOperatorForbidden(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	form := url.Values{"message": {"sneaky viewer message"}}
	req := httptest.NewRequest(http.MethodPost, "/plans/gd-plan1/assistant/prompt", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("viewer must not prompt the assistant; got 200")
	}
	if f.createCalls != 0 || cm.nudgeCalls != 0 {
		t.Errorf("viewer POST must not mutate; createCalls=%d nudgeCalls=%d", f.createCalls, cm.nudgeCalls)
	}
}

// TestPlanAssistantPrompt_PlanNotFound: hitting the endpoint on a non-existent
// or non-epic id returns 404.
func TestPlanAssistantPrompt_PlanNotFound(t *testing.T) {
	f := newFake()
	// No epic seeded.
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	form := url.Values{"message": {"anyone home?"}}
	req := operatorPost("/plans/gd-nope/assistant/prompt", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing plan, got %d", w.Code)
	}
}

// TestPlanAssistantView_RendersNoneState: a plan with no assistant yet renders
// the lane with state=none and the first-message hint.
func TestPlanAssistantView_RendersNoneState(t *testing.T) {
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
	if !strings.Contains(body, `data-state="none"`) {
		t.Errorf("expected state=none for a plan with no assistant; body=%s", body)
	}
	// The liveness chip moved to the unified plan-agent chip in the gardener bar
	// (step 5, ka-0000001332); the standalone assistant fragment no longer
	// carries a chip. data-state still drives the compose-box placeholder/hint.
	if strings.Contains(body, "plan-assistant-chip") {
		t.Errorf("assistant fragment should no longer render its own chip; body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/plans/gd-plan1/assistant/prompt"`) {
		t.Errorf("operator view should render the prompt form; body=%s", body)
	}
	// S5 (ka-0000003325): with no assistant bead the compose button reads "Spawn"
	// (the first message cold-starts one). Spawn is the expected first-contact
	// path, so it is NOT gated by a confirm (that friction is reserved for the
	// sensitive Wake transition) — and gating it would block the existing
	// spawn-send flow under a headless dialog-dismiss.
	if !strings.Contains(body, `data-send-state="none"`) {
		t.Errorf("expected the compose button to carry data-send-state=none; body=%s", body)
	}
	if !strings.Contains(body, `>Spawn</button>`) {
		t.Errorf("expected the none-state compose button to read Spawn; body=%s", body)
	}
	// The only confirm copy the compose form ever renders is the Wake gate; a
	// none-state (Spawn) form must not carry it. (Other card controls — e.g.
	// "Enable gardener" — carry their own unrelated confirms, so we match the
	// compose-form copy specifically rather than any hx-confirm.)
	if strings.Contains(body, "Wake the plan agent with this message?") {
		t.Errorf("a first-contact Spawn must not carry the Wake confirm gate; body=%s", body)
	}
}

// TestPlanAssistantView_RendersLiveState: a plan with a live assistant renders
// state=live plus the "Open ->" link. With no coopmux configured the link
// falls back to the assistant's bead page /{id} (ka-0000007019) — never the old
// /agents/{id}, which 404s.
func TestPlanAssistantView_RendersLiveState(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
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
		`data-state="live"`,
		`href="/gd-asstlive"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected the live-lane body to contain %q; body=%s", want, body)
		}
	}
	// The liveness chip is now the unified plan-agent chip in the gardener bar
	// (step 5, ka-0000001332); the standalone assistant fragment is chip-less.
	if strings.Contains(body, "plan-assistant-chip") {
		t.Errorf("assistant fragment should no longer render its own chip; body=%s", body)
	}
	if strings.Contains(body, "/agents/gd-asstlive") {
		t.Errorf("Open link must NOT use the 404 route /agents/{id}; body=%s", body)
	}
	// S5 (ka-0000003325): a live assistant takes the message directly, so the
	// compose button reads "Send" and carries NO confirm gate (a live nudge is a
	// single tap, not a pod-minting action).
	if !strings.Contains(body, `data-send-state="live"`) {
		t.Errorf("expected the compose button to carry data-send-state=live; body=%s", body)
	}
	if !strings.Contains(body, `>Send</button>`) {
		t.Errorf("expected the live-state compose button to read Send; body=%s", body)
	}
	// A live Send is a single tap — the compose button carries neither the Spawn
	// nor the Wake confirm gate (those guard only the pod-minting transitions).
	if strings.Contains(body, "Spawn the plan agent with this message?") ||
		strings.Contains(body, "Wake the plan agent with this message?") {
		t.Errorf("a live Send must not be operator-gated by a spawn/wake confirm; body=%s", body)
	}
}

// TestPlanAssistantView_RendersParkedState: assistant present but stale/stopping
// renders state=parked, no Nudge affordance in the copy.
func TestPlanAssistantView_RendersParkedState(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstpark", "gardenio-plan-asst-park", "gd-plan1", "stopping", planAssistantTestNow.Add(-3*time.Hour))
	mux := plansAssistantMux(t, f, nil)

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/agent", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-state="parked"`) {
		t.Errorf("expected state=parked; body=%s", body)
	}
	if !strings.Contains(body, "Next message wakes it") {
		t.Errorf("expected the wake hint copy; body=%s", body)
	}
	// S5 (ka-0000003325): a parked assistant is thawed by the next message, so the
	// compose button reads "Wake" and carries the wake confirm gate (a pod-thawing
	// action is a deliberate act, not a single tap).
	if !strings.Contains(body, `data-send-state="parked"`) {
		t.Errorf("expected the compose button to carry data-send-state=parked; body=%s", body)
	}
	if !strings.Contains(body, `>Wake</button>`) {
		t.Errorf("expected the parked-state compose button to read Wake; body=%s", body)
	}
	if !strings.Contains(body, "Wake the plan agent with this message?") {
		t.Errorf("expected the Wake transition to be operator-gated by an hx-confirm; body=%s", body)
	}
}

// TestSendButtonLabel: the compose button verb is a pure projection of the
// resolved assistant state — Spawn (none) / Wake (parked) / Send (everything a
// reachable agent covers: live, waking, queued) — and the confirm gate fires on
// exactly the sensitive Wake transition (S5, ka-0000003325). Spawn stays
// unblocked (first contact is expected) and Send stays unblocked (an instant
// live nudge must be a single tap). This pins the mapping directly so a state
// added later that should be Send-with-no-confirm (e.g. a new alive facet) is a
// deliberate change here, not a silent drift.
func TestSendButtonLabel(t *testing.T) {
	cases := []struct {
		state       planAssistantState
		wantLabel   string
		wantConfirm bool
	}{
		{planAssistantStateNone, "Spawn", false}, // first-contact Spawn is expected — not gated
		{planAssistantStateParked, "Wake", true}, // reviving a dormant pod is the one gated send
		{planAssistantStateLive, "Send", false},  // instant live nudge — single tap
		{planAssistantStateWaking, "Send", false},
		{planAssistantStateQueued, "Send", false},
		{planAssistantState(""), "Send", false}, // unknown → safe non-state-changing default
	}
	for _, tc := range cases {
		if got := sendButtonLabel(tc.state); got != tc.wantLabel {
			t.Errorf("sendButtonLabel(%q) = %q, want %q", tc.state, got, tc.wantLabel)
		}
		if got := sendButtonNeedsConfirm(tc.state); got != tc.wantConfirm {
			t.Errorf("sendButtonNeedsConfirm(%q) = %v, want %v", tc.state, got, tc.wantConfirm)
		}
	}
}

// TestHandlePlanDetail_EmbedsAssistantLane: the /plans/{id} page renders the
// assistant lane inline via the template, so the operator sees the prompt box
// on the first paint (no HTMX round-trip required).
func TestHandlePlanDetail_EmbedsAssistantLane(t *testing.T) {
	f := seedPlansFake()
	mux := plansAssistantMux(t, f, nil)

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-agent"`, // the merged card itself
		"Plan agent",      // the section heading
		"not started",     // the chip label for the no-assistant state
		// First-paint regression guard (ka-0000004026): the plan-detail page
		// includes the lane by passing the FULL data bag through, so the new
		// input-hardening fields must render on the initial load, not only via
		// the HTMX round-trip. A key-by-key dict re-projection silently dropped
		// these before.
		`maxlength="` + strconv.Itoa(maxPlanAssistantMessageLen) + `"`,
		`hx-on::response-error=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan detail should embed the assistant lane; missing %q; body=%s", want, body)
		}
	}
	// The dead .Error key must not resurface as a stray "<no value>" render.
	if strings.Contains(body, "<no value>") {
		t.Errorf("plan detail embed leaked a <no value> (missing data-bag key); body=%s", body)
	}
}

// TestPlanAssistantPrompt_MessageTooLong rejects an oversized message before
// touching coopmux or the daemon.
func TestPlanAssistantPrompt_MessageTooLong(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	huge := strings.Repeat("x", maxPlanAssistantMessageLen+1)
	form := url.Values{"message": {huge}}
	req := operatorPost("/plans/gd-plan1/assistant/prompt", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized message, got %d", w.Code)
	}
	if f.createCalls != 0 || cm.nudgeCalls != 0 {
		t.Errorf("oversized message must not mutate anything; createCalls=%d nudgeCalls=%d", f.createCalls, cm.nudgeCalls)
	}
}

// TestHandlePlanEvents_EmitsAssistantFrameOnAgentChange proves the SSE stream
// on /plans/{id}/events emits BOTH a plan-updated frame (steps) AND a
// plan-agent-updated frame when the bead that changed is the plan's
// assistant (S2, ka-0000007556). Assistant is included in the interest set
// via the assigned reverse-dep edge S1 writes.
func TestHandlePlanEvents_EmitsAssistantFrameOnAgentChange(t *testing.T) {
	f := seedPlansFake()
	// Add a live assistant so the filter includes its id and the type=agent
	// event renders the lane.
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return planAssistantTestNow })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// An update to the assistant bead — should emit BOTH plan-updated and
	// plan-agent-updated frames.
	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-asstlive","type":"agent"}}`),
	}
	// A non-agent update (the plan itself) should only emit plan-updated.
	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-plan1","type":"epic"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-updated") {
		t.Errorf("expected a plan-updated frame; got: %s", got)
	}
	if !strings.Contains(got, "event: plan-agent-updated") {
		t.Errorf("expected a plan-agent-updated frame for the agent-typed event; got: %s", got)
	}
	if !strings.Contains(got, `id="plan-agent"`) {
		t.Errorf("expected the assistant lane fragment in the SSE payload; got: %s", got)
	}
	// The epic update should NOT trigger a second plan-agent-updated frame
	// (non-agent type). Count occurrences.
	if got := strings.Count(got, "event: plan-agent-updated"); got != 1 {
		t.Errorf("expected exactly 1 plan-agent-updated frame, got %d", got)
	}
}

// TestPlanAssistantPrompt_LiveWithoutSessionFallsBackToSpawn: a "live" assistant
// bead that has no last_session_id (transient race — pod up, session not yet
// stamped) falls through to the wake/spawn path so the operator's click is
// preserved. Coopmux is NOT called (no session id to target).
func TestPlanAssistantPrompt_LiveWithoutSessionFallsBackToSpawn(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	_ = epic
	// Live-shape (alive state + fresh) but strip last_session_id so the nudge
	// path can't run.
	a := seedAssistantBead(f, "gd-asstsess", "gardenio-plan-asst-sess", "gd-plan1", "working", planAssistantTestNow)
	a.Fields["last_session_id"] = "" // simulate the race
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	form := url.Values{"message": {"try me"}}
	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("no last_session_id means nudge must NOT be called; got %d", cm.nudgeCalls)
	}
	// Fall-through takes the wake path, which REVIVES the existing bead in place
	// (no new AGENT bead) rather than minting one — the prior assistant already
	// exists. (The operator-turn transcript mint, a type=message create, is
	// expected and orthogonal — count only agent-bead creates here.)
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("expected wake-in-place (no agent CreateBead); got %d", got)
	}
	// Continuity: the SAME bead is revived (stable id) in ONE atomic status+fields
	// write (ka-0000001583).
	if f.updateLastID != "gd-asstsess" || f.updateLastReq.Status == nil || *f.updateLastReq.Status != "in_progress" {
		t.Errorf("wake should flip gd-asstsess to status=in_progress; got id=%q status=%v", f.updateLastID, f.updateLastReq.Status)
	}
}

// --- 2d: input hardening + honest delivery state (ka-0000004026) -------------
//
// These guard the R1 "message into the void" fix (ka-0000005794#10): a send is
// acknowledged with the HONEST delivery outcome (sent / queued-waking /
// spawning) distinct from the liveness chip, and every FAILURE returns a
// non-2xx plain-text body (so htmx does not swap it → the operator's textarea
// keeps their message) that the form surfaces inline via hx-on::response-error.

// TestPlanAssistantPrompt_HonestDelivery_Sent: nudging a LIVE assistant returns
// the "sent" ack (delivered to the live pod now), not a bare chip flip.
func TestPlanAssistantPrompt_HonestDelivery_Sent(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"go"}}.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`plan-assistant-delivery-sent`, // the honest ack class
		"the plan agent is live",       // the ack copy
		`data-state="live"`,            // chip still reflects liveness, separately
	} {
		if !strings.Contains(body, want) {
			t.Errorf("live-nudge response should contain %q; body=%s", want, body)
		}
	}
	// The ack must NOT masquerade as one of the other outcomes.
	if strings.Contains(body, "plan-assistant-delivery-queued-waking") || strings.Contains(body, "plan-assistant-delivery-spawning") {
		t.Errorf("live-nudge ack must be 'sent' only; body=%s", body)
	}
}

// TestPlanAssistantPrompt_HonestDelivery_QueuedWaking: waking a PARKED assistant
// returns the "queued-waking" ack — the message is parked and delivered on wake,
// which is the honest state (not "sent").
func TestPlanAssistantPrompt_HonestDelivery_QueuedWaking(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstpark", "gardenio-plan-asst-park", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"wake"}}.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-assistant-delivery-queued-waking") {
		t.Errorf("parked wake should ack 'queued-waking'; body=%s", body)
	}
	// A wake is queued, not spawned fresh, and certainly not "sent".
	if strings.Contains(body, "plan-assistant-delivery-spawning") || strings.Contains(body, "plan-assistant-delivery-sent") {
		t.Errorf("parked wake ack must be 'queued-waking' only; body=%s", body)
	}
}

// TestPlanAssistantPrompt_HonestDelivery_Spawning: the FIRST message to a plan
// with no assistant returns the "spawning" ack (cold start), distinct from the
// wake case above.
func TestPlanAssistantPrompt_HonestDelivery_Spawning(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"draft it"}}.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-assistant-delivery-spawning") {
		t.Errorf("first message should ack 'spawning'; body=%s", body)
	}
	if strings.Contains(body, "plan-assistant-delivery-queued-waking") || strings.Contains(body, "plan-assistant-delivery-sent") {
		t.Errorf("first-spawn ack must be 'spawning' only; body=%s", body)
	}
}

// TestPlanAssistantPrompt_SpawnFailure_PlainBodyPreservesInput: a spawn that
// fails returns a non-2xx status with a PLAIN-TEXT body (not the lane partial).
// This is what keeps the operator's typed message intact — htmx does not swap
// 4xx/5xx responses, so the textarea is never reset, and the hx-on::response-
// error hook surfaces this body inline. Directly guards R1.
func TestPlanAssistantPrompt_SpawnFailure_PlainBodyPreservesInput(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	f.createErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"draft it"}}.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("spawn failure should be a non-2xx (502) so htmx skips the swap; got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to start the plan assistant") {
		t.Errorf("failure body should carry the inline-surfaceable message; body=%s", body)
	}
	// Crucially: the body must NOT be the lane partial — re-rendering it would
	// reset the operator's textarea and lose the message (the R1 regression).
	if strings.Contains(body, `id="plan-agent"`) || strings.Contains(body, "plan-assistant-form") {
		t.Errorf("failure must NOT return the lane partial (would reset the textarea); body=%s", body)
	}
}

// TestPlanAssistantPrompt_HardNudgeFailure_PlainBody: a hard (non-session-gone)
// nudge error also returns a non-2xx plain body via translateUpstreamWriteError,
// never the partial — same textarea-preservation contract.
func TestPlanAssistantPrompt_HardNudgeFailure_PlainBody(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{nudgeErr: errors.New("coopmux 500")}
	mux := plansAssistantMux(t, f, cm)

	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"go"}}.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("a hard nudge failure must be non-2xx so htmx skips the swap; got 200 body=%q", w.Body.String())
	}
	// No spawn fallback on a hard error (mirrors the prior handler contract).
	// A spawn is an AGENT-bead create; the operator-turn transcript mint
	// (type=message, done before delivery) is expected and must not be counted.
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("hard nudge error must NOT fall back to spawn; got agent createCalls=%d", got)
	}
	if strings.Contains(w.Body.String(), `id="plan-agent"`) {
		t.Errorf("hard nudge failure must NOT return the lane partial; body=%s", w.Body.String())
	}
}

// TestPlanAssistantPrompt_SessionGoneFallsBackToSpawn_QueuedWaking: an upstream
// ErrSessionNotFound is recoverable — the handler degrades to the wake/spawn
// path so the message is never lost, and acks "queued-waking" (an assistant
// already exists) rather than erroring.
func TestPlanAssistantPrompt_SessionGoneFallsBackToSpawn_QueuedWaking(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{nudgeErr: coopmuxapi.ErrSessionNotFound}
	mux := plansAssistantMux(t, f, cm)

	req := operatorHxPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"go"}}.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("session-gone should recover via wake, got %d body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-assistant-delivery-queued-waking") {
		t.Errorf("session-gone recovery should ack 'queued-waking' (assistant exists); body=%s", body)
	}
	if !strings.Contains(body, `data-state="waking"`) {
		t.Errorf("session-gone recovery should render the waking chip; body=%s", body)
	}
}

// TestPlanAssistantLane_RendersInputHardening: the operator form binds the
// textarea maxlength to the server cap (no client/server drift) and wires the
// inline error-surfacing hook so a failed send is never silent (R1).
func TestPlanAssistantLane_RendersInputHardening(t *testing.T) {
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
	// maxlength bound to the server constant, not a hard-coded literal.
	wantMaxLen := `maxlength="` + strconv.Itoa(maxPlanAssistantMessageLen) + `"`
	if !strings.Contains(body, wantMaxLen) {
		t.Errorf("textarea maxlength should bind to the server cap %s; body=%s", wantMaxLen, body)
	}
	// The inline error surface + response-error hook (the R1 fix).
	for _, want := range []string{
		`class="plan-assistant-error`,
		`hx-on::response-error=`,
		`event.detail.xhr.responseText`,
		// after-request guards reset() on success AND runs the clear-on-send
		// reconcile (ka-0000002657) as an INLINE handler so it fires even when a
		// racing plan-agent-updated frame has already detached this form (a
		// document-level listener would miss the detached-node afterRequest).
		`hx-on::after-request="if(event.detail.successful) this.reset(); if(window.planComposerAfterRequest) window.planComposerAfterRequest(event)"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("operator form should wire %q; body=%s", want, body)
		}
	}
}

// --- native-submit fallback: Send never reloads the page (ka-0000005067) ------
//
// The composer form carries hx-post but ALSO a real method+action so a submit
// that fires before htmx wired the form (defer-load gap on first paint, a
// swapped-in card htmx.process missed/threw, or htmx not loading at all) POSTs
// to the SAME endpoint instead of doing a bare native submit against the current
// URL — which was the reported "hitting Send reloads the page" bug. The handler
// tells the two apart by the HX-Request header htmx sets on its AJAX submit.

// TestPlanAssistantLane_RendersNativeSubmitFallback proves the operator form
// renders a real method=post + action pointing at the prompt endpoint, so a
// pre-htmx submit is a POST to the handler (recoverable), never a page-reloading
// bare native submit.
func TestPlanAssistantLane_RendersNativeSubmitFallback(t *testing.T) {
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
	for _, want := range []string{
		`method="post"`,
		`action="/plans/gd-plan1/assistant/prompt"`,
		`hx-post="/plans/gd-plan1/assistant/prompt"`, // htmx path still present
	} {
		if !strings.Contains(body, want) {
			t.Errorf("composer form should render the native-submit fallback %q; body=%s", want, body)
		}
	}
}

// TestPlanAssistantPrompt_NativeSubmitRedirects proves a NATIVE (non-htmx) submit
// — no HX-Request header — still DELIVERS the message (nudges the live assistant)
// and then 303-redirects back to the plan (PRG), rather than returning a lane
// fragment the browser would paint as a bare page. This is the fix's core
// behavior: Send never reloads/strands the operator even when htmx hasn't wired
// the form.
func TestPlanAssistantPrompt_NativeSubmitRedirects(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	// operatorPost (no HX-Request) models the native fallback submit.
	req := operatorPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"native send"}}.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("native submit should 303-redirect (PRG), got %d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/plans/gd-plan1" {
		t.Errorf("native submit should redirect back to the plan; Location=%q", got)
	}
	// The message must still be DELIVERED — the whole point is Send works, not
	// just that it doesn't reload.
	if cm.nudgeCalls != 1 {
		t.Errorf("native submit must still nudge the live assistant; got %d nudge calls", cm.nudgeCalls)
	}
	if cm.nudgeLastText != "native send" {
		t.Errorf("native nudge message = %q, want %q", cm.nudgeLastText, "native send")
	}
}

// TestPlanAssistantPrompt_NativeSubmitFailureRedirects proves a native submit
// whose delivery FAILS still 303-redirects back to the plan rather than emitting
// a bare plain-text error page — the native fallback has no inline error hook, so
// stranding the operator on an error body would be a worse regression than the
// original reload.
func TestPlanAssistantPrompt_NativeSubmitFailureRedirects(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	f.createErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/assistant/prompt", url.Values{"message": {"draft it"}}.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("native submit failure should 303-redirect, not error-body; got %d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/plans/gd-plan1" {
		t.Errorf("native submit failure should redirect back to the plan; Location=%q", got)
	}
}
