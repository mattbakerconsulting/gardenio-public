package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// charts_catalog_http.go — the E5 chart-catalog HTTP surface (ka-0000005217).
//
//	GET /v1/charts             → ChartCatalogList   (registered chart defs + param schemas)
//	GET /v1/charts/{chart_id}  → ChartRender        ({data, spec, meta})
//
// The handlers translate universal params (since/until/step/window/tz/theme)
// plus chart-specific params into a single PromQL range query (via the
// ChartDef.QueryTemplate built in charts_catalog.go), call VMClient.QueryRange,
// reshape the matrix into long-format `data` rows, inline that data into a
// Vega-Lite v5 `spec`, and stamp `meta` (etag + timings). Per decision D1
// (ka-0000001695) the hybrid contract ships both: web renders `spec`, iOS reads
// `data` guided by `ios_hint`. PNG/SVG fallback lands in E5.4.

// chartDefaultWindow is the default look-back when ?since is absent.
const chartDefaultWindow = 24 * time.Hour

// chartMaxPoints bounds the number of steps in a range query so a tiny step
// over a huge window can't ask vmsingle for millions of points. When the
// requested (or default) step would exceed this, the handler widens the step.
const chartMaxPoints = 1500

// ChartRender is the {data, spec, meta} response for one rendered chart.
type ChartRender struct {
	ChartID  string           `json:"chart_id"`
	Title    string           `json:"title"`
	Subtitle string           `json:"subtitle,omitempty"`
	Params   map[string]any   `json:"params"`
	Data     []map[string]any `json:"data"`
	Spec     map[string]any   `json:"spec"`
	IOSHint  IOSHint          `json:"ios_hint"`
	Meta     ChartMeta        `json:"meta"`
}

// ChartMeta carries cache + provenance metadata for a render.
type ChartMeta struct {
	VMQueryCount  int    `json:"vm_query_count"`
	VMQueryTimeMS int64  `json:"vm_query_time_ms"`
	DataETag      string `json:"data_etag"`
	ComputedAt    string `json:"computed_at"`
	PointCount    int    `json:"point_count"`
	SeriesCount   int    `json:"series_count"`
	Empty         bool   `json:"empty"`
}

// ChartCatalogEntry is one row of the catalog listing — enough for a client to
// build a chart picker without rendering anything.
type ChartCatalogEntry struct {
	ChartID       string               `json:"chart_id"`
	Title         string               `json:"title"`
	Category      string               `json:"category"`
	Surfaces      []string             `json:"supported_surfaces"`
	ParamsSchema  map[string]ParamSpec `json:"params_schema"`
	DefaultParams map[string]any       `json:"default_params,omitempty"`
}

// ChartCatalogList is the GET /v1/charts response.
type ChartCatalogList struct {
	Charts []ChartCatalogEntry `json:"charts"`
}

// handleChartCatalogList handles GET /v1/charts: the registered chart
// definitions and their param schemas. Cacheable; independent of VM state, so
// it is served even when the VM backend is unconfigured (a client can still
// learn what charts exist).
func (s *BeadsServer) handleChartCatalogList(w http.ResponseWriter, r *http.Request) {
	cat := chartCatalog()
	entries := make([]ChartCatalogEntry, 0, len(cat))
	for _, def := range cat {
		entries = append(entries, ChartCatalogEntry{
			ChartID:       def.ID,
			Title:         def.Title,
			Category:      def.Category,
			Surfaces:      def.Surfaces,
			ParamsSchema:  def.Params,
			DefaultParams: def.DefaultParams,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ChartID < entries[j].ChartID })
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, ChartCatalogList{Charts: entries})
}

// handleChartRender handles GET /v1/charts/{chart_id}. It validates the chart
// id and params, builds and runs the range query, and returns the
// {data, spec, meta} contract. An If-None-Match matching the computed data
// etag short-circuits to 304.
func (s *BeadsServer) handleChartRender(w http.ResponseWriter, r *http.Request) {
	chartID := r.PathValue("chart_id")
	def, ok := chartCatalog()[chartID]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown chart_id %q", chartID))
		return
	}
	if s.vmClient == nil {
		writeError(w, http.StatusServiceUnavailable, "charts backend not configured (no VictoriaMetrics endpoint)")
		return
	}

	req, err := parseChartRequest(def, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	render, err := s.computeChartRender(r.Context(), def, req, time.Now().UTC())
	if err != nil {
		writeServerError(r.Context(), w, "handleChartRender", err, "chart query failed")
		return
	}

	// ETag / If-None-Match short-circuit (Layer-2 validator, R5 §5.2).
	w.Header().Set("ETag", render.Meta.DataETag)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, stale-while-revalidate=30", int(req.Step.Seconds())))
	if match := r.Header.Get("If-None-Match"); match != "" && match == render.Meta.DataETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, render)
}

