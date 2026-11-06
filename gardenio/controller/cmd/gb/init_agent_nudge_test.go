package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// nudgeConfigServer serves a daemon whose config-bead listing returns one
// global nudge-prompts bead with the given templates. While failing is true
// every request returns 500. Every other path returns 404.
func nudgeConfigServer(t *testing.T, templates map[string]string, failing *atomic.Bool) *httptest.Server {
	t.Helper()
	value, err := json.Marshal(templates)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing != nil && failing.Load() {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/v1/beads" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{map[string]any{
			"id":          "gd-nudge",
			"title":       "nudge-prompts",
			"type":        "config",
			"status":      "open",
			"labels":      []string{"global"},
			"description": string(value),
		}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// resetNudgeState clears the process-wide fallback streak and last-good cache
// and makes the config retry instant, restoring all three on cleanup.
func resetNudgeState(t *testing.T) {
	t.Helper()
	consecutiveNudgeFallbacks.Store(0)
	lastGoodNudge.Lock()
	lastGoodNudge.byType = map[string]string{}
	lastGoodNudge.Unlock()
	orig := resolveConfigBackoffs
	resolveConfigBackoffs = []time.Duration{time.Millisecond}
	t.Cleanup(func() {
		resolveConfigBackoffs = orig
		consecutiveNudgeFallbacks.Store(0)
		lastGoodNudge.Lock()
		lastGoodNudge.byType = map[string]string{}
		lastGoodNudge.Unlock()
	})
}

// nudgeTestEnv pins the env that detectNudgeType and buildNudgeVars read, so
// the prompt type is "default" and no task hint needs a daemon lookup.
func nudgeTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BOAT_AGENT_STATE", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_TASK_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "plan-gardener")
}

func TestResolveNudgeTemplate_ReportsWhy(t *testing.T) {
	resetNudgeState(t)
	nudgeTestEnv(t)

	t.Run("no daemon", func(t *testing.T) {
		oldD := daemon
		daemon = nil
		t.Cleanup(func() { daemon = oldD })
		_, err := resolveNudgeTemplate("default")
		if err == nil || !strings.Contains(err.Error(), "no beads daemon client") {
			t.Fatalf("err = %v, want no-daemon error", err)
		}
	})

	t.Run("listing fails", func(t *testing.T) {
		var failing atomic.Bool
		failing.Store(true)
		swapDaemon(t, nudgeConfigServer(t, nil, &failing).URL)
		_, err := resolveNudgeTemplate("default")
		if err == nil || !strings.Contains(err.Error(), "listing nudge-prompts config beads") {
			t.Fatalf("err = %v, want listing error", err)
		}
		// The subscriptions tried must be in the message, so the log says
		// which project/role the lookup ran as.
		if !strings.Contains(err.Error(), "role:plan-gardener") || !strings.Contains(err.Error(), "project:gardenio") {
			t.Errorf("err = %v, want subscriptions named", err)
		}
	})

	t.Run("no matching bead", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		}))
		t.Cleanup(srv.Close)
		swapDaemon(t, srv.URL)
		_, err := resolveNudgeTemplate("default")
		if err == nil || !strings.Contains(err.Error(), "no nudge-prompts config bead matches") {
			t.Fatalf("err = %v, want no-match error", err)
		}
	})

	t.Run("type missing", func(t *testing.T) {
		swapDaemon(t, nudgeConfigServer(t, map[string]string{"thread": "T"}, nil).URL)
		_, err := resolveNudgeTemplate("default")
		if err == nil || !strings.Contains(err.Error(), `has no "default" template`) {
			t.Fatalf("err = %v, want missing-type error", err)
		}
	})
}

