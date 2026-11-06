package tests

import (
	"strings"
	"testing"
)

// Per-run git credentials for MicroVM satellites (ka-0000004762): the factory
// token that mints per-run GitLab project access tokens must reach the
// controller ONLY as a Secret env-ref (never a plaintext chart value), and
// none of the git-credential env may render when the feature (or the whole
// microvm block) is off.

// microvmBaseFlags satisfies the `required` guards on the microvm block.
func microvmBaseFlags(extra ...string) []string {
	base := []string{
		"agents.enabled=true",
		"agents.microvm.enabled=true",
		"agents.microvm.launcherRoleARN=arn:aws:iam::1:role/launcher",
		"agents.microvm.region=us-east-2",
		"agents.microvm.imageIdentifier=img-arn",
		"agents.microvm.executionRoleARN=arn:aws:iam::1:role/exec",
	}
	return append(base, extra...)
}

func TestMicrovmGitFactoryTokenRendersAsSecretRef(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		microvmBaseFlags(
			"agents.microvm.gitCredentials.factoryTokenSecret=microvm-git-factory",
			"agents.microvm.gitCredentials.gitlabBaseURL=https://gitlab.example.com",
		)...)
	block := blockAfter(out, "MICROVM_GIT_FACTORY_TOKEN")
	if block == "" {
		t.Fatal("expected MICROVM_GIT_FACTORY_TOKEN env var in rendered controller deployment")
	}
	if !strings.Contains(block, "secretKeyRef") || !strings.Contains(block, `name: "microvm-git-factory"`) {
		t.Errorf("factory token must render as a secretKeyRef to the configured Secret, got:\n%s", block)
	}
	if !strings.Contains(block, "key: token") {
		t.Errorf("factory token secretKeyRef must read the \"token\" key, got:\n%s", block)
	}
	if !strings.Contains(out, "MICROVM_GITLAB_BASE_URL") || !strings.Contains(out, "https://gitlab.example.com") {
		t.Error("expected MICROVM_GITLAB_BASE_URL to render when gitlabBaseURL is set")
	}
}

func TestMicrovmGitEnvAbsentByDefault(t *testing.T) {
	t.Parallel()
	// microvm on, gitCredentials unset → shared-fallback mode, no git env.
	out := helmTemplate(t, "templates/controller/deployment.yaml", microvmBaseFlags()...)
	for _, envName := range []string{"MICROVM_GIT_FACTORY_TOKEN", "MICROVM_GITLAB_BASE_URL"} {
		if strings.Contains(out, envName) {
			t.Errorf("%s must not render when gitCredentials is unset", envName)
		}
	}
}

func TestMicrovmGitEnvAbsentWhenMicrovmDisabled(t *testing.T) {
	t.Parallel()
	// Whole microvm block off → git values are ignored even if set (byte-
	// identical render guarantee of the master switch).
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.microvm.gitCredentials.factoryTokenSecret=microvm-git-factory",
	)
	if strings.Contains(out, "MICROVM_GIT") {
		t.Error("no MICROVM_GIT_* env may render while agents.microvm.enabled=false")
	}
}

// The factory-token ExternalSecret (externalSecrets.microvmGitFactoryToken):
// materializes the operator-seeded SM value as the K8s Secret the controller
// env ref reads. The env ref itself is `optional: true` so the chart wiring
// can deploy before the operator seeds the store (shared-fallback until then).
func TestMicrovmGitFactoryExternalSecretRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/external-secrets.yaml",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"externalSecrets.microvmGitFactoryToken.remoteRef=gardenio/prod/microvm-git-factory-token",
		"externalSecrets.microvmGitFactoryToken.property=token",
	)
	for _, want := range []string{
		"name: gardenio-microvm-git-factory-token",
		`key: "gardenio/prod/microvm-git-factory-token"`,
		`property: "token"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("factory-token ExternalSecret missing %q in:\n%s", want, out)
		}
	}
}

func TestMicrovmGitFactoryExternalSecretAbsentByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/external-secrets.yaml",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"externalSecrets.beadsToken.remoteRef=gardenio/prod/kd-beads-token",
	)
	if strings.Contains(out, "microvm-git-factory-token") {
		t.Error("factory-token ExternalSecret must not render without its remoteRef")
	}
}

func TestMicrovmGitFactoryTokenEnvRefIsOptional(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		microvmBaseFlags(
			"agents.microvm.gitCredentials.factoryTokenSecret=microvm-git-factory",
		)...)
	block := blockAfter(out, "MICROVM_GIT_FACTORY_TOKEN")
	if !strings.Contains(block, "optional: true") {
		t.Errorf("factory-token secretKeyRef must be optional (pre-seed deploys run shared-fallback), got:\n%s", block)
	}
}
