package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/slack-go/slack"
)

// mockReactionServer creates an HTTP mux that handles Slack reactions.add
// and reactions.remove API calls, recording them in the given recorder.
func mockReactionServer(rec *reactionRecorder) *http.ServeMux {
	mux := http.NewServeMux()

	handle := func(method string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			emoji := r.FormValue("name")
			ch := r.FormValue("channel")
			ts := r.FormValue("timestamp")
			rec.record(method, emoji, slack.ItemRef{Channel: ch, Timestamp: ts})
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		}
	}

	mux.HandleFunc("/reactions.add", handle("add"))
	mux.HandleFunc("/reactions.remove", handle("remove"))
	// Catch-all for auth.test etc.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
	return mux
}

// Garden-default reaction names (no theme resolver wired, or an unresolvable
// channel): the Phase 4 vocabulary in theme.go.
var (
	gardenSeenReaction    = Garden.Reaction(ReactionSeen)
	gardenWorkingReaction = Garden.Reaction(ReactionWorking)
	gardenDoneReaction    = Garden.Reaction(ReactionDone)
	gardenFailedReaction  = Garden.Reaction(ReactionFailed)
)

// --- Unit tests for stateToReaction ---

func TestStateToEmoji(t *testing.T) {
	tests := []struct {
		state string
		want  string
	}{
		{"spawning", ReactionSeen},
		{"working", ReactionWorking},
		{"done", ReactionDone},
		{"failed", ReactionFailed},
		{"unknown", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := stateToReaction(tc.state); got != tc.want {
			t.Errorf("stateToReaction(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// --- ReactionLifecycle integration tests with mock Slack ---

// mockReactionCall records a single AddReaction or RemoveReaction call.
type mockReactionCall struct {
	Method string // "add" or "remove"
	Emoji  string
	Ref    slack.ItemRef
}

// reactionRecorder captures reaction API calls. We wrap ReactionLifecycle
// and intercept at the Slack HTTP level using httptest.
type reactionRecorder struct {
	mu    sync.Mutex
	calls []mockReactionCall
}

func (r *reactionRecorder) record(method, emoji string, ref slack.ItemRef) {
	r.mu.Lock()
	r.calls = append(r.calls, mockReactionCall{Method: method, Emoji: emoji, Ref: ref})
	r.mu.Unlock()
}

func (r *reactionRecorder) getCalls() []mockReactionCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]mockReactionCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestReactionLifecycle_FullTransition(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Use a mock Slack HTTP server to capture reaction API calls.
	rec := &reactionRecorder{}
	mux := mockReactionServer(rec)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	api := slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	rl := NewReactionLifecycle(api, logger)

	// SetTarget should store the target.
	rl.SetTarget("agent-1", "C123", "1234.5678")

	rl.mu.Lock()
	target, ok := rl.targets["agent-1"]
	rl.mu.Unlock()
	if !ok {
		t.Fatal("expected target to be set")
	}
	if target.Channel != "C123" || target.Timestamp != "1234.5678" {
		t.Errorf("unexpected target: %+v", target)
	}

	// spawning → seedling
	rl.TransitionState("agent-1", "spawning")
	calls := rec.getCalls()
	if len(calls) != 1 || calls[0].Method != "add" || calls[0].Emoji != gardenSeenReaction {
		t.Errorf("expected add(%s), got %+v", gardenSeenReaction, calls)
	}

	// working → remove seedling, add leaves
	rl.TransitionState("agent-1", "working")
	calls = rec.getCalls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls, got %d: %+v", len(calls), calls)
	}
	if calls[1].Method != "remove" || calls[1].Emoji != gardenSeenReaction {
		t.Errorf("expected remove(%s), got %+v", gardenSeenReaction, calls[1])
	}
	if calls[2].Method != "add" || calls[2].Emoji != gardenWorkingReaction {
		t.Errorf("expected add(%s), got %+v", gardenWorkingReaction, calls[2])
	}

	// done → remove leaves, add deciduous_tree + cleanup tracking
	rl.TransitionState("agent-1", "done")
	calls = rec.getCalls()
	if len(calls) != 5 {
		t.Fatalf("expected 5 calls, got %d: %+v", len(calls), calls)
	}
	if calls[3].Method != "remove" || calls[3].Emoji != gardenWorkingReaction {
		t.Errorf("expected remove(%s), got %+v", gardenWorkingReaction, calls[3])
	}
	if calls[4].Method != "add" || calls[4].Emoji != gardenDoneReaction {
		t.Errorf("expected add(%s), got %+v", gardenDoneReaction, calls[4])
	}

	// Tracking should be cleaned up.
	rl.mu.Lock()
	_, hasCurrent := rl.current["agent-1"]
	_, hasTarget := rl.targets["agent-1"]
	rl.mu.Unlock()
	if hasCurrent {
		t.Error("expected current to be cleaned up after terminal state")
	}
	if hasTarget {
		t.Error("expected target to be cleaned up after terminal state")
	}
}

func TestReactionLifecycle_NoTargetNoOp(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rec := &reactionRecorder{}
	mux := mockReactionServer(rec)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	api := slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	rl := NewReactionLifecycle(api, logger)

	// TransitionState without a target should be a no-op (no API calls).
	rl.TransitionState("unknown-agent", "working")

	calls := rec.getCalls()
	if len(calls) != 0 {
		t.Errorf("expected no API calls without target, got %d", len(calls))
	}
}

func TestReactionLifecycle_UnknownStateNoOp(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rec := &reactionRecorder{}
	mux := mockReactionServer(rec)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	api := slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	rl := NewReactionLifecycle(api, logger)

	rl.SetTarget("agent-1", "C123", "1234.5678")
	rl.TransitionState("agent-1", "rate_limited") // unknown state

	calls := rec.getCalls()
	if len(calls) != 0 {
		t.Errorf("unknown state should not trigger API calls, got %d", len(calls))
	}
}

func TestReactionLifecycle_Cleanup(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rec := &reactionRecorder{}
	mux := mockReactionServer(rec)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	api := slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	rl := NewReactionLifecycle(api, logger)

	rl.SetTarget("agent-1", "C123", "1234.5678")
	rl.current["agent-1"] = gardenWorkingReaction

	rl.Cleanup("agent-1")

	rl.mu.Lock()
	_, hasCurrent := rl.current["agent-1"]
	_, hasTarget := rl.targets["agent-1"]
	rl.mu.Unlock()
	if hasCurrent || hasTarget {
		t.Error("Cleanup should remove all tracking for the agent")
	}
}

func TestReactionLifecycle_MissingScopeCircuitBreaker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Use a mock Slack server that returns missing_scope errors.
	rec := &reactionRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/reactions.add", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		emoji := r.FormValue("name")
		ch := r.FormValue("channel")
		ts := r.FormValue("timestamp")
		rec.record("add", emoji, slack.ItemRef{Channel: ch, Timestamp: ts})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "missing_scope"})
	})
	mux.HandleFunc("/reactions.remove", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		emoji := r.FormValue("name")
		ch := r.FormValue("channel")
		ts := r.FormValue("timestamp")
		rec.record("remove", emoji, slack.ItemRef{Channel: ch, Timestamp: ts})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "missing_scope"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	api := slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	rl := NewReactionLifecycle(api, logger)

	rl.SetTarget("agent-1", "C123", "1234.5678")
	rl.SetTarget("agent-2", "C123", "2345.6789")

	// First call should hit the API and trip the circuit breaker.
	rl.TransitionState("agent-1", "spawning")
	calls := rec.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 API call (to trip breaker), got %d", len(calls))
	}

	// Second call should be skipped by the circuit breaker — no API call.
	rl.TransitionState("agent-2", "spawning")
	calls = rec.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected circuit breaker to skip API call, got %d total calls", len(calls))
	}

	// Verify the breaker is tripped.
	if !rl.disabled.Load() {
		t.Error("expected disabled flag to be set after missing_scope error")
	}
}

