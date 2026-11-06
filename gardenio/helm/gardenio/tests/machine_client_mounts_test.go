package tests

import (
	"strings"
	"testing"
)

// Role-bead machine_client allowlist (escalation routing Task 15 C5,
// gd-yT5xAPB0uJ): agents.machineClientMounts renders the controller's
// GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS, and only when set.

const machineClientMountsEnv = "GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS"

func TestMachineClientMounts_DefaultRendersNothing(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl, "agents.enabled=true")
	if strings.Contains(out, machineClientMountsEnv) {
		t.Errorf("%s must not render when agents.machineClientMounts is empty", machineClientMountsEnv)
	}
}

func TestMachineClientMounts_JoinsList(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"agents.machineClientMounts={gardenio-cluster-coordinator,gardenio-other}",
	)
	if got, ok := slackViewEnvValue(t, out, machineClientMountsEnv); !ok || got != "gardenio-cluster-coordinator,gardenio-other" {
		t.Errorf("%s = %q (present=%v), want gardenio-cluster-coordinator,gardenio-other", machineClientMountsEnv, got, ok)
	}
}

func TestMachineClientMounts_K3sDeployValues(t *testing.T) {
	t.Parallel()
	d := renderDeployment(t, objectStoreCredsTpl, []string{k3sOverlayPath(t)})
	if e, ok := d.env("", machineClientMountsEnv); !ok || e.Value != "gardenio-cluster-coordinator" {
		t.Errorf("%s = %q (present %v), want gardenio-cluster-coordinator", machineClientMountsEnv, e.Value, ok)
	}
}

// gd-pnrrLJNUHn: with the k3s overlay (public issuer, in-cluster TLS), the
// controller hands machine-client pods the Service discovery URL and the
// keycloak-runtime TLS Secret for its ca.crt.
func TestMachineClientMounts_K3sInClusterIssuer(t *testing.T) {
	t.Parallel()
	d := renderDeployment(t, objectStoreCredsTpl, []string{k3sOverlayPath(t)})
	if e, ok := d.env("", "GARDENIO_AGENT_OIDC_DISCOVERY_URL"); !ok || !strings.HasPrefix(e.Value, "https://") || !strings.Contains(e.Value, ".svc:") {
		t.Errorf("GARDENIO_AGENT_OIDC_DISCOVERY_URL = %q (present %v), want the https Service URL", e.Value, ok)
	}
	if e, ok := d.env("", "GARDENIO_AGENT_OIDC_CA_SECRET"); !ok || !strings.HasSuffix(e.Value, "-tls") {
		t.Errorf("GARDENIO_AGENT_OIDC_CA_SECRET = %q (present %v), want the keycloak-runtime TLS Secret", e.Value, ok)
	}
}

func TestMachineClientMounts_NoMountsNoIssuerEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl, "agents.enabled=true")
	if strings.Contains(out, "GARDENIO_AGENT_OIDC_DISCOVERY_URL") || strings.Contains(out, "GARDENIO_AGENT_OIDC_CA_SECRET") {
		t.Error("in-cluster issuer env must not render without agents.machineClientMounts")
	}
}
