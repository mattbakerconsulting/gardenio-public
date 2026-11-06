package tests

import (
	"strings"
	"testing"
)

// The agent NATS token must exist in EVERY namespace agent pods route to.
// When the gVisor namespace split is active (gvisor.enabled +
// gvisor.namespace.name), runsc agents schedule in the gVisor namespace and
// the controller's verifySecretsExist gate defers pod creation until the
// referenced -nats-agent-creds Secret exists THERE; MicroVM launches likewise
// resolve the token in the routed namespace (resolveMicrovmNatsToken). A
// release-namespace-only render wedged every plan-gardener on prod the moment
// subject isolation went live (bug ka-0000004273, E2E gate ka-0000000183).

func TestNatsAgentCredsRenderedInGvisorNamespace(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/nats/agent-creds-secret.yaml",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"externalSecrets.natsAgentToken.remoteRef=gardenio/prod/nats-agent-token",
		"gvisor.enabled=true",
		"gvisor.namespace.name=gardenio-gvisor",
	)
	// The release namespace varies (helm template defaults it from the kube
	// context), so assert the gVisor copy plus the total object count.
	if !strings.Contains(out, "namespace: gardenio-gvisor") {
		t.Errorf("agent-creds must render in the gVisor namespace too, got:\n%s", out)
	}
	if got := strings.Count(out, "kind: ExternalSecret"); got != 2 {
		t.Errorf("expected exactly 2 ExternalSecrets (release + gVisor ns), got %d", got)
	}
}

func TestNatsAgentCredsSingleNamespaceWithoutGvisor(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/nats/agent-creds-secret.yaml",
		"nats.auth.subjectIsolation.agentToken=dummy",
	)
	if got := strings.Count(out, "kind: Secret"); got != 1 {
		t.Errorf("without the gVisor split exactly 1 Secret must render, got %d", got)
	}
	if strings.Contains(out, "gardenio-gvisor") {
		t.Error("no gVisor-namespace object may render when the split is off")
	}
}
