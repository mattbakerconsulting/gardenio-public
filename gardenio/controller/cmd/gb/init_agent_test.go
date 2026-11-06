package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/coopapi"
)

func TestBypassStartup_IdleAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := bypassStartup(ctx, coop, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBypassStartup_WorkingAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := bypassStartup(ctx, coop, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBypassStartup_SetupPromptWithExit(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch r.URL.Path {
		case "/api/v1/agent":
			if callCount <= 3 {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"state": "starting",
					"prompt": map[string]string{
						"type": "setup",
					},
				})
			} else {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
			}
		case "/api/v1/screen":
			w.Write([]byte("Please select:\n1. Yes\n2. No, exit"))
		case "/api/v1/agent/respond":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := bypassStartup(ctx, coop, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestBypassStartup_AcceptsBypassPermissions: claudeless's bypass dialog is
// not groomed by coop, so bypassStartup must answer it once with Down+Return
// ("Yes, I accept") or a mock pod never reaches idle (gd-K77LsUtqhK).
func TestBypassStartup_AcceptsBypassPermissions(t *testing.T) {
	var accepted, keyPosts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			if accepted.Load() > 0 {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
				return
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "prompt",
				Prompt: &coopapi.AgentPrompt{Type: "setup", Subtype: "bypass_permissions"}})
		case "/api/v1/input/keys":
			keyPosts.Add(1)
			var body struct {
				Keys []string `json:"keys"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Keys) == 2 && body.Keys[0] == "Down" && body.Keys[1] == "Return" {
				accepted.Add(1)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := bypassStartup(ctx, coopapi.New(srv.URL), false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if accepted.Load() != 1 || keyPosts.Load() != 1 {
		t.Fatalf("want exactly one Down+Return, got %d key posts, %d accepts", keyPosts.Load(), accepted.Load())
	}
}

func TestBypassStartup_Cancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(coopapi.AgentState{State: "starting"})
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	err := bypassStartup(ctx, coop, false)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestContainsCI(t *testing.T) {
	tests := []struct {
		s, substr string
		want      bool
	}{
		{"Resume Session", "resume", true},
		{"RESUME SESSION", "session", true},
		{"hello world", "Resume", false},
		{"Resume a session", "resume", true},
	}
	for _, tt := range tests {
		got := containsCI(tt.s, tt.substr)
		if got != tt.want {
			t.Errorf("containsCI(%q, %q) = %v, want %v", tt.s, tt.substr, got, tt.want)
		}
	}
}

func TestEnvDuration_Default(t *testing.T) {
	d := envDuration("NONEXISTENT_GB_TEST_VAR_12345", 42*time.Second)
	if d != 42*time.Second {
		t.Errorf("expected 42s, got %v", d)
	}
}

func TestEnvDuration_Set(t *testing.T) {
	t.Setenv("TEST_GB_INIT_DUR", "10")
	d := envDuration("TEST_GB_INIT_DUR", 42*time.Second)
	if d != 10*time.Second {
		t.Errorf("expected 10s, got %v", d)
	}
}

func TestInjectPrompt_WorkingAgent_StillDelivers(t *testing.T) {
	// Regression test: injectPrompt must NOT skip when the agent is "working".
	// Coop accepts nudges during working state — the agent picks up the
	// message when the current generation finishes. The consumed prompt shows
	// up as request_count advancing (gd-udek4CJVRY), which is what lets a
	// single delivery succeed here.
	withFastPromptConsume(t)
	var nudges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			var requests uint64
			if nudges.Load() > 0 {
				requests = 1
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: requests})
		case "/api/v1/agent/nudge":
			nudges.Add(1)
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/input/keys":
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := injectPrompt(ctx, coop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := nudges.Load(); got != 1 {
		t.Fatalf("nudges: got %d, want exactly 1 — a working agent whose request_count advances consumed the prompt", got)
	}
}

// TestInjectPrompt_WorkingButUnsent_PressesEnter is the regression test for
// gd-udek4CJVRY: after a coop restart with --resume, coop pins the state at
// "working" while Claude Code is idle with the nudge text sitting as an
// unsubmitted paste ("❯ [Pasted text #1 +2 lines]"). Three crew sat like that
// for 8h46m. injectPrompt must not trust "working": with no turn landing it
// reads the screen, sees the unsent input, presses Enter, and only then (the
// turn lands, request_count advances) declares success — without a second
// copy of the prompt.
func TestInjectPrompt_WorkingButUnsent_PressesEnter(t *testing.T) {
	withFastPromptConsume(t)
	t.Setenv("BOAT_COMMAND", "claude")
	var nudges, enters atomic.Int32
	screen := "● 2 background shell command tasks didn't finish before the previous session ended.\n" +
		"────────────────────────────\n" +
		"❯ [Pasted text #1 +2 lines]\n" +
		"────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · PR #1452\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			var requests uint64
			if enters.Load() > 0 {
				requests = 1
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: requests})
		case "/api/v1/agent/nudge":
			nudges.Add(1)
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/screen/text":
			_, _ = w.Write([]byte(screen))
		case "/api/v1/input/keys":
			var body struct {
				Keys []string `json:"keys"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, k := range body.Keys {
				if k == "Return" {
					enters.Add(1)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := injectPrompt(ctx, coop); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := enters.Load(); got != 1 {
		t.Fatalf("Enter presses: got %d, want 1 (the unsent paste must be submitted)", got)
	}
	if got := nudges.Load(); got != 1 {
		t.Fatalf("nudges: got %d, want 1 (Enter resolved it; no duplicate prompt)", got)
	}
}

// TestInjectPrompt_WorkingNoTurnEmptyPrompt_ReSends: coop says "working", no
// turn lands, and the screen shows an EMPTY idle prompt (the paste never
// arrived at all). No stray Enter is pressed; the prompt is re-sent and the
// second delivery is consumed.
func TestInjectPrompt_WorkingNoTurnEmptyPrompt_ReSends(t *testing.T) {
	withFastPromptConsume(t)
	t.Setenv("BOAT_COMMAND", "claude")
	var nudges, enters atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			var requests uint64
			if nudges.Load() >= 2 {
				requests = 1
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: requests})
		case "/api/v1/agent/nudge":
			nudges.Add(1)
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/screen/text":
			_, _ = w.Write([]byte("● Done.\n\n❯\n  ⏵⏵ bypass permissions on\n"))
		case "/api/v1/input/keys":
			var body struct {
				Keys []string `json:"keys"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, k := range body.Keys {
				if k == "Return" {
					enters.Add(1)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := injectPrompt(ctx, coop); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := nudges.Load(); got != 2 {
		t.Fatalf("nudges: got %d, want 2 (re-send after the first was never consumed)", got)
	}
	if got := enters.Load(); got != 0 {
		t.Fatalf("Enter presses: got %d, want 0 (an empty prompt gets no stray Enter)", got)
	}
}

func TestScreenInputLineHasText(t *testing.T) {
	tests := []struct {
		name   string
		screen string
		want   bool
	}{
		{"unsent paste", "❯ You have been spawned...\n● working\n❯ [Pasted text #1 +2 lines]\n──\n  ⏵⏵ bypass permissions on\n", true},
		{"unsent plain text", "● Done.\n❯ continue\n", true},
		{"empty idle prompt", "● Done.\n\n❯\n  ⏵⏵ bypass permissions on\n", false},
		{"dialog option is not the input line", "Do you trust this folder?\n❯ 1. Yes\n  2. No\n", false},
		{"earlier echo above an empty prompt", "❯ old prompt text\n● answer\n❯ \n", false},
		{"no prompt glyph at all", "✶ Germinating… (31m)\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := screenInputLineHasText(tt.screen); got != tt.want {
				t.Errorf("screenInputLineHasText(%q) = %v, want %v", tt.screen, got, tt.want)
			}
		})
	}
}

// withFastPromptConsume shrinks confirmPromptConsumed's timeout/poll so tests
// exercising the working-agent consumption check don't wait the production
// 3m, and restores them afterward.
func withFastPromptConsume(t *testing.T) {
	t.Helper()
	origTimeout, origPoll := promptConsumeTimeout, promptConsumePoll
	promptConsumeTimeout = 300 * time.Millisecond
	promptConsumePoll = 20 * time.Millisecond
	t.Cleanup(func() {
		promptConsumeTimeout = origTimeout
		promptConsumePoll = origPoll
	})
}

func TestInjectPrompt_IdleAgent_Delivers(t *testing.T) {
	// After a nudge is delivered to an idle agent, the turn must start
	// (state→working) for injectPrompt to declare success. Flip to "working"
	// once the nudge lands. ka-0000002714.
	withFastConfirmTurn(t)
	nudgeDelivered := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			state := "idle"
			if nudgeDelivered {
				state = "working"
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{State: state})
		case "/api/v1/agent/nudge":
			nudgeDelivered = true
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := injectPrompt(ctx, coop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nudgeDelivered {
		t.Fatal("nudge was not delivered to idle agent")
	}
}

func TestInjectPrompt_RetryOnFailure(t *testing.T) {
	withFastConfirmTurn(t)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			// Stay idle until the successful (3rd) delivery, then start the turn.
			state := "idle"
			if attempts >= 3 {
				state = "working"
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{State: state})
		case "/api/v1/agent/nudge":
			attempts++
			if attempts < 3 {
				json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: false, Reason: "agent busy"})
			} else {
				json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
			}
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := injectPrompt(ctx, coop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts < 3 {
		t.Fatalf("expected at least 3 nudge attempts, got %d", attempts)
	}
}

// TestInjectPrompt_DeliveredButNoTurn_ReSends is the regression test for
// ka-0000002714: coop.Nudge reports Delivered=true the moment bytes are queued,
// but a nudge fired into the startup-idle window (coop reports idle before
// Claude's first hook) is silently dropped and the agent sits idle forever.
// injectPrompt must NOT trust the first Delivered=true: it must confirm the
// turn started and re-send when it didn't. Here the first delivery is "lost"
// (agent stays idle); only after the second delivery does the turn start.
func TestInjectPrompt_IdleInstantTurn_RequestCountConfirms(t *testing.T) {
	// claudeless answers instantly: coop goes idle -> idle and no poll sees
	// "working", but request_count advances. That must confirm the turn on the
	// first delivery, not re-send the prompt 3 times (gd-acTuM7LA75).
	withFastConfirmTurn(t)
	var deliveries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle", RequestCount: 4 + uint64(deliveries.Load())})
		case "/api/v1/agent/nudge":
			deliveries.Add(1)
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/input/keys":
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	baseline, err := injectPrompt(ctx, coopapi.New(srv.URL))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := deliveries.Load(); got != 1 {
		t.Errorf("expected exactly 1 delivery, got %d", got)
	}
	if baseline != 4 {
		t.Errorf("expected pre-prompt request_count baseline 4, got %d", baseline)
	}
}

func TestInjectPrompt_DeliveredButNoTurn_ReSends(t *testing.T) {
	withFastConfirmTurn(t)
	deliveries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			// Turn only starts after the SECOND delivery — the first was lost
			// in the startup-idle window.
			state := "idle"
			if deliveries >= 2 {
				state = "working"
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{State: state})
		case "/api/v1/agent/nudge":
			deliveries++
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/input/keys":
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := injectPrompt(ctx, coop); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deliveries < 2 {
		t.Fatalf("expected a re-send after the lost first nudge (>=2 deliveries), got %d", deliveries)
	}
}

// withFastConfirmTurn shrinks confirmNudgeTurn's timeout/poll so tests that
// exercise the turn-confirmation path don't wait the production 30s, and
// restores them afterward.
func withFastConfirmTurn(t *testing.T) {
	t.Helper()
	origTimeout, origPoll, origOutcome := confirmTurnTimeout, confirmTurnPoll, turnOutcomeTimeout
	confirmTurnTimeout = 2 * time.Second
	confirmTurnPoll = 20 * time.Millisecond
	turnOutcomeTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		confirmTurnTimeout = origTimeout
		confirmTurnPoll = origPoll
		turnOutcomeTimeout = origOutcome
	})
}

// withFastRecovery shrinks the inter-key delay AND the turn-confirmation
// timeout used by recoverFromErrorAndNudge so error-recovery tests don't wait
// the production 200ms×N + 30s. Restores both afterward.
func withFastRecovery(t *testing.T) {
	t.Helper()
	withFastConfirmTurn(t)
	origKeyDelay := recoveryKeyDelay
	recoveryKeyDelay = 1 * time.Millisecond
	t.Cleanup(func() { recoveryKeyDelay = origKeyDelay })
}

// TestRecoverFromErrorAndNudge_ClearsStuckError is the direct regression test
// for ka-0000006423: an agent WEDGED at coop state="error" (as it is after a
// 429) must be driven back to "working". The recovery must (a) dismiss the
// error screen via keystrokes, (b) submit the work prompt via raw /api/v1/input
// (which bypasses the nudge state-gate that refuses delivery in "error"), and
// (c) confirm the fresh turn started. It must NOT use /api/v1/agent/nudge — that
// path is exactly what the bug proved gets silently dropped in the error state.
func TestRecoverFromErrorAndNudge_ClearsStuckError(t *testing.T) {
	withFastRecovery(t)

	dismissKeys := 0
	inputText := ""
	inputEnter := false
	nudgeCalled := false
	inputSubmitted := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			// Stuck at error until the raw prompt is submitted; then a fresh
			// turn starts (working) — proof the error cleared.
			if inputSubmitted {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			} else {
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State: "error", ErrorCategory: "rate_limited",
				})
			}
		case "/api/v1/input/keys":
			dismissKeys++
			w.WriteHeader(http.StatusOK)
		case "/api/v1/input":
			var req struct {
				Text  string `json:"text"`
				Enter bool   `json:"enter"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			inputText, inputEnter, inputSubmitted = req.Text, req.Enter, true
			json.NewEncoder(w).Encode(map[string]int{"bytes_written": len(req.Text)})
		case "/api/v1/agent/nudge":
			nudgeCalled = true
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := recoverFromErrorAndNudge(ctx, coop); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dismissKeys == 0 {
		t.Error("error screen was never dismissed (no SendKeys)")
	}
	if !inputSubmitted {
		t.Fatal("work prompt was never submitted via raw /api/v1/input — the stuck error was not cleared")
	}
	if inputText == "" {
		t.Error("submitted input text was empty — expected the resolved work prompt")
	}
	if !inputEnter {
		t.Error("raw input was sent without enter=true — the prompt would sit unsubmitted (see bug note)")
	}
	if nudgeCalled {
		t.Error("recovery used /api/v1/agent/nudge — that is refused in the error state and is the bug being fixed")
	}
}

// TestRecoverFromErrorAndNudge_RetriesUntilTurnStarts proves the recovery
// retries the full dismiss+submit sequence when the first raw input does not
// produce a work turn (the confirm-turn safety net), rather than giving up.
func TestRecoverFromErrorAndNudge_RetriesUntilTurnStarts(t *testing.T) {
	withFastRecovery(t)

	inputSubmits := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			// Only after the SECOND submitted prompt does a turn start — the
			// first submit "landed but never took" (confirm-turn returns false).
			if inputSubmits >= 2 {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			} else {
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State: "error", ErrorCategory: "rate_limited",
				})
			}
		case "/api/v1/input/keys":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/input":
			inputSubmits++
			json.NewEncoder(w).Encode(map[string]int{"bytes_written": 12})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := recoverFromErrorAndNudge(ctx, coop); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inputSubmits < 2 {
		t.Fatalf("expected a retry after the first submit failed to start a turn (>=2 submits), got %d", inputSubmits)
	}
}

func TestRateLimitBackoff_NoHint_JitteredExponential(t *testing.T) {
	oldStreak := rateLimitNoHintStreak
	t.Cleanup(func() { rateLimitNoHintStreak = oldStreak })
	rateLimitNoHintStreak = 0

	// No reset hint (the account-spend-limit case). Each consecutive hit must
	// (a) stay within the equal-jitter window [ceil/2, ceil] for that attempt,
	// (b) have a non-decreasing ceiling as the streak grows, and (c) never
	// exceed the 30m cap.
	const msg = "API Error: Rate limit reached"
	var prevCeil time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		got := rateLimitBackoff(msg)

		// Expected deterministic ceiling for this attempt.
		ceil := rateLimitBackoffCap
		if shift := attempt - 1; shift < 30 {
			if scaled := rateLimitBackoffBase << shift; scaled > 0 && scaled < ceil {
				ceil = scaled
			}
		}
		if got < ceil/2 || got > ceil {
			t.Errorf("attempt %d: %v outside equal-jitter window [%v,%v]", attempt, got, ceil/2, ceil)
		}
		if got > rateLimitBackoffCap {
			t.Errorf("attempt %d: %v exceeds cap %v", attempt, got, rateLimitBackoffCap)
		}
		if ceil < prevCeil {
			t.Errorf("attempt %d: ceiling %v shrank below previous %v", attempt, ceil, prevCeil)
		}
		prevCeil = ceil
	}
	// After many hits the ceiling has saturated at the cap.
	if prevCeil != rateLimitBackoffCap {
		t.Errorf("ceiling did not saturate at cap: %v", prevCeil)
	}
}

func TestRateLimitBackoff_NoHint_GrowsThenCaps(t *testing.T) {
	oldStreak := rateLimitNoHintStreak
	t.Cleanup(func() { rateLimitNoHintStreak = oldStreak })
	rateLimitNoHintStreak = 0

	// Lower bound (ceil/2) is strictly increasing while below the cap, which
	// proves exponential growth independent of the random jitter component.
	const msg = "garbage with no reset"
	var prevLower time.Duration = -1
	cappedAt := 0
	for attempt := 1; attempt <= 8; attempt++ {
		_ = rateLimitBackoff(msg)
		ceil := rateLimitBackoffCap
		if shift := attempt - 1; shift < 30 {
			if scaled := rateLimitBackoffBase << shift; scaled > 0 && scaled < ceil {
				ceil = scaled
			}
		}
		lower := ceil / 2
		if ceil == rateLimitBackoffCap && cappedAt == 0 {
			cappedAt = attempt
		}
		if cappedAt == 0 && lower <= prevLower {
			t.Errorf("attempt %d: lower bound %v did not grow past %v", attempt, lower, prevLower)
		}
		prevLower = lower
	}
	if cappedAt == 0 {
		t.Error("backoff never reached the cap within 8 attempts")
	}
}

func TestRateLimitBackoff_ResetHint_ClearsStreak(t *testing.T) {
	oldStreak := rateLimitNoHintStreak
	t.Cleanup(func() { rateLimitNoHintStreak = oldStreak })

	// Build up a no-hint streak, then a reset-hint hit must clear it so the
	// next no-hint hit restarts backoff from the base attempt.
	rateLimitNoHintStreak = 5
	if d := rateLimitBackoff("Claude usage limit reached · resets 11pm (UTC)"); d <= time.Minute {
		t.Errorf("expected parsed reset duration > 1m, got %v", d)
	}
	if rateLimitNoHintStreak != 0 {
		t.Errorf("reset hint did not clear no-hint streak: %d", rateLimitNoHintStreak)
	}
	// Next no-hint hit is attempt 1 again → within base equal-jitter window.
	if d := rateLimitBackoff("no hint here"); d < rateLimitBackoffBase/2 || d > rateLimitBackoffBase {
		t.Errorf("post-reset first hit out of base window: %v", d)
	}
}

func TestRateLimitBackoff_ParsesResetTime(t *testing.T) {
	// Use a time well in the future so the test doesn't depend on time of day.
	// parseResetDuration handles "resets Xpm" — we just verify it returns > 0
	// and the 1-minute buffer is applied.
	msg := "Claude usage limit reached · resets 11pm (UTC)"
	d := rateLimitBackoff(msg)
	if d <= time.Minute {
		t.Errorf("expected > 1m for parseable reset time, got %v", d)
	}
}

func TestRateLimitBackoff_Override(t *testing.T) {
	rateLimitBackoffOverride = 50 * time.Millisecond
	t.Cleanup(func() { rateLimitBackoffOverride = 0 })

	d := rateLimitBackoff("API Error: Rate limit reached")
	if d != 50*time.Millisecond {
		t.Errorf("override not applied: got %v", d)
	}
}

// -- Model-group rate limit backoff (ka-0000001718) --------------------------

func TestModelRateLimitBackoff_GrowsThenCaps(t *testing.T) {
	oldStreak := modelRateLimitStreak
	t.Cleanup(func() { modelRateLimitStreak = oldStreak })
	modelRateLimitStreak = 0

	// The equal-jitter lower bound (ceil/2) grows exponentially until the 30m
	// cap, independent of the random jitter component.
	var prevLower time.Duration = -1
	cappedAt := 0
	for attempt := 1; attempt <= 12; attempt++ {
		got := modelRateLimitBackoff()
		ceil := modelRateLimitBackoffCap
		if shift := attempt - 1; shift < 30 {
			if scaled := modelRateLimitBackoffBase << shift; scaled > 0 && scaled < ceil {
				ceil = scaled
			}
		}
		if got < ceil/2 || got > ceil {
			t.Errorf("attempt %d: %v outside equal-jitter window [%v,%v]", attempt, got, ceil/2, ceil)
		}
		if got > modelRateLimitBackoffCap {
			t.Errorf("attempt %d: %v exceeds cap %v", attempt, got, modelRateLimitBackoffCap)
		}
		lower := ceil / 2
		if ceil == modelRateLimitBackoffCap && cappedAt == 0 {
			cappedAt = attempt
		}
		if cappedAt == 0 && lower <= prevLower {
			t.Errorf("attempt %d: lower bound %v did not grow past %v", attempt, lower, prevLower)
		}
		prevLower = lower
	}
	if cappedAt == 0 {
		t.Error("model-rate-limit backoff never reached the cap within 12 attempts")
	}
}

func TestModelRateLimitBackoff_Override(t *testing.T) {
	modelRateLimitBackoffOverride = 50 * time.Millisecond
	t.Cleanup(func() { modelRateLimitBackoffOverride = 0 })

	if d := modelRateLimitBackoff(); d != 50*time.Millisecond {
		t.Errorf("override not applied: got %v", d)
	}
}

func TestMonitorExit_ModelRateLimited_NoRotationNoShutdown(t *testing.T) {
	// A model-group (pool-exhaustion) 429 must be handled IN PLACE by
	// monitorExit exactly like the account rate-limit case: dismiss the error
	// screen, sleep in place (NO shutdown → liveness probe stays up), re-inject
	// the work prompt, and continue the SAME session. There is no exit-10 /
	// credential switch involved (that is prevented upstream in coop, since
	// ModelRateLimited is not is_auth_exhaustion) — this test proves the
	// controller's park branch fires on error_category=model_rate_limited.
	modelRateLimitBackoffOverride = 50 * time.Millisecond
	monitorSleepOverride = 10 * time.Millisecond
	withFastRecovery(t)
	t.Cleanup(func() { modelRateLimitBackoffOverride = 0; monitorSleepOverride = 0 })

	shutdownCalled := false
	nudgeCalled := false
	inputSubmitted := false
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			callCount++
			switch {
			case callCount <= 3:
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			case !inputSubmitted:
				// Model-group rate limited (pool exhausted, no fallback): coop is
				// WEDGED at state="error" until a fresh submitted turn clears it.
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State:         "error",
					ErrorCategory: "model_rate_limited",
					LastMessage:   "Received Model Group=claude-opus-4-8-oauth Available Model Group Fallbacks=None",
				})
			default:
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			}
		case "/api/v1/input/keys":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/input":
			inputSubmitted = true
			json.NewEncoder(w).Encode(map[string]int{"bytes_written": 12})
		case "/api/v1/agent/nudge":
			nudgeCalled = true
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("shutdown was called during model rate limit — should sleep in-place instead")
	}
	if !inputSubmitted {
		t.Error("raw PTY input was not submitted — recovery from the stuck error state did not happen (ka-0000006423)")
	}
	if nudgeCalled {
		t.Error("plain nudge was used — it is refused in the error state; recovery must use raw /api/v1/input")
	}
}

