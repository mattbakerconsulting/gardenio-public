package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// charts_catalog_e2e_test.go — end-to-end tests for the E5 chart catalog
// (gd-m2ppngKgpP). A synthetic VictoriaMetrics range endpoint serves
// deterministic matrix data; the tests drive all five named charts through the
// catalog and assert the {data, spec, meta} contract, plus the empty-series
// graceful path for charts whose source metrics (E3/E4) are not yet emitting.

// vmRangeStub is an httptest VM that returns canned /api/v1/query_range matrix
// responses keyed by a substring match on the PromQL query. The first matching
// rule wins; an unmatched query returns an empty matrix (the honest-empty
// path).
type vmRangeStub struct {
	rules []vmRule
}

type vmRule struct {
	contains string
	body     string
}

func newVMRangeStub(t *testing.T, rules ...vmRule) *VMClient {
	t.Helper()
	stub := &vmRangeStub{rules: rules}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		q := r.FormValue("query")
		w.Header().Set("Content-Type", "application/json")
		for _, rule := range stub.rules {
			if rule.contains == "" || strings.Contains(q, rule.contains) {
				_, _ = io.WriteString(w, rule.body)
				return
			}
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	t.Cleanup(srv.Close)
	return NewVMClient(srv.URL, "")
}

// matrixSeries builds a one-series matrix body with the given label and points
// at start, start+step, start+2*step.
func matrixSeries(label, value string, start int64, step int64, vals ...string) string {
	var pts []string
	for i, v := range vals {
		pts = append(pts, fmt.Sprintf("[%d,%q]", start+int64(i)*step, v))
	}
	metric := "{}"
	if label != "" {
		metric = fmt.Sprintf("{%q:%q}", label, value)
	}
	return fmt.Sprintf(`{"metric":%s,"values":[%s]}`, metric, strings.Join(pts, ","))
}

func TestChartRender_SpendByAgent_FullContract(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[%s,%s]}}`,
		matrixSeries("agent", "agent-a", start, step, "0.5", "0.7"),
		matrixSeries("agent", "agent-b", start, step, "0.1", "0.2"),
	)
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "gardenio_agent_spend_logs_usd", body: body}))

	def := chartCatalog()["spend-by-agent"]
	req := &chartRequest{
		Since:    time.Unix(start, 0).UTC(),
		Until:    time.Unix(start+2*step, 0).UTC(),
		Step:     time.Hour,
		Selector: "",
		Echo:     map[string]any{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	// data: long format, one row per (t, agent), sorted by (t, series).
	if len(render.Data) != 4 {
		t.Fatalf("want 4 data rows, got %d: %+v", len(render.Data), render.Data)
	}
	first := render.Data[0]
	if first["agent"] != "agent-a" {
		t.Errorf("row0 agent = %v, want agent-a", first["agent"])
	}
	if first["spend_usd"] != 0.5 {
		t.Errorf("row0 spend_usd = %v, want 0.5", first["spend_usd"])
	}
	if _, ok := first["t"].(string); !ok {
		t.Errorf("row0 t not a string: %v", first["t"])
	}

	// spec: valid Vega-Lite v5 with data inlined and a color encoding for the
	// multi-series chart.
	if render.Spec["$schema"] != "https://vega.github.io/schema/vega-lite/v5.json" {
		t.Errorf("spec schema = %v", render.Spec["$schema"])
	}
	specData, _ := render.Spec["data"].(map[string]any)
	if specData == nil || specData["values"] == nil {
		t.Errorf("spec data not inlined: %v", render.Spec["data"])
	}
	enc, _ := render.Spec["encoding"].(map[string]any)
	if enc["color"] == nil {
		t.Errorf("multi-series spec missing color encoding")
	}

	// ios_hint + meta.
	if render.IOSHint.Kind != "bar-stacked" || !render.IOSHint.SupportsNative {
		t.Errorf("ios_hint = %+v", render.IOSHint)
	}
	if render.Meta.DataETag == "" || render.Meta.SeriesCount != 2 || render.Meta.Empty {
		t.Errorf("meta = %+v", render.Meta)
	}

	// The whole render must round-trip through JSON (the wire contract).
	if _, err := json.Marshal(render); err != nil {
		t.Errorf("render not JSON-serializable: %v", err)
	}
}

// TestChartRender_CostPerBead_RollupAndStacking drives the chonky cost-per-bead
// chart (ka-0000003269) end to end: the group_left() fleet-denominator query
// returns spend series keyed by the requested roll-up dimension, and the render
// must (a) advertise the stacked-bar shape (IOSKind bar-stacked), (b) key the
// data/series on the *resolved* roll-up label (not the static default), and
// (c) thread the roll-up into the subtitle. The VM stub answers the spend
// numerator with two series keyed by `role`.
func TestChartRender_CostPerBead_RollupAndStacking(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	stub := newVMRangeStub(t,
		vmRule{contains: "gardenio_agent_spend_logs_usd", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s,%s]}}`,
			matrixSeries("role", "gardener", start, step, "2.0", "3.0"),
			matrixSeries("role", "crew", start, step, "1.0", "1.5"))},
	)
	s := &BeadsServer{}
	s.SetVMClient(stub)

	def := chartCatalog()["agent-cost-per-bead"]
	// IOSKind flipped from the old per-agent line-multi to the chonky bar-stacked.
	if def.IOSKind != "bar-stacked" {
		t.Fatalf("catalog IOSKind = %q, want bar-stacked", def.IOSKind)
	}

	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Rollup:  "role",
		Echo:    map[string]any{"rollup": "role", "top_n": 10},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	// The render advertises stacked bars, and the series/value fields are keyed
	// on the resolved roll-up (role), so a role=gardener/crew split reshapes
	// correctly even though the catalog's static SeriesField is also role.
	if render.IOSHint.Kind != "bar-stacked" || !render.IOSHint.SupportsNative {
		t.Errorf("ios_hint = %+v, want bar-stacked/native", render.IOSHint)
	}
	if render.IOSHint.SeriesField != "role" {
		t.Errorf("ios_hint series_field = %q, want role", render.IOSHint.SeriesField)
	}
	// Two role series × two points = four rows, each carrying the role key.
	if len(render.Data) != 4 {
		t.Fatalf("want 4 data rows, got %d: %+v", len(render.Data), render.Data)
	}
	roles := map[string]bool{}
	for _, row := range render.Data {
		if _, ok := row["usd_per_bead"]; !ok {
			t.Errorf("row missing usd_per_bead: %+v", row)
		}
		if r, ok := row["role"].(string); ok {
			roles[r] = true
		}
	}
	if !roles["gardener"] || !roles["crew"] {
		t.Errorf("expected gardener+crew role series, got %v", roles)
	}
	// Subtitle threads the roll-up + top_n.
	if want := "USD per closed bead — top 10 by role"; render.Subtitle != want {
		t.Errorf("subtitle = %q, want %q", render.Subtitle, want)
	}
}

