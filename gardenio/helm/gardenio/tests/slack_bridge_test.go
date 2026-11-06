package tests

import (
	"strings"
	"testing"
)

// --- slackBridge.beadsViewerURL: BEADS_VIEWER_URL env var plumbing ---

func TestSlackBridge_BeadsViewerURL_DefaultNotPresent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
	)
	if strings.Contains(out, "BEADS_VIEWER_URL") {
		t.Error("BEADS_VIEWER_URL should NOT be present when neither slackBridge.beadsViewerURL nor beadsViewer.ingress is set")
	}
}

func TestSlackBridge_BeadsViewerURL_Explicit(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"slackBridge.beadsViewerURL=https://explicit-viewer.example.com",
	)
	if !strings.Contains(out, "BEADS_VIEWER_URL") {
		t.Fatal("BEADS_VIEWER_URL env var should be present when slackBridge.beadsViewerURL is set")
	}
	if !strings.Contains(out, `value: "https://explicit-viewer.example.com"`) {
		t.Errorf("BEADS_VIEWER_URL should reflect the explicit value, got:\n%s", out)
	}
}

func TestSlackBridge_BeadsViewerURL_FallbackFromIngress(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"beadsViewer.ingress.enabled=true",
		"beadsViewer.ingress.host=viewer.example.com",
	)
	if !strings.Contains(out, "BEADS_VIEWER_URL") {
		t.Fatal("BEADS_VIEWER_URL env var should be present when beadsViewer.ingress.host is set")
	}
	if !strings.Contains(out, `value: "https://viewer.example.com"`) {
		t.Errorf("BEADS_VIEWER_URL should be computed from beadsViewer.ingress.host, got:\n%s", out)
	}
}

func TestSlackBridge_BeadsViewerURL_FallbackWithPathPrefix(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"beadsViewer.ingress.enabled=true",
		"beadsViewer.ingress.host=shared.example.com",
		"beadsViewer.ingress.pathPrefix=/beads",
	)
	if !strings.Contains(out, `value: "https://shared.example.com/beads"`) {
		t.Errorf("BEADS_VIEWER_URL should include pathPrefix, got:\n%s", out)
	}
}

func TestSlackBridge_BeadsViewerURL_ExplicitOverridesIngress(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"slackBridge.beadsViewerURL=https://explicit.example.com",
		"beadsViewer.ingress.enabled=true",
		"beadsViewer.ingress.host=ingress.example.com",
	)
	if !strings.Contains(out, `value: "https://explicit.example.com"`) {
		t.Error("explicit slackBridge.beadsViewerURL should take precedence over beadsViewer.ingress.host")
	}
	if strings.Contains(out, "ingress.example.com") {
		t.Error("ingress host should not appear when explicit URL is set")
	}
}

// --- slackBridge.slack.bareIDPreview: SLACK_BARE_ID_PREVIEW_ENABLED plumbing ---

func TestSlackBridge_BareIDPreview_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
	)
	if strings.Contains(out, "SLACK_BARE_ID_PREVIEW_ENABLED") {
		t.Error("SLACK_BARE_ID_PREVIEW_ENABLED should NOT be present when slackBridge.slack.bareIDPreview is unset (default off)")
	}
}

func TestSlackBridge_BareIDPreview_Enabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"slackBridge.slack.bareIDPreview=true",
	)
	if !strings.Contains(out, "SLACK_BARE_ID_PREVIEW_ENABLED") {
		t.Fatal("SLACK_BARE_ID_PREVIEW_ENABLED env var should be present when slackBridge.slack.bareIDPreview=true")
	}
}

// --- slackBridge.slack.linkHydrate: SLACK_LINK_HYDRATE_ENABLED plumbing ---

func TestSlackBridge_LinkHydrate_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
	)
	if strings.Contains(out, "SLACK_LINK_HYDRATE_ENABLED") {
		t.Error("SLACK_LINK_HYDRATE_ENABLED should NOT be present when slackBridge.slack.linkHydrate is unset (default off)")
	}
}

func TestSlackBridge_LinkHydrate_Enabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"slackBridge.slack.linkHydrate=true",
	)
	if !strings.Contains(out, "SLACK_LINK_HYDRATE_ENABLED") {
		t.Fatal("SLACK_LINK_HYDRATE_ENABLED env var should be present when slackBridge.slack.linkHydrate=true")
	}
}

// --- slackBridge.slack.linkUnfurlChannels: SLACK_LINK_UNFURL_CHANNELS plumbing ---

func TestSlackBridge_LinkUnfurlChannels_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
	)
	if strings.Contains(out, "SLACK_LINK_UNFURL_CHANNELS") {
		t.Error("SLACK_LINK_UNFURL_CHANNELS should NOT be present when slackBridge.slack.linkUnfurlChannels is unset (default off)")
	}
}

