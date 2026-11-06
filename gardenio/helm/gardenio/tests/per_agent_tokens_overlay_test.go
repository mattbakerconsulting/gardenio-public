package tests

import (
	"strings"
	"testing"
)

// P7 Task 2: the live overlay turns per-agent tokens on and gives agents a
// coopmux token that is not kd-beads-token; coopmux's own daemon bearer stays
// the beads token so coopmux keeps reaching the daemon.
func TestOverlay_PerAgentTokensOn(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, "templates/controller/deployment.yaml",
		[]string{"../../../deploy-values/gardenio-k3s.yaml"})
	if !strings.Contains(out, "name: PER_AGENT_TOKENS") {
		t.Fatal("deploy-values/gardenio-k3s.yaml must set agents.perAgentTokens=true (controller env PER_AGENT_TOKENS)")
	}
	if !strings.Contains(out, "value: coopmux-agent-token") {
		t.Fatal("controller COOPMUX_TOKEN_SECRET must name coopmux-agent-token, not the beads token secret")
	}
}

// gd-ke6PC7bRto: with coopmux.machinePrincipal on (it is, on k3s) coopmux
// carries no BEADS_TOKEN at all: every coopmux call is the machine principal,
// and the daemon is about to stop accepting the shared static token. With it off (the
// rollback) the beads-token bearer returns, and it is never the agent-facing
// coopmux token.
func TestCoopmux_DaemonBearerIsMachinePrincipalNotBeadsToken(t *testing.T) {
	t.Parallel()
	overlay := []string{"../../../deploy-values/gardenio-k3s.yaml"}
	out := helmTemplateShowOnlyWithValues(t, "templates/coopmux/deployment.yaml", overlay)
	if strings.Contains(out, "- name: BEADS_TOKEN\n") {
		t.Fatal("coopmux must not carry BEADS_TOKEN with coopmux.machinePrincipal.enabled (k3s overlay)")
	}
	if strings.Contains(out, "kd-beads-token") {
		t.Fatal("coopmux must not reference kd-beads-token with the machine principal on")
	}
	if !strings.Contains(out, "- name: BEADS_OIDC_CLIENT_ID") {
		t.Fatal("coopmux must render BEADS_OIDC_CLIENT_ID with the machine principal on")
	}
	// Positive control: the agent-facing auth token IS the split secret.
	j := strings.Index(out, "- name: COOP_MUX_AUTH_TOKEN")
	if j < 0 || !strings.Contains(out[j:min(len(out), j+200)], "coopmux-agent-token") {
		t.Fatal("coopmux COOP_MUX_AUTH_TOKEN must come from coopmux-agent-token")
	}

	// Rollback: machine principal off brings the beads-token bearer back.
	off := renderDeployment(t, "templates/coopmux/deployment.yaml", []string{k3sOverlayPath(t)}, "coopmux.machinePrincipal.enabled=false")
	e, ok := off.env("", "BEADS_TOKEN")
	if !ok || e.ValueFrom == nil {
		t.Fatal("coopmux must fall back to BEADS_TOKEN from a Secret with the machine principal off (rollback)")
	}
	ref, _ := e.ValueFrom["secretKeyRef"].(map[string]any)
	if ref == nil {
		t.Fatalf("coopmux BEADS_TOKEN must be a secretKeyRef with the machine principal off, got %v", e.ValueFrom)
	}
	if ref["name"] == "coopmux-agent-token" {
		t.Fatal("coopmux BEADS_TOKEN must come from the beads token secret, not the agent-facing coopmux token")
	}
}
