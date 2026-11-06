package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// perf-overview dashboard content guards (task ka-0000002367, epic ka-0000005085).
//
// perf-overview.json is the headline board for the perf-monorepo estate: ECS task
// health, RDS/Aurora engine load, and the k6 load-test request KPIs (rate, latency
// p50/p95/p99, error rate). Like the sibling dashboards it is NOT rendered as a
// ConfigMap (ka-0000002799 removed those templates); the JSON is baked into the
// grafana-bridge image and provisioned at runtime, so these tests read the bytes
// straight from dashboards/ — the same bytes the bridge ships.
//
// The invariants guarded here are the ones that silently rot a metrics board:
//   - valid JSON + the stable uid the dashboard-links reference;
//   - every panel scopes to source="perf" so no gardenio-native series leak in;
//   - the AWS families are the exact federated metric names (a typo renders an
//     empty panel with no error);
//   - the honest-empty gate — every data panel carries the "No data" noValue that
//     mirrors monorepo-db-performance.json when the perf-federation scrape is
//     down (ka-0000002619); and
//   - the two headline load tiles tolerate a keycloak gap WITHOUT fabricating a
//     zero: a bare sum(fastapi)+sum(http_server) collapses to empty whenever
//     keycloak's http_server_* series are momentarily absent (blanking a tile
//     under live k6 load), but `or vector(0)` fixed that by painting a confident
//     0 over a fully blind federation. Both tiles now sum the two app families
//     through one `{__name__=~"..."}` union selector and, on the error tile,
//     earn each error leg's zero off that union rate — so partial absence still
//     reads live and TOTAL absence falls through to the honest-empty noValue
//     (ka-0000006531).

