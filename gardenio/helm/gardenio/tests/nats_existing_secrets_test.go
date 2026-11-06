package tests

import (
	"regexp"
	"strings"
	"testing"
)

// nats.auth.subjectIsolation.existingSecrets (gd-XieCvZHhKN): on a cluster
// without External Secrets the operator creates both NATS creds Secrets, so
// the chart must render neither while every client still points at the same
// fixed names.

var (
	infraCredsObject = regexp.MustCompile(`(?m)^  name: test-nats-infra-creds$`)
	agentCredsObject = regexp.MustCompile(`(?m)^  name: test-nats-agent-creds$`)
)

func TestNatsExistingSecretsRendersNoCredsSecret(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "", "agents.enabled=true", "nats.auth.subjectIsolation.existingSecrets=true")
	if infraCredsObject.MatchString(out) {
		t.Error("existingSecrets=true must not render the -nats-infra-creds Secret")
	}
	if agentCredsObject.MatchString(out) {
		t.Error("existingSecrets=true must not render the -nats-agent-creds Secret")
	}
	if strings.Contains(out, "templates/nats/infra-creds-secret.yaml") ||
		strings.Contains(out, "templates/nats/agent-creds-secret.yaml") {
		t.Error("the creds Secret templates must render empty under existingSecrets=true")
	}
	// The clients still reference the operator-created Secrets by name.
	for _, want := range []string{
		"name: test-nats-infra-creds",
		"value: test-nats-agent-creds", // controller NATS_AGENT_USER_SECRET
		"name: NATS_INFRA_TOKEN",
		"name: NATS_AGENT_TOKEN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("existingSecrets=true must keep the client wiring; missing %q", want)
		}
	}
}

func TestNatsExistingSecretsDefaultStillRendersCredsSecrets(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "",
		"nats.auth.subjectIsolation.infraToken=dummy-infra",
		"nats.auth.subjectIsolation.agentToken=dummy-agent",
	)
	if !infraCredsObject.MatchString(out) || !agentCredsObject.MatchString(out) {
		t.Error("with existingSecrets unset the chart must still render both creds Secrets")
	}
}

func TestNatsExistingSecretsRefusesInlineTokens(t *testing.T) {
	t.Parallel()
	for _, extra := range [][]string{
		{"nats.auth.subjectIsolation.infraToken=dummy"},
		{"nats.auth.subjectIsolation.agentToken=dummy"},
		{
			"externalSecrets.enabled=true",
			"externalSecrets.secretStoreName=aws-secrets-manager",
			"externalSecrets.natsInfraToken.remoteRef=gardenio/prod/nats-infra-token",
		},
	} {
		args := []string{}
		for _, s := range append(append(append([]string{}, subjectIsolationFlags...),
			"nats.auth.subjectIsolation.existingSecrets=true"), extra...) {
			args = append(args, "--set", s)
		}
		out := helmTemplateExpectError(t, "", args...)
		if !strings.Contains(out, "existingSecrets is exclusive") {
			t.Errorf("%v: expected the exclusivity failure, got:\n%s", extra, out)
		}
	}
}
