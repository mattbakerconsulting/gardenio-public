package tests

import (
	"strings"
	"testing"
)

// Native suspend-on-stop env wiring for the lambda_microvm backend on the
// agent-controller deployment (plan ka-0000004518, epic ka-0000004689, step 2
// ka-0000007189).
//
// The chart contract these tests pin:
//   - suspendOnStop.enabled=false (the default) renders no MICROVM_SUSPEND_ENABLED
//     env at all, so the controller terminates every MicroVM stop as before,
//   - suspendOnStop.enabled=true renders MICROVM_SUSPEND_ENABLED=true,
//   - it never renders when the whole microvm block is off.

// TestMicrovmSuspend_DefaultOff: enabling the microvm backend without suspend-on
// -stop must render no MICROVM_SUSPEND_ENABLED env.
func TestMicrovmSuspend_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags()...)
	if strings.Contains(out, "MICROVM_SUSPEND_ENABLED") {
		t.Errorf("MICROVM_SUSPEND_ENABLED must NOT render without agents.microvm.suspendOnStop.enabled; got:\n%s", out)
	}
}

// TestMicrovmSuspend_EnabledRenders: enabling suspend-on-stop renders the env var
// set to "true".
func TestMicrovmSuspend_EnabledRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.suspendOnStop.enabled=true",
	)...)
	block := blockAfter(out, "MICROVM_SUSPEND_ENABLED")
	if block == "" {
		t.Fatal("expected MICROVM_SUSPEND_ENABLED env when suspendOnStop.enabled=true")
	}
	if !strings.Contains(block, `value: "true"`) {
		t.Errorf("MICROVM_SUSPEND_ENABLED must render value \"true\"; got:\n%s", block)
	}
}

// TestMicrovmSuspend_InertWhenBackendOff: suspend-on-stop must never render when
// the whole microvm block is off, even if the knob is set.
func TestMicrovmSuspend_InertWhenBackendOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl,
		"agents.enabled=true",
		"agents.microvm.enabled=false",
		"agents.microvm.suspendOnStop.enabled=true",
	)
	if strings.Contains(out, "MICROVM_SUSPEND_ENABLED") {
		t.Errorf("MICROVM_SUSPEND_ENABLED must NOT render when agents.microvm.enabled=false; got:\n%s", out)
	}
}

// Max-standby-age cap env wiring (plan ka-0000004518, step 4 ka-0000006202): the
// controller-side cap that bounds how long a suspended standby VM may bill. The
// chart contract these tests pin:
//   - maxStandbyAge empty (the default) renders no MICROVM_MAX_STANDBY_AGE env,
//   - maxStandbyAge set renders MICROVM_MAX_STANDBY_AGE with that value,
//   - it never renders when the whole microvm block is off.

// TestMicrovmMaxStandbyAge_DefaultOff: enabling the microvm backend without a
// maxStandbyAge must render no MICROVM_MAX_STANDBY_AGE env (cap disabled).
func TestMicrovmMaxStandbyAge_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags()...)
	if strings.Contains(out, "MICROVM_MAX_STANDBY_AGE") {
		t.Errorf("MICROVM_MAX_STANDBY_AGE must NOT render without agents.microvm.suspendOnStop.maxStandbyAge; got:\n%s", out)
	}
}

// TestMicrovmMaxStandbyAge_SetRenders: a maxStandbyAge duration renders the env
// var with that value.
func TestMicrovmMaxStandbyAge_SetRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.suspendOnStop.maxStandbyAge=24h",
	)...)
	block := blockAfter(out, "MICROVM_MAX_STANDBY_AGE")
	if block == "" {
		t.Fatal("expected MICROVM_MAX_STANDBY_AGE env when suspendOnStop.maxStandbyAge is set")
	}
	if !strings.Contains(block, `value: "24h"`) {
		t.Errorf("MICROVM_MAX_STANDBY_AGE must render value \"24h\"; got:\n%s", block)
	}
}

// TestMicrovmMaxStandbyAge_InertWhenBackendOff: the cap must never render when
// the whole microvm block is off, even if the knob is set.
func TestMicrovmMaxStandbyAge_InertWhenBackendOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl,
		"agents.enabled=true",
		"agents.microvm.enabled=false",
		"agents.microvm.suspendOnStop.maxStandbyAge=24h",
	)
	if strings.Contains(out, "MICROVM_MAX_STANDBY_AGE") {
		t.Errorf("MICROVM_MAX_STANDBY_AGE must NOT render when agents.microvm.enabled=false; got:\n%s", out)
	}
}
