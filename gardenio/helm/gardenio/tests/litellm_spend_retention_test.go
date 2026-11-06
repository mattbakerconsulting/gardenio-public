package tests

import (
	"strings"
	"testing"
)

// gd-oih6JxjJMM: db-init owns the litellm database's statement_timeout, so
// Postgres cancels a runaway GET /spend/tags scan instead of running it to the
// end after the caller hung up. Default 20s; empty resets it.
func TestLitellm_DbInit_StatementTimeout(t *testing.T) {
	t.Parallel()
	base := []string{"litellm.enabled=true", "postgres.enabled=true", "litellm.dbInit.enabled=true"}

	dbInit := sectionAfter(helmTemplate(t, "templates/litellm/deployment.yaml", base...), "name: db-init")
	for _, want := range []string{
		`STATEMENT_TIMEOUT="20s"`,
		`SET statement_timeout = '$STATEMENT_TIMEOUT'`,
		`RESET statement_timeout`,
	} {
		if !strings.Contains(dbInit, want) {
			t.Errorf("default db-init must contain %q", want)
		}
	}

	off := sectionAfter(helmTemplate(t, "templates/litellm/deployment.yaml",
		append(base, "litellm.dbInit.statementTimeout=")...), "name: db-init")
	if !strings.Contains(off, `STATEMENT_TIMEOUT=""`) {
		t.Error(`statementTimeout="" must render an empty STATEMENT_TIMEOUT (the RESET branch)`)
	}

	out := helmTemplateExpectError(t, "templates/litellm/deployment.yaml",
		"--set", "litellm.enabled=true", "--set", "postgres.enabled=true",
		"--set", "litellm.dbInit.statementTimeout=20s; DROP DATABASE litellm")
	if !strings.Contains(out, "litellm.dbInit.statementTimeout") {
		t.Errorf("a non-duration statementTimeout must fail the render naming the value; got:\n%s", out)
	}
}

// gd-oih6JxjJMM: spend-log retention ships OFF (it shrinks the lifetime totals
// the controller reads from /spend/tags; see values.yaml). A period renders
// LiteLLM's general_settings keys, and setting it twice fails the render.
func TestLitellm_SpendLogsRetention(t *testing.T) {
	t.Parallel()
	base := []string{"litellm.enabled=true", "postgres.enabled=true"}

	if def := helmTemplate(t, "templates/litellm/configmap.yaml", base...); strings.Contains(def, "maximum_spend_logs_") {
		t.Error("default render must not set any maximum_spend_logs_* key")
	}

	on := helmTemplate(t, "templates/litellm/configmap.yaml",
		append(base, "litellm.spendLogsRetention.period=7d", "litellm.spendLogsRetention.cron=17 * * * *")...)
	for _, want := range []string{
		`maximum_spend_logs_retention_period: "7d"`,
		`maximum_spend_logs_retention_interval: "1d"`,
		`maximum_spend_logs_cleanup_cron: "17 * * * *"`,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("retention render must contain %q", want)
		}
	}

	out := helmTemplateExpectError(t, "templates/litellm/configmap.yaml",
		"--set", "litellm.enabled=true", "--set", "postgres.enabled=true",
		"--set", "litellm.spendLogsRetention.period=7d",
		"--set", "litellm.extraGeneralSettings.maximum_spend_logs_retention_period=30d")
	if !strings.Contains(out, "not both") {
		t.Errorf("retention set in both places must fail the render; got:\n%s", out)
	}
}

// gd-Um9gnz9FuW: litellm.tagSpendSource reaches the controller as
// LITELLM_TAG_SPEND_SOURCE; empty renders nothing (the controller defaults to
// auto). Pinning daily is the step before spendLogsRetention.
func TestController_TagSpendSourceEnv(t *testing.T) {
	t.Parallel()
	base := []string{"agents.enabled=true", "litellm.enabled=true", "postgres.enabled=true"}

	if def := helmTemplate(t, "templates/controller/deployment.yaml", base...); strings.Contains(def, "LITELLM_TAG_SPEND_SOURCE") {
		t.Error("default render must not set LITELLM_TAG_SPEND_SOURCE")
	}
	daily := helmTemplate(t, "templates/controller/deployment.yaml", append(base, "litellm.tagSpendSource=daily")...)
	if !strings.Contains(daily, "- name: LITELLM_TAG_SPEND_SOURCE\n              value: \"daily\"") {
		t.Error(`litellm.tagSpendSource=daily must render LITELLM_TAG_SPEND_SOURCE="daily" on the controller`)
	}
}
