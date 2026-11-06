package tests

import (
	"strings"
	"testing"
)

// Recycle-before-cap env wiring for the MicroVM warm pool on the agent-controller
// deployment (plan ka-0000004145, Phase 4 ka-0000003944).
//
// The chart contract these tests pin:
//   - agents.prewarmedMicrovmMaxAge empty (the default) renders no
//     PREWARMED_MICROVM_MAX_AGE env, so the controller derives the recycle age
//     from the VM lifetime cap (half of maximumDurationSeconds),
//   - a set value renders PREWARMED_MICROVM_MAX_AGE verbatim (truthy-guarded,
//     mirroring the sibling prewarmedPoolInterval knob).

const prewarmedMicrovmMaxAgeTpl = "templates/controller/deployment.yaml"

// TestPrewarmedMicrovmMaxAge_DefaultOff: with no override the env must NOT
// render, so the controller falls back to its cap-derived default.
func TestPrewarmedMicrovmMaxAge_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, prewarmedMicrovmMaxAgeTpl, "agents.enabled=true")
	if strings.Contains(out, "PREWARMED_MICROVM_MAX_AGE") {
		t.Errorf("PREWARMED_MICROVM_MAX_AGE must NOT render without agents.prewarmedMicrovmMaxAge; got:\n%s", out)
	}
}

// TestPrewarmedMicrovmMaxAge_SetRenders: a set value renders the env verbatim.
func TestPrewarmedMicrovmMaxAge_SetRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, prewarmedMicrovmMaxAgeTpl,
		"agents.enabled=true",
		"agents.prewarmedMicrovmMaxAge=20m",
	)
	block := blockAfter(out, "PREWARMED_MICROVM_MAX_AGE")
	if block == "" {
		t.Fatal("expected PREWARMED_MICROVM_MAX_AGE env when agents.prewarmedMicrovmMaxAge is set")
	}
	if !strings.Contains(block, `value: "20m"`) {
		t.Errorf("PREWARMED_MICROVM_MAX_AGE must render value \"20m\"; got:\n%s", block)
	}
}