func TestChartRender_TotalCost_RollupAndStacking(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	// The by-role arm of realBillTagFlowOr reads the lifetime tag gauge; feed it a
	// role split so the stack reshapes on the resolved roll-up dimension.
	stub := newVMRangeStub(t,
		vmRule{contains: "gardenio_tag_cost_usd", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s,%s]}}`,
			matrixSeries("role", "gardener", start, step, "2.0", "3.0"),
			matrixSeries("role", "crew", start, step, "1.0", "1.5"))},
	)
	s := &BeadsServer{}
	s.SetVMClient(stub)

	def := chartCatalog()["total-cost"]
	// Chonky stacked bars, no closed-bead denominator.
	if def.IOSKind != "bar-stacked" {
		t.Fatalf("catalog IOSKind = %q, want bar-stacked", def.IOSKind)
	}
	if def.ValueField != "spend_usd" {
		t.Fatalf("ValueField = %q, want spend_usd (total cost, not cost-per-bead)", def.ValueField)
	}
	// This is total cost, NOT cost-per-bead: the query must NOT carry the
	// closed-bead denominator the sibling agent-cost-per-bead chart divides by.
	if strings.Contains(def.QueryTemplate, `event="closed"`) {
		t.Errorf("total-cost query must not carry a closed-bead denominator: %s", def.QueryTemplate)
	}

	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Rollup:  "role",
		Echo:    map[string]any{"rollup": "role", "top_n": 10},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	if render.IOSHint.Kind != "bar-stacked" || !render.IOSHint.SupportsNative {
		t.Errorf("ios_hint = %+v, want bar-stacked/native", render.IOSHint)
	}
	if render.IOSHint.SeriesField != "role" {
		t.Errorf("ios_hint series_field = %q, want role", render.IOSHint.SeriesField)
	}
	// Two role series × two points = four rows, each carrying the role key + value.
	if len(render.Data) != 4 {
		t.Fatalf("want 4 data rows, got %d: %+v", len(render.Data), render.Data)
	}
	roles := map[string]bool{}
	for _, row := range render.Data {
		if _, ok := row["spend_usd"]; !ok {
			t.Errorf("row missing spend_usd: %+v", row)
		}
		if r, ok := row["role"].(string); ok {
			roles[r] = true
		}
	}
	if !roles["gardener"] || !roles["crew"] {
		t.Errorf("expected gardener+crew role series, got %v", roles)
	}
	if want := "real-bill USD — top 10 by role"; render.Subtitle != want {
		t.Errorf("subtitle = %q, want %q", render.Subtitle, want)
	}
}

// TestChartRender_TotalCost_OperatorRollup exercises the operator branch of the
// total-cost rollup (gd-dtmUm70beX): rollup=operator routes to the operator
// metric, groups by the opaque operator_id, and resolves each id -> human name
// SERVER-SIDE via store.ResolveOperators. It asserts the full approach-A
// privacy + resolve contract: the PromQL queries operator_id only (no name
// label), resolved names appear in the render payload, opaque ids never surface,
// operator_id=0 renders as "unattributed", and a right-to-be-forgotten operator
// is redacted (its display never re-appears).
func TestChartRender_TotalCost_OperatorRollup(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)

	// Capture the PromQL the operator branch actually evaluates so we can assert
	// it groups on operator_id only and never references a PII label.
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		gotQuery = r.FormValue("query")
		w.Header().Set("Content-Type", "application/json")
		// operator_id-keyed series: 7 (Alice), 9 (forgotten), 0 (unattributed).
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s,%s,%s]}}`,
			matrixSeries("operator_id", "7", start, step, "2.0", "3.0"),
			matrixSeries("operator_id", "9", start, step, "1.0", "1.5"),
			matrixSeries("operator_id", "0", start, step, "0.5", "0.25")))
	}))
	t.Cleanup(srv.Close)

	m := newMockStore()
	seedOperator(m, 7, "oidc", "alice@acme.example", "Alice", "alice@acme.example", "alice", false)
	// Operator 9 is right-to-be-forgotten: the resolver redacts display; the
	// chart must NEVER render "Ghost" or the email — only the opaque fallback.
	seedOperator(m, 9, "oidc", "ghost@acme.example", "Ghost", "ghost@acme.example", "ghost", true)

	s := NewBeadsServer(m, nil)
	s.SetVMClient(NewVMClient(srv.URL, ""))

	def := chartCatalog()["total-cost"]
	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Rollup:  "operator",
		Echo:    map[string]any{"rollup": "operator", "top_n": 10},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	// 1. The query groups on the opaque operator_id and carries no PII label.
	if !strings.Contains(gotQuery, "by (operator_id)") {
		t.Errorf("operator query must group by operator_id: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "gardenio_operator_cost_usd_total") {
		t.Errorf("operator branch must query the operator metric: %q", gotQuery)
	}
	for _, bad := range []string{"email", "display", "slug", "Alice", "Ghost", "alice@", "ghost@"} {
		if strings.Contains(gotQuery, bad) {
			t.Errorf("operator query leaked %q into PromQL: %q", bad, gotQuery)
		}
	}

	// 2. The reshaped series key on the resolved-name "operator" label.
	if render.IOSHint.SeriesField != "operator" {
		t.Errorf("ios_hint series_field = %q, want operator", render.IOSHint.SeriesField)
	}
	ops := map[string]bool{}
	for _, row := range render.Data {
		if _, ok := row["spend_usd"]; !ok {
			t.Errorf("row missing spend_usd: %+v", row)
		}
		if o, ok := row["operator"].(string); ok {
			ops[o] = true
		}
	}
	// Alice resolves to her display; operator_id=0 is the unattributed bucket;
	// the forgotten operator 9 falls back to the opaque "operator 9".
	if !ops["Alice"] {
		t.Errorf("expected resolved operator 'Alice', got series %v", ops)
	}
	if !ops[unattributedDisplay] {
		t.Errorf("expected unattributed bucket %q, got series %v", unattributedDisplay, ops)
	}
	if !ops["operator 9"] {
		t.Errorf("expected forgotten operator redacted to 'operator 9', got series %v", ops)
	}

	// 3. PRIVACY: no bare opaque id for a resolvable operator, and the forgotten
	//    operator's name/email must never surface anywhere in the payload.
	body, _ := json.Marshal(render)
	for _, leaked := range []string{"Ghost", "ghost@acme.example", "alice@acme.example", `"operator 7"`, `"operator":"7"`} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("render payload leaked %q: %s", leaked, body)
		}
	}
	// Subtitle reflects the operator rollup.
	if want := "real-bill USD — top 10 by operator"; render.Subtitle != want {
		t.Errorf("subtitle = %q, want %q", render.Subtitle, want)
	}
}

