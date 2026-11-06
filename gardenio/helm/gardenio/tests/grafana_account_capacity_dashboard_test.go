package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Account Capacity & Rate-Limit Health dashboard (ka-0000003179).
//
// A dedicated Grafana board over the credential-account capacity/health family
// (gardenio_account_* with labels {account,env}), which the controller sets from
// the Anthropic rate-limit probe on each credential-account bead
// (gardenio/controller/internal/metrics/metrics.go). Before this board these
// series had no first-class home — cost-overview only lightly references them —
// so an operator could not see, on one board, WHICH account is near its 5h/7d
// window ceiling, WHICH is throttled/warming, and HOW load is distributed.
// This closes idea #8 ("warming/throttle × utilisation correlation") from the
// GRADE audit idea-catalog (docs/grafana-grade/.../idea-catalog.md).
//
// The dashboards/*.json source rides the grafana-bridge image and is provisioned
// at runtime as a type=dashboard bead (ka-0000002799), so these guards read the
// file directly from the dashboards/ source dir (readDashboardJSON) — the same
// bytes baked into the image. The panel queries were each replayed against the
// live gardenio-vmsingle during authoring (GRADE-G / accuracy-first), so this
// test guards the honesty properties that keep them accurate over time:
//   - the board embeds as PARSEABLE JSON under its stable uid,
//   - every panel target binds the prometheus datasource var (${DS_PROMETHEUS}),
//   - every account aggregation dedups the per-pod gauge republish with
//     `max by (account` — never a bare sum that would multiply by pod count,
//   - E2E/probe throwaway accounts are excluded with the byte-identical matcher
//     the beads-viewer rollup and cost-overview use (ka-0000005167),
//   - the whole account_* capacity family is actually queried somewhere.

const accountCapacityUID = "gardenio-account-capacity"

func accountCapacityDashboard(t *testing.T) dashboard {
	t.Helper()
	raw := readDashboardJSON(t, "account-capacity.json")
	var d dashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("account-capacity.json is not valid JSON: %v", err)
	}
	return d
}

func TestAccountCapacityDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	d := accountCapacityDashboard(t)
	if d.UID != accountCapacityUID {
		t.Errorf("account-capacity board must carry its stable uid %q; got %q", accountCapacityUID, d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("account-capacity board must have panels")
	}
}

// The board must query the full credential-account capacity/health family, so an
// operator sees utilization ceilings, throttle/warming state, and load in one
// place. Each metric is a live series on prod (verified during authoring).
func TestAccountCapacityDashboard_QueriesTheAccountFamily(t *testing.T) {
	t.Parallel()
	exprs := allExprs(accountCapacityDashboard(t))
	wantMetric := []string{
		"gardenio_account_utilization_5h",
		"gardenio_account_utilization_7d",
		"gardenio_account_throttled",
		"gardenio_account_warming",
		"gardenio_account_sessions_current",
		"gardenio_account_requests_current",
		"gardenio_account_input_tokens_current",
		"gardenio_account_output_tokens_current",
		"gardenio_account_cache_read_tokens_current",
	}
	for _, m := range wantMetric {
		if !strings.Contains(exprs, m) {
			t.Errorf("account-capacity board must query %s somewhere; not found", m)
		}
	}
}

// Every account aggregation must dedup the per-pod gauge republish. The account_*
// gauges are re-exported by every controller/agents pod, so a bare
// sum(gardenio_account_*) would multiply each account's value by the pod count.
// The honest form aggregates the per-account max first (`max by (account` ...),
// the same dedup cost-overview uses on gardenio_account_cost_usd. Guard that no
// panel regresses to a bare sum over a raw account gauge.
func TestAccountCapacityDashboard_DedupsPerPodChurn(t *testing.T) {
	t.Parallel()
	d := accountCapacityDashboard(t)
	for _, p := range d.Panels {
		for _, tgt := range p.Targets {
			e := tgt.Expr
			if e == "" {
				continue
			}
			// Any panel that touches a raw account gauge must do so through a
			// max-by-account (or count of one) — never a bare sum(gardenio_account_*).
			if strings.Contains(e, "gardenio_account_") && !strings.Contains(e, "max by (account") {
				t.Errorf("panel %q query does not dedup per-pod churn with 'max by (account': %s", p.Title, e)
			}
		}
	}
}

