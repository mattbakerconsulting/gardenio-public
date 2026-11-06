package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
)

// agentsActiveLimit / agentsRecentLimit cap the daemon list calls. Active is
// the live fleet (well under 100 in practice); recent is a tail of
// recently-terminated agents that we then trim to agentsRecentWindow
// client-side. The fetch limit is intentionally larger than the rendered cap:
// the daemon has no "updated_after" filter, so we over-fetch by updated_at desc
// and discard anything outside the window. 100 is the empirical p95 of closed
// agents per 6h across all projects with comfortable headroom.
//
// agentsRecentWindow defines what "Recent" means: closed agents whose
// updated_at is within this window of now. Anything older is dropped. The
// window is also surfaced in the UI tooltip so operators don't have to guess
// (ka-0000001416).
const (
	agentsActiveLimit = 100
	// agentsRecentLimit is sized to cover the 24h history window used by the
	// Last24h summary block (ka-0000003534), not just the 6h Recent listing.
	// p95 of closed agents across all projects in a 24h window stays well under
	// 200 even on busy days.
	agentsRecentLimit = 200
	// agentsRecentWindow defines what "Recent" means in the Recent listing
	// below the live fleet: closed agents whose updated_at is within this
	// window of now.
	agentsRecentWindow = 6 * time.Hour
	// agentsHistoryWindow is the longer horizon used by the Last24h summary
	// block (closures, failures, average lifetime) and the fleet-size sparkline.
	// Surfaces churn that the 6h Recent window hides (ka-0000003534).
	agentsHistoryWindow = 24 * time.Hour
	// agentsDormantLimit caps the dedicated dormant-bucket query — CLOSED agent
	// beads carrying the engine-set agent_state=dormant (parked persistent
	// identities awaiting their wake, task ka-0000004675). This is a SEPARATE,
	// window-INDEPENDENT query: a parked identity's next_respawn_at can sit hours
	// or days out, so it would age past the 6h/24h recent windows and vanish. A
	// server-side field filter on the engine-set state keeps the bucket
	// authoritative regardless of how long an identity has been parked. Sized well
	// above any realistic parked-fleet size.
	agentsDormantLimit = 200
	// agentsSparklineBuckets is the number of hourly buckets in the fleet-size
	// sparkline. Six covers the same horizon as the Recent listing while
	// staying narrow enough to fit beside the Alive counter.
	agentsSparklineBuckets = 6
)

// Phase 4 UI-state cookies. Persist filter mode and row density across reloads
// so an operator's view sticks. Long max-age (1 year) — these are pure UI
// preferences, no PII, no auth carrying.
const (
	cookieAgentFeedMode     = "agent_feed_mode"    // "" (default) | "slack"
	cookieAgentFeedDensity  = "agent_feed_density" // "" (comfortable) | "compact"
	cookieAgentTermFont     = "agent_term_fontsize"
	cookieConstellationOpen = "bead_constellation" // "expanded" (default) | "collapsed"
	uiCookieMaxAgeSeconds   = 365 * 24 * 3600
)

var (
	allowedFeedModes = map[string]struct{}{"": {}, "slack": {}}
	// "list" is the explicit toggle-back value users land on when leaving the
	// compact default. The original implementation only stored "" or "compact"
	// in the cookie, but readUIPreference's `v != ""` guard prevented "" from
	// round-tripping through a `?density=` link — so once compact became the
	// default, there was no way to opt out. "list" makes the opt-out explicit.
	// Templates treat anything that isn't "compact" as the list view.
	allowedFeedDensities       = map[string]struct{}{"": {}, "list": {}, "compact": {}}
	allowedTermFontSizes       = map[string]struct{}{"small": {}, "medium": {}, "large": {}}
	allowedConstellationStates = map[string]struct{}{"collapsed": {}, "expanded": {}}
	defaultFeedMode            = ""
	// defaultFeedDensity: "compact" — the audit (ka-0000004132) flagged the
	// list view as sparse because each agent card uses 4 stacked rows and
	// only the leftmost ~60% has content. The compact table packs every row
	// into a single line (name | role | state | pod | task | id | up | active)
	// and matches the audit's first recommendation. Operators who prefer the
	// list view can still toggle to it; the cookie persists their choice.
	defaultFeedDensity  = "compact"
	defaultTermFontSize = "medium"
	// defaultConstellationState: "expanded" — the constellation graph renders
	// open on first load (ka-0000004042) so the dependency hierarchy is visible
	// without clicking the reveal strip. Users who prefer it collapsed can flip
	// the default from the reveal-strip checkbox; the cookie persists the choice.
	defaultConstellationState = "expanded"
)

// readUIPreference returns the value of a cookie-backed UI preference, falling
// back to a default when missing or rejected by the allow-list. When the
// request has a `?<paramName>=` query string and the value is allowed, it
// overrides the cookie AND is written back as the new cookie value.
//
// The query→cookie round-trip lets a deep-link like `/agents?mode=slack` set
// the cookie on first load, so subsequent visits without the param keep the
// chosen mode.
func readUIPreference(w http.ResponseWriter, r *http.Request, name, paramName, fallback string, allowed map[string]struct{}) string {
	if v := strings.TrimSpace(r.URL.Query().Get(paramName)); v != "" {
		if _, ok := allowed[v]; ok {
			http.SetCookie(w, &http.Cookie{
				Name:     name,
				Value:    v,
				Path:     "/",
				MaxAge:   uiCookieMaxAgeSeconds,
				HttpOnly: false,
				Secure:   cookieSecure(r),
				SameSite: http.SameSiteLaxMode,
			})
			return v
		}
	}
	if c, err := r.Cookie(name); err == nil {
		v := strings.TrimSpace(c.Value)
		if _, ok := allowed[v]; ok {
			return v
		}
	}
	return fallback
}

// feedMode returns the current `agent_feed_mode` cookie value (query→cookie
// override) or "" for the default unfiltered view. Exposed to templates so the
// chip strip can render the active state.
func (s *Server) feedMode(w http.ResponseWriter, r *http.Request) string {
	return readUIPreference(w, r, cookieAgentFeedMode, "mode", defaultFeedMode, allowedFeedModes)
}

// feedDensity returns the current `agent_feed_density` cookie value
// (query→cookie override) or "" for the default comfortable view.
func (s *Server) feedDensity(w http.ResponseWriter, r *http.Request) string {
	return readUIPreference(w, r, cookieAgentFeedDensity, "density", defaultFeedDensity, allowedFeedDensities)
}

// termFontSize returns the current xterm font-size preference for the
// agent-detail page. Used by the template to set a body class so the CSS
// can size the terminal without a flash of default-size content.
func (s *Server) termFontSize(w http.ResponseWriter, r *http.Request) string {
	return readUIPreference(w, r, cookieAgentTermFont, "fontsize", defaultTermFontSize, allowedTermFontSizes)
}

// constellationDefaultState returns the bead-show constellation reveal
// preference: "collapsed" (default) hides the graph behind a Show button,
// "expanded" renders it open. The `constellation` query parameter sets the
// cookie so users can toggle their default from a checkbox in the reveal
// strip without a separate settings page.
func (s *Server) constellationDefaultState(w http.ResponseWriter, r *http.Request) string {
	return readUIPreference(w, r, cookieConstellationOpen, "constellation", defaultConstellationState, allowedConstellationStates)
}

// agentRowView is one row in either the active or recent agent list.
//
// Info-density fields added for ka-0000000571: the audit observed that the
// right two-thirds of each row was empty whitespace. Uptime, TerminalURL,
// SlackURL, WrapUpSummary, and SpawnedBy now pack the unused width with
// the missing signal the audit called out.
type agentRowView struct {
	ID         string
	Name       string
	Role       string
	// Harness is the agent CLI the pod resolved to (claude|codex|gemini), read from
	// the controller-stamped resolved_harness field, else the spawn-time
	// harness override. Empty for beads that predate the field. Rows badge
	// only non-default harnesses (agentHarnessBadge) so the common claude
	// row is unchanged.
	Harness    string
	Project    string
	AgentState string
	PodPhase   string
	Status     string
	TaskID     string
	TaskTitle  string
	SpawnedBy  string
	// ScheduleID is the gardener trigger bead's ID, stamped onto every agent
	// bead a gardener spawns (`stampGardenerLoopContext`, design doc §9). When
	// non-empty the agent card renders a "gardener" link to /gardener/{id}
	// folding this run into its gardener's life-thread. Empty for one-off
	// (non-gardener-spawned) agents, which render nothing extra.
	ScheduleID        string
	OperatedByDisplay string
	// OperatedBySlug is the canonical per-user-homepage slug for the
	// operator (when resolvable). When non-empty, the template renders
	// the operator label as a link to /u/{slug} via {{userLink}}
	// (ka-0000007502). Empty for scheduler-driven rows and for rows
	// whose operated_by_id has no operators(id) match yet.
	OperatedBySlug string
	Desc           string
	Ago            string
	UpdatedAtRFC   string
	// Uptime is "time since pod spawn", derived from b.CreatedAt as a proxy
	// for pod boot time (agent beads are created when the pod is spawned).
	// Distinct from Ago (time since last bead activity).
	Uptime       string
	CreatedAtRFC string
	// LastProgressAt is the agent bead's last_progress_at heartbeat — stamped
	// only when a genuine, usage-bearing turn lands (it freezes the instant work
	// stops). Zero when the field is absent/unparseable. Carried so the gardener
	// page can derive a silent-gardener signal (last progress stale though runs
	// keep spawning) without re-fetching the bead (bug ka-0000006095 / A1).
	LastProgressAt time.Time
	// LongRunning flags an active agent whose uptime has crossed
	// agentUptimeWarnThreshold so the template can style the chip.
	LongRunning bool
	// Stale flags an active agent in an "alive" agent_state bucket whose
	// last bead activity exceeds agentStaleThreshold. Surfaces hung/crashed
	// pods that the alive/stuck/idle bucket alone cannot distinguish
	// (ka-0000007486).
	Stale bool
	// TerminalURL / SlackURL are populated when the corresponding deep-link
	// is configured + the bead carries the necessary fields. Empty otherwise.
	TerminalURL string
	SlackURL    string
	// WrapUpSummary is an accomplishments+blockers preview parsed from the
	// agent bead's wrapup field. Populated only for closed (terminated) rows.
	WrapUpSummary string
	// WrapUpMRs is the resolved MR-pill view for each URL the agent listed in
	// wrapup.pull_requests. Each entry is either a hit (full fields from the
	// merge-request bead) or a degraded {URL, State:"unknown"} when no MR bead
	// matches. Populated only for closed (terminated) rows. See ka-0000004478.
	WrapUpMRs []wrapUpMRView
	// WrapUpBeadsClosed is the count of bead IDs the run listed in
	// wrapup.beads_closed. Drives a "N closed" outcome chip and feeds the
	// derived-outcome fallback when the run left no prose summary. Closed rows
	// only (ka-0000001187).
	WrapUpBeadsClosed int
	// RunOutcome classifies a closed run's wrapup as "ok" (clean finish),
	// "fail" (ended blocked), or "unknown" (no structured wrap-up). Empty for
	// live rows. Drives the exit-reason chip (ka-0000001187).
	RunOutcome string
	// DerivedOutcome is a one-line outcome stitched from structured signals
	// (MRs opened, beads closed, terminal status) for closed runs that left no
	// prose wrap-up. It backstops WrapUpSummary so an empty-wrapup card still
	// carries an outcome instead of a bare title. Empty when nothing can be
	// derived (ka-0000001187).
	DerivedOutcome string
	// Usage is the pre-formatted context-size + cost summary for the row. For
	// live rows it carries the current context-window occupancy and a running
	// cost; for closed rows only the reconciled cost. Empty fields render
	// nothing (ka-0000007210).
	Usage agentUsageView

	// --- Gardener-grouping + lineage keys (task ka-0000006067). Carried on
	//     every row so the ?group=gardener view can bucket by gardener ownership
	//     and collapse each succession chain to its live head WITHOUT a second
	//     bead decode. None of these render directly in the default (project)
	//     view — they only feed buildAgentGardenerGroups. ---

	// GardenerID is the canonical gardener owner, resolved from the bead's
	// spawn-context fields via beadsapi.DeriveGardenerID (gardener_id ∨
	// schedule_id/trigger_id ∨ synthetic-gardener:<source>). Total — never empty.
	// A synthetic value (beadsapi.IsSyntheticGardenerID) routes the row into the
	// fleet-direct lane instead of a real gardener bucket.
	GardenerID string
	// LineageRootID is the agent's succession-chain root (lineage_root_id),
	// falling back to its own ID when unset — a first-of-line agent is the root
	// of a one-run chain. Groups successor incarnations under one head.
	LineageRootID string
	// PriorAgentID is the immediate succession back-pointer (prior_agent_id ∨
	// schedule_prior_agent_id). Empty for a first-of-line agent. Used to pick
	// the live head of a chain (the run nothing else points at).
	PriorAgentID string
	// CreatedAt is the agent bead's birth, retained as the tie-break ordering
	// key when collapsing a chain to its newest incarnation.
	CreatedAt time.Time
}