// chartRequest is the validated, resolved request for one render: the time
// window, the step, the rolling window, and the per-chart params (raw multi
// values for the selector + the resolved scalar params echoed in the response).
type chartRequest struct {
	Since    time.Time
	Until    time.Time
	Step     time.Duration
	Window   string // PromQL duration for rolling-window charts
	Rollup   string // resolved roll-up dimension for rollup charts (agent|role|project|model)
	Selector string // PromQL label-matcher body (no braces)
	Echo     map[string]any
	rawVals  map[string][]string
}

// parseChartRequest validates the universal + per-chart params against def and
// returns a resolved chartRequest. Unknown params are rejected so a typo
// surfaces rather than silently doing nothing; enum/int params are bounds- and
// membership-checked.
func parseChartRequest(def ChartDef, r *http.Request) (*chartRequest, error) {
	q := r.URL.Query()
	now := time.Now().UTC()

	until := now
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("invalid until: %v", err)
		}
		until = t.UTC()
	}
	since := until.Add(-chartDefaultWindow)
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("invalid since: %v", err)
		}
		since = t.UTC()
	}
	if !since.Before(until) {
		return nil, fmt.Errorf("since must be before until")
	}

	step, err := resolveStep(q.Get("step"), since, until)
	if err != nil {
		return nil, err
	}

	// Universal params consumed here, not as label matchers.
	universal := map[string]bool{
		"since": true, "until": true, "step": true,
		"tz": true, "theme": true, "format": true, "window": true, "top_n": true,
	}

	echo := map[string]any{}
	rawVals := map[string][]string{}
	for name, vals := range q {
		if universal[name] {
			continue
		}
		spec, known := def.Params[name]
		if !known {
			return nil, fmt.Errorf("unknown param %q for chart %s", name, def.ID)
		}
		if spec.Label != "" {
			rawVals[name] = vals
			echo[name] = vals[0]
		}
	}

	// Rolling window (1d/7d) for charts that use it; validated against the enum
	// when the chart declares one.
	window := ""
	if def.UsesWindow {
		window = q.Get("window")
		if window == "" {
			if d, ok := def.DefaultParams["window"].(string); ok {
				window = d
			} else {
				window = "1d"
			}
		}
		if spec, ok := def.Params["window"]; ok && len(spec.Enum) > 0 && !strInSlice(spec.Enum, window) {
			return nil, fmt.Errorf("invalid window %q (allowed: %v)", window, spec.Enum)
		}
		echo["window"] = window
	}

	// top_n bounds check (echoed; topN trimming applied post-query).
	if v := q.Get("top_n"); v != "" {
		spec, ok := def.Params["top_n"]
		if !ok {
			return nil, fmt.Errorf("chart %s does not accept top_n", def.ID)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < spec.Min || n > spec.Max {
			return nil, fmt.Errorf("top_n must be an int in [%d,%d]", spec.Min, spec.Max)
		}
		echo["top_n"] = n
	}

	// Roll-up dimension (agent|role|project|model) for charts that group their
	// primary query by a caller-chosen label. It is consumed into the
	// `sum by({{.Rollup}})(…)` clause, not as a label matcher, so it is resolved
	// and enum-validated here rather than in buildSelector. Defaults to the
	// param's Default (or the chart's SeriesLabel); validated against the enum.
	rollup := ""
	if spec, ok := def.Params["rollup"]; ok {
		rollup = q.Get("rollup")
		if rollup == "" {
			if d, dok := spec.Default.(string); dok && d != "" {
				rollup = d
			} else {
				rollup = def.SeriesLabel
			}
		}
		if len(spec.Enum) > 0 && !strInSlice(spec.Enum, rollup) {
			return nil, fmt.Errorf("invalid rollup %q (allowed: %v)", rollup, spec.Enum)
		}
		echo["rollup"] = rollup
	}

	echo["since"] = since.Format(time.RFC3339)
	echo["until"] = until.Format(time.RFC3339)
	echo["step"] = promDuration(step)

	return &chartRequest{
		Since:    since,
		Until:    until,
		Step:     step,
		Window:   window,
		Rollup:   rollup,
		Selector: buildSelector(def, rawVals),
		Echo:     echo,
		rawVals:  rawVals,
	}, nil
}