func TestChartRender_AllFiveCharts_RenderAgainstStub(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	// One stub that answers every chart's query family with a small series.
	stub := newVMRangeStub(t,
		vmRule{contains: "gardenio_agent_spend_logs_usd", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("agent", "agent-a", start, step, "1", "2"))},
		vmRule{contains: "gardenio_beads_events_total", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("type", "bead.created", start, step, "3", "4"))},
	)
	s := &BeadsServer{}
	s.SetVMClient(stub)

	for id := range chartCatalog() {
		def := chartCatalog()[id]
		req := &chartRequest{
			Since: time.Unix(start, 0).UTC(),
			Until: time.Unix(start+2*step, 0).UTC(),
			Step:  time.Hour,
			Echo:  map[string]any{},
		}
		if def.UsesWindow {
			req.Window = "1d"
		}
		render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
		if err != nil {
			t.Errorf("chart %s: computeChartRender: %v", id, err)
			continue
		}
		if render.ChartID != id {
			t.Errorf("chart %s: render id = %s", id, render.ChartID)
		}
		if render.Spec["$schema"] == nil {
			t.Errorf("chart %s: missing spec schema", id)
		}
		if render.Meta.DataETag == "" {
			t.Errorf("chart %s: missing data etag", id)
		}
		if _, err := json.Marshal(render); err != nil {
			t.Errorf("chart %s: not JSON-serializable: %v", id, err)
		}
	}
}