// wrapUpMRView is one MR pill rendered under an /agents row's wrap-up snippet.
// Sourced from a wrapup.pull_requests URL resolved against the merge-request
// bead store via daemon.MRBeadByURL. Cartesian (mr_state × pipeline_status)
// drives the pill chip palette.
type wrapUpMRView struct {
	URL            string
	IID            string
	Title          string
	State          string
	PipelineStatus string
}

// agentUptimeWarnThreshold flags an agent as long-running. Crossed → uptime
// chip is styled as a warning so operators can spot pods that have been up
// for an unusually long time at a glance.
const agentUptimeWarnThreshold = 4 * time.Hour

// agentStaleThreshold flags an "alive" agent whose last bead activity is older
// than this as stale — alive in name but not actually pushing updates. Audit
// ka-0000007486 called out that the alive/stuck/idle bucket misses this case:
// an agent that crashed mid-task still reads "alive" until the controller
// reaps it. 1h matches the global "stale in_progress" heuristic in
// gb advice ka-0000007726.
const agentStaleThreshold = time.Hour

// agentProjectGroup is one project section on /agents (active half).
type agentProjectGroup struct {
	Project string
	Agents  []agentRowView
}

// agentFleetTileView is the per-project fleet header tile rendered when
// /agents has a ?project=<name> filter applied. Mirrors /health's fleet tile
// shape (alive/stuck/idle/total) but scoped to the filtered project so an
// operator can read project liveness at a glance without round-tripping to
// /health. Counts derive from the active (open + in_progress) agent beads
// already loaded for the page — no extra daemon round-trip.
type agentFleetTileView struct {
	Project string
	Alive   int
	Stuck   int
	Idle    int
	// Dormant is the count of parked persistent identities in this project —
	// CLOSED agent beads carrying the engine-set dormant agent_state, awaiting
	// their wake (task ka-0000004675). Surfaced as its own bucket so a parked
	// identity does not read as either a live agent or a termination.
	Dormant int
	Total   int
	// Closed24h is the count of agent beads in the same project whose status
	// is closed and updated_at is within the last 24h. Sourced from the
	// recent list already loaded for the page; not a separate daemon call.
	// Surfaces "are pods churning?" signal that the alive/stuck/idle counts
	// alone hide. Window is 24h, NOT agentsRecentWindow (6h), because
	// crash-rate is a longer-horizon signal than "Recent" listing.
	Closed24h int
	// RoleBreakdown is the count of active agents in the same project
	// keyed by role label (crew, gardener, triage, etc.), sorted by count
	// desc. Empty roles fold to "untyped". Densifies the filter-scoped tile
	// (ka-0000001370) by exposing the role mix at a glance.
	RoleBreakdown []agentRoleCount
	// StateBreakdown is the count of active agents in the same project
	// keyed by raw agent_state (working, spawning, yielding, idle, …),
	// sorted by count desc. Complements the bucketed Alive/Stuck/Idle
	// numbers above by exposing the within-bucket distribution at a glance
	// (ka-0000005313): an operator can see "6 working · 3 spawning" rather
	// than just "Alive: 9". Empty agent_state folds to "idle".
	StateBreakdown []agentStateCount
}

// agentStateCount is one bucket of the raw agent_state breakdown surfaced
// on the filter-scoped fleet tile.
type agentStateCount struct {
	State string
	Count int
}

// agentRoleCount is one bucket of the role breakdown surfaced on the
// filter-scoped fleet tile.
type agentRoleCount struct {
	Role  string
	Count int
}

// agentSecondaryChip is one chip in the role or state secondary-filter
// strips rendered under the project filter on /agents?project=<name>.
// Surfaces filter-narrowing affordances inline so operators can drill
// down without leaving the page (ka-0000001370).
type agentSecondaryChip struct {
	Label  string
	Value  string // "" for the All chip
	Count  int
	Active bool
	Href   string
}

// agentStateBucket classifies an agent_state into the same alive/stuck/idle
// buckets the fleet tile and /health use. Empty string → idle. The engine-set
// dormant state gets its own bucket so a parked identity is never miscounted as
// idle (it appears only on CLOSED beads, so this is defensive — the live-fleet
// state filter still only offers alive/stuck/idle).
func agentStateBucket(state string) string {
	switch {
	case state == agentStateDormant:
		return "dormant"
	case isAgentStateIn(state, agentStateAlive):
		return "alive"
	case isAgentStateIn(state, agentStateStuck):
		return "stuck"
	default:
		return "idle"
	}
}

var allowedAgentStateBuckets = map[string]struct{}{"alive": {}, "stuck": {}, "idle": {}}

// agentStatePrewarmed is the agent_state poolmanager writes on a freshly
// created pool member (internal/poolmanager/poolmanager.go). It stays set until
// AssignPrewarmed transitions the bead to "assigning" on engagement. An agent
// in this state is a warm-standby pod that nobody has talked to yet.
const agentStatePrewarmed = "prewarmed"

// agentStateQueued is the agent_state the controller writes on a woken agent
// whose spawn it DECLINED because the project account pool is fully exhausted
// (cmd/controller/main.go SpawnBlocked path) — a transient capacity wait the
// reconciler re-attempts when a key frees up. Also the state a capacity-deferred
// pool member strands in (see isUnengagedPrewarmedAgent). Shared here so the
// /plans queued chip (ka-0000003525) and the pool-shell filter agree on the value.
const agentStateQueued = "queued"

// agentStateSpawning is the alive agent_state a crew carries while its pod is
// booting — dispatched and coming up, but not yet grinding its step. It is a
// member of agentStateAlive (so it counts as live/working everywhere), but the
// /plans crew chip surfaces it as a calm "(K starting)" sub-qualifier on the
// "· M working" facet (ka-0000005352) so a still-booting crew does not read as
// full working progress. Shared here so the index rollup and the cockpit roster
// classify a spawning crew off the SAME literal.
const agentStateSpawning = "spawning"

// isUnengagedPrewarmedAgent reports whether a bead is a prewarmed-pool agent
// that has not yet been assigned to a thread. These are warm-standby pods, not
// real work — surfacing them as idle agent cards clutters /agents with rows an
// operator can do nothing about (the pool manager owns their lifecycle). Once
// assigned, AssignPrewarmed flips agent_state to "assigning"/"working".
//
// It is NOT enough to match agent_state=="prewarmed": an unassigned pool member
// also strands in "queued" (its pod was capacity-deferred) or "idle" (the pod
// started before assignment), and those leaked into /agents as pointless rows
// (ka-0000007786). Mirror the controller's isParkedPoolMember: a member is an
// unengaged pool shell when it carries the pool's minting convention
// (agent/title prefix "prewarmed-") AND has no thread/task — an ASSIGNED member
// carries one of those and is real work we must still show.
func isUnengagedPrewarmedAgent(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	state := b.Fields["agent_state"]
	if state == agentStatePrewarmed {
		return true
	}
	if state != agentStateQueued && state != "idle" {
		return false
	}
	if b.Fields["slack_thread_ts"] != "" || b.Fields["task_id"] != "" {
		return false // assigned off the pool — real work
	}
	name := b.Fields["agent"]
	if name == "" {
		name = b.Title
	}
	return strings.HasPrefix(name, "prewarmed-")
}

