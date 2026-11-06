package main

// prerun_systemprompt_test.go — integration test that the per-role prerun
// results actually land in buildSystemPromptBody (the --append-system-prompt-file
// body a gardener launches with), and that a role WITHOUT a prerun config bead
// (crew) gets no such section. This is the "agent boots and sees injected
// results" half of the ka-0000007894 e2e contract, exercised at the function
// that produces the prompt.

import (
	"bytes"
	"strings"
	"testing"
)

func TestBuildSystemPromptBody_GardenerSeesPrerunResults(t *testing.T) {
	cfg := []budgetBead{
		globalClaudeInstructionsBead(),
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			// Deterministic, hermetic commands (no real kd/gb needed): echo the
			// assigned bead + a fixed news-shaped line.
			{"id": "assigned", "cmd": "echo assigned=" + prerunAssignedBeadPlaceholder, "heading": "kd show <assigned>"},
			{"id": "news", "cmd": "echo fleet-news-line", "heading": "gb news"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_TASK_ID", "gd-MISSION1")
	t.Setenv("KD_AGENT_ID", "")

	oldFor, oldNoAdvice := primeForAgent, primeNoAdvice
	primeForAgent = ""
	primeNoAdvice = true
	defer func() { primeForAgent, primeNoAdvice = oldFor, oldNoAdvice }()

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, "gardenio-gardener-test", "gardener")
	out := buf.String()

	if !strings.Contains(out, "## Prerun results") {
		t.Fatalf("gardener system prompt missing ## Prerun results section:\n%s", out)
	}
	if !strings.Contains(out, "assigned=gd-MISSION1") {
		t.Errorf("assigned-bead prerun output not injected (placeholder unresolved?):\n%s", out)
	}
	if !strings.Contains(out, "fleet-news-line") {
		t.Errorf("gb news prerun output not injected:\n%s", out)
	}
	// The prerun section must precede the advice/wrap-up tail — it is placed
	// right after workflow context. Assert it appears before the memory pointer
	// footer at minimum (advice is disabled in this test).
	prerunIdx := strings.Index(out, "## Prerun results")
	workflowIdx := strings.Index(out, "Beads Workflow Context")
	if workflowIdx >= 0 && prerunIdx >= 0 && prerunIdx < workflowIdx {
		t.Error("prerun results should appear AFTER workflow context, not before")
	}
}

func TestBuildSystemPromptBody_CrewGetsNoPrerunSection(t *testing.T) {
	cfg := []budgetBead{
		globalClaudeInstructionsBead(),
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			{"id": "news", "cmd": "echo should-not-appear"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_TASK_ID", "gd-MISSION1")
	t.Setenv("KD_AGENT_ID", "")

	oldFor, oldNoAdvice := primeForAgent, primeNoAdvice
	primeForAgent = ""
	primeNoAdvice = true
	defer func() { primeForAgent, primeNoAdvice = oldFor, oldNoAdvice }()

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, "gardenio-crew-test", "crew")
	out := buf.String()

	if strings.Contains(out, "## Prerun results") {
		t.Errorf("crew must NOT get a prerun section (prerun is role:gardener-scoped):\n%s", out)
	}
	if strings.Contains(out, "should-not-appear") {
		t.Error("gardener-scoped prerun leaked into crew system prompt")
	}
}
