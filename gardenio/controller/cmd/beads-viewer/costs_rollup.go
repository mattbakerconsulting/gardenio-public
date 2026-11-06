package main

// costs_rollup.go — the NATIVE, interactive multi-axis cost rollup on /costs
// (operator decision ka-0000000742 option "costs-page").
//
// Background. The "Total cost — rollup" Grafana dashboard
// (uid gb-cost-per-bead-rollup-preview, model bead ka-0000004964) lets an
// operator filter spend by Project / Role / Model / Account and roll it up by
// any of {agent,role,project,model,account} — so "all gardener costs for the
// monorepo project" is Role=gardener + Project=monorepo, roll up by agent. But
// that interactivity lived ONLY on the standalone Grafana dashboard; /costs
// embedded just the static role-rollup panel. This file moves the same
// query shape onto the /costs page itself so the operator never has to leave
// the viewer.
//
// How. The rollup is PromQL-backed, dispatching per dimension onto whichever
// real-bill metric can express it (resolveRollupSource):
//
//	by-account            → account-cost gauge     (gardenio_account_cost_usd)
//	by-{role,project,model} → per-dimension tag gauge (gardenio_tag_cost_usd), unless
//	                          a cross-dimension filter is active
//	by-agent, cross-filter → per-agent spend-logs gauge (gardenio_agent_spend_logs_usd),
//	                          the only real-bill series carrying every
//	                          {agent,project,role,model,account} label
//
// Every source is the real LiteLLM bill. The by-agent path used to read the
// at-close counter gardenio_agent_spend_usd_total, which went dead in prod and read
// ~1000x low — the /costs "Settled cost" P0 ka-0000002699 re-pointed it here. The
// account/tag gauges are lifetime-cumulative, so their windowed spend is
// increase(gauge[range]); the per-agent spend-logs gauge is a Reset+Set gauge whose
// series population is already the sweep's ~48h lookback, so it is summed as a RAW
// instant (increase() on it trips counter-reset detection and reads garbage).
//
// The viewer already runs server-side INSTANT queries against the same in-cluster
// vmsingle for the fleet headline rate (costs_rate.go), so this file reuses that
// plumbing: it builds the ranked query server-side, runs it as an instant query,
// and renders a ranked-cost table — the "view all <X> costs" list. An HTMX form
// re-requests the table fragment on any dropdown change, so the page is interactive
// without a full reload and without opening Grafana. The Grafana bar-over-time
// chart stays as a complementary pop-out (the table answers "who spent how much",
// the bar chart answers "when").
//
// Safety. dim/range come from closed allowlists; filter VALUES are validated
// against a conservative label-value charset and escaped for the PromQL string
// literal before interpolation. Nothing user-supplied reaches vmsingle
// unvalidated, and the query is read-only (instant query, same as the rate
// headline). When no vmsingle upstream is wired (local dev / tests) the section
// degrades to an honest note + the Grafana link, mirroring the rate headline's
// "—" empty state.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/gardenhub"
)

// costsRollupMetric is the per-agent REAL-BILL spend gauge
// (gardenio_agent_spend_logs_usd, internal/metrics/spend.go): each tracked
// agent's lifetime spend recomputed every sweep directly from the LiteLLM
// /spend/logs request rows (spendlogs_sweep.go, C3). It is the only real-bill
// series carrying the full {agent,project,role,model,account} label set, so it
// backs the by-agent roll-up AND the cross-dimension-filtered fallback (the only
// source that can INTERSECT dimensions on the real bill).
//
// WHY THIS REPLACED THE AT-CLOSE COUNTER (ka-0000002699 — the /costs "Settled
// cost" 1000x-low P0): the by-agent path used to read
// gardenio_agent_spend_usd_total, an at-close counter that only ticks when a
// session closes. It went fully DEAD in prod (0 live series, ~$0 against a ~$39k
// bill) once the settlement pipeline moved to the stateless spend-logs sweep, so
// the DEFAULT cost-explorer view read ~1000x low. This gauge is that sweep's live
// output (verified: 859 series, real per-agent dollars), so by-agent now reads the
// real bill.
//
// QUERY SHAPE (critical): unlike a counter, this is a Reset+Set GAUGE holding an
// absolute lifetime figure per agent (SetSpendLogsCosts). increase()/rate() on it
// are MEANINGLESS — a Reset+Set gauge trips PromQL counter-reset detection every
// sweep and returns inflated garbage (measured: increase[24h] read ~$27k vs a real
// ~$2.7k). So the by-agent query is a RAW, deduped INSTANT sum
// (buildCostsRollupAgentGaugeQuery), never an increase(). Coverage is the sweep's
// ~48h lookback (agents born inside it), so the by-agent total is a recent-fleet
// slice, not the lifetime bill — the caption says so and points at the lifetime
// by-role/project/account gauges.
const costsRollupMetric = "gardenio_agent_spend_logs_usd"

// costsRollupGaugeMetric is the LiteLLM-synced account-cost gauge — the REAL
// bill, refreshed every ~5 min by the controller's spend_sync daemon. It tracks
// the actual account bill, so the by-account rollup uses it for an accurate number
// that reconciles with the /costs headline fleet spend rate (costs_rate.go,
// ka-0000004779). The gauge carries ONLY {account, env} labels, so it backs the
// by-account dim and nothing else; the by-agent dim and cross-filter fallbacks use
// costsRollupMetric (the per-agent spend-logs gauge, the only real-bill series with
// {agent,role,project,model,account} labels). Every rollup source is now the real
// bill: ka-0000006213 put by-account here, ka-0000000478 put role/project/model on
// the tag gauge, and ka-0000002699 moved by-agent off the dead at-close counter.
const costsRollupGaugeMetric = "gardenio_account_cost_usd"

// costsRollupGaugeSubStep is the aligned-subquery grid for the gauge's windowed
// increase. It matches the headline rate query VERBATIM (costs_rate.go /
// charts_catalog fleet-spend-rate, ka-0000000170): the [<window>:5m] subquery
// grid removes the per-step extrapolation sawtooth, and the `max by (account,
// env)` dedup collapses the per-pod gauge duplicates that agent-pod churn
// produces — so the by-account rollup magnitude reconciles with the headline
// instead of reading ~3.5x low like the raw counter did (ka-0000006213).
const costsRollupGaugeSubStep = "5m"