// computeChartRender runs the range query and assembles the {data, spec, meta}
// payload. Separated from the handler so tests can drive it with a fixed now.
func (s *BeadsServer) computeChartRender(ctx context.Context, def ChartDef, req *chartRequest, computedAt time.Time) (*ChartRender, error) {
	// For a roll-up chart the primary query groups by the caller-chosen
	// dimension and the returned series are keyed by that label — so the data
	// reshaper, Vega-Lite spec, and iOS hint must read SeriesField/SeriesLabel =
	// the resolved rollup, not the chart's static default. Everything else (the
	// fixed-label charts) leaves req.Rollup empty and uses def unchanged.
	def = withRollupSeries(def, req.Rollup)

	start := time.Now()

	// The operator rollup is the one dimension that is NOT a label on the agent
	// metric: it lives on a separate metric keyed by an opaque operator_id and
	// requires a SERVER-SIDE operator_id -> name join (store.ResolveOperators)
	// that the templated PromQL path has no seam for. Route it to the dedicated
	// branch, which returns series already re-keyed on the resolved human name
	// under the "operator" label — then fall into the shared top-N / reshape /
	// spec path unchanged. The four label dims never reach this branch.
	var series []VMSeries
	if req.Rollup == operatorRollupValue {
		var err error
		series, err = s.computeOperatorSeries(ctx, req)
		if err != nil {
			return nil, err
		}
	} else {
		promql, err := renderTemplate(def.QueryTemplate, queryContext{
			Selector:    req.Selector,
			Step:        promDuration(req.Step),
			Window:      defaultStr(req.Window, "1d"),
			WindowHours: windowHours(defaultStr(req.Window, "1d")),
			Rollup:      req.Rollup,
		})
		if err != nil {
			return nil, fmt.Errorf("build promql for %s: %w", def.ID, err)
		}
		series, err = s.vmClient.QueryRange(ctx, promql, req.Since, req.Until, req.Step)
		if err != nil {
			return nil, err
		}
	}

	if def.SeriesLabel != "" {
		if def.RollupOther {
			series = topNSeriesWithOther(series, topNFromEcho(req.Echo))
		} else {
			series = topNSeries(series, topNFromEcho(req.Echo))
		}
	}

	data := reshapeLongFormat(def, series)

	// Multi-query extension (ka-0000006625): run each overlay's query and append
	// its long-format rows (keyed by the overlay's own ValueField) to the same
	// `data` array. The web renderer builds one ECharts series per value field;
	// rows missing a field are simply absent from that series. A failed overlay
	// query fails the whole render — a half-drawn chart that silently omits the
	// agent-count line would be more misleading than an error.
	queryCount := 1
	overlayHints := make([]IOSOverlay, 0, len(def.ExtraSeries))
	for _, ex := range def.ExtraSeries {
		exPromQL, err := renderTemplate(ex.QueryTemplate, queryContext{
			Selector:    buildSelectorFiltered(def, req.rawVals, ex.AllowedLabels),
			Step:        promDuration(req.Step),
			Window:      defaultStr(req.Window, "1d"),
			WindowHours: windowHours(defaultStr(req.Window, "1d")),
		})
		if err != nil {
			return nil, fmt.Errorf("build overlay promql for %s/%s: %w", def.ID, ex.Name, err)
		}
		exSeries, err := s.vmClient.QueryRange(ctx, exPromQL, req.Since, req.Until, req.Step)
		if err != nil {
			return nil, err
		}
		queryCount++
		data = append(data, reshapeOverlayRows(def.XField, ex.ValueField, exSeries)...)
		overlayHints = append(overlayHints, IOSOverlay{
			Name:       ex.Name,
			YField:     ex.ValueField,
			YFormat:    ex.YFormat,
			YTitle:     ex.YTitle,
			YAxisIndex: ex.YAxisIndex,
		})
	}
	// Keep `data` time-ordered after appending overlay rows so the etag stays
	// deterministic regardless of how many overlays ran.
	if len(def.ExtraSeries) > 0 {
		sort.SliceStable(data, func(i, j int) bool {
			ti, _ := data[i][def.XField].(string)
			tj, _ := data[j][def.XField].(string)
			return ti < tj
		})
	}
	queryMS := time.Since(start).Milliseconds()

	// The Vega-Lite spec (PNG/SVG fallback, iOS) renders the primary series
	// only; the secondary-axis overlay is a web/native-hint extension consumed
	// via ios_hint.overlays.
	spec := buildVegaLiteSpec(def, reshapeLongFormat(def, series))

	subtitle, _ := renderTemplate(def.Subtitle, queryContext{
		Step:   promDuration(req.Step),
		Window: defaultStr(req.Window, "1d"),
		TopN:   topNSubtitle(req.Echo),
		Rollup: req.Rollup,
	})

	render := &ChartRender{
		ChartID:  def.ID,
		Title:    def.Title,
		Subtitle: subtitle,
		Params:   req.Echo,
		Data:     data,
		Spec:     spec,
		IOSHint: IOSHint{
			Kind:           def.IOSKind,
			XField:         def.XField,
			YField:         def.ValueField,
			SeriesField:    def.SeriesField,
			YFormat:        def.YFormat,
			SupportsNative: true,
			YTitle:         overlayYTitle(def),
			PrimaryName:    overlayPrimaryName(def),
			Overlays:       overlayHintsOrNil(overlayHints),
		},
		Meta: ChartMeta{
			VMQueryCount:  queryCount,
			VMQueryTimeMS: queryMS,
			ComputedAt:    computedAt.Format(time.RFC3339),
			PointCount:    len(data),
			SeriesCount:   len(series),
			Empty:         len(data) == 0,
		},
	}
	render.Meta.DataETag = dataETag(def.ID, req.Echo, data)
	return render, nil
}