type perfOverviewBoard struct {
	UID         string `json:"uid"`
	Description string `json:"description"`
	Style       string `json:"style"`
	Panels      []struct {
		ID          int    `json:"id"`
		Type        string `json:"type"`
		Title       string `json:"title"`
		FieldConfig struct {
			Defaults struct {
				NoValue string `json:"noValue"`
			} `json:"defaults"`
		} `json:"fieldConfig"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
	Templating struct {
		List []struct {
			Name  string `json:"name"`
			Query struct {
				Query string `json:"query"`
			} `json:"query"`
		} `json:"list"`
	} `json:"templating"`
}

func loadPerfOverviewBoard(t *testing.T) perfOverviewBoard {
	t.Helper()
	raw := readDashboardJSON(t, "perf-overview.json")
	var b perfOverviewBoard
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("dashboards/perf-overview.json is not valid JSON: %v", err)
	}
	if b.UID != "perf-overview" {
		t.Fatalf("perf-overview board must carry its stable uid; got %q", b.UID)
	}
	return b
}

// dataPanels returns only the panels that actually query data (skips row + text).
func (b perfOverviewBoard) dataPanels() []int {
	var ids []int
	for _, p := range b.Panels {
		if p.Type == "row" || p.Type == "text" {
			continue
		}
		ids = append(ids, p.ID)
	}
	return ids
}

func TestPerfOverviewDashboard_ValidAndDarkThemed(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	// Tufte dark-theme ask from the task description.
	if b.Style != "dark" {
		t.Errorf("perf-overview must be dark-themed (style=\"dark\"); got %q", b.Style)
	}
	if len(b.dataPanels()) == 0 {
		t.Fatal("perf-overview has no data panels")
	}
}

func TestPerfOverviewDashboard_EveryTargetScopedToPerf(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	for _, p := range b.Panels {
		if p.Type == "row" || p.Type == "text" {
			continue
		}
		for _, tg := range p.Targets {
			if !strings.Contains(tg.Expr, `source="perf"`) {
				t.Errorf("panel id=%d %q has a target without source=\"perf\" scoping "+
					"(would mix in gardenio-native series): %q", p.ID, p.Title, tg.Expr)
			}
		}
	}
}

func TestPerfOverviewDashboard_HonestEmptyGate(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	// The board's honest-empty contract: every data panel names the paused-scrape
	// noValue so a down federation reads "No data", not a phantom zero
	// (ka-0000002619). Mirrors monorepo-db-performance.json.
	for _, p := range b.Panels {
		if p.Type == "row" || p.Type == "text" {
			continue
		}
		nv := p.FieldConfig.Defaults.NoValue
		if !strings.Contains(nv, "perf-federation") {
			t.Errorf("data panel id=%d %q must set the honest-empty noValue referencing "+
				"the perf-federation gate; got %q", p.ID, p.Title, nv)
		}
	}
	// The description must document the gate + the tracking bead.
	if !strings.Contains(b.Description, "ka-0000002619") {
		t.Errorf("board description must cite the federation-down tracking bead ka-0000002619; got %q", b.Description)
	}
}

func TestPerfOverviewDashboard_UsesFederatedMetricFamilies(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	all := ""
	for _, p := range b.Panels {
		for _, tg := range p.Targets {
			all += tg.Expr + "\n"
		}
	}
	// These are the exact federated metric names probed live (2026-08-07). A typo
	// would render an empty panel with no query error, so pin them.
	mustContain := []string{
		"amazonaws_com_ECS_ContainerInsights_RunningTaskCount",
		"amazonaws_com_ECS_ContainerInsights_DesiredTaskCount",
		"amazonaws_com_AWS_RDS_DBLoad",
		"amazonaws_com_AWS_RDS_CPUUtilization_percent",
		"amazonaws_com_AWS_RDS_DatabaseConnections",
		"amazonaws_com_AWS_RDS_BufferCacheHitRatio_percent",
		"amazonaws_com_AWS_RDS_DiskQueueDepth",
		"fastapi_sponsor_api_http_requests_total",
		"fastapi_sponsor_api_http_request_duration_highr_seconds_bucket",
		"http_server_requests_seconds_count",
		`up{job="perf-federation"`,
	}
	for _, m := range mustContain {
		if !strings.Contains(all, m) {
			t.Errorf("perf-overview must query the federated family %q (probed live); absent from all targets", m)
		}
	}
}

// TestPerfOverviewDashboard_LatencyUsesHighResHistogram guards that the p50/p95/p99
// percentiles come from the high-resolution histogram (22 buckets, 10ms→60s), not
// the coarse duration_seconds histogram (only le 0.1/0.5/1.0/+Inf) which cannot
// resolve a p95 under load.
func TestPerfOverviewDashboard_LatencyUsesHighResHistogram(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	var found bool
	for _, p := range b.Panels {
		for _, tg := range p.Targets {
			if strings.Contains(tg.Expr, "histogram_quantile") &&
				strings.Contains(tg.Expr, "duration_highr_seconds_bucket") {
				found = true
			}
			// The coarse (non-highr) bucket must never feed a percentile — it has
			// only 4 buckets and silently pins p95/p99 to the +Inf boundary.
			if strings.Contains(tg.Expr, "histogram_quantile") &&
				strings.Contains(tg.Expr, "duration_seconds_bucket") &&
				!strings.Contains(tg.Expr, "highr") {
				t.Errorf("panel id=%d %q derives a percentile from the coarse "+
					"duration_seconds_bucket (4 buckets); use the highr histogram: %q", p.ID, p.Title, tg.Expr)
			}
		}
	}
	if !found {
		t.Error("perf-overview must derive latency percentiles from the high-resolution histogram")
	}
}

// TestPerfOverviewDashboard_HeadlineLoadTilesTolerateKeycloakGaps guards the
// keycloak-intermittency fix on the two headline load tiles — and guards that the
// fix is not the `or vector(0)` mask it started as (ka-0000006531).
//
// sponsor-api emits continuously under k6 load, but keycloak's http_server_*
// series appear only on auth traffic; a bare sum(fastapi)+sum(http_server)
// collapses to an empty vector whenever the keycloak half is absent, blanking a
// tile that should read a live rate. The original fix folded the keycloak term in
// with `or vector(0)` — which also painted a confident 0 when NEITHER family was
// reporting, i.e. exactly when the tile should have said "No data".
//
// The contract now: sum both families through one `{__name__=~"..."}` union
// selector (partial absence still reads live), and on the error tile earn each
// error leg's zero off that union request rate rather than off `vector(0)` (a 0%
// only renders while some traffic is demonstrably being scraped). Total absence
// then yields an empty result and the honest-empty noValue fires.
func TestPerfOverviewDashboard_HeadlineLoadTilesTolerateKeycloakGaps(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	// The union selector both tiles must route the two app families through.
	const union = `{__name__=~"fastapi_sponsor_api_http_requests_total|http_server_requests_seconds_count"`
	var checked int
	for _, p := range b.Panels {
		if p.Type != "stat" {
			continue
		}
		for _, tg := range p.Targets {
			e := tg.Expr
			combinesBoth := strings.Contains(e, "http_server_requests_seconds_count") &&
				strings.Contains(e, "fastapi_sponsor_api_http_requests_total")
			if !combinesBoth {
				continue
			}
			checked++
			// 1. No fabricated zero. `or vector(0)` makes noValue unreachable, so a
			//    dead federation and a genuine idle harness render identically
			//    (ka-0000007164 / gd-bUyK5hRdNU / ka-0000006531).
			if strings.Contains(e, "vector(0)") {
				t.Errorf("headline load tile id=%d %q coalesces with `vector(0)`; that paints a "+
					"confident 0 over a blind federation and makes the honest-empty noValue "+
					"unreachable: %q", p.ID, p.Title, e)
			}
			// 2. Partial absence must still read live — that is what the union
			//    selector buys, and it is the gap `or vector(0)` was covering.
			if !strings.Contains(e, union) {
				t.Errorf("headline load tile id=%d %q must sum both app families through the "+
					"single %s...} union selector so an idle keycloak cannot blank the tile: %q",
					p.ID, p.Title, union, e)
			}
			// 3. Any zero the error tile falls back to must be EARNED off observed
			//    traffic (`or (<union rate> * 0)`), never fabricated.
			if strings.Contains(e, " or ") && !strings.Contains(e, "* 0)") {
				t.Errorf("headline load tile id=%d %q falls back with `or` but not onto an earned "+
					"zero (`or (<union rate> * 0)`): %q", p.ID, p.Title, e)
			}
		}
	}
	if checked != 2 {
		t.Errorf("expected the 2 headline load tiles to combine both app families; checked %d", checked)
	}
}

func TestPerfOverviewDashboard_TemplateVarsScopedToPerf(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	want := map[string]bool{"ecs_cluster": false, "db": false, "load_cluster": false}
	for _, v := range b.Templating.List {
		if _, ok := want[v.Name]; ok {
			want[v.Name] = true
			if !strings.Contains(v.Query.Query, `source="perf"`) {
				t.Errorf("template var %q must scope its label_values query to source=\"perf\"; got %q", v.Name, v.Query.Query)
			}
		}
	}
	for name, present := range want {
		if !present {
			t.Errorf("perf-overview must define the %q template variable", name)
		}
	}
}

// TestPerfOverviewDashboard_RawGaugesPinASingleQuantile guards the CloudWatch
// aggregation-drift invariant surfaced by the T3.2 ground-truth cross-check
// (ka-0000005823, docs/grafana-grade/perf-overview-onboarding/): the federated
// CloudWatch gauge families are exported per (cluster, service) with BOTH a
// quantile="0" (period-min) and a quantile="1" (period-max) series. A `sum(...)`
// or `max(...)` over such a family WITHOUT pinning a single quantile silently
// folds min+max together and over-counts — the classic aggregation drift. The
// board avoids this by selecting exactly one quantile on every raw-gauge target.
//
// The rate-averaged families (`_sum` / `_count` divided pairs) carry NO quantile
// label at all, so they must NOT reference quantile — asserting a pin on them
// would be wrong. This test therefore checks the direct-gauge targets only: any
// target that selects a bare CloudWatch family (no `rate(`) must pin `quantile=`.
// TestPerfOverviewDashboard_RDSSaturationPanelsPresent guards the two RDS
// saturation panels added to raise the board's Depth dimension (GRADE audit
// gd-T1OenPjA8D): buffer cache hit ratio (period-min, the working-set-spills-to-
// disk leading indicator) and disk queue depth (period-max, storage saturation).
// Both are grounded in federated CloudWatch families with verified live series
// and follow the same $db-scoped, single-quantile, honest-empty conventions as
// the sibling RDS panels — so a refactor that drops them regresses Depth back to
// the pre-audit board.
func TestPerfOverviewDashboard_RDSSaturationPanelsPresent(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	want := map[string]string{
		"BufferCacheHitRatio": "amazonaws_com_AWS_RDS_BufferCacheHitRatio_percent",
		"DiskQueueDepth":      "amazonaws_com_AWS_RDS_DiskQueueDepth",
	}
	found := map[string]bool{}
	for _, p := range b.Panels {
		for _, tg := range p.Targets {
			for k, fam := range want {
				if strings.Contains(tg.Expr, fam) {
					found[k] = true
					// Must scope to the $db template var like the other RDS panels
					// so instance selection applies uniformly.
					if !strings.Contains(tg.Expr, "$db") {
						t.Errorf("RDS saturation panel %q (id=%d) must scope to the $db "+
							"template var like its siblings: %q", k, p.ID, tg.Expr)
					}
				}
			}
		}
	}
	for k := range want {
		if !found[k] {
			t.Errorf("perf-overview must include the RDS %s saturation panel (Depth); absent", k)
		}
	}
}

func TestPerfOverviewDashboard_RawGaugesPinASingleQuantile(t *testing.T) {
	t.Parallel()
	b := loadPerfOverviewBoard(t)
	for _, p := range b.Panels {
		if p.Type == "row" || p.Type == "text" {
			continue
		}
		for _, tg := range p.Targets {
			e := tg.Expr
			// Only the federated CloudWatch families expose the quantile label.
			if !strings.Contains(e, "amazonaws_com_") {
				continue
			}
			// The rate-averaged `_sum`/`_count` families carry no quantile label;
			// a `rate(` in the target marks that shape — skip it.
			if strings.Contains(e, "rate(") {
				if strings.Contains(e, "quantile=") {
					t.Errorf("panel id=%d %q rate-averages a CloudWatch family but pins "+
						"quantile= — those `_sum`/`_count` series expose no quantile label, "+
						"so the selector matches nothing: %q", p.ID, p.Title, e)
				}
				continue
			}
			// Direct gauge selection MUST pin a single quantile to avoid folding
			// the min (quantile="0") and max (quantile="1") series together.
			if !strings.Contains(e, `quantile="0"`) && !strings.Contains(e, `quantile="1"`) {
				t.Errorf("panel id=%d %q selects a raw CloudWatch gauge without pinning "+
					"quantile=\"0\" or \"1\"; sum/max over both folds period-min+max and "+
					"over-counts (aggregation drift): %q", p.ID, p.Title, e)
			}
		}
	}
}