// The board must exclude E2E/probe throwaway accounts with the byte-identical
// matcher the beads-viewer by-account rollup and cost-overview use
// (ka-0000005167) — otherwise a harness run seeds phantom accounts that outlive
// their bead for the vmsingle retention window and pollute every panel + the
// $account dropdown.
func TestAccountCapacityDashboard_ExcludesE2EAccounts(t *testing.T) {
	t.Parallel()
	d := accountCapacityDashboard(t)
	const matcher = `account!~"(?i:e2e-.*|probe-acct-.*)"`
	for _, p := range d.Panels {
		for _, tgt := range p.Targets {
			e := tgt.Expr
			if e == "" || !strings.Contains(e, "gardenio_account_") {
				continue
			}
			if !strings.Contains(e, matcher) {
				t.Errorf("panel %q must exclude E2E/probe accounts with %s; got: %s", p.Title, matcher, e)
			}
		}
	}
	// The $account template var definition must carry the same exclusion so the
	// dropdown never lists a throwaway account. Parse it out (rather than string
	// matching the escaped raw JSON) so the check reads the real value.
	raw := readDashboardJSON(t, "account-capacity.json")
	var doc struct {
		Templating struct {
			List []struct {
				Name       string `json:"name"`
				Definition string `json:"definition"`
				Query      string `json:"query"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("account-capacity.json parse: %v", err)
	}
	var found bool
	for _, v := range doc.Templating.List {
		if v.Name != "account" {
			continue
		}
		found = true
		if !strings.Contains(v.Definition, matcher) || !strings.Contains(v.Query, matcher) {
			t.Errorf("$account template var must exclude E2E/probe accounts; definition=%q query=%q", v.Definition, v.Query)
		}
	}
	if !found {
		t.Error("account-capacity board must define an $account template var")
	}
}

// Every panel target must bind the prometheus datasource template var, matching
// the house convention (cost-overview / system-health / fleet-lifecycle all use
// ${DS_PROMETHEUS}); a stray hardcoded/other datasource would fail to provision
// or render empty.
func TestAccountCapacityDashboard_BindsPrometheusDatasourceVar(t *testing.T) {
	t.Parallel()
	raw := readDashboardJSON(t, "account-capacity.json")
	// Parse loosely to inspect each panel target's datasource uid.
	var doc struct {
		Panels []struct {
			Type    string `json:"type"`
			Title   string `json:"title"`
			Targets []struct {
				Datasource struct {
					UID string `json:"uid"`
				} `json:"datasource"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("account-capacity.json parse: %v", err)
	}
	for _, p := range doc.Panels {
		if p.Type == "row" {
			continue
		}
		for _, tgt := range p.Targets {
			if tgt.Datasource.UID != "${DS_PROMETHEUS}" {
				t.Errorf("panel %q target binds datasource uid %q, want ${DS_PROMETHEUS}", p.Title, tgt.Datasource.UID)
			}
		}
	}
}

// The companion AccountThrottled gap-alert (ka-0000003179) deep-links to this
// board. These guards mirror the sibling gap-alert tests (vmalert_gapalerts_test.go,
// vmalert_xcheck_drift_alert_test.go): default-off, opt-in renders in the
// FIREABLE group, and the deep link points at the account-capacity board.

func TestGapAlerts_AccountThrottledDefaultOff(t *testing.T) {
	t.Parallel()
	// Enabled gap-alert pack + notifier, but accountThrottled left at its default.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(out, "alert: AccountThrottled") {
		t.Errorf("AccountThrottled must be OFF by default, got:\n%s", out)
	}
	// Sanity: the rest of the fireable pack still renders.
	if !strings.Contains(out, "alert: UnattributedSpendHigh") {
		t.Error("baseline fireable pack should render UnattributedSpendHigh")
	}
}

func TestGapAlerts_AccountThrottledOptInRendersFireable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.accountThrottled.enabled=true")...)
	if !strings.Contains(out, "alert: AccountThrottled") {
		t.Fatalf("accountThrottled.enabled=true must render the alert, got:\n%s", out)
	}
	// It is fireable-today (live series on prod), so it must land in the fireable
	// group gardenio.gap-alerts — NOT the blocked pack (gardenio.gap-alerts-blocked),
	// which only renders behind a prereq toggle. Assert the alert appears before
	// the blocked group header in the rendered rule file.
	at := strings.Index(out, "alert: AccountThrottled")
	blocked := strings.Index(out, "name: gardenio.gap-alerts-blocked")
	if blocked != -1 && at > blocked {
		t.Error("AccountThrottled must render in the fireable group, not gardenio.gap-alerts-blocked")
	}
	// The alert must query the throttle gauge with the churn-safe count form and
	// exclude E2E/probe accounts (byte-identical to the dashboard + rollup matcher).
	seg := out[at:]
	if !strings.Contains(seg, "gardenio_account_throttled") {
		t.Error("AccountThrottled expr must read gardenio_account_throttled")
	}
	if !strings.Contains(seg, `account!~"(?i:e2e-.*|probe-acct-.*)"`) {
		t.Error("AccountThrottled expr must exclude E2E/probe accounts")
	}
	if !strings.Contains(seg, "max by (account)") {
		t.Error("AccountThrottled expr must dedup per-pod churn with max by (account)")
	}
}

func TestGapAlerts_AccountThrottledDeepLinksAccountCapacityBoard(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.accountThrottled.enabled=true",
			"vmalert.gapAlerts.grafanaBaseURL=https://prod.gardenio.acme.example/grafana")...)
	seg := out[strings.Index(out, "alert: AccountThrottled"):]
	// Trim to just this alert's block (up to the next alert or group).
	if next := strings.Index(seg[len("alert: AccountThrottled"):], "alert: "); next != -1 {
		seg = seg[:len("alert: AccountThrottled")+next]
	}
	if !strings.Contains(seg, "/d/gardenio-account-capacity/") {
		t.Errorf("AccountThrottled dashboard link must point at the account-capacity board, got:\n%s", seg)
	}
}
