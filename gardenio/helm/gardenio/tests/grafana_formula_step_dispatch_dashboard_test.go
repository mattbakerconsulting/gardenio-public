package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Formula step-dispatcher dashboard (task ka-0000004839). The dispatcher exists
// to prevent SILENT loss of a formula handoff, so it must itself be observable.
// This board is the operator's answer to the two headline questions the task
// names: "did it spawn anything today?" and "if not, why not?".
//
// The controller grafana-dashboard ConfigMap embeds each dashboards/*.json
// verbatim via `.Files.Get | nindent` under a YAML block scalar — easy to break.
// These tests guard that:
//   - the board is registered under the ConfigMap and embeds as PARSEABLE JSON,
//   - it carries its stable uid,
//   - the affirmative spawn series and the by-reason decline series are both
//     actually queried (the two halves of the observability pair),
//   - the reconciliation panel documents the second (bead-count) derivation.

func parseFormulaStepDispatchDashboard(t *testing.T) dashboard {
	t.Helper()
	data := dashboardConfigMapData(t)
	const key = "gardenio-formula-step-dispatch.json"
	d := parseDashboard(t, data, key)
	if d.UID != "gardenio-formula-step-dispatch" {
		t.Errorf("board must carry its stable uid; got %q", d.UID)
	}
	return d
}

func TestFormulaStepDispatchDashboard_Registered(t *testing.T) {
	t.Parallel()
	d := parseFormulaStepDispatchDashboard(t)
	if len(d.Panels) == 0 {
		t.Fatal("dashboard has no panels")
	}
	if !strings.Contains(d.Title, "Formula Step Dispatcher") {
		t.Errorf("unexpected title %q", d.Title)
	}
}

// The spawn counter is the affirmative "did it spawn?" half; it must be charted
// both by role and by formula so a dark lane is attributable.
func TestFormulaStepDispatchDashboard_ChartsSpawnByRoleAndFormula(t *testing.T) {
	t.Parallel()
	d := parseFormulaStepDispatchDashboard(t)
	exprs := allExprs(d)
	if !strings.Contains(exprs, "gardenio_formula_step_dispatch_spawned_total") {
		t.Error("board must query the spawn counter gardenio_formula_step_dispatch_spawned_total")
	}
	if !strings.Contains(exprs, "by (role)") {
		t.Error("spawns must be faceted by role — 'did the <role> lane spawn anything today?'")
	}
	if !strings.Contains(exprs, "by (formula)") {
		t.Error("spawns must be faceted by formula — which formulas' opt-in steps reach an agent")
	}
}

// The decline counter is the "why not?" half; it must be charted by reason.
func TestFormulaStepDispatchDashboard_ChartsDeclineByReason(t *testing.T) {
	t.Parallel()
	d := parseFormulaStepDispatchDashboard(t)
	exprs := allExprs(d)
	if !strings.Contains(exprs, "gardenio_formula_step_dispatch_declined_total") {
		t.Error("board must query the decline counter gardenio_formula_step_dispatch_declined_total")
	}
	if !strings.Contains(exprs, "by (reason)") {
		t.Error("declines must be faceted by reason — the 'why didn't my step fire?' answer")
	}
}

// The reconciliation cross-check (advice ka-0000005924) is the second, bead-count
// derivation. Prometheus can't query the daemon, so the panel documents the kd
// query rather than charting a phantom series — assert the description carries
// the dispatched_by stamp the reconciliation keys on so it never silently drops.
func TestFormulaStepDispatchDashboard_DocumentsReconciliation(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	raw := data["gardenio-formula-step-dispatch.json"]
	if !strings.Contains(raw, "dispatched_by=") && !strings.Contains(raw, "dispatched_by==") {
		t.Error("board must document the dispatched_by=formula-step-dispatcher reconciliation (advice ka-0000005924)")
	}
	if !strings.Contains(raw, "ka-0000005924") {
		t.Error("board should cite the two-way-measurement advice ka-0000005924")
	}
}

