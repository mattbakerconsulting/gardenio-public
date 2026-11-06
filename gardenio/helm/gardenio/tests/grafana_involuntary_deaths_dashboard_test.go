package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Involuntary-deaths "per day" trend panel (bug ka-0000005444).
//
// The panel titled "Involuntary deaths per day, total (7d trend …)" must render
// as SEVEN discrete per-day bars, not a rolling cumulative curve. The bug was
// that increase(...[1d]) evaluated at the dashboard's auto step (worst at the
// 24h picker default) produces an overlapping, monotonically-climbing sawtooth
// that reads as an ever-climbing cumulative line — the opposite of the
// "should trend to zero" intent.
//
// The fix pins the panel to a fixed 7-day window (timeFrom: "7d") independent
// of the dashboard time picker AND forces a 1-day query step (target interval:
// "1d") so each bar is exactly one calendar day's deaths. These tests guard
// that both overrides survive future edits to the embedded dashboard JSON.

// involuntaryDeathsDashboard is the minimal shape these tests assert against —
// it captures the panel-level time/step overrides the shared `dashboard`
// struct in grafana_fleet_lifecycle_dashboard_test.go does not.
type involuntaryDeathsDashboard struct {
	UID   string `json:"uid"`
	Links []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"links"`
	Panels []struct {
		Title         string          `json:"title"`
		Type          string          `json:"type"`
		TimeFrom      json.RawMessage `json:"timeFrom"`
		MaxDataPoints int             `json:"maxDataPoints"`
		Targets       []struct {
			Expr     string `json:"expr"`
			Interval string `json:"interval"`
		} `json:"targets"`
		FieldConfig struct {
			Defaults struct {
				NoValue  string `json:"noValue"`
				Mappings []struct {
					Type    string `json:"type"`
					Options struct {
						Match  string `json:"match"`
						Result struct {
							Text  string `json:"text"`
							Color string `json:"color"`
						} `json:"result"`
					} `json:"options"`
				} `json:"mappings"`
				Thresholds struct {
					Steps []struct {
						Color string          `json:"color"`
						Value json.RawMessage `json:"value"`
					} `json:"steps"`
				} `json:"thresholds"`
			} `json:"defaults"`
		} `json:"fieldConfig"`
	} `json:"panels"`
}

func parseInvoluntaryDeathsDashboard(t *testing.T) involuntaryDeathsDashboard {
	t.Helper()
	data := dashboardConfigMapData(t)
	const key = "gardenio-involuntary-deaths.json"
	raw, ok := data[key]
	if !ok {
		t.Fatalf("dashboard ConfigMap is missing key %q; keys present: %v", key, keysOf(data))
	}
	var d involuntaryDeathsDashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("embedded %s is not valid JSON (nindent likely broke it): %v", key, err)
	}
	if d.UID != "gardenio-involuntary-deaths" {
		t.Errorf("involuntary-deaths board must carry its stable uid; got %q", d.UID)
	}
	return d
}

func TestInvoluntaryDeathsDashboard_PerDayPanelIsPinnedTo7dWithDailyStep(t *testing.T) {
	t.Parallel()
	d := parseInvoluntaryDeathsDashboard(t)

	const wantTitle = "Involuntary deaths per day, total (7d trend — should trend to zero)"
	var found bool
	for _, p := range d.Panels {
		if p.Title != wantTitle {
			continue
		}
		found = true

		// The panel must be pinned to a fixed 7-day window independent of the
		// dashboard time picker, so the 24h default can never squash it into a
		// rolling cumulative curve.
		if string(p.TimeFrom) != `"7d"` {
			t.Errorf("per-day panel must set timeFrom \"7d\" (fixed 7-day window); got %s", p.TimeFrom)
		}

		// A 1-day query step is what turns increase(...[1d]) into one bar per
		// day instead of an overlapping rolling curve.
		if len(p.Targets) == 0 {
			t.Fatal("per-day panel has no targets")
		}
		if got := p.Targets[0].Interval; got != "1d" {
			t.Errorf("per-day panel target must set interval \"1d\" (one bar per day); got %q", got)
		}

		// maxDataPoints must be small enough that Grafana cannot subdivide the
		// 7-day window below the 1-day step (7 days + a lead-in bucket = 8).
		if p.MaxDataPoints == 0 || p.MaxDataPoints > 8 {
			t.Errorf("per-day panel maxDataPoints must be set and <= 8 so bars stay per-day; got %d", p.MaxDataPoints)
		}
	}
	if !found {
		t.Fatalf("could not find the per-day trend panel %q on the involuntary-deaths board", wantTitle)
	}
}

// WS4 (ka-0000007155): the involuntary-deaths board must carry the node-reclaim
// sentinel — the panels that visualize exactly what the AgentNodeReclaimDeaths
// alert fires on (context_cancelled family, per-hour, against the 2/h
// threshold) — so a WS1/WS2 regression is legible at a glance.
func TestInvoluntaryDeathsDashboard_HasNodeReclaimSentinel(t *testing.T) {
	t.Parallel()
	d := parseInvoluntaryDeathsDashboard(t)

	// A stat + timeseries panel keyed on the same context_cancelled/1h series
	// the alert thresholds on. Both must exist so the operator can read the
	// current value AND the trend-vs-threshold.
	const wantExpr = `gardenio_agent_involuntary_deaths_total{env=~"$env",project=~"$project",family="context_cancelled"}[1h]`
	var stat, ts bool
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			if !strings.Contains(tg.Expr, wantExpr) {
				continue
			}
			switch p.Type {
			case "stat":
				stat = true
			case "timeseries":
				ts = true
			}
		}
	}
	if !stat {
		t.Errorf("involuntary-deaths board must have a node-reclaim STAT panel keyed on the context_cancelled/1h series")
	}
	if !ts {
		t.Errorf("involuntary-deaths board must have a node-reclaim TIMESERIES panel keyed on the context_cancelled/1h series")
	}
}

// C9 (ka-0000001146): the board must carry the unclaimed-mid-turn-kill panels —
// the context_cancelled_unclaimed family that counts SIGTERM'd busy-but-taskless
// agents (previously invisible: plain context_cancelled never reached the deaths
// counter). A stat (24h headline) plus a by-role timeseries (who the invisible
// kills were hitting) so the new family is legible without touching the
// node-reclaim sentinel's claimed-issue precision.
func TestInvoluntaryDeathsDashboard_HasUnclaimedKillPanels(t *testing.T) {
	t.Parallel()
	d := parseInvoluntaryDeathsDashboard(t)

	const familySel = `family="context_cancelled_unclaimed"`
	var stat, ts bool
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			if !strings.Contains(tg.Expr, familySel) {
				continue
			}
			switch p.Type {
			case "stat":
				stat = true
			case "timeseries":
				ts = true
			}
		}
	}
	if !stat {
		t.Errorf("involuntary-deaths board must have an unclaimed-kill STAT panel keyed on the context_cancelled_unclaimed family")
	}
	if !ts {
		t.Errorf("involuntary-deaths board must have an unclaimed-kill TIMESERIES panel keyed on the context_cancelled_unclaimed family")
	}
}

// Track D (ka-0000001509): the board must carry the OOMKill panels — the panels
// that visualize exactly what the AgentOOMKilled alert fires on (the
// oom_killed lifecycle series, per hour) — so an operator following the alert's
// dashboard deep-link lands on a board that shows the firing signal.
func TestInvoluntaryDeathsDashboard_HasOOMKillPanels(t *testing.T) {
	t.Parallel()
	d := parseInvoluntaryDeathsDashboard(t)

	const wantSel = `gardenio_agent_lifecycle_events_total{env=~"$env",project=~"$project",event="oom_killed"}`
	var stat, ts bool
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			if !strings.Contains(tg.Expr, wantSel) {
				continue
			}
			switch p.Type {
			case "stat":
				stat = true
			case "timeseries":
				ts = true
			}
		}
	}
	if !stat {
		t.Errorf("involuntary-deaths board must have an OOMKill STAT panel keyed on the oom_killed lifecycle series")
	}
	if !ts {
		t.Errorf("involuntary-deaths board must have an OOMKill TIMESERIES panel keyed on the oom_killed lifecycle series")
	}
}

// Track D (ka-0000001509): the board must carry the node-lifecycle-churn panels
// — what the AgentNodeLifecycleEvents alert fires on (the new
// gardenio_node_lifecycle_events_total counter) — so the node-lifecycle alert's
// dashboard deep-link lands on a board that shows the firing signal.
func TestInvoluntaryDeathsDashboard_HasNodeLifecyclePanels(t *testing.T) {
	t.Parallel()
	d := parseInvoluntaryDeathsDashboard(t)

	const wantMetric = "gardenio_node_lifecycle_events_total"
	var stat, ts bool
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			if !strings.Contains(tg.Expr, wantMetric) {
				continue
			}
			switch p.Type {
			case "stat":
				stat = true
			case "timeseries":
				ts = true
			}
		}
	}
	if !stat {
		t.Errorf("involuntary-deaths board must have a node-lifecycle STAT panel keyed on gardenio_node_lifecycle_events_total")
	}
	if !ts {
		t.Errorf("involuntary-deaths board must have a node-lifecycle TIMESERIES panel keyed on gardenio_node_lifecycle_events_total")
	}
}

// gd-bUyK5hRdNU (supersedes ka-0000005155): the earlier fix appended `or
// vector(0)` to every stat-panel expr so an un-emitted counter would render a
// real 0 instead of a blank void. But `gardenio_agent_involuntary_deaths_total`
// is created LAZILY on the first involuntary death and is reset to non-existent
// on controller restart — after release 2026.241.23 restarted the controller,
// `sum(increase(...[window]))` returned EMPTY, and `or vector(0)` painted that
// stale-empty as a confident GREEN 0 while the same board's donut showed 2,283
// deaths. A stale counter must read as STALE (neutral/grey), not as a healthy 0.
//
// The fix drops `or vector(0)` on every liveness stat panel and instead makes an
// ABSENT series render neutral: an explicit `noValue` staleness message plus a
// special "null"-match value mapping to a non-green colour, plus a base
// threshold step (value: null) that is NOT green — so green is reached only at a
// real, live 0 (a green step whose value is 0), never by a missing series. These
// assertions guard that contract PER-PANEL so a revert to `or vector(0)`, or a
// green base/no-data step, reds here.
func TestInvoluntaryDeathsDashboard_StatPanelsRenderStaleNotGreenZero(t *testing.T) {
	t.Parallel()
	d := parseInvoluntaryDeathsDashboard(t)

	var statPanels int
	for _, p := range d.Panels {
		if p.Type != "stat" {
			continue
		}
		statPanels++
		if len(p.Targets) == 0 {
			t.Errorf("stat panel %q has no targets", p.Title)
			continue
		}

		// 1. The expr must NOT coalesce an empty result to 0 — that is exactly
		//    what masked a stale counter as a green 0 (gd-bUyK5hRdNU).
		for _, tg := range p.Targets {
			expr := strings.TrimSpace(tg.Expr)
			if strings.HasSuffix(expr, "or vector(0)") {
				t.Errorf("stat panel %q expr must NOT end with `or vector(0)`: coalescing a stale/absent counter to 0 paints it a confident green 0 (gd-bUyK5hRdNU); got %q", p.Title, expr)
			}
		}

		fc := p.FieldConfig.Defaults

		// 2. An absent series must display an explicit staleness message, not a
		//    blank headline (the concern the original `or vector(0)` addressed).
		if strings.TrimSpace(fc.NoValue) == "" {
			t.Errorf("stat panel %q must set fieldConfig.defaults.noValue (a staleness message) so an un-emitting counter reads stale, not blank", p.Title)
		}

		// 3. A special "null"-match mapping must recolour an absent series to a
		//    NON-green colour, so no-data can never render as a healthy 0.
		var nullMapped bool
		for _, m := range fc.Mappings {
			if m.Type == "special" && m.Options.Match == "null" {
				nullMapped = true
				if isGreen(m.Options.Result.Color) {
					t.Errorf("stat panel %q null-match mapping must NOT colour absent data green (that is the false-green-0 bug); got %q", p.Title, m.Options.Result.Color)
				}
			}
		}
		if !nullMapped {
			t.Errorf("stat panel %q must map a special \"null\" match to a neutral colour so a stale counter renders grey, not green", p.Title)
		}

		// 4. The base threshold step (value: null, i.e. everything below the
		//    first numeric step) must NOT be green — green may begin only at a
		//    real, live 0. A green base step repaints no-data green even after
		//    `or vector(0)` is removed.
		steps := fc.Thresholds.Steps
		if len(steps) == 0 {
			t.Errorf("stat panel %q must define threshold steps", p.Title)
			continue
		}
		if string(steps[0].Value) != "null" {
			t.Errorf("stat panel %q first threshold step must be the value:null base step; got value %s", p.Title, steps[0].Value)
		}
		if isGreen(steps[0].Color) {
			t.Errorf("stat panel %q base (value:null) threshold step must NOT be green — a missing series would repaint green; green must start at a real 0 (a step with value 0); got %q", p.Title, steps[0].Color)
		}
		var greenAtZero bool
		for _, s := range steps {
			if isGreen(s.Color) && string(s.Value) == "0" {
				greenAtZero = true
			}
		}
		if !greenAtZero {
			t.Errorf("stat panel %q must have a green threshold step at value 0 so a genuine live 0 still reads healthy; steps=%+v", p.Title, steps)
		}
	}

	// Guard the guard: the loop above is vacuously green if the board has no
	// stat panels. The board ships five liveness headlines (24h deaths,
	// node-reclaim, unclaimed kills, OOMKills, node-gone).
	if statPanels < 5 {
		t.Errorf("involuntary-deaths board must have >=5 stat panels to guard staleness rendering on; found %d", statPanels)
	}
}

// isGreen reports whether a Grafana colour token renders as the green/healthy
// hue — either the named "green" or a "green"-prefixed shade (e.g. "dark-green",
// "semi-dark-green", "light-green").
func isGreen(color string) bool {
	c := strings.ToLower(strings.TrimSpace(color))
	return c == "green" || strings.Contains(c, "green")
}

// WS4 (ka-0000007155): the board cross-links to the resume-latency board so the
// operator can pivot from "an interruption happened" to "was the resume fast"
// in one click — the two halves of the same interruption story.
func TestInvoluntaryDeathsDashboard_CrossLinksResumeLatency(t *testing.T) {
	t.Parallel()
	d := parseInvoluntaryDeathsDashboard(t)
	var found bool
	for _, l := range d.Links {
		if strings.Contains(l.URL, "/d/gardenio-resume-latency/") {
			found = true
		}
	}
	if !found {
		t.Errorf("involuntary-deaths board must cross-link to the resume-latency board; got links %+v", d.Links)
	}
}
