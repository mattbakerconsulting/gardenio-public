package config

import "testing"

func TestAgentSecretAllowed(t *testing.T) {
	if !(&Config{}).AgentSecretAllowed("gardenio-keycloak-runtime-admin") {
		t.Error("empty allow-list must allow every name (the forbidden markers still apply)")
	}

	c := &Config{AgentSecretAllowlist: []string{"*-git-credentials", "gardenio-claude-oauth-*", "claude-oauth", "[bad"}}
	for name, want := range map[string]bool{
		"gardenio-git-credentials":             true,
		"Gardenio-Git-Credentials":             true,
		"gardenio-claude-oauth-fimbaz":         true,
		"claude-oauth":                         true,
		"claude-oauth-extra":                   false,
		"gardenio-machine-gardenio-controller": false,
		"gardenio-keycloak-runtime-admin":      false,
		"gardenio-forwardauth-secrets":         false,
		"[bad":                                 false,
		"":                                     false,
	} {
		if got := c.AgentSecretAllowed(name); got != want {
			t.Errorf("AgentSecretAllowed(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParse_AgentSecretAllowlist(t *testing.T) {
	t.Setenv("AGENT_SECRET_ALLOWLIST", "*-git-credentials, infra-aws-readonly")
	got := Parse().AgentSecretAllowlist
	if len(got) != 2 || got[0] != "*-git-credentials" || got[1] != "infra-aws-readonly" {
		t.Errorf("AgentSecretAllowlist = %q", got)
	}
}
