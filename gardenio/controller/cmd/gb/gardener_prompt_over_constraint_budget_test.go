package main

// gardener_prompt_over_constraint_budget_test.go is the durable CI guard for the
// gardener trigger-prompt AUTHORING surface (task ka-0000004887, finding N1 of
// the P2 post-remediation re-audit ka-0000002501, under the P4 over-constraint
// epic ka-0000006604). It is the gardener-template analog of
// advice_over_constraint_budget_test.go (advice corpus) and
// prime_directive_density_budget_test.go (rendered prime body).
//
// WHY this surface: the re-audit found that once init_prompts.go was
// de-constrained (4.25 -> 0.73/1k), the hand-authored gardener trigger prompt
// became the single hottest injected surface in the corpus (3.89/1k). The
// templated scaffolding a gardener is spawned FROM is `gb gardener start`'s
// rendered playbook (renderGardenerPrompt + appendGardenerExitGuidance) — the
// authoring template every new gardener inherits. Part 1 of the task reframed
// that template into judgment-based guidance (0 emphatic directives); this test
// pins that win so a future edit that re-stacks shout-directives into the
// gardener authoring template turns CI red.
//
// It runs the exact production render path and the exact three over-constraint
// checks the `gb lint trigger-prompts --over-constraint` CLI runs
// (advice.LintTriggerPromptOverConstraint), against the SAME
// instruction-surface extraction the CLI feeds them, so the test and the CLI
// can never disagree on what "over-constrained" means for a gardener prompt.

import (
	"testing"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// gardenerTemplateDirectiveBaseline pins the emphatic-directive count of the
// rendered `gb gardener start` playbook template. Frozen 2026-07-26 (task
// ka-0000004887): the reframe brought the rendered template to 0 emphatic
// directives (was 4 primary markers pre-reframe). This is a ceiling to ratchet
// DOWN, never up — a positive-intent reframe keeps a load-bearing invariant
// (e.g. "leave your own trigger enabled") without a shout-directive. If a
// genuinely load-bearing new imperative is unavoidable, bump this WITH a note
// on why the extra imperative pressure is justified, exactly as the sibling
// budget tests require.
const gardenerTemplateDirectiveBaseline = 0

// gardenerTemplateHeadroom is the slack the rendered template may grow before the
// guard trips — set to 0 so ANY new emphatic directive in the gardener authoring
// template is caught, because this is a template (one edit propagates to every
// gardener spawned from it), not a one-off bead.
const gardenerTemplateHeadroom = 0

// renderGardenerTemplateForBudget renders the canonical gardener playbook prompt
// the way `gb gardener start` does — the template body plus the appended loop-exit
// guidance — with fixed, representative parameters so the measurement is
// deterministic (independent of any real epic id or flag state).
func renderGardenerTemplateForBudget(t *testing.T) string {
	t.Helper()
	const epic = "gd-QYASUFNR2W"
	prompt, err := renderGardenerPrompt(epic)
	if err != nil {
		t.Fatalf("renderGardenerPrompt: %v", err)
	}
	return appendGardenerExitGuidance(prompt, gardenerExitGuidance{
		EpicID:        epic,
		Terminal:      "All children of " + epic + " are closed.",
		LoopWallClock: "168h",
		RunWallClock:  "30m",
	})
}

// TestGardenerTemplateOverConstraintBudget renders the gardener authoring template
// and fails CI if it carries more emphatic directives than its frozen baseline —
// the durable guard that keeps the Part-1 reframe from silently regressing.
func TestGardenerTemplateOverConstraintBudget(t *testing.T) {
	rendered := renderGardenerTemplateForBudget(t)

	// Sanity: the rendered template must be non-trivial, else a broken render
	// would pass the budget vacuously.
	if len(rendered) < 1500 {
		t.Fatalf("rendered gardener template suspiciously small (%d bytes) — render likely broke", len(rendered))
	}

	got := advice.CountDirectiveMarkers(rendered)
	cap := gardenerTemplateDirectiveBaseline + gardenerTemplateHeadroom
	t.Logf("BASELINE gardener-template: emphatic directives=%d (baseline %d, cap %d), bytes=%d",
		got, gardenerTemplateDirectiveBaseline, cap, len(rendered))

	if got > cap {
		t.Errorf("gardener authoring template has %d emphatic directive(s) > cap %d (baseline %d + %d headroom) — "+
			"a shout-directive re-leak into the `gb gardener start` playbook (the surface every gardener inherits); "+
			"rewrite the negative as positive intent + WHY (see the F1-F10 treatment in gardenio/docs/audits/2026-07-26-post-remediation-reaudit/), "+
			"or bump gardenerTemplateDirectiveBaseline in-MR with justification",
			got, cap, gardenerTemplateDirectiveBaseline, gardenerTemplateHeadroom)
	}
}

// TestGardenerTemplateNoOverConstraintFindings runs the exact three
// over-constraint checks the `gb lint trigger-prompts --over-constraint` CLI
// runs over the rendered gardener template (as a single prompt surface) and
// asserts it is clean — no rules-over-judgment framing, no directive-density
// spike. The redundancy check is not exercised here (no ref corpus): a template
// legitimately shares command idioms with the injected context, and redundancy
// is a per-live-bead signal, not a template-authoring one.
func TestGardenerTemplateNoOverConstraintFindings(t *testing.T) {
	rendered := renderGardenerTemplateForBudget(t)
	surfaces := []advice.TriggerPromptSurface{{
		ID: "gardener-start-template", Title: "gb gardener start playbook", Field: "prompt", Text: rendered,
	}}
	findings := advice.LintTriggerPromptOverConstraint(surfaces, "", advice.OverConstraintConfig{})
	for _, f := range findings {
		t.Errorf("gardener authoring template tripped over-constraint check %s (%s) at line %d: %q",
			f.Check, f.Rule, f.Line, f.Snippet)
	}
}

// Compile-time assurance that the budget test's surface shape matches what the
// CLI adapter produces, so the two stay coupled: triggerPromptSurfaces yields
// advice.TriggerPromptSurface, the same type the budget test hand-builds.
var _ = func() []advice.TriggerPromptSurface {
	return triggerPromptSurfaces([]*beadsapi.BeadDetail{})
}
