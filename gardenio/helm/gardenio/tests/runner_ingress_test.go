package tests

// Public runner listener (local-agents spec §4.3, plan gd-fwFefkDP4H Task 10
// gd-lNbLtKuWoq): templates/beads/runner-ingress.yaml and the slack-bridge's
// GARDENIO_RUNNER_PUBLIC_URL.

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const runnerTpl = "templates/beads/runner-ingress.yaml"

var runnerOn = []string{
	"beads.ingress.runnerRoute.enabled=true",
	"beads.ingress.runnerRoute.host=runners.example.com",
}

func TestRunnerRoute_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "", "slackBridge.enabled=true")
	for _, absent := range []string{"-runner-register", "X-Gardenio-Listener", "GARDENIO_RUNNER_PUBLIC_URL"} {
		if strings.Contains(out, absent) {
			t.Errorf("runner listener must not render by default; found %q", absent)
		}
	}
}

func TestRunnerRoute_EnabledWithoutHostFails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, runnerTpl, "beads.ingress.runnerRoute.enabled=true")
	if err == nil {
		t.Fatal("enabling the runner route without a host must fail at template time")
	}
	if !strings.Contains(out, "beads.ingress.runnerRoute.host is empty") {
		t.Errorf("want the host-required message, got:\n%s", out)
	}
}

func TestRunnerRoute_NotInFleetMode(t *testing.T) {
	t.Parallel()
	// A fleet release runs no beads daemon to route to.
	out, err := helmTemplateGardenio(t, runnerTpl, append(runnerOn, "fleet.enabled=true")...)
	if err == nil && strings.Contains(out, "kind: IngressRoute") {
		t.Errorf("runner listener must not render with fleet.enabled:\n%s", out)
	}
}

func TestRunnerRoute_RendersAllowlistOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, runnerTpl, runnerOn...)
	for _, want := range []string{
		"name: test-beads-runner\n",
		"name: test-beads-runner-register\n",
		"Host(`runners.example.com`) && HeaderRegexp(`Authorization`, `^Bearer gr_`) && (",
		"(Method(`POST`) && PathRegexp(`^/v1/runners/[^/]+/heartbeat$`))",
		"(Method(`GET`) && Path(`/v1/events/stream`))",
		"(Method(`PATCH`) && PathRegexp(`^/v1/beads/[^/]+$`))",
		"(Method(`POST`) && PathRegexp(`^/v1/escalations/[^/]+/result$`))",
		"(Method(`GET`) && PathRegexp(`^/v1/configs/.+$`))",
		"(Method(`GET`) && PathRegexp(`^/v1/agents-by-name/[^/]+$`))",
		"Host(`runners.example.com`) && Method(`POST`) && Path(`/v1/runners/register`)",
		"X-Gardenio-Listener: runner",
		"- name: test-beads-runner-headers",
		"- name: test-beads-runner-ratelimit",
		"- name: test-beads-runner-register-ratelimit",
		"average: 20\n    burst: 40",
		"average: 10\n    period: 1h\n    burst: 10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in rendered runner listener:\n%s", want, out)
		}
	}
	// No prefix match (wider than the daemon's allowlist), no other auth or
	// IP gate (the bearer is the gate), and never the operator routes.
	for _, forbidden := range []string{"PathPrefix(", "forwardauth", "ipWhiteList", "ipAllowList", "/revoke", "/v1/runners/invite", "stripPrefix"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("runner listener must not render %q:\n%s", forbidden, out)
		}
	}
	if n := strings.Count(out, "kind: IngressRoute"); n != 2 {
		t.Errorf("expected exactly two IngressRoutes (authenticated + register), got %d", n)
	}
	if n := strings.Count(out, "kind: Middleware"); n != 3 {
		t.Errorf("expected exactly three Middlewares (headers + two rate limits), got %d", n)
	}
}

// TestRunnerRoute_MatchesServerAllowlist pins the route to kbeads'
// runnerAllowedPatterns, the source of truth: every pattern there has exactly
// one method-pinned clause here, and nothing else does.
func TestRunnerRoute_MatchesServerAllowlist(t *testing.T) {
	t.Parallel()
	want := serverRunnerPatterns(t)
	out := helmTemplate(t, runnerTpl, runnerOn...)
	var match string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "HeaderRegexp(`Authorization`, `^Bearer gr_`)") {
			match = line
		}
	}
	if match == "" {
		t.Fatalf("no runner-token match rule rendered:\n%s", out)
	}
	clause := regexp.MustCompile("\\(Method\\(`([A-Z]+)`\\) && (Path|PathRegexp)\\(`([^`]+)`\\)\\)")
	var got []string
	for _, m := range clause.FindAllStringSubmatch(match, -1) {
		got = append(got, m[1]+" "+m[2]+" "+m[3])
	}
	if rest := clause.ReplaceAllString(match, ""); strings.Contains(rest, "Path") || strings.Contains(rest, "Method") {
		t.Errorf("rule has a path or method matcher outside the allowlist clauses: %s", rest)
	}
	var exp []string
	for _, p := range want {
		method, path, _ := strings.Cut(p, " ")
		if strings.Contains(path, "{") {
			re := regexp.MustCompile(`\{[^}]+\.\.\.\}`).ReplaceAllString(path, ".+")
			exp = append(exp, method+" PathRegexp ^"+runnerWildcard.ReplaceAllString(re, "[^/]+")+"$")
		} else {
			exp = append(exp, method+" Path "+path)
		}
	}
	slices.Sort(got)
	slices.Sort(exp)
	if !slices.Equal(got, exp) {
		t.Errorf("runner route clauses drifted from kbeads runnerAllowedPatterns\n got: %q\nwant: %q", got, exp)
	}
}

