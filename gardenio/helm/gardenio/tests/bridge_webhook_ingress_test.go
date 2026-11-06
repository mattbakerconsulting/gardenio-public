package tests

import (
	"strings"
	"testing"
)

// The gitlab-bridge :8092 /webhook and jira-bridge :8091 /webhook endpoints
// were exposed ONLY via Traefik IngressRoute templates, but prod-gardenio-eks
// runs no Traefik — only the AWS Load Balancer Controller (ingressclass
// "alb"). Enabling either bridge there yielded a bridge with NO reachable
// /webhook (ka-0000002713). These tests pin the plain networking.k8s.io/v1
// Ingress alternative: values-gated, default OFF, coexisting with (not
// replacing) the Traefik IngressRoutes, with all controller-specific behavior
// (ALB scheme / target-type / certificate-arn / inbound-cidrs) supplied via
// the caller's annotations map.

// bridgeWebhookIngressCount counts plain networking/v1 Ingress manifests for
// the given bridge component in a full-chart render. Matching on both the
// exact kind line and the component label keeps IngressRoute / NetworkPolicy
// docs out of the count.
func bridgeWebhookIngressCount(rendered, component string) int {
	n := 0
	for doc := range strings.SplitSeq(rendered, "\n---") {
		if strings.Contains(doc, "\nkind: Ingress\n") &&
			strings.Contains(doc, "app.kubernetes.io/component: "+component) {
			n++
		}
	}
	return n
}

func TestBridgeWebhookIngress_DefaultOff(t *testing.T) {
	t.Parallel()
	// Both bridges ON with webhook features configured, webhookIngress left at
	// the default: no plain Ingress may render. Render the whole chart (not
	// --show-only) — helm errors when a --show-only target renders empty,
	// which would mask the assertion.
	out := helmTemplate(t, "",
		"gitlabBridge.enabled=true",
		"jiraBridge.enabled=true",
		"jiraBridge.webhook.enabled=true",
		"jiraBridge.webhook.host=jw.example.com",
	)
	if n := bridgeWebhookIngressCount(out, "gitlab-bridge"); n != 0 {
		t.Errorf("gitlab-bridge plain Ingress must NOT render by default, got %d", n)
	}
	if n := bridgeWebhookIngressCount(out, "jira-bridge"); n != 0 {
		t.Errorf("jira-bridge plain Ingress must NOT render by default, got %d", n)
	}
}

func TestBridgeWebhookIngress_GitlabRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/gitlab-bridge/webhook-ingress.yaml",
		map[string]string{
			"gitlabBridge.webhookIngress.annotations": `{"alb.ingress.kubernetes.io/scheme":"internet-facing","alb.ingress.kubernetes.io/target-type":"ip","alb.ingress.kubernetes.io/inbound-cidrs":"35.227.35.0/24"}`,
		},
		"gitlabBridge.enabled=true",
		"gitlabBridge.webhookIngress.enabled=true",
		"gitlabBridge.webhookIngress.className=alb",
		"gitlabBridge.webhookIngress.host=gitlab-webhook.prod.gardenio.acme.example",
	)
	for _, want := range []string{
		"apiVersion: networking.k8s.io/v1",
		"kind: Ingress",
		`ingressClassName: "alb"`,
		`host: "gitlab-webhook.prod.gardenio.acme.example"`,
		"path: /webhook",
		"pathType: Exact",
		"name: test-gitlab-bridge",
		// Backend targets the Service's named http port so a
		// gitlabBridge.service.port override cannot desync the Ingress.
		"name: http",
		// Caller-supplied annotations pass through verbatim — the chart
		// hardcodes no ALB specifics.
		"alb.ingress.kubernetes.io/scheme: internet-facing",
		"alb.ingress.kubernetes.io/target-type: ip",
		"alb.ingress.kubernetes.io/inbound-cidrs: 35.227.35.0/24",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gitlab-bridge webhook Ingress missing %q, got:\n%s", want, out)
		}
	}
}

