package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Grounded-zero sweep (bug ka-0000006531, epic ka-0000008066).
//
// The 2026-09-02 GRADE iteration ran an automated `or vector(0)` detector for the
// first time: it replays the LEFT operand of every `<expr> or vector(0)` on its
// own over 30d, and a left side with zero points means the panel is painting a
// fabricated 0 over a query that has never returned anything. `noValue` and value
// mappings can never fire behind `or vector(0)`, so such a panel can never warn
// you when it goes blind — a genuine 0 and a blind 0 render identically.
//
// Two of the eight it found were live display lies:
//
//   - jira-metrics "Scrape errors (rate)" showed a confident 0 while
//     gardenio_jira_scrape_errors_total's last sample was 317h old, and
//   - gardenio-dark-spend "Active dark agents" rendered a confident blue 0 on a
//     board whose own text panel says, in bold, "empty is not $0" (monorepo-dark
//     has never been federated into gardenio-vmsingle — ka-0000005942).
//
// The fix for each panel is one of three shapes, never a bare "counter not
// emitting" claim (that over-correction is ka-0000002930, where a sporadic gauge
// got labelled dead because an instant read fell outside the 5m staleness
// lookback):
//
//  1. DROP the guard where absence genuinely means "not in scope here", and let
//     an honest noValue name that case (dark-spend, whose sibling spend tiles
//     already ship this pattern; involuntary-deaths / MR !8007 is the precedent).
//  2. EARN the zero off a liveness anchor that is live whenever the emitter is —
//     `<event counter> or (<anchor> * 0)`. The zero then means "we were watching
//     and nothing happened"; if the emitter stops, the anchor stops with it and
//     the panel falls through to noValue.
//  3. MEASURE the zero directly with `< bool` instead of a filtering `count()`,
//     so every reported series scores 0/1 and an unreported gauge stays empty.
//
// Re-running the detector against the tree at merge time surfaced two MORE panels
// of the same class that the 07:23 pass had not flagged (cicd-minutes "Scrape
// errors (rate)" and operated-by "M3: resolver errors by error_type" — both
// label-gated error CounterVecs on a live emitter, whose series had vanished in a
// process restart ~57m earlier). They are swept here too, so the table below
// covers ten panels, not the eight the triage named.
//
// These assertions pin the outcome per-panel so a revert to `or vector(0)` — or a
// new panel copied from the old shape — fails the chart tests rather than
// silently reinstating a confident zero.
func TestGroundedZeroSweep_NoFabricatedZeroOnSweptPanels(t *testing.T) {
	t.Parallel()
	type panelCase struct {
		file   string
		panel  string
		anchor string // substring the replacement must contain ("" = no anchor required)
		why    string
	}
	cases := []panelCase{
		{
			file:   "jira-metrics.json",
			panel:  "Scrape errors (rate)",
			anchor: "gardenio_jira_scrape_duration_seconds) * 0",
			why:    "errors_total is a label-gated CounterVec (no {query} series until that query has failed once since process start), so the zero must be earned off the per-query scrape-duration gauge the scrape loop rewrites every cycle",
		},
		{
			file:   "dark-spend.json",
			panel:  "Active dark agents",
			anchor: "",
			why:    "monorepo-dark is not federated into gardenio-vmsingle (ka-0000005942); absence must read as the board's documented 'empty is not $0', not as 0 agents",
		},
		{
			file:   "control-plane-liveness.json",
			panel:  "#12 Probe-loop tick rate (second, independent liveness measure)",
			anchor: "gardenio_probe_loop_ticks_total[$__rate_interval])) * 0",
			why:    "stalls_detected_total is only-on-stall; a 0 stalls/min line may only render while the tick counter on the same panel is demonstrably advancing",
		},
		{
			file:   "resume-latency.json",
			panel:  "Cold-fallback (thaw-fresh) count — 30m",
			anchor: "gardenio_resume_latency_seconds_count{env=~\"$env\"}[30m])) * 0",
			why:    "path=thaw-fresh has never emitted (only path=cold has 30d samples), so a 0 may only render when some resume was observed in the same window",
		},
		{
			file:   "xcheck-trust.json",
			panel:  "Pairs drifting below 0.95 (now)",
			anchor: "< bool 0.95",
			why:    "`count(x < 0.95)` is empty both when no pair drifted and when the gauge stopped reporting; `< bool` scores every reported pair so the healthy case is a measured 0",
		},
		{
			file:   "kbeads-embedding.json",
			panel:  "Embed error rate (per 5m)",
			anchor: "gardenio_beads_embed_duration_seconds_count{backend=~\"$backend\"}[5m])) * 0",
			why:    "embed_errors_total is only-on-error, so the zero must be earned off the per-backend embed call counter that exists whenever the path is wired",
		},
		{
			file:   "cicd-minutes.json",
			panel:  "Scrape errors (rate)",
			anchor: "gardenio_cicd_scrape_duration_seconds) * 0",
			why:    "scrape_errors_total is a label-gated CounterVec whose {provider} series disappears on a collector restart until the next failure, so the zero must be earned off the per-provider scrape-duration gauge rewritten every cycle",
		},
		{
			file:   "operated-by.json",
			panel:  "M3: resolver errors by error_type",
			anchor: "gardenio_beads_operated_by_writes_total{env=~\"$env\"}[5m])) * 0",
			why:    "resolver_errors_total is only-on-error; the board already carried a noValue that `or vector(0)` made permanently unreachable, so the zero must be earned off the operated-by write counter that runs whenever the resolver does",
		},
		{
			file:   "perf-overview.json",
			panel:  "Load request rate",
			anchor: `{__name__=~"fastapi_sponsor_api_http_requests_total|http_server_requests_seconds_count"`,
			why:    "one union selector keeps a keycloak gap from blanking the tile without fabricating a zero when NEITHER family reports",
		},
		{
			file:   "perf-overview.json",
			panel:  "Load error rate",
			anchor: `{__name__=~"fastapi_sponsor_api_http_requests_total|http_server_requests_seconds_count"`,
			why:    "each error leg earns its zero off the union request rate, so 0% only renders while traffic is demonstrably being scraped",
		},
	}

	for _, c := range cases {
		var b struct {
			Panels []struct {
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
		}
		if err := json.Unmarshal([]byte(readDashboardJSON(t, c.file)), &b); err != nil {
			t.Errorf("dashboards/%s is not valid JSON: %v", c.file, err)
			continue
		}

		var found bool
		for _, p := range b.Panels {
			if p.Title != c.panel {
				continue
			}
			found = true
			joined := ""
			for _, tg := range p.Targets {
				joined += tg.Expr + "\n"
				// The core invariant: no fabricated zero on a swept panel.
				if strings.Contains(tg.Expr, "vector(0)") {
					t.Errorf("%s panel %q reinstates a fabricated zero (`vector(0)`): %s. Got %q",
						c.file, p.Title, c.why, tg.Expr)
				}
			}
			// An earned/measured zero must name its anchor, or the panel silently
			// degrades back to "absent and zero look the same".
			if c.anchor != "" && !strings.Contains(joined, c.anchor) {
				t.Errorf("%s panel %q must ground its zero on %q — %s. Got %q",
					c.file, p.Title, c.anchor, c.why, strings.TrimSpace(joined))
			}
			// Every swept panel must be able to SAY it went blind.
			if p.FieldConfig.Defaults.NoValue == "" && c.anchor == "" {
				t.Errorf("%s panel %q drops its guard without a noValue: an empty result would "+
					"render as a bare grafana \"No data\", indistinguishable from a broken board — %s",
					c.file, p.Title, c.why)
			}
		}
		if !found {
			t.Errorf("dashboards/%s no longer has the swept panel %q (renamed? this guard must follow it)",
				c.file, c.panel)
		}
	}
}

// TestGroundedZeroSweep_LiveLiePanelsNameTheEmptyCase pins the two panels that
// were actively lying on 2026-09-02 to a noValue that names WHY they are empty.
// A bare grafana "No data" would swap one ambiguity for another; the operator has
// to be able to tell "the emitter stopped" from "nothing happened".
func TestGroundedZeroSweep_LiveLiePanelsNameTheEmptyCase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file, panel string
		mustContain []string
	}{
		// The dark-spend tile must mirror its sibling spend tiles' contract and cite
		// the federation bead, so it can never again contradict the board's own
		// "empty is not $0" text panel.
		{"dark-spend.json", "Active dark agents", []string{"federation", "ka-0000005942"}},
		// The jira tile must say the scrape-error rate is UNKNOWN, not zero, when
		// the exporter itself stops reporting.
		{"jira-metrics.json", "Scrape errors (rate)", []string{"UNKNOWN", "not 0"}},
		// operated-by.json already SHIPPED a noValue ("no resolver errors — healthy")
		// that `or vector(0)` made permanently unreachable — the anti-pattern in its
		// purest form: an author wrote the honest-empty string and the guard ate it.
		// It now names the blind case instead of asserting health.
		{"operated-by.json", "M3: resolver errors by error_type", []string{"UNKNOWN", "not 0"}},
		{"cicd-minutes.json", "Scrape errors (rate)", []string{"UNKNOWN", "not 0"}},
	}
	for _, c := range cases {
		var b struct {
			Panels []struct {
				Title       string `json:"title"`
				FieldConfig struct {
					Defaults struct {
						NoValue string `json:"noValue"`
					} `json:"defaults"`
				} `json:"fieldConfig"`
			} `json:"panels"`
		}
		if err := json.Unmarshal([]byte(readDashboardJSON(t, c.file)), &b); err != nil {
			t.Errorf("dashboards/%s is not valid JSON: %v", c.file, err)
			continue
		}
		for _, p := range b.Panels {
			if p.Title != c.panel {
				continue
			}
			nv := p.FieldConfig.Defaults.NoValue
			for _, want := range c.mustContain {
				if !strings.Contains(nv, want) {
					t.Errorf("%s panel %q noValue must name the empty case (contain %q); got %q",
						c.file, p.Title, want, nv)
				}
			}
		}
	}
}