// costsRollupGaugeDim is the roll-up dimension served from the account-cost
// gauge. account is the only dimension that gauge can express (its label set is
// {account, env}); every other dim is served by the per-dimension tag gauge or the
// per-agent spend-logs gauge (ka-0000006213 / ka-0000000478 / ka-0000002699).
const costsRollupGaugeDim = "account"

// costsRollupE2EAccountExclusion is the PromQL label matcher that drops E2E
// test-account series from the by-account rollup and the Accounts dropdown
// (ka-0000005167). It is needed because the account series OUTLIVE the bead: the
// E2E harness runs against the same long-lived deployment, and the metric
// retains e2e series for the vmsingle retention window after the harness closes
// the bead — so the producer-side skip (metrics.collectAccounts) cannot clean the
// retention tail. This matcher does.
//
// It mirrors the TITLE-PREFIX signal of beadsapi.IsE2EAccountBead — the only
// signal expressible at the metric layer, where all we carry is the {account}
// label (= the bead title): e2e-* and probe-acct-*. PromQL regex matchers are
// fully anchored (verified against the live vmsingle: each alternation branch is
// wrapped individually, `^(?:e2e-.*|probe-acct-.*)$`), so this excludes exactly
// those two hyphen-anchored prefixes and never a real account merely beginning
// with the letters "e2e" (e.g. e2eflow-prod, my-e2e-lab).
//
// The (?i:…) group makes it CASE-INSENSITIVE to match IsE2EAccountBead, which
// lowercases the title before prefix-matching. Without it the two layers disagree:
// PromQL matchers are case-sensitive (verified: {account=~"GARDENIO2"} matches
// nothing while {account=~"(?i)GARDENIO2"} matches gardenio2), so an account titled
// "E2E-acct-primary-…" would be skipped by the producer yet keep its retention-tail
// series in the rollup and dropdown — silently defeating the one thing this matcher
// exists to do.
//
// Do NOT propagate this exclusion to the bill-conservation x-check sweep
// (cmd/controller/xcheck_sweep.go): a side there is the WHOLE LiteLLM bill, so
// filtering the other side would push gardenio_xcheck_agreement below its floor
// and fire a false cost-integrity alert (ka-0000001340). (The settled-vs-bill
// pair, another instance of this hazard, was retired in ka-0000000543.)
const costsRollupE2EAccountExclusion = `account!~"(?i:e2e-.*|probe-acct-.*)"`

// costsRollupAgentRealBillCaption is the provenance note for the by-agent view
// (and any per-agent-gauge fallback), sourced from the per-agent real-bill
// spend-logs gauge. It is the REAL LiteLLM bill per agent (ka-0000002699 replaced
// the dead at-close counter that read ~1000x low here), but its series population
// is the spend-logs sweep's ~48h lookback — only agents active in that window
// carry a figure — so the by-agent TOTAL is a recent-fleet slice, not the lifetime
// bill. The caption states that coverage explicitly (the bead's ask: a caption must
// name the number's actual relationship to the bill, never imply a subset is the
// whole) and points at the lifetime by-role/project/account gauges.
const costsRollupAgentRealBillCaption = "Real LiteLLM bill per agent — recomputed from LiteLLM /spend/logs, refreshed ~30 min. Covers agents active in roughly the last 48h, so this per-agent total is a recent slice of spend, not the lifetime bill; roll up by role, project, or account for the full lifetime figure."

// costsRollupRealBillCaption is the provenance note for the by-account view,
// sourced from the real-bill account-cost gauge — accurate and lag-free, so it
// carries a plain provenance line rather than the settled warning (the bead's
// explicit "the by-account view should NOT carry that caption", ka-0000006213).
const costsRollupRealBillCaption = "Real LiteLLM bill — account-cost gauge, refreshed ~5 min; reconciles with the /costs headline fleet spend rate."

// costsRollupTagGaugeMetric is the LiteLLM-synced per-dimension real-bill gauge
// (gardenio_tag_cost_usd, internal/metrics/spend.go), populated by the controller's
// tag_spend_sync poller from GET /spend/tags across every proxy. It carries
// {dim, value, env} labels where dim ∈ {role, project, model}; for those three
// roll-up dimensions it is the REAL bill — the same source family as the account
// gauge and the /costs headline — so by-role/by-project/by-model finally read the
// live LiteLLM bill instead of the at-close counter (ka-0000000478, the deferred
// back-half of Option A from ka-0000006213).
//
// Limitation: /spend/tags reports per-single-tag totals, so the gauge cannot
// INTERSECT dimensions (no "role=gardener AND project=monorepo"). When a
// cross-dimension filter is active the rollup falls back to the cross-filterable
// per-agent spend-logs gauge (resolveRollupSource); with no cross filter it uses
// this gauge.
const costsRollupTagGaugeMetric = "gardenio_tag_cost_usd"

// costsRollupTagGaugeDims are the roll-up dimensions served from the per-dimension
// real-bill gauge (when no cross-dimension filter is active). They match
// metrics.TagCostDims. account has its own gauge; agent rolls up on the per-agent
// spend-logs gauge (cardinality + per-row bead links).
var costsRollupTagGaugeDims = []string{"role", "project", "model"}

// costsRollupTagGaugeKeyLabel is the gauge label that carries the roll-up group
// value (the tag value, e.g. "gardener"). Unlike the per-agent/account gauges —
// where the group label IS the dim name (agent/role/project/model/account) — the
// tag gauge groups by its shared `value` label, with the dim selected via
// dim="<d>". Rows are therefore keyed off this label rather than the dim name.
const costsRollupTagGaugeKeyLabel = "value"

// costsRollupTagRealBillCaption is the provenance note for a by-{role,project,
// model} view served from the per-dimension real-bill gauge. Like the by-account
// note it is a neutral provenance line, but it also flags the cross-filter
// fallback so an operator understands why adding a second filter axis changes the
// number (ka-0000000478).
const costsRollupTagRealBillCaption = "Real LiteLLM bill — per-dimension spend tags, refreshed ~5 min; reconciles with the /costs headline. Cross-dimension filters fall back to the per-agent real-bill gauge (the tag gauge can't intersect tags)."