func TestMonitorExit_RateLimited_NoShutdown(t *testing.T) {
	// When rate-limited, monitorExit must NOT call /api/v1/shutdown while waiting.
	// It should sleep in-place, then RECOVER FROM THE STUCK error state (dismiss
	// the error screen + raw PTY input, NOT a plain nudge), and continue.
	//
	// This is the ka-0000006423 regression: after a 429 coop is stuck at
	// state="error", which NEVER clears on its own (the low-confidence
	// screen-parse tier can't downgrade error→idle). A plain nudge is refused in
	// the error state, so recovery MUST drive a fresh turn via raw /api/v1/input.
	// We model the sticky-error reality: the state stays "error" until raw input
	// is sent, then flips to "working".
	rateLimitBackoffOverride = 50 * time.Millisecond
	monitorSleepOverride = 10 * time.Millisecond
	withFastRecovery(t)
	t.Cleanup(func() { rateLimitBackoffOverride = 0; monitorSleepOverride = 0 })

	shutdownCalled := false
	nudgeCalled := false
	inputSubmitted := false
	dismissed := false
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			callCount++
			switch {
			case callCount <= 3:
				// Initial working state (covers the startup sleep).
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			case !inputSubmitted:
				// From the 429 onward coop is WEDGED at state="error" — it stays
				// there through the backoff and does NOT self-clear to idle. Only
				// raw input (a fresh submitted turn) can move it off error.
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State:         "error",
					ErrorCategory: "rate_limited",
					LastMessage:   "API Error: Rate limit reached",
				})
			default:
				// Raw prompt was submitted → a fresh turn started → working.
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			}
		case "/api/v1/input/keys":
			dismissed = true
			w.WriteHeader(http.StatusOK)
		case "/api/v1/input":
			inputSubmitted = true
			json.NewEncoder(w).Encode(map[string]int{"bytes_written": 12})
		case "/api/v1/agent/nudge":
			nudgeCalled = true
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	// Cancel after 1s — enough time for rate-limit + recovery cycle, but
	// before any legitimate exit so shutdown is never triggered.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := monitorExit(ctx, coop, nil, noRequestBaseline)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("shutdown was called during rate limit — should sleep in-place instead")
	}
	if !dismissed {
		t.Error("error screen was not dismissed (no SendKeys) during rate-limit recovery")
	}
	if !inputSubmitted {
		t.Error("raw PTY input was not submitted — recovery from the stuck error state did not happen (ka-0000006423)")
	}
	if nudgeCalled {
		t.Error("plain nudge was used — it is refused in the error state; recovery must use raw /api/v1/input")
	}
}

