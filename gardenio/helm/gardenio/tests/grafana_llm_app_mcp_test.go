package tests

import (
	"strings"
	"testing"
)

// grafana-llm-app EMBEDDED MCP server: managed service account + curated tool
// surface (C7, ka-0000001262, epic ka-0000002011 — OSS Grafana chat box, Path C).
//
// The chat box (C3) connects to grafana-llm-app's *embedded* mcp-grafana server
// (resource path /api/plugins/grafana-llm-app/resources/mcp/grafana), NOT the
// standalone Track-A `mcpGrafana` pod (which stays read-only). Off Grafana Cloud
// that embedded server authenticates to the Grafana API ONLY via the plugin's
// managed external service account (cfg.PluginAppClientSecret()), which Grafana
// mints from grafana-llm-app's plugin.json iam block.
//
// SECURITY-OF-RECORD (corrects C7 !4943, ka-0000007430): that iam block is
// BROADER than dashboards+folders — as of grafana-llm-app 1.0.8 it also grants
// alert.rules:read/create/write/DELETE, datasources:read/query, and
// orgs/teams/users reads. So the managed-SA RBAC does NOT scope the chat box
// down; the toolset family allow-list (layer a, below) is the LOAD-BEARING
// control. Two things make that work and keep it tight:
//   1. deployment.yaml turns on the managed-SA feature toggles (gated on
//      grafana.llm.enabled) — without them PluginAppClientSecret() is empty and
//      every embedded MCP call 302-loops to /login (read AND write break).
//   2. configmap.yaml provisions jsonData.mcp.toolsets as an explicit per-family
//      allow-list (read + dashboard/folder create only) — a nil family defaults
//      to ENABLED upstream, so denials must be explicit false. Never relax the
//      list for alerting/admin without a separate RBAC review.
//
// These tests pin: (1) default-off/additive contract, (2) the managed-SA toggles
// render when enabled, (3) the toolset allow-list enables read + dashboard +
// folder and explicitly disables the write/admin families, (4) featureToggles
// REPLACE semantics, (5) the mcp: block never renders as a bare null key, and
// (6) the allow-list holds when layered under an overlay-style write profile.

// TestGrafanaLLMMCP_DisabledByDefault — with grafana on but llm untouched, none
// of the C7 artifacts render. Guards the additive contract.
func TestGrafanaLLMMCP_DisabledByDefault(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	for _, needle := range []string{
		"GF_FEATURE_TOGGLES_ENABLE",
		"GF_AUTH_MANAGED_SERVICE_ACCOUNTS_ENABLED",
		"externalServiceAccounts",
	} {
		if strings.Contains(dep, needle) {
			t.Errorf("grafana deployment must not contain %q by default (additive contract, ka-0000001262)", needle)
		}
	}
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(cm, "toolsets") || strings.Contains(cm, "mcp:") {
		t.Error("grafana provisioning configmap must not render the mcp toolset block by default (ka-0000001262)")
	}
}

// TestGrafanaLLMMCP_ManagedSATogglesRender — enabling the plugin renders BOTH
// managed-SA env vars with the externalServiceAccounts feature toggle. This is
// the prerequisite for the embedded MCP to authenticate to Grafana at all.
func TestGrafanaLLMMCP_ManagedSATogglesRender(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
	)
	if !strings.Contains(dep, "GF_FEATURE_TOGGLES_ENABLE") ||
		!strings.Contains(dep, "externalServiceAccounts") {
		t.Errorf("managed-SA feature toggle must render when grafana.llm.enabled (ka-0000001262); got:\n%s", dep)
	}
	if !strings.Contains(dep, "GF_AUTH_MANAGED_SERVICE_ACCOUNTS_ENABLED") {
		t.Error("GF_AUTH_MANAGED_SERVICE_ACCOUNTS_ENABLED must render when grafana.llm.enabled (ka-0000001262)")
	}
}