// agentsOverallSummary is the page-level summary header rendered above the
// chip strip on the unfiltered /agents view. Same alive/stuck/idle buckets as
// the per-project tile, plus a recent-window terminated count. Lets an
// operator read fleet liveness without per-project drill-down — the audit
// (ka-0000000571) called this out as missing signal.
type agentsOverallSummary struct {
	Alive int
	Stuck int
	Idle  int
	// Dormant is the fleet-wide count of parked persistent identities — CLOSED
	// agent beads carrying the engine-set dormant agent_state, awaiting their
	// wake (task ka-0000004675). Its own bucket, distinct from Recent
	// (terminated): a parked identity is suspended, not dead.
	Dormant     int
	Total       int
	RecentCount int
	WindowLabel string
	// Stale is the count of active agents whose agent_state is "alive" but
	// whose last bead activity exceeds agentStaleThreshold. Surfaces hung
	// pods that read as alive but are not pushing updates (ka-0000007486).
	Stale int
	// Roles is the count of active agents per role (e.g. "crew": 4, "gardener":
	// 2). Surfaces the role mix that the audit (ka-0000004132) called out as
	// missing signal in the fleet header. Sorted by count desc then name asc
	// in buildOverallSummary so the template can render deterministically.
	Roles []agentSummaryChip
	// Projects is the count of active agents per project label. Same shape
	// as Roles. Surfaces a per-project breakdown beyond just the chip-strip
	// filter (which only counts in/out of filter, not relative weights).
	Projects []agentSummaryChip
	// RecentRuns is the last N closed agents rendered as a compact ok/fail
	// glyph strip, newest first. Densifies the fleet header with the
	// at-a-glance run history the audit (ka-0000003320) called out as missing
	// signal under "Add 'recent runs' panel under the fleet KPI (last 20 task
	// closes, with check/x glyphs)". Outcome derives from the wrapup
	// blockers field: empty → ok, non-empty → fail, missing → unknown.
	RecentRuns []agentRecentRun
	// Last24h rolls up closures, failures, and average lifetime across the
	// longer 24h history window so operators can read churn at a glance.
	// Audit ka-0000003534 called out "agents seen in last 24h / agent
	// crashes / average lifetime" as missing signal on /agents.
	Last24h *agentsLast24h
	// Sparkline is a 6-bucket hourly fleet-size strip (oldest → newest).
	// Each bucket counts agents whose lifespan covered that hour: still
	// alive, or closed-after-bucket-start. Surfaces the trend the audit
	// (ka-0000003534) asked for under "small sparkline (fleet size over
	// last 6h) next to the Alive count".
	Sparkline *agentsSparkline
}

// agentsLast24h is the rolled-up history block surfaced below the fleet
// summary. Counts and averages derive from closed agent beads whose
// updated_at falls within agentsHistoryWindow of now.
type agentsLast24h struct {
	// WindowLabel is the human-readable horizon ("24h").
	WindowLabel string
	// Closed counts every closed agent in the window.
	Closed int
	// Failed counts closures whose wrapup payload carried a non-empty
	// blockers field (treats ka-0000003320 outcome="fail" as crashes/
	// hand-offs).
	Failed int
	// AvgLifetime is the mean of (updated_at - created_at) across closed
	// agents in the window. Zero when nothing closed.
	AvgLifetime time.Duration
	// AvgLifetimeLabel renders AvgLifetime in the same "Nh / Nm / Ns" style
	// the rest of the page uses, so templates can drop it in verbatim.
	AvgLifetimeLabel string
}

// agentsSparkline is the hourly fleet-size strip rendered next to the
// Alive counter. Buckets are ordered oldest → newest; Max is the peak
// bucket value so templates can scale bar heights.
type agentsSparkline struct {
	// BucketHours is the bucket width in hours (always 1 today; surfaced
	// for the template tooltip).
	BucketHours int
	// Buckets[i] is the fleet-size estimate at the start of bucket i.
	// Length == agentsSparklineBuckets.
	Buckets []agentsSparkBucket
	// Max is the largest count across Buckets, or 1 when every bucket is
	// zero (avoids divide-by-zero in the template's height calc).
	Max int
}

// agentsSparkBucket is one column of the fleet-size sparkline.
type agentsSparkBucket struct {
	// Count is the number of agents alive at the start of this bucket.
	Count int
	// Label is the bucket's start time relative to now ("6h" → "0h"), used
	// as a hover tooltip.
	Label string
}

// agentSummaryChip is one role/project entry in the overall summary header.
type agentSummaryChip struct {
	Label string
	Count int
}

// agentRecentRun is one glyph in the recent-runs strip on /agents. Outcome is
// "ok" (agent wrapped up without blockers), "fail" (wrapup carried blockers),
// or "unknown" (no wrapup posted) so the template can choose a glyph. MRState
// (ka-0000006109) is the merge_request state of the most-recent MR produced by
// the run ("" | "merged" | "opened" | "closed" | "draft"); the template draws
// a secondary corner badge keyed by this field. MRIID carries the matching
// !iid so the tooltip can name the MR.
type agentRecentRun struct {
	ID      string
	Name    string
	Project string
	Role    string
	Outcome string // "ok" | "fail" | "platform" | "unknown"
	Ago     string
	Title   string // composed tooltip
	MRState string // "" | "merged" | "opened" | "closed" | "draft"
	MRIID   string
}

// recentRunsLimit caps the recent-runs glyph strip. Audit ka-0000003320 asked
// for "last 20 task closes" — at 20 entries the strip stays ~one row tall on
// a typical viewport while still showing enough history to read fleet
// health-over-time at a glance.
const recentRunsLimit = 20

// buildRecentRuns walks the (already-window-trimmed) closed agents list newest
// first and emits up to recentRunsLimit glyph entries. Outcome derives from
// the wrapup blockers field. Empty input → nil so the template can suppress
// the strip on a quiet fleet.
func buildRecentRuns(recent []*beadsapi.BeadDetail, now time.Time) []agentRecentRun {
	if len(recent) == 0 {
		return nil
	}
	out := make([]agentRecentRun, 0, recentRunsLimit)
	for _, b := range recent {
		if b == nil {
			continue
		}
		name := strings.TrimSpace(b.Fields["agent"])
		if name == "" {
			name = b.Title
		}
		outcome := recentRunOutcome(b)
		ago := fmtTimeAgo(b.UpdatedAt, now)
		project := agentProjectName(b)
		role := strings.TrimSpace(b.Fields["role"])
		title := name
		if role != "" {
			title += " · " + role
		}
		if project != "" {
			title += " · " + project
		}
		if ago != "" {
			title += " · " + ago
		}
		switch outcome {
		case "ok":
			title += " · clean"
		case "fail":
			if blk := strings.TrimSpace(extractWrapupBlockers(b.Fields["wrapup"])); blk != "" {
				title += " · blocked: " + snippet(blk, 100)
			} else {
				title += " · failed"
			}
		case "platform":
			title += " · platform failure (no inference turn)"
		default:
			title += " · no wrapup"
		}
		out = append(out, agentRecentRun{
			ID:      b.ID,
			Name:    name,
			Project: project,
			Role:    role,
			Outcome: outcome,
			Ago:     ago,
			Title:   title,
		})
		if len(out) >= recentRunsLimit {
			break
		}
	}
	return out
}

// decorateRecentRunsWithMRState walks the daemon's 2-hop produces graph for
// each agent bead in runs and stamps the most-recent MR's state onto the
// matching agentRecentRun. The tooltip is extended with " · <state> !<iid>"
// (or " · <state>" when no iid) so hover keeps the MR context. Best-effort:
// daemon errors degrade silently — the strip still renders without the badge.
//
// MR recency uses the MR bead's UpdatedAt; MRBeadsForAgent already dedupes
// across multiple producing tasks.
func (s *Server) decorateRecentRunsWithMRState(ctx context.Context, runs []agentRecentRun) {
	if s == nil || s.daemon == nil || len(runs) == 0 {
		return
	}
	for i := range runs {
		mrs, err := s.daemon.MRBeadsForAgent(ctx, runs[i].ID)
		if err != nil {
			s.logger.Warn("walking produced MRs for recent-runs strip", "agent_bead", runs[i].ID, "error", err)
			continue
		}
		if len(mrs) == 0 {
			continue
		}
		var pick *beadsapi.BeadDetail
		for _, mr := range mrs {
			if mr == nil {
				continue
			}
			if pick == nil || mr.UpdatedAt.After(pick.UpdatedAt) {
				pick = mr
			}
		}
		if pick == nil {
			continue
		}
		state := strings.TrimSpace(pick.Fields["mr_state"])
		if state == "" {
			state = strings.TrimSpace(pick.Fields["state"])
		}
		if state == "" {
			continue
		}
		runs[i].MRState = state
		runs[i].MRIID = strings.TrimSpace(pick.Fields["mr_iid"])
		if runs[i].MRIID != "" {
			runs[i].Title += " · " + state + " !" + runs[i].MRIID
		} else {
			runs[i].Title += " · " + state
		}
	}
}

// agentRunOutcome classifies a closed agent bead's wrapup payload as a clean
// finish ("ok"), a blocked finish ("fail"), or no wrapup at all ("unknown").
// Stays in lockstep with parseWrapUpSummary's parsing logic.
func agentRunOutcome(wrapupJSON string) string {
	wrapupJSON = strings.TrimSpace(wrapupJSON)
	if wrapupJSON == "" {
		return "unknown"
	}
	if blk := extractWrapupBlockers(wrapupJSON); blk != "" {
		return "fail"
	}
	return "ok"
}

// agentZeroInferenceTurn reports whether a bead's usage fields prove the run
// never completed a single inference turn — a hard platform failure (model
// outage or a pod-startup infra fault before first inference) rather than an
// agent that did work and forgot to wrap up (ka-0000000315). It mirrors the
// bridge render classifier (zeroInferenceTurn) and the write-side twin
// (beadZeroInferenceTurn): a request was attempted (total_request_count >= 1)
// yet no context reached the model and no tool ran (total_input_tokens == 0 &&
// total_tool_calls == 0). Requiring a counted request keeps legacy/absent-usage
// beads (all fields 0) out of the classification.
func agentZeroInferenceTurn(fields map[string]string) bool {
	if fields == nil {
		return false
	}
	if parseTokenField(fields["total_request_count"]) < 1 {
		return false
	}
	return parseTokenField(fields["total_input_tokens"]) == 0 &&
		parseTokenField(fields["total_tool_calls"]) == 0
}

// recentRunOutcome is the recent-runs-strip refinement of agentRunOutcome: it
// first checks whether the bead's usage fields mark a zero-inference-turn
// platform failure and, if so, returns "platform" so the strip renders a
// distinct platform-status glyph instead of the misleading blocked/idle glyph
// (ka-0000000315). Otherwise it falls back to the wrapup-based classification.
// Scoped to the strip so the fleet-count / gardener-accuracy callers of
// agentRunOutcome keep their existing ok/fail/unknown semantics.
func recentRunOutcome(b *beadsapi.BeadDetail) string {
	if b != nil && agentZeroInferenceTurn(b.Fields) {
		return "platform"
	}
	return agentRunOutcome(b.Fields["wrapup"])
}

// extractWrapupBlockers returns the blockers field from a wrapup JSON object,
// or empty string when the payload is missing/unparseable.
func extractWrapupBlockers(wrapupJSON string) string {
	var w struct {
		Blockers string `json:"blockers"`
	}
	if err := json.Unmarshal([]byte(wrapupJSON), &w); err != nil {
		return ""
	}
	return strings.TrimSpace(w.Blockers)
}

