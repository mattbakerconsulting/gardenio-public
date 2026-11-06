package tests

import (
	"strings"
	"testing"
)

// GitHub App per-agent installation tokens (task gd-fzNBBfmdEO, epic
// gd-BHLDnjX1Hw, plan gardenio/plans/github-app-onboarding.md §4.2/§6/§8 P2).
//
// Invariants locked here:
//   - agents.githubApp.enabled defaults false and renders nothing GitHub-App.
//   - Enabled: controller gets the App id + a key FILE PATH (never the key in
//     env), the key Secret is mounted read-only, and RBAC lets it update/list
//     per-agent token Secrets.
//   - The private key Secret is NEVER mirrored into the gVisor namespace: a
//     mirror naming it (or its SSM path) fails the render.

const (
	ghAppOn     = "agents.enabled=true,agents.githubApp.enabled=true,agents.githubApp.appId=5029022"
	ghKeySecret = "gardenio-github-app-private-key"
)

func TestGithubApp_DisabledByDefault(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true")
	for _, forbidden := range []string{"GITHUB_APP_", "github-app-private-key", ghKeySecret} {
		if strings.Contains(dep, forbidden) {
			t.Errorf("controller Deployment must not reference %q when githubApp is disabled", forbidden)
		}
	}
	rbac := helmTemplate(t, "templates/controller/rbac.yaml", "agents.enabled=true")
	if strings.Contains(rbac, `"update"`) && strings.Contains(rbac, "Per-agent token provisioning") {
		t.Error("per-agent token Secret rule must not render when both perAgentTokens and githubApp are off")
	}
}

func TestGithubApp_PerAgentTokenRuleGrantsUpdate(t *testing.T) {
	t.Parallel()
	// Plan §0 item 2 / §4.2 step 5: the rule was create+delete only, so an
	// in-place rewrite of a per-agent token Secret could never work.
	out := helmTemplate(t, "templates/controller/rbac.yaml", "agents.enabled=true,agents.perAgentTokens=true")
	if !strings.Contains(out, `verbs: ["create", "update", "delete", "list"]`) {
		t.Errorf("per-agent token rule must grant create, update, delete, list; got:\n%s", out)
	}
}

// gd-d4YmLsF49g: the token janitor lists agent-token-* Secrets to sweep
// orphans, so perAgentTokens alone (githubApp off) must grant list too.
func TestPerAgentTokens_RBACGrantsListWithoutGithubApp(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml", "agents.enabled=true,agents.perAgentTokens=true,agents.githubApp.enabled=false")
	if !strings.Contains(out, `verbs: ["create", "update", "delete", "list"]`) {
		t.Errorf("token janitor needs list on Secrets when only perAgentTokens is on; got:\n%s", out)
	}
}

func TestGithubApp_EnabledRBACAddsListAndUpdate(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml", ghAppOn)
	if !strings.Contains(out, `verbs: ["create", "update", "delete", "list"]`) {
		t.Errorf("githubApp needs create/update/delete/list on per-agent token Secrets; got:\n%s", out)
	}
}

func TestGithubApp_EnabledWiresKeyAsFileNotEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml", ghAppOn+",agents.githubApp.installationId=170295668")
	for _, want := range []string{
		"name: GITHUB_APP_ENABLED",
		`value: "5029022"`,
		"name: GITHUB_APP_INSTALLATION_ID",
		`value: "170295668"`,
		"name: GITHUB_APP_PRIVATE_KEY_FILE",
		`value: "/etc/gardenio/github-app/private-key"`,
		"secretName: " + ghKeySecret,
		"mountPath: /etc/gardenio/github-app",
		"readOnly: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("controller Deployment missing %q", want)
		}
	}
	// The key must never ride env: no secretKeyRef to the key Secret, no PEM.
	if strings.Contains(out, "secretKeyRef:\n                  name: "+ghKeySecret) || strings.Contains(out, "BEGIN") {
		t.Error("private key must be a mounted file, never an env var")
	}
}

