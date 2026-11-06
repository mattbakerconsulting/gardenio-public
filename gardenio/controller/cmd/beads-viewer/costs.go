package main

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/grafanaembed"
)

// costsAccountLimit caps the daemon list call. iOS UsageMonitor uses 50, which
// matches the practical fleet size; if it ever grows past this the page will
// silently drop the tail — fine for a summary view.
const costsAccountLimit = 50

// accountBillingSubscription / accountBillingMetered mirror the controller's
// billing_type values (ratelimit_probe.go billingTypeSubscription/Metered). A
// subscription account's headline bills against its rolling 5h/7d subscription
// windows; a metered account keeps the calendar-day presentation, which is
// correct for pay-as-you-go (ka-0000003343). Distinct binaries so the constants
// cannot be imported — a rename there must be mirrored here.
const (
	accountBillingSubscription = "subscription"
	accountBillingMetered      = "metered"
)

// representativeClaimSevenDay is the representative_claim value marking the 7d
// window as the binding constraint (else five_hour binds). Mirrors the probe
// header value (ratelimit_probe.go) and the spread selector's binding-window
// pick (account_spread.go:404-410).
const representativeClaimSevenDay = "seven_day"

// costsDashboardUID is the Grafana uid of the accepted /costs candidate
// dashboard (type=dashboard bead ka-0000005371, dashboard_uid=
// gardenio-costs-candidate). The operator ACCEPTED it (decision ka-0000007043),
// so the two inline ECharts cards that used to live on /costs are now embedded
// Grafana solo panels instead (epic ka-0000006518, task ka-0000002791), joined
// by a third "spend by account" panel (ka-0000004020). grafana-bridge keeps the
// dashboard in sync from the bead model (generated from the kbeads chart
// catalog via `kd admin gen-dashboards`, epic ka-0000002763); this page only
// embeds it. Charset-validated by grafanaembed.HTML before it goes into an
// iframe URL.
const costsDashboardUID = "gardenio-costs-candidate"

// NOTE (ka-0000002699): the cross-dashboard "Settled cost — rollup by role" embed
// that used to live here — sourced from the total-cost rollup dashboard
// (uid gb-cost-per-bead-rollup-preview) panel 5 over the at-close per-agent counter
// — was removed. That counter went dead in prod and rendered ~1000x below the real
// bill (the /costs "Settled cost" P0), and the native on-page Cost explorer
// (costs_rollup.go) now rolls up by role/project/model/account/agent on real-bill
// gauges, so the embed was both wrong and redundant. Its consts
// (costsRollupDashboardUID, costsRollupPanelID) and default-vars helper were
// dropped with it; the dashboard bead + the kbeads catalog entries that still ride
// the counter are retired separately in follow-up ka-0000005951.

// costsEmbedHeight is the per-panel iframe height (px). 360 is the
// Playwright-verified scroll-free height the proof bundle (ka-0000005570) used:
// each solo panel renders at scrollHeight==clientHeight==360 with no inner
// Grafana scrollbar. Kept in sync with the bundle doc's
// `:::grafana gardenio-costs-candidate panel=N height=360` embeds. The scroll
// fix itself (iframe scrolling=no + .grafana-embed-frame{overflow:hidden})
// lives in grafanaembed.HTML/layout.html (MR !4566) — reusing that builder here
// means this surface can't regress it.
const costsEmbedHeight = "360"

// costsGrafanaPanel is one Grafana solo-panel embed on /costs. PanelID is the
// Grafana panelId (matching the source dashboard); Title/Scope/Note reproduce
// the headings the former inline cards carried so the page reads consistently
// even while the iframe loads.
//
// DashboardUID overrides the source dashboard for this panel; when empty the
// panel comes from costsDashboardUID (the accepted /costs candidate). Vars are
// template-variable assignments passed through to the embed (e.g. var-rollup);
// they let one panel be scoped to a default view while its pop-out stays
// interactive.
type costsGrafanaPanel struct {
	PanelID      string
	Title        string
	Scope        string
	Note         string
	DashboardUID string // "" => costsDashboardUID
	Vars         []grafanaembed.Var
	// TrustPair names the x-check pair whose agreement score qualifies this
	// panel's numbers (costs_trust.go, ka-0000008212). The card head renders
	// that pair's badge next to the title; empty = no badge.
	TrustPair string
}

// costsGrafanaPanels returns the Grafana solo panels shown on /costs, in render
// order. Panels 1–4 mirror the accepted dashboard (gardenio-costs-candidate) and
// the proof bundle's `:::grafana gardenio-costs-candidate panel=N height=360`
// embeds; their ids match the dashboard spec (dashboardCatalog() in kbeads
// charts_grafana_dashboard.go):
//
//	1 cost-per-agent-hour    — Cost per agent-hour (USD/hour/agent, 1h + 24h rolling + agents, ka-0000006587)
//	2 fleet-spend-rate       — Fleet spend rate (USD/hour, 24h + 1h rolling + cost/agent 24h + 1h + active/1h/24h agents)
//	3 agent-spend-over-time  — Per-agent cost over time (top agents + other)
//	4 spend-by-account       — Spend by account over time (USD/hour, 1h rolling, top accounts + other, ka-0000004020 / ka-0000007972)
//
// PANEL IDS ARE POSITIONAL. AssembleDashboard assigns them as CatalogIDs index+1,
// so inserting cost-per-agent-hour at the FRONT of that slice (ka-0000006587)
// shifted every later panel's id by one — these embeds were renumbered in the same
// change. If you reorder CatalogIDs, renumber here or every card renders the wrong
// panel; TestAssembleDashboard_CostsCandidate pins the dashboard-side order.
//
// The former panel 4 (spend-by-model iframe) was REMOVED (ka-0000006501): its
// host dashboard gardenio-costs-candidate has no committed chart bundle
// (grafana_configmap_test.go asserts it is bead-only) so it was never
// provisioned as a live 4-panel dashboard — the panelId=4 iframe rendered
// broken. And its source gardenio_agent_spend_usd_total resolved the `model`
// label from the account default, never the effective model, so even a live
// panel collapsed to ~1 series. The on-page "By model" section
// (buildCostsModelRollups) and the cost explorer by-model rollup
// (costs_rollup.go) both read the real-bill gardenio_tag_cost_usd{dim="model"}
// instead and give the correct per-effective-model split.
//
// The former 5th panel — a cross-dashboard "Settled cost — rollup by role" embed
// over the at-close per-agent counter — was removed in ka-0000002699 (dead counter,
// ~1000x low; superseded by the native real-bill Cost explorer). See the NOTE above.
func costsGrafanaPanels() []costsGrafanaPanel {
	return []costsGrafanaPanel{
		{
			PanelID:   "1",
			Title:     "Cost per agent-hour",
			Scope:     "USD/hour per producing agent · 1h + 24h rolling · agents (1h avg)",
			Note:      "Unit economic: fleet spend rate ÷ average concurrent producing agents. Flat while the fleet scales = pure scale-out; rising = per-agent cost regression. Numerator is the whole credential bill (incl. non-agent traffic), so read it as an upper bound.",
			TrustPair: costsTrustPairBillConservation,
		},
		{
			PanelID:   "2",
			Title:     "Fleet spend rate",
			Scope:     "USD/hour · 24h + 1h rolling · cost per agent (24h + 1h) · agents (live, 1h, 24h) · LiteLLM bill",
			TrustPair: costsTrustPairBillConservation,
		},
		{
			PanelID:   "3",
			Title:     "Per-agent cost over time",
			Scope:     "USD per bucket · top agents + other · stacked",
			Note:      "Per-agent reconciled spend. Fleet rate above is total credential billing (incl. non-agent/test traffic), so it runs a bit higher.",
			TrustPair: costsTrustPairAgentSpendParity,
		},
		{
			PanelID:   "4",
			Title:     "Spend by account over time",
			Scope:     "USD/hour · 1h rolling · top accounts + other · stacked",
			Note:      "Per-credential-account 1h spend rate from the LiteLLM account-cost gauge — same source and rate basis as the fleet 1h line, so this stack reconciles with it.",
			TrustPair: costsTrustPairBillConservation,
		},
		// Panels 4 and 5 were both removed. Panel 4 (spend-by-model iframe) went in
		// ka-0000006501 — see the NOTE above; the on-page "By model" section reads the
		// real-bill tag gauge instead. Panel 5 ("Settled cost — rollup by role") went
		// in ka-0000002699: it embedded the total-cost rollup dashboard over the
		// at-close counter gardenio_agent_spend_usd_total, which went dead in prod (0
		// live series) and rendered ~1000x below the real bill (the /costs "Settled
		// cost" P0). Its costsRollup{PanelID,DashboardUID} consts + default-vars helper
		// were dropped with it. Both are superseded by the native real-bill Cost
		// explorer (costs_rollup.go), which rolls up every dimension on real-bill
		// gauges. The rollup dashboard bead + kbeads catalog entries are retired in
		// follow-up ka-0000005951.
	}
}