// agent_state enum values per gardenio/controller/internal/bridge/init.go:
// idle, spawning, working, done, failed, rate_limited, budget_exceeded.
// "yielding" is also written by the scheduler (see scheduler.go). We map them
// to fleet buckets the same way /health and the Slack dashboard do.
var (
	agentStateAlive = map[string]struct{}{"working": {}, "spawning": {}, "yielding": {}}
	agentStateStuck = map[string]struct{}{"failed": {}, "rate_limited": {}, "budget_exceeded": {}}
)

// agentStateDormant is the ENGINE-SET agent_state a PARKED persistent identity
// carries while it awaits its wake (persistent-agents Phase 2, task
// ka-0000004675). The idle-TTL park path stamps it on the CLOSED bead alongside
// successor=same + next_respawn_at. The viewer buckets a parked identity on this
// single engine-owned field rather than DERIVING dormancy from the
// (successor=same ∧ future next_respawn_at ∧ closed) combination: because both
// the wake-scanner and `gb wake` CLEAR agent_state in the atomic reopen, the row
// moves dormant → (cleared) → working monotonically and never FLAPS across the
// respawn window.
const agentStateDormant = beadsapi.AgentStateDormant

// isDormantAgent reports whether a bead is a parked persistent identity awaiting
// its wake: a CLOSED type=agent bead carrying the engine-set dormant agent_state.
// Dormant agents are counted in their own viewer bucket, NOT folded into the
// "recently terminated" listing — a park is a suspension, not a death.
func isDormantAgent(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	return b.Status == "closed" && b.Fields["agent_state"] == agentStateDormant
}

// buildProjectFleetTile classifies the active agent beads belonging to
// `project` into alive/stuck/idle/total and counts terminations in the
// last 24h from the recent list. Beads with an unrecognised
// agent_state fall into Idle (matches /health's "everything not alive/stuck
// is idle" semantics). Returns nil when project is empty.
func buildProjectFleetTile(active, recent, dormant []*beadsapi.BeadDetail, project string, now time.Time) *agentFleetTileView {
	if project == "" {
		return nil
	}
	v := &agentFleetTileView{Project: project}
	roleCounts := map[string]int{}
	stateCounts := map[string]int{}
	for _, b := range active {
		if b == nil {
			continue
		}
		if agentProjectName(b) != project {
			continue
		}
		v.Total++
		state := b.Fields["agent_state"]
		switch {
		case isAgentStateIn(state, agentStateAlive):
			v.Alive++
		case isAgentStateIn(state, agentStateStuck):
			v.Stuck++
		default:
			v.Idle++
		}
		role := strings.TrimSpace(b.Fields["role"])
		if role == "" {
			role = "untyped"
		}
		roleCounts[role]++
		rawState := strings.TrimSpace(state)
		if rawState == "" {
			rawState = "idle"
		}
		stateCounts[rawState]++
	}
	cutoff := now.Add(-24 * time.Hour)
	for _, b := range recent {
		if b == nil {
			continue
		}
		if agentProjectName(b) != project {
			continue
		}
		// A parked identity is closed; keep it out of the crash-rate signal.
		if isDormantAgent(b) {
			continue
		}
		if b.UpdatedAt.Before(cutoff) {
			continue
		}
		v.Closed24h++
	}
	// Dormant (parked persistent identities) come from the dedicated engine-set
	// query, so the count is window-independent — a long-parked identity is still
	// counted here even after it ages past the 24h crash-rate window above.
	for _, b := range dormant {
		if b == nil || agentProjectName(b) != project {
			continue
		}
		if isDormantAgent(b) {
			v.Dormant++
		}
	}
	// Sort role breakdown by count desc, role asc for ties — stable order
	// across renders so the chip strip doesn't shuffle on every refresh.
	roles := make([]agentRoleCount, 0, len(roleCounts))
	for role, count := range roleCounts {
		roles = append(roles, agentRoleCount{Role: role, Count: count})
	}
	sort.Slice(roles, func(i, j int) bool {
		if roles[i].Count != roles[j].Count {
			return roles[i].Count > roles[j].Count
		}
		return roles[i].Role < roles[j].Role
	})
	v.RoleBreakdown = roles
	states := make([]agentStateCount, 0, len(stateCounts))
	for state, count := range stateCounts {
		states = append(states, agentStateCount{State: state, Count: count})
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].Count != states[j].Count {
			return states[i].Count > states[j].Count
		}
		return states[i].State < states[j].State
	})
	v.StateBreakdown = states
	return v
}

// buildSecondaryChips builds the role + state filter-narrowing chip strips
// for the filtered /agents view. Counts reflect the rows that match the
// current project filter (one dimension only, so "All" in the role strip
// shows totals across all roles for the active state filter, and vice
// versa). Hrefs preserve density + opposite-dimension state via callbacks
// that the handler supplies.
//
// Returns role chips and state chips. Each strip's "All" chip clears that
// dimension. Empty when the inputs produce no rows.
func buildSecondaryChips(active []*beadsapi.BeadDetail, project, roleFilter, stateFilter string, hrefFn func(role, state string) string) (roleChips, stateChips []agentSecondaryChip) {
	if project == "" {
		return nil, nil
	}
	// Build role counts using the current STATE filter (so the role chip
	// strip shows what each role narrows TO under the chosen state).
	roleCounts := map[string]int{}
	roleAll := 0
	// State counts use the current ROLE filter, symmetric to above.
	stateCounts := map[string]int{}
	stateAll := 0
	for _, b := range active {
		if b == nil {
			continue
		}
		if agentProjectName(b) != project {
			continue
		}
		role := strings.TrimSpace(b.Fields["role"])
		if role == "" {
			role = "untyped"
		}
		bucket := agentStateBucket(b.Fields["agent_state"])
		// Role-strip counts: filter by state if active.
		if stateFilter == "" || stateFilter == bucket {
			roleCounts[role]++
			roleAll++
		}
		// State-strip counts: filter by role if active.
		if roleFilter == "" || roleFilter == role {
			stateCounts[bucket]++
			stateAll++
		}
	}
	// Role chips: All + each role sorted by count desc, role asc.
	roleChips = append(roleChips, agentSecondaryChip{
		Label:  "All",
		Value:  "",
		Count:  roleAll,
		Active: roleFilter == "",
		Href:   hrefFn("", stateFilter),
	})
	roleNames := make([]string, 0, len(roleCounts))
	for name := range roleCounts {
		roleNames = append(roleNames, name)
	}
	sort.Slice(roleNames, func(i, j int) bool {
		ci, cj := roleCounts[roleNames[i]], roleCounts[roleNames[j]]
		if ci != cj {
			return ci > cj
		}
		return roleNames[i] < roleNames[j]
	})
	for _, name := range roleNames {
		roleChips = append(roleChips, agentSecondaryChip{
			Label:  name,
			Value:  name,
			Count:  roleCounts[name],
			Active: roleFilter == name,
			Href:   hrefFn(name, stateFilter),
		})
	}
	// State chips: fixed order alive → stuck → idle (matches fleet tile
	// reading order) + All.
	stateChips = append(stateChips, agentSecondaryChip{
		Label:  "All",
		Value:  "",
		Count:  stateAll,
		Active: stateFilter == "",
		Href:   hrefFn(roleFilter, ""),
	})
	for _, bucket := range []string{"alive", "stuck", "idle"} {
		stateChips = append(stateChips, agentSecondaryChip{
			Label:  bucket,
			Value:  bucket,
			Count:  stateCounts[bucket],
			Active: stateFilter == bucket,
			Href:   hrefFn(roleFilter, bucket),
		})
	}
	return roleChips, stateChips
}

func isAgentStateIn(s string, set map[string]struct{}) bool {
	_, ok := set[s]
	return ok
}

// isAgentStale reports whether an active agent bead reads as "alive" but
// has not pushed an update in agentStaleThreshold. Closed beads and
// non-alive states return false. UpdatedAt zero (uninitialised) also
// returns false so test fixtures without timestamps don't spuriously
// flag every agent stale.
func isAgentStale(b *beadsapi.BeadDetail, now time.Time) bool {
	if b == nil {
		return false
	}
	if !isAgentStateIn(b.Fields["agent_state"], agentStateAlive) {
		return false
	}
	if b.UpdatedAt.IsZero() {
		return false
	}
	return now.Sub(b.UpdatedAt) >= agentStaleThreshold
}

// buildOverallSummary classifies all active agent beads (no project filter)
// into alive/stuck/idle/total — the same buckets the per-project tile uses.
// Returns nil when the active set is empty so the template can suppress the
// header strip on a quiet page.
//
// `recent` is the 6h closed-agent slice used for RecentRuns; `history` is
// the broader 24h slice (a superset of `recent`) used for the Last24h
// summary block and the fleet-size sparkline.
func buildOverallSummary(active, recent, dormant, history []*beadsapi.BeadDetail, recentCount int, windowLabel string, now time.Time) *agentsOverallSummary {
	dormantCount := 0
	for _, b := range dormant {
		if isDormantAgent(b) {
			dormantCount++
		}
	}
	if len(active) == 0 && recentCount == 0 && len(history) == 0 && dormantCount == 0 {
		return nil
	}
	v := &agentsOverallSummary{RecentCount: recentCount, Dormant: dormantCount, WindowLabel: windowLabel}
	roleCounts := map[string]int{}
	projectCounts := map[string]int{}
	for _, b := range active {
		if b == nil {
			continue
		}
		v.Total++
		state := b.Fields["agent_state"]
		switch {
		case isAgentStateIn(state, agentStateAlive):
			v.Alive++
			if isAgentStale(b, now) {
				v.Stale++
			}
		case isAgentStateIn(state, agentStateStuck):
			v.Stuck++
		default:
			v.Idle++
		}
		if role := strings.TrimSpace(b.Fields["role"]); role != "" {
			roleCounts[role]++
		}
		projectCounts[agentProjectName(b)]++
	}
	v.Roles = sortedSummaryChips(roleCounts)
	v.Projects = sortedSummaryChips(projectCounts)
	v.RecentRuns = buildRecentRuns(recent, now)
	v.Last24h = buildLast24h(history, now)
	v.Sparkline = buildSparkline(active, history, now)
	return v
}

