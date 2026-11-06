package tests

import (
	"strings"
	"testing"
)

// BEADS_BRIDGE_SUBJECTS lists the fleet subjects whose X-Operated-By relay the
// daemon trusts for access control (kbeads/internal/server/relay_trust.go, P7
// Task 3, gd-wStnnYEt3M). beads.bridgeSubjects defaults empty so a default
// render leaves the env unset and the daemon's own defaultBridgeSubjects
// applies. A non-empty chart default would render "github-bridge" with
// githubBridge.enabled=false (TestGithubBridge_DisabledByDefault).

func TestBeadsBridgeSubjectsEnv_DefaultUnset(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t)
	if !strings.Contains(out, "BEADS_HTTP_ADDR") {
		t.Fatalf("control: beads Deployment did not render the daemon env:\n%s", out)
	}
	if strings.Contains(out, "BEADS_BRIDGE_SUBJECTS") {
		t.Errorf("default render must not set BEADS_BRIDGE_SUBJECTS (beads.bridgeSubjects defaults empty → daemon defaultBridgeSubjects)")
	}
}

func TestBeadsBridgeSubjectsEnv_Set(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t, "--set", "beads.bridgeSubjects={slack-bridge,acme-bridge}")
	if got, want := envValue(t, out, "BEADS_BRIDGE_SUBJECTS"), "slack-bridge,acme-bridge"; got != want {
		t.Errorf("BEADS_BRIDGE_SUBJECTS = %q, want %q", got, want)
	}
}
