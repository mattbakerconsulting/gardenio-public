package tests

import (
	"strings"
	"testing"
)

// global.tolerations is a chart-wide scheduling knob: it is applied to every
// pod AND every pre/post-install hook Job via the gardenio.tolerations helper,
// then per-component tolerations are appended on top.
//
// Regression: POC ephemeral deploys (poc-gardenio-eks) hard-failed on every tag
// because the only autoscaling Karpenter pool is tainted
// gardenio.agent=true:NoSchedule and the ephemeral stack + pre-install hooks
// carried no toleration, so helm --wait stalled on "Insufficient cpu". The
// psa-labels pre-install hook (hook-weight -20, runs first) was the canary.
// (ka-0000005127)

// helm --set list syntax for a single toleration entry.
var globalAgentToleration = []string{
	`global.tolerations[0].key=gardenio.agent`,
	`global.tolerations[0].operator=Equal`,
	`global.tolerations[0].value=true`,
	`global.tolerations[0].effect=NoSchedule`,
}

func TestGlobalTolerations_DefaultAbsentOnPSAHook(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/namespace-psa.yaml")
	if strings.Contains(out, "tolerations:") {
		t.Errorf("psa-labels hook should have NO tolerations block by default; got:\n%s", out)
	}
}

func TestGlobalTolerations_AppliedToPSAHook(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/namespace-psa.yaml", globalAgentToleration...)
	if !strings.Contains(out, "tolerations:") {
		t.Fatalf("psa-labels hook should carry a tolerations block when global.tolerations is set; got:\n%s", out)
	}
	if !strings.Contains(out, "key: gardenio.agent") {
		t.Errorf("psa-labels hook tolerations should include the gardenio.agent key; got:\n%s", out)
	}
}

func TestGlobalTolerations_AppliedToAdoptPVCsHook(t *testing.T) {
	t.Parallel()
	// pre-upgrade hook: render in an upgrade-shaped invocation by enabling a
	// component with persistence so the hook body is non-empty.
	out := helmTemplate(t, "templates/pre-upgrade-adopt-pvcs.yaml",
		append([]string{
			"gitlabBridge.enabled=true",
			"gitlabBridge.persistence.enabled=true",
		}, globalAgentToleration...)...)
	if !strings.Contains(out, "key: gardenio.agent") {
		t.Errorf("adopt-pvcs hook should carry the gardenio.agent toleration when global.tolerations is set; got:\n%s", out)
	}
}

func TestGlobalTolerations_AppliedToComponentPod(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml", globalAgentToleration...)
	if !strings.Contains(out, "key: gardenio.agent") {
		t.Errorf("beads deployment should carry the global gardenio.agent toleration; got:\n%s", out)
	}
}

// forwardauth (oauth2-proxy) is the browser auth front door and runs in every
// POC ephemeral namespace. MR !4318 wired the gardenio.tolerations helper into
// 34 templates but MISSED forwardauth/deployment.yaml (it had no scheduling
// block at all), so on poc-gardenio-eks the forwardauth pods stayed Pending with
// "did not tolerate gardenio.agent=true:NoSchedule" and the ephemeral deploy
// still timed out AFTER psa-labels finally scheduled. (gd-UkCGPlvS8Q)
func TestGlobalTolerations_AppliedToForwardAuth(t *testing.T) {
	t.Parallel()
	flags := append([]string{
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.oidcIssuerUrl=https://issuer.example.com/realms/x",
		"keycloak.forwardAuth.clientId=gardenio",
	}, globalAgentToleration...)
	out := helmTemplate(t, "templates/forwardauth/deployment.yaml", flags...)
	if !strings.Contains(out, "key: gardenio.agent") {
		t.Errorf("forwardauth deployment should carry the global gardenio.agent toleration; got:\n%s", out)
	}
}

func TestGlobalTolerations_DefaultAbsentOnForwardAuth(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/deployment.yaml",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.oidcIssuerUrl=https://issuer.example.com/realms/x",
		"keycloak.forwardAuth.clientId=gardenio",
	)
	if strings.Contains(out, "tolerations:") {
		t.Errorf("forwardauth deployment should have NO tolerations block by default; got:\n%s", out)
	}
}

func TestGlobalTolerations_AppendedAfterComponentTolerations(t *testing.T) {
	t.Parallel()
	// Both global and per-component tolerations must appear (global ++ local).
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		append([]string{
			`beads.tolerations[0].key=dedicated`,
			`beads.tolerations[0].operator=Exists`,
		}, globalAgentToleration...)...)
	if !strings.Contains(out, "key: gardenio.agent") {
		t.Errorf("global toleration missing when a component toleration is also set; got:\n%s", out)
	}
	if !strings.Contains(out, "key: dedicated") {
		t.Errorf("component toleration missing when global toleration is also set; got:\n%s", out)
	}
}

func TestGlobalTolerations_DefaultAbsentOnComponentPod(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml")
	if strings.Contains(out, "tolerations:") {
		t.Errorf("beads deployment should have NO tolerations block by default; got:\n%s", out)
	}
}