func TestReactionLifecycle_FailedState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rec := &reactionRecorder{}
	mux := mockReactionServer(rec)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	api := slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	rl := NewReactionLifecycle(api, logger)

	rl.SetTarget("agent-1", "C123", "1234.5678")
	rl.TransitionState("agent-1", "spawning")
	rl.TransitionState("agent-1", "working")
	rl.TransitionState("agent-1", "failed")

	calls := rec.getCalls()
	// spawning: add seedling
	// working: remove seedling, add leaves
	// failed: remove leaves, add wilted_flower
	if len(calls) != 5 {
		t.Fatalf("expected 5 calls, got %d: %+v", len(calls), calls)
	}
	last := calls[len(calls)-1]
	if last.Method != "add" || last.Emoji != gardenFailedReaction {
		t.Errorf("expected final add(%s), got %+v", gardenFailedReaction, last)
	}

	// Tracking should be cleaned up for terminal state.
	rl.mu.Lock()
	_, hasCurrent := rl.current["agent-1"]
	_, hasTarget := rl.targets["agent-1"]
	rl.mu.Unlock()
	if hasCurrent || hasTarget {
		t.Error("failed state should clean up tracking")
	}
}

func TestReactionLifecycle_GardenDefaultNames(t *testing.T) {
	for key, want := range map[string]string{
		ReactionSeen:    "seedling",
		ReactionWorking: "leaves",
		ReactionDone:    "deciduous_tree",
		ReactionFailed:  "wilted_flower",
	} {
		if got := Garden.Reaction(key); got != want {
			t.Errorf("Garden.Reaction(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestReactionLifecycle_ThemeResolvedPerChannel proves the emoji names come
// from the target channel's theme: a Business channel keeps the plain marks, a
// Garden (or unconfigured) channel gets the garden names.
func TestReactionLifecycle_ThemeResolvedPerChannel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rec := &reactionRecorder{}
	srv := httptest.NewServer(mockReactionServer(rec))
	defer srv.Close()

	api := slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	rl := NewReactionLifecycle(api, logger)
	rl.theme = func(_ context.Context, channel string) Theme {
		if channel == "CBIZ" {
			return Business
		}
		return Garden
	}

	rl.SetTarget("biz", "CBIZ", "1.1")
	rl.SetTarget("gard", "CGARDEN", "2.2")
	for _, agent := range []string{"biz", "gard"} {
		rl.TransitionState(agent, "spawning")
		rl.TransitionState(agent, "working")
		rl.TransitionState(agent, "done")
	}

	var biz, gard []string
	for _, c := range rec.getCalls() {
		if c.Method != "add" {
			continue
		}
		if c.Ref.Channel == "CBIZ" {
			biz = append(biz, c.Emoji)
		} else {
			gard = append(gard, c.Emoji)
		}
	}
	if want := []string{"white_circle", "hourglass_flowing_sand", "white_check_mark"}; !reflect.DeepEqual(biz, want) {
		t.Errorf("business reactions = %v, want %v", biz, want)
	}
	if want := []string{"seedling", "leaves", "deciduous_tree"}; !reflect.DeepEqual(gard, want) {
		t.Errorf("garden reactions = %v, want %v", gard, want)
	}
}

func TestReactionLifecycle_BusinessFailedState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	rec := &reactionRecorder{}
	srv := httptest.NewServer(mockReactionServer(rec))
	defer srv.Close()

	api := slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	rl := NewReactionLifecycle(api, logger)
	rl.theme = func(context.Context, string) Theme { return Business }

	rl.SetTarget("agent-1", "CBIZ", "1.1")
	rl.TransitionState("agent-1", "working")
	rl.TransitionState("agent-1", "failed")

	calls := rec.getCalls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls, got %d: %+v", len(calls), calls)
	}
	if calls[1].Method != "remove" || calls[1].Emoji != "hourglass_flowing_sand" {
		t.Errorf("expected remove(hourglass_flowing_sand), got %+v", calls[1])
	}
	if calls[2].Method != "add" || calls[2].Emoji != "x" {
		t.Errorf("expected add(x), got %+v", calls[2])
	}
}