func TestSlackBridge_LinkUnfurlChannels_Enabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"slackBridge.slack.linkUnfurlChannels=C0AHE7C0E1G\\,C123ABC",
	)
	if !strings.Contains(out, "SLACK_LINK_UNFURL_CHANNELS") {
		t.Fatal("SLACK_LINK_UNFURL_CHANNELS env var should be present when slackBridge.slack.linkUnfurlChannels is set")
	}
	if !strings.Contains(out, `value: "C0AHE7C0E1G,C123ABC"`) {
		t.Errorf("SLACK_LINK_UNFURL_CHANNELS should reflect the channel list, got:\n%s", out)
	}
}

// --- slackBridge persistence: deadlock-safe PVC binding (ka-0000004683) ---
//
// Re-enabling persistence must NOT recreate the WaitForFirstConsumer deadlock
// (ka-0000002370) that stranded prod on 2026.209.6 for ~11h. When enabled, the
// slack-bridge PVC must reference an Immediate-binding storageClass so a
// brand-new PVC binds at creation time (before the atomic helm_release --wait
// inspects the consumer Deployment) instead of staying Pending until a consumer
// schedules. These tests pin every load-bearing half of that wiring.

func TestSlackBridge_Persistence_DefaultEnabled(t *testing.T) {
	t.Parallel()
	// The chart default is persistence.enabled=true (durable bridge state:
	// AgentCards / ThreadAgents / ChatMessages survive redeploys — ka-0000005549).
	out := helmTemplate(t, "templates/slack-bridge/pvc.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
	)
	if !strings.Contains(out, "kind: PersistentVolumeClaim") {
		t.Fatalf("slack-bridge PVC must render when persistence is enabled by default, got:\n%s", out)
	}
	if !strings.Contains(out, "name: test-slack-bridge-state") {
		t.Errorf("expected the slack-bridge-state PVC, got:\n%s", out)
	}
}

func TestSlackBridge_Persistence_PVCUsesGivenStorageClass(t *testing.T) {
	t.Parallel()
	// Pinning the deadlock-safe path: when the operator sets the Immediate class,
	// the PVC's storageClassName reflects it (this is what breaks the deadlock).
	out := helmTemplate(t, "templates/slack-bridge/pvc.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
		"slackBridge.persistence.enabled=true",
		"slackBridge.persistence.storageClass=gardenio-gp3-immediate",
	)
	if !strings.Contains(out, "storageClassName: gardenio-gp3-immediate") {
		t.Errorf("PVC must reference the Immediate-binding storageClass, got:\n%s", out)
	}
}

func TestSlackBridge_Persistence_DisabledRendersNoPVC(t *testing.T) {
	t.Parallel()
	// The MR !6257 escape hatch stays available: enabled=false → no PVC, volume
	// falls back to emptyDir (behavior-preserving unblock path).
	out, err := helmTemplateAllowFail(t, "templates/slack-bridge/pvc.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
		"slackBridge.persistence.enabled=false",
	)
	if err == nil && strings.Contains(out, "kind: PersistentVolumeClaim") {
		t.Errorf("no PVC should render when persistence is disabled, got:\n%s", out)
	}
	dep := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
		"slackBridge.persistence.enabled=false",
	)
	if !strings.Contains(dep, "emptyDir: {}") {
		t.Errorf("with persistence off, the state volume must be emptyDir, got:\n%s", dep)
	}
	if strings.Contains(dep, "strategy:") {
		t.Errorf("with persistence off, no Recreate strategy should be emitted, got:\n%s", dep)
	}
}

func TestSlackBridge_Persistence_DeploymentUsesRecreateAndPVCVolume(t *testing.T) {
	t.Parallel()
	// With persistence on, the Deployment switches to strategy: Recreate (a
	// single ReadWriteOnce PVC cannot be shared across a RollingUpdate surge)
	// and mounts the PVC as the `state` volume.
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
		"slackBridge.persistence.enabled=true",
	)
	if !strings.Contains(out, "type: Recreate") {
		t.Errorf("persistence-on deployment must use strategy: Recreate, got:\n%s", out)
	}
	if !strings.Contains(out, "claimName: test-slack-bridge-state") {
		t.Errorf("state volume must reference the PVC when persistence is on, got:\n%s", out)
	}
}

func TestSlackBridge_Persistence_AdoptHookIncludesPVC(t *testing.T) {
	t.Parallel()
	// The pre-upgrade adopt-pvcs hook must also see the PVC (create-if-missing /
	// stamp Helm ownership) so a drifted or absent PVC is reconciled with the
	// SAME storageClass the PVC template uses — otherwise the hook could recreate
	// it on the wrong (WFC) class.
	out := helmTemplate(t, "templates/pre-upgrade-adopt-pvcs.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
		"slackBridge.persistence.enabled=true",
		"slackBridge.persistence.storageClass=gardenio-gp3-immediate",
	)
	if !strings.Contains(out, `ensure_pvc "test-slack-bridge-state" "100Mi" "gardenio-gp3-immediate"`) {
		t.Errorf("adopt hook must ensure the slack-bridge PVC on the Immediate class, got:\n%s", out)
	}
}