func TestGithubApp_EnabledRequiresAppID(t *testing.T) {
	t.Parallel()
	out := helmTemplateExpectError(t, "templates/controller/deployment.yaml", "--set", "agents.enabled=true,agents.githubApp.enabled=true")
	if !strings.Contains(out, "agents.githubApp.appId is required") {
		t.Errorf("expected appId required error, got:\n%s", out)
	}
}

func TestGithubApp_ExternalSecretSyncsFromSSMIntoReleaseNamespace(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", "templates/controller/github-app-private-key-external-secret.yaml",
		ghAppOn+",agents.githubApp.externalSecret.enabled=true,agents.githubApp.externalSecret.secretStoreName=aws-ssm")
	for _, want := range []string{
		"kind: ExternalSecret",
		"namespace: gardenio",
		"name: " + ghKeySecret,
		"secretKey: private-key",
		"key: \"/gardenio/github-app/agents/private-key\"",
		"name: aws-ssm",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ExternalSecret missing %q:\n%s", want, out)
		}
	}
}

// --- gVisor mirror exclusion (plan §6: "a test should assert it") ---

func TestGithubApp_GvisorMirrorCannotCarryThePrivateKey(t *testing.T) {
	t.Parallel()
	base := gvisorMirrorBase + "," + ghAppOn
	for name, flags := range map[string][]string{
		"mirror named after the key secret": {
			"gvisor.namespace.secretMirrors[0].name=" + ghKeySecret,
			"gvisor.namespace.secretMirrors[0].remoteRef=gardenio/anything",
		},
		"mirror of the SSM path": {
			"gvisor.namespace.secretMirrors[0].name=innocuous-name",
			"gvisor.namespace.secretMirrors[0].remoteRef=/gardenio/github-app/agents/private-key",
		},
		"mirror name mentions github-app": {
			"gvisor.namespace.secretMirrors[0].name=github-app-key-copy",
			"gvisor.namespace.secretMirrors[0].remoteRef=gardenio/x",
		},
	} {
		t.Run(name, func(t *testing.T) {
			args := []string{"--set", base}
			for _, f := range flags {
				args = append(args, "--set", f)
			}
			out := helmTemplateExpectError(t, "templates/gvisor-namespace.yaml", args...)
			if !strings.Contains(out, "must never be mirrored into the gVisor namespace") {
				t.Errorf("expected the exclusion guard to fire, got:\n%s", out)
			}
		})
	}
}

func TestGithubApp_GvisorNamespaceRendersNoKeyAndOrdinaryMirrorsStillWork(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gvisor-namespace.yaml",
		gvisorMirrorBase+","+ghAppOn+","+
			"gvisor.namespace.secretMirrors[0].name=kd-beads-token,"+
			"gvisor.namespace.secretMirrors[0].remoteRef=gardenio/kd-beads-token,"+
			"gvisor.namespace.secretMirrors[0].secretKey=token")
	if strings.Contains(out, ghKeySecret) || strings.Contains(out, "github-app") {
		t.Errorf("gVisor namespace render must never reference the GitHub App key:\n%s", out)
	}
	if !strings.Contains(out, "name: kd-beads-token") {
		t.Error("ordinary mirrors must keep working")
	}
	// The gVisor Role gets update+list on secrets only because githubApp is on.
	if !strings.Contains(out, `verbs: ["get", "create", "delete", "update", "list"]`) {
		t.Errorf("gVisor Role should let the controller refresh/sweep GitHub tokens there:\n%s", out)
	}
}

func TestGithubApp_GvisorRoleUnchangedWhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gvisor-namespace.yaml", gvisorMirrorBase)
	if !strings.Contains(out, `verbs: ["get", "create", "delete"]`) || strings.Contains(out, `"update", "list"`) {
		t.Error("gVisor Role secrets verbs must be unchanged while githubApp is disabled")
	}
}