// TestCatalogSpend_ReconcilesRealBillNotDeadCounter is the acceptance-criteria
// magnitude regression for ka-0000001663 (sibling of the /costs P0 ka-0000002699).
// It reproduces the prod topology at the time of the bug — the at-close counter
// gardenio_agent_spend_usd_total DEAD (0 series, would render ~$0) while the
// per-agent REAL-BILL gauge gardenio_agent_spend_logs_usd holds the real per-agent
// bill — and proves every per-agent spend chart now renders the real bill and
// reconciles with the attributed total WITHIN TOLERANCE, instead of the ~1000x-low
// figure the dead counter produced.
//
// The VM stub answers each query FROM ITS SHAPE: any query on the dead counter
// returns an empty matrix (as prod did), and the real-bill gauge returns three
// agents summing to $2745.69. The attributed total over the same window is
// $2944.95 (a superset — it includes agents that aged out of the gauge's lookback),
// so the two reconcile within a structural gap, NOT by 1000x. If a future edit
// re-points any of these charts back at the dead counter (or switches to increase()
// on the Reset+Set gauge), the rendered total collapses to ~$0 and this fails.
func TestCatalogSpend_ReconcilesRealBillNotDeadCounter(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	// attributed = the real-bill superset; the gauge rows sum to $2745.69.
	const attributed = 2944.95
	const gaugeTotal = 2745.69
	// Three agents on the real-bill gauge, each a flat lifetime level across the
	// two points (a Reset+Set gauge holds an absolute figure, not a per-bucket
	// increment). The stub keys the gauge answer by its metric name; the by-agent
	// charts group by(agent), so the three agents come back as three series.
	gaugeBody := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[%s,%s,%s]}}`,
		matrixSeries("agent", "daily-bundle-editor-2rli", start, step, "1286.86", "1286.86"),
		matrixSeries("agent", "gardenio-3-more-rwx-s0a", start, step, "958.83", "958.83"),
		matrixSeries("agent", "monorepo-fsm-triage-kfiy", start, step, "500.00", "500.00"),
	)
	stub := newVMRangeStub(t,
		// The DEAD at-close counter: any query touching it returns an empty
		// matrix, exactly as prod did (0 live series).
		vmRule{contains: "gardenio_agent_spend_usd_total", body: `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
		// The real-bill gauge answers with the three-agent level.
		vmRule{contains: "gardenio_agent_spend_logs_usd", body: gaugeBody},
	)
	s := &BeadsServer{}
	s.SetVMClient(stub)

	// The by-agent level charts must all reconcile with the real bill. (The rate
	// charts read the same gauge; cost-per-bead is a ratio, exercised separately.)
	for _, id := range []string{"spend-by-agent", "agent-spend-over-time", "spend-by-model", "agent-spend-cumulative"} {
		def := chartCatalog()[id]
		req := &chartRequest{
			Since:   time.Unix(start, 0).UTC(),
			Until:   time.Unix(start+2*step, 0).UTC(),
			Step:    time.Hour,
			Echo:    map[string]any{"top_n": 50},
			rawVals: map[string][]string{},
		}
		render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
		if err != nil {
			t.Fatalf("chart %s: computeChartRender: %v", id, err)
		}
		// Sum the last-bucket value across every series — the operator-visible
		// magnitude. spend_usd is every re-pointed chart's value field.
		var lastTS string
		for _, row := range render.Data {
			if ts, _ := row["t"].(string); ts > lastTS {
				lastTS = ts
			}
		}
		var total float64
		for _, row := range render.Data {
			if ts, _ := row["t"].(string); ts != lastTS {
				continue
			}
			if v, ok := row["spend_usd"].(float64); ok {
				total += v
			}
		}
		// The core of the bug: the dead counter produced <1% of the bill. Pin the
		// real-bill magnitude — at least 10% of attributed, and no over-count.
		if total < 0.10*attributed {
			t.Errorf("chart %s: last-bucket total $%.2f is <10%% of the $%.2f attributed bill — the 1000x-low regression is back", id, total, attributed)
		}
		if total > 1.25*attributed {
			t.Errorf("chart %s: last-bucket total $%.2f exceeds 125%% of the $%.2f attributed bill — over-counting", id, total, attributed)
		}
		// Sanity: the exact sum of the three gauge rows (the fleet-scoped
		// agent-spend-cumulative sums all three into one series; the by-agent/model
		// charts keep them split but still total the same).
		if total < gaugeTotal-1 || total > gaugeTotal+1 {
			t.Errorf("chart %s: last-bucket total $%.2f, want ~$%.2f (sum of the gauge rows)", id, total, gaugeTotal)
		}
	}
}