// costsRenderedPanel pairs a panel's heading metadata with its pre-rendered
// embed HTML (iframe, or the degraded link card when Grafana is unconfigured).
type costsRenderedPanel struct {
	Title string
	Scope string
	Note  string
	Embed template.HTML
	// TrustPair carries the qualifying x-check pair name through to the
	// template, which resolves it to a badge via Trust.Badge (costs_trust.go).
	TrustPair string
}

// renderCostsChartPanels builds the embed HTML for each /costs chart card via
// the shared grafanaembed.HTML helper — the same scroll-fixed builder the
// `:::grafana` directive and /charts use — so the page and bead-level embeds
// stay byte-identical and the scroll fix (MR !4566) can't drift. base is the
// viewer's configured Grafana base URL (Server.grafanaBaseURL); when empty,
// grafanaembed.HTML emits its degraded link-card fallback.
func renderCostsChartPanels(base string) []costsRenderedPanel {
	panels := costsGrafanaPanels()
	out := make([]costsRenderedPanel, 0, len(panels))
	for _, p := range panels {
		uid := p.DashboardUID
		if uid == "" {
			uid = costsDashboardUID
		}
		embed := grafanaembed.HTML(base, uid, grafanaembed.Opts{
			PanelID: p.PanelID,
			Height:  costsEmbedHeight,
			Vars:    p.Vars,
		})
		out = append(out, costsRenderedPanel{
			Title: p.Title,
			Scope: p.Scope,
			Note:  p.Note,
			// embed is built from the operator-set base + a const uid + a const
			// numeric panel id + const template vars, and HTML-escaped inside
			// grafanaembed.HTML; nothing here is author-supplied.
			Embed:     template.HTML(embed), //nolint:gosec // see above — no untrusted input.
			TrustPair: p.TrustPair,
		})
	}
	return out
}

// costsScaffoldNames are placeholder labels rendered when /costs has zero
// accounts. They give the empty viewport a planned-shape preview so users
// see what the page will look like once account beads are synced.
var costsScaffoldNames = []string{
	"anthropic-prod-01",
	"anthropic-prod-02",
	"anthropic-prod-03",
}

// costAccountView is one card on /costs.
type costAccountView struct {
	ID            string
	Name          string
	Provider      string
	Model         string
	Status        string // active / exhausted / etc.
	IsActive      bool
	IsThrottled   bool
	ThrottleLabel string // e.g. "throttled", "rejected", "auth-dead"
	ProbeAgo      string

	// IsAuthDead mirrors probe_auth_status=="rejected", set by the controller
	// probe on a hard 401/403. That path early-returns WITHOUT refreshing
	// throttle_status / utilization_* / throttle_reset_at, so those fields
	// freeze at their last-good values and keep advertising headroom forever —
	// an auth-dead account otherwise renders as a healthy card and is skipped
	// by the Throttled panel entirely (it is never throttle_status=="rejected").
	// Observed live: matthewbaker1110 showing "allowed" with 73% of its 5h
	// window free while 403ing every request for a week.
	IsAuthDead bool

	// Utilization (0.0–1.0) shown as bars. Zero values render the bar muted.
	Util5h           float64
	Util5hPct        int
	Util5hHas        bool
	Util7d           float64
	Util7dPct        int
	Util7dHas        bool
	ThrottleReset    string // formatted countdown for the binding window, "" when not applicable
	Reset5hCountdown string // formatted countdown derived from throttle_reset_5h_at
	Reset7dCountdown string // formatted countdown derived from throttle_reset_7d_at

	// Overage warning — shown when overage_status is set and not "enabled".
	OverageStatus string
	OverageReason string
	HasOverage    bool

	// Spend.
	DailyCostUSD    float64
	WeeklyCostUSD   float64
	TotalCostUSD    float64
	SinceResetSpend float64
	HasSpend        bool

	// Billing-period budget (gd-iomVjSovXs, gd-594kYy1IYo). BudgetUSD is
	// spend_limit_usd (LiteLLM's max_budget, synced by spend_sync), else the
	// operator-set max_budget hint, the precedence trigger_dark_lane.go uses;
	// BudgetHas is false when neither is above 0. PeriodSpendUSD is
	// total_cost_usd - spend_at_last_reset floored at 0, a missing baseline
	// counting as 0 as accountOverBudget (podspec.go) does, so a never-reset
	// account is not read as unspent (SinceResetSpend stays 0 then).
	// RemainingUSD is BudgetUSD - PeriodSpendUSD floored at 0. Only meaningful
	// when BudgetHas.
	BudgetUSD      float64
	BudgetHas      bool
	PeriodSpendUSD float64
	RemainingUSD   float64

	// Subscription-window spend (ka-0000003343). Bill5hUSD / Bill7dUSD are the
	// dollars accrued since the current 5h / 7d subscription window opened
	// (throttle_reset_*_at − window), written by spend_sync against the SAME
	// /spend/logs rows daily_cost_usd comes from but re-windowed onto the
	// subscription's clock. Has* is false when the field is absent (metered
	// account, or a probe that has not written the reset instant yet) — the
	// headline then falls back to the calendar-day figure rather than showing $0.
	BillingType   string // "subscription" (default) | "metered"
	Bill5hUSD     float64
	Bill5hHas     bool
	Bill7dUSD     float64
	Bill7dHas     bool
	// RepresentativeClaim ("five_hour" | "seven_day") is the BINDING window the
	// probe reports — which of the two figures is the primary headline
	// (account_spread.go:404-410). Empty defaults to five_hour.
	RepresentativeClaim string

	// LiteLLMKey is the account bead's litellm_key. spend_sync stamps the same
	// per-key /key/info total onto EVERY account bead sharing a key, so this is
	// the dedup basis for booking a shared bill once fleet-wide (ka-0000003446).
	// Empty for single-key accounts, which are never deduped.
	LiteLLMKey string
	// CostDuplicate is set by markCostDuplicates when another account bead is the
	// deterministic owner of this shared key's cost. When true, every fleet-wide
	// money aggregation (summary totals + provider/model/top-spender rollups)
	// skips this view's dollars, so a shared key contributes its bill exactly
	// once. The per-account CARD still shows this bead's real cost, and
	// session/util/status counts stay per-account — only money is booked once.
	CostDuplicate bool

	// Usage.
	SessionCount      int
	TotalOutputTokens int
	HasUsage          bool
}