// buildLast24h walks the 24h closed-agent slice and returns the rolled-up
// counts + average lifetime surfaced under the fleet summary. Returns nil
// when nothing closed in the window so the template can suppress the block.
func buildLast24h(history []*beadsapi.BeadDetail, now time.Time) *agentsLast24h {
	if len(history) == 0 {
		return nil
	}
	cutoff := now.Add(-agentsHistoryWindow)
	v := &agentsLast24h{WindowLabel: shortDuration(agentsHistoryWindow)}
	var lifetimeSum time.Duration
	lifetimeN := 0
	for _, b := range history {
		if b == nil || b.UpdatedAt.Before(cutoff) {
			continue
		}
		v.Closed++
		if agentRunOutcome(b.Fields["wrapup"]) == "fail" {
			v.Failed++
		}
		if !b.CreatedAt.IsZero() && !b.UpdatedAt.Before(b.CreatedAt) {
			lifetimeSum += b.UpdatedAt.Sub(b.CreatedAt)
			lifetimeN++
		}
	}
	if v.Closed == 0 {
		return nil
	}
	if lifetimeN > 0 {
		v.AvgLifetime = lifetimeSum / time.Duration(lifetimeN)
		v.AvgLifetimeLabel = shortLifetime(v.AvgLifetime)
	}
	return v
}

// shortLifetime renders a duration in the compact "Nh / Nm / Ns" style the
// fleet summary uses elsewhere. Differs from shortDuration in that it
// floors to the largest non-zero unit so a 73-minute average renders "1h"
// rather than "1h13m0s".
func shortLifetime(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	switch {
	case d >= time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	case d >= time.Minute:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
}

// buildSparkline returns a 6-bucket hourly fleet-size strip (oldest →
// newest). Bucket i covers [now-(N-i)h, now-(N-i-1)h]; its count is the
// number of agents whose lifespan covered the bucket's start instant:
//
//   - active agents with created_at on/before bucket start; AND
//   - closed agents whose created_at <= bucket start AND updated_at >= bucket start.
//
// Returns nil when both inputs are empty.
func buildSparkline(active, history []*beadsapi.BeadDetail, now time.Time) *agentsSparkline {
	if len(active) == 0 && len(history) == 0 {
		return nil
	}
	v := &agentsSparkline{
		BucketHours: 1,
		Buckets:     make([]agentsSparkBucket, agentsSparklineBuckets),
		Max:         1,
	}
	for i := range agentsSparklineBuckets {
		// Oldest bucket first: i=0 → (N) hours ago. The instant we sample
		// is the bucket START, so i=0 samples N hours ago and i=N-1
		// samples 1 hour ago.
		hoursAgo := agentsSparklineBuckets - i
		bucketStart := now.Add(-time.Duration(hoursAgo) * time.Hour)
		count := 0
		for _, b := range active {
			if b == nil || b.CreatedAt.IsZero() {
				continue
			}
			if !b.CreatedAt.After(bucketStart) {
				count++
			}
		}
		for _, b := range history {
			if b == nil || b.CreatedAt.IsZero() {
				continue
			}
			if b.CreatedAt.After(bucketStart) {
				continue
			}
			if b.UpdatedAt.Before(bucketStart) {
				continue
			}
			count++
		}
		v.Buckets[i] = agentsSparkBucket{
			Count: count,
			Label: strconv.Itoa(hoursAgo) + "h ago: " + strconv.Itoa(count) + " agents",
		}
		if count > v.Max {
			v.Max = count
		}
	}
	return v
}

// sortedSummaryChips returns the count map as a slice sorted by count desc
// then label asc. Deterministic ordering so the template renders chips in a
// stable position across reloads.
func sortedSummaryChips(counts map[string]int) []agentSummaryChip {
	if len(counts) == 0 {
		return nil
	}
	out := make([]agentSummaryChip, 0, len(counts))
	for label, count := range counts {
		out = append(out, agentSummaryChip{Label: label, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label)
	})
	return out
}

// parseWrapUpSummary extracts a one-line preview from the agent's wrapup
// JSON. The validated `tldr` (gb stop --wrapup) is featured FIRST when present
// — matching the bridge's "TLDR is the headline" convention (bead ka-0000006621)
// — then falls back to accomplishments + blockers (with "blocked:" prefix).
// Empty when wrapup is missing or carries none of those fields. Truncated to
// fit a row chip without wrapping — full wrap-up is still on the bead page.
func parseWrapUpSummary(wrapupJSON string) string {
	wrapupJSON = strings.TrimSpace(wrapupJSON)
	if wrapupJSON == "" {
		return ""
	}
	var w struct {
		TLDR            string `json:"tldr"`
		Accomplishments string `json:"accomplishments"`
		Blockers        string `json:"blockers"`
	}
	if err := json.Unmarshal([]byte(wrapupJSON), &w); err != nil {
		return snippet(wrapupJSON, 140)
	}
	if tldr := strings.TrimSpace(w.TLDR); tldr != "" {
		return snippet(unescapeLegacyWrapUpTLDR(tldr), 200)
	}
	acc := strings.TrimSpace(w.Accomplishments)
	blk := strings.TrimSpace(w.Blockers)
	switch {
	case acc != "" && blk != "":
		return snippet(acc+" · blocked: "+blk, 200)
	case acc != "":
		return snippet(acc, 200)
	case blk != "":
		return snippet("blocked: "+blk, 200)
	}
	return ""
}

// unescapeLegacyWrapUpTLDR reverses the angle-bracket escaping that the wrap-up
// writer (cmd/gb/wrapup.go normalizeAndValidateTLDR) applied to the tldr BEFORE
// ka-0000005806. Those legacy beads stored the value pre-escaped ("<2h" as
// "&lt;2h"), and the beads-viewer then renders every tldr through Go
// html/template's auto-escape — which escaped the leading '&' AGAIN, so the
// operator saw literal "&lt;2h" / "P1-&gt;P0" on the gardener page.
//
// The writer only ever substituted '<'→"&lt;" and '>'→"&gt;" (it left '&'
// untouched to avoid clobbering author-typed entities), so reversing exactly
// those two entities is the precise inverse and cannot corrupt a fresh raw
// tldr — a raw value never contains "&lt;"/"&gt;" unless the agent literally
// typed them, which the old writer would itself have mangled. New (post-fix)
// beads store raw text, so this is a no-op for them and the single
// render-time html/template escape produces correct output ("&lt;2h").
func unescapeLegacyWrapUpTLDR(tldr string) string {
	if !strings.Contains(tldr, "&lt;") && !strings.Contains(tldr, "&gt;") {
		return tldr
	}
	return strings.NewReplacer("&lt;", "<", "&gt;", ">").Replace(tldr)
}

// agentCardTLDRView is the featured wrap-up headline rendered at the TOP of the
// beads-viewer HTML agent card (type_agent.html), mirroring the Slack card's
// "TLDR is the banner" convention (bead ka-0000006621). Empty Text suppresses
// the whole banner so live/never-wrapped agents render unchanged.
type agentCardTLDRView struct {
	// Text is the one-line summary — the validated `tldr` when present, else a
	// derived fallback (see buildAgentCardTLDR). Already trimmed + length-capped.
	Text string
	// Glyph is the outcome emoji (✅ done, ⛔ blocked, ↗️ handed-off, 🛰️ platform
	// failure) shown ahead of the text so the outcome is legible at a glance.
	Glyph string
	// Outcome is the CSS-suffix class ("ok" | "fail" | "handoff" | "platform")
	// the template appends to `agent-tldr-` so light/dark skins can tint the
	// banner border per outcome.
	Outcome string
}

// agentCardTLDRDisplayCap bounds the featured headline on the HTML card. The
// writer field (gb stop --wrapup, bead ka-0000005783) already validates tldr to
// one line, so this only defends against a legacy/oversized value distorting
// the banner.
const agentCardTLDRDisplayCap = 280

// buildAgentCardTLDR derives the featured headline banner for the HTML agent
// card. It is populated only for terminal (closed) agent beads — a live agent
// has no wrap-up yet — and features the validated `tldr` first, matching the
// bridge's Slack-card headline ladder (bead ka-0000006621):
//
//   - a real wrap-up `tldr`               → that text, outcome-classed
//   - wrap-up but no tldr                 → first non-empty line of accomplishments
//   - no wrap-up at all                   → derived from close_reason
//
// Returns a zero view (empty Text) for live agents and for terminal beads that
// carry no headline-worthy signal, so the template omits the banner entirely.
func buildAgentCardTLDR(bead *beadsapi.BeadDetail) agentCardTLDRView {
	if bead == nil || bead.Fields == nil {
		return agentCardTLDRView{}
	}
	// Only terminal runs have a settled wrap-up; a live agent's card keeps the
	// working-state header untouched.
	live := bead.Status != "closed" && isAgentStateIn(bead.Fields["agent_state"], agentStateAlive)
	if live {
		return agentCardTLDRView{}
	}

	wrapup := strings.TrimSpace(bead.Fields["wrapup"])

	// Platform failure (ka-0000000315): a zero-inference-turn run never got a
	// model response. Its synthesized tldr reads as agent laziness, so re-badge
	// the banner as a platform-status headline — but a genuine human tldr wins.
	var w struct {
		TLDR            string `json:"tldr"`
		Accomplishments string `json:"accomplishments"`
		Blockers        string `json:"blockers"`
		HandoffNotes    string `json:"handoff_notes"`
		Synthesized     string `json:"synthesized"`
	}
	if wrapup != "" {
		_ = json.Unmarshal([]byte(wrapup), &w)
	}
	// A genuinely human-authored wrap-up (mirrors the bridge's hasHumanWrapUp):
	// a non-empty tldr/accomplishments that is NOT the close-time synthesized
	// fallback. The synthesized fallback's tldr reads as agent laziness, so it
	// must not suppress the platform-failure re-badge below.
	humanTLDR := strings.TrimSpace(w.Synthesized) != "true" &&
		(strings.TrimSpace(w.TLDR) != "" || strings.TrimSpace(w.Accomplishments) != "")
	if agentZeroInferenceTurn(bead.Fields) && !humanTLDR {
		return agentCardTLDRView{
			Text:    snippet("Platform failure — the agent never got an inference turn (no model response before teardown)", agentCardTLDRDisplayCap),
			Glyph:   "🛰️",
			Outcome: "platform",
		}
	}

	// Headline text ladder: validated tldr → accomplishments → close_reason.
	// The tldr is defensively un-escaped for legacy pre-escaped beads
	// (ka-0000005806) so the template's single render-time escape is correct.
	text := unescapeLegacyWrapUpTLDR(strings.TrimSpace(w.TLDR))
	if text == "" {
		text = strings.TrimSpace(w.Accomplishments)
	}
	if text == "" {
		text = strings.TrimSpace(bead.Fields["close_reason"])
	}
	if text == "" {
		return agentCardTLDRView{}
	}

	// Outcome glyph/class: blockers → blocked, handoff_notes → handed-off, else
	// clean finish. Mirrors the bridge's wrapUpOutcome ladder (blocked beats
	// handoff beats done).
	glyph, outcome := "✅", "ok"
	switch {
	case strings.TrimSpace(w.Blockers) != "":
		glyph, outcome = "⛔", "fail"
	case strings.TrimSpace(w.HandoffNotes) != "":
		glyph, outcome = "↗️", "handoff"
	}
	return agentCardTLDRView{
		Text:    snippet(text, agentCardTLDRDisplayCap),
		Glyph:   glyph,
		Outcome: outcome,
	}
}

// wrapUpBeadsClosedCount returns the number of bead IDs the run listed in
// wrapup.beads_closed. Zero when the field is missing/empty/unparseable.
func wrapUpBeadsClosedCount(wrapupJSON string) int {
	wrapupJSON = strings.TrimSpace(wrapupJSON)
	if wrapupJSON == "" {
		return 0
	}
	var w struct {
		BeadsClosed []string `json:"beads_closed"`
	}
	if err := json.Unmarshal([]byte(wrapupJSON), &w); err != nil {
		return 0
	}
	n := 0
	for _, id := range w.BeadsClosed {
		if strings.TrimSpace(id) != "" {
			n++
		}
	}
	return n
}

// derivedRunOutcome stitches a one-line outcome from a closed run's structured
// signals when it left no prose wrap-up (no tldr / accomplishments / blockers).
// It backstops the card so an empty-wrapup iteration still shows what it did —
// MRs opened, beads closed, and a terminal-status phrase — instead of a bare
// title. Returns "" when nothing meaningful can be derived (callers then fall
// back to the task title / description). See ka-0000001187.
func derivedRunOutcome(mrCount, beadsClosed int, outcome string) string {
	var parts []string
	if mrCount == 1 {
		parts = append(parts, "1 MR opened")
	} else if mrCount > 1 {
		parts = append(parts, fmt.Sprintf("%d MRs opened", mrCount))
	}
	if beadsClosed == 1 {
		parts = append(parts, "1 bead closed")
	} else if beadsClosed > 1 {
		parts = append(parts, fmt.Sprintf("%d beads closed", beadsClosed))
	}
	if len(parts) == 0 {
		// No work signals — fall back to a bare terminal-status phrase so a
		// blocked/clean finish still reads as an outcome, but only when we can
		// say something more than "ran". "unknown" stays empty so the card
		// degrades to the task title.
		switch outcome {
		case "fail":
			return "ended blocked — no wrap-up"
		case "ok":
			return "finished — no wrap-up"
		}
		return ""
	}
	return strings.Join(parts, " · ")
}

// wrapupMRResolver is the subset of beadAPI ResolveWrapupMRs uses to translate
// a wrapup.pull_requests URL into a merge-request bead. Narrowing it to one
// method keeps the test fake trivial and decouples this helper from the broader
// daemon interface.
type wrapupMRResolver interface {
	MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error)
}

