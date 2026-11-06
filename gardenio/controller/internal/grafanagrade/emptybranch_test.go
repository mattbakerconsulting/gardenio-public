package grafanagrade

import (
	"encoding/json"
	"strings"
	"testing"
)

// panelFrom builds a panel the way a dashboard JSON would carry it, so the
// tests exercise the same decode path the fleet scan uses.
func panelFrom(t *testing.T, typ, title, expr, noValue string) *Panel {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type":  typ,
		"title": title,
		"targets": []map[string]any{
			{"expr": expr},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{"noValue": noValue},
		},
	})
	if err != nil {
		t.Fatalf("marshal panel: %v", err)
	}
	var p Panel
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal panel: %v", err)
	}
	return &p
}

func classes(fs []Finding) []Class {
	out := make([]Class, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Class)
	}
	return out
}

func hasClass(fs []Finding, c Class) bool {
	for _, f := range fs {
		if f.Class == c {
			return true
		}
	}
	return false
}

// TestCheckPanel_DeferredAgentsHistory is the anti-vacuity core: it replays
// every shape the cost-overview "Deferred Agents" panel has actually shipped —
// three "fixes" in three days, each trading one false reading for another — and
// asserts the detector separates the three lies from the one honest form.
//
// If this test can be deleted without the fleet scan going red, the scan is
// vacuous: a checker that never fires is indistinguishable from a clean fleet
// (advice ka-0000002586 — positive-control a detector before reporting its
// zero).
func TestCheckPanel_DeferredAgentsHistory(t *testing.T) {
	const (
		gauge = "gardenio_agents_deferred_total"
		gate  = "gardenio_reconcile_total"
	)

	cases := []struct {
		name      string
		expr      string
		noValue   string
		wantClass []Class
	}{
		{
			// ka-0000007164: painted a confident green 0 over a gauge that was
			// not emitting at all.
			name:      "pass 0 — ungated or vector(0)",
			expr:      "sum(" + gauge + ") or vector(0)",
			noValue:   "",
			wantClass: []Class{ClassUngatedZero},
		},
		{
			// ka-0000002930 pass 1: dropping the mask left a bare instant read
			// whose noValue asserts the emitter is dark — in the HEALTHY case.
			name:      "pass 1 — bare instant read asserting a dead emitter",
			expr:      "sum(" + gauge + ")",
			noValue:   "stale — no samples in window (counter not emitting)",
			wantClass: []Class{ClassUnsupportedNoValue},
		},
		{
			// ka-0000002930 pass 2 (shipped on main): reports the last deferral
			// anywhere in the window as the current count. Measured live
			// 2026-09-02 it rendered an orange 1 while the last reconcile pass
			// deferred 0 and the newest sample was 57 min old.
			name:      "pass 2 — windowed read re-dated as current",
			expr:      "sum(last_over_time(" + gauge + "[$__range]))",
			noValue:   "no defer events in window",
			wantClass: []Class{ClassStaleWindow, ClassUnsupportedNoValue},
		},
		{
			// MR !8081: instant read, zero gated on the reconciler measurably
			// advancing, and noValue that only speaks about the gate.
			name:      "pass 3 — instant read with an independent liveness gate",
			expr:      "sum(" + gauge + ") or (0 * (sum(increase(" + gate + "[15m])) > 0))",
			noValue:   "no reconcile signal (controller not reporting)",
			wantClass: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckPanel("cost-overview.json", panelFrom(t, "stat", "Deferred Agents", tc.expr, tc.noValue))
			defects := Defects(got)
			if len(tc.wantClass) == 0 {
				if len(defects) != 0 {
					t.Fatalf("want no defects, got %v (%v)", classes(defects), defects)
				}
				return
			}
			for _, want := range tc.wantClass {
				if !hasClass(defects, want) {
					t.Errorf("want class %q, got %v", want, classes(defects))
				}
			}
			if len(defects) != len(tc.wantClass) {
				t.Errorf("want exactly %v, got %v", tc.wantClass, classes(defects))
			}
		})
	}
}

