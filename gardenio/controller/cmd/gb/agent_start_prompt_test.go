package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/coopapi"
)

func TestResolveStartupPrompt(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name      string
		scenario  string // TOML body (always written so sidecar lookup has a base path)
		sidecar   string // sidecar body; if "", no sidecar file is created
		empty     bool   // pass empty scenarioPath to resolveStartupPrompt
		writeSide bool   // whether to write the sidecar (allows empty-body sidecars)
		want      string
	}{
		{
			name:  "empty path skips",
			empty: true,
			want:  "",
		},
		{
			name: "missing sidecar returns empty",
			scenario: `[claude]
session_id = "x"
`,
			want: "",
		},
		{
			name: "with sidecar prompt",
			scenario: `[claude]
session_id = "x"
`,
			sidecar:   "Begin the test.\n",
			writeSide: true,
			want:      "Begin the test.",
		},
		{
			name: "empty sidecar returns empty",
			scenario: `[claude]
session_id = "x"
`,
			sidecar:   "",
			writeSide: true,
			want:      "",
		},
		{
			name: "sidecar trims trailing whitespace only",
			scenario: `[claude]
session_id = "x"
`,
			sidecar:   "  Hello world.  \n\n",
			writeSide: true,
			want:      "  Hello world.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if !tc.empty {
				path = filepath.Join(dir, sanitize(tc.name)+".toml")
				if err := os.WriteFile(path, []byte(tc.scenario), 0644); err != nil {
					t.Fatal(err)
				}
				if tc.writeSide {
					if err := os.WriteFile(startupPromptSidecarPath(path), []byte(tc.sidecar), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := resolveStartupPrompt(path)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func sanitize(name string) string {
	return strings.ReplaceAll(name, " ", "_")
}

// TestBypassLocalStartup_DismissesBypassPermissions verifies that the local
// startup-bypass actively dismisses the claudeless `bypass_permissions`
// setup dialog (which coop's groom does NOT auto-dismiss) and then returns
// once the agent reaches `idle`. This is the regression test for
// gd-0Fq9XRQcUu: without the bypass, the local agent stalls in `prompt`
// state and `injectLocalStartupPrompt` times out.
func TestBypassLocalStartup_DismissesBypassPermissions(t *testing.T) {
	// Speed the poll loop up so the test runs in milliseconds.
	prev := bypassLocalPollInterval
	bypassLocalPollInterval = 10 * time.Millisecond
	defer func() { bypassLocalPollInterval = prev }()

	var (
		mu         sync.Mutex
		stateCalls int32
		sentKeys   [][]string
		dismissed  bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent":
			atomic.AddInt32(&stateCalls, 1)
			mu.Lock()
			d := dismissed
			mu.Unlock()
			if !d {
				// Stuck on the bypass_permissions setup dialog.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"state": "prompt",
					"prompt": map[string]string{
						"type":    "setup",
						"subtype": "bypass_permissions",
					},
				})
				return
			}
			// After dismissal: idle.
			_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/input/keys":
			var body struct {
				Keys []string `json:"keys"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			sentKeys = append(sentKeys, body.Keys)
			dismissed = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bypassLocalStartup(ctx, c)

	mu.Lock()
	defer mu.Unlock()
	if len(sentKeys) != 1 {
		t.Fatalf("expected 1 SendKeys call, got %d (%v)", len(sentKeys), sentKeys)
	}
	got := sentKeys[0]
	want := []string{"Down", "Return"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("SendKeys got %v, want %v", got, want)
	}
	if atomic.LoadInt32(&stateCalls) < 2 {
		t.Errorf("expected at least 2 state polls (before+after dismiss), got %d", stateCalls)
	}
}

// TestBypassLocalStartup_ReturnsImmediatelyWhenIdle verifies the early-exit
// path: if coop is already idle, no SendKeys is sent.
func TestBypassLocalStartup_ReturnsImmediatelyWhenIdle(t *testing.T) {
	prev := bypassLocalPollInterval
	bypassLocalPollInterval = 10 * time.Millisecond
	defer func() { bypassLocalPollInterval = prev }()

	var sentKeys int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent":
			_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/input/keys":
			atomic.AddInt32(&sentKeys, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	bypassLocalStartup(ctx, c)

	if atomic.LoadInt32(&sentKeys) != 0 {
		t.Errorf("expected no SendKeys calls when idle, got %d", sentKeys)
	}
}

// Sanity: every local-*.toml scenario in gardenio/tests/e2e/claudeless/ must
// have a non-empty sidecar local-*.startup-prompt.txt so runLocal can drive
// the scenario's first phase. The sidecar lives outside the TOML because
// claudeless rejects unknown top-level keys (e.g. [test_scenario]).
func TestAllLocalScenariosHaveStartupPrompt(t *testing.T) {
	paths, err := filepath.Glob("../../../tests/e2e/claudeless/local-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Skip("local-*.toml fixtures not found from this test cwd")
	}
	for _, p := range paths {
		if got := resolveStartupPrompt(p); got == "" {
			t.Errorf("%s: missing or empty sidecar %s", p, startupPromptSidecarPath(p))
		}
	}
}

func TestResolveNudges(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name      string
		sidecar   string
		writeSide bool
		empty     bool
		want      []string
	}{
		{
			name:  "empty path returns nil",
			empty: true,
			want:  nil,
		},
		{
			name: "missing sidecar returns nil",
			want: nil,
		},
		{
			name:      "empty sidecar returns nil",
			sidecar:   "",
			writeSide: true,
			want:      nil,
		},
		{
			name:      "comments-only sidecar returns nil",
			sidecar:   "# only comments\n#another\n\n",
			writeSide: true,
			want:      nil,
		},
		{
			name:      "single line",
			sidecar:   "Now claim it.\n",
			writeSide: true,
			want:      []string{"Now claim it."},
		},
		{
			name:      "multiple lines skip blanks and comments",
			sidecar:   "# first phase already fired from startup\nNow claim it.\n\nNow do the work.\n# trailing comment\nNow close it.\n",
			writeSide: true,
			want:      []string{"Now claim it.", "Now do the work.", "Now close it."},
		},
		{
			name:      "trailing whitespace trimmed",
			sidecar:   "Now claim it.  \nNow stop.\t\n",
			writeSide: true,
			want:      []string{"Now claim it.", "Now stop."},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if !tc.empty {
				path = filepath.Join(dir, sanitize(tc.name)+".toml")
				if err := os.WriteFile(path, []byte("[claude]\nsession_id = \"x\"\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if tc.writeSide {
					if err := os.WriteFile(nudgesSidecarPath(path), []byte(tc.sidecar), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := resolveNudges(path)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d nudges %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("nudge %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestInjectLocalFollowupNudges_WaitsForTurnCompleteBetweenNudges verifies
// the multi-nudge runner injects each follow-up only after the agent has
// emitted a working→idle state transition since the previous nudge — the
// hard-idle-transition gate from decision ka-0000008116. Without this gate
// the second nudge races the first response and claudeless's `[[responses]]`
// rules fire out of order or not at all (ka-0000002034).
func TestInjectLocalFollowupNudges_WaitsForTurnCompleteBetweenNudges(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	followupNudgePollInterval = 5 * time.Millisecond
	followupNudgeMaxPolls = 200
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
	}()

	var (
		mu              sync.Mutex
		nudgesSent      []string
		nextSeqAfterPre uint64 // bumped each time a nudge fires; transition with this seq
		// gateOpen indicates the synthetic agent has "completed" a turn —
		// catchup will return a working→idle transition once true, mimicking
		// claudeless emitting an assistant turn. We toggle gateOpen after
		// each nudge to model "agent takes some time, then completes".
		gateOpen    bool
		pollsAtGate int // poll count since gate flipped to false; flips true after a couple polls
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent":
			_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup"):
			mu.Lock()
			// Two polls after each delivered nudge, flip gate open and
			// return a transition with seq > pre-snapshot.
			pollsAtGate++
			if pollsAtGate >= 2 {
				gateOpen = true
			}
			resp := coopapi.EventsCatchup{}
			if gateOpen {
				entry := []byte(formatTransition(nextSeqAfterPre+1, "working", "idle"))
				resp.StateEvents = append(resp.StateEvents, entry)
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/nudge":
			var body struct {
				Message string `json:"message"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			nudgesSent = append(nudgesSent, body.Message)
			// Reset gate so the next nudge has to wait for a fresh
			// transition above the new pre-snapshot.
			gateOpen = false
			pollsAtGate = 0
			nextSeqAfterPre += 10
			mu.Unlock()
			_, _ = w.Write([]byte(`{"delivered": true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	injectLocalFollowupNudges(ctx, c, []string{"one", "two", "three"})

	mu.Lock()
	defer mu.Unlock()
	if len(nudgesSent) != 3 {
		t.Fatalf("expected 3 nudges, got %d: %v", len(nudgesSent), nudgesSent)
	}
	for i, want := range []string{"one", "two", "three"} {
		if nudgesSent[i] != want {
			t.Errorf("nudge %d: got %q, want %q", i, nudgesSent[i], want)
		}
	}
}

// formatTransition is a small helper so tests can hand-build a
// TransitionEntry-shaped JSON without importing coop's event_log types.
// The coopapi.EventsCatchup field is []json.RawMessage so we just inject
// pre-encoded bytes.
func formatTransition(seq uint64, prev, next string) string {
	b, _ := json.Marshal(map[string]any{
		"seq":          seq,
		"prev":         prev,
		"next":         next,
		"cause":        "test",
		"timestamp_ms": 0,
	})
	return string(b)
}

// formatHookEvent is a small helper that returns a HookEntry-shaped JSON
// bytes for tests injecting Stop / PostToolUse / etc. into the events
// catchup mock. It mirrors the real coop FIFO envelope written by the hook
// scripts in coop/crates/cli/src/driver/claude/hooks.rs:
//
//	{"event":"<lowercased>","data":{"hook_event_name":"<EventName>", ...}}
//
// `event` is the lower-cased coop-controlled marker (e.g. "stop",
// "post_tool_use"); `hook_event_name` is the upstream Claude/claudeless name
// nested under `data`. Pinning this shape in tests keeps the gate honest —
// the prior helper put `hook_event_name` at the top level, which is not what
// production coop ever writes (ka-0000002926).
func formatHookEvent(hookSeq uint64, eventName string) string {
	envelopeEvent := envelopeEventForHookName(eventName)
	b, _ := json.Marshal(map[string]any{
		"hook_seq":     hookSeq,
		"timestamp_ms": 0,
		"json": map[string]any{
			"event": envelopeEvent,
			"data": map[string]any{
				"hook_event_name": eventName,
			},
		},
	})
	return string(b)
}

// envelopeEventForHookName converts Claude hook event names ("Stop",
// "PostToolUse", ...) to the lower-cased outer-envelope `event` value coop's
// hook scripts write. Mirrors the inverse mapping in
// coop/crates/cli/src/driver/hook_recv.rs::parse_hook_line.
func envelopeEventForHookName(name string) string {
	switch name {
	case "Stop":
		return "stop"
	case "PostToolUse":
		return "post_tool_use"
	case "PreToolUse":
		return "pre_tool_use"
	case "SessionStart":
		return "start"
	case "SessionEnd":
		return "session_end"
	case "Notification":
		return "notification"
	case "UserPromptSubmit":
		return "user_prompt_submit"
	default:
		return ""
	}
}

// TestWaitForTurnComplete_StaleIdleDoesNotTrigger pins the ka-0000002034
// regression: even when state == "idle" continuously, the new gate must
// not return true until a fresh working→idle transition (or Stop hook)
// arrives with seq > preStateSeq / hook_seq > preHookSeq. This is the
// failure mode the prior `waitForIdle` poll silently allowed and which
// caused MR !2604 to merge-but-not-fix the bug.
func TestWaitForTurnComplete_StaleIdleDoesNotTrigger(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	followupNudgePollInterval = 2 * time.Millisecond
	followupNudgeMaxPolls = 30 // ~60ms total; bound tight
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
	}()

	// Catchup returns only a STALE transition (seq <= preStateSeq) — proves
	// the gate filters by seq, not just by transition presence.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup") {
			resp := coopapi.EventsCatchup{}
			resp.StateEvents = append(resp.StateEvents, []byte(formatTransition(5, "working", "idle")))
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// preStateSeq=10 is HIGHER than the stale transition's seq=5, so the
	// gate must NOT open.
	if waitForTurnComplete(ctx, c, 10, 0) {
		t.Fatal("expected waitForTurnComplete to time out when only stale transitions are present")
	}
}

// TestWaitForTurnComplete_StopHookOpensGate verifies the Stop-hook
// signal: even if no state transition arrives, a Stop hook event with
// hook_seq > preHookSeq opens the gate. This is the belt-and-suspenders
// signal in case claudeless emits the hook but not a visible transition.
func TestWaitForTurnComplete_StopHookOpensGate(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	followupNudgePollInterval = 2 * time.Millisecond
	followupNudgeMaxPolls = 100
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
	}()

	var polled int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup") {
			resp := coopapi.EventsCatchup{}
			// Don't return Stop until after a couple polls so we exercise the
			// poll loop, not a same-tick short-circuit.
			if atomic.AddInt32(&polled, 1) >= 2 {
				resp.HookEvents = append(resp.HookEvents, []byte(formatHookEvent(11, "Stop")))
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !waitForTurnComplete(ctx, c, 0, 10) {
		t.Fatal("expected Stop hook event with hook_seq=11 > preHookSeq=10 to open the gate")
	}
}

// TestIsStopHook_AllShapes pins the wire-format matching contract for the
// Stop-hook signal. Coop's FIFO scripts emit
// `{"event":"stop","data":{"hook_event_name":"Stop", ...}}` — but older /
// synthetic payloads sometimes had `hook_event_name` at the top level.
// isStopHook MUST accept both, and MUST NOT match non-Stop envelopes.
// Resolves ka-0000002926: the prior top-level-only check silently missed
// every real Stop hook in production.
func TestIsStopHook_AllShapes(t *testing.T) {
	cases := []struct {
		name string
		json map[string]any
		want bool
	}{
		{
			name: "production envelope (event=stop, hook_event_name under data)",
			json: map[string]any{
				"event": "stop",
				"data":  map[string]any{"hook_event_name": "Stop", "stop_hook_active": false},
			},
			want: true,
		},
		{
			name: "envelope event only (no data)",
			json: map[string]any{"event": "stop"},
			want: true,
		},
		{
			name: "legacy top-level hook_event_name",
			json: map[string]any{"hook_event_name": "Stop"},
			want: true,
		},
		{
			name: "data-only hook_event_name (no envelope event)",
			json: map[string]any{
				"data": map[string]any{"hook_event_name": "Stop"},
			},
			want: true,
		},
		{
			name: "PostToolUse envelope must not match",
			json: map[string]any{
				"event": "post_tool_use",
				"data":  map[string]any{"hook_event_name": "PostToolUse"},
			},
			want: false,
		},
		{
			name: "empty payload",
			json: map[string]any{},
			want: false,
		},
		{
			name: "nil payload",
			json: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStopHook(tc.json); got != tc.want {
				t.Fatalf("isStopHook(%v) = %v, want %v", tc.json, got, tc.want)
			}
		})
	}
}

// TestWaitForTurnComplete_NonStopHookDoesNotTrigger asserts that non-Stop
// hook events (PostToolUse, PreToolUse, etc.) — even with hook_seq above
// preHookSeq — do NOT open the gate. Only Stop is the turn-complete signal.
func TestWaitForTurnComplete_NonStopHookDoesNotTrigger(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	followupNudgePollInterval = 2 * time.Millisecond
	followupNudgeMaxPolls = 20
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup") {
			resp := coopapi.EventsCatchup{}
			resp.HookEvents = append(resp.HookEvents, []byte(formatHookEvent(11, "PostToolUse")))
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	if waitForTurnComplete(ctx, c, 0, 10) {
		t.Fatal("expected non-Stop hook (PostToolUse) to NOT open the gate")
	}
}

// TestInjectLocalFollowupNudges_TimeoutWarnsButContinues verifies that
// when no turn-complete signal arrives within the budget, the runner logs
// a warning but still delivers each nudge and proceeds to the next. The
// scenario's own assertions stay authoritative for end-of-run pass/fail.
func TestInjectLocalFollowupNudges_TimeoutWarnsButContinues(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	followupNudgePollInterval = 2 * time.Millisecond
	followupNudgeMaxPolls = 10
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
	}()

	var (
		mu         sync.Mutex
		nudgesSent []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup"):
			_ = json.NewEncoder(w).Encode(coopapi.EventsCatchup{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/nudge":
			var body struct {
				Message string `json:"message"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			nudgesSent = append(nudgesSent, body.Message)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"delivered": true}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	injectLocalFollowupNudges(ctx, c, []string{"a", "b"})

	mu.Lock()
	defer mu.Unlock()
	if len(nudgesSent) != 2 {
		t.Fatalf("expected 2 nudges (warnings notwithstanding), got %d: %v", len(nudgesSent), nudgesSent)
	}
}

// TestInjectLocalFollowupNudges_NoNudgesIsNoOp verifies the empty-list
// case so callers can pass resolveNudges(...) result unconditionally.
func TestInjectLocalFollowupNudges_NoNudgesIsNoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	injectLocalFollowupNudges(ctx, c, nil)
}

// TestRunLocalNudgeSequence_StartupThenFollowups verifies the wrapper
// delivers the startup prompt and then walks the follow-up nudges in
// order. This is the goroutine body extracted from runLocal so the
// multi-nudge orchestration is testable without spinning up the full
// runLocal integration loop. Patch coverage for agent_start.go (ka-0000002034).
func TestRunLocalNudgeSequence_StartupThenFollowups(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	prevBypass := bypassLocalPollInterval
	followupNudgePollInterval = 5 * time.Millisecond
	followupNudgeMaxPolls = 200
	bypassLocalPollInterval = 5 * time.Millisecond
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
		bypassLocalPollInterval = prevBypass
	}()

	var (
		mu         sync.Mutex
		nudgesSent []string
	)

	var nudgeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent":
			_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup"):
			resp := coopapi.EventsCatchup{}
			// Always return a fresh working→idle transition with a high
			// seq number so waitForTurnComplete opens the gate
			// immediately. Seq grows with each call so it always exceeds
			// the prior nudge's pre-snapshot.
			seq := 1000 + uint64(atomic.AddInt32(&nudgeCount, 1))
			resp.StateEvents = append(resp.StateEvents, []byte(formatTransition(seq, "working", "idle")))
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/input/keys":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/nudge":
			var body struct {
				Message string `json:"message"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			nudgesSent = append(nudgesSent, body.Message)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"delivered": true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runLocalNudgeSequence(ctx, c, "startup", []string{"phase2", "phase3"}, "scenario.nudges.txt")

	mu.Lock()
	defer mu.Unlock()
	want := []string{"startup", "phase2", "phase3"}
	if len(nudgesSent) != len(want) {
		t.Fatalf("expected %d nudges, got %d: %v", len(want), len(nudgesSent), nudgesSent)
	}
	for i, w := range want {
		if nudgesSent[i] != w {
			t.Errorf("nudge %d: got %q, want %q", i, nudgesSent[i], w)
		}
	}
}

// TestRunLocalNudgeSequence_NoFollowupsIsNoOp covers the single-shot
// scenario path: startup-only, no sidecar — runLocalNudgeSequence sends
// exactly the startup prompt.
func TestRunLocalNudgeSequence_NoFollowupsIsNoOp(t *testing.T) {
	prevBypass := bypassLocalPollInterval
	bypassLocalPollInterval = 5 * time.Millisecond
	defer func() { bypassLocalPollInterval = prevBypass }()

	var (
		mu         sync.Mutex
		nudgesSent []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent":
			_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/input/keys":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/nudge":
			var body struct {
				Message string `json:"message"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			nudgesSent = append(nudgesSent, body.Message)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"delivered": true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runLocalNudgeSequence(ctx, c, "startup-only", nil, "")

	mu.Lock()
	defer mu.Unlock()
	if len(nudgesSent) != 1 || nudgesSent[0] != "startup-only" {
		t.Errorf("expected exactly one nudge 'startup-only', got %v", nudgesSent)
	}
}

// TestRunLocalNudgeSequence_StartupFailureSkipsFollowups verifies that
// when the startup prompt cannot be delivered (canceled ctx), the
// follow-up loop is not entered.
func TestRunLocalNudgeSequence_StartupFailureSkipsFollowups(t *testing.T) {
	prevBypass := bypassLocalPollInterval
	bypassLocalPollInterval = 5 * time.Millisecond
	defer func() { bypassLocalPollInterval = prevBypass }()

	var nudgeCalls int32

	// State endpoint always returns prompt/setup so waitForReady never
	// succeeds; deliverNudge is never reached.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "prompt"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/nudge":
			atomic.AddInt32(&nudgeCalls, 1)
			_, _ = w.Write([]byte(`{"delivered": true}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	runLocalNudgeSequence(ctx, c, "startup", []string{"phase2"}, "scenario.nudges.txt")

	if got := atomic.LoadInt32(&nudgeCalls); got != 0 {
		t.Errorf("expected no nudge calls when startup fails, got %d", got)
	}
}

// TestRequestScenarioShutdown_HappyPath verifies the scenario-mode self-
// exit handshake from ka-0000003566: once the final nudge's turn completes
// (a fresh working→idle transition with seq > the pre-snapshot), the runner
// POSTs /api/v1/shutdown so coop cancels and runLocal exits 0 instead of
// hanging until the 90s harness SIGTERM.
func TestRequestScenarioShutdown_HappyPath(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	followupNudgePollInterval = 5 * time.Millisecond
	followupNudgeMaxPolls = 200
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
	}()

	var (
		mu             sync.Mutex
		shutdownCalled bool
		polls          int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup"):
			n := atomic.AddInt32(&polls, 1)
			resp := coopapi.EventsCatchup{}
			// First call is the pre-snapshot (observedSeqs) — return
			// nothing so it captures seq=0. Subsequent waitForTurnComplete
			// polls see a fresh working→idle transition with seq>0 and
			// open the gate.
			if n >= 2 {
				resp.StateEvents = append(resp.StateEvents, []byte(formatTransition(42, "working", "idle")))
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/shutdown":
			mu.Lock()
			shutdownCalled = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	requestScenarioShutdown(ctx, c)

	mu.Lock()
	defer mu.Unlock()
	if !shutdownCalled {
		t.Fatal("expected POST /api/v1/shutdown after the final turn completed")
	}
}

// TestRequestScenarioShutdown_ShutsDownEvenOnTurnTimeout asserts the
// belt-and-suspenders behaviour: if waitForTurnComplete times out (e.g.
// claudeless wedged before emitting working→idle for the last nudge), the
// runner still POSTs /api/v1/shutdown so coop is torn down instead of
// hanging until the 90s harness SIGTERM. The agent's own assertions stay
// authoritative for end-of-run pass/fail.
func TestRequestScenarioShutdown_ShutsDownEvenOnTurnTimeout(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	followupNudgePollInterval = 2 * time.Millisecond
	followupNudgeMaxPolls = 10 // ~20ms total
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
	}()

	var (
		mu             sync.Mutex
		shutdownCalled bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup"):
			// Never emit a working→idle transition; gate never opens.
			_ = json.NewEncoder(w).Encode(coopapi.EventsCatchup{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/shutdown":
			mu.Lock()
			shutdownCalled = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	requestScenarioShutdown(ctx, c)

	mu.Lock()
	defer mu.Unlock()
	if !shutdownCalled {
		t.Fatal("expected POST /api/v1/shutdown even when the final turn signal does not arrive")
	}
}

// TestRequestScenarioShutdown_ShutdownErrorLogsButReturns asserts a 5xx
// from /api/v1/shutdown does not panic — coop-down-already and
// transient errors must not propagate. Best-effort by design.
func TestRequestScenarioShutdown_ShutdownErrorLogsButReturns(t *testing.T) {
	prevPoll := followupNudgePollInterval
	prevMax := followupNudgeMaxPolls
	followupNudgePollInterval = 2 * time.Millisecond
	followupNudgeMaxPolls = 5
	defer func() {
		followupNudgePollInterval = prevPoll
		followupNudgeMaxPolls = prevMax
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/events/catchup"):
			_ = json.NewEncoder(w).Encode(coopapi.EventsCatchup{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/shutdown":
			http.Error(w, "coop already shutting down", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Should not panic, should return promptly.
	done := make(chan struct{})
	go func() {
		requestScenarioShutdown(ctx, c)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("requestScenarioShutdown did not return after coop shutdown returned 500")
	}
}

// TestAllMultiPhaseLocalScenariosHaveNudges asserts the three local-mode
// scenarios with [[responses]] beyond Phase 1 ship their .nudges.txt
// sidecar. Single-shot scenarios (one response, or first response is
// driven externally by the runner) are excluded — they don't need
// follow-up nudges.
func TestAllMultiPhaseLocalScenariosHaveNudges(t *testing.T) {
	multiPhase := []string{
		"local-lifecycle",
		"local-task-preassignment",
		"local-yield-close",
	}
	for _, name := range multiPhase {
		toml := filepath.Join("../../../tests/e2e/claudeless", name+".toml")
		if _, err := os.Stat(toml); err != nil {
			t.Skipf("%s not found from this cwd: %v", toml, err)
		}
		got := resolveNudges(toml)
		if len(got) == 0 {
			t.Errorf("%s: missing or empty .nudges.txt sidecar (%s)", toml, nudgesSidecarPath(toml))
		}
	}
}

// TestClaudelessDecisionCreatesAreDedupeSafe guards against the
// gd-WTiyh6TLsn regression: `gb decision create` refuses to create a
// decision whose normalized prompt-prefix matches an existing OPEN decision
// in scope (the ka-0000003825 dedup guard). A scenario that hardcodes a
// FIXED prompt therefore fails DETERMINISTICALLY on retries once any prior
// run leaves a decision open — create returns non-zero, the scenario's
// `set -e` bash aborts before its side-channel write, and the runner reports
// "side-channel never populated" while the rest of the lifecycle still
// (misleadingly) goes green.
//
// Every claudeless scenario that invokes `gb decision create` must therefore
// be dedup-safe: either pass --allow-duplicate (this decision is a throwaway
// test fixture, exempt from dedup) or embed a per-run unique token in the
// prompt ($TAG / $$ / $(date …)), matching the gardener-trigger-decision-*
// scenarios. This scans every *.toml so a newly-authored scenario that
// reintroduces a fixed prompt is caught at build time, not on a red release.
func TestClaudelessDecisionCreatesAreDedupeSafe(t *testing.T) {
	paths, err := filepath.Glob("../../../tests/e2e/claudeless/*.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Skip("claudeless *.toml fixtures not found from this cwd")
	}
	// A per-run unique token that de-collides the prompt bucket. $$ (pid),
	// $(date …) (epoch), and $TAG (conventionally "…-$$-$(date +%s)") are the
	// idioms the scenarios already use.
	uniqueMarkers := []string{"$$", "$(date", "$TAG", "${TAG"}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		body := string(data)
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			// Only consider real invocations, not prose in the header
			// comments that document the scenario.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if !strings.Contains(line, "gb decision create") && !strings.Contains(line, "decision create --") {
				continue
			}
			if strings.Contains(line, "--allow-duplicate") {
				continue
			}
			// No --allow-duplicate: the surrounding scenario body must build a
			// per-run-unique prompt. We accept a unique marker anywhere in the
			// file body since the prompt is usually assembled a few lines above
			// the create call (PROMPT="…$TAG…").
			hasUnique := false
			for _, m := range uniqueMarkers {
				if strings.Contains(body, m) {
					hasUnique = true
					break
				}
			}
			if !hasUnique {
				t.Errorf("%s: `gb decision create` without --allow-duplicate and no per-run-unique prompt token (%v) — hardcoded prompts fail deterministically against the ka-0000003825 dedup guard on release retries (gd-WTiyh6TLsn). Add --allow-duplicate or tag the prompt with $TAG/$$/$(date).", p, uniqueMarkers)
			}
		}
	}
}