// reshapeLongFormat turns the VM matrix into long-format rows
// {<XField>: rfc3339, <SeriesField>: key, <ValueField>: n}. Rows are sorted by
// (time, series) so the output — and therefore the etag — is deterministic.
func reshapeLongFormat(def ChartDef, series []VMSeries) []map[string]any {
	rows := make([]map[string]any, 0)
	for _, ser := range series {
		key := ""
		if def.SeriesLabel != "" {
			key = ser.Metric[def.SeriesLabel]
		}
		for _, p := range ser.Points {
			row := map[string]any{
				def.XField:     p.T.UTC().Format(time.RFC3339),
				def.ValueField: p.Value,
			}
			if def.SeriesField != "" {
				row[def.SeriesField] = key
			}
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		ti, _ := rows[i][def.XField].(string)
		tj, _ := rows[j][def.XField].(string)
		if ti != tj {
			return ti < tj
		}
		if def.SeriesField == "" {
			return false
		}
		ki, _ := rows[i][def.SeriesField].(string)
		kj, _ := rows[j][def.SeriesField].(string)
		return ki < kj
	})
	return rows
}

// reshapeOverlayRows turns an overlay query's VM matrix into long-format rows
// {<xField>: rfc3339, <valueField>: n}. Overlay series are single aggregates
// (no series key), so each point becomes one row carrying only this overlay's
// value field — distinct from the primary's ValueField so the two measures
// share an x but never collide in a row.
func reshapeOverlayRows(xField, valueField string, series []VMSeries) []map[string]any {
	rows := make([]map[string]any, 0)
	for _, ser := range series {
		for _, p := range ser.Points {
			rows = append(rows, map[string]any{
				xField:     p.T.UTC().Format(time.RFC3339),
				valueField: p.Value,
			})
		}
	}
	return rows
}

// overlayYTitle / overlayPrimaryName / overlayHintsOrNil keep the IOSHint
// construction terse: they return zero values for single-series charts so the
// omitempty JSON tags drop the fields entirely, leaving the existing contract
// byte-identical for every chart without overlays.
func overlayYTitle(def ChartDef) string {
	if len(def.ExtraSeries) == 0 {
		return ""
	}
	return def.YTitle
}