// TestGrafanaLLMMCP_FeatureTogglesVerbatim — featureToggles is fed VERBATIM to
// GF_FEATURE_TOGGLES_ENABLE, so an operator who explicitly KEEPS
// externalServiceAccounts can append extra toggles. The comma is escaped so
// helm's --set parser keeps it as one value. (Formerly named "...Mergeable" —
// the template does NOT merge; see ...ReplaceSemantics below, ka-0000007430.)
func TestGrafanaLLMMCP_FeatureTogglesVerbatim(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		`grafana.llm.featureToggles=externalServiceAccounts\,somethingElse`,
	)
	if !strings.Contains(dep, "externalServiceAccounts,somethingElse") {
		t.Errorf("grafana.llm.featureToggles must be fed verbatim to GF_FEATURE_TOGGLES_ENABLE; got:\n%s", dep)
	}
}

// TestGrafanaLLMMCP_FeatureTogglesReplaceSemantics — pins the DOCUMENTED (and
// dangerous) replace behavior: the template feeds featureToggles verbatim and
// the "externalServiceAccounts" default fires ONLY when the value is empty, so
// an override that OMITS externalServiceAccounts silently strips the managed
// SA's identity (→ embedded MCP 302-loops to /login, both read+write down).
// This test makes that contract explicit so a future "make it mergeable" change
// is a deliberate, test-breaking decision rather than a silent regression. The
// comments in values.yaml + grafana/deployment.yaml document the replace-with-
// care requirement: callers MUST keep externalServiceAccounts in the list.
func TestGrafanaLLMMCP_FeatureTogglesReplaceSemantics(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		// Operator override that forgets externalServiceAccounts.
		"grafana.llm.featureToggles=publicDashboards",
	)
	// Extract the GF_FEATURE_TOGGLES_ENABLE value line.
	var toggleVal string
	lines := strings.Split(dep, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "GF_FEATURE_TOGGLES_ENABLE") && i+1 < len(lines) {
			toggleVal = lines[i+1]
			break
		}
	}
	if toggleVal == "" {
		t.Fatalf("GF_FEATURE_TOGGLES_ENABLE must render when grafana.llm.enabled; got:\n%s", dep)
	}
	if !strings.Contains(toggleVal, "publicDashboards") {
		t.Errorf("override value must be fed verbatim; got value line: %q", toggleVal)
	}
	// REPLACE semantics: the override is NOT merged with the default, so
	// externalServiceAccounts is absent. This is the documented footgun.
	if strings.Contains(toggleVal, "externalServiceAccounts") {
		t.Errorf("featureToggles uses REPLACE semantics (ka-0000007430): an override "+
			"omitting externalServiceAccounts must NOT silently re-add it. If this "+
			"fails the template was made mergeable — update values.yaml/deployment.yaml "+
			"docs which warn callers they MUST keep externalServiceAccounts. value line: %q", toggleVal)
	}
}

// TestGrafanaLLMMCP_ToolsetAllowList — the provisioned toolsets enable the
// read + dashboard-create families and EXPLICITLY disable the write/admin
// families. The explicit-false denials are load-bearing: nil ⇒ enabled upstream.
func TestGrafanaLLMMCP_ToolsetAllowList(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
	)
	if !strings.Contains(cm, "mcp:") || !strings.Contains(cm, "toolsets:") {
		t.Fatalf("configmap must render jsonData.mcp.toolsets when grafana.llm.enabled (ka-0000001262); got:\n%s", cm)
	}
	// Families that must be ENABLED (read + dashboard-create).
	for _, enabled := range []string{
		"dashboard: true",
		"folder: true",
		"search: true",
		"datasource: true",
		"prometheus: true",
		"navigation: true",
		"rendering: true",
	} {
		if !strings.Contains(cm, enabled) {
			t.Errorf("toolset %q must be enabled in the chat-box allow-list (ka-0000001262); got:\n%s", enabled, cm)
		}
	}
	// Families that must be EXPLICITLY DISABLED (write/admin/unused). Explicit
	// false is required because a nil family defaults to enabled upstream.
	for _, disabled := range []string{
		"admin: false",
		"alerting: false",
		"annotations: false",
		"oncall: false",
		"incident: false",
		"asserts: false",
		"sift: false",
		"pyroscope: false",
		"loki: false",
		"clickhouse: false",
		"cloudwatch: false",
		"elasticsearch: false",
		"examples: false",
		"searchlogs: false",
	} {
		if !strings.Contains(cm, disabled) {
			t.Errorf("toolset %q must be EXPLICITLY disabled in the chat-box allow-list (nil would enable it, ka-0000001262); got:\n%s", disabled, cm)
		}
	}
}

