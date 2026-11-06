package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// ── detectNudgeType ──────────────────────────────────────────────────────

func TestDetectNudgeType_Extra(t *testing.T) {
	// Redirect the assigned sentinel off /tmp so an unrelated real prewarmed
	// pod's sentinel can't flake the prewarmed cases below.
	origSentinel := standbyAssignedSentinelPath
	standbyAssignedSentinelPath = filepath.Join(t.TempDir(), "standby_assigned")
	defer func() { standbyAssignedSentinelPath = origSentinel }()

	// Unassigned prewarmed wins over everything.
	t.Setenv("BOAT_AGENT_STATE", "prewarmed")
	if got := detectNudgeType(); got != "prewarmed" {
		t.Errorf("prewarmed: %q", got)
	}

	// Assigned prewarmed (sentinel present) falls through to the real nudge.
	t.Setenv("SLACK_THREAD_CHANNEL", "C1")
	if err := os.WriteFile(standbyAssignedSentinelPath, []byte("1"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if got := detectNudgeType(); got != "thread" {
		t.Errorf("assigned prewarmed → thread: %q", got)
	}
	_ = os.Remove(standbyAssignedSentinelPath)
	t.Setenv("SLACK_THREAD_CHANNEL", "")

	t.Setenv("BOAT_AGENT_STATE", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "C1")
	if got := detectNudgeType(); got != "thread" {
		t.Errorf("thread: %q", got)
	}

	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("BOAT_PROMPT", "hi")
	if got := detectNudgeType(); got != "adhoc" {
		t.Errorf("adhoc: %q", got)
	}

	t.Setenv("BOAT_PROMPT", "")
	if got := detectNudgeType(); got != "default" {
		t.Errorf("default: %q", got)
	}
}

// ── substituteNudgeVars ──────────────────────────────────────────────────

func TestSubstituteNudgeVars_Extra(t *testing.T) {
	got := substituteNudgeVars("hi {{.Project}} / {{.Role}} / {{.TaskHint}}", nudgeVars{
		Project: "p", Role: "r", TaskHint: "th",
	})
	if got != "hi p / r / th" {
		t.Errorf("got %q", got)
	}
}

// ── buildNudgeVars ───────────────────────────────────────────────────────

func TestBuildNudgeVars_Defaults(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_TASK_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("BOAT_ROLE", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	t.Setenv("BOAT_PROMPT", "")

	// Swap daemon to nil so resolveTaskFromDeps and the GetBead check no-op.
	oldD := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldD })

	v := buildNudgeVars()
	if v.Project != "gardenio" {
		t.Errorf("project: %q", v.Project)
	}
	if v.TaskHint != "" {
		t.Errorf("taskhint: %q", v.TaskHint)
	}
}

func TestBuildNudgeVars_WithProjectAndTask(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "myp")
	t.Setenv("BOAT_TASK_ID", "gd-T1")
	t.Setenv("BOAT_REFERENCE_REPOS", "repo1,repo2")
	t.Setenv("BOAT_PROMPT", "custom prompt")

	oldD := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldD })

	v := buildNudgeVars()
	if v.Project != "myp" {
		t.Errorf("project: %q", v.Project)
	}
	if v.TaskHint == "" {
		t.Error("expected task hint")
	}
	if v.MonorepoHint == "" {
		t.Error("expected monorepo hint")
	}
	if v.ProjectHint == "" {
		t.Error("expected project hint")
	}
	if v.BoatPrompt != "custom prompt" {
		t.Errorf("prompt: %q", v.BoatPrompt)
	}
}

func TestBuildNudgeVars_SkipsClosedTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-T1", "title": "done", "type": "task", "status": "closed",
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_TASK_ID", "gd-T1")
	t.Setenv("BOAT_PROJECT", "p")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("BOAT_ROLE", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	t.Setenv("BOAT_PROMPT", "")

	v := buildNudgeVars()
	if v.TaskHint != "" {
		t.Errorf("expected empty task hint for closed task, got %q", v.TaskHint)
	}
}

// ── resolveTaskFromDeps ──────────────────────────────────────────────────

func TestResolveTaskFromDeps_NoAgent(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	if got := resolveTaskFromDeps(); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestResolveTaskFromDeps_NoDaemon(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-A1")
	oldD := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldD })
	if got := resolveTaskFromDeps(); got != "" {
		t.Errorf("got %q", got)
	}
}

// ── runNudgePrompt ───────────────────────────────────────────────────────

func TestRunNudgePrompt_NoConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_AGENT_STATE", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	oldType := nudgePromptType
	nudgePromptType = ""
	t.Cleanup(func() { nudgePromptType = oldType })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runNudgePrompt(cmd, nil); err == nil {
		t.Error("expected error (no config)")
	}
}

// ── resolveNudgeFromConfig ───────────────────────────────────────────────

func TestResolveNudgeFromConfig_NoDaemon(t *testing.T) {
	oldD := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldD })
	if got := resolveNudgeFromConfig("default", nudgeVars{}); got != "" {
		t.Errorf("got %q", got)
	}
}

// ── setup hook detection ─────────────────────────────────────────────────

func TestIsGBHookCommand(t *testing.T) {
	// At least one of these prefixes should work.
	// Use a literal that is very likely to be a prefix (gb is the binary).
	if !isGBHookCommand("gb hook check-mail") {
		t.Error("expected gb hook to match")
	}
	if isGBHookCommand("some-other-cmd --flag") {
		t.Error("random cmd should not match")
	}
}

func TestHookContainsGB(t *testing.T) {
	// Valid structure with a gb hook command.
	hooks := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"command": "gb hook check-mail"},
				},
			},
		},
	}
	if !hookContainsGB(hooks, "SessionStart") {
		t.Error("expected match")
	}

	// No matching hook type.
	if hookContainsGB(hooks, "Stop") {
		t.Error("no Stop hook should be false")
	}

	// Empty.
	if hookContainsGB(map[string]any{}, "Stop") {
		t.Error("empty should be false")
	}

	// Bad inner shape.
	bad := map[string]any{
		"Stop": []any{
			map[string]any{"hooks": []any{map[string]any{"command": "other"}}},
		},
	}
	if hookContainsGB(bad, "Stop") {
		t.Error("non-gb command should not match")
	}

	// Bad outer shape.
	if hookContainsGB(map[string]any{"Stop": "not-array"}, "Stop") {
		t.Error("bad outer should be false")
	}
}

// ── prime formatters ─────────────────────────────────────────────────────

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 30*time.Minute, "2h30m"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%v): got %q want %q", c.d, got, c.want)
		}
	}
}

func TestFormatIdleDur(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{30, "30s"},
		{65, "1m5s"},
		{3661, "1h1m"},
	}
	for _, c := range cases {
		if got := formatIdleDur(c.in); got != c.want {
			t.Errorf("formatIdleDur(%v): got %q want %q", c.in, got, c.want)
		}
	}
}