// costsRollupCrossFilterSettledCaption is the note for a by-{role,project,model}
// view that FELL BACK to the per-agent real-bill spend-logs gauge because a
// cross-dimension filter is active (the tag gauge can't intersect single-tag
// dimensions). Still the real bill, but scoped to the per-agent gauge's ~48h
// active-agent coverage — so the operator knows this cross-filtered slice is
// recent-fleet, and that clearing the other axes returns the lifetime tag-gauge
// view (ka-0000000478 / ka-0000002699).
const costsRollupCrossFilterSettledCaption = "Real LiteLLM bill (per-agent gauge) — a cross-dimension filter is active, so this falls back from the lifetime spend-tag gauge to the per-agent gauge, which covers agents active in roughly the last 48h. Clear the other filter axes to roll up by a single dimension on the full lifetime bill."

// costsRollupBackfillSettledCaption is the note when a by-{role,project,model}
// view WANTED the real-bill tag gauge but it returned no data, so the rollup fell
// back to the per-agent real-bill spend-logs gauge. This is expected right after
// the tag-spend producer deploys: the tag gauge is empty until agents accumulate
// tagged spend, and the view auto-upgrades to the per-dimension tag gauge as soon
// as it fills (ka-0000000478). Still the real bill — just via the per-agent gauge,
// whose ~48h active-agent coverage the caption names.
const costsRollupBackfillSettledCaption = "Real LiteLLM bill (per-agent gauge) — the per-dimension spend-tag view is still warming up (no tagged spend in this window yet), so this rolls up the per-agent gauge, which covers agents active in roughly the last 48h. It auto-upgrades to the lifetime per-dimension view once tag spend fills in."

// costsRollupTopN caps the ranked table, mirroring the dashboard's topk(50) on
// the "Ranked cost" panel (ka-0000004964). 50 distinct groups is already past
// what an operator scans; the long tail rolls into neither the table nor the
// total, which is acceptable for a "top spenders" view and matches Grafana.
const costsRollupTopN = 50

// costsRollupDims is the closed set of roll-up dimensions, matching the rollup
// dashboard's $rollup custom variable. Each is a label on costsRollupMetric, so
// `sum by(<dim>)` is always well-formed; the allowlist is also the injection
// guard for the by-clause. Order is the dropdown order.
var costsRollupDims = []string{"agent", "role", "project", "model", "account"}

// costsRollupDimDefault is the initial roll-up dimension — agent, so the
// section opens on the headline "chonky per-agent cost" view: which individual
// agents spent the most over the window. The earlier default was role (to match
// the static Grafana panel /costs embedded), but the native section now leads
// with per-agent because that is the question operators ask first — "who is
// burning the budget" — and, unlike the role rollup, every agent key links
// straight to its bead (applyRollupRowLinks).
const costsRollupDimDefault = "agent"

// costsRollupRangeOpt is one range choice: Key is the PromQL range-vector
// window (the increase() lookback), Label is the human dropdown text.
type costsRollupRangeOpt struct {
	Key   string
	Label string
}

// costsRollupRanges are the selectable increase() windows. All are valid
// VictoriaMetrics/PromQL duration literals, so each is safe to interpolate once
// validated against this allowlist.
var costsRollupRanges = []costsRollupRangeOpt{
	{Key: "1h", Label: "last 1h"},
	{Key: "6h", Label: "last 6h"},
	{Key: "24h", Label: "last 24h"},
	{Key: "7d", Label: "last 7d"},
	{Key: "30d", Label: "last 30d"},
}

// costsRollupRangeDefault is the initial window — 24h, the same span the fleet
// headline rate smooths over, so the native rollup and the headline agree on
// "today-ish".
const costsRollupRangeDefault = "24h"

// costsRollupFilterLabels are the filterable axes, in stable (dropdown + query
// build) order. Each is a label on costsRollupMetric.
var costsRollupFilterLabels = []string{"project", "role", "model", "account"}

// costsRollupValueRe is the conservative charset a filter VALUE must match to
// be interpolated into the PromQL selector. Account/agent/project/role/model
// values in practice are ASCII identifiers (e.g. "claude-opus-4-8",
// "thread-1782329116-766089", "monorepo", "gardener", "litellm-prod"); anything
// outside this set is treated as "All" rather than risk a malformed or crafted
// query. The hyphen is last so it is a literal, not a range. This is belt-and-
// suspenders with escapePromQLLabelValue — the regex rejects, the escape
// neutralizes.
var costsRollupValueRe = regexp.MustCompile(`^[A-Za-z0-9 ._:@/+-]+$`)

// costsRollupRow is one ranked group: the $rollup label value, its windowed
// spend, and its share of the visible total (for the bar + percent).
type costsRollupRow struct {
	Rank     int // 1-based rank in the ranked table (precomputed; templates stay dumb)
	Key      string
	USD      float64
	USDLabel string // pre-formatted "$12.34"
	SharePct int    // share of the visible total (0..100)
	BarPct   int    // share of the top row's spend (0..100), for the bar width
	// Href is the viewer URL this row's key navigates to, or "" when the key
	// is not linkable. Computed server-side in applyRollupRowLinks after the
	// rows are ranked: agent keys resolve to their bead via the per-request
	// agentResolver, role/project keys map to their hub pages. Making the
	// ranked rows clickable turns the table from a read-only list into a
	// navigable cost explorer — the headline ask of "much nicer per-agent
	// costs": click the costliest agent to land on its bead.
	Href string
	// Budget is the project's budget as /garden shows it (by-project rows
	// only; nil when the project has no budget or the farm has no figure for
	// it). It covers the budget's own period, not the rollup range.
	Budget *costsRollupBudget
}

// costsRollupBudget is a by-project row's budget, copied from the farm's
// cost row (gardenhub.FarmCostProject, gd-GmJYMnSKGJ) so /costs and /garden
// show the same budget, remaining and level. Display only.
type costsRollupBudget struct {
	BudgetLabel    string // "$250.00 a month" / "$40.00 a rolling week"
	RemainingLabel string // "$59.80"; "—" while the farm's spend read is down
	Level          string // gardenhub.BudgetLevel*; "" while the spend is unknown
}

