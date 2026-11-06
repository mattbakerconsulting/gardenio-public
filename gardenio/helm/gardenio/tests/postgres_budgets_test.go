package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Postgres efficiency budgets (gd-ylgk1jBhLi): the vmalert ConfigMap key
// postgres-budgets.yaml renders only with the exporter and a notifier, reads
// only series the exporter emits, and each budget fires on a breach and stays
// quiet on a near miss (postgres_budgets_rules_test.yaml).

var pgBudgetArgs = append(append([]string{}, gapAlertsNotifierArgs...),
	"postgres.enabled=true",
	"postgres.exporter.enabled=true",
)

var pgBudgetAlerts = []string{
	"PostgresQueryDominatesExecTime",
	"PostgresSlowFrequentQuery",
	"PostgresLongActiveQuery",
	"PostgresQueryPileUp",
	"PostgresTableGrowth",
	"PostgresBeadsCacheHitLow",
	"PostgresCPUSaturatingNode",
}

func TestPostgresBudgets_Gating(t *testing.T) {
	t.Parallel()
	on := helmTemplate(t, "templates/vmalert/configmap.yaml", pgBudgetArgs...)
	if !strings.Contains(on, "postgres-budgets.yaml: |") {
		t.Fatalf("postgres-budgets key must render with the exporter and a notifier, got:\n%s", on)
	}
	for _, a := range pgBudgetAlerts {
		if !strings.Contains(on, "alert: "+a+"\n") {
			t.Errorf("alert %s missing", a)
		}
	}

	for name, args := range map[string][]string{
		"exporter off": append(append([]string{}, gapAlertsNotifierArgs...), "postgres.enabled=true"),
		"toggle off":   append(append([]string{}, pgBudgetArgs...), "vmalert.postgresBudgets.enabled=false"),
		"no notifier": {"vmalert.enabled=true", "alertmanager.enabled=false", "vmalert.notifierURL=",
			"vmalert.notifierBlackhole=false", "postgres.enabled=true", "postgres.exporter.enabled=true"},
	} {
		if out := helmTemplate(t, "templates/vmalert/configmap.yaml", args...); strings.Contains(out, "postgres-budgets.yaml") {
			t.Errorf("%s: postgres-budgets key must not render, got:\n%s", name, out)
		}
	}
}

func TestPostgresBudgets_Thresholds(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", append(append([]string{}, pgBudgetArgs...),
		"vmalert.postgresBudgets.shareRatio=0.3",
		"vmalert.postgresBudgets.longQuerySeconds=60",
		"vmalert.postgresBudgets.growthBytes=1073741824",
		"vmalert.postgresBudgets.excludeApps=gardenio-backup|pg_dump",
		"vmalert.postgresBudgets.severity=page",
	)...)
	for alert, want := range map[string]string{
		"PostgresQueryDominatesExecTime": "group_left sum(increase(pg_top_statements_exec_seconds{component=\"postgres\"}[1h])) > 0.3)",
		"PostgresLongActiveQuery":        "application_name!~\"gardenio-backup|pg_dump\"}) > 60",
		"PostgresQueryPileUp":            "application_name!~\"gardenio-backup|pg_dump\"}) > 3",
		"PostgresTableGrowth":            "> 1073741824 or",
		"PostgresCPUSaturatingNode":      `severity: "page"`,
	} {
		block := strings.Join(strings.Fields(alertBlock(t, out, alert)), " ")
		if !strings.Contains(block, want) {
			t.Errorf("%s: override not applied, %q missing in:\n%s", alert, want, block)
		}
	}
}

// TestPostgresBudgets_ReadsOnlyEmittedSeries: a rule on a series the exporter
// does not emit is silent forever, with nothing saying why.
func TestPostgresBudgets_ReadsOnlyEmittedSeries(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", pgBudgetArgs...)
	emitted := pgExporterEmittedSeries(t)
	read := map[string]bool{}
	for _, name := range pgCustomSeries.FindAllString(out[strings.Index(out, "postgres-budgets.yaml"):], -1) {
		read[name] = true
	}
	if len(read) < 6 {
		t.Fatalf("rules read only %d custom series — did the queries get renamed?", len(read))
	}
	var missing []string
	for name := range read {
		if !emitted[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("budget rules read series queries.yaml does not emit: %s", strings.Join(missing, ", "))
	}
}

func TestPostgresBudgets_Fire(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"helm", "yq", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping promtool firing proof", bin)
		}
	}
	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "postgres_budgets_rules.yaml")
	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --show-only templates/vmalert/configmap.yaml "+
			"--set "+strings.Join(pgBudgetArgs, " --set ")+" | "+
			`yq eval '.data."postgres-budgets.yaml"' - | `+
			`yq eval '.groups[].rules |= map(del(.annotations))' -`)
	out, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render rules failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(rulesPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := exec.Command("promtool", "check", "rules", rulesPath).CombinedOutput(); err != nil {
		t.Fatalf("promtool check rules failed: %v\n%s", err, res)
	}
	tmpl, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "postgres_budgets_rules_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	promtoolTestRules(t, "postgres_budgets_test.yaml", strings.ReplaceAll(string(tmpl), "__RULES_FILE__", rulesPath))
}

// TestPostgresBudgets_ControllerReportEnv: the controller's daily report runs
// only with the exporter, and gets vmalert.postgresBudgets as JSON carrying
// every threshold pgbudget.Budgets reads, so a day is judged by the rules'
// own numbers.
func TestPostgresBudgets_ControllerReportEnv(t *testing.T) {
	t.Parallel()
	const tmpl = "templates/controller/deployment.yaml"
	out := helmTemplate(t, tmpl, "agents.enabled=true", "postgres.enabled=true", "postgres.exporter.enabled=true",
		"postgres.exporter.report.parentEpic=gd-PkZRCb0BuB")
	for _, want := range []string{
		"- name: GARDENIO_PG_BUDGET_REPORT_HOUR\n              value: \"8\"",
		"- name: GARDENIO_PG_BUDGET_LAND\n              value: \"gardenio\"",
		"- name: GARDENIO_PG_BUDGET_EPIC\n              value: \"gd-PkZRCb0BuB\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("controller env missing %q", want)
		}
	}
	i := strings.Index(out, "- name: GARDENIO_PG_BUDGETS\n")
	if i < 0 {
		t.Fatalf("controller env has no GARDENIO_PG_BUDGETS")
	}
	line := strings.SplitN(out[i:], "\n", 3)[1]
	for _, key := range []string{"shareRatio", "shareMinTotalSeconds", "slowMeanSeconds", "slowMinCallsPerHour",
		"excludeQueryRegex", "growthRatio", "growthBytes", "growthMinBytes"} {
		if !strings.Contains(line, `\"`+key+`\":`) {
			t.Errorf("GARDENIO_PG_BUDGETS lacks %s: %s", key, line)
		}
	}

	for name, args := range map[string][]string{
		"exporter off": {"agents.enabled=true", "postgres.enabled=true"},
		"report off":   {"agents.enabled=true", "postgres.enabled=true", "postgres.exporter.enabled=true", "postgres.exporter.report.enabled=false"},
	} {
		if strings.Contains(helmTemplate(t, tmpl, args...), "GARDENIO_PG_BUDGET") {
			t.Errorf("%s: the report env must not render", name)
		}
	}
}
