package tests

import (
	"strings"
	"testing"
)

// Grafana agent path (gd-RUhOpXWwmz). With the header auth-proxy on, any pod
// that reaches Grafana's trusted port inside the whitelist can claim a user by
// sending the identity header. grafana.auth.proxy.agentPath moves agent pods to
// a second port whose nginx sidecar blanks every identity header, fences the
// trusted port to the edge and platform callers, and stops handing agents the
// header name. Off, the chart must render exactly as before.

// agentPathOff is today's live shape: proxy on, edge fence on, agents on 3000.
func agentPathOff() []string {
	return []string{
		"agents.enabled=true",
		"grafana.enabled=true",
		"grafana.auth.proxy.enabled=true",
		"edgeBackends.enabled=true",
		"beadsViewer.enabled=true",
		"networkPolicies.enabled=true",
	}
}

// agentPathOn adds the agent path with grafanaCallers overridden to drop the
// agent selector (the chart refuses the default list).
func agentPathOn(extra ...string) []string {
	return append(append(agentPathOff(),
		"grafana.auth.proxy.agentPath.enabled=true",
		"edgeBackends.grafanaCallers[0].matchLabels.role=platform",
	), extra...)
}

// splitDocs keys every rendered object by "Kind/name".
func splitDocs(out string) map[string]string {
	docs := map[string]string{}
	for doc := range strings.SplitSeq(out, "\n---") {
		kind, name := "", ""
		for line := range strings.SplitSeq(doc, "\n") {
			if kind == "" && strings.HasPrefix(line, "kind: ") {
				kind = strings.TrimPrefix(line, "kind: ")
			}
			if name == "" && strings.HasPrefix(line, "  name: ") {
				name = strings.TrimPrefix(line, "  name: ")
			}
		}
		if kind != "" && name != "" {
			docs[kind+"/"+name] = doc
		}
	}
	return docs
}

// ingressRules splits a NetworkPolicy's ingress list into its rules.
func ingressRules(doc string) []string {
	_, rest, ok := strings.Cut(doc, "\n  ingress:\n")
	if !ok {
		return nil
	}
	var rules []string
	for r := range strings.SplitSeq(rest, "\n    - from:") {
		if strings.TrimSpace(r) != "" {
			rules = append(rules, r)
		}
	}
	return rules
}

func TestGrafanaAgentPath_OffRendersTodaysShape(t *testing.T) {
	t.Parallel()
	docs := splitDocs(helmTemplate(t, "", agentPathOff()...))

	dep := docs["Deployment/test-grafana"]
	for _, bad := range []string{"grafana-agent-proxy", "http-agent", "checksum/agent-proxy", "GF_AUTH_ANONYMOUS_ORG_ROLE"} {
		if strings.Contains(dep, bad) {
			t.Errorf("agentPath off: grafana Deployment must not contain %q", bad)
		}
	}
	if !strings.Contains(dep, "name: GF_AUTH_ANONYMOUS_ENABLED\n              value: \"false\"") {
		t.Error("agentPath off: anonymous access must stay disabled under auth-proxy")
	}
	if _, ok := docs["ConfigMap/test-grafana-agent-proxy"]; ok {
		t.Error("agentPath off: no agent-proxy ConfigMap")
	}
	if strings.Contains(docs["Service/test-grafana"], "http-agent") {
		t.Error("agentPath off: Service must keep a single port")
	}
	np := docs["NetworkPolicy/test-edge-grafana"]
	if rules := ingressRules(np); len(rules) != 1 || !strings.Contains(rules[0], "gardenio.io/agent") || !strings.Contains(rules[0], "port: 3000") {
		t.Errorf("agentPath off: edge-grafana must keep its one rule admitting agents on 3000:\n%s", np)
	}

	ctl := docs["Deployment/test-agents"]
	if !strings.Contains(ctl, `value: "http://test-grafana:3000"`) {
		t.Errorf("agentPath off: agents keep GRAFANA_URL on 3000:\n%s", ctl)
	}
	if !strings.Contains(ctl, "GRAFANA_AUTH_PROXY_HEADER") {
		t.Error("agentPath off: agents keep GRAFANA_AUTH_PROXY_HEADER")
	}
}

func TestGrafanaAgentPath_OnAddsStrippingSidecar(t *testing.T) {
	t.Parallel()
	docs := splitDocs(helmTemplate(t, "", agentPathOn("grafana.auth.proxy.headerName=X-Custom-Identity")...))

	dep := docs["Deployment/test-grafana"]
	for _, want := range []string{
		"- name: grafana-agent-proxy",
		`image: "nginxinc/nginx-unprivileged:1.27-alpine"`,
		"containerPort: 3001",
		"port: http-agent",
		"checksum/agent-proxy: ",
		"name: test-grafana-agent-proxy",
		"name: GF_AUTH_ANONYMOUS_ENABLED\n              value: \"true\"",
		"name: GF_AUTH_ANONYMOUS_ORG_ROLE\n              value: \"Viewer\"",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("agentPath on: grafana Deployment missing %q", want)
		}
	}
	if strings.Contains(dep, "name: GF_AUTH_ANONYMOUS_ENABLED\n              value: \"false\"") {
		t.Error("agentPath on: GF_AUTH_ANONYMOUS_ENABLED rendered twice / still false")
	}

	conf := docs["ConfigMap/test-grafana-agent-proxy"]
	if !strings.Contains(conf, "listen 3001;") || !strings.Contains(conf, "proxy_pass http://127.0.0.1:3000;") {
		t.Errorf("agent proxy must listen on the agent port and forward to loopback Grafana:\n%s", conf)
	}
	for _, h := range []string{"x-custom-identity", "x-auth-request-email", "x-auth-request-user", "x-auth-request-groups", "x-webauth-user", "x-forwarded-email", "x-forwarded-user"} {
		if !strings.Contains(conf, "proxy_set_header "+h+` "";`) {
			t.Errorf("agent proxy must blank %s:\n%s", h, conf)
		}
	}
	if strings.Contains(strings.ToLower(conf), "authorization") {
		t.Error("agent proxy must pass Authorization through (service-account token path)")
	}

	svc := docs["Service/test-grafana"]
	if !strings.Contains(svc, "port: 3001\n      targetPort: http-agent") || !strings.Contains(svc, "name: http-agent") {
		t.Errorf("agentPath on: Service must expose http-agent on 3001:\n%s", svc)
	}
}