// costsRollupView is the view-model for the interactive rollup section. The
// full fragment (costs_rollup) reads the dropdown option lists + selected
// filters; the table-only fragment (costs_rollup_table) reads Rows/Total/echo.
type costsRollupView struct {
	// Unavailable is true when no vmsingle upstream is wired — the section
	// degrades to a note rather than empty/broken controls.
	Unavailable bool
	// Available is true when the ranked query returned a usable vector (even an
	// empty one). False → the table renders a "metrics unavailable" note.
	Available bool

	Rows       []costsRollupRow
	TotalUSD   float64
	TotalLabel string

	RollupBy   string // selected dim
	RangeKey   string // selected range window key
	RangeLabel string // human label for RangeKey

	// Caption is the provenance note for the current roll-up source — every source
	// is now the real LiteLLM bill (ka-0000002699 retired the dead at-close
	// counter), so the note states the source and its coverage: lifetime for the
	// account/tag gauges, or the per-agent gauge's ~48h active-agent window for the
	// by-agent view and cross-filter fallbacks. Rendered under the table.
	Caption string
	// SourceIsRealBill flags a real-bill source for caption styling. It is now
	// always true — after ka-0000002699 every rollup dim reads a real-bill gauge —
	// so the caption always renders as a neutral provenance note, never the old
	// "settled" warning. Retained so the template styling and the tests that pin
	// the real-bill/no-warning contract stay explicit. It also gates the
	// "filters ignored on the gauge" note (by-account only).
	SourceIsRealBill bool
	// IgnoredFilters lists the active filter axes that the by-account gauge
	// query could not apply (project/role/model — the gauge has no such labels).
	// Empty for every other dim, which honor every filter. Surfaced so an operator
	// who set, say, role=gardener on the by-account view understands why it had no
	// effect (ka-0000006213).
	IgnoredFilters string

	FilterProject string
	FilterRole    string
	FilterModel   string
	FilterAccount string
	FilterEcho    string // human summary of active filters, e.g. "project=monorepo, role=gardener"

	// Dropdown option lists (only populated for the full fragment).
	Projects []string
	Roles    []string
	Models   []string
	Accounts []string

	// Static option lists, surfaced for the template's range over them.
	Dims   []string
	Ranges []costsRollupRangeOpt

	// GrafanaBaseURL gates the "Open bar chart in Grafana ↗" pop-out link.
	GrafanaBaseURL string
}

// validRollupDim returns dim when it is an allowed roll-up dimension, else the
// default. The result is interpolated into a `sum by(<dim>)` clause, so the
// allowlist is the injection guard.
func validRollupDim(dim string) string {
	if slices.Contains(costsRollupDims, dim) {
		return dim
	}
	return costsRollupDimDefault
}

// validRollupRange returns key when it is an allowed range window, else the
// default. The result is interpolated into an `increase(...[<key>])` clause.
func validRollupRange(key string) string {
	for _, r := range costsRollupRanges {
		if key == r.Key {
			return key
		}
	}
	return costsRollupRangeDefault
}

// rollupRangeLabel maps a (already-validated) range key to its human label.
func rollupRangeLabel(key string) string {
	for _, r := range costsRollupRanges {
		if r.Key == key {
			return r.Label
		}
	}
	return key
}

// sanitizeFilterValue trims a filter value and reports whether it is an ACTIVE
// filter. An empty value (or one failing the conservative charset) is "All" —
// returned as ("", false) so the caller omits the selector entirely. A passing
// value is returned trimmed with true.
func sanitizeFilterValue(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	if !costsRollupValueRe.MatchString(v) {
		return "", false
	}
	return v, true
}

// escapePromQLLabelValue escapes a value for a PromQL double-quoted string
// literal: backslash then double-quote. Callers have already charset-validated
// the value (so neither character can be present), but escaping keeps the
// builder correct by construction if the validation ever loosens.
func escapePromQLLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

// rollupSource is which metric backs a given (dim, filters) roll-up: the at-close
// per-agent counter, the per-account real-bill gauge, or the per-dimension
// real-bill tag gauge. resolveRollupSource is the single source of truth so the
// query builder, the row-key extractor, and the caption all agree.
type rollupSource int

const (
	// rollupSourceAgentGauge — the per-agent REAL-BILL spend-logs gauge
	// (gardenio_agent_spend_logs_usd). Backs by-agent always, and
	// by-{role,project,model} when a cross-dimension filter is active (the only
	// real-bill source carrying every {agent,project,role,model,account} label, so
	// the only one that can INTERSECT dimensions). Reads a raw deduped instant, not
	// an increase() (the gauge is Reset+Set — see buildCostsRollupAgentGaugeQuery).
	// It replaced the dead at-close counter that read ~1000x low (ka-0000002699).
	rollupSourceAgentGauge rollupSource = iota
	// rollupSourceAccountGauge — the per-account real-bill gauge (by-account).
	rollupSourceAccountGauge
	// rollupSourceTagGauge — the per-dimension real-bill tag gauge. Backs
	// by-{role,project,model} when NO cross-dimension filter is active
	// (ka-0000000478).
	rollupSourceTagGauge
)

// resolveRollupSource picks the backing metric for a roll-up. by-account → the
// account gauge; by-{role,project,model} → the per-dimension real-bill tag gauge
// UNLESS a cross-dimension filter is active (then the cross-filterable per-agent
// spend-logs gauge, the only real-bill source carrying every label); by-agent
// (and anything else) → that same per-agent gauge. Every source is now the real
// LiteLLM bill: ka-0000002699 retired the dead at-close counter the by-agent path
// used to read (~1000x low), building on the ka-0000006213/ka-0000000478 moves that
// had already put account/role/project/model on real-bill gauges.
func resolveRollupSource(dim string, filters map[string]string) rollupSource {
	if dim == costsRollupGaugeDim {
		return rollupSourceAccountGauge
	}
	if slices.Contains(costsRollupTagGaugeDims, dim) && !hasCrossDimFilter(dim, filters) {
		return rollupSourceTagGauge
	}
	return rollupSourceAgentGauge
}

// hasCrossDimFilter reports whether any filter axis OTHER than dim is set. The
// per-dimension tag gauge carries single-dimension tags, so it cannot honor a
// filter on a different axis (no "role=gardener AND project=monorepo"); such a
// filter routes the roll-up to the per-agent spend-logs gauge instead (it carries
// every label). A filter on dim itself is fine (it maps onto the gauge's shared
// `value` label).
func hasCrossDimFilter(dim string, filters map[string]string) bool {
	for _, label := range costsRollupFilterLabels {
		if label == dim {
			continue
		}
		if filters[label] != "" {
			return true
		}
	}
	return false
}