func overlayPrimaryName(def ChartDef) string {
	if len(def.ExtraSeries) == 0 {
		return ""
	}
	if def.PrimarySeriesName != "" {
		return def.PrimarySeriesName
	}
	return def.Title
}

func overlayHintsOrNil(h []IOSOverlay) []IOSOverlay {
	if len(h) == 0 {
		return nil
	}
	return h
}

// buildVegaLiteSpec assembles a Vega-Lite v5 spec with the data inlined, so
// vega-embed needs zero extra fetches and vl-convert (E5.4) can render the same
// document server-side. The encoding mirrors ios_hint so web and iOS draw the
// same chart (parity bar, ka-0000001797).
func buildVegaLiteSpec(def ChartDef, data []map[string]any) map[string]any {
	yEnc := map[string]any{"field": def.ValueField, "type": "quantitative", "title": def.YTitle}
	// bar-stacked: each time bucket is one bar segmented by series, so stack
	// from zero. Vega-Lite stacks bar+color by default, but we set it
	// explicitly so the intent is documented and parity-checked with iOS/web.
	if def.IOSKind == "bar-stacked" {
		yEnc["stack"] = "zero"
	}
	encoding := map[string]any{
		"x": map[string]any{"field": def.XField, "type": "temporal", "title": "Time"},
		"y": yEnc,
	}
	if def.SeriesField != "" {
		encoding["color"] = map[string]any{"field": def.SeriesField, "type": "nominal"}
	}
	mark := map[string]any{"type": def.Mark}
	if def.Mark == "line" {
		mark["point"] = true
	}
	return map[string]any{
		"$schema":  "https://vega.github.io/schema/vega-lite/v5.json",
		"data":     map[string]any{"values": data},
		"mark":     mark,
		"encoding": encoding,
		"config":   map[string]any{"background": nil},
	}
}

// topNSeries keeps the n series with the largest total value, summarizing the
// remainder is out of scope for v1 (R5 notes an "others" row as future work);
// here we simply cap to bound response size. n<=0 means no cap.
func topNSeries(series []VMSeries, n int) []VMSeries {
	if n <= 0 || len(series) <= n {
		return series
	}
	type scored struct {
		s   VMSeries
		sum float64
	}
	scoredSeries := make([]scored, len(series))
	for i, s := range series {
		var total float64
		for _, p := range s.Points {
			total += p.Value
		}
		scoredSeries[i] = scored{s: s, sum: total}
	}
	sort.Slice(scoredSeries, func(i, j int) bool { return scoredSeries[i].sum > scoredSeries[j].sum })
	out := make([]VMSeries, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, scoredSeries[i].s)
	}
	return out
}

// otherSeriesLabel is the series-key used for the summarized long-tail series on
// charts with RollupOther. The same constant is referenced from tests and the
// web renderer styling, so it lives here as the single source of truth.
const otherSeriesLabel = "other"