func TestGrafanaAgentPath_OnSplitsTheFence(t *testing.T) {
	t.Parallel()
	docs := splitDocs(helmTemplate(t, "", agentPathOn()...))

	rules := ingressRules(docs["NetworkPolicy/test-edge-grafana"])
	if len(rules) != 2 {
		t.Fatalf("edge-grafana must have a trusted-port rule and an agent-port rule, got %d:\n%s", len(rules), docs["NetworkPolicy/test-edge-grafana"])
	}
	trusted, agent := rules[0], rules[1]
	if !strings.Contains(trusted, "port: 3000") || strings.Contains(trusted, "gardenio.io/agent") {
		t.Errorf("trusted port must not admit agent pods:\n%s", trusted)
	}
	if !strings.Contains(trusted, `kubernetes.io/metadata.name: "public-edge"`) || !strings.Contains(trusted, "role: platform") {
		t.Errorf("trusted port keeps the edge namespace and grafanaCallers:\n%s", trusted)
	}
	if !strings.Contains(agent, "port: 3001") || !strings.Contains(agent, "gardenio.io/agent") || strings.Contains(agent, "namespaceSelector") {
		t.Errorf("agent port admits exactly grafanaAgentCallers:\n%s", agent)
	}

	// networkPolicies.enabled pair: agent egress and grafana-ingress move too,
	// or the general grafana-ingress would re-admit agents on 3000.
	for _, n := range []string{"NetworkPolicy/test-agent-egress", "NetworkPolicy/test-grafana-ingress"} {
		doc := docs[n]
		if !strings.Contains(doc, "port: 3001") {
			t.Errorf("%s must use the agent port:\n%s", n, doc)
		}
	}
	if gi := docs["NetworkPolicy/test-grafana-ingress"]; strings.Contains(gi, "port: 3000") {
		t.Errorf("grafana-ingress must not admit agents on 3000:\n%s", gi)
	}

	ctl := docs["Deployment/test-agents"]
	if !strings.Contains(ctl, `value: "http://test-grafana:3001"`) {
		t.Errorf("agents must get GRAFANA_URL on the agent port:\n%s", ctl)
	}
	if strings.Contains(ctl, "GRAFANA_AUTH_PROXY_HEADER") {
		t.Error("agents must not get GRAFANA_AUTH_PROXY_HEADER with the agent path on")
	}
}

func TestGrafanaAgentPath_SubpathCarriesThrough(t *testing.T) {
	t.Parallel()
	sub := []string{"grafana.serveFromSubPath=true", "grafana.rootURL=https://grafana.example.test/grafana"}
	ctl := helmTemplate(t, "templates/controller/deployment.yaml", agentPathOn(sub...)...)
	if !strings.Contains(ctl, `value: "http://test-grafana:3001/grafana"`) {
		t.Errorf("agent GRAFANA_URL must keep the subpath on the agent port:\n%s", ctl)
	}
	dep := helmTemplate(t, "templates/grafana/deployment.yaml", agentPathOn(sub...)...)
	if !strings.Contains(dep, "path: /grafana/api/health\n              port: http-agent") {
		t.Error("sidecar readiness probe must follow the subpath")
	}
}

func TestGrafanaAgentPath_RefusesUnsafeConfigs(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		flags []string
		want  string
	}{
		"without auth-proxy": {
			flags: []string{"grafana.enabled=true", "grafana.auth.proxy.agentPath.enabled=true", "edgeBackends.enabled=true", "edgeBackends.grafanaCallers=null"},
			want:  "requires grafana.auth.proxy.enabled",
		},
		"without the edge fence": {
			flags: []string{"grafana.enabled=true", "grafana.auth.proxy.enabled=true", "grafana.auth.proxy.agentPath.enabled=true"},
			want:  "requires edgeBackends.enabled",
		},
		"agents still in grafanaCallers": {
			flags: []string{"grafana.enabled=true", "grafana.auth.proxy.enabled=true", "grafana.auth.proxy.agentPath.enabled=true", "edgeBackends.enabled=true"},
			want:  "still admits agent pods",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplateAllowFail(t, "templates/grafana/deployment.yaml", c.flags...)
			if err == nil || !strings.Contains(out, c.want) {
				t.Errorf("want render failure %q, got err=%v:\n%s", c.want, err, out)
			}
		})
	}
}
