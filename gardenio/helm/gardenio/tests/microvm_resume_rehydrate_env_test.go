package tests

import (
	"strings"
	"testing"
)

// Resume rehydration correctness-boundary env wiring for the lambda_microvm
// backend on the agent-controller deployment (step 5, plan ka-0000004518, epic
// ka-0000004689).
//
// The chart contract these tests pin:
//   - the gate is DEFAULT-ON: the base microvm config renders NO
//     MICROVM_RESUME_REHYDRATE_ENABLED env (the controller defaults it to true),
//   - resumeRehydrate.enabled=false renders MICROVM_RESUME_REHYDRATE_ENABLED
//     ="false" (the step-3 rollback),
//   - a non-zero deadlineSeconds renders MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS,
//   - none of it renders when the whole microvm block is off.

// TestMicrovmResumeRehydrate_DefaultOnRendersNoDisableEnv: with the microvm
// backend on and resumeRehydrate left at its default (enabled=true,
// deadlineSeconds=0), neither env renders — the controller's default-on gate and
// default deadline apply.
func TestMicrovmResumeRehydrate_DefaultOnRendersNoDisableEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags()...)
	for _, forbidden := range []string{
		"MICROVM_RESUME_REHYDRATE_ENABLED",
		"MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s must NOT render at the default (gate default-on, deadline default); got:\n%s", forbidden, out)
		}
	}
}

// TestMicrovmResumeRehydrate_DisabledRendersFalse: setting enabled=false renders
// the explicit disable env (the step-3 rollback — stamp working immediately).
func TestMicrovmResumeRehydrate_DisabledRendersFalse(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.resumeRehydrate.enabled=false",
	)...)
	block := blockAfter(out, "MICROVM_RESUME_REHYDRATE_ENABLED")
	if block == "" {
		t.Fatal("expected MICROVM_RESUME_REHYDRATE_ENABLED env when resumeRehydrate.enabled=false")
	}
	if !strings.Contains(block, `value: "false"`) {
		t.Errorf("MICROVM_RESUME_REHYDRATE_ENABLED must render value \"false\"; got:\n%s", block)
	}
}

// TestMicrovmResumeRehydrate_DeadlineRendersWhenSet: a non-zero deadlineSeconds
// renders the deadline env; the gate stays default-on (no disable env).
func TestMicrovmResumeRehydrate_DeadlineRendersWhenSet(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.resumeRehydrate.deadlineSeconds=120",
	)...)
	block := blockAfter(out, "MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS")
	if block == "" {
		t.Fatal("expected MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS env when deadlineSeconds is set")
	}
	if !strings.Contains(block, `value: "120"`) {
		t.Errorf("deadline env must render value \"120\"; got:\n%s", block)
	}
	if strings.Contains(out, "MICROVM_RESUME_REHYDRATE_ENABLED") {
		t.Errorf("gate stays default-on: no disable env should render; got:\n%s", out)
	}
}

// TestMicrovmResumeRehydrate_InertWhenBackendOff: nothing renders when the whole
// microvm block is off, even if the rehydrate knobs are set.
func TestMicrovmResumeRehydrate_InertWhenBackendOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl,
		"agents.enabled=true",
		"agents.microvm.enabled=false",
		"agents.microvm.resumeRehydrate.enabled=false",
		"agents.microvm.resumeRehydrate.deadlineSeconds=120",
	)
	for _, forbidden := range []string{
		"MICROVM_RESUME_REHYDRATE_ENABLED",
		"MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s must NOT render when agents.microvm.enabled=false; got:\n%s", forbidden, out)
		}
	}
}