// TestResolveNudgeMessage_AfterCredentialSwitch is the restart path from
// gd-PPzLrS2k8e: coop exits 10, gb init applies the switch file's credentials,
// and the next session's inject must still get the configured prompt, not the
// fallback.
func TestResolveNudgeMessage_AfterCredentialSwitch(t *testing.T) {
	resetNudgeState(t)
	nudgeTestEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "http://litellm-a")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-a")
	t.Setenv("BOAT_ACCOUNT", "account-a")
	t.Setenv("BOAT_MODEL_PINNED", "")
	swapDaemon(t, nudgeConfigServer(t, map[string]string{"default": "ROLE PROMPT for {{.Role}} on {{.Project}}"}, nil).URL)

	const want = "ROLE PROMPT for plan-gardener on gardenio"
	if got := resolveNudgeMessage(); got != want {
		t.Fatalf("boot inject = %q, want %q", got, want)
	}

	data := `{"credentials":{"ANTHROPIC_BASE_URL":"http://litellm-b","ANTHROPIC_AUTH_TOKEN":"tok-b"},"profile":"account-b"}`
	if err := os.WriteFile(credentialSwitchFile, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(credentialSwitchFile) })
	loadSwitchCredentials()
	if os.Getenv("ANTHROPIC_BASE_URL") != "http://litellm-b" {
		t.Fatal("switch credentials not applied")
	}

	if got := resolveNudgeMessage(); got != want {
		t.Errorf("post-switch inject = %q, want %q", got, want)
	}
	if n := consecutiveNudgeFallbacks.Load(); n != 0 {
		t.Errorf("fallback streak = %d after post-switch inject, want 0", n)
	}
}

// A resolution failure after a successful one re-uses the last template for
// that type and does not count toward the fallback bound.
func TestResolveNudgeMessage_ReusesLastGoodTemplate(t *testing.T) {
	resetNudgeState(t)
	nudgeTestEnv(t)
	var failing atomic.Bool
	swapDaemon(t, nudgeConfigServer(t, map[string]string{"default": "REAL {{.Role}}"}, &failing).URL)

	if got := resolveNudgeMessage(); got != "REAL plan-gardener" {
		t.Fatalf("first inject = %q", got)
	}
	failing.Store(true)
	for i := 0; i < nudgeFallbackBound+1; i++ {
		if got := resolveNudgeMessage(); got != "REAL plan-gardener" {
			t.Fatalf("inject %d after failure = %q, want the cached template", i, got)
		}
	}
	if _, exhausted := nudgeFallbacksExhausted(); exhausted {
		t.Error("cached-template injects must not exhaust the fallback bound")
	}
}

// With nothing cached, each failed inject uses the fallback and counts; the
// bound trips at nudgeFallbackBound, and one resolved prompt resets it.
func TestResolveNudgeMessage_FallbackStreakTripsBound(t *testing.T) {
	resetNudgeState(t)
	nudgeTestEnv(t)
	var failing atomic.Bool
	failing.Store(true)
	swapDaemon(t, nudgeConfigServer(t, map[string]string{"default": "REAL"}, &failing).URL)

	for i := 1; i <= nudgeFallbackBound; i++ {
		got := resolveNudgeMessage()
		if !strings.Contains(got, "built-in fallback") {
			t.Fatalf("inject %d = %q, want the fallback", i, got)
		}
		n, exhausted := nudgeFallbacksExhausted()
		if int(n) != i || exhausted != (i >= nudgeFallbackBound) {
			t.Fatalf("after inject %d: streak=%d exhausted=%v", i, n, exhausted)
		}
	}

	failing.Store(false)
	if got := resolveNudgeMessage(); got != "REAL" {
		t.Fatalf("recovered inject = %q", got)
	}
	if n, exhausted := nudgeFallbacksExhausted(); n != 0 || exhausted {
		t.Errorf("streak=%d exhausted=%v after a resolved prompt, want reset", n, exhausted)
	}
}

func TestFallbackNudgeMessage(t *testing.T) {
	t.Run("role and task", func(t *testing.T) {
		got := fallbackNudgeMessage(nudgeVars{Project: "gardenio", Role: "plan-gardener", TaskHint: taskHintFor("gd-X", "task")})
		for _, want := range []string{"`plan-gardener`", "`gardenio`", "kd claim gd-X", "gb stop"} {
			if !strings.Contains(got, want) {
				t.Errorf("fallback %q missing %q", got, want)
			}
		}
		if strings.Contains(got, "gb ready") {
			t.Errorf("fallback with a task should not send the agent to gb ready: %q", got)
		}
	})
	t.Run("adhoc prompt", func(t *testing.T) {
		got := fallbackNudgeMessage(nudgeVars{Project: "gardenio", Role: "crew", BoatPrompt: "fix the thing"})
		if !strings.Contains(got, "Here is your task: fix the thing") {
			t.Errorf("fallback %q missing the BOAT_PROMPT task", got)
		}
	})
	t.Run("no work assigned", func(t *testing.T) {
		got := fallbackNudgeMessage(nudgeVars{Project: "gardenio"})
		if !strings.Contains(got, "gb ready") || !strings.Contains(got, "gb stop") {
			t.Errorf("fallback %q should point at gb ready and gb stop", got)
		}
	})
}
