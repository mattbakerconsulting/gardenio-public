package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestLocalRolePrompts_RenderAgainstSeededDaemon is the end-to-end check for
// gd-rIHQbpZR0Y: against a daemon holding exactly what bridge.EnsureConfigs
// seeds, `BOAT_ROLE=local gb prime --system-prompt` renders the role:local
// claude-instructions sections in place of the global ones, and `BOAT_ROLE=local
// gb nudge-prompt` resolves the role:local nudge templates.
func TestLocalRolePrompts_RenderAgainstSeededDaemon(t *testing.T) {
	cleanup := startBudgetDaemon(t, nil, realConfigBudgetBeads(t))
	defer cleanup()

	t.Setenv("BOAT_ROLE", "local")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_ACTOR", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_STATE", "")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	t.Setenv("BOAT_TASK_ID", "")

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, "gardenio/locals/test-local", "local")
	body := buf.String()
	for _, want := range []string{
		"## Local Agent Workflow",
		"**The thread grammar**",
		"## Local Agent — Acting As Your Owner",
		"gb escalation create --requesting-task",
		"# Session Close", // inherited from global
	} {
		if !strings.Contains(body, want) {
			t.Errorf("BOAT_ROLE=local system prompt missing %q", want)
		}
	}
	for _, replaced := range []string{"## Common Workflows", "## Agents Are Ephemeral"} {
		if strings.Contains(body, replaced) {
			t.Errorf("BOAT_ROLE=local system prompt still renders the global %q section", replaced)
		}
	}

	if got := detectNudgeType(); got != "default" {
		t.Fatalf("detectNudgeType() = %q, want default for a plain local session", got)
	}
	vars := nudgeVars{Project: "gardenio", ProjectHint: " PROJECT_HINT.", TaskHint: " TASK_HINT.", BoatPrompt: "THE_TASK"}
	for typ, wants := range map[string][]string{
		"default": {"You are a local agent on your owner's machine", "PROJECT_HINT.", "TASK_HINT.", "gb escalation create"},
		"adhoc":   {"You are a local agent on your owner's machine", "TASK_HINT.", "Here is your task: THE_TASK"},
		// Not overridden by role:local — inherits the global template.
		"thread": {"thread-bound agent"},
	} {
		got := resolveNudgeFromConfig(typ, vars)
		for _, w := range wants {
			if !strings.Contains(got, w) {
				t.Errorf("BOAT_ROLE=local nudge %q missing %q; got %q", typ, w, got)
			}
		}
	}
}
