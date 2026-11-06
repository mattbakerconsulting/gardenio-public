package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Live-corpus advice-budget alert (ka-0000007517, Option 3 of ka-0000006923).
// The chart renders a PrometheusRule with two tiered alerts on the
// gardenio_advice_catalog_bytes gauge (emitted per project/role/scope by the
// metrics collector via advice.AuditCatalog): ArchetypeOverCeiling fires past
// the 20k hard ceiling, ArchetypeApproachingBudget fires in the 18k-20k soft
// band. This is the real-time complement to the frozen-fixture CI budget test
// (TestRealCatalogPerArchetypeBudget). OFF by default; per-env overlays opt in.

func TestAdviceBudgetAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/advice-budget-alert.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when adviceBudget.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestAdviceBudgetAlert_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/advice-budget-alert.yaml",
		"agents.metrics.prometheusRule.adviceBudget.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	for _, want := range []string{
		"alert: AdviceCatalogArchetypeOverCeiling",
		"alert: AdviceCatalogArchetypeApproachingBudget",
		// R4 tripwire (ka-0000002748).
		"alert: AdvicePrimeRenderApproachingBudget",
		"alert: AdviceOpenPoolApproachingCap",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("alert %q missing, got:\n%s", want, out)
		}
	}
	// The R4 render tripwire must watch the abbreviated-render demand gauge at
	// the 48000-byte (80% of 60000) default, and the pool tripwire the open-pool
	// gauge at 450. These are the exact thresholds the acceptance names.
	if !strings.Contains(out, "max by (project, role) (gardenio_advice_prime_render_bytes) > 48000") {
		t.Errorf("render tripwire must fire on the render gauge at 48000, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_advice_open_total > 450") {
		t.Errorf("open-pool tripwire must fire on the open-pool gauge at 450, got:\n%s", out)
	}
	if !strings.Contains(out, "past 80% of the abbreviated byte budget") {
		t.Errorf("render tripwire summary must state the computed 80%% of budget, got:\n%s", out)
	}
	// Both alerts must sum the byte gauge per archetype and convert to ~tokens
	// via the same TokenBytesRatio (=4) the CLI / CI-test / dashboard use.
	if !strings.Contains(out, "sum by (project, role) (gardenio_advice_catalog_bytes) / 4") {
		t.Errorf("expr must sum the byte gauge per archetype and divide by TokenBytesRatio 4, got:\n%s", out)
	}
	// Defaults: 20k hard ceiling, 18k soft budget, ticket severity.
	if !strings.Contains(out, "/ 4 > 20000") {
		t.Errorf("default hard ceiling 20000 not in expr, got:\n%s", out)
	}
	if !strings.Contains(out, "/ 4 > 18000") {
		t.Errorf("default soft budget 18000 not in expr, got:\n%s", out)
	}
	// The soft-band alert must be capped at the hard ceiling so it is mutually
	// exclusive with the ceiling alert (an over-ceiling archetype pages once).
	if !strings.Contains(out, "/ 4 <= 20000") {
		t.Errorf("soft-band alert must be capped at the hard ceiling (<= 20000) for mutual exclusion, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "ticket"`) {
		t.Errorf("default severity must be ticket, got:\n%s", out)
	}
	if !strings.Contains(out, "subsystem: advice") {
		t.Errorf("alerts must carry subsystem: advice, got:\n%s", out)
	}
}

func TestAdviceBudgetAlert_ThresholdsConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/advice-budget-alert.yaml",
		"agents.metrics.prometheusRule.adviceBudget.enabled=true",
		"agents.metrics.prometheusRule.adviceBudget.hardCeilingTokens=25000",
		"agents.metrics.prometheusRule.adviceBudget.softBudgetTokens=22000",
		"agents.metrics.prometheusRule.adviceBudget.severity=page",
	)
	if !strings.Contains(out, "/ 4 > 25000") {
		t.Errorf("hardCeilingTokens override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, "/ 4 > 22000") {
		t.Errorf("softBudgetTokens override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, "/ 4 <= 25000") {
		t.Errorf("soft-band cap must track the hard-ceiling override, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "page"`) {
		t.Errorf("severity override not applied, got:\n%s", out)
	}
}

func TestAdviceBudgetAlert_R4ThresholdsConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/advice-budget-alert.yaml",
		"agents.metrics.prometheusRule.adviceBudget.enabled=true",
		"agents.metrics.prometheusRule.adviceBudget.renderWarnBytes=50000",
		"agents.metrics.prometheusRule.adviceBudget.renderBudgetBytes=62500",
		"agents.metrics.prometheusRule.adviceBudget.openPoolWarn=470",
	)
	if !strings.Contains(out, "gardenio_advice_prime_render_bytes) > 50000") {
		t.Errorf("renderWarnBytes override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_advice_open_total > 470") {
		t.Errorf("openPoolWarn override not applied, got:\n%s", out)
	}
	// The annotated percentage must recompute from the overridden warn/budget
	// pair: 50000 / 62500 = 80%.
	if !strings.Contains(out, "past 80% of the abbreviated byte budget") {
		t.Errorf("render tripwire %% must recompute from renderWarnBytes/renderBudgetBytes, got:\n%s", out)
	}
}

// TestAdviceBudgetAlert_R4TripwireFires is the acceptance proof for R4
// (ka-0000002748): it renders the rule and feeds promtool synthetic series for
// an over-budget render (49000 B > 48000) and an over-cap pool (460 > 450),
// asserting each tripwire fires, and that today's levels (~42 KB render, ~354
// pool) stay quiet. The promtool cases live in advice_budget_rules_test.yaml.
//
// Skips gracefully when helm/yq/promtool are not on PATH (a bare `go test`
// without the chart toolchain) — the helm-render tests above still run.
func TestAdviceBudgetAlert_R4TripwireFires(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"helm", "yq", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping promtool firing proof", bin)
		}
	}

	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "advice_rules.yaml")

	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --show-only templates/advice-budget-alert.yaml "+
			"--set agents.metrics.prometheusRule.adviceBudget.enabled=true | yq eval '.spec' -")
	out, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render rules failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(rulesPath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	tmplBytes, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "advice_budget_rules_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testYAML := strings.ReplaceAll(string(tmplBytes), "__RULES_FILE__", rulesPath)
	testPath := filepath.Join(tmp, "advice_test.yaml")
	if err := os.WriteFile(testPath, []byte(testYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := exec.Command("promtool", "test", "rules", testPath).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool firing proof failed: %v\n%s", err, res)
	}
	if !strings.Contains(string(res), "SUCCESS") {
		t.Errorf("expected promtool SUCCESS, got:\n%s", res)
	}
}

func TestAdviceBudgetAlert_RuleSelectorLabelsApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/advice-budget-alert.yaml",
		"agents.metrics.prometheusRule.adviceBudget.enabled=true",
		"agents.metrics.prometheusRule.adviceBudget.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
	)
	if !strings.Contains(out, "release: kube-prometheus-stack") {
		t.Errorf("prometheusRuleSelectorLabels must land on PrometheusRule metadata, got:\n%s", out)
	}
}