func TestChartRender_EmptySeries_GracefulWhenMetricAbsent(t *testing.T) {
	// VM returns an empty matrix (E3/E4 metrics not yet emitting). The chart
	// must render as an honest empty time series, not an error.
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t)) // no rules → always empty matrix

	def := chartCatalog()["beads-events-rate"]
	req := &chartRequest{
		Since: time.Unix(1700000000, 0).UTC(),
		Until: time.Unix(1700003600, 0).UTC(),
		Step:  time.Hour,
		Echo:  map[string]any{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Now().UTC())
	if err != nil {
		t.Fatalf("empty series should not error: %v", err)
	}
	if len(render.Data) != 0 || !render.Meta.Empty {
		t.Errorf("want empty render, got %d rows, empty=%v", len(render.Data), render.Meta.Empty)
	}
	// Spec is still well-formed (an empty values array) so the client renders an
	// empty chart frame rather than crashing.
	specData, _ := render.Spec["data"].(map[string]any)
	if specData == nil {
		t.Errorf("empty render missing spec data block")
	}
}

func TestChartCatalogList_HTTP(t *testing.T) {
	s := &BeadsServer{}
	// Catalog listing is served even without a VM client.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/charts", nil)
	s.handleChartCatalogList(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var list ChartCatalogList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Charts) != 20 {
		t.Fatalf("want 20 charts in catalog, got %d", len(list.Charts))
	}
	// Sorted by id, schemas present. The perf-fastapi-* entries are the W2b
	// VM-metrics RED panels (ka-0000003074); the perf-span-* entries are the
	// span-derived RED panels built on the federated spanmetrics families
	// (ka-0000008091) — the three per-service RED panels plus the three
	// per-operation (span_name) topk drill-downs (top-operations /
	// operation-errors / slowest-operations) ported from the ACME
	// distributed-traces dashboard (W2b parity, ka-0000001551). spend-by-account
	// is the /costs account panel and spend-by-model is the /costs per-model
	// panel (ka-0000003821). agent-spend-cumulative is the former hourly-rate
	// chart, re-cast to a cumulative real-bill level (ka-0000001663).
	// cost-per-agent-hour is the /costs headline unit economic — USD per hour
	// per producing agent (ka-0000006587).
	want := []string{"agent-cost-per-bead", "agent-spend-cumulative", "agent-spend-over-time", "bead-status-flow", "beads-events-rate", "cost-per-agent-hour", "fleet-spend-rate", "perf-fastapi-error-ratio", "perf-fastapi-latency-quantiles", "perf-fastapi-request-rate", "perf-span-error-ratio", "perf-span-latency-quantiles", "perf-span-operation-errors", "perf-span-request-rate", "perf-span-slowest-operations", "perf-span-top-operations", "spend-by-account", "spend-by-agent", "spend-by-model", "total-cost"}
	for i, c := range list.Charts {
		if c.ChartID != want[i] {
			t.Errorf("chart[%d] = %s, want %s", i, c.ChartID, want[i])
		}
		if c.ParamsSchema == nil {
			t.Errorf("chart %s missing params schema", c.ChartID)
		}
	}
}