// rollupKeyLabel is the response label that carries each ranked row's group key.
// The counter and account gauge group BY the dim name, so the key label IS the
// dim; the tag gauge groups by its shared `value` label (the dim is selected via
// dim="<d>"), so its key lives there.
func rollupKeyLabel(dim string, filters map[string]string) string {
	if resolveRollupSource(dim, filters) == rollupSourceTagGauge {
		return costsRollupTagGaugeKeyLabel
	}
	return dim
}

// buildCostsRollupQuery assembles the ranked-cost instant query for the chosen
// roll-up dimension, dispatching on resolveRollupSource: by-account → the
// real-bill account-cost gauge; by-{role,project,model} with no cross-dimension
// filter → the real-bill per-dimension tag gauge (ka-0000000478); everything else
// (by-agent, or a cross-filtered role/project/model) → the per-agent real-bill
// spend-logs gauge (ka-0000002699).
func buildCostsRollupQuery(dim string, filters map[string]string, rangeWin string) string {
	switch resolveRollupSource(dim, filters) {
	case rollupSourceAccountGauge:
		return buildCostsRollupGaugeQuery(filters, rangeWin)
	case rollupSourceTagGauge:
		return buildCostsRollupTagGaugeQuery(dim, filters, rangeWin)
	default:
		return buildCostsRollupAgentGaugeQuery(dim, filters)
	}
}

// buildCostsRollupTagGaugeQuery assembles the ranked by-{role,project,model} query
// against the per-dimension real-bill tag gauge. It mirrors the account-gauge
// query's dedup + aligned-subquery shape: `max by (dim, value, env)` collapses any
// per-replica duplicates (HA), and the [<window>:5m] subquery grid de-bumps the
// extrapolation sawtooth — so the by-role/by-project/by-model totals reconcile
// with the /costs headline. dim is from the closed costsRollupTagGaugeDims
// allowlist (injection-safe); a same-dimension value filter is applied via the
// gauge's shared `value` label. Cross-dimension filters never reach here
// (resolveRollupSource routes them to the per-agent spend-logs gauge).
func buildCostsRollupTagGaugeQuery(dim string, filters map[string]string, rangeWin string) string {
	sel := []string{`dim="` + dim + `"`}
	if v := filters[dim]; v != "" {
		sel = append(sel, costsRollupTagGaugeKeyLabel+`="`+escapePromQLLabelValue(v)+`"`)
	}
	return "topk(" + strconv.Itoa(costsRollupTopN) + ", " + costsTagGaugeSpendExpr(strings.Join(sel, ","), rangeWin) + ")"
}

// costsTagGaugeSpendExpr is the windowed spend per `value` on the tag gauge for
// a selector: the dedup + aligned-subquery shape described on
// buildCostsRollupTagGaugeQuery, without the topk. The /garden farm's
// per-project rows (farmSpendSource) read the same expression unranked, so they
// agree with the by-project rollup here.
func costsTagGaugeSpendExpr(matcher, rangeWin string) string {
	return "sum by(" + costsRollupTagGaugeKeyLabel + ")(increase((max by (dim, value, env) (" +
		costsRollupTagGaugeMetric + "{" + matcher + "}))[" + rangeWin + ":" + costsRollupGaugeSubStep + "]))"
}

// buildCostsRollupAgentGaugeQuery assembles the ranked-cost instant query against
// the per-agent REAL-BILL spend-logs gauge (gardenio_agent_spend_logs_usd). It
// backs the by-agent dim and the cross-dimension-filtered fallback for
// role/project/model — the only real-bill source carrying every
// {agent,project,role,model,account} label, so the only one that can INTERSECT
// dimensions. filters maps a label to its already-sanitized value; empty/absent
// labels are omitted (== "All").
//
// CRITICAL — this is a RAW deduped INSTANT sum, NOT an increase() over a window,
// and it takes no rangeWin:
//
//   - The metric is a Reset+Set GAUGE (SetSpendLogsCosts): each cycle it Resets
//     then re-Sets every tracked agent's absolute lifetime figure. increase() sees
//     the periodic Reset as a counter reset and returns inflated garbage — measured
//     live at ~$27k over [24h] against a real ~$2.7k tracked population, and
//     ~$49k with max_over_time. Only the instant value is meaningful.
//   - Its series population IS the window: the spend-logs sweep only publishes
//     agents whose bead was created inside its ~48h lookback (spendlogs_sweep.go),
//     so "sum the live gauge" already yields recent-fleet spend without a PromQL
//     range. The explorer's range dropdown therefore does not scope this source
//     (the caption says the figure is the sweep's lookback slice, and points at the
//     lifetime by-role/project/account gauges for longer windows).
//
// `max by (agent,project,role,model,account)` collapses any duplicate series (HA
// controller replicas double-publishing the same agent) so a doubled scrape can't
// inflate the sum — matching the dedup the account/tag gauge queries apply. The
// E2E test-account exclusion (ka-0000005167) is ANDed in for the same reason the
// account gauge applies it: this gauge carries an `account` label and outlives the
// bead, so the retention tail of an E2E harness account must not leak into the
// real-bill by-agent total (keeps it reconciled with the by-account view).
func buildCostsRollupAgentGaugeQuery(dim string, filters map[string]string) string {
	sel := []string{costsRollupE2EAccountExclusion}
	for _, label := range costsRollupFilterLabels {
		v := filters[label]
		if v == "" {
			continue
		}
		sel = append(sel, label+`="`+escapePromQLLabelValue(v)+`"`)
	}
	matcher := strings.Join(sel, ",")
	return "topk(" + strconv.Itoa(costsRollupTopN) + ", sum by(" + dim +
		")(max by (agent, project, role, model, account) (" +
		costsRollupMetric + "{" + matcher + "})))"
}