// serverRunnerPatterns reads runnerAllowedPatterns out of the daemon source.
func serverRunnerPatterns(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../../kbeads/internal/server/runner_auth.go")
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(raw), "var runnerAllowedPatterns = map[string]bool{")
	if !ok {
		t.Fatal("runner_auth.go: runnerAllowedPatterns map not found")
	}
	body, _, _ = strings.Cut(body, "\n}")
	var patterns []string
	for _, m := range regexp.MustCompile(`"([A-Z]+ /[^"]+)":\s*true`).FindAllStringSubmatch(body, -1) {
		patterns = append(patterns, m[1])
	}
	if len(patterns) == 0 {
		t.Fatal("runner_auth.go: runnerAllowedPatterns is empty or unparsed")
	}
	return patterns
}

func TestRunnerRoute_UsesChartTLSSecret(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, runnerTpl, append(runnerOn, "tls.enabled=true")...)
	if n := strings.Count(out, "secretName: test-wildcard-tls"); n != 2 {
		t.Errorf("both runner IngressRoutes must terminate TLS with the chart secret, got %d:\n%s", n, out)
	}
}

func TestRunnerRoute_EntryPointsOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, runnerTpl, append(runnerOn, "beads.ingress.runnerRoute.entryPoints={webhooks}")...)
	if n := strings.Count(out, "entryPoints:\n    - webhooks\n  routes:"); n != 2 {
		t.Errorf("both runner IngressRoutes must bind the configured entrypoints, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "- websecure") {
		t.Errorf("the default entrypoint must be replaced, not added to:\n%s", out)
	}
}

func TestRunnerRoute_SlackBridgePublicURL(t *testing.T) {
	t.Parallel()
	const tpl = "templates/slack-bridge/deployment.yaml"
	on := helmTemplate(t, tpl, append(runnerOn, "slackBridge.enabled=true")...)
	if !strings.Contains(on, "- name: GARDENIO_RUNNER_PUBLIC_URL\n              value: \"https://runners.example.com\"") {
		t.Errorf("slack-bridge must get GARDENIO_RUNNER_PUBLIC_URL=https://<runnerRoute.host>:\n%s", on)
	}
	off := helmTemplate(t, tpl, "slackBridge.enabled=true", "beads.ingress.runnerRoute.host=runners.example.com")
	if strings.Contains(off, "GARDENIO_RUNNER_PUBLIC_URL") {
		t.Error("GARDENIO_RUNNER_PUBLIC_URL must not be set while runnerRoute.enabled is false")
	}
}

// Where another edge serves the listener (k3s: the nginx public edge's
// runners.clonk.tv, gd-T4eL6P5OtC), runnerRoute.publicURL sets the bridge's
// URL without rendering the Traefik route, and wins over https://<host>.
func TestRunnerRoute_SlackBridgePublicURLOverride(t *testing.T) {
	t.Parallel()
	const tpl = "templates/slack-bridge/deployment.yaml"
	const want = "- name: GARDENIO_RUNNER_PUBLIC_URL\n              value: \"https://runners.clonk.tv\""
	edge := helmTemplate(t, tpl, "slackBridge.enabled=true", "beads.ingress.runnerRoute.publicURL=https://runners.clonk.tv")
	if !strings.Contains(edge, want) {
		t.Errorf("slack-bridge must get GARDENIO_RUNNER_PUBLIC_URL from runnerRoute.publicURL:\n%s", edge)
	}
	both := helmTemplate(t, tpl, append(runnerOn, "slackBridge.enabled=true", "beads.ingress.runnerRoute.publicURL=https://runners.clonk.tv")...)
	if !strings.Contains(both, want) || strings.Count(both, "GARDENIO_RUNNER_PUBLIC_URL") != 1 {
		t.Errorf("runnerRoute.publicURL must win over https://<host>, set once:\n%s", both)
	}
	if out, err := helmTemplateGardenio(t, runnerTpl, "beads.ingress.runnerRoute.publicURL=https://runners.clonk.tv"); err == nil && strings.Contains(out, "kind: IngressRoute") {
		t.Errorf("runnerRoute.publicURL alone must not render the Traefik route:\n%s", out)
	}
}