func TestChartRender_HTTP_UnknownChartAnd304(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "", body: fmt.Sprintf(
		`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
		matrixSeries("agent", "a", start, step, "1", "2"))}))

	// Unknown chart → 404.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/charts/does-not-exist", nil)
	r.SetPathValue("chart_id", "does-not-exist")
	s.handleChartRender(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown chart status = %d, want 404", rec.Code)
	}

	// Known chart → 200 with ETag; replay with If-None-Match → 304.
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/v1/charts/spend-by-agent?since=2023-11-14T22:13:20Z&until=2023-11-15T00:13:20Z&step=1h", nil)
	r.SetPathValue("chart_id", "spend-by-agent")
	s.handleChartRender(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("render status = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag header")
	}

	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/v1/charts/spend-by-agent?since=2023-11-14T22:13:20Z&until=2023-11-15T00:13:20Z&step=1h", nil)
	r2.SetPathValue("chart_id", "spend-by-agent")
	r2.Header.Set("If-None-Match", etag)
	s.handleChartRender(rec2, r2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match replay status = %d, want 304", rec2.Code)
	}
}

func TestParseChartRequest_RejectsBadParams(t *testing.T) {
	def := chartCatalog()["spend-by-agent"]

	cases := []struct {
		name string
		url  string
	}{
		{"unknown param", "/v1/charts/spend-by-agent?bogus=1"},
		{"top_n out of range", "/v1/charts/spend-by-agent?top_n=999"},
		{"bad since", "/v1/charts/spend-by-agent?since=not-a-time"},
		{"since after until", "/v1/charts/spend-by-agent?since=2023-11-15T00:00:00Z&until=2023-11-14T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if _, err := parseChartRequest(def, r); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}

	// Window enum is enforced for the rolling rate chart (fleet-spend-rate still
	// varies its query by ?window=; the former hourly-rate chart is now the
	// cumulative-level agent-spend-cumulative with no window param — ka-0000001663).
	rdef := chartCatalog()["fleet-spend-rate"]
	r := httptest.NewRequest(http.MethodGet, "/v1/charts/fleet-spend-rate?window=99d", nil)
	if _, err := parseChartRequest(rdef, r); err == nil {
		t.Error("expected error for invalid window enum")
	}

	// Roll-up enum is enforced for the cost-per-bead chart; a valid roll-up
	// resolves onto the request.
	cdef := chartCatalog()["agent-cost-per-bead"]
	bad := httptest.NewRequest(http.MethodGet, "/v1/charts/agent-cost-per-bead?rollup=bogus", nil)
	if _, err := parseChartRequest(cdef, bad); err == nil {
		t.Error("expected error for invalid rollup enum")
	}
	good := httptest.NewRequest(http.MethodGet, "/v1/charts/agent-cost-per-bead?rollup=model", nil)
	creq, err := parseChartRequest(cdef, good)
	if err != nil {
		t.Fatalf("valid rollup rejected: %v", err)
	}
	if creq.Rollup != "model" {
		t.Errorf("resolved rollup = %q, want model", creq.Rollup)
	}
	// Absent rollup defaults to the chart's DefaultParams value (role).
	defReq := httptest.NewRequest(http.MethodGet, "/v1/charts/agent-cost-per-bead", nil)
	dreq, err := parseChartRequest(cdef, defReq)
	if err != nil {
		t.Fatalf("default rollup request failed: %v", err)
	}
	if dreq.Rollup != "role" {
		t.Errorf("default rollup = %q, want role", dreq.Rollup)
	}
}

func TestBuildSelector_SanitizesAndAlternates(t *testing.T) {
	def := chartCatalog()["spend-by-agent"]
	// Multiple agents → regex alternation; project → exact match; injection
	// chars stripped.
	sel := buildSelector(def, map[string][]string{
		"agent":   {"agent-a", "agent-b"},
		"project": {`gardenio"} bad`},
	})
	if !strings.Contains(sel, `agent=~"agent-a|agent-b"`) {
		t.Errorf("missing agent alternation: %s", sel)
	}
	if strings.Contains(sel, `"}`) || strings.Contains(sel, " bad") {
		t.Errorf("injection not sanitized: %s", sel)
	}
	if !strings.Contains(sel, `project="gardeniobad"`) {
		t.Errorf("project not sanitized as expected: %s", sel)
	}
}

// TestChartRender_AgentSpendCumulative_AgentCountOverlay drives the multi-query
// extension (ka-0000006625): the agent-spend-cumulative chart (formerly
// hourly-rate-rolling-window, re-cast to a cumulative real-bill level —
// ka-0000001663) overlays the active-agent count on a secondary axis. The VM stub
// answers the spend query and the gardenio_agents_active query distinctly; the
// render must carry both series in `data` (keyed by spend_usd and agents
// respectively), two VM queries in meta, and an ios_hint.overlays entry bound to
// y_axis_index 1.
func TestChartRender_AgentSpendCumulative_AgentCountOverlay(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	stub := newVMRangeStub(t,
		vmRule{contains: "gardenio_agents_active", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("", "", start, step, "3", "5"))},
		vmRule{contains: "gardenio_agent_spend_logs_usd", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("", "", start, step, "0.25", "0.40"))},
	)
	s := &BeadsServer{}
	s.SetVMClient(stub)

	def := chartCatalog()["agent-spend-cumulative"]
	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Window:  "1d",
		Echo:    map[string]any{"window": "1d"},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	// Two VM queries ran (primary spend + agent-count overlay).
	if render.Meta.VMQueryCount != 2 {
		t.Errorf("VMQueryCount = %d, want 2", render.Meta.VMQueryCount)
	}

	// data carries both measures: spend rows (spend_usd) and overlay rows
	// (agents). Count each.
	var spendRows, agentRows int
	for _, row := range render.Data {
		if _, ok := row["spend_usd"]; ok {
			spendRows++
		}
		if _, ok := row["agents"]; ok {
			agentRows++
		}
	}
	if spendRows != 2 {
		t.Errorf("spend rows = %d, want 2", spendRows)
	}
	if agentRows != 2 {
		t.Errorf("agent rows = %d, want 2", agentRows)
	}

	// ios_hint.overlays describes the secondary-axis count series.
	if len(render.IOSHint.Overlays) != 1 {
		t.Fatalf("overlays = %d, want 1", len(render.IOSHint.Overlays))
	}
	ov := render.IOSHint.Overlays[0]
	if ov.YField != "agents" || ov.YAxisIndex != 1 || ov.YFormat != "count" {
		t.Errorf("overlay = %+v, want {YField:agents YAxisIndex:1 YFormat:count}", ov)
	}
	if render.IOSHint.PrimaryName != "spend" {
		t.Errorf("primary_name = %q, want spend", render.IOSHint.PrimaryName)
	}

	// data must be time-ordered after the overlay append so the etag is stable.
	var prev string
	for _, row := range render.Data {
		ts, _ := row["t"].(string)
		if ts < prev {
			t.Errorf("data not time-ordered: %s before %s", prev, ts)
		}
		prev = ts
	}

	if _, err := json.Marshal(render); err != nil {
		t.Errorf("render not JSON-serializable: %v", err)
	}
}