// TestCheckPanel_GateShapes pins what does and does not count as an independent
// liveness term — the single distinction the whole detector rests on.
func TestCheckPanel_GateShapes(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		wantGate bool
	}{
		{
			name:     "reconcile control, measurably advancing",
			expr:     "sum(gardenio_agents_deferred_total) or (0 * (sum(increase(gardenio_reconcile_total[15m])) > 0))",
			wantGate: true,
		},
		{
			name:     "always-present sibling series from the same exporter",
			expr:     "sum(increase(kube_pod_container_status_restarts_total{pod=~\"crew-.*\"}[1h])) or (vector(0) and (count(kube_pod_container_status_last_terminated_reason) > 0))",
			wantGate: true,
		},
		{
			name:     "up scrape signal in an and-gate",
			expr:     "sum(vpa_recommender_vpa_objects_count) or (vector(0) and (count(up{job=~\".*vpa-recommender.*\"} == 1) > 0))",
			wantGate: true,
		},
		{
			name:     "the panel reads up directly",
			expr:     "max(up{job=\"perf-federation\"})",
			wantGate: true,
		},
		{
			name:     "bare constant coalesce is not a gate",
			expr:     "sum(gardenio_agents_deferred_total) or vector(0)",
			wantGate: false,
		},
		{
			// The control must be INDEPENDENT: gating a metric on its own
			// liveness inherits exactly the blindness it claims to rule out.
			name:     "self-gated on the same metric is not a gate",
			expr:     "sum(gardenio_agents_deferred_total) or (0 * (sum(increase(gardenio_agents_deferred_total[15m])) > 0))",
			wantGate: false,
		},
		{
			// Without the comparison the branch inherits the control's own
			// emptiness, so it cannot establish that the control is advancing.
			name:     "control without a comparison is not a gate",
			expr:     "sum(gardenio_agents_deferred_total) or (0 * sum(increase(gardenio_reconcile_total[15m])))",
			wantGate: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := livenessGate(tc.expr)
			if got != tc.wantGate {
				t.Fatalf("livenessGate(%q) = %v (%s), want %v", tc.expr, got, why, tc.wantGate)
			}
		})
	}
}

// TestCheckPanel_NoValueClaims covers the cheap half of the detector: the text
// check needs no series metadata and would have caught passes 1 and 2 on its
// own.
func TestCheckPanel_NoValueClaims(t *testing.T) {
	const gated = "sum(gardenio_agents_deferred_total) or (0 * (sum(increase(gardenio_reconcile_total[15m])) > 0))"
	const bare = "sum(increase(gardenio_agent_involuntary_deaths_total[24h]))"

	cases := []struct {
		name      string
		expr      string
		noValue   string
		wantClass Class
	}{
		{"emitter-health claim, no gate", bare, "stale — no samples in window (counter not emitting)", ClassUnsupportedNoValue},
		{"real-zero claim, no gate", bare, "no defer events in window", ClassUnsupportedNoValue},
		{"real-zero claim behind a gate", gated, "no defer events in window", ClassNoValueClaimsZero},
		{"emitter-health claim behind a gate is supported", gated, "no reconcile signal (controller not reporting)", ""},
		{"neutral text is not a claim", bare, "no data", ""},
		// Deployment/scrape state is knowledge the author has out of band and
		// dates with a bead; it is not an inference drawn from the empty result.
		{"documented deployment state is not a claim", bare, "pending — VPA recommender not deployed (epic ka-0000005189)", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defects := Defects(CheckPanel("b.json", panelFrom(t, "stat", "P", tc.expr, tc.noValue)))
			if tc.wantClass == "" {
				for _, f := range defects {
					if f.Class == ClassUnsupportedNoValue || f.Class == ClassNoValueClaimsZero {
						t.Fatalf("unexpected text finding: %s", f)
					}
				}
				return
			}
			if !hasClass(defects, tc.wantClass) {
				t.Fatalf("want %q, got %v", tc.wantClass, classes(defects))
			}
		})
	}
}

// TestCheckPanel_NullMappingCarriesTheClaim: Grafana renders the empty branch
// through a `special`/null value mapping as well as noValue, so a claim moved
// into a mapping must not escape the detector.
func TestCheckPanel_NullMappingCarriesTheClaim(t *testing.T) {
	raw := []byte(`{
	  "type": "stat",
	  "title": "Involuntary deaths (last 24h)",
	  "targets": [{"expr": "sum(increase(gardenio_agent_involuntary_deaths_total[24h]))"}],
	  "fieldConfig": {"defaults": {"mappings": [
	    {"type": "special", "options": {"match": "null", "result": {"text": "stale — no samples"}}},
	    {"type": "value", "options": {"0": {"text": "OK"}}}
	  ]}}
	}`)
	var p Panel
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !hasClass(Defects(CheckPanel("involuntary-deaths.json", &p)), ClassUnsupportedNoValue) {
		t.Fatalf("a null-mapping claim must be caught like a noValue claim")
	}
}

