package tests

import (
	"regexp"
	"strings"
	"testing"
)

// Regression guards for ka-0000004295 — the bridge-token rotator MUST write the
// same K8s Secret the beads daemon reads.
//
// The rotator's Rotate() path (controller/internal/bridgetokenrotator/rotator.go)
// patches the per-bridge keys in BRIDGE_TOKEN_ROTATION_SECRET and the daemon
// authenticates bridge callers against the secret resolved by the
// gardenio.beads.bridgeTokenSecretName helper. If the two diverge, a live
// rotation mints fresh tokens the daemon never sees and bridge auth breaks at
// the TTL boundary — exactly the silent breakage the preprod soak exists to
// catch. Before the fix, a values file that pinned ONLY
// beads.bridgeFleetTokenSecret left the rotator on its independent
// "bridge-tokens" Go default; these tests lock the coupling in.

// rotationSecretEnv extracts the BRIDGE_TOKEN_ROTATION_SECRET env value from a
// rendered controller Deployment.
func rotationSecretEnv(t *testing.T, out string) string {
	t.Helper()
	// Match:  - name: BRIDGE_TOKEN_ROTATION_SECRET\n              value: "<x>"
	re := regexp.MustCompile(`name: BRIDGE_TOKEN_ROTATION_SECRET\s*\n\s*value: "([^"]*)"`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("BRIDGE_TOKEN_ROTATION_SECRET env not found in rendered controller Deployment; got:\n%s", out)
	}
	return m[1]
}

func TestBridgeTokenRotatorSecret_InheritsDaemonDefault(t *testing.T) {
	t.Parallel()
	// No pins: rotator inherits the chart default "bridge-tokens", same as the
	// daemon's bridgeTokenSecretName helper default.
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
	)
	if got := rotationSecretEnv(t, out); got != "bridge-tokens" {
		t.Errorf("rotator secret should default to the daemon default %q; got %q", "bridge-tokens", got)
	}
}

func TestBridgeTokenRotatorSecret_InheritsDeprecatedAliasPin(t *testing.T) {
	t.Parallel()
	// The prod case: the daemon is pinned to bridge-fleet-tokens via
	// the DEPRECATED alias and the rotator secretName is left unset. The rotator
	// MUST follow the daemon onto bridge-fleet-tokens, not strand on
	// bridge-tokens.
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
		"beads.bridgeFleetTokenSecret=bridge-fleet-tokens",
	)
	if got := rotationSecretEnv(t, out); got != "bridge-fleet-tokens" {
		t.Errorf("rotator secret must follow the daemon's bridgeFleetTokenSecret pin to %q; got %q — a live rotation would strand the daemon on a stale secret (ka-0000004295)", "bridge-fleet-tokens", got)
	}
}

func TestBridgeTokenRotatorSecret_InheritsCanonicalPin(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
		"beads.bridgeTokenSecret=my-bridge-tokens",
	)
	if got := rotationSecretEnv(t, out); got != "my-bridge-tokens" {
		t.Errorf("rotator secret must follow the daemon's canonical bridgeTokenSecret pin to %q; got %q", "my-bridge-tokens", got)
	}
}

func TestBridgeTokenRotatorSecret_ExplicitOverrideWins(t *testing.T) {
	t.Parallel()
	// An explicit rotator secretName is the deliberate decouple escape hatch and
	// must still win over the daemon's resolved name.
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
		"beads.bridgeFleetTokenSecret=bridge-fleet-tokens",
		"agents.bridgeTokenRotation.secretName=explicit-secret",
	)
	if got := rotationSecretEnv(t, out); got != "explicit-secret" {
		t.Errorf("explicit rotator secretName must win; got %q", got)
	}
}

// TestBridgeTokenRotatorSecret_RBACCoversWrittenSecret guards that the
// controller Role's Update verb is scoped to the SAME secret the rotator
// writes — so the daemon-pin case cannot render an env the RBAC forbids.
func TestBridgeTokenRotatorSecret_RBACCoversWrittenSecret(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
		"beads.bridgeFleetTokenSecret=bridge-fleet-tokens",
	)
	written := rotationSecretEnv(t, dep)

	rbac := helmTemplate(t, "templates/controller/rbac.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
		"beads.bridgeFleetTokenSecret=bridge-fleet-tokens",
	)
	if !strings.Contains(rbac, `"`+written+`"`) {
		t.Errorf("controller Role must scope secrets.update to the secret the rotator writes (%q); rbac:\n%s", written, rbac)
	}
}