// TestChartRender_FleetSpendRate_1hCompanionAndAgents drives the fleet-spend-rate
// chart end to end (gd-Uvo8TZ8Kei, ka-0000003828): the primary {{.Window}} spend
// line, the fixed 1h spend companion (left axis), the cost-per-agent lines (24h +
// 1h, left axis), the active-agent overlay and the 1h/24h rolling agent-count
// averages (right axis, ka-0000006587). The VM stub answers the seven distinct
// queries; the render must carry all seven measures in `data` (usd_per_hour,
// usd_per_hour_1h, usd_per_agent, usd_per_agent_1h, agents, agents_1h,
// agents_24h), seven VM queries in meta, and six ios_hint.overlays — the three
// USD lines on axis 0, the three agent counts on axis 1.
func TestChartRender_FleetSpendRate_1hCompanionAndAgents(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	stub := newVMRangeStub(t,
		// The cost-per-agent ratios read BOTH gauges — match them first (before
		// the single-gauge rules) since first match wins. The 1h ratio carries the
		// fixed [1h:5m] numerator; the 24h ratio carries [1d:5m]. Both divide by an
		// avg_over_time(sum(...)) OUTER-subquery agent-count denominator
		// (ka-0000006587 — the former per-series inner form read 117x high).
		vmRule{contains: "[1h:5m])) / 1 / (avg_over_time(sum(max by (project, role, state)(gardenio_agents_active", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("", "", start, step, "0.15", "0.20"))},
		vmRule{contains: "/ (avg_over_time(sum(max by (project, role, state)(gardenio_agents_active", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("", "", start, step, "0.08", "0.10"))},
		// [1h] is the spend companion — match it before the generic account-cost
		// rule (which the [1d] primary also satisfies), since first match wins.
		vmRule{contains: "[1h:5m])) / 1", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("", "", start, step, "0.50", "0.90"))},
		vmRule{contains: "gardenio_account_cost_usd", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("", "", start, step, "0.25", "0.40"))},
		vmRule{contains: "gardenio_agents_active", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("", "", start, step, "3", "5"))},
	)
	s := &BeadsServer{}
	s.SetVMClient(stub)

	def := chartCatalog()["fleet-spend-rate"]
	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Window:  "1d",
		Echo:    map[string]any{"window": "1d"},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	// Seven VM queries ran (primary + 1h spend + cost-per-agent 24h +
	// cost-per-agent 1h + instantaneous agent count + 1h/24h rolling agent-count
	// averages).
	if render.Meta.VMQueryCount != 7 {
		t.Errorf("VMQueryCount = %d, want 7", render.Meta.VMQueryCount)
	}

	// data carries all seven measures, keyed by their own value fields.
	var primaryRows, oneHRows, perAgentRows, perAgent1hRows, agentRows, agents1hRows, agents24hRows int
	for _, row := range render.Data {
		if _, ok := row["usd_per_hour"]; ok {
			primaryRows++
		}
		if _, ok := row["usd_per_hour_1h"]; ok {
			oneHRows++
		}
		if _, ok := row["usd_per_agent"]; ok {
			perAgentRows++
		}
		if _, ok := row["usd_per_agent_1h"]; ok {
			perAgent1hRows++
		}
		if _, ok := row["agents"]; ok {
			agentRows++
		}
		if _, ok := row["agents_1h"]; ok {
			agents1hRows++
		}
		if _, ok := row["agents_24h"]; ok {
			agents24hRows++
		}
	}
	if primaryRows != 2 || oneHRows != 2 || perAgentRows != 2 || perAgent1hRows != 2 ||
		agentRows != 2 || agents1hRows != 2 || agents24hRows != 2 {
		t.Errorf("rows: primary=%d 1h=%d perAgent=%d perAgent1h=%d agents=%d agents1h=%d agents24h=%d, want all 2",
			primaryRows, oneHRows, perAgentRows, perAgent1hRows, agentRows, agents1hRows, agents24hRows)
	}

	// Six overlays: the 1h spend + both cost-per-agent lines on the primary axis
	// (0); the instantaneous count + both rolling averages on the secondary axis (1).
	if len(render.IOSHint.Overlays) != 6 {
		t.Fatalf("overlays = %d, want 6", len(render.IOSHint.Overlays))
	}
	byField := map[string]IOSOverlay{}
	for _, ov := range render.IOSHint.Overlays {
		byField[ov.YField] = ov
	}
	oneH, ok := byField["usd_per_hour_1h"]
	if !ok {
		t.Fatalf("missing usd_per_hour_1h overlay; have %+v", render.IOSHint.Overlays)
	}
	if oneH.YAxisIndex != 0 || oneH.YFormat != "rate_per_hour" {
		t.Errorf("1h overlay = %+v, want {YAxisIndex:0 YFormat:rate_per_hour}", oneH)
	}
	perAgent, ok := byField["usd_per_agent"]
	if !ok {
		t.Fatalf("missing usd_per_agent overlay; have %+v", render.IOSHint.Overlays)
	}
	if perAgent.YAxisIndex != 0 || perAgent.YFormat != "rate_per_hour" {
		t.Errorf("cost-per-agent overlay = %+v, want {YAxisIndex:0 YFormat:rate_per_hour}", perAgent)
	}
	perAgent1h, ok := byField["usd_per_agent_1h"]
	if !ok {
		t.Fatalf("missing usd_per_agent_1h overlay; have %+v", render.IOSHint.Overlays)
	}
	if perAgent1h.YAxisIndex != 0 || perAgent1h.YFormat != "rate_per_hour" {
		t.Errorf("cost-per-agent (1h) overlay = %+v, want {YAxisIndex:0 YFormat:rate_per_hour}", perAgent1h)
	}
	agents, ok := byField["agents"]
	if !ok {
		t.Fatalf("missing agents overlay; have %+v", render.IOSHint.Overlays)
	}
	if agents.YAxisIndex != 1 || agents.YFormat != "count" {
		t.Errorf("agents overlay = %+v, want {YAxisIndex:1 YFormat:count}", agents)
	}
	// The rolling agent-count averages (ka-0000006587) share the instantaneous
	// count's right-hand axis and count format — they are the same measure, just
	// smoothed, so a different axis or unit would misrepresent them.
	for _, field := range []string{"agents_1h", "agents_24h"} {
		ov, ok := byField[field]
		if !ok {
			t.Fatalf("missing %s overlay; have %+v", field, render.IOSHint.Overlays)
		}
		if ov.YAxisIndex != 1 || ov.YFormat != "count" {
			t.Errorf("%s overlay = %+v, want {YAxisIndex:1 YFormat:count}", field, ov)
		}
	}

	// data must stay time-ordered after the overlay appends so the etag is stable.
	var prev string
	for _, row := range render.Data {
		ts, _ := row["t"].(string)
		if ts < prev {
			t.Errorf("data not time-ordered: %s before %s", prev, ts)
		}
		prev = ts
	}

	if _, err := json.Marshal(render); err != nil {
		t.Errorf("render not JSON-serializable: %v", err)
	}
}