// TestCheckPanel_ScopeRules pins the panel-type and datasource scoping so the
// detector stays off the surfaces where it has nothing to say.
func TestCheckPanel_ScopeRules(t *testing.T) {
	t.Run("stale-window is an instant-read rule", func(t *testing.T) {
		expr := "sum(last_over_time(gardenio_agents_deferred_total[$__range]))"
		if hasClass(CheckPanel("b.json", panelFrom(t, "timeseries", "trend", expr, "")), ClassStaleWindow) {
			t.Fatal("a timeseries shows the gap itself; stale-window must not fire on it")
		}
		if !hasClass(CheckPanel("b.json", panelFrom(t, "stat", "now", expr, "")), ClassStaleWindow) {
			t.Fatal("stale-window must fire on a stat")
		}
	})

	t.Run("window statistics are not resurrections", func(t *testing.T) {
		for _, expr := range []string{
			"count(last_over_time(gardenio_session_boot_context_tokens[$__range]))",
			"avg(last_over_time(gardenio_session_boot_context_tokens[$__range]))",
			"quantile(0.95, last_over_time(gardenio_session_boot_context_tokens[$__range]))",
		} {
			if hasClass(CheckPanel("b.json", panelFrom(t, "stat", "Sessions in Window", expr, "")), ClassStaleWindow) {
				t.Errorf("population statistic over the window wrongly flagged: %s", expr)
			}
		}
	})

	t.Run("ungated zero fires on any panel type", func(t *testing.T) {
		expr := "sum by (query) (rate(gardenio_jira_scrape_errors_total[5m])) or vector(0)"
		if !hasClass(CheckPanel("b.json", panelFrom(t, "timeseries", "Scrape errors", expr, "")), ClassUngatedZero) {
			t.Fatal("masking a blind emitter with a constant is a lie on any panel type")
		}
	})

	t.Run("non-PromQL targets are skipped", func(t *testing.T) {
		raw := []byte(`{
		  "type": "stat", "title": "Traces",
		  "targets": [{"expr": "_time:5m", "datasource": {"type": "victoriametrics-logs-datasource"}}]
		}`)
		var p Panel
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := CheckPanel("b.json", &p); len(got) != 0 {
			t.Fatalf("LogsQL target is not PromQL-replayable; want no findings, got %v", got)
		}
	})

	t.Run("hidden targets are skipped", func(t *testing.T) {
		raw := []byte(`{
		  "type": "stat", "title": "Hidden",
		  "targets": [{"expr": "sum(m) or vector(0)", "hide": true}]
		}`)
		var p Panel
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := CheckPanel("b.json", &p); len(got) != 0 {
			t.Fatalf("want no findings for a hidden target, got %v", got)
		}
	})
}

// TestCheckPanel_BlindEmptyIsAdvisory: a panel that simply cannot answer the
// question is reported, but does not gate — it has not lied, and fixing it
// needs a liveness control chosen per exporter.
func TestCheckPanel_BlindEmptyIsAdvisory(t *testing.T) {
	got := CheckPanel("b.json", panelFrom(t, "stat", "Deferred Agents", "sum(gardenio_agents_deferred_total)", ""))
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %v", got)
	}
	if got[0].Class != ClassBlindEmpty || got[0].Severity != SeverityAdvisory {
		t.Fatalf("want advisory blind-empty, got %s/%s", got[0].Class, got[0].Severity)
	}
	if len(Defects(got)) != 0 {
		t.Fatal("advisories must not gate")
	}
}

// TestCheckDashboard_WalksRows: row panels nest their children, and a detector
// that stops at the top level silently misses whole rows.
func TestCheckDashboard_WalksRows(t *testing.T) {
	raw := []byte(`{
	  "uid": "b", "title": "B",
	  "panels": [
	    {"type": "row", "title": "Fleet", "panels": [
	      {"type": "stat", "title": "Nested", "targets": [{"expr": "sum(m) or vector(0)"}]}
	    ]}
	  ]
	}`)
	d, err := ParseDashboard(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := Defects(CheckDashboard("b.json", d))
	if len(got) != 1 || got[0].Panel != "Nested" {
		t.Fatalf("want the nested panel flagged, got %v", got)
	}
	if strings.Contains(got[0].PanelType, "row") {
		t.Fatalf("row containers must not be reported as panels: %v", got)
	}
}