func TestBridgeWebhookIngress_JiraRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/webhook-ingress.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.webhook.enabled=true",
		"jiraBridge.webhookIngress.enabled=true",
		"jiraBridge.webhookIngress.className=alb",
		"jiraBridge.webhookIngress.host=jira-webhook.prod.gardenio.acme.example",
	)
	for _, want := range []string{
		"apiVersion: networking.k8s.io/v1",
		"kind: Ingress",
		`ingressClassName: "alb"`,
		`host: "jira-webhook.prod.gardenio.acme.example"`,
		"path: /webhook",
		"pathType: Exact",
		"name: test-jira-bridge",
		"name: http",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("jira-bridge webhook Ingress missing %q, got:\n%s", want, out)
		}
	}
}

func TestBridgeWebhookIngress_JiraGatedOnWebhookEnabled(t *testing.T) {
	t.Parallel()
	// jiraBridge.webhook.enabled is the webhook-feature switch (it also gates
	// the webhook-secret ExternalSecret). webhookIngress ON with the feature
	// OFF must render nothing rather than expose a dead endpoint.
	out := helmTemplate(t, "",
		"jiraBridge.enabled=true",
		"jiraBridge.webhookIngress.enabled=true",
		"jiraBridge.webhookIngress.host=jira-webhook.prod.gardenio.acme.example",
	)
	if n := bridgeWebhookIngressCount(out, "jira-bridge"); n != 0 {
		t.Errorf("jira-bridge plain Ingress must NOT render when jiraBridge.webhook.enabled=false, got %d", n)
	}
}

func TestBridgeWebhookIngress_EmptyHostFails(t *testing.T) {
	t.Parallel()
	_, err := helmTemplateAllowFail(t, "",
		"gitlabBridge.enabled=true",
		"gitlabBridge.webhookIngress.enabled=true",
	)
	if err == nil {
		t.Error("gitlab-bridge webhookIngress with empty host must fail template rendering")
	}
	_, err = helmTemplateAllowFail(t, "",
		"jiraBridge.enabled=true",
		"jiraBridge.webhook.enabled=true",
		"jiraBridge.webhook.host=jw.example.com",
		"jiraBridge.webhookIngress.enabled=true",
	)
	if err == nil {
		t.Error("jira-bridge webhookIngress with empty host must fail template rendering")
	}
}

func TestBridgeWebhookIngress_CoexistsWithTraefikRoute(t *testing.T) {
	t.Parallel()
	// A Traefik env that (hypothetically) also flips webhookIngress on must get
	// BOTH the IngressRoute and the plain Ingress — the new template is additive
	// and must not be gated on gardenio.traefik.ingressRoutesEnabled.
	out := helmTemplate(t, "",
		"traefik.enabled=true",
		"gitlabBridge.enabled=true",
		"gitlabBridge.ingress.enabled=true",
		"gitlabBridge.ingress.host=gw.example.com",
		"gitlabBridge.webhookIngress.enabled=true",
		"gitlabBridge.webhookIngress.host=gw-alb.example.com",
	)
	if n := bridgeWebhookIngressCount(out, "gitlab-bridge"); n != 1 {
		t.Errorf("gitlab-bridge plain Ingress must render alongside Traefik routes, got %d", n)
	}
	if !strings.Contains(out, "kind: IngressRoute") {
		t.Errorf("Traefik IngressRoute must still render when webhookIngress is also enabled, got:\n%s", out)
	}
}

func TestBridgeWebhookIngress_TLSBlockRenders(t *testing.T) {
	t.Parallel()
	// Optional spec.tls for nginx-style controllers (ALB ignores it and uses
	// the certificate-arn annotation instead).
	out := helmTemplateJSON(t, "templates/gitlab-bridge/webhook-ingress.yaml",
		map[string]string{
			"gitlabBridge.webhookIngress.tls": `[{"hosts":["gw.example.com"],"secretName":"gw-tls"}]`,
		},
		"gitlabBridge.enabled=true",
		"gitlabBridge.webhookIngress.enabled=true",
		"gitlabBridge.webhookIngress.host=gw.example.com",
	)
	if !strings.Contains(out, "secretName: gw-tls") {
		t.Errorf("webhookIngress.tls must render into spec.tls, got:\n%s", out)
	}
}