// TestGrafanaLLMMCP_NoWriteFamiliesEnabled — defense-in-depth: assert NONE of
// the write/admin families are accidentally set to true. Catches a copy-paste
// regression where a denied family flips to enabled.
func TestGrafanaLLMMCP_NoWriteFamiliesEnabled(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
	)
	for _, forbidden := range []string{
		"admin: true",
		"alerting: true",
		"annotations: true",
		"oncall: true",
	} {
		if strings.Contains(cm, forbidden) {
			t.Errorf("write/admin family %q must NEVER be enabled on the chat box (ka-0000001262); got:\n%s", forbidden, cm)
		}
	}
}

// TestGrafanaLLMMCP_DisabledFlag — mcp.disabled=true turns the embedded MCP
// server off entirely (provisions disabled: true under mcp).
func TestGrafanaLLMMCP_DisabledFlag(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.llm.mcp.disabled=true",
	)
	if !strings.Contains(cm, "disabled: true") {
		t.Errorf("mcp.disabled=true must render `disabled: true` under the mcp block (ka-0000001262); got:\n%s", cm)
	}
}

// TestGrafanaLLMMCP_NoBareMcpNullRender — regression guard for the dangling
// `mcp:` null render (ka-0000007430 item 5). Before the fix, configmap.yaml's
// `{{- with .Values.grafana.llm.mcp }}` would emit a bare `mcp:` key (no value)
// when an overlay supplied an mcp block with .toolsets explicitly nulled. The
// template now guards with `{{- if or .disabled .toolsets }}` so an
// "all-false" mcp block emits nothing.
//
// FOOTGUN (ka-0000007430 item 5 follow-up; documented in configmap.yaml and
// values.yaml): the guard's render-suppression is byte-correct but is NOT a
// fail-CLOSED security control. If an overlay clears the chart-default
// allow-list via `mcp.toolsets: null` without also setting `mcp.disabled:
// true`, NO `mcp:` block lands in provisioning → grafana-llm-app's
// Settings.MCP.Toolsets is nil → upstream's "nil ⇒ ENABLED" default applies
// to every family. The DOCUMENTED operator escape hatch is `mcp.disabled:
// true` (which DOES render), NOT nulling toolsets. An overlay that enables MCP
// re-states the full explicit-false deny-list — this test locks the guard's
// render contract.
//
// Verified 2026-06-28: an empty-map override (`toolsets: {}`) does NOT reach
// the guard's falsy branch because helm deep-merges {} with the chart-default
// map → the full chart-default allow-list survives. Only `toolsets: null`
// erases the chart default, so only the null case can hit the guard.
func TestGrafanaLLMMCP_NoBareMcpNullRender(t *testing.T) {
	t.Parallel()
	cm := helmTemplateJSON(t,
		"templates/grafana/configmap.yaml",
		map[string]string{
			"grafana.llm.mcp": `{"disabled":false,"toolsets":null}`,
		},
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
	)
	// Look only inside the grafana-llm-app provisioning entry. The app's own
	// org-level `disabled: false` (the disabled-the-plugin flag) is unrelated;
	// the mcp key, if present, would appear under `jsonData:` between `openAI:`
	// and `secureJsonData:`.
	startIdx := strings.Index(cm, "openAI:")
	endIdx := strings.Index(cm, "secureJsonData:")
	if startIdx == -1 || endIdx == -1 || endIdx <= startIdx {
		t.Fatalf("could not locate jsonData region in rendered configmap:\n%s", cm)
	}
	jsonDataRegion := cm[startIdx:endIdx]
	if strings.Contains(jsonDataRegion, "mcp:") {
		t.Errorf("bare `mcp:` key must NOT render when toolsets is null (ka-0000007430 item 5); jsonData region:\n%s", jsonDataRegion)
	}
}