// CostPerSession returns the lifetime average cost per session
// (totalCostUSD / sessionCount) when both are nonzero, else 0. Both inputs are
// lifetime counters on the account bead so the windows match. The prior
// behaviour divided the 1-day daily cost by the lifetime session count, which
// yielded a meaningless unit that shrank over time regardless of real
// per-session cost (audit F6, ka-0000006998).
func (v costAccountView) CostPerSession() float64 {
	if v.SessionCount <= 0 {
		return 0
	}
	return v.TotalCostUSD / float64(v.SessionCount)
}

// costsSummaryView is the top-of-page summary card.
type costsSummaryView struct {
	GrandTotalUSD float64 // sum of dailyCostUSD across accounts
	// HourlyRateUSD is the fleet spend rate in USD/hour, sourced from the SAME
	// rolling-window PromQL the /costs chart renders (costs_rate.go,
	// ka-0000002806) — NOT GrandTotal/hours-since-midnight, which was an
	// average-over-the-day-so-far that read absurdly high on a quiet night.
	// Only meaningful when HourlyRateAvailable is true.
	HourlyRateUSD float64
	// HourlyRateAvailable is true when vmsingle returned a usable rolling-rate
	// sample. False (local dev, no upstream, or the spend counter absent) →
	// the surfaces render "—", matching the chart's own empty state, rather
	// than a fabricated average.
	HourlyRateAvailable bool
	// HourlyRate1hUSD is the 1-hour moving-average fleet spend rate in USD/hour
	// (ka-0000001327) — the load-reactive companion to the 24h HourlyRateUSD
	// headline. The 24h window smooths variable load by construction; the 1h
	// window surfaces the hour-to-hour swings it hides. Only meaningful when
	// HourlyRate1hAvailable is true.
	HourlyRate1hUSD float64
	// HourlyRate1hAvailable is true when vmsingle returned a usable 1h-window
	// sample. Independent of HourlyRateAvailable so a miss on one window does
	// not blank the other.
	HourlyRate1hAvailable bool
	WeeklyTotalUSD        float64
	TotalCostAllUSD       float64 // sum of totalCostUSD across accounts (lifetime)
	ActiveCount           int
	ThrottledCount        int
	OverageCount          int
	TotalCount            int
	WithSpendCount        int     // accounts with any spend recorded
	TotalSessions         int     // sum of sessionCount across accounts
	AvgPerActiveUSD       float64 // grandTotal / activeCount (0 when 0 active)
	CostPerSessionUSD     float64 // lifetime cost / totalSessions (0 when 0 sessions)
	ProjectedTodayUSD     float64 // rolling hourlyRate * 24 — naive day projection (0 when rate unavailable)
	DayProgressPct        int     // 0..100, fraction of UTC day elapsed
	// MTDProjectionUSD is NOT a true month-to-date sum — we have no per-day
	// cost history. It is a projection that assumes spend ran at the 7-day
	// average daily rate (WeeklyTotalUSD / 7) for every elapsed day this month:
	// dailyAvg * dayOfMonth. It diverges from the real MTD whenever spend is
	// non-uniform (e.g. a spike outside the 7d window is invisible). Labeled
	// honestly as a 7d-avg projection on the surface (audit F7, ka-0000006998).
	MTDProjectionUSD  float64
	ProjectedMonthUSD float64 // dailyAvg * daysInMonth — same 7d-avg basis
	MonthProgressPct  int     // 0..100, fraction of month elapsed
	MonthDayOfMonth   int     // 1..31
	MonthDaysInMonth  int     // 28..31
	// MaxUtil5hPct surfaces the hottest 5h utilization across the fleet —
	// a single-glance answer to "are any accounts near saturation?"
	MaxUtil5hPct int
	MaxUtil7dPct int
	LastUpdated  string // formatted "HH:MM" time of render

	// Subscription headline (ka-0000003343). The fleet is almost entirely Claude
	// subscription accounts whose real constraint is quota-against-a-rolling-
	// window, so the headline bills against the two subscription windows Matt
	// asked for — the 5h limit and the week — instead of the 00:00-UTC calendar
	// day (which reset on a clock unrelated to the subscription). Bill5hUSD /
	// Bill7dUSD are the fleet sums of the per-account window spend; the *Has
	// flags gate rendering so an all-metered fleet (or a fleet whose probes have
	// not written a reset instant) never shows an empty subscription headline.
	Bill5hUSD float64
	Bill5hHas bool
	Bill7dUSD float64
	Bill7dHas bool
	// PrimaryBindsSevenDay chooses which window is THE headline number: the
	// binding window per representative_claim, decided fleet-wide by whether any
	// subscription account reports seven_day as binding (account_spread.go:404).
	PrimaryBindsSevenDay bool
	// SubscriptionCount is how many contributing accounts are subscription-billed
	// — gates the whole subscription headline block (0 → metered-only fleet keeps
	// the calendar-day "Today" headline).
	SubscriptionCount int

	// Measure-twice cross-check (advice ka-0000005924, epic's "Reconciled"
	// rubric). The 5h and 7d window bills are summed from the SAME /spend/logs
	// stream via the SAME window function, differing only in window start, so the
	// 5h window is a strict sub-interval of the 7d window (both end at now) and
	// spend_5h_window_usd ≤ spend_7d_window_usd MUST hold. A violation means one
	// windowed sum is lying — most often a STALE throttle_reset_7d_at that opened
	// the 7d window LATER than the 5h window. BillXCheckAccounts counts accounts
	// carrying both figures (the checkable population); BillXCheckViolations
	// counts those where 5h exceeds 7d beyond tolerance. Surfaced on the page
	// rather than silently picking a winner.
	BillXCheckAccounts   int
	BillXCheckViolations int

	// Account budgets (gd-594kYy1IYo): the budgeted accounts' BudgetUSD,
	// PeriodSpendUSD and RemainingUSD summed, a shared key's booked once. The
	// /garden purse shows the same figures (farmSpendSource.FleetBudgetUSD).
	// BudgetCount is how many accounts have a budget; 0 means no budget, which
	// a surface shows as a dash, never "$0 left".
	BudgetCount    int
	BudgetUSD      float64
	BudgetSpentUSD float64
	RemainingUSD   float64
}

// billWindowXCheckTolUSD is the slack allowed on the 5h ≤ 7d containment check
// before an account is counted a violation — absorbs float rounding and a
// sub-cent settlement row landing in one window's fetch but not the other's.
const billWindowXCheckTolUSD = 0.01

// PrimaryBillUSD / SecondaryBillUSD return the binding and non-binding
// subscription-window bill for the headline. Primary is the window
// representative_claim marks as binding (seven_day → the week; else the 5h
// limit); secondary is the other. Callers gate on Bill5hHas / Bill7dHas.
func (s costsSummaryView) PrimaryBillUSD() float64 {
	if s.PrimaryBindsSevenDay {
		return s.Bill7dUSD
	}
	return s.Bill5hUSD
}

