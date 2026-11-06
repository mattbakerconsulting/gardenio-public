package tests

import (
	"strings"
	"testing"
)

// BEADS_SHORT_TITLE_GATE selects the kbeads create-time short_title gate mode
// (kbeads/internal/server/short_title_gate.go, gd-V5nGseRS5f). The chart owns
// it through beads.shortTitleGate; empty leaves the env unset so the daemon
// default (warn) applies.

const shortTitleGateEnvName = "- name: BEADS_SHORT_TITLE_GATE"

func TestBeadsShortTitleGateEnv_DefaultUnset(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t)
	if !strings.Contains(out, "BEADS_HTTP_ADDR") {
		t.Fatalf("control: beads Deployment did not render the daemon env:\n%s", out)
	}
	if strings.Contains(out, "BEADS_SHORT_TITLE_GATE") {
		t.Errorf("default render must not set BEADS_SHORT_TITLE_GATE (beads.shortTitleGate defaults empty → daemon default warn)")
	}
}

func TestBeadsShortTitleGateEnv_Set(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"warn", "enforce", "off"} {
		t.Run(mode, func(t *testing.T) {
			lines := strings.Split(helmBeadsDeployment(t, "--set", "beads.shortTitleGate="+mode), "\n")
			for i, l := range lines {
				if strings.TrimSpace(l) != shortTitleGateEnvName {
					continue
				}
				if want := `value: "` + mode + `"`; i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != want {
					t.Fatalf("BEADS_SHORT_TITLE_GATE must be followed by %s; got:\n%s", want, strings.Join(lines[i:min(i+2, len(lines))], "\n"))
				}
				return
			}
			t.Fatalf("expected BEADS_SHORT_TITLE_GATE in render, not found")
		})
	}
}
