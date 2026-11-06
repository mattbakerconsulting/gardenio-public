package tests

import (
	"strings"
	"testing"
)

// BEADS_OPERATION_TYPE_GATE selects the kbeads escalation operation_type
// registry gate mode (kbeads/internal/server/escalation_op_registry.go,
// gd-OXdyPW15fC). The chart owns it through beads.operationTypeGate; empty
// leaves the env unset so the daemon default (warn) applies.

const operationTypeGateEnvName = "- name: BEADS_OPERATION_TYPE_GATE"

func TestBeadsOperationTypeGateEnv_DefaultUnset(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t)
	if !strings.Contains(out, "BEADS_HTTP_ADDR") {
		t.Fatalf("control: beads Deployment did not render the daemon env:\n%s", out)
	}
	if strings.Contains(out, "BEADS_OPERATION_TYPE_GATE") {
		t.Errorf("default render must not set BEADS_OPERATION_TYPE_GATE (beads.operationTypeGate defaults empty → daemon default warn)")
	}
}

func TestBeadsOperationTypeGateEnv_Set(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"warn", "enforce", "off"} {
		t.Run(mode, func(t *testing.T) {
			lines := strings.Split(helmBeadsDeployment(t, "--set", "beads.operationTypeGate="+mode), "\n")
			for i, l := range lines {
				if strings.TrimSpace(l) != operationTypeGateEnvName {
					continue
				}
				if want := `value: "` + mode + `"`; i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != want {
					t.Fatalf("BEADS_OPERATION_TYPE_GATE must be followed by %s; got:\n%s", want, strings.Join(lines[i:min(i+2, len(lines))], "\n"))
				}
				return
			}
			t.Fatalf("expected BEADS_OPERATION_TYPE_GATE in render, not found")
		})
	}
}