func (s costsSummaryView) SecondaryBillUSD() float64 {
	if s.PrimaryBindsSevenDay {
		return s.Bill5hUSD
	}
	return s.Bill7dUSD
}

// SecondaryBillHas reports whether the non-binding window actually has a fleet
// figure, so the template simply omits a window with no data rather than
// painting "$0.00" that reads as "nothing spent" when it means "not measured".
func (s costsSummaryView) SecondaryBillHas() bool {
	if s.PrimaryBindsSevenDay {
		return s.Bill5hHas
	}
	return s.Bill7dHas
}

// PrimaryBillLabel / SecondaryBillLabel name the headline windows so the label
// always states which window the dollar figure is billed against (the epic's
// "Intelligible" rubric: the number must mean what the operator thinks).
func (s costsSummaryView) PrimaryBillLabel() string {
	if s.PrimaryBindsSevenDay {
		return "bill vs week (7d limit)"
	}
	return "bill vs 5h limit"
}

func (s costsSummaryView) SecondaryBillLabel() string {
	if s.PrimaryBindsSevenDay {
		return "bill vs 5h limit"
	}
	return "bill vs week (7d limit)"
}

// costsProviderRollup is one row in the "By provider" section. Lets the
// viewer answer "where is the spend going?" at a glance without scanning
// every account card.
type costsProviderRollup struct {
	Provider       string
	AccountCount   int
	DailyUSD       float64
	WeeklyUSD      float64
	TotalUSD       float64
	SharePct       int     // share of GrandTotal (0..100), 0 when GrandTotal=0
	AvgUtil7d      float64 // mean utilization_7d across accounts that report it
	AvgUtil7dPct   int
	AvgUtil7dHas   bool
	ThrottledCount int
}

// costsTopSpender is one row in the top-N spenders mini-list.
type costsTopSpender struct {
	ID          string
	Name        string
	Provider    string
	DailyUSD    string // pre-formatted "$12.34"
	WeeklyUSD   string
	SharePct    int // share of GrandTotal (0..100)
	IsThrottled bool
}

// costsThrottledView surfaces throttled accounts with their reset windows.
// Lets operators see "who's hot right now" without scanning every card.
type costsThrottledView struct {
	ID               string
	Name             string
	Provider         string
	ThrottleLabel    string
	ThrottleReset    string
	Util5hPct        int
	Util5hHas        bool
	Util7dPct        int
	Util7dHas        bool
	Reset5hCountdown string
	Reset7dCountdown string
}

// costsModelRollup is one row in the "By model" rollup, bucketed by the
// EFFECTIVE model that served the spend. Lets operators see "where is opus
// spend going vs haiku" at a glance.
//
// Source (ka-0000006501): the real-bill per-dimension tag gauge
// gardenio_tag_cost_usd{dim="model"} — NOT the account bead's optional `model`
// field. That field was only a per-account seed (role/project/agent overrides
// mean the effective model differs, podspec.go), it was unset on most accounts
// (config.go schema has no required:true), and shared-litellm_key dedup zeroed
// whole rows — so the old account-field rollup collapsed to a single "unknown"
// bucket on prod. The tag gauge is fed by the ALREADY-CORRECTED LiteLLM `model:`
// tag (retagLiteLLMModel), so a haiku role riding an opus-default account lands
// in the haiku bucket, and there is no "unknown" bucket. It is the same source
// the working cost explorer by-model rollup uses (costs_rollup.go).
//
// DailyUSD/WeeklyUSD are 24h/7d rolling increases of the cumulative gauge;
// TotalUSD is its current (lifetime-cumulative) value. SharePct is the row's
// share of the visible model total (self-relative, always ≤100) — distinct from
// the provider rollup's share-of-GrandTotal, because this rollup is sourced from
// the LiteLLM bill rather than the account-bead daily fields.
type costsModelRollup struct {
	Model     string
	DailyUSD  float64
	WeeklyUSD float64
	TotalUSD  float64
	SharePct  int // share of the visible model total (0..100)
}

// costsUtilizationView is one row in the "Hottest utilization" mini-list —
// accounts sorted by 5h util descending. Distinct from "Top spenders" because
// utilization is what predicts an imminent throttle, not raw $ spent.
type costsUtilizationView struct {
	ID               string
	Name             string
	Provider         string
	Util5hPct        int
	Util5hHas        bool
	Util7dPct        int
	Util7dHas        bool
	Reset5hCountdown string
	Reset7dCountdown string
	IsThrottled      bool
}

// handleCosts renders /costs — fleet-wide cost summary + per-account cards.
// Mirrors iOS CostsTab. Source data is account beads; cost fields are kept in
// sync from LiteLLM by the controller's spend_sync daemon.
func (s *Server) handleCosts(w http.ResponseWriter, r *http.Request) {
	// UTC, not local: this now feeds the calendar-day and calendar-month
	// progress math in buildCostsSummary (DayProgressPct, MonthProgressPct /
	// MonthDayOfMonth), whose numerators — daily_cost_usd / weekly are bucketed
	// to 00:00 UTC by the controller's spend_sync (spend_sync.go:124-125). A
	// local now against a UTC-bucketed numerator makes the "% of day elapsed"
	// caption disagree with the spend it paces the moment a non-UTC TZ is set on
	// the pod (ka-0000004628). Durations (fmtResetCountdown / fmtTimeAgo) are
	// t.Sub(now) instant differences and are location-independent regardless.
	now := time.Now().UTC()
	// Trust strip (C6, ka-0000008212): per-pair x-check corroboration badges
	// from gardenio_xcheck_agreement, fetched before the account list so the
	// strip renders even on the account-load error path — trust context matters
	// most when other data is misbehaving. Unavailability degrades to
	// all-"unverified" badges (never green) inside buildCostsTrust.
	trustScores, trustQueried := s.fetchCostsTrustScores(r.Context())
	data := map[string]any{
		"Summary":       costsSummaryView{LastUpdated: now.Format("15:04")},
		"Accounts":      []costAccountView{},
		"ScaffoldNames": costsScaffoldNames,
		"Trust":         buildCostsTrust(trustScores, trustQueried, s.grafanaBaseURL),
		// ChartPanels are the embedded Grafana solo panels (fleet-spend-rate,
		// per-agent cost over time, and spend-by-account) that replaced the former
		// inline ECharts cards on operator acceptance (decision ka-0000007043, task
		// ka-0000002791) plus the per-account breakdown (ka-0000004020). Rendered
		// unconditionally — even on the account-list error path — so the cost
		// charts stay visible regardless of account load. GrafanaBaseURL gates the
		// header "Open in Grafana ↗" link.
		"ChartPanels":    renderCostsChartPanels(s.grafanaBaseURL),
		"GrafanaBaseURL": s.grafanaBaseURL,
	}

	accounts, res, err := s.costAccounts(r.Context(), now)
	if err != nil {
		s.logger.Error("listing account beads", "error", err)
		data["Error"] = "Failed to load accounts."
		s.render(w, r, "costs.html", data)
		return
	}

	summary := buildCostsSummary(accounts, now)
	// Headline spend rate comes from the rolling-window PromQL that backs the
	// same-page chart (ka-0000002806), so the two agree within rounding. On
	// unavailability the rate stays zero / HourlyRateAvailable false and the
	// template renders "—".
	s.applyFleetHourlyRate(r.Context(), &summary)
	data["Accounts"] = accounts
	data["Summary"] = summary
	data["Providers"] = buildCostsProviderRollups(accounts, summary.GrandTotalUSD)
	// "By model" is sourced from the real-bill tag gauge (effective model), not
	// the account bead's optional/uncorrected `model` field (ka-0000006501). It
	// runs its own instant queries against vmsingle, so it is nil (section
	// hidden) when no metrics upstream is wired.
	data["Models"] = s.buildCostsModelRollups(r.Context())
	data["TopSpenders"] = buildCostsTopSpenders(accounts, summary.GrandTotalUSD, 5)
	data["Hottest"] = buildCostsHottestUtilization(accounts, 5)
	data["Throttled"] = buildCostsThrottled(accounts)
	// Truncation is a daemon-side concept — did the account listing hit the
	// costsAccountLimit cap. Compare against the RAW returned bead count, not the
	// post-filter card count, so dropping E2E accounts (ka-0000005167) can never
	// spuriously trip the "truncated" banner.
	data["Truncated"] = res.Total > len(res.Beads)
	data["Limit"] = costsAccountLimit
	s.render(w, r, "costs.html", data)
}

