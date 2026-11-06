package tests

import (
	"strings"
	"testing"
)

// gd-mZUh2mvdsZ: the unified-host jira-bridge legacy redirect gated/derived its
// host off .Values.jiraBridge.ingress.{enabled,host} — but jiraBridge has NO
// `ingress` key; the real keys are jiraBridge.webhook.{enabled,host} (the
// jira-bridge's own IngressRoute keys the legacy host off webhook.host). Go
// text/template `and` evaluates ALL its args (no short-circuit), so once
// jiraBridge.enabled + a jira path made $jiraEnabled true, evaluating the nil
// `ingress` map aborted the ENTIRE chart render:
//   nil pointer evaluating interface {}.enabled
// These tests lock in the fix: (1) jiraBridge enabled with webhook off must
// render the whole chart without the nil-pointer abort and emit NO jira
// unified-redirect; (2) webhook.enabled+host set emits the redirect keyed off
// the webhook host.

// TestUnifiedHost_JiraRedirect_RendersWithoutNilPointer is the core regression:
// pre-fix this render aborted at ingressroute.yaml with a nil-pointer on the
// missing jiraBridge.ingress map, taking the whole release down with it.
func TestUnifiedHost_JiraRedirect_RendersWithoutNilPointer(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"jiraBridge.enabled=true",
			"jiraBridge.path=/jira",
		)...,
	)
	if err != nil {
		t.Fatalf("chart must render with jiraBridge enabled + webhook off, got error:\n%s", out)
	}
	// Webhook is off (default), so no jira unified redirect should render.
	if strings.Contains(out, "jira-bridge-unified-redirect") {
		t.Error("jira unified redirect must NOT render when jiraBridge.webhook is off")
	}
}

// TestUnifiedHost_JiraRedirect_EmittedFromWebhookHost proves the redirect
// renders and is keyed off the webhook host (not a nonexistent ingress host).
func TestUnifiedHost_JiraRedirect_EmittedFromWebhookHost(t *testing.T) {
	t.Parallel()
	const webhookHost = "jira-webhook.example.com"
	out, err := helmTemplateAllowFail(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"jiraBridge.enabled=true",
			"jiraBridge.path=/jira",
			"jiraBridge.webhook.enabled=true",
			"jiraBridge.webhook.host="+webhookHost,
		)...,
	)
	if err != nil {
		t.Fatalf("chart must render with jiraBridge webhook enabled, got error:\n%s", out)
	}
	if !strings.Contains(out, "jira-bridge-unified-redirect") {
		t.Fatal("jira unified redirect must render when jiraBridge.webhook.enabled + host are set")
	}
	if !strings.Contains(out, webhookHost) {
		t.Errorf("jira unified redirect must be keyed off the webhook host %q", webhookHost)
	}
}