// ResolveWrapupMRs parses wrapup.pull_requests out of an agent's wrap-up JSON
// and resolves each URL into a wrapUpMRView pill. On a hit the pill carries
// the merge-request bead's mr_iid / mr_title / mr_state / pipeline_status; on
// a miss the pill falls back to a degraded {URL, State:"unknown"} entry so
// the operator still sees a deep-link rather than a silent drop.
//
// Returns nil when the wrap-up is empty, unparseable, or carries no URLs.
// daemon errors per-URL are swallowed and degraded to "unknown" — losing one
// row should not blank the whole /agents view.
func ResolveWrapupMRs(ctx context.Context, daemon wrapupMRResolver, wrapupJSON string) []wrapUpMRView {
	wrapupJSON = strings.TrimSpace(wrapupJSON)
	if wrapupJSON == "" {
		return nil
	}
	var w struct {
		PullRequests []string `json:"pull_requests"`
	}
	if err := json.Unmarshal([]byte(wrapupJSON), &w); err != nil {
		return nil
	}
	if len(w.PullRequests) == 0 {
		return nil
	}
	out := make([]wrapUpMRView, 0, len(w.PullRequests))
	for _, raw := range w.PullRequests {
		url := strings.TrimSpace(raw)
		if url == "" {
			continue
		}
		view := wrapUpMRView{URL: url, State: "unknown"}
		if daemon != nil {
			if mr, err := daemon.MRBeadByURL(ctx, url); err == nil && mr != nil {
				view = wrapUpMRViewFromBead(url, mr)
			}
		}
		out = append(out, view)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// wrapUpMRViewFromBead projects a merge-request bead onto the pill view-model.
// Falls back to mr_state="unknown" only when the bead carries no state at all
// (mirrors the degraded miss path).
func wrapUpMRViewFromBead(url string, mr *beadsapi.BeadDetail) wrapUpMRView {
	v := wrapUpMRView{URL: url}
	if f := mr.Fields["mr_url"]; f != "" {
		v.URL = f
	}
	v.IID = mr.Fields["mr_iid"]
	v.Title = mr.Fields["mr_title"]
	if v.Title == "" {
		v.Title = mr.Title
	}
	v.State = mr.Fields["mr_state"]
	if v.State == "" {
		v.State = "unknown"
	}
	v.PipelineStatus = mr.Fields["mr_pipeline_status"]
	if v.PipelineStatus == "" {
		v.PipelineStatus = mr.Fields["pipeline_status"]
	}
	return v
}

// buildAgentProducedMRs walks the 2-hop produces graph from the given agent
// bead and projects each MR bead onto the shared mr_pill view shape. Returns
// an empty slice on daemon error or when nothing is linked — the type_agent
// template omits the section entirely when the slice is empty.
func (s *Server) buildAgentProducedMRs(ctx context.Context, agentBeadID string) []wrapUpMRView {
	if s.daemon == nil || agentBeadID == "" {
		return nil
	}
	mrs, err := s.daemon.MRBeadsForAgent(ctx, agentBeadID)
	if err != nil {
		s.logger.Warn("walking produced MRs for agent", "agent", agentBeadID, "error", err)
		return nil
	}
	if len(mrs) == 0 {
		return nil
	}
	out := make([]wrapUpMRView, 0, len(mrs))
	for _, mr := range mrs {
		if mr == nil {
			continue
		}
		url := mr.Fields["mr_url"]
		if url == "" {
			// Skip beads with no canonical URL — the pill anchor would
			// dead-link otherwise.
			continue
		}
		out = append(out, wrapUpMRViewFromBead(url, mr))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// handleAgents renders /agents — running agent fleet grouped by project,
// followed by a Recent section of terminated agents. Mirrors iOS AgentsTab.
//
// Optional ?project=<name> filters both lists to one project.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	roleFilter := strings.TrimSpace(r.URL.Query().Get("role"))
	stateFilter := strings.TrimSpace(r.URL.Query().Get("state"))
	// Grouping mode: "project" (default — flat per-project sections) or
	// "gardener" (bucket the live fleet by canonical gardener ownership with
	// collapsible lineage heads, task ka-0000006067). Any other value falls back
	// to the project default.
	groupMode := strings.TrimSpace(r.URL.Query().Get("group"))
	if groupMode != "gardener" {
		groupMode = "project"
	}
	// Secondary filters are only honored alongside a project filter: they
	// would just thrash an unfiltered fleet view otherwise (ka-0000001370).
	if projectFilter == "" {
		roleFilter = ""
		stateFilter = ""
	}
	if _, ok := allowedAgentStateBuckets[stateFilter]; !ok && stateFilter != "" {
		stateFilter = ""
	}

	data := map[string]any{
		"ProjectFilter":     projectFilter,
		"RoleFilter":        roleFilter,
		"StateFilter":       stateFilter,
		"GroupMode":         groupMode,
		"Chips":             []filterChip{},
		"RoleChips":         []agentSecondaryChip{},
		"StateChips":        []agentSecondaryChip{},
		"Groups":            []agentProjectGroup{},
		"GardenerGroups":    []agentGardenerBucket{},
		"Recent":            []agentRowView{},
		"Dormant":           []agentRowView{},
		"ActiveCount":       0,
		"RecentCount":       0,
		"DormantCount":      0,
		"FeedMode":          s.feedMode(w, r),
		"FeedDensity":       s.feedDensity(w, r),
		"Fleet":             (*agentFleetTileView)(nil),
		"RecentWindowLabel": shortDuration(agentsRecentWindow),
		"GardenHref":        s.gardenBanner(r, retireAgents, gardenAgentsTarget),
	}

	// Three queries: active (open + in_progress), recent (closed), and dormant
	// (closed + engine-set agent_state=dormant). Mirrors iOS KbeadsStore
	// agentBeads / recentAgentBeads plus the parked-identity bucket. Independent —
	// run them concurrently so /agents wall clock is the slowest of the three
	// rather than their sum.
	var (
		active, recent, dormant *beadsapi.ListBeadsResult
		activeErr               error
		wg                      sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		active, activeErr = s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"open", "in_progress"},
			Sort:     "updated_at",
			Limit:    agentsActiveLimit,
		})
	}()
	go func() {
		defer wg.Done()
		// Recent is best-effort: if the daemon errors here we still render
		// the active fleet — losing the Recent section is a downgrade, not
		// a failure. Errors are logged and we substitute an empty result
		// before reading from `recent` below.
		var err error
		recent, err = s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"closed"},
			Sort:     "-updated_at",
			Limit:    agentsRecentLimit,
		})
		if err != nil {
			s.logger.Warn("listing recent agents", "error", err)
			recent = &beadsapi.ListBeadsResult{}
		}
	}()
	go func() {
		defer wg.Done()
		// Dormant is best-effort too: a dedicated, window-INDEPENDENT query for
		// parked persistent identities (CLOSED agent beads carrying the engine-set
		// agent_state=dormant). This is the "engine-set not derived" bucket the
		// task calls for — the daemon filters server-side on the one field the
		// engine owns, so the row can never flap between dormant and terminated
		// during the respawn window as derived multi-field logic would
		// (ka-0000004675).
		var err error
		dormant, err = s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"closed"},
			Fields:   map[string]string{beadsapi.FieldAgentState: agentStateDormant},
			Sort:     "-updated_at",
			Limit:    agentsDormantLimit,
		})
		if err != nil {
			s.logger.Warn("listing dormant agents", "error", err)
			dormant = &beadsapi.ListBeadsResult{}
		}
	}()
	wg.Wait()

	if activeErr != nil {
		s.logger.Error("listing active agents", "error", activeErr)
		data["Error"] = "Failed to load agents."
		s.render(w, r, "agents.html", data)
		return
	}

	// Drop unengaged prewarmed-pool members before any downstream
	// classification. They are open type=agent beads with agent_state=prewarmed
	// (warm-standby pods the pool manager owns); folding them into the idle
	// bucket showed them as agent cards before anyone engaged them
	// (ka-0000005111). Once assigned they flip to "assigning"/"working" and
	// reappear here as real work. Filtering the shared slice keeps chip counts,
	// the fleet tile, and the overall summary consistent — they all read from
	// active.Beads below.
	if active != nil {
		kept := active.Beads[:0]
		for _, b := range active.Beads {
			if isUnengagedPrewarmedAgent(b) {
				continue
			}
			kept = append(kept, b)
		}
		active.Beads = kept
	}

	// Resolve operators(id) → /u/{slug} for both lists so the `op …` meta
	// chip on each row renders as a link to the operator's homepage
	// (ka-0000007502). populateOperatedBySlugs is nil-safe and best-effort:
	// rows with no slug fall back to plain text in the template.
	s.populateOperatedBySlugs(r.Context(), active.Beads)
	s.populateOperatedBySlugs(r.Context(), recent.Beads)
	s.populateOperatedBySlugs(r.Context(), dormant.Beads)

	now := time.Now()

	// Trim the fetched closed beads twice: once to the 24h history window
	// (used by the Last24h block and sparkline), once to the 6h Recent
	// window for the listing below the live fleet. The daemon has no
	// "updated_after" filter, so without these guards ancient closed agents
	// leak in whenever recent activity is sparse (ka-0000001416).
	historyCutoff := now.Add(-agentsHistoryWindow)
	recentCutoff := now.Add(-agentsRecentWindow)
	history := make([]*beadsapi.BeadDetail, 0, len(recent.Beads))
	filteredRecent := recent.Beads[:0]
	for _, b := range recent.Beads {
		if b == nil {
			continue
		}
		// A parked (dormant) identity is CLOSED, so it also matches the recent
		// closed-agent query. Exclude it from both the history and Recent
		// partitions so it counts ONLY in the dedicated dormant bucket — a parked
		// identity is neither a termination (Last24h/failures) nor a
		// recently-finished run. This is the anti-flap guarantee: the row lives in
		// exactly one bucket, keyed off the engine-set state, never split between
		// "recent" and "dormant" as its updated_at ages.
		if isDormantAgent(b) {
			continue
		}
		if b.UpdatedAt.Before(historyCutoff) {
			continue
		}
		history = append(history, b)
		if b.UpdatedAt.Before(recentCutoff) {
			continue
		}
		filteredRecent = append(filteredRecent, b)
	}
	recent.Beads = filteredRecent

	// Build chip strip from the union of project labels across both lists.
	// Counts include both active + recent (matches iOS ProjectChip semantics).
	totalCounts := map[string]int{}
	for _, b := range active.Beads {
		if b == nil {
			continue
		}
		totalCounts[agentProjectName(b)]++
	}
	for _, b := range recent.Beads {
		if b == nil {
			continue
		}
		totalCounts[agentProjectName(b)]++
	}
	allTotal := 0
	for _, c := range totalCounts {
		allTotal += c
	}
	chipNames := make([]string, 0, len(totalCounts))
	for name := range totalCounts {
		chipNames = append(chipNames, name)
	}
	sort.Slice(chipNames, func(i, j int) bool {
		return strings.ToLower(chipNames[i]) < strings.ToLower(chipNames[j])
	})
	// Href preserves the density preference across a project-filter click,
	// matching the strip's pre-consolidation inline template logic
	// (ka-0000005357). Computed here so the shared filter_strip partial can
	// consume a clean filterChip slice.
	density := s.feedDensity(w, r)
	agentChipHref := func(project string) string {
		// Order is project then density to match the strip's original inline
		// template (?project=X&density=compact / ?density=compact).
		u := s.basePath + "/agents"
		var params []string
		if project != "" {
			params = append(params, "project="+url.QueryEscape(project))
		}
		if density == "compact" {
			params = append(params, "density=compact")
		}
		if len(params) > 0 {
			u += "?" + strings.Join(params, "&")
		}
		return u
	}
	chips := []filterChip{
		{Label: "All", Value: "", Count: allTotal, Active: projectFilter == "", Href: agentChipHref("")},
	}
	for _, name := range chipNames {
		chips = append(chips, filterChip{
			Label:  name,
			Value:  name,
			Count:  totalCounts[name],
			Active: projectFilter == name,
			Href:   agentChipHref(name),
		})
	}
	data["Chips"] = chips

	// Group active agents by project, applying the optional filters. When a
	// project filter is set, role / state filters narrow the rendered rows
	// further (ka-0000001370). Filtering only the rendered rows preserves
	// chip counts: the secondary-chip strip and fleet tile are built from
	// the unnarrowed project subset so each chip's count reflects what
	// switching to it would show.
	activeRows := make([]agentRowView, 0, len(active.Beads))
	groupsMap := map[string][]agentRowView{}
	for _, b := range active.Beads {
		if b == nil {
			continue
		}
		project := agentProjectName(b)
		if projectFilter != "" && project != projectFilter {
			continue
		}
		if roleFilter != "" {
			role := strings.TrimSpace(b.Fields["role"])
			if role == "" {
				role = "untyped"
			}
			if role != roleFilter {
				continue
			}
		}
		if stateFilter != "" {
			if agentStateBucket(b.Fields["agent_state"]) != stateFilter {
				continue
			}
		}
		row := s.agentRowFromBeadCtx(r.Context(), b, project, now)
		groupsMap[project] = append(groupsMap[project], row)
		activeRows = append(activeRows, row)
	}
	groupNames := make([]string, 0, len(groupsMap))
	for name := range groupsMap {
		groupNames = append(groupNames, name)
	}
	sort.Slice(groupNames, func(i, j int) bool {
		return strings.ToLower(groupNames[i]) < strings.ToLower(groupNames[j])
	})
	groups := make([]agentProjectGroup, 0, len(groupNames))
	for _, name := range groupNames {
		// Daemon already sorted by updated_at desc — preserve insertion order.
		groups = append(groups, agentProjectGroup{Project: name, Agents: groupsMap[name]})
	}
	data["Groups"] = groups
	data["ActiveCount"] = len(activeRows)

	// Gardener grouping (task ka-0000006067): re-bucket the SAME active rows by
	// canonical gardener ownership with collapsible lineage heads. Built only when
	// the toggle is on so the default view pays no gardener-fetch cost. Uses the
	// filtered activeRows so project/role/state filters compose with the gardener
	// grouping exactly as they do with project grouping.
	if groupMode == "gardener" {
		data["GardenerGroups"] = s.buildAgentGardenerGroups(r.Context(), activeRows)
	}

	// Project-fleet header tile — only when a project filter is active so the
	// unfiltered /agents view stays unchanged. Counts from the active list
	// already in memory.
	if projectFilter != "" {
		data["Fleet"] = buildProjectFleetTile(active.Beads, recent.Beads, dormant.Beads, projectFilter, now)
		// Project-context strip: pull the same open/blocked/owners signal
		// /p/<project> renders, so /agents?project=<name> stops being a
		// duplicate of /agents with only a banner difference (ka-0000004331).
		// Best-effort: a daemon error here only hides the strip; the page
		// still renders the fleet tile and agent table.
		if ctx, ctxErr := buildProjectSummary(r.Context(), s.daemon, projectFilter, now); ctxErr == nil {
			data["ProjectContext"] = ctx
		} else {
			s.logger.Warn("building agents project-context summary", "project", projectFilter, "error", ctxErr)
		}
		// Secondary filter chips (role + state). Hrefs encode the
		// remaining filter dimensions verbatim so clicking one chip
		// only changes that one dimension. Density is cookie-backed,
		// so the default is omitted from chip links to keep URLs clean.
		density := s.feedDensity(w, r)
		if density == defaultFeedDensity {
			density = ""
		}
		hrefFn := func(role, state string) string {
			return buildAgentsHref(s.basePath, projectFilter, role, state, density)
		}
		roleChips, stateChips := buildSecondaryChips(active.Beads, projectFilter, roleFilter, stateFilter, hrefFn)
		data["RoleChips"] = roleChips
		data["StateChips"] = stateChips
	}

	// Recent (terminated) — flat list, optional project filter. The role
	// filter narrows recent rows the same way it narrows active rows; the
	// state filter only applies to live agents (a closed bead has no
	// alive/stuck/idle reading), so we skip it here.
	recentRows := make([]agentRowView, 0, len(recent.Beads))
	for _, b := range recent.Beads {
		if b == nil {
			continue
		}
		project := agentProjectName(b)
		if projectFilter != "" && project != projectFilter {
			continue
		}
		if roleFilter != "" {
			role := strings.TrimSpace(b.Fields["role"])
			if role == "" {
				role = "untyped"
			}
			if role != roleFilter {
				continue
			}
		}
		recentRows = append(recentRows, s.agentRowFromBeadCtx(r.Context(), b, project, now))
	}
	data["Recent"] = recentRows
	data["RecentCount"] = len(recentRows)

	// Dormant (parked persistent identities) — flat list, optional project +
	// role filter, mirroring the Recent partition. No state filter: a dormant
	// bead is closed and carries only the engine-set dormant state, so the
	// alive/stuck/idle state filter does not apply. Rows come from the dedicated
	// dormant query, so a long-parked identity stays listed regardless of the 6h
	// Recent / 24h history windows.
	dormantRows := make([]agentRowView, 0, len(dormant.Beads))
	for _, b := range dormant.Beads {
		if b == nil {
			continue
		}
		project := agentProjectName(b)
		if projectFilter != "" && project != projectFilter {
			continue
		}
		if roleFilter != "" {
			role := strings.TrimSpace(b.Fields["role"])
			if role == "" {
				role = "untyped"
			}
			if role != roleFilter {
				continue
			}
		}
		dormantRows = append(dormantRows, s.agentRowFromBeadCtx(r.Context(), b, project, now))
	}
	data["Dormant"] = dormantRows
	data["DormantCount"] = len(dormantRows)

	// Overall summary header (unfiltered view only — per-project Fleet tile
	// already covers the filtered case). Built from the active beads already
	// in memory + the filtered recent count.
	if projectFilter == "" {
		summary := buildOverallSummary(active.Beads, recent.Beads, dormant.Beads, history, len(recentRows), shortDuration(agentsRecentWindow), now)
		if summary != nil {
			s.decorateRecentRunsWithMRState(r.Context(), summary.RecentRuns)
		}
		data["Summary"] = summary
	}

	// Prewarm agent link lookups for any assignees/owners surfaced on this
	// page. The agents page renders agent beads themselves, but row links
	// for assignee/owner still go through agentLink in shared partials.
	combined := make([]*beadsapi.BeadDetail, 0, len(active.Beads)+len(recent.Beads)+len(dormant.Beads))
	combined = append(combined, active.Beads...)
	combined = append(combined, recent.Beads...)
	combined = append(combined, dormant.Beads...)
	r = s.prewarmAgentLinksFromBeads(r, combined)
	s.render(w, r, "agents.html", data)
}

