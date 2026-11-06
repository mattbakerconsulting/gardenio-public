package tests

import (
	"strings"
	"testing"
)

// Regression guards for gd-JgE3i3knhO — controller RBAC gap for the bridge
// token rotator (ka-0000002353).
//
// When agents.bridgeTokenRotation.enabled=true the controller's in-namespace
// Role must grant the verbs the rotator's Rotate() path calls (see
// controller/internal/bridgetokenrotator/rotator.go):
//   - secrets.Update on the configured secretName (default bridge-tokens) AND
//     the legacy bridge-fleet-tokens name during the fleet→bridge rename
//     compat window (ka-0000005788)
//   - deployments.Get + Update on the configured bridge deployments
//
// When disabled (default), those rules MUST be omitted so the controller's
// blast radius stays minimal.

func TestBridgeTokenRotatorRBAC_DisabledByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml",
		"agents.enabled=true",
	)
	if strings.Contains(out, "bridge-fleet-tokens") || strings.Contains(out, "bridge-tokens") {
		t.Errorf("controller Role must NOT reference the bridge token secret when rotator is disabled; got:\n%s", out)
	}
	if strings.Contains(out, `resources: ["deployments"]`) {
		t.Errorf("controller Role must NOT grant deployments verbs when rotator is disabled; got:\n%s", out)
	}
}

func TestBridgeTokenRotatorRBAC_EnabledGrantsSecretUpdate(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
	)
	// The rotator's patchSecret path needs Update on the configured secret
	// (Get is already granted by the standing `secrets: ["get"]` rule). The
	// default secretName renamed to "bridge-tokens"; the legacy
	// "bridge-fleet-tokens" name is also granted during the compat window.
	if !strings.Contains(out, `resourceNames: ["bridge-tokens", "bridge-fleet-tokens"]`) {
		t.Errorf("controller Role must scope the secret Update verb to both bridge-tokens and the legacy bridge-fleet-tokens; got:\n%s", out)
	}
	if !strings.Contains(out, `verbs: ["update"]`) {
		t.Errorf("controller Role must grant secrets.update for the rotator; got:\n%s", out)
	}
}

func TestBridgeTokenRotatorRBAC_EnabledGrantsDeploymentBounce(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
	)
	// The rotator's bounceDeployment path needs Get + Update on each bridge
	// Deployment so it can stamp the rotation annotation.
	if !strings.Contains(out, `resources: ["deployments"]`) {
		t.Errorf("controller Role must grant apps/deployments rule when rotator is enabled; got:\n%s", out)
	}
	if !strings.Contains(out, `verbs: ["get", "update"]`) {
		t.Errorf("controller Role must grant deployments get+update for the rotator; got:\n%s", out)
	}
	for _, fleet := range []string{"slack-bridge", "gitlab-bridge", "jira-bridge"} {
		if !strings.Contains(out, `- "`+fleet+`"`) {
			t.Errorf("controller Role must list %q in the deployments resourceNames; got:\n%s", fleet, out)
		}
	}
}

func TestBridgeTokenRotatorRBAC_CustomSecretName(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
		"agents.bridgeTokenRotation.secretName=custom-fleet-tokens",
	)
	// A custom secretName is honoured, and the legacy bridge-fleet-tokens
	// name is still appended for the compat window (ka-0000005788).
	if !strings.Contains(out, `resourceNames: ["custom-fleet-tokens", "bridge-fleet-tokens"]`) {
		t.Errorf("controller Role must honor a custom secretName and append the legacy name; got:\n%s", out)
	}
	if strings.Contains(out, `resourceNames: ["bridge-tokens"`) {
		t.Errorf("controller Role must NOT still reference the default secret name when overridden; got:\n%s", out)
	}
}

// When the operator pins the rotation secretName to the legacy
// bridge-fleet-tokens name, the resourceNames list must NOT duplicate it.
func TestBridgeTokenRotatorRBAC_LegacySecretNameNotDuplicated(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml",
		"agents.enabled=true",
		"agents.bridgeTokenRotation.enabled=true",
		"agents.bridgeTokenRotation.secretName=bridge-fleet-tokens",
	)
	if !strings.Contains(out, `resourceNames: ["bridge-fleet-tokens"]`) {
		t.Errorf("controller Role must list the legacy name exactly once; got:\n%s", out)
	}
}