// topNSeriesWithOther keeps the n highest-total series and folds every series
// past the cap into a single "other" series, summed per time bucket. Unlike
// topNSeries (which drops the tail to bound response size), this preserves the
// stacked total so a stacked-area chart still sums to the fleet figure. The
// series are sorted by total descending; the "other" series, when present, is
// appended last so it stacks at the top of the chart. The "other" series carries
// the canonical series-label key under EVERY label found on the contributing
// series so reshapeLongFormat reads otherSeriesLabel regardless of which label
// the chart keys on — in practice charts key on one label (e.g. "agent").
func topNSeriesWithOther(series []VMSeries, n int) []VMSeries {
	if n <= 0 || len(series) <= n {
		return series
	}
	type scored struct {
		s   VMSeries
		sum float64
	}
	scoredSeries := make([]scored, len(series))
	for i, s := range series {
		var total float64
		for _, p := range s.Points {
			total += p.Value
		}
		scoredSeries[i] = scored{s: s, sum: total}
	}
	sort.Slice(scoredSeries, func(i, j int) bool { return scoredSeries[i].sum > scoredSeries[j].sum })

	out := make([]VMSeries, 0, n+1)
	for i := 0; i < n; i++ {
		out = append(out, scoredSeries[i].s)
	}

	// Fold the tail into one series, summing values at matching timestamps. The
	// kept series share a step-aligned time grid (a single range query), so the
	// "other" grid is taken from the first tail series and accumulated by index.
	tail := scoredSeries[n:]
	if len(tail) == 0 {
		return out
	}
	byTS := make(map[int64]float64)
	order := make([]int64, 0)
	seen := make(map[int64]bool)
	for _, sc := range tail {
		for _, p := range sc.s.Points {
			ts := p.T.Unix()
			if !seen[ts] {
				seen[ts] = true
				order = append(order, ts)
			}
			byTS[ts] += p.Value
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	otherPoints := make([]VMPoint, 0, len(order))
	for _, ts := range order {
		otherPoints = append(otherPoints, VMPoint{T: time.Unix(ts, 0).UTC(), Value: byTS[ts]})
	}
	// Label the "other" series so reshapeLongFormat keys it correctly: replicate
	// otherSeriesLabel under every label key seen on the tail's series.
	otherMetric := map[string]string{}
	for _, sc := range tail {
		for k := range sc.s.Metric {
			otherMetric[k] = otherSeriesLabel
		}
	}
	out = append(out, VMSeries{Metric: otherMetric, Points: otherPoints})
	return out
}

// topNSubtitle renders the echoed top_n value as a string for a chart's
// human-facing Subtitle ("top {{.TopN}} agents"). Empty when top_n is absent.
func topNSubtitle(echo map[string]any) string {
	if n := topNFromEcho(echo); n > 0 {
		return strconv.Itoa(n)
	}
	return ""
}

// dataETag is a strong validator over the rendered data + identifying params.
// W/ weak prefix is intentionally omitted — the body is byte-stable for a given
// (chart, params, data) so a strong validator is correct and enables 304s.
func dataETag(chartID string, params map[string]any, data []map[string]any) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", chartID)
	// Params in sorted key order for stability.
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%v\n", k, params[k])
	}
	enc, _ := json.Marshal(data)
	h.Write(enc)
	return fmt.Sprintf(`"sha256:%x"`, h.Sum(nil)[:16])
}

// resolveStep picks the range-query step. An explicit ?step= (Go duration or
// bare seconds) wins; otherwise it auto-selects ~ window/120 clamped to
// [15s, 1h], then widens further if needed to stay under chartMaxPoints.
func resolveStep(raw string, since, until time.Time) (time.Duration, error) {
	window := until.Sub(since)
	var step time.Duration
	if raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			// Allow a bare integer = seconds.
			if n, e2 := strconv.Atoi(raw); e2 == nil {
				d = time.Duration(n) * time.Second
			} else {
				return 0, fmt.Errorf("invalid step %q", raw)
			}
		}
		if d <= 0 {
			return 0, fmt.Errorf("step must be positive")
		}
		step = d
	} else {
		step = window / 120
		if step < 15*time.Second {
			step = 15 * time.Second
		}
		if step > time.Hour {
			step = time.Hour
		}
	}
	// Bound total points.
	if window/step > chartMaxPoints {
		step = window / chartMaxPoints
	}
	return step, nil
}

// promDuration renders a Go duration as a PromQL duration string. PromQL
// accepts s/m/h/d; we emit seconds for sub-minute, minutes for sub-hour, else
// hours, which covers every step the auto-selector produces.
func promDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0 && d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

func strInSlice(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func topNFromEcho(echo map[string]any) int {
	if n, ok := echo["top_n"].(int); ok {
		return n
	}
	return 0
}

// withRollupSeries returns def with SeriesField/SeriesLabel overridden to the
// resolved roll-up dimension, so the reshaper reads the right label off each
// returned series (a `sum by(role)(…)` query returns series keyed by `role`,
// not the chart's static-default `agent`). rollup is empty for every
// non-rollup chart, in which case def is returned unchanged. The override is a
// scalar-field copy: def.Params (a map) is shared, but only SeriesField/
// SeriesLabel are mutated, so this never aliases mutable state.
func withRollupSeries(def ChartDef, rollup string) ChartDef {
	if rollup == "" || def.SeriesField == "" {
		return def
	}
	def.SeriesField = rollup
	def.SeriesLabel = rollup
	return def
}