// costAccounts lists the account beads /costs reads, as views: open and
// in-progress only (match iOS UsageMonitor: closed accounts are historical
// and would skew totals), E2E accounts dropped, sorted by name, and shared-key
// cost-duplicates flagged. res is the raw listing, for the truncation banner.
// The /garden purse reads the same list (farmSpendSource.FleetBudgetUSD).
func (s *Server) costAccounts(ctx context.Context, now time.Time) ([]costAccountView, *beadsapi.ListBeadsResult, error) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
		Sort:     "title",
		Limit:    costsAccountLimit,
	})
	if err != nil {
		return nil, nil, err
	}

	accounts := make([]costAccountView, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		// Drop E2E test accounts (ka-0000005167): the harness seeds throwaway
		// type=account beads on the same deployment and they otherwise land on the
		// cards, Top spenders, Hottest, and Throttled rollups. Matched on title
		// prefix + label prefix + e2e actor because two harnesses stamp no e2e:
		// label at all (a label-only filter would miss them).
		if beadsapi.IsE2EAccountBead(b) {
			continue
		}
		accounts = append(accounts, buildCostAccountView(b, now))
	}
	sort.Slice(accounts, func(i, j int) bool {
		return strings.ToLower(accounts[i].Name) < strings.ToLower(accounts[j].Name)
	})
	// Flag shared-litellm_key cost-duplicates so every fleet-wide money
	// aggregation books a shared bill exactly once (ka-0000003446). Per-account
	// cards and per-account counts are unaffected.
	markCostDuplicates(accounts)
	return accounts, res, nil
}

func buildCostAccountView(b *beadsapi.BeadDetail, now time.Time) costAccountView {
	f := b.Fields
	v := costAccountView{
		ID:            b.ID,
		Name:          b.Title,
		Provider:      f["provider"],
		Model:         f["model"],
		Status:        f["status"],
		ThrottleLabel: f["throttle_status"],
		OverageStatus: f["overage_status"],
		OverageReason: f["overage_disabled_reason"],
	}
	// Mirror the controller's own usable-status check (account_spread.go
	// selectAccount): status "" is active-by-default, only a non-empty,
	// non-"active" value (e.g. "exhausted") excludes the account.
	v.IsActive = v.Status == "" || v.Status == "active"
	v.IsThrottled = v.ThrottleLabel == "throttled" || v.ThrottleLabel == "rejected"

	// An auth-dead credential cannot serve traffic, so it must never read as
	// active/healthy capacity. Override the frozen throttle label and fold it
	// into IsThrottled so the Throttled panel surfaces it — otherwise the one
	// account an operator most needs to see is the only one hidden from it.
	if f["probe_auth_status"] == "rejected" {
		v.IsAuthDead = true
		v.ThrottleLabel = "auth-dead"
		v.IsThrottled = true
		v.IsActive = false
	}

	if probed := f["probe_checked_at"]; probed != "" {
		if t, err := time.Parse(time.RFC3339, probed); err == nil {
			v.ProbeAgo = fmtTimeAgo(t, now)
		}
	}

	if u, ok := parseFloat(f["utilization_5h"]); ok {
		v.Util5h = u
		v.Util5hPct = int(u * 100)
		v.Util5hHas = true
	}
	if u, ok := parseFloat(f["utilization_7d"]); ok {
		v.Util7d = u
		v.Util7dPct = int(u * 100)
		v.Util7dHas = true
	}
	if reset := f["throttle_reset_at"]; reset != "" {
		if t, err := time.Parse(time.RFC3339, reset); err == nil {
			v.ThrottleReset = fmtResetCountdown(t, now)
		}
	}
	if reset := f["throttle_reset_5h_at"]; reset != "" {
		if t, err := time.Parse(time.RFC3339, reset); err == nil {
			v.Reset5hCountdown = fmtResetCountdown(t, now)
		}
	}
	if reset := f["throttle_reset_7d_at"]; reset != "" {
		if t, err := time.Parse(time.RFC3339, reset); err == nil {
			v.Reset7dCountdown = fmtResetCountdown(t, now)
		}
	}

	v.HasOverage = v.OverageStatus != "" && v.OverageStatus != "enabled"

	// Cost read-only contract (epic ka-0000000758, design §2.3): every USD
	// figure on this card comes solely from the account bead's LiteLLM-sourced
	// fields. daily/weekly/total_cost_usd are written by the controller's
	// spend_sync daemon from LiteLLM's /key/info + /spend/logs; spend_at_last_reset
	// is a snapshot of that same total. The viewer performs NO client-side cost
	// estimation — it never multiplies tokens by a rate and has no fallback price
	// table. A missing field parses to 0 (parseFloat's bool is discarded here),
	// which flows to HasSpend=false so an account with no LiteLLM spend renders
	// $0, never a fabricated estimate. Do not add any cost source here that is
	// not a LiteLLM-populated bead field.
	v.DailyCostUSD, _ = parseFloat(f["daily_cost_usd"])
	v.WeeklyCostUSD, _ = parseFloat(f["weekly_cost_usd"])
	v.TotalCostUSD, _ = parseFloat(f["total_cost_usd"])
	if baseline, ok := parseFloat(f["spend_at_last_reset"]); ok {
		v.SinceResetSpend = max(v.TotalCostUSD-baseline, 0)
	}
	v.HasSpend = v.TotalCostUSD > 0 || v.WeeklyCostUSD > 0 || v.DailyCostUSD > 0
	if b, ok := parseFloat(f["spend_limit_usd"]); ok && b > 0 {
		v.BudgetUSD, v.BudgetHas = b, true
	} else if b, ok := parseFloat(f["max_budget"]); ok && b > 0 {
		v.BudgetUSD, v.BudgetHas = b, true
	}
	if v.BudgetHas {
		baseline, _ := parseFloat(f["spend_at_last_reset"])
		v.PeriodSpendUSD = max(v.TotalCostUSD-baseline, 0)
		v.RemainingUSD = max(v.BudgetUSD-v.PeriodSpendUSD, 0)
	}

	// Subscription-window spend + binding window (ka-0000003343). billing_type
	// defaults to "subscription" — the whole fleet is subscription-billed and
	// every account bead predates the field (ratelimit_probe.go:879-896), so a
	// missing value MUST resolve to subscription, matching the controller's own
	// billingType() default.
	v.BillingType = f["billing_type"]
	if v.BillingType == "" {
		v.BillingType = accountBillingSubscription
	}
	v.RepresentativeClaim = f["representative_claim"]
	v.Bill5hUSD, v.Bill5hHas = parseFloat(f["spend_5h_window_usd"])
	v.Bill7dUSD, v.Bill7dHas = parseFloat(f["spend_7d_window_usd"])
	// litellm_key is the shared-bill dedup basis (ka-0000003446). spend_sync
	// stamps the same per-key /key/info total onto every account bead sharing a
	// key, so markCostDuplicates uses this to book a shared bill once fleet-wide.
	v.LiteLLMKey = f["litellm_key"]

	if n, err := strconv.Atoi(f["session_count"]); err == nil {
		v.SessionCount = n
	}
	if n, err := strconv.Atoi(f["total_output_tokens"]); err == nil {
		v.TotalOutputTokens = n
	}
	v.HasUsage = v.SessionCount > 0 || v.TotalOutputTokens > 0

	return v
}