// agentProjectName returns the bead's project (from project:<name> label or
// the project field), falling back to "untagged" so the chip strip and
// section headers always have a label.
func agentProjectName(b *beadsapi.BeadDetail) string {
	if p := projectFromLabels(b.Labels); p != "" {
		return p
	}
	if p := b.Fields["project"]; p != "" {
		return p
	}
	return "untagged"
}

// coopmuxTerminalURL returns a public-facing coopmux dashboard link
// (`<COOPMUX_PUBLIC_URL>#<pod_name>`) for the agent bead's pod, or empty
// string when either the public URL is unconfigured or the bead has no
// pod_name yet. Display-only — does not touch the internal coop_url note,
// which stays an in-cluster pod IP for controller→coop API calls.
func (s *Server) coopmuxTerminalURL(bead *beadsapi.BeadDetail) string {
	if bead == nil || s.coopmuxPublicURL == "" {
		return ""
	}
	podName := strings.TrimSpace(beadsapi.ParseNotes(bead.Notes)["pod_name"])
	if podName == "" {
		return ""
	}
	return s.coopmuxPublicURL + "#" + podName
}

// slackThreadURL returns a deep-link to the Slack thread that spawned this
// agent, in the canonical `https://<workspace>.slack.com/archives/<channel>/p<ts>?thread_ts=<ts>`
// form. Returns empty when slackWorkspaceDomain is unset or the bead has no
// slack_thread_channel/slack_thread_ts fields (non-Slack-spawned agents).
func (s *Server) slackThreadURL(bead *beadsapi.BeadDetail) string {
	if bead == nil || s.slackWorkspaceDomain == "" {
		return ""
	}
	channel := strings.TrimSpace(bead.Fields["slack_thread_channel"])
	ts := strings.TrimSpace(bead.Fields["slack_thread_ts"])
	if channel == "" || ts == "" {
		return ""
	}
	// Slack permalink format: p<TS_with_dot_stripped>. Preserve `ts` verbatim
	// in the thread_ts query so Slack opens the thread side panel.
	tsCompact := strings.ReplaceAll(ts, ".", "")
	return "https://" + s.slackWorkspaceDomain + ".slack.com/archives/" + channel + "/p" + tsCompact + "?thread_ts=" + ts + "&cid=" + channel
}