// spawnStatPanel is the shape TestFormulaStepDispatchDashboard_SpawnHeadlinesHonestWhenEmpty
// needs beyond the shared `dashboard` struct: a panel's noValue text and its
// nested children (the board groups panels under rows).
type spawnStatPanel struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
	FieldConfig struct {
		Defaults struct {
			NoValue string `json:"noValue"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
	Panels []spawnStatPanel `json:"panels"`
}

func (p spawnStatPanel) allExprs() string {
	var b strings.Builder
	for _, tgt := range p.Targets {
		b.WriteString(tgt.Expr)
		b.WriteByte('\n')
	}
	return b.String()
}

// statPanelsQuerying walks the panel tree (rows nest their children) and returns
// every stat panel whose targets reference the given metric.
func statPanelsQuerying(panels []spawnStatPanel, metric string) []spawnStatPanel {
	var out []spawnStatPanel
	for _, p := range panels {
		if p.Type == "stat" && strings.Contains(p.allExprs(), metric) {
			out = append(out, p)
		}
		out = append(out, statPanelsQuerying(p.Panels, metric)...)
	}
	return out
}

// Empty-state honesty guard (bugs ka-0000007851 → ka-0000000749 → ka-0000006178).
// A spawn is a BURSTY, infrequent event (a handful per week) whose raw CounterVec
// lives on the short-lived trigger-engine pod, so any single window can
// legitimately contain no spawn. When it does, the spawn-counter headline panels
// must read as a known idle state, NOT a broken board.
//
// History — and why this guard changed shape twice:
//
//   - ka-0000007851 shipped interim text saying "awaiting first formula opt-in":
//     at the time NO formula declared suggest_new_agent, so dispatch()/recordSpawn
//     was never reached and the counter had zero series.
//
//   - ka-0000000749 retired that framing (8 formulas now opt in; the counter emits
//     and reconciles with the dispatched_by bead count, A == B == 5 over 30d) and
//     required the empty-state PROSE to affirm the emitter is live, so an empty
//     window read as idle rather than dark.
//
//   - ka-0000006178 supersedes that prose requirement. noValue text renders only
//     when the query returned NOTHING — the one state in which "the emitter is
//     live" is exactly what cannot be established. Asserting it there is the
//     `unsupported-novalue` defect the grafanagrade empty-branch detector flags
//     (gardenio/controller/internal/grafanagrade). The affirmation now lives where
//     it can actually be evidenced: in the query, as a liveness gate on the zero
//     fallback —
//
//     sum(increase(gardenio_formula_step_dispatch_spawned_total[$__range]))
//     or (vector(0) and (count(gardenio_formula_step_dispatch_declined_total) > 0))
//
//     The decline counter is registered by the same dispatcher in the same process
//     and on the same :9092 scrape, so a rendered 0 is an EVIDENCED idle/all-declined
//     window, while a genuinely dark dispatcher now leaves the panel empty for the
//     noValue to explain honestly.
//
// The durable properties pinned here: both spawn stat headlines gate their zero,
// their empty-state text describes the dark case instead of claiming liveness, and
// the framing stays attributable (names suggest_new_agent, cites the tracking bug).
func TestFormulaStepDispatchDashboard_SpawnHeadlinesHonestWhenEmpty(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	raw := data["gardenio-formula-step-dispatch.json"]

	var board struct {
		Panels []spawnStatPanel `json:"panels"`
	}
	if err := json.Unmarshal([]byte(raw), &board); err != nil {
		t.Fatalf("embedded formula-step-dispatch board is not valid JSON: %v", err)
	}

	const control = "count(gardenio_formula_step_dispatch_declined_total) > 0"
	wantGate := "or (vector(0) and (" + control + "))"

	stats := statPanelsQuerying(board.Panels, "gardenio_formula_step_dispatch_spawned_total")
	if len(stats) < 2 {
		t.Fatalf("expected >=2 spawn-counter STAT headline panels; found %d", len(stats))
	}

	for _, p := range stats {
		expr := p.allExprs()

		// The zero fallback must be GATED on an independent, measurably-advancing
		// control, so an idle window renders an evidenced 0 and a dark dispatcher
		// renders nothing. A bare `or vector(0)` paints a green 0 over a dead
		// counter — the ungated-zero defect (ka-0000007164).
		if !strings.Contains(expr, wantGate) {
			t.Errorf("spawn-counter stat %q must gate its zero fallback on an independent liveness control so an idle window reads an evidenced 0, not a blind one; want %q, expr was: %q",
				p.Title, wantGate, expr)
		}
		if strings.Contains(strings.ReplaceAll(expr, wantGate, ""), "or vector(0)") {
			t.Errorf("spawn-counter stat %q still carries an UNGATED `or vector(0)` fallback; expr was: %q", p.Title, expr)
		}

		// With the gate in place, the empty branch means the control is dark, so
		// the noValue must say that rather than affirm liveness it cannot observe.
		nv := p.FieldConfig.Defaults.NoValue
		if nv == "" {
			t.Errorf("spawn-counter stat %q must carry a noValue explaining the (now gated) empty case", p.Title)
			continue
		}
		low := strings.ToLower(nv)
		if !strings.Contains(low, "dark") && !strings.Contains(low, "not reporting") {
			t.Errorf("spawn-counter stat %q noValue must describe the exporter-dark case the gate leaves empty (the count is UNKNOWN, not 0); got %q", p.Title, nv)
		}
		if strings.Contains(low, "emitter is live") || strings.Contains(low, "emitter live") {
			t.Errorf("spawn-counter stat %q noValue must not affirm the emitter is live — noValue renders only when the query returned nothing, which is the one state that cannot establish it (ka-0000006178); got %q", p.Title, nv)
		}
	}

	// Guard against a regression to the obsolete "awaiting first formula opt-in"
	// story now that the counter demonstrably emits (A == B == 5).
	if strings.Contains(raw, "awaiting first formula opt-in") {
		t.Error("the obsolete 'awaiting first formula opt-in' framing (bug ka-0000007851) must be removed — 8 formulas now opt in and the counter emits (bug ka-0000000749)")
	}

	// The empty-state framing must stay traceable to the tracking bug.
	if !strings.Contains(raw, "ka-0000000749") {
		t.Error("the empty-state framing should cite the tracking bug ka-0000000749")
	}

	// The framing must name suggest_new_agent as the dispatcher opt-in, so an
	// empty window is attributable to dispatcher behaviour rather than a broken
	// emitter (recordSpawn is on the spawn-success branch).
	if !strings.Contains(raw, "suggest_new_agent") {
		t.Error("the empty-state framing must name suggest_new_agent as the dispatcher opt-in, so an empty window is attributable to dispatcher behaviour rather than a broken emitter (bug ka-0000000749)")
	}
}

// latencyPanel is the shape the dispatch-latency asserts need beyond the shared
// `dashboard` struct: a panel's noValue text alongside its targets, so the
// grounded-empty guard can check both on the same panel.
type latencyPanel struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Targets []struct {
		Expr   string `json:"expr"`
		Format string `json:"format"`
	} `json:"targets"`
	FieldConfig struct {
		Defaults struct {
			Unit       string `json:"unit"`
			NoValue    string `json:"noValue"`
			Thresholds struct {
				Steps []struct {
					Color string   `json:"color"`
					Value *float64 `json:"value"`
				} `json:"steps"`
			} `json:"thresholds"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
	Panels []latencyPanel `json:"panels"`
}

func (p latencyPanel) allExprs() string {
	var b strings.Builder
	for _, tgt := range p.Targets {
		b.WriteString(tgt.Expr)
		b.WriteByte('\n')
	}
	return b.String()
}

// panelsQuerying walks the panel tree (rows nest their children) and returns
// every panel of any type whose targets reference the given substring.
func panelsQuerying(panels []latencyPanel, metric string) []latencyPanel {
	var out []latencyPanel
	for _, p := range panels {
		if strings.Contains(p.allExprs(), metric) {
			out = append(out, p)
		}
		out = append(out, panelsQuerying(p.Panels, metric)...)
	}
	return out
}

func parseLatencyPanels(t *testing.T) []latencyPanel {
	t.Helper()
	raw := dashboardConfigMapData(t)["gardenio-formula-step-dispatch.json"]
	var board struct {
		Panels []latencyPanel `json:"panels"`
	}
	if err := json.Unmarshal([]byte(raw), &board); err != nil {
		t.Fatalf("embedded formula-step-dispatch board is not valid JSON: %v", err)
	}
	return board.Panels
}

const (
	dispatchLatencyHist   = "gardenio_formula_step_dispatch_latency_seconds"
	dispatchLatencyUnmeas = "gardenio_formula_step_dispatch_latency_unmeasured_total"
)

// The distribution half of the board (task ka-0000006705). The two counters this
// board shipped with answer BOOLEAN questions — "did it spawn?", "why not?" —
// and left the operator with no way to ask "how long did a ready step WAIT for
// its agent?". Before the histogram existed the only available answer was a
// PROXY: quantile_over_time over the pending gauge, which measures queue depth
// over time rather than any individual step's wait.
//
// Pinned here so the real distribution cannot silently regress to that proxy:
// the bucket series is queried, the p50/p95/p99 triple is present, and the
// per-path facet survives (path=event is the SSE hot path and should be
// sub-second-to-seconds; path=backfill is the 2-minute sweep recovering a step
// the hot path missed and is legitimately minutes-to-hours — pooling them would
// produce one meaningless quantile).
func TestFormulaStepDispatchDashboard_ChartsDispatchLatencyDistribution(t *testing.T) {
	t.Parallel()
	d := parseFormulaStepDispatchDashboard(t)
	exprs := allExprs(d)

	if !strings.Contains(exprs, dispatchLatencyHist+"_bucket") {
		t.Fatalf("board must query the dispatch-latency histogram %s_bucket — the whole point of task ka-0000006705 is that the distribution is measured, not proxied", dispatchLatencyHist)
	}
	for _, q := range []string{"histogram_quantile(0.50,", "histogram_quantile(0.95,", "histogram_quantile(0.99,"} {
		if !strings.Contains(exprs, q) {
			t.Errorf("board must carry the %s target — the p50/p95/p99 read is the operator-facing ask", q)
		}
	}
	if !strings.Contains(exprs, "by (le, path)") {
		t.Error("latency percentiles must be faceted by path — the SSE hot path (sub-second) and the backfill sweep (minutes-to-hours) are different distributions and pooling them yields one meaningless quantile")
	}

	// The mean is the robust companion: no bucket interpolation, so a
	// single-sample window reports that sample exactly rather than a bucket
	// edge. At a handful of dispatches per week the percentiles step rather
	// than curve, so shipping them WITHOUT the mean would invite reading noise
	// as a regression.
	if !strings.Contains(exprs, dispatchLatencyHist+"_sum") || !strings.Contains(exprs, dispatchLatencyHist+"_count") {
		t.Errorf("board must chart the windowed mean (increase(%s_sum)/increase(%s_count)) alongside the percentiles — quantiles are jumpy at this event rate", dispatchLatencyHist, dispatchLatencyHist)
	}

	// A heatmap is the only panel that shows the SHAPE of the distribution —
	// specifically whether it is bimodal, which is the expected healthy shape
	// here (a sub-second event cluster plus a sparse backfill tail).
	var heatmaps int
	for _, p := range panelsQuerying(parseLatencyPanels(t), dispatchLatencyHist+"_bucket") {
		if p.Type == "heatmap" {
			heatmaps++
			for _, tgt := range p.Targets {
				if strings.Contains(tgt.Expr, dispatchLatencyHist+"_bucket") && tgt.Format != "heatmap" {
					t.Errorf("heatmap panel %q must set target format=%q or Grafana renders the bucket series as plain timeseries; got %q", p.Title, "heatmap", tgt.Format)
				}
			}
		}
	}
	if heatmaps == 0 {
		t.Error("board must carry a bucket heatmap — percentiles cannot show a bimodal distribution, and event-vs-backfill bimodality is the expected healthy shape")
	}
}

// Every latency panel must be honest about seconds and about emptiness.
//
// Emptiness is a REAL state here, not an edge case: a dispatch is a
// handful-per-week event, so most windows legitimately contain no sample. That
// makes `or vector(0)` actively dangerous on this row — a fabricated 0 on a
// LATENCY panel reads as an instantaneous dispatch and is indistinguishable
// from a real one, and noValue can never fire behind it (the grounded-zero
// class, bug ka-0000006531). So: no ungated zero anywhere on the row, and every
// panel carries text explaining what empty means.
func TestFormulaStepDispatchDashboard_LatencyPanelsGroundedWhenEmpty(t *testing.T) {
	t.Parallel()
	panels := panelsQuerying(parseLatencyPanels(t), dispatchLatencyHist)
	if len(panels) < 4 {
		t.Fatalf("expected >=4 panels querying %s (percentiles, headline, mean, heatmap); found %d", dispatchLatencyHist, len(panels))
	}

	for _, p := range panels {
		if strings.Contains(p.allExprs(), "or vector(0)") {
			t.Errorf("latency panel %q must not fabricate a zero: a 0 on a latency panel reads as an instant dispatch and is indistinguishable from a real one, and noValue can never render behind it (bug ka-0000006531)", p.Title)
		}
		if p.FieldConfig.Defaults.NoValue == "" {
			t.Errorf("latency panel %q must carry a noValue — an empty window is the COMMON case at a handful of dispatches per week, and must read as 'no dispatch in range', not as a broken board", p.Title)
		}
	}

	// Seconds-valued panels must say so, or a 1800 renders as "1.8K" and an
	// operator reads a half-hour wait as a large dimensionless number.
	//
	// Not every panel touching the histogram family reports a duration: the
	// accounting panels read _count as a COUNT of samples (dimensionless, unit
	// "short") to reconcile against the spawn counter. Those are identified by
	// the counters they sit beside, and are exempt from the seconds contract —
	// stamping unit="s" on them would be the mislabel, not the fix.
	for _, p := range panels {
		if p.Type == "heatmap" {
			continue // heatmaps carry the unit on options.yAxis, not fieldConfig
		}
		e := p.allExprs()
		if strings.Contains(e, "gardenio_formula_step_dispatch_spawned_total") || strings.Contains(e, dispatchLatencyUnmeas) {
			continue // accounting panel: counts samples, not seconds
		}
		if p.FieldConfig.Defaults.Unit != "s" {
			t.Errorf("duration panel %q must set unit=\"s\" — otherwise a 1800-second wait renders as the dimensionless \"1.8K\"; got %q", p.Title, p.FieldConfig.Defaults.Unit)
		}
		if len(p.FieldConfig.Defaults.Thresholds.Steps) < 2 {
			t.Errorf("duration panel %q must carry latency thresholds so a slow dispatch colours instead of having to be read off the axis", p.Title)
		}
	}
}

// The accounting invariant (advice ka-0000005924) is what makes the histogram's
// _count a trustworthy denominator rather than a silent undercount: every
// SUCCESSFUL dispatch produces exactly one entry, either a latency sample or an
// unmeasured-reason increment, so
//
//	_count + unmeasured_total == spawned_total
//
// over any window. Some real dispatches genuinely have no readiness instant to
// measure from (a step with no blocking dep) or an unreadable one (a cleared
// blocker with neither closed_at nor updated_at); skipping those silently would
// shrink _count with no trace. The board must therefore chart the sum against
// the spawn counter AND attribute the unmeasured remainder by reason.
func TestFormulaStepDispatchDashboard_ChartsLatencyAccounting(t *testing.T) {
	t.Parallel()
	d := parseFormulaStepDispatchDashboard(t)
	exprs := allExprs(d)
	raw := dashboardConfigMapData(t)["gardenio-formula-step-dispatch.json"]

	if !strings.Contains(exprs, dispatchLatencyUnmeas) {
		t.Fatalf("board must query %s — without it a gap between the histogram's _count and the spawn counter is mysterious rather than attributable", dispatchLatencyUnmeas)
	}
	if !strings.Contains(exprs, "by (reason)") {
		t.Error("unmeasured dispatches must be faceted by reason — 'why does this spawn have no latency?' is the follow-up question")
	}

	// The reconciliation panel: the sum must be charted against the spawn
	// counter on the same panel, or the invariant is prose rather than a
	// visible cross-check.
	var reconciled bool
	for _, p := range panelsQuerying(parseLatencyPanels(t), dispatchLatencyUnmeas) {
		e := p.allExprs()
		if strings.Contains(e, dispatchLatencyHist+"_count") && strings.Contains(e, "gardenio_formula_step_dispatch_spawned_total") {
			reconciled = true
		}
	}
	if !reconciled {
		t.Errorf("one panel must plot increase(%s_count) + increase(%s) against increase(gardenio_formula_step_dispatch_spawned_total) so the accounting invariant is a VISIBLE cross-check (advice ka-0000005924), not just prose", dispatchLatencyHist, dispatchLatencyUnmeas)
	}

	// The reason enum is hard-bounded in code; the board must document all
	// three so an operator can act on each without reading the source.
	for _, reason := range []string{"no_blocking_deps", "blocker_time_missing", "clock_skew"} {
		if !strings.Contains(raw, reason) {
			t.Errorf("board must document the %q unmeasured reason so an operator can tell a benign structural gap from real clock drift", reason)
		}
	}

	// Latency work must stay traceable to its task, like the rest of the board.
	if !strings.Contains(raw, "ka-0000006705") {
		t.Error("the dispatch-latency row should cite its task ka-0000006705")
	}
}