// markCostDuplicates flags, for each litellm_key shared by more than one
// account, every account but one representative owner as a cost-duplicate. It
// mutates accounts in place, setting CostDuplicate=true on the non-owners.
//
// spend_sync stamps the same per-key /key/info total onto EVERY account bead
// sharing a virtual key (e.g. litellm-bedrock ka-0000003488 + glm-bedrock
// ka-0000007590 both carry sk-…pX0heA1A at the same $761), so summing raw
// per-account would count that one bill N times in the /costs summary and in
// every rollup (ka-0000003446, follow-up to the metrics + Slack dedup in
// MR !5950). Booking it once — on a single deterministic owner — is what keeps
// the summary and all three rollups reconciled (no SharePct > 100).
//
// The owner is the lexicographically-first account NAME for the key, matching
// pickCostOwners in internal/metrics/metrics.go, so the choice is deterministic
// and stable regardless of the incoming account order (accounts is sorted by
// name before this runs, but the min-name rule does not rely on that). Accounts
// with an empty litellm_key are never deduped — each keeps its own cost — so
// single-key accounts are unaffected.
//
// Only MONEY is deduped: the CostDuplicate flag gates the dollar aggregations,
// while per-account cards keep rendering each bead's real cost and the
// session/util/status counts (ActiveCount, ThrottledCount, TotalSessions, …)
// stay genuinely per-account because those beads differ in sessions, models,
// utilization, etc.
func markCostDuplicates(accounts []costAccountView) {
	// owner[key] = lexicographically-first account name carrying that key.
	owner := make(map[string]string)
	for _, a := range accounts {
		if a.LiteLLMKey == "" {
			continue
		}
		if cur, ok := owner[a.LiteLLMKey]; !ok || a.Name < cur {
			owner[a.LiteLLMKey] = a.Name
		}
	}
	for i := range accounts {
		key := accounts[i].LiteLLMKey
		if key == "" {
			continue
		}
		accounts[i].CostDuplicate = accounts[i].Name != owner[key]
	}
}

func buildCostsSummary(accounts []costAccountView, now time.Time) costsSummaryView {
	// Every calendar-boundary field this function derives from now —
	// DayProgressPct, MonthProgressPct, MonthDayOfMonth, MonthDaysInMonth —
	// paces a numerator (daily_cost_usd / weekly) that spend_sync buckets to
	// 00:00 UTC (spend_sync.go:124-125). Normalize now to UTC so the caption
	// can never disagree with the spend it paces, no matter the pod's TZ
	// (ka-0000004628). LastUpdated also renders in UTC, consistent with the
	// page's explicit "UTC day (resets 00:00 UTC)" framing.
	now = now.UTC()
	s := costsSummaryView{
		TotalCount:  len(accounts),
		LastUpdated: now.Format("15:04"),
	}
	for _, a := range accounts {
		// Money is booked once per shared litellm_key: a cost-duplicate account
		// shares its key's bill with a representative owner, so its dollars are
		// skipped here (ka-0000003446). Session/active/throttle/overage/spend
		// COUNTS stay per-account — those are genuinely distinct per bead.
		if !a.CostDuplicate {
			s.GrandTotalUSD += a.DailyCostUSD
			s.WeeklyTotalUSD += a.WeeklyCostUSD
			s.TotalCostAllUSD += a.TotalCostUSD
			if a.BudgetHas {
				s.BudgetCount++
				s.BudgetUSD += a.BudgetUSD
				s.BudgetSpentUSD += a.PeriodSpendUSD
				s.RemainingUSD += a.RemainingUSD
			}
			// Subscription-window headline (ka-0000003343): sum the reset-aligned
			// window spend only for subscription accounts and only when the probe
			// wrote the window figure. A metered account (litellm-bedrock) is
			// excluded — its calendar-day presentation is correct — and an account
			// missing the field contributes nothing rather than a phantom $0. The
			// binding window is seven_day if ANY contributing account reports it so
			// (matching the per-account representative_claim pick), else the 5h
			// limit binds.
			if a.BillingType != accountBillingMetered {
				if a.Bill5hHas {
					s.Bill5hUSD += a.Bill5hUSD
					s.Bill5hHas = true
				}
				if a.Bill7dHas {
					s.Bill7dUSD += a.Bill7dUSD
					s.Bill7dHas = true
				}
				if a.Bill5hHas || a.Bill7dHas {
					s.SubscriptionCount++
				}
				// Measure-twice containment check: 5h window ⊆ 7d window, so the
				// 5h bill must not exceed the 7d bill (advice ka-0000005924).
				if a.Bill5hHas && a.Bill7dHas {
					s.BillXCheckAccounts++
					if a.Bill5hUSD > a.Bill7dUSD+billWindowXCheckTolUSD {
						s.BillXCheckViolations++
					}
				}
				if a.RepresentativeClaim == representativeClaimSevenDay {
					s.PrimaryBindsSevenDay = true
				}
			}
		}
		s.TotalSessions += a.SessionCount
		if a.IsActive {
			s.ActiveCount++
		}
		if a.IsThrottled {
			s.ThrottledCount++
		}
		if a.HasOverage {
			s.OverageCount++
		}
		if a.HasSpend {
			s.WithSpendCount++
		}
	}
	// If representative_claim binds the 7d window but no account wrote a 7d
	// figure (probe race: claim present, throttle_reset_7d_at not yet), fall the
	// headline back to whichever window HAS data rather than headlining a
	// misleading "$0.00" for a window we never measured. Symmetric for 5h.
	if s.PrimaryBindsSevenDay && !s.Bill7dHas && s.Bill5hHas {
		s.PrimaryBindsSevenDay = false
	} else if !s.PrimaryBindsSevenDay && !s.Bill5hHas && s.Bill7dHas {
		s.PrimaryBindsSevenDay = true
	}

	// HourlyRateUSD / ProjectedTodayUSD are NOT computed here. The headline
	// spend rate is the rolling-window rate from vmsingle (costs_rate.go,
	// ka-0000002806), applied by the handler via applyFleetHourlyRate so it
	// agrees with the same-page chart. The prior GrandTotal/hours formula was
	// an average-since-UTC-midnight that read absurdly high on a quiet night
	// (tiny early-hour denominator amplifying a lumpy at-close numerator).
	hours := float64(now.Hour()) + float64(now.Minute())/60.0
	s.DayProgressPct = int((hours / 24.0) * 100)
	if s.ActiveCount > 0 {
		s.AvgPerActiveUSD = s.GrandTotalUSD / float64(s.ActiveCount)
	}
	// Cost-per-session uses lifetime cost over the lifetime session count so
	// numerator and denominator share a window. Dividing the 1-day GrandTotal
	// by the lifetime session count (the prior behaviour) produced a unit that
	// shrank as sessions accumulated, regardless of real per-session cost
	// (audit F6, ka-0000006998).
	if s.TotalSessions > 0 {
		s.CostPerSessionUSD = s.TotalCostAllUSD / float64(s.TotalSessions)
	}
	// Month projection from the 7-day average daily rate (WeeklyTotal/7). We
	// have no per-day cost history, so this is NOT a true month-to-date sum —
	// MTDProjectionUSD assumes the 7d-avg rate held for every elapsed day, and
	// ProjectedMonthUSD extends that to the full month. Both diverge from the
	// real MTD on non-uniform spend; the surface labels them as 7d-avg
	// projections rather than measured month-to-date (audit F7, ka-0000006998).
	s.MonthDayOfMonth = now.Day()
	s.MonthDaysInMonth = daysInMonth(now.Year(), now.Month())
	if s.MonthDaysInMonth > 0 {
		s.MonthProgressPct = int((float64(s.MonthDayOfMonth) / float64(s.MonthDaysInMonth)) * 100)
	}
	if s.WeeklyTotalUSD > 0 && s.MonthDayOfMonth > 0 {
		dailyAvg := s.WeeklyTotalUSD / 7.0
		s.MTDProjectionUSD = dailyAvg * float64(s.MonthDayOfMonth)
		s.ProjectedMonthUSD = dailyAvg * float64(s.MonthDaysInMonth)
	}
	// Fleet-wide max utilization — surfaces "are any accounts near saturation"
	// in a single glance, without scrolling the per-account list.
	for _, a := range accounts {
		if a.Util5hHas && a.Util5hPct > s.MaxUtil5hPct {
			s.MaxUtil5hPct = a.Util5hPct
		}
		if a.Util7dHas && a.Util7dPct > s.MaxUtil7dPct {
			s.MaxUtil7dPct = a.Util7dPct
		}
	}
	return s
}