// buildCostsRollupGaugeQuery assembles the ranked by-account query against the
// real-bill account-cost gauge. It mirrors the headline fleet-rate query's
// dedup + aligned-subquery shape VERBATIM (costs_rate.go, ka-0000004779 /
// ka-0000000170): `max by (account, env)` collapses the per-pod gauge duplicates
// that agent-pod churn produces, and the [<window>:5m] subquery grid de-bumps
// the per-step extrapolation sawtooth — so the by-account total reconciles with
// the /costs headline (~$43-63/h trailing) instead of reading ~3.5x low like the
// counter did (ka-0000006213). Only the `account` filter is applied: the gauge
// carries no project/role/model labels, so those filters cannot scope it
// (handleCostsRollup surfaces that they were ignored).
func buildCostsRollupGaugeQuery(filters map[string]string, rangeWin string) string {
	// Always drop E2E test-account series (ka-0000005167) — the gauge outlives the
	// bead, so the producer-side skip cannot clean the retention tail. An explicit
	// account filter is ANDed on top of the exclusion.
	sel := []string{costsRollupE2EAccountExclusion}
	if v := filters[costsRollupGaugeDim]; v != "" {
		sel = append(sel, costsRollupGaugeDim+`="`+escapePromQLLabelValue(v)+`"`)
	}
	matcher := strings.Join(sel, ",")
	return "topk(" + strconv.Itoa(costsRollupTopN) +
		", sum by(" + costsRollupGaugeDim + ")(increase((max by (account, env) (" +
		costsRollupGaugeMetric + "{" + matcher + "}))[" + rangeWin + ":" + costsRollupGaugeSubStep + "])))"
}

// vmVectorResponse is the subset of vmsingle's /api/v1/query JSON for a
// "vector" result we read: each sample carries its label set (metric) and a
// [<ts>, "<stringified float>"] value. Distinct from costs_rate.go's
// vmInstantResponse, which discards the label set (the rate is a single scalar).
type vmVectorResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// instantVectorQuery runs a single instant query against vmsingle and returns
// the parsed vector. The bool is false when no upstream is wired, the upstream
// errors / returns non-200 / unparseable, or the status is not "success". Used
// for both the ranked-cost query and the label-value (dropdown) lookups.
func (s *Server) instantVectorQuery(ctx context.Context, query string) (*vmVectorResponse, bool) {
	if s.metricsUpstream == "" {
		return nil, false
	}
	var parsed vmVectorResponse
	if !s.vmGetJSON(ctx, s.metricsUpstream+vmsingleInstantQueryPath+"?query="+url.QueryEscape(query), "costs rollup", &parsed) || parsed.Status != "success" {
		return nil, false
	}
	return &parsed, true
}

// vmGetJSON GETs u from vmsingle and decodes its JSON into out: the timeout, the 1 MiB body cap and the failure logs
// (each prefixed with what) every vmsingle read here shares. False when the upstream errors, answers non-200 or a body
// that does not parse; the caller checks the status field.
func (s *Server) vmGetJSON(ctx context.Context, u, what string, out any) bool {
	client := s.metricsHTTP
	if client == nil {
		client = &http.Client{Timeout: metricsProxyTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, metricsProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		s.logger.Warn(what+": build request", "error", err)
		return false
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn(what+": upstream error", "error", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.logger.Warn(what+": upstream status", "status", resp.StatusCode)
		return false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		s.logger.Warn(what+": read body", "error", err)
		return false
	}
	if err := json.Unmarshal(body, out); err != nil {
		s.logger.Warn(what+": parse body", "error", err)
		return false
	}
	return true
}

// parseVMSampleValue extracts the float from a vmsingle sample value's second
// element (the JSON string "<float>"). Rejects NaN/Inf and unparseable strings.
func parseVMSampleValue(raw json.RawMessage) (float64, bool) {
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
	if err != nil {
		return 0, false
	}
	if v != v || v > 1e300 || v < -1e300 { // NaN / ±Inf are not a meaningful spend
		return 0, false
	}
	return v, true
}

// costsRollupRowsFromVector turns a ranked-query vector into sorted rows + the
// visible total. dim is the roll-up label whose value keys each row; a sample
// missing it (should not happen for this metric) buckets under "(none)".
// Negative samples (a counter reset mid-window can momentarily yield one) are
// floored to 0 so a transient reset never shows a negative spend.
func costsRollupRowsFromVector(resp *vmVectorResponse, dim string) ([]costsRollupRow, float64) {
	if resp == nil {
		return nil, 0
	}
	rows := make([]costsRollupRow, 0, len(resp.Data.Result))
	var total float64
	for _, r := range resp.Data.Result {
		v, ok := parseVMSampleValue(r.Value[1])
		if !ok {
			continue
		}
		if v < 0 {
			v = 0
		}
		key := r.Metric[dim]
		if key == "" {
			key = "(none)"
		}
		rows = append(rows, costsRollupRow{Key: key, USD: v, USDLabel: fmtUSD(v)})
		total += v
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].USD > rows[j].USD })
	if len(rows) > 0 {
		topUSD := rows[0].USD
		for i := range rows {
			rows[i].Rank = i + 1
			if total > 0 {
				rows[i].SharePct = int((rows[i].USD / total) * 100)
			}
			if topUSD > 0 {
				rows[i].BarPct = int((rows[i].USD / topUSD) * 100)
			}
		}
	}
	return rows, total
}

// applyRollupRowLinks fills each row's Href so the ranked table is navigable.
// The link target depends on the roll-up dimension:
//
//   - agent   → the agent bead, resolved by name via the per-request resolver
//     (same path agentLink uses, so it shares the prewarmed cache and emits a
//     stable /{ka-id} URL the hover popover already understands).
//   - role    → the role hub (/role/<role>).
//   - project → the project page (/p/<project>).
//   - model / account → no per-row page exists, so these stay unlinked.
//
// The "(none)" sentinel and any agent name that fails to resolve are left
// unlinked (Href == "") so the template renders them as plain text rather than
// a dead link. basePath is the viewer mount prefix; resolver may be nil (tests
// without a daemon), in which case agent rows simply stay unlinked.
func applyRollupRowLinks(rows []costsRollupRow, dim, basePath string, resolver *agentResolver) {
	for i := range rows {
		key := rows[i].Key
		if key == "" || key == "(none)" {
			continue
		}
		switch dim {
		case "agent":
			if !looksLikeAgentName(key) {
				continue
			}
			if id, ok := resolver.Resolve(key); ok {
				rows[i].Href = basePath + "/" + url.PathEscape(id)
			}
		case "role":
			rows[i].Href = basePath + "/role/" + url.PathEscape(key)
		case "project":
			rows[i].Href = basePath + "/p/" + url.PathEscape(key)
		}
	}
}

