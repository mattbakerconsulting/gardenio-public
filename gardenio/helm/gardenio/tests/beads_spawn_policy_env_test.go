package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// BEADS_SPAWN_POLICY_ENABLED turns on the kbeads spawn-policy evaluator
// (kbeads/internal/server/spawn_policy.go). The chart owns it through
// beads.spawnPolicyEnabled; it is off by default and on for the k3s
// deployment (deploy-values/gardenio-k3s.yaml).

const (
	beadsDeploymentTemplate = "templates/beads/deployment.yaml"
	spawnPolicyEnvName      = "- name: BEADS_SPAWN_POLICY_ENABLED"
	k3sDeployValues         = "../../../../deploy-values/gardenio-k3s.yaml"
)

// helmBeadsDeployment renders the beads Deployment with raw helm args (the
// shared helmTemplate helper only takes --set flags, which cannot express
// --values).
func helmBeadsDeployment(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"template", "test", chartDir(t), "--show-only", beadsDeploymentTemplate}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// assertSpawnPolicyEnvOn requires the env name present with value "true" on
// the very next line.
func assertSpawnPolicyEnvOn(t *testing.T, out string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != spawnPolicyEnvName {
			continue
		}
		if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != `value: "true"` {
			t.Fatalf("BEADS_SPAWN_POLICY_ENABLED must be followed by value: \"true\"; got:\n%s", strings.Join(lines[i:min(i+2, len(lines))], "\n"))
		}
		return
	}
	t.Fatalf("expected BEADS_SPAWN_POLICY_ENABLED in render, not found:\n%s", out)
}

func TestBeadsSpawnPolicyEnv_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t)
	if !strings.Contains(out, "BEADS_HTTP_ADDR") {
		t.Fatalf("control: beads Deployment did not render the daemon env:\n%s", out)
	}
	if strings.Contains(out, "BEADS_SPAWN_POLICY_ENABLED") {
		t.Errorf("default render must not set BEADS_SPAWN_POLICY_ENABLED (beads.spawnPolicyEnabled defaults false)")
	}
}

func TestBeadsSpawnPolicyEnv_SetOn(t *testing.T) {
	t.Parallel()
	assertSpawnPolicyEnvOn(t, helmBeadsDeployment(t, "--set", "beads.spawnPolicyEnabled=true"))
}

func TestBeadsSpawnPolicyEnv_K3sDeployValuesOn(t *testing.T) {
	t.Parallel()
	assertSpawnPolicyEnvOn(t, helmBeadsDeployment(t, "--values", k3sDeployValues))
}