// daysInMonth returns the number of days in the given calendar month using
// time.Date's day-overflow normalization (day 0 of next month == last day of
// this month). Pure function — no allocation, no time.Now dependency.
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// buildCostsProviderRollups groups accounts by provider and totals their
// spend + utilization. Empty provider strings are bucketed under "unknown"
// so a misconfigured account doesn't disappear from the rollup.
func buildCostsProviderRollups(accounts []costAccountView, grandTotal float64) []costsProviderRollup {
	type agg struct {
		count, throttled int
		daily, weekly    float64
		total            float64
		utilSum          float64
		utilN            int
	}
	by := map[string]*agg{}
	order := []string{}
	for _, a := range accounts {
		p := a.Provider
		if p == "" {
			p = "unknown"
		}
		row, ok := by[p]
		if !ok {
			row = &agg{}
			by[p] = row
			order = append(order, p)
		}
		row.count++
		// Book shared-key money once (ka-0000003446): a cost-duplicate's dollars
		// belong to its owner's bucket, so skip them here. Account count,
		// throttle count, and utilization stay per-account.
		if !a.CostDuplicate {
			row.daily += a.DailyCostUSD
			row.weekly += a.WeeklyCostUSD
			row.total += a.TotalCostUSD
		}
		if a.IsThrottled {
			row.throttled++
		}
		if a.Util7dHas {
			row.utilSum += a.Util7d
			row.utilN++
		}
	}
	sort.Slice(order, func(i, j int) bool {
		return by[order[i]].daily > by[order[j]].daily
	})
	out := make([]costsProviderRollup, 0, len(order))
	for _, p := range order {
		row := by[p]
		r := costsProviderRollup{
			Provider:       p,
			AccountCount:   row.count,
			DailyUSD:       row.daily,
			WeeklyUSD:      row.weekly,
			TotalUSD:       row.total,
			ThrottledCount: row.throttled,
		}
		if grandTotal > 0 {
			r.SharePct = int((row.daily / grandTotal) * 100)
		}
		if row.utilN > 0 {
			r.AvgUtil7d = row.utilSum / float64(row.utilN)
			r.AvgUtil7dPct = int(r.AvgUtil7d * 100)
			r.AvgUtil7dHas = true
		}
		out = append(out, r)
	}
	return out
}

// buildCostsTopSpenders returns the top-N accounts by DailyCostUSD,
// skipping accounts with zero daily spend. The list is sorted descending
// and capped to limit entries.
func buildCostsTopSpenders(accounts []costAccountView, grandTotal float64, limit int) []costsTopSpender {
	filtered := make([]costAccountView, 0, len(accounts))
	for _, a := range accounts {
		// A cost-duplicate carries a copy of its owner's shared-key bill, so
		// listing it here would double-list the same dollars and its SharePct
		// would double-count against GrandTotal (ka-0000003446). The owner
		// already represents that spend; skip the duplicate.
		if a.CostDuplicate {
			continue
		}
		if a.DailyCostUSD > 0 {
			filtered = append(filtered, a)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].DailyCostUSD > filtered[j].DailyCostUSD
	})
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	out := make([]costsTopSpender, 0, len(filtered))
	for _, a := range filtered {
		row := costsTopSpender{
			ID:          a.ID,
			Name:        a.Name,
			Provider:    a.Provider,
			DailyUSD:    "$" + strconv.FormatFloat(a.DailyCostUSD, 'f', 2, 64),
			WeeklyUSD:   "$" + strconv.FormatFloat(a.WeeklyCostUSD, 'f', 2, 64),
			IsThrottled: a.IsThrottled,
		}
		if grandTotal > 0 {
			row.SharePct = int((a.DailyCostUSD / grandTotal) * 100)
		}
		out = append(out, row)
	}
	return out
}

// costsModelRollupTotalQuery is the lifetime-cumulative per-model query against
// the real-bill tag gauge. It mirrors the windowed tag-gauge query's dedup
// (`max by (dim, value, env)` collapses per-replica HA duplicates) but reads the
// gauge's CURRENT value (no increase()), so it is the lifetime bill per model.
const costsModelRollupTotalQuery = "sum by(" + costsRollupTagGaugeKeyLabel +
	")(max by (dim, value, env) (" + costsRollupTagGaugeMetric + `{dim="model"}))`

// costsModelRollupWindowQuery builds a windowed per-model spend query against
// the real-bill tag gauge. It is the dim="model" specialization of
// buildCostsRollupTagGaugeQuery's shape WITHOUT the topk cap (the page shows
// every model — cardinality is tiny, ~3-5 live) and without a value filter: the
// `[<window>:5m]` aligned subquery de-bumps the extrapolation sawtooth and the
// dedup collapses HA replicas, so these totals reconcile with the /costs
// headline and the explorer by-model rollup (ka-0000006501).
func costsModelRollupWindowQuery(window string) string {
	return "sum by(" + costsRollupTagGaugeKeyLabel +
		")(increase((max by (dim, value, env) (" + costsRollupTagGaugeMetric +
		`{dim="model"}))[` + window + ":" + costsRollupGaugeSubStep + "]))"
}