// applyRollupRowBudgets puts each by-project row's budget on it, read from
// the farm's published cost blob (gardenhub.Farm.ProjectCost) rather than
// recomputed, so the figures are the ones the /garden header shows.
func applyRollupRowBudgets(rows []costsRollupRow, projectCost func(string) *gardenhub.FarmCostProject) {
	for i := range rows {
		rows[i].Budget = rollupBudgetOf(projectCost(rows[i].Key))
	}
}

// rollupBudgetOf formats one farm cost row's budget, or nil when it has none.
func rollupBudgetOf(p *gardenhub.FarmCostProject) *costsRollupBudget {
	if p == nil || !p.HasBudget || p.BudgetUSD == nil {
		return nil
	}
	b := &costsRollupBudget{BudgetLabel: fmtUSD(*p.BudgetUSD) + " a month", RemainingLabel: "—", Level: p.Level}
	if p.Period == gardenhub.Budget7d {
		b.BudgetLabel = fmtUSD(*p.BudgetUSD) + " a rolling week"
	}
	if p.Has && p.RemainingUSD != nil {
		b.RemainingLabel = fmtUSD(*p.RemainingUSD)
	}
	return b
}

// costsRollupLabelValues fetches the distinct values of a metric label for a
// filter dropdown via `count by(<label>)(metric)` — one series per value. label
// is from the fixed costsRollupFilterLabels set, so it is safe to interpolate.
// Returns a sorted, de-duplicated, non-empty list (nil when unavailable).
func (s *Server) costsRollupLabelValues(ctx context.Context, label string) []string {
	// Each dropdown must be sourced from the SAME metric that backs the rollup it
	// scopes, or it can offer values the table never ranks (and hide values it
	// does). The by-account rollup is served from the real-bill account-cost gauge
	// (costsRollupGaugeMetric, ka-0000006213), so the Accounts picker must read that
	// gauge too — NOT the per-agent spend-logs gauge (costsRollupMetric), whose
	// account label only covers accounts with an agent active in the sweep's ~48h
	// window. Sourcing the picker from costsRollupMetric let the by-account table
	// rank 16 accounts while the dropdown offered a near-empty subset — the account
	// gauge refreshes ~5min and carries the full retention tail, the spend-logs
	// gauge lags and is sparse (ka-0000007866). The other axes (project/role/model)
	// have no dedicated gauge here, so they stay on costsRollupMetric.
	metric := costsRollupMetric
	if label == costsRollupGaugeDim {
		metric = costsRollupGaugeMetric
	}
	// The account dropdown must drop E2E test accounts (ka-0000005167): the account
	// gauge's series outlive the bead, so a bead-side filter alone would keep listing
	// them in the picker long after the harness closed them. This exclusion is
	// LOAD-BEARING on the gauge (it carries every e2e account in its retention tail),
	// where on the old spend-logs source it was a provable no-op. The other axes
	// (project/role/model) carry no e2e-account naming and need no matcher.
	matcher := ""
	if label == costsRollupGaugeDim {
		matcher = "{" + costsRollupE2EAccountExclusion + "}"
	}
	resp, ok := s.instantVectorQuery(ctx, "count by("+label+")("+metric+matcher+")")
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		v := r.Metric[label]
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// costsRollupFilterEcho renders a compact human summary of the active filters,
// e.g. "project=monorepo, role=gardener", or "" when none are set.
func costsRollupFilterEcho(filters map[string]string) string {
	var parts []string
	for _, label := range costsRollupFilterLabels {
		if v := filters[label]; v != "" {
			parts = append(parts, label+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}

// costsRollupGaugeIgnoredFilters lists the active filter axes a by-account gauge
// query cannot apply, in canonical order. The gauge carries only {account, env}
// labels, so every filter except `account` is silently dropped from the query;
// this surfaces which ones, so the table can tell an operator who set, say,
// role=gardener on the by-account view that it had no effect (ka-0000006213).
// Returns "" when nothing was dropped (only the account filter, or none, set).
func costsRollupGaugeIgnoredFilters(filters map[string]string) string {
	var parts []string
	for _, label := range costsRollupFilterLabels {
		if label == costsRollupGaugeDim {
			continue
		}
		if v := filters[label]; v != "" {
			parts = append(parts, label+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}

// applyCostsRollupCaption sets the source-provenance fields on the view by
// roll-up source (resolveRollupSource). EVERY source now reads the real LiteLLM
// bill — ka-0000002699 retired the dead at-close counter the by-agent path used to
// ride — so SourceIsRealBill is always true and no view carries the old "settled /
// lags ~1d" warning. What differs is the coverage the caption states:
//   - account gauge (by-account) → lifetime real bill + the list of filters the
//     gauge could not apply (ka-0000006213).
//   - tag gauge (by-{role,project,model}, no cross filter) → lifetime real bill,
//     noting the cross-filter fallback (ka-0000000478).
//   - per-agent gauge (by-agent, or a cross-filtered/backfilled role/project/model)
//     → real bill scoped to the spend-logs sweep's ~48h active-agent coverage, so
//     the caption names that the total is a recent slice, not the lifetime bill.
func applyCostsRollupCaption(view *costsRollupView, filters map[string]string) {
	view.SourceIsRealBill = true
	switch resolveRollupSource(view.RollupBy, filters) {
	case rollupSourceAccountGauge:
		view.Caption = costsRollupRealBillCaption
		view.IgnoredFilters = costsRollupGaugeIgnoredFilters(filters)
	case rollupSourceTagGauge:
		view.Caption = costsRollupTagRealBillCaption
	default:
		// A role/project/model rollup only reaches the per-agent gauge when a
		// cross-dim filter forced the fallback; say so specifically. by-agent (and
		// any other per-agent-gauge rollup) gets the generic per-agent coverage note.
		if slices.Contains(costsRollupTagGaugeDims, view.RollupBy) && hasCrossDimFilter(view.RollupBy, filters) {
			view.Caption = costsRollupCrossFilterSettledCaption
		} else {
			view.Caption = costsRollupAgentRealBillCaption
		}
	}
}

// runCostsRollupQuery runs the ranked-cost instant query for (dim, filters, rng)
// and returns the parsed vector, the response label its rows are keyed on, and
// whether a per-dimension→per-agent BACKFILL fallback was taken.
//
// For a by-{role,project,model} rollup that resolves to the real-bill tag gauge,
// the gauge is empty until the tag-spend producer deploys and agents accumulate
// tagged spend. Rather than render a blank table during that window, this falls
// back to the per-agent real-bill spend-logs gauge (grouped by the same dim, which
// it also carries) when the tag gauge returns no rows — so the view is correct
// from first deploy and auto-upgrades to the per-dimension tag gauge once it fills
// (ka-0000000478; the fallback source is the per-agent gauge post-ka-0000002699,
// not the retired at-close counter). The fallback is signalled so the caller can
// re-caption.
//
// The bool (last return) is false only when the upstream is unreachable/errored
// for the primary query AND no usable fallback ran — matching the prior
// "metrics unavailable" behavior.
func (s *Server) runCostsRollupQuery(ctx context.Context, dim string, filters map[string]string, rng string) (resp *vmVectorResponse, keyLabel string, usedFallback bool, ok bool) {
	primary, pok := s.instantVectorQuery(ctx, buildCostsRollupQuery(dim, filters, rng))
	source := resolveRollupSource(dim, filters)

	// Real-bill tag gauge with no rows (unavailable OR empty) → backfill fallback
	// to the per-agent real-bill spend-logs gauge (it carries role/project/model
	// too), grouped by the same dim. Only when there is no active cross filter (a
	// cross filter already routes to the per-agent gauge, so source would not be the
	// tag gauge here).
	if source == rollupSourceTagGauge && (!pok || len(primary.Data.Result) == 0) {
		fbQ := buildCostsRollupAgentGaugeQuery(dim, filters)
		if fb, fok := s.instantVectorQuery(ctx, fbQ); fok {
			return fb, dim, true, true
		}
	}

	if !pok {
		return nil, "", false, false
	}
	return primary, rollupKeyLabel(dim, filters), false, true
}

// handleCostsRollup serves the interactive rollup fragment for /costs.
//
//	GET /costs/rollup            → full fragment (controls + table); lazy-loaded.
//	GET /costs/rollup?part=table → table only; the controls re-request this on
//	                               change so only the table swaps (the <select>s
//	                               keep their browser state, no flicker).
//
// Query params: rollup={agent|role|project|model|account}, range={1h|6h|24h|7d|
// 30d}, and the filter axes project/role/model/account (empty = All). All are
// validated/sanitized before they reach vmsingle.
func (s *Server) handleCostsRollup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dim := validRollupDim(q.Get("rollup"))
	rng := validRollupRange(q.Get("range"))
	tableOnly := q.Get("part") == "table"

	filters := map[string]string{}
	for _, label := range costsRollupFilterLabels {
		if v, ok := sanitizeFilterValue(q.Get(label)); ok {
			filters[label] = v
		}
	}

	view := costsRollupView{
		RollupBy:       dim,
		RangeKey:       rng,
		RangeLabel:     rollupRangeLabel(rng),
		FilterProject:  filters["project"],
		FilterRole:     filters["role"],
		FilterModel:    filters["model"],
		FilterAccount:  filters["account"],
		FilterEcho:     costsRollupFilterEcho(filters),
		Dims:           costsRollupDims,
		Ranges:         costsRollupRanges,
		GrafanaBaseURL: s.grafanaBaseURL,
	}
	// Source provenance caption: every dim now reads a real-bill gauge — account
	// gauge (by-account), per-dimension tag gauge (role/project/model), or per-agent
	// spend-logs gauge (agent + cross-filter fallbacks) — with the caption naming
	// each source's coverage (ka-0000006213 / ka-0000000478 / ka-0000002699).
	// Computed from dim/filters regardless of upstream availability so the caption
	// is stable across the table-only swaps.
	applyCostsRollupCaption(&view, filters)

	if s.metricsUpstream == "" {
		view.Unavailable = true
	} else {
		if resp, keyLabel, usedFallback, ok := s.runCostsRollupQuery(r.Context(), dim, filters, rng); ok {
			// The row group-key lives on the dim label for the per-agent/account
			// gauge but on the shared `value` label for the per-dimension tag gauge
			// (ka-0000000478). The Key VALUE is identical either way (e.g.
			// "gardener"); only the source label differs, so row links below still
			// switch on the logical dim.
			view.Rows, view.TotalUSD = costsRollupRowsFromVector(resp, keyLabel)
			view.TotalLabel = fmtUSD(view.TotalUSD)
			view.Available = true
			// If the per-dimension tag gauge was empty and we fell back to the
			// per-agent real-bill gauge (backfill window after the tag-spend producer
			// deploys), re-caption to name that source's ~48h active-agent coverage.
			// Still the real bill — just a recent slice — so SourceIsRealBill stays
			// true (ka-0000000478 / ka-0000002699).
			if usedFallback {
				view.Caption = costsRollupBackfillSettledCaption
			}
			// Make the ranked rows clickable. For an agent rollup, prewarm the
			// resolver with every ranked agent name in ONE batched daemon call
			// (FindAgentBeadsByNames) before applyRollupRowLinks resolves them —
			// otherwise 50 rows would fan out to 50 per-name lookups. role/
			// project rows link deterministically and need no daemon round-trip.
			resolver := newAgentResolver(r.Context(), s.daemon)
			if dim == "agent" {
				names := make([]string, 0, len(view.Rows))
				for _, row := range view.Rows {
					names = append(names, row.Key)
				}
				resolver.Prewarm(names)
			}
			applyRollupRowLinks(view.Rows, dim, s.basePath, resolver)
			if dim == "project" && s.garden != nil && s.garden.Farm != nil {
				applyRollupRowBudgets(view.Rows, s.garden.Farm.ProjectCost)
			}
		}
		// Dropdown options are only re-rendered with the full fragment; a
		// table-only swap leaves the existing <select>s untouched, so skip the
		// four label-value round trips on every dropdown change.
		if !tableOnly {
			view.Projects = s.costsRollupLabelValues(r.Context(), "project")
			view.Roles = s.costsRollupLabelValues(r.Context(), "role")
			view.Models = s.costsRollupLabelValues(r.Context(), "model")
			view.Accounts = s.costsRollupLabelValues(r.Context(), "account")
		}
	}

	name := "costs_rollup"
	if tableOnly {
		name = "costs_rollup_table"
	}
	s.renderPartial(w, r, "templates/partials/costs_rollup.html", name, map[string]any{"Rollup": view})
}
