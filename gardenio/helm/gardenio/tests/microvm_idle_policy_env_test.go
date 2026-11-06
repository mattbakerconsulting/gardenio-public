package tests

import (
	"strings"
	"testing"
)

// Native suspend/resume IdlePolicy env wiring for the lambda_microvm backend on
// the agent-controller deployment (plan ka-0000004518, epic ka-0000004689).
//
// The chart contract these tests pin:
//   - idlePolicy.enabled=false renders no MICROVM_IDLE_* env at all, so the
//     backend leaves RunMicrovm's IdlePolicy member unset (byte-for-byte
//     pre-idle-policy launch shape),
//   - idlePolicy.enabled=true renders all three SDK-required members (the AWS
//     SDK requires them together whenever an IdlePolicy is present),
//   - autoResumeEnabled renders only when explicitly set (default false stays
//     unset; the controller defaults it to false).

// TestMicrovmIdlePolicy_DefaultOff: enabling the microvm backend without an idle
// policy must render none of the MICROVM_IDLE_* env vars.
func TestMicrovmIdlePolicy_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags()...)
	for _, forbidden := range []string{
		"MICROVM_IDLE_POLICY_ENABLED",
		"MICROVM_IDLE_AUTO_RESUME_ENABLED",
		"MICROVM_IDLE_MAX_IDLE_DURATION_SECONDS",
		"MICROVM_IDLE_SUSPENDED_DURATION_SECONDS",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s must NOT render without agents.microvm.idlePolicy.enabled; got:\n%s", forbidden, out)
		}
	}
}

// TestMicrovmIdlePolicy_EnabledRendersAllRequired: enabling the policy renders
// the master switch plus both required duration members. autoResumeEnabled stays
// unset (default false), so the controller supplies false.
func TestMicrovmIdlePolicy_EnabledRendersAllRequired(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.idlePolicy.enabled=true",
		"agents.microvm.idlePolicy.maxIdleDurationSeconds=900",
		"agents.microvm.idlePolicy.suspendedDurationSeconds=28800",
	)...)
	for _, want := range []string{
		"name: MICROVM_IDLE_POLICY_ENABLED",
		"name: MICROVM_IDLE_MAX_IDLE_DURATION_SECONDS",
		`value: "900"`,
		"name: MICROVM_IDLE_SUSPENDED_DURATION_SECONDS",
		`value: "28800"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("idlePolicy.enabled must render %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MICROVM_IDLE_AUTO_RESUME_ENABLED") {
		t.Errorf("MICROVM_IDLE_AUTO_RESUME_ENABLED must NOT render when autoResumeEnabled is false; got:\n%s", out)
	}
}

// TestMicrovmIdlePolicy_AutoResumeRendersWhenSet: autoResumeEnabled renders only
// when explicitly true (still requires the master switch on).
func TestMicrovmIdlePolicy_AutoResumeRendersWhenSet(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.idlePolicy.enabled=true",
		"agents.microvm.idlePolicy.autoResumeEnabled=true",
		"agents.microvm.idlePolicy.maxIdleDurationSeconds=600",
		"agents.microvm.idlePolicy.suspendedDurationSeconds=7200",
	)...)
	block := blockAfter(out, "MICROVM_IDLE_AUTO_RESUME_ENABLED")
	if block == "" {
		t.Fatal("expected MICROVM_IDLE_AUTO_RESUME_ENABLED env when autoResumeEnabled=true")
	}
	if !strings.Contains(block, `value: "true"`) {
		t.Errorf("MICROVM_IDLE_AUTO_RESUME_ENABLED must render value \"true\"; got:\n%s", block)
	}
}

// TestMicrovmIdlePolicy_InertWhenBackendOff: the idle policy must never render
// when the whole microvm block is off, even if the idle knobs are set.
func TestMicrovmIdlePolicy_InertWhenBackendOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl,
		"agents.enabled=true",
		"agents.microvm.enabled=false",
		"agents.microvm.idlePolicy.enabled=true",
		"agents.microvm.idlePolicy.maxIdleDurationSeconds=900",
		"agents.microvm.idlePolicy.suspendedDurationSeconds=28800",
	)
	if strings.Contains(out, "MICROVM_IDLE_POLICY_ENABLED") {
		t.Errorf("MICROVM_IDLE_* must NOT render when agents.microvm.enabled=false; got:\n%s", out)
	}
}