// buildCostsModelRollups builds the on-page "By model" rollup from the real-bill
// per-dimension tag gauge (gardenio_tag_cost_usd{dim="model"}), the same source
// the working cost explorer by-model rollup reads (ka-0000006501).
//
// This replaced an account-bead grouping that keyed on the account's optional,
// uncorrected `model` field — which was unset on most prod accounts (collapsing
// to a single "unknown" bucket) and, even when set, attributed ALL of an
// account's spend to its default model regardless of the role/project/agent
// override that actually rode it. The tag gauge is fed by the already-corrected
// LiteLLM `model:` tag (retagLiteLLMModel), so each row reflects the EFFECTIVE
// model and there is no "unknown" bucket.
//
// Three instant queries (24h/7d windowed increase + lifetime cumulative) are
// merged by model. Rows are sorted by 24h spend descending; SharePct is each
// row's share of the visible 24h model total (self-relative, always ≤100).
//
// Returns nil when no metrics upstream is wired (local dev / tests) or the gauge
// is empty — the `{{if .Models}}` template gate then hides the section, the same
// honest empty state the metric-backed rollup and headline rate use, rather than
// a fabricated or single-"unknown" table.
func (s *Server) buildCostsModelRollups(ctx context.Context) []costsModelRollup {
	if s.metricsUpstream == "" {
		return nil
	}
	daily := s.costsModelRollupValues(ctx, costsModelRollupWindowQuery(fleetHourlyRateWindow))
	weekly := s.costsModelRollupValues(ctx, costsModelRollupWindowQuery("7d"))
	total := s.costsModelRollupValues(ctx, costsModelRollupTotalQuery)
	if len(daily) == 0 && len(weekly) == 0 && len(total) == 0 {
		return nil
	}

	// Union the model keys seen across all three windows so a model with only
	// lifetime spend (nothing in the last 24h) still renders a row.
	seen := map[string]bool{}
	order := []string{}
	addKeys := func(m map[string]float64) {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
	}
	addKeys(daily)
	addKeys(weekly)
	addKeys(total)

	var dailyTotal float64
	for _, v := range daily {
		dailyTotal += v
	}

	out := make([]costsModelRollup, 0, len(order))
	for _, m := range order {
		r := costsModelRollup{
			Model:     m,
			DailyUSD:  daily[m],
			WeeklyUSD: weekly[m],
			TotalUSD:  total[m],
		}
		if dailyTotal > 0 {
			r.SharePct = int((daily[m] / dailyTotal) * 100)
		}
		out = append(out, r)
	}
	// Sort by 24h spend desc, then by lifetime desc as a stable tiebreak so a
	// model with no recent spend still orders deterministically by its bill.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DailyUSD != out[j].DailyUSD {
			return out[i].DailyUSD > out[j].DailyUSD
		}
		return out[i].TotalUSD > out[j].TotalUSD
	})
	return out
}

// costsModelRollupValues runs one per-model instant query and returns a
// model→USD map, flooring transient negatives (a mid-window counter reset can
// momentarily yield one) to 0. Returns nil when the upstream is unavailable so
// the caller can distinguish "no data" from "zero spend".
func (s *Server) costsModelRollupValues(ctx context.Context, query string) map[string]float64 {
	resp, ok := s.instantVectorQuery(ctx, query)
	if !ok {
		return nil
	}
	out := map[string]float64{}
	for _, r := range resp.Data.Result {
		key := r.Metric[costsRollupTagGaugeKeyLabel]
		if key == "" {
			continue
		}
		v, ok := parseVMSampleValue(r.Value[1])
		if !ok {
			continue
		}
		if v < 0 {
			v = 0
		}
		out[key] = v
	}
	return out
}

// buildCostsHottestUtilization returns accounts sorted by 5h utilization
// descending, capped to limit and dropping accounts with no 5h util signal.
// This is the leading indicator for an imminent throttle — distinct from
// top spenders, which is the lagging indicator of fleet load.
func buildCostsHottestUtilization(accounts []costAccountView, limit int) []costsUtilizationView {
	filtered := make([]costAccountView, 0, len(accounts))
	for _, a := range accounts {
		// Auth-dead accounts carry FROZEN utilization from their last good
		// probe (the 401/403 path never refreshes it). This panel is the
		// leading indicator of an *imminent* throttle, so a week-old number
		// is not a signal — it is noise that can outrank a genuinely hot
		// account. The Throttled panel reports auth-dead accounts instead.
		if a.IsAuthDead {
			continue
		}
		if a.Util5hHas || a.Util7dHas {
			filtered = append(filtered, a)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Util5hPct != filtered[j].Util5hPct {
			return filtered[i].Util5hPct > filtered[j].Util5hPct
		}
		return filtered[i].Util7dPct > filtered[j].Util7dPct
	})
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	out := make([]costsUtilizationView, 0, len(filtered))
	for _, a := range filtered {
		// Skip rows where both util values are zero — they have no signal
		// and would dilute the panel with idle accounts.
		if a.Util5hPct == 0 && a.Util7dPct == 0 {
			continue
		}
		out = append(out, costsUtilizationView{
			ID:               a.ID,
			Name:             a.Name,
			Provider:         a.Provider,
			Util5hPct:        a.Util5hPct,
			Util5hHas:        a.Util5hHas,
			Util7dPct:        a.Util7dPct,
			Util7dHas:        a.Util7dHas,
			Reset5hCountdown: a.Reset5hCountdown,
			Reset7dCountdown: a.Reset7dCountdown,
			IsThrottled:      a.IsThrottled,
		})
	}
	return out
}

// buildCostsThrottled returns currently-throttled accounts so operators
// can see the live throttle window without scanning every card.
func buildCostsThrottled(accounts []costAccountView) []costsThrottledView {
	out := make([]costsThrottledView, 0)
	for _, a := range accounts {
		if !a.IsThrottled {
			continue
		}
		out = append(out, costsThrottledView{
			ID:               a.ID,
			Name:             a.Name,
			Provider:         a.Provider,
			ThrottleLabel:    a.ThrottleLabel,
			ThrottleReset:    a.ThrottleReset,
			Util5hPct:        a.Util5hPct,
			Util5hHas:        a.Util5hHas,
			Util7dPct:        a.Util7dPct,
			Util7dHas:        a.Util7dHas,
			Reset5hCountdown: a.Reset5hCountdown,
			Reset7dCountdown: a.Reset7dCountdown,
		})
	}
	return out
}

// parseFloat returns (value, true) only when the string parses to a finite
// number. Empty strings and parse errors return (0, false) so callers can
// distinguish "missing" from "zero".
func parseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// fmtResetCountdown formats a future reset timestamp like "resets in 2h 15m".
// Mirrors iOS fmtResetCountdown.
func fmtResetCountdown(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "resetting now"
	}
	secs := int(d.Seconds())
	h := secs / 3600
	m := (secs % 3600) / 60
	if h > 0 {
		return "resets in " + strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
	if m > 0 {
		return "resets in " + strconv.Itoa(m) + "m"
	}
	return "resets in " + strconv.Itoa(secs) + "s"
}
