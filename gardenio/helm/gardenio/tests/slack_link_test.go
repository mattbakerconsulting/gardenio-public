package tests

import (
	"os"
	"strings"
	"testing"
)

// Slack account linking (gd-OZJ6H2S3xx): chart plumbing for
// slackBridge.accountLink, beads.clientRoleCeiling and the authproxy route.

func TestSlackLink_DisabledRendersNothing(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
	)
	for _, env := range []string{"SLACK_LINK_ENABLED", "SLACK_LINK_PUBLIC_URL", "SLACK_LINK_KEY", "SLACK_LINK_CLIENT_SECRET"} {
		if strings.Contains(out, env) {
			t.Errorf("%s must not render while accountLink.enabled is false", env)
		}
	}
	beads := helmTemplate(t, "templates/beads/deployment.yaml", "beads.enabled=true")
	if strings.Contains(beads, "BEADS_CLIENT_ROLE_CEILING") {
		t.Error("BEADS_CLIENT_ROLE_CEILING must not render while beads.clientRoleCeiling is empty")
	}
}

func TestSlackLink_EnabledRendersEnvAndSecrets(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"slackBridge.accountLink.enabled=true",
		"slackBridge.accountLink.publicURL=https://gardenio.example.com",
		"slackBridge.accountLink.issuerURL=https://auth.example.com/auth/realms/gardenio",
	)
	for _, want := range []string{
		`- name: SLACK_LINK_ENABLED`,
		`value: "https://gardenio.example.com"`,
		`value: "https://auth.example.com/auth/realms/gardenio"`,
		`- name: SLACK_LINK_CLIENT_ID`,
		`value: "gardenio-slack-link"`,
		`- name: SLACK_LINK_CLIENT_SECRET`,
		`name: gardenio-slack-link`,
		`key: clientSecret`,
		`- name: SLACK_LINK_KEY`,
		`name: gardenio-slack-link-key`,
		`key: key`,
		`- name: SLACK_LINK_STORE_PATH`,
		`value: "/data/slack-links.json"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered slack-bridge deployment lacks %q", want)
		}
	}
	// The key and client secret are Secret refs, never inline values.
	if strings.Contains(out, "SLACK_LINK_KEY\n              value:") || strings.Contains(out, "SLACK_LINK_CLIENT_SECRET\n              value:") {
		t.Error("SLACK_LINK_KEY / SLACK_LINK_CLIENT_SECRET must come from secretKeyRef")
	}

	beads := helmTemplate(t, "templates/beads/deployment.yaml", "beads.enabled=true",
		"beads.clientRoleCeiling=gardenio-slack-link=gardenio-operator")
	if !strings.Contains(beads, "- name: BEADS_CLIENT_ROLE_CEILING") || !strings.Contains(beads, `value: "gardenio-slack-link=gardenio-operator"`) {
		t.Errorf("beads deployment lacks BEADS_CLIENT_ROLE_CEILING:\n%s", beads)
	}
}

func TestSlackLink_EnabledWithoutPublicURLFails(t *testing.T) {
	t.Parallel()
	if _, err := helmTemplateAllowFail(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
		"slackBridge.accountLink.enabled=true",
		"slackBridge.accountLink.issuerURL=https://auth.example.com/auth/realms/gardenio",
	); err == nil {
		t.Fatal("accountLink.enabled without publicURL must fail to render (the bridge would refuse to start anyway)")
	}
}

// The public edge's gardenio.clonk.tv block exposes exactly /slack/link/ of the
// bridge (slackBridge.accountLink.publicURL), with no admin-group auth_request
// (the state and the Keycloak sign-in are the authentication), and blanks any
// client bearer or cookie so nothing of the viewer session leaks to the bridge.
// It lived on viewer-authproxy's ziti front door until ziti was switched off
// (gd-xNMLrsgajy); the proxy must not carry it any more.
func TestPublicEdge_SlackLinkLocation(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../gardenio-ziti/public-edge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	_, viewer, ok := strings.Cut(doc, "server_name gardenio.clonk.tv;")
	if !ok {
		t.Fatal("public-edge.yaml has no gardenio.clonk.tv TLS server block")
	}
	viewer, _, _ = strings.Cut(viewer, "server_name mux.clonk.tv;")
	start := strings.Index(viewer, "location ^~ /slack/link/ {")
	if start < 0 {
		t.Fatal("the gardenio.clonk.tv block has no `location ^~ /slack/link/` block")
	}
	block := viewer[start:]
	block = block[:strings.Index(block, "\n      }")+8]
	for _, want := range []string{
		"set $up_slack gardenio-slack-bridge.gardenio.svc.cluster.local:8090;",
		"proxy_pass http://$up_slack;",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("/slack/link/ must proxy to the slack-bridge service on 8090 (%q):\n%s", want, block)
		}
	}
	if strings.Contains(block, "auth_request") {
		t.Errorf("/slack/link/ must not sit behind the admin-group auth_request:\n%s", block)
	}
	for _, h := range []string{"Authorization", "Cookie"} {
		if !strings.Contains(block, `proxy_set_header `+h+` "";`) {
			t.Errorf("/slack/link/ must blank the %s header", h)
		}
	}
	// It must precede location /, which would otherwise demand the sign-in.
	if root := strings.Index(viewer, "location / {"); root < 0 || root < start {
		t.Error("/slack/link/ location must come before location / in the gardenio.clonk.tv block")
	}

	ap, err := os.ReadFile("../../gardenio-ziti/viewer-authproxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ap), "/slack/link/") {
		t.Error("viewer-authproxy.yaml must not route /slack/link/ any more: the public edge serves it")
	}
}