func TestMonitorExit_RateLimited_ContextCancelled(t *testing.T) {
	// Context cancellation during rate limit sleep must return cleanly (no hang).
	rateLimitBackoffOverride = 5 * time.Second // long enough that cancel fires first
	monitorSleepOverride = 10 * time.Millisecond
	t.Cleanup(func() { rateLimitBackoffOverride = 0; monitorSleepOverride = 0 })

	shutdownCalled := false
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			callCount++
			if callCount <= 3 {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			} else {
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State:         "working",
					ErrorCategory: "rate_limited",
					LastMessage:   "API Error: Rate limit reached",
				})
			}
		case "/api/v1/input/keys":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- monitorExit(ctx, coop, nil, noRequestBaseline) }()

	// Let it hit the rate_limited branch, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("monitorExit did not return after context cancellation")
	}
	if shutdownCalled {
		t.Error("shutdown should not be called on context cancellation during rate limit sleep")
	}
}

func TestMonitorExit_ServerError_ReNudges(t *testing.T) {
	// When coop reports a transient server error (HTTP 500 / api_error),
	// monitorExit must NOT shut down. It should dismiss the error screen,
	// sleep in-place (liveness probe stays up), then re-inject the prompt and
	// continue — exactly the recovery the rate_limited path already does, but
	// keyed on error_category=server_error (ka-0000002780).
	serverErrorBackoffOverride = 50 * time.Millisecond
	monitorSleepOverride = 10 * time.Millisecond
	withFastRecovery(t)
	t.Cleanup(func() { serverErrorBackoffOverride = 0; monitorSleepOverride = 0; serverErrorStreak = 0 })

	shutdownCalled := false
	nudgeCalled := false
	inputSubmitted := false
	dismissed := false
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			callCount++
			switch {
			case callCount <= 3:
				// Initial working state (covers the startup sleep).
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			case !inputSubmitted:
				// Transient 500 surfaced by coop: state is WEDGED at "error"
				// until a fresh submitted turn clears it (ka-0000006423).
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State:         "error",
					ErrorCategory: "server_error",
					LastMessage:   `API Error: 500 {"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
				})
			default:
				// Raw prompt submitted → fresh turn → recovered to working.
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			}
		case "/api/v1/input/keys":
			dismissed = true
			w.WriteHeader(http.StatusOK)
		case "/api/v1/input":
			inputSubmitted = true
			json.NewEncoder(w).Encode(map[string]int{"bytes_written": 12})
		case "/api/v1/agent/nudge":
			nudgeCalled = true
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("shutdown was called on a transient server error — should sleep in-place + recover")
	}
	if !dismissed {
		t.Error("error screen was not dismissed (no SendKeys) on server error")
	}
	if !inputSubmitted {
		t.Error("raw PTY input was not submitted — recovery from the stuck error state did not happen (ka-0000006423)")
	}
	if nudgeCalled {
		t.Error("plain nudge was used — it is refused in the error state; recovery must use raw /api/v1/input")
	}
}

func TestMonitorExit_ServerError_ContextCancelled(t *testing.T) {
	// Context cancellation during the server-error backoff sleep must return
	// cleanly (no hang, no shutdown).
	serverErrorBackoffOverride = 5 * time.Second // long enough that cancel fires first
	monitorSleepOverride = 10 * time.Millisecond
	t.Cleanup(func() { serverErrorBackoffOverride = 0; monitorSleepOverride = 0; serverErrorStreak = 0 })

	shutdownCalled := false
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			callCount++
			if callCount <= 3 {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			} else {
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State:         "working",
					ErrorCategory: "server_error",
					LastMessage:   "API Error: 500 Internal server error",
				})
			}
		case "/api/v1/input/keys":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- monitorExit(ctx, coop, nil, noRequestBaseline) }()

	// Let it hit the server_error branch, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("monitorExit did not return after context cancellation")
	}
	if shutdownCalled {
		t.Error("shutdown should not be called on context cancellation during server-error sleep")
	}
}

func TestServerErrorBackoff_ExponentialAndReset(t *testing.T) {
	t.Cleanup(func() { serverErrorStreak = 0 })
	serverErrorStreak = 0

	// First hit: equal-jitter around base (5s) → [2.5s, 5s].
	d1 := serverErrorBackoff()
	if d1 < serverErrorBackoffBase/2 || d1 > serverErrorBackoffBase {
		t.Errorf("first server-error backoff %v outside [%v, %v]", d1, serverErrorBackoffBase/2, serverErrorBackoffBase)
	}

	// Streak grows; values stay within the cap.
	for range 10 {
		d := serverErrorBackoff()
		if d > serverErrorBackoffCap {
			t.Errorf("server-error backoff %v exceeded cap %v", d, serverErrorBackoffCap)
		}
	}

	// Recovery resets the streak (monitorExit does this on state=working), so
	// the next hit is back to the base window.
	serverErrorStreak = 0
	dReset := serverErrorBackoff()
	if dReset < serverErrorBackoffBase/2 || dReset > serverErrorBackoffBase {
		t.Errorf("post-reset backoff %v outside base window [%v, %v]", dReset, serverErrorBackoffBase/2, serverErrorBackoffBase)
	}
}

func TestServerErrorBackoff_Override(t *testing.T) {
	serverErrorBackoffOverride = 50 * time.Millisecond
	t.Cleanup(func() { serverErrorBackoffOverride = 0 })

	if d := serverErrorBackoff(); d != 50*time.Millisecond {
		t.Errorf("override not applied: got %v", d)
	}
}

func TestMonitorExit_NoInternet_ReNudges(t *testing.T) {
	// When coop reports a transient connectivity drop (bare connection refused /
	// ECONNREFUSED / DNS failure), monitorExit must NOT shut down. It should
	// dismiss the error screen, sleep in-place (liveness probe stays up), then
	// re-inject the prompt and continue — exactly the recovery the server_error
	// path does, but keyed on error_category=no_internet (ka-0000001822).
	noInternetBackoffOverride = 50 * time.Millisecond
	monitorSleepOverride = 10 * time.Millisecond
	withFastRecovery(t)
	t.Cleanup(func() { noInternetBackoffOverride = 0; monitorSleepOverride = 0; noInternetStreak = 0 })

	shutdownCalled := false
	nudgeCalled := false
	inputSubmitted := false
	dismissed := false
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			callCount++
			switch {
			case callCount <= 3:
				// Initial working state (covers the startup sleep).
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			case !inputSubmitted:
				// Transient connectivity drop surfaced by coop: state is WEDGED at
				// "error" until a fresh submitted turn clears it (ka-0000006423).
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State:         "error",
					ErrorCategory: "no_internet",
					LastMessage:   "API Error: Connection refused (ECONNREFUSED)",
				})
			default:
				// Raw prompt submitted → fresh turn → recovered to working.
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			}
		case "/api/v1/input/keys":
			dismissed = true
			w.WriteHeader(http.StatusOK)
		case "/api/v1/input":
			inputSubmitted = true
			json.NewEncoder(w).Encode(map[string]int{"bytes_written": 12})
		case "/api/v1/agent/nudge":
			nudgeCalled = true
			json.NewEncoder(w).Encode(coopapi.NudgeResponse{Delivered: true})
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("shutdown was called on a transient connectivity error — should sleep in-place + recover")
	}
	if !dismissed {
		t.Error("error screen was not dismissed (no SendKeys) on connectivity error")
	}
	if !inputSubmitted {
		t.Error("raw PTY input was not submitted — recovery from the stuck error state did not happen (ka-0000006423)")
	}
	if nudgeCalled {
		t.Error("plain nudge was used — it is refused in the error state; recovery must use raw /api/v1/input")
	}
}

func TestMonitorExit_NoInternet_ContextCancelled(t *testing.T) {
	// Context cancellation during the connectivity backoff sleep must return
	// cleanly (no hang, no shutdown).
	noInternetBackoffOverride = 5 * time.Second // long enough that cancel fires first
	monitorSleepOverride = 10 * time.Millisecond
	t.Cleanup(func() { noInternetBackoffOverride = 0; monitorSleepOverride = 0; noInternetStreak = 0 })

	shutdownCalled := false
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			callCount++
			if callCount <= 3 {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			} else {
				json.NewEncoder(w).Encode(coopapi.AgentState{
					State:         "working",
					ErrorCategory: "no_internet",
					LastMessage:   "API Error: connection refused",
				})
			}
		case "/api/v1/input/keys":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- monitorExit(ctx, coop, nil, noRequestBaseline) }()

	// Let it hit the no_internet branch, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("monitorExit did not return after context cancellation")
	}
	if shutdownCalled {
		t.Error("shutdown should not be called on context cancellation during connectivity sleep")
	}
}

func TestNoInternetBackoff_ExponentialAndReset(t *testing.T) {
	t.Cleanup(func() { noInternetStreak = 0 })
	noInternetStreak = 0

	// First hit: equal-jitter around base (5s) → [2.5s, 5s].
	d1 := noInternetBackoff()
	if d1 < noInternetBackoffBase/2 || d1 > noInternetBackoffBase {
		t.Errorf("first connectivity backoff %v outside [%v, %v]", d1, noInternetBackoffBase/2, noInternetBackoffBase)
	}

	// Streak grows; values stay within the cap.
	for range 10 {
		d := noInternetBackoff()
		if d > noInternetBackoffCap {
			t.Errorf("connectivity backoff %v exceeded cap %v", d, noInternetBackoffCap)
		}
	}

	// Recovery resets the streak (monitorExit does this on state=working), so
	// the next hit is back to the base window.
	noInternetStreak = 0
	dReset := noInternetBackoff()
	if dReset < noInternetBackoffBase/2 || dReset > noInternetBackoffBase {
		t.Errorf("post-reset backoff %v outside base window [%v, %v]", dReset, noInternetBackoffBase/2, noInternetBackoffBase)
	}
}

func TestNoInternetBackoff_Override(t *testing.T) {
	noInternetBackoffOverride = 50 * time.Millisecond
	t.Cleanup(func() { noInternetBackoffOverride = 0 })

	if d := noInternetBackoff(); d != 50*time.Millisecond {
		t.Errorf("override not applied: got %v", d)
	}
}

func TestIsSessionAlive_ActiveAgent(t *testing.T) {
	sessionAliveGrace = 100 * time.Millisecond
	sessionAlivePoll = 10 * time.Millisecond
	t.Cleanup(func() { sessionAliveGrace = 0; sessionAlivePoll = 0 })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx := context.Background()

	if !isSessionAlive(ctx, coop) {
		t.Fatal("expected isSessionAlive to return true for idle agent")
	}
}

func TestIsSessionAlive_ZombieAgent(t *testing.T) {
	// Simulates the zombie case: agent stuck in "starting" state because
	// credentials were missing. isSessionAlive should return false after
	// the grace period, causing coop shutdown.
	sessionAliveGrace = 100 * time.Millisecond
	sessionAlivePoll = 10 * time.Millisecond
	t.Cleanup(func() { sessionAliveGrace = 0; sessionAlivePoll = 0 })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(coopapi.AgentState{State: "starting"})
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx := context.Background()

	if isSessionAlive(ctx, coop) {
		t.Fatal("expected isSessionAlive to return false for zombie agent stuck in starting state")
	}
}

func TestIsSessionAlive_ExitedAgent(t *testing.T) {
	sessionAliveGrace = 100 * time.Millisecond
	sessionAlivePoll = 10 * time.Millisecond
	t.Cleanup(func() { sessionAliveGrace = 0; sessionAlivePoll = 0 })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(coopapi.AgentState{State: "exited"})
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx := context.Background()

	if isSessionAlive(ctx, coop) {
		t.Fatal("expected isSessionAlive to return false for exited agent")
	}
}

func TestIsSessionAlive_CoopGone(t *testing.T) {
	sessionAliveGrace = 100 * time.Millisecond
	sessionAlivePoll = 10 * time.Millisecond
	t.Cleanup(func() { sessionAliveGrace = 0; sessionAlivePoll = 0 })

	// Use a closed server to simulate coop being unavailable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	coop := coopapi.New(srv.URL)
	ctx := context.Background()

	if isSessionAlive(ctx, coop) {
		t.Fatal("expected isSessionAlive to return false when coop is gone")
	}
}

func TestIsSessionAlive_ContextCancelled(t *testing.T) {
	sessionAliveGrace = 5 * time.Second
	sessionAlivePoll = 10 * time.Millisecond
	t.Cleanup(func() { sessionAliveGrace = 0; sessionAlivePoll = 0 })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(coopapi.AgentState{State: "starting"})
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	if isSessionAlive(ctx, coop) {
		t.Fatal("expected isSessionAlive to return false when context is cancelled")
	}
}

func TestIsSessionAlive_LateStart(t *testing.T) {
	// Agent transitions from starting → idle during the grace period.
	sessionAliveGrace = 500 * time.Millisecond
	sessionAlivePoll = 10 * time.Millisecond
	t.Cleanup(func() { sessionAliveGrace = 0; sessionAlivePoll = 0 })

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 2 {
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "starting"})
		} else {
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx := context.Background()

	if !isSessionAlive(ctx, coop) {
		t.Fatal("expected isSessionAlive to return true when agent starts late within grace period")
	}
}

// A credential-not-ready-at-spawn agent reaches "idle" but its screen shows a
// persistent "Not logged in" banner. isSessionAlive must NOT treat it as alive
// (ka-0000007394) — it keeps polling and, since the banner persists through the
// grace window, returns false so coop is shut down instead of nudging a dead
// agent.
func TestIsSessionAlive_IdleWithLoginBanner(t *testing.T) {
	sessionAliveGrace = 100 * time.Millisecond
	sessionAlivePoll = 10 * time.Millisecond
	t.Cleanup(func() { sessionAliveGrace = 0; sessionAlivePoll = 0 })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/screen/text":
			_, _ = w.Write([]byte("Not logged in · Please run /login"))
		default:
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx := context.Background()

	if isSessionAlive(ctx, coop) {
		t.Fatal("expected isSessionAlive to return false for idle agent stuck on a login banner")
	}
}

// A transient login banner that clears within the grace window must NOT block:
// the next poll (clean screen) returns true. Guards against nuking a recoverable
// credential-switch blip (the 25/26-recover calibration, ka-0000006636).
func TestIsSessionAlive_IdleLoginBannerClears(t *testing.T) {
	sessionAliveGrace = 5 * time.Second
	sessionAlivePoll = 10 * time.Millisecond
	t.Cleanup(func() { sessionAliveGrace = 0; sessionAlivePoll = 0 })

	screenCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/screen/text":
			screenCalls++
			if screenCalls <= 2 {
				_, _ = w.Write([]byte("Not logged in · Please run /login"))
			} else {
				_, _ = w.Write([]byte("> ready"))
			}
		default:
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx := context.Background()

	if !isSessionAlive(ctx, coop) {
		t.Fatal("expected isSessionAlive to return true once the transient login banner clears")
	}
}

// startup_login prompt subtype is recognized even if the screen fetch is empty.
func TestSessionLoginBlocked_StartupLoginSubtype(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("")) // empty screen
	}))
	defer srv.Close()
	coop := coopapi.New(srv.URL)
	ctx := context.Background()

	state := &coopapi.AgentState{State: "prompt", Prompt: &coopapi.AgentPrompt{Type: "setup", Subtype: "startup_login"}}
	if !sessionLoginBlocked(ctx, coop, state) {
		t.Fatal("expected sessionLoginBlocked=true for startup_login prompt subtype")
	}

	clean := &coopapi.AgentState{State: "idle"}
	if sessionLoginBlocked(ctx, coop, clean) {
		t.Fatal("expected sessionLoginBlocked=false for a clean idle state with empty screen")
	}
}

func TestMonitorExit_ZombieWatchdog(t *testing.T) {
	// Simulates a zombie pod: agent stays "idle" forever after nudge delivery.
	// The watchdog should trigger coop shutdown after the deadline.
	zombieWatchdogTimeout = 100 * time.Millisecond
	monitorSleepOverride = 10 * time.Millisecond
	t.Cleanup(func() { zombieWatchdogTimeout = 0; monitorSleepOverride = 0 })

	shutdownCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			// Agent stays idle forever — never "working".
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := monitorExit(ctx, coop, nil, noRequestBaseline)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !shutdownCalled {
		t.Error("zombie watchdog should have triggered coop shutdown")
	}
}

func TestMonitorExit_ZombieWatchdog_WorkingResetsWatchdog(t *testing.T) {
	// Once the agent reaches "working" at least once, the watchdog is satisfied.
	zombieWatchdogTimeout = 100 * time.Millisecond
	monitorSleepOverride = 10 * time.Millisecond
	t.Cleanup(func() { zombieWatchdogTimeout = 0; monitorSleepOverride = 0 })

	shutdownCalled := false
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			callCount++
			if callCount == 1 {
				// First poll: working.
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			} else {
				// Then idle for the rest — but watchdog should not fire.
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
			}
		case "/api/v1/shutdown":
			shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	// Short context — just long enough to confirm watchdog does NOT fire.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_ = monitorExit(ctx, coop, nil, noRequestBaseline)
	if shutdownCalled {
		t.Error("watchdog should NOT fire when agent has been working at least once")
	}
}

// zombieWatchdogFires runs monitorExit against an agent that stays "idle" at
// the given request_count and reports whether the watchdog shut coop down.
func zombieWatchdogFires(t *testing.T, requestCount, baseline uint64) bool {
	t.Helper()
	zombieWatchdogTimeout = 100 * time.Millisecond
	monitorSleepOverride = 10 * time.Millisecond
	t.Cleanup(func() { zombieWatchdogTimeout = 0; monitorSleepOverride = 0 })

	var shutdownCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle", RequestCount: requestCount})
		case "/api/v1/shutdown":
			shutdownCalled.Store(true)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = monitorExit(ctx, coopapi.New(srv.URL), nil, baseline)
	return shutdownCalled.Load()
}

func TestMonitorExit_ZombieWatchdog_RequestCountPastBaselineCountsAsWorked(t *testing.T) {
	// A claudeless turn landed during injectPrompt (count 3 > baseline 2) though
	// no poll ever saw "working": the watchdog must not fire (gd-acTuM7LA75).
	if zombieWatchdogFires(t, 3, 2) {
		t.Error("watchdog should NOT fire once request_count advanced past the pre-prompt baseline")
	}
}

func TestMonitorExit_ZombieWatchdog_ResumedCountAtBaselineStillFires(t *testing.T) {
	// A resumed coop starts above zero; a count still at its own pre-prompt
	// baseline is no turn, so a stuck resumed session is still caught.
	if !zombieWatchdogFires(t, 7, 7) {
		t.Error("watchdog should fire when request_count never moved past the baseline")
	}
}

func TestMonitorExit_AgentExited(t *testing.T) {
	// Counted per path. The original single callCount was incremented by the
	// shutdown request too, so it could not be asserted on without encoding
	// that off-by-one; atomic because the handler runs on the server's
	// goroutine.
	var agentPolls, shutdowns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			// Working for the first two polls, exited on the third.
			if agentPolls.Add(1) <= 2 {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
			} else {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "exited"})
			}
		case "/api/v1/shutdown":
			shutdowns.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The comment that used to sit here said the sleeps were reduced for test
	// speed. They were not: monitorSleepOverride was never set, so this ran the
	// real 10s initial sleep plus 5s per poll and took 25.01s -- 30% of the
	// whole cmd/gb package (83.7s), which is the package that bounds the
	// controller `test` step's wall clock. Every other monitorExit test in this
	// package already sets the override; this one was simply missed.
	//
	// Nothing is given up by overriding here. Unlike the create/claim window in
	// internal/bridge, monitorExit has no duration-margin logic for a long sleep
	// to exercise -- it sleeps, polls, and acts on the state it reads, and the
	// httptest server answers instantly at any interval. The production values
	// are asserted directly below instead, which is the part a drift would
	// actually break.
	if monitorExitInitialSleep != 10*time.Second || monitorExitPollSleep != 5*time.Second {
		t.Errorf("production poll timings changed: initial=%v poll=%v; update the deploy docs before relaxing this",
			monitorExitInitialSleep, monitorExitPollSleep)
	}
	monitorSleepOverride = 10 * time.Millisecond
	t.Cleanup(func() { monitorSleepOverride = 0 })

	err := monitorExit(ctx, coop, nil, noRequestBaseline)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The agent reports "working" twice and then "exited"; monitorExit must
	// return on that third poll rather than keep polling, and must have asked
	// coop to shut down exactly once. Without these the test passes on a
	// monitorExit that never polls at all.
	if got := agentPolls.Load(); got != 3 {
		t.Errorf("expected monitorExit to stop at the first exited poll (3 polls), got %d", got)
	}
	if got := shutdowns.Load(); got != 1 {
		t.Errorf("expected exactly 1 shutdown request, got %d", got)
	}
}

func TestIsTerminalHardAuth403(t *testing.T) {
	cases := []struct {
		name  string
		state *coopapi.AgentState
		want  bool
	}{
		{"nil", nil, false},
		{
			"terminal park",
			&coopapi.AgentState{State: "parked", ParkedReason: "terminal_hard_auth_403: key not allowed to access model after 3 consecutive turns"},
			true,
		},
		{
			"bare prefix",
			&coopapi.AgentState{State: "parked", ParkedReason: "terminal_hard_auth_403"},
			true,
		},
		{
			// Recoverable rate-limit park must NOT trip the terminal close.
			"rate-limited park",
			&coopapi.AgentState{State: "parked", ParkedReason: "all_profiles_rate_limited"},
			false,
		},
		{
			// Right reason text but not actually parked → not terminal.
			"working with 403 message",
			&coopapi.AgentState{State: "working", LastMessage: "403 key not allowed to access model"},
			false,
		},
		{
			"parked no reason",
			&coopapi.AgentState{State: "parked"},
			false,
		},
		{
			"idle",
			&coopapi.AgentState{State: "idle"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTerminalHardAuth403(tc.state); got != tc.want {
				t.Fatalf("isTerminalHardAuth403(%+v) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestIsTerminalNotLoggedIn(t *testing.T) {
	cases := []struct {
		name  string
		state *coopapi.AgentState
		want  bool
	}{
		{"nil", nil, false},
		{
			"terminal park",
			&coopapi.AgentState{State: "parked", ParkedReason: "terminal_login_required: not logged in after 3 consecutive turns (credential not ready at spawn)"},
			true,
		},
		{
			"bare prefix",
			&coopapi.AgentState{State: "parked", ParkedReason: "terminal_login_required"},
			true,
		},
		{
			// Recoverable rate-limit park must NOT trip the terminal close.
			"rate-limited park",
			&coopapi.AgentState{State: "parked", ParkedReason: "all_profiles_rate_limited"},
			false,
		},
		{
			// A terminal hard-auth-403 park is a DIFFERENT terminal condition —
			// it must not be misread as login-required (and vice versa).
			"hard-auth-403 park",
			&coopapi.AgentState{State: "parked", ParkedReason: "terminal_hard_auth_403: key not allowed to access model after 3 consecutive turns"},
			false,
		},
		{
			// Right reason text but not actually parked → not terminal (the
			// transient-recovered "Not logged in" during a credential switch).
			"idle with not-logged-in message",
			&coopapi.AgentState{State: "idle", LastMessage: "Not logged in · Please run /login"},
			false,
		},
		{
			"parked no reason",
			&coopapi.AgentState{State: "parked"},
			false,
		},
		{
			"idle",
			&coopapi.AgentState{State: "idle"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTerminalNotLoggedIn(tc.state); got != tc.want {
				t.Fatalf("isTerminalNotLoggedIn(%+v) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}
