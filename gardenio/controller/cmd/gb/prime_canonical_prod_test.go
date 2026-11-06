package main

// prime_canonical_prod_test.go pins the gd-dr9mF1rWwq regression: the
// canonical-prod role MUST still see the CRITICAL RULES preamble after
// ka-0000005600 scoped it to canonical-prod only.
//
// ka-0000005600 (MR !2249) tightened MatchesSubscriptions so that an advice
// tagged `project:gardenio, role:canonical-prod` no longer leaks to every
// gardenio agent. The verification probe (gd-rSmB7OxQ5U) reported
// `critical_rules=no` for shape=canonical-prod, raising the question: did
// the fix over-scope and remove the preamble from canonical-prod itself?
//
// TestResolveConfigBeads_CanonicalProdScopingDoesNotLeak in config_resolve_test.go
// already pins this at the resolution layer. This file pins it one layer up:
// buildSystemPromptBody — the function that actually produces the prompt body
// claude --append-system-prompt-file consumes — must include the literal
// "## CRITICAL RULES — canonical-prod" header for a canonical-prod role and
// must NOT include it for plain crew/gardener/thread/gardener-delivery.

import (
	"bytes"
	"strings"
	"testing"
)

// canonicalProdLiveConfigBead returns a budgetBead shaped like the live
// gd-Xur4qDwePi claude-instructions config bead: title "claude-instructions",
// labels ["project:gardenio", "role:canonical-prod"], description carrying the
// canonical-prod CRITICAL RULES under the core_rules JSON key.
func canonicalProdLiveConfigBead() budgetBead {
	return budgetBead{
		ID:    "gd-Xur4qDwePi",
		Title: "claude-instructions",
		Description: `{` +
			`"identity":"## canonical-prod\nYou are a canonical-prod agent.",` +
			`"core_rules":"## CRITICAL RULES — canonical-prod\n\n1. No implementation until human approval.\n2. Design artifacts land in the repo.\n",` +
			`"lifecycle":"## Lifecycle\nTask-bound agent.",` +
			`"commands":"## Commands\nkd, gb, glab.",` +
			`"workflows":"## Workflows\nResearch task → design doc."` +
			`}`,
		Type:   "config",
		Kind:   "config",
		Status: "open",
		Labels: []string{"project:gardenio", "role:canonical-prod"},
	}
}

// globalClaudeInstructionsBead returns a minimal global claude-instructions
// bead so the role:canonical-prod resolution has a global layer to merge with —
// matches the layered-config shape every role sees in production.
func globalClaudeInstructionsBead() budgetBead {
	return budgetBead{
		ID:    "gd-cfg-global-claude-instructions",
		Title: "claude-instructions",
		Description: `{` +
			`"prime_header":"# Beads Workflow Context\n",` +
			`"core_rules":"## Core Rules\n- Use kd for CRUD.\n",` +
			`"stop_gate":"## Stop Gate Contract\nNever bypass."` +
			`}`,
		Type:   "config",
		Kind:   "config",
		Status: "open",
		Labels: []string{"global"},
	}
}

func TestBuildSystemPromptBody_CanonicalProdSeesCriticalRules(t *testing.T) {
	configs := []budgetBead{
		globalClaudeInstructionsBead(),
		canonicalProdLiveConfigBead(),
	}
	cleanup := startBudgetDaemon(t, nil, configs)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")

	oldFor, oldNoAdvice := primeForAgent, primeNoAdvice
	primeForAgent = ""
	primeNoAdvice = true // we are testing the workflow context, not advice rendering
	defer func() { primeForAgent, primeNoAdvice = oldFor, oldNoAdvice }()

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, "gardenio-canonical-prod-test", "canonical-prod")
	out := buf.String()

	// canonical-prod role MUST see the CRITICAL RULES header. The live
	// config bead embeds it under core_rules; outputConfigSections renders
	// core_rules into the body as-is.
	if !strings.Contains(out, "## CRITICAL RULES — canonical-prod") {
		t.Errorf("canonical-prod system prompt missing CRITICAL RULES header — preamble was over-filtered.\n--- output:\n%s", out)
	}
	// Lifecycle from the canonical-prod layer should also land — proves the
	// canonical-prod config bead resolved (not just the global one).
	if !strings.Contains(out, "Task-bound agent.") {
		t.Errorf("canonical-prod lifecycle section missing — canonical-prod config bead did not resolve.\n--- output:\n%s", out)
	}
}

// TestBuildSystemPromptBody_PlainRolesDoNotSeeCanonicalProdRules pins the
// MatchesSubscriptions role-required gate one layer up: a plain crew, gardener,
// thread, or gardener-delivery agent on project:gardenio MUST NOT receive the
// canonical-prod core_rules content.
func TestBuildSystemPromptBody_PlainRolesDoNotSeeCanonicalProdRules(t *testing.T) {
	configs := []budgetBead{
		globalClaudeInstructionsBead(),
		canonicalProdLiveConfigBead(),
	}

	for _, role := range []string{"crew", "gardener", "thread", "gardener-delivery"} {
		t.Run(role, func(t *testing.T) {
			cleanup := startBudgetDaemon(t, nil, configs)
			defer cleanup()

			t.Setenv("BOAT_PROJECT", "gardenio")
			t.Setenv("KD_AGENT_ID", "")
			t.Setenv("BOAT_TASK_ID", "skip-auto-assign")

			oldFor, oldNoAdvice := primeForAgent, primeNoAdvice
			primeForAgent = ""
			primeNoAdvice = true
			defer func() { primeForAgent, primeNoAdvice = oldFor, oldNoAdvice }()

			var buf bytes.Buffer
			buildSystemPromptBody(&buf, "gardenio-"+role+"-test", role)
			out := buf.String()

			if strings.Contains(out, "## CRITICAL RULES — canonical-prod") {
				t.Errorf("[%s] canonical-prod CRITICAL RULES leaked into plain-role system prompt", role)
			}
		})
	}
}