// --- slackBridge credentials ExternalSecret wiring ---
//
// Regression guard for the "enabled component, unwired dependency chain"
// half-wire class (bug ka-0000004760, audit task ka-0000002430). slack-bridge
// mounts SLACK_BOT_TOKEN as a REQUIRED (non-optional) secretKeyRef, so enabling
// the bridge without materializing its K8s Secret leaves the pod in
// CreateContainerConfigError. The chart's slack ExternalSecret (56723f5d9)
// materializes that Secret from SM, but ONLY when
// externalSecrets.slackCredentials.remoteRef is set — the knob a values
// file can miss while it wires the gitlab/jira ones.

func TestSlackBridge_CredentialsExternalSecret_RendersWithRemoteRef(t *testing.T) {
	t.Parallel()
	// ESO gated on externalSecrets.enabled + slackBridge.enabled +
	// slackCredentials.remoteRef + slack.secretName.
	out := helmTemplate(t, "templates/external-secrets.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"externalSecrets.slackCredentials.remoteRef=gardenio/prod/slack-bot-credentials",
	)
	if !strings.Contains(out, "kind: ExternalSecret") {
		t.Fatalf("slack credentials ExternalSecret must render when remoteRef is set, got:\n%s", out)
	}
	for _, want := range []string{
		`key: "gardenio/prod/slack-bot-credentials"`,
		"name: slack-bot-credentials",
		"secretKey: bot_token",
		"secretKey: app_token",
		`property: "bot-token"`,
		`property: "app-token"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("slack credentials ExternalSecret missing %q, got:\n%s", want, out)
		}
	}
	// includeSigningSecret defaults false → the signing-secret key must be
	// omitted from the SLACK-BRIDGE ExternalSecret (Socket-Mode apps have no
	// signing secret; the deployment mounts it optional so the pod starts
	// regardless). Scope the check to the slack-bridge block — the shared
	// external-secrets.yaml also renders idp/forwardAuth secrets that carry
	// their own unrelated signing keys.
	slackES := out
	if i := strings.Index(slackES, "component: slack-bridge"); i >= 0 {
		slackES = slackES[i:]
		if j := strings.Index(slackES, "\n---\n"); j >= 0 {
			slackES = slackES[:j]
		}
	}
	if strings.Contains(slackES, "signing-secret") {
		t.Errorf("signing-secret key must be omitted from the slack-bridge ExternalSecret when includeSigningSecret is false, got:\n%s", slackES)
	}
}

func TestSlackBridge_CredentialsExternalSecret_NotRenderedWithoutRemoteRef(t *testing.T) {
	t.Parallel()
	// The half-wire this fix closes: bridge enabled + secretName set, but no
	// remoteRef → the ExternalSecret NEVER renders, so a fresh terraform apply
	// cannot materialize the Secret the required SLACK_BOT_TOKEN mount depends on.
	out, err := helmTemplateAllowFail(t, "templates/external-secrets.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
	)
	if err == nil && strings.Contains(out, "name: slack-bot-credentials") {
		t.Errorf("slack credentials ExternalSecret must NOT render without a remoteRef, got:\n%s", out)
	}
}

func TestSlackBridge_BotTokenIsRequiredSecretKeyRef(t *testing.T) {
	t.Parallel()
	// SLACK_BOT_TOKEN must be a hard-required secretKeyRef (NOT optional): that
	// is why the missing ExternalSecret was a live CreateContainerConfigError,
	// not a soft degradation. SLACK_APP_TOKEN / SLACK_SIGNING_SECRET stay
	// optional.
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-bot-credentials",
	)
	// Isolate the SLACK_BOT_TOKEN env block and assert it carries no
	// "optional: true" before the next env entry.
	idx := strings.Index(out, "name: SLACK_BOT_TOKEN")
	if idx < 0 {
		t.Fatalf("SLACK_BOT_TOKEN env must be present when slack.secretName is set, got:\n%s", out)
	}
	rest := out[idx:]
	if end := strings.Index(rest, "- name: SLACK_"); end > 0 {
		rest = rest[:end]
	}
	if !strings.Contains(rest, "key: bot-token") {
		t.Errorf("SLACK_BOT_TOKEN must reference the bot-token key, got:\n%s", rest)
	}
	if strings.Contains(rest, "optional: true") {
		t.Errorf("SLACK_BOT_TOKEN must be a required secretKeyRef (no optional: true), got:\n%s", rest)
	}
}