// TestExtraSeriesSelector_DropsAbsentLabel guards that an overlay's selector is
// filtered to the labels its metric actually carries: the active-agent gauge
// has no `agent` label, so an agent= request param must NOT leak into its
// matcher (it would select zero series), while project= still scopes both.
func TestExtraSeriesSelector_DropsAbsentLabel(t *testing.T) {
	def := chartCatalog()["agent-spend-cumulative"]
	if len(def.ExtraSeries) != 1 {
		t.Fatalf("expected one extra series, got %d", len(def.ExtraSeries))
	}
	raw := map[string][]string{
		"agent":   {"agent-a"},
		"project": {"gardenio"},
	}
	// Primary selector includes the agent filter.
	if got := buildSelector(def, raw); !strings.Contains(got, `agent="agent-a"`) {
		t.Errorf("primary selector should include agent: %s", got)
	}
	// Overlay selector drops agent (absent label) but keeps project.
	ov := buildSelectorFiltered(def, raw, def.ExtraSeries[0].AllowedLabels)
	if strings.Contains(ov, "agent=") {
		t.Errorf("overlay selector must not include agent label: %s", ov)
	}
	if !strings.Contains(ov, `project="gardenio"`) {
		t.Errorf("overlay selector should keep project: %s", ov)
	}
}
