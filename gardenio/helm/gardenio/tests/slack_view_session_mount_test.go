package tests

import (
	"regexp"
	"strings"
	"testing"
)

// Slack viewer session mount (gd-QpZeW4qxAU): the chart tells the controller
// which Secret holds the Slack test user's storageState and which roles get it
// mounted (as an OPTIONAL volume — see podmanager.VolumeSlackViewSession).

func slackViewEnvValue(t *testing.T, out, name string) (string, bool) {
	t.Helper()
	re := regexp.MustCompile(`(?s)- name: ` + regexp.QuoteMeta(name) + `\s+value: "?([^"\n]*)"?\n`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func TestSlackViewSession_DefaultsToESOSecretAndThreadOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl, "agents.enabled=true")
	if got, ok := slackViewEnvValue(t, out, "SLACK_VIEW_SESSION_SECRET"); !ok || got != "slack-e2e-session" {
		t.Errorf("SLACK_VIEW_SESSION_SECRET = %q (present=%v), want slack-e2e-session", got, ok)
	}
	if got, ok := slackViewEnvValue(t, out, "SLACK_VIEW_SESSION_ROLES"); !ok || got != "thread" {
		t.Errorf("SLACK_VIEW_SESSION_ROLES = %q (present=%v), want thread", got, ok)
	}
}

// The mount follows the ExternalSecret's target name, so pointing the ESO block
// at a different Secret needs no second knob.
func TestSlackViewSession_FollowsExternalSecretName(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"externalSecrets.slackE2ESession.secretName=custom-slack-session",
	)
	if got, _ := slackViewEnvValue(t, out, "SLACK_VIEW_SESSION_SECRET"); got != "custom-slack-session" {
		t.Errorf("SLACK_VIEW_SESSION_SECRET = %q, want custom-slack-session", got)
	}
}

// A hand-created Secret overrides the ESO name, and the role list is a value.
func TestSlackViewSession_SecretNameAndRolesOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"agents.slackViewSession.secretName=hand-made-slack",
		"agents.slackViewSession.roles={thread,crew,ux}",
	)
	if got, _ := slackViewEnvValue(t, out, "SLACK_VIEW_SESSION_SECRET"); got != "hand-made-slack" {
		t.Errorf("SLACK_VIEW_SESSION_SECRET = %q, want hand-made-slack", got)
	}
	if got, _ := slackViewEnvValue(t, out, "SLACK_VIEW_SESSION_ROLES"); got != "thread,crew,ux" {
		t.Errorf("SLACK_VIEW_SESSION_ROLES = %q, want thread,crew,ux", got)
	}
}

func TestSlackViewSession_CanBeDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"agents.slackViewSession.enabled=false",
	)
	for _, unwanted := range []string{"SLACK_VIEW_SESSION_SECRET", "SLACK_VIEW_SESSION_ROLES"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("%s must not render when agents.slackViewSession.enabled=false", unwanted)
		}
	}
}

// secretName "-" is the other off switch: rendering "-" as a Secret name would
// make every matching pod fail to create.
func TestSlackViewSession_DashSecretNameDisables(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"agents.slackViewSession.secretName=-",
	)
	for _, unwanted := range []string{"SLACK_VIEW_SESSION_SECRET", "SLACK_VIEW_SESSION_ROLES"} {
		if strings.Contains(out, unwanted) {
			t.Errorf(`%s must not render when agents.slackViewSession.secretName="-"`, unwanted)
		}
	}
}

// The ExternalSecret and the mount must agree on the Secret key the controller
// projects (podmanager.SlackViewSessionFileName).
func TestSlackViewSession_ExternalSecretKeyMatchesMount(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, slackE2EExtSecret,
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secretsmanager",
		"externalSecrets.slackE2ESession.remoteRef=gardenio/prod/slack-e2e/test-user-session",
	)
	if !strings.Contains(out, "storage-state.json:") {
		t.Errorf("ExternalSecret must materialize key storage-state.json; got:\n%s", out)
	}
}