// terminalAvailable reports whether the agent-detail page should render the
// live PTY pane scaffolding. The terminal is only useful when coopmux is
// configured AND the bead names a coopmux session (MuxSessionID) to attach to. Otherwise
// the template renders a one-line "agent terminal unavailable in this
// deployment" banner — no broken WS attempts, no empty <pre> staring back.
func (s *Server) terminalAvailable(bead *beadsapi.BeadDetail) bool {
	if bead == nil {
		return false
	}
	if bead.MuxSessionID() == "" {
		return false
	}
	return s.coopmux != nil && !s.coopmux.Disabled()
}

// coopmuxTerminalURLFor is coopmuxTerminalURL for a request: empty for a scoped
// principal or a verifier. The coopmux dashboard lists every pod, so its link
// stays admin-only, like /agents/{id}/screen.
func (s *Server) coopmuxTerminalURLFor(ctx context.Context, bead *beadsapi.BeadDetail) string {
	if isScopedCtx(ctx) || isVerifier(ctx) {
		return ""
	}
	return s.coopmuxTerminalURL(bead)
}

// getScreenSnapshot returns the cached PTY screen text for the agent bead's
// `MuxSessionID` (the pod name, else last_session_id), or empty string when there is no session, no client, or
// the session was not found. Other coopmux errors are logged at debug level
// and returned as an empty string — a missing first-paint snapshot is a
// downgrade, not a page failure.
func (s *Server) getScreenSnapshot(ctx context.Context, bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	sessionID := bead.MuxSessionID()
	if sessionID == "" {
		return ""
	}
	if s.coopmux == nil || s.coopmux.Disabled() {
		return ""
	}
	screen, err := s.coopmux.GetScreen(ctx, sessionID)
	if err != nil {
		switch {
		case errors.Is(err, coopmuxapi.ErrSessionNotFound), errors.Is(err, coopmuxapi.ErrDisabled):
			// Benign: session has rotated away or coopmux is not deployed.
		default:
			s.logger.Debug("coopmux GetScreen failed", "session", sessionID, "error", err)
		}
		return ""
	}
	return screen.Text()
}

// handleAgentScreen serves the htmx fragment that re-renders the cached PTY
// screen snapshot for an agent. Used by agent_terminal.js when the WebSocket
// drops, so the page can fall back to a fresh server-side render rather than
// staying with whatever the disconnected xterm last had on screen.
func (s *Server) handleAgentScreen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Agent not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting agent bead for screen fragment", "id", id, "error", err)
		http.Error(w, "Failed to load agent", http.StatusInternalServerError)
		return
	}
	if bead.Type != "agent" {
		http.Error(w, "Not an agent bead", http.StatusBadRequest)
		return
	}
	s.renderPartial(w, r, "templates/partials/agent_screen.html", "agent_screen", map[string]any{
		"Bead":           bead,
		"ScreenAnsiText": s.getScreenSnapshot(r.Context(), bead),
	})
}

// buildAgentsHref renders /agents?... query string with the supplied filter
// dimensions, dropping empty values so we never emit `?role=&state=` noise.
// Order: project, role, state, density — stable for snapshot diffs and easy
// for templates to read.
func buildAgentsHref(basePath, project, role, state, density string) string {
	out := basePath + "/agents"
	parts := []string{}
	if project != "" {
		parts = append(parts, "project="+project)
	}
	if role != "" {
		parts = append(parts, "role="+role)
	}
	if state != "" {
		parts = append(parts, "state="+state)
	}
	if density == "compact" {
		parts = append(parts, "density=compact")
	}
	if len(parts) > 0 {
		out += "?" + strings.Join(parts, "&")
	}
	return out
}

// shortDuration renders a whole-hour duration as "Nh" (or "Nm" when sub-hour).
// Used for the "last 6h" label on the Recent section so the displayed window
// tracks agentsRecentWindow if it's ever retuned.
func shortDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return strings.TrimSuffix(d.String(), "0m0s")
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return strings.TrimSuffix(d.String(), "0s")
	}
	return d.String()
}

// agentRowFromBeadCtx builds the row view-model from an agent bead. Method on
// *Server so it can resolve deep-link URLs (coopmux terminal, Slack thread)
// using the server's configuration, threading the request context into the
// MR-bead lookup.
func (s *Server) agentRowFromBeadCtx(ctx context.Context, b *beadsapi.BeadDetail, project string, now time.Time) agentRowView {
	name := b.Fields["agent"]
	if name == "" {
		name = b.Title
	}
	// CreatedAt is the agent bead's birth — a usable proxy for "pod up since"
	// because the controller creates the bead when it spawns the pod.
	uptime := fmtTimeAgo(b.CreatedAt, now)
	longRunning := false
	if !b.CreatedAt.IsZero() && now.Sub(b.CreatedAt) >= agentUptimeWarnThreshold {
		longRunning = true
	}
	row := agentRowView{
		ID:                b.ID,
		Name:              name,
		Role:              b.Fields["role"],
		Harness:           agentHarness(b.Fields),
		Project:           project,
		AgentState:        b.Fields["agent_state"],
		PodPhase:          b.Fields["pod_phase"],
		Status:            b.Status,
		TaskID:            b.Fields["task_id"],
		TaskTitle:         b.Fields["task_title"],
		SpawnedBy:         b.Fields["spawned_by"],
		ScheduleID:        b.Fields["schedule_id"],
		OperatedByDisplay: b.OperatedByDisplay,
		OperatedBySlug:    b.OperatedBySlug,
		Desc:              snippet(b.Description, 160),
		Ago:               fmtTimeAgo(b.UpdatedAt, now),
		UpdatedAtRFC:      rfcTime(b.UpdatedAt),
		Uptime:            uptime,
		CreatedAtRFC:      rfcTime(b.CreatedAt),
		LastProgressAt:    parseRFC3339OrZero(strings.TrimSpace(b.Fields["last_progress_at"])),
		LongRunning:       longRunning,
		Stale:             isAgentStale(b, now),
		TerminalURL:       s.coopmuxTerminalURLFor(ctx, b),
		SlackURL:          s.slackThreadURL(b),
		// Gardener-grouping + lineage keys (task ka-0000006067). Resolved with no
		// extra fetch — DeriveGardenerID + the lineage back-pointers are pure
		// projections over the bead's fields.
		GardenerID:    beadsapi.DeriveGardenerID(b.Fields),
		LineageRootID: agentLineageRootID(b),
		PriorAgentID:  lineagePriorID(b.Fields),
		CreatedAt:     b.CreatedAt,
	}
	// Context size is only meaningful for a still-running agent; cost folds in
	// the live estimate only while live (closed sessions are reconciled into
	// total_cost_usd). Treat any non-closed agent in an alive state as live.
	rowLive := b.Status != "closed" && isAgentStateIn(b.Fields["agent_state"], agentStateAlive)
	row.Usage = buildAgentUsageView(b.Fields, rowLive)
	if b.Status == "closed" {
		wrapup := b.Fields["wrapup"]
		row.WrapUpSummary = parseWrapUpSummary(wrapup)
		row.WrapUpMRs = ResolveWrapupMRs(ctx, s.daemon, wrapup)
		row.WrapUpBeadsClosed = wrapUpBeadsClosedCount(wrapup)
		row.RunOutcome = agentRunOutcome(wrapup)
		// Backstop: when the run left no prose summary, derive an outcome line
		// from its structured signals so the card never renders a bare title
		// (ka-0000001187).
		if row.WrapUpSummary == "" {
			row.DerivedOutcome = derivedRunOutcome(len(row.WrapUpMRs), row.WrapUpBeadsClosed, row.RunOutcome)
		}
	}
	return row
}

// agentHarness returns the agent CLI an agent bead runs: the controller's
// resolved_harness stamp (ADR 0004 section 4, written once at spawn so no
// consumer re-resolves), else the per-spawn harness override. Empty when
// neither is set (a bead that predates the field, i.e. claude).
func agentHarness(fields map[string]string) string {
	if h := strings.TrimSpace(fields["resolved_harness"]); h != "" {
		return h
	}
	return strings.TrimSpace(fields["harness"])
}
