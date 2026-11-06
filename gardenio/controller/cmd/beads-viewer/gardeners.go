package main

// Gardeners index page (epic ka-0000004574, task ka-0000004521).
//
// GET /gardeners — a list of every gardener (a `type=trigger` bead carrying
// `role:gardener`, plus legacy `type=schedule` + `role:gardener`) with at-a-
// glance columns and a link to each gardener's `/gardener/{id}` life-thread page.
//
// The per-gardener page `/gardener/{id}` shipped in 2026.152.5 (epic
// ka-0000001579) but was undiscoverable: no index, no nav entry. This page +
// the "Gardeners" nav link in layout.html close that gap. It reuses the existing
// gardenerView header view-model (gardener.go) — the same decode path the detail
// page and the engine use — so the index never drifts from the spec.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
)

// gardenersListLimit caps the daemon list call. Gardeners are operator-authored
// and bounded by hand, so a single fetch covers any realistic deployment (same
// reasoning as triggersListLimit).
const gardenersListLimit = 500

// gardenersFilter is the ?filter= query value.
type gardenersFilter int

const (
	// gardenersFilterLive is the DEFAULT view (ka-0000004655): the operational
	// fleet — every gardener that has NOT been formally retired (status != closed),
	// i.e. active + disabled. A retired (closed) gardener is hidden here so the
	// page is not cluttered by the historical tail; it surfaces only under the
	// Archived view. This makes the empty `?filter=` land on the live fleet
	// instead of All.
	gardenersFilterLive     gardenersFilter = iota
	gardenersFilterAll                      // every gardener regardless of lifecycle
	gardenersFilterActive                   // open/in_progress AND enabled
	gardenersFilterDisabled                 // open but disabled
	// gardenersFilterArchived is the "click Archived to see retired gardeners"
	// view (ka-0000004655): closed/terminated gardeners, including those formally
	// retired by an operator (gardener_terminated_kind=retired). Semantically the
	// same row set as the legacy `closed` filter, which remains a reachable alias.
	gardenersFilterArchived
	gardenersFilterNeedsAttention // verdict stalled or stuck
	gardenersFilterGoalAchieved   // closed having reached its terminal goal
	gardenersFilterUnachieved     // has NOT (yet) reached its terminal goal
)

// gardenerVerdictNeedsAttention reports whether a verdict kind is one an operator
// should look at now (the gardener is spinning or failing). Shared by the row
// matcher and the chip count so the "needs attention" filter and badge agree.
func gardenerVerdictNeedsAttention(verdictKind string) bool {
	return verdictKind == "stalled" || verdictKind == "stuck"
}

// gardenerRowNeedsAttention is the single "does this row warrant operator eyes
// now" predicate shared by the needs-attention chip COUNT and the
// ?filter=needs-attention row matcher, so the badge and the filtered view can
// never disagree. It unions the three attention signals: a stalled/stuck health
// verdict, the pouring-but-never-worked stall, and a runaway DOLLAR spend
// ("over" bucket) — the C1 addition (bug ka-0000001009). The spend signal is
// scoped to OPEN gardeners (a closed gardener cannot be operated, so its spend is
// history, not a call to action), matching the row-level NeedsAttention bump in
// applyGardenerSpend.
func gardenerRowNeedsAttention(row gardenerRowView) bool {
	if gardenerVerdictNeedsAttention(row.VerdictKind) || row.PourStalled {
		return true
	}
	return row.SpendLevel == "over" && !row.IsClosed
}

// gardenerRowView is one row on /gardeners. It is a thin projection of the shared
// gardenerView header (gardener.go) plus a couple of list-only derived fields, so
// the index and the detail page agree on identity, goal, counters, and owner.
type gardenerRowView struct {
	ID   string
	Name string // GardenerSpec.Name, falling back to the bead title
	// Title is the raw gardener BEAD title. It is carried alongside Name so the
	// index can reconcile a displayed-name vs bead-title divergence (bug
	// ka-0000008054): when a gardener's GardenerSpec.Name differs from its bead
	// title, the row surfaces the bead title as a subtitle instead of silently
	// showing only the spec name. Empty when it equals Name (no divergence to
	// show); see NameDivergesFromTitle.
	Title        string
	TerminalGoal string
	// Prompt is the gardener's per-run `prompt` field — the operative instruction
	// (bug ka-0000003596). Carried on the row so a prompt-only gardener (no
	// terminal goal) still shows a clamped one-liner of what it does + a full
	// title tooltip, and so the row's ChasingSummary can fall back to it rather
	// than reading "idle" with no hint. Full text; the template clamps display.
	Prompt       string
	OwnerDisplay string
	OwnerSlug    string
	// OwnerEmail is the gardener bead's resolved operator email (b.OperatedByEmail
	// after populateOperatedBySlug). Carried so the /maps picker can default to
	// "the viewer's own gardeners" by matching it against the request's auth email
	// (ka-0000003229). Empty when the gardener has no resolvable operator email.
	OwnerEmail string

	RunsSpawned   int
	RunsCompleted int

	// RunsDerived is true when RunsSpawned/RunsCompleted were DERIVED from the
	// gardener's iteration beads (a bounded CountOnly enrichment pass) rather than
	// read from a GardenerSpec engine snapshot. Legacy spawn_agent / bare-trigger
	// gardeners carry no `gardener` blob, so their spec tally is null/zero even
	// though their life-thread has dozens-to-hundreds of real runs — they showed
	// "0/0" in the Runs column (bug ka-0000005661). enrichGardenerRunCounts fills
	// the count from the iteration beads for that subset and flags it here so the
	// template can mark the figure "~" (a findable-run count, not the engine's
	// monotonic lifetime tally).
	RunsDerived bool

	// --- Per-gardener spend roll-up (ka-0000003220). SpendUSD is the gardener's
	//     maintained gardener_spend_usd field: the summed authoritative
	//     total_cost_usd over its iteration runs PLUS the crew they transitively
	//     spawned, recomputed on a slow cadence by the controller's
	//     gardenerSpendSync loop. HasSpend gates rendering (a never-synced gardener
	//     has no field yet); SpendLabel is the pre-formatted "$12.34"; spendUSD
	//     is the raw float retained as the sort key so the index can float
	//     wasteful gardeners to the top. SpendStale flags a roll-up older than
	//     gardenerSpendStaleAfter so the chip can be marked approximate. ---
	HasSpend   bool
	SpendLabel string
	SpendStale bool
	spendUSD   float64

	// SpendLevel buckets the dollar roll-up for runaway-cost styling + triage
	// (bug ka-0000001009 / C1): "" (below the warn threshold — no emphasis),
	// "warn" (expensive), or "over" (runaway). Mirrors gardenerBudgetBurn.Level
	// for the run-COUNT budget, but keyed on DOLLARS. An "over" level on an OPEN
	// gardener bumps NeedsAttention so a quietly-burning loop bubbles to the top
	// of the fleet without the operator having to Spend-sort and eyeball it.
	// Empty on an unsynced (HasSpend=false) row. Set by applyGardenerSpend.
	SpendLevel string

	// SpendExcludesLiveRun flags that the dollar roll-up UNDER-reports because a
	// run is in flight (bug ka-0000001009 / C3). total_cost_usd is written only
	// at session close, so a running gardener's live-run cost is not yet in the
	// summed figure; the chip marks itself approximate ("~") with an "excludes
	// the in-flight run" hint, exactly during the window an operator watches a
	// possibly-runaway gardener. Distinct from SpendStale (the roll-up loop being
	// behind) — both can be true and each carries its own tooltip. Set in
	// applyLastRun once IsRunning is known, gated on HasSpend.
	SpendExcludesLiveRun bool

	// LastRunOutcome summarizes the most recent completed run as "ok" | "failed"
	// | "" (none yet). It is populated from the ACTUAL last-run wrap-up
	// (agentRunOutcome over GardenerSpec.LastAgentID's bead) when that bead is
	// fetchable, falling back to the engine-counter derivation
	// (gardenerLastRunOutcome over ConsecutiveFailures + RunsCompleted) when
	// LastAgentID is empty or the bead can't be fetched.
	LastRunOutcome      string
	ConsecutiveFailures int

	// --- Last-run inline result + run-centric sort keys (epic ka-0000004446).
	//     Populated from the gardener's last-agent bead (GardenerSpec.LastAgentID),
	//     batch-fetched once in handleGardeners. All zero-valued when LastAgentID
	//     is empty or the bead is unfetchable — the row then falls back to the
	//     engine-counter outcome chip and sorts as a no-completed-run row. ---

	// IsRunning is true when the gardener's last-agent bead is not closed — a live
	// in-flight run. Drives the running-first sort and the animated badge.
	IsRunning bool

	// LastRunTLDR is the one-line headline of the last run, shown inline on the
	// row WITHOUT expanding the drawer. Parsed from the last-agent bead's wrap-up
	// `tldr` (parseWrapUpTLDRAndAccomplishments), backstopped with the
	// accomplishments line / a derived outcome / the task title so it is never
	// blank, then clamped to a single line. Empty only when there is no last-agent
	// bead at all.
	LastRunTLDR string

	// LastCompletedAt is when the last run COMPLETED (last-agent UpdatedAt, closed
	// only) — the secondary sort key for non-running rows (most-recently-completed
	// first). Zero for a running gardener or one with no fetchable completed run.
	// LastCompletedAgo / LastCompletedAtRFC are its rendered forms.
	LastCompletedAt    time.Time
	LastCompletedAgo   string
	LastCompletedAtRFC string

	// lastAgentID is GardenerSpec.LastAgentID, carried so handleGardeners can batch
	// the last-run fetch across all rows. lastActivityAt is the last-agent bead's
	// UpdatedAt regardless of status — the tie-break among running rows
	// (most-recently-active first). Neither is rendered directly.
	lastAgentID    string
	lastActivityAt time.Time

	// --- Live-agent overlay (maps2 v2, ka-0000000595). The gardener's last-agent
	//     bead (GardenerSpec.LastAgentID), already batch-fetched for the last-run
	//     inline peek, IS the currently-live agent when the gardener is running.
	//     applyLastRun projects its identity here off that same bead — no extra
	//     fan-out. LiveAgentName is the agent's display name (Fields["agent"],
	//     falling back to the bead title); LiveAgentHref links to the agent bead;
	//     LiveAgentAlive mirrors the /agents liveness predicate (agent_state ∈
	//     {working,spawning,yielding} AND not stale) so the /maps card can show a
	//     pulsing live dot only when the agent is genuinely alive, not merely
	//     "last run not yet closed". All empty/false when the gardener has no
	//     fetchable last-agent bead. ---
	LiveAgentName  string
	LiveAgentHref  string
	LiveAgentAlive bool

	// --- Live-context occupancy (maps2 v2 cost/context chips, ka-0000001668).
	//     Projected from the SAME last-agent bead the live-agent overlay already
	//     fetched (no extra fan-out): the working agent's reported
	//     `context_tokens` as a percentage of the model's auto-compaction
	//     threshold (internal/contextwindow), mirroring /agents'
	//     buildAgentUsageView. Context is only meaningful for a
	//     genuinely-live agent (a closed shell has no "current context"), so the
	//     fields are gated on LiveAgentAlive in applyLastRun. HasContext gates the
	//     chip; ContextLabel is the humanized token count ("128k"); ContextPct is
	//     the whole-number occupancy; ContextTitle is the hover detail. All
	//     empty/false when the gardener has no live agent or it reported no
	//     context size yet. ---
	HasContext   bool
	ContextLabel string
	ContextPct   int
	ContextTitle string

	// --- Turns (🔁 server round-trips) — the third metric in the triad (epic
	//     ka-0000001199). Projected from the SAME last-agent bead as context,
	//     via buildAgentUsageView's live→total fallback: while the run is live it
	//     reads the advancing live_turns, and once the run closes it reads the
	//     authoritative total_request_count. Unlike context (which is only
	//     meaningful for a live agent, so its fields are gated on LiveAgentAlive),
	//     turns stay meaningful after close — a gardener's last completed run keeps
	//     its round-trip count — so HasTurns is NOT gated on liveness. HasTurns
	//     gates the chip; TurnsLabel is the humanized count ("47", "1.2k"). ---
	HasTurns   bool
	TurnsLabel string

	// Health verdict, projected from the gardener's trajectory (the same
	// Verdict/VerdictKind buildGardenerTrajectory computes for the detail-page
	// header — no extra fetch, the row decode already has the spec). Verdict is
	// the one-line call ("making progress" | "stalled" | "stuck" | "starting
	// up"); VerdictKind is the CSS/filter hook ("progress" | "stalled" |
	// "stuck" | "neutral"); VerdictDetail is the parenthetical evidence for the
	// tooltip. All empty when the gardener has no trajectory data yet.
	Verdict       string
	VerdictKind   string
	VerdictDetail string

	// Legible is the decoded, human-readable firing + stop config (task
	// ka-0000005782 / I2.4): wakes-when (cron/gate/event/decision) and stops-when
	// (loop_end/run_end/hard_stop conditions) as prose, replacing the raw JSON
	// dumps. Copied verbatim from the shared gardenerView header (no extra fetch —
	// gardenerRowFromBead already decodes it), so the index row's wakes/stops
	// summaries match the detail page exactly. The list row reads
	// .Legible.WakesSummary / .Legible.StopsSummary (the compact one-liners).
	Legible gardenerLegible

	// Runs is the gardener's recent-run glyph strip (oldest→newest), copied
	// verbatim from the trajectory the row decode already built — NO extra fetch
	// and NO N+1 (the glyphs come from the gardener bead's embedded ProgressState
	// during gardenerViewFromBead). It backs the /maps board's per-gardener
	// recent-run strip (epic ka-0000001644). The board takes its own tail-N; this
	// carries the full bounded sparkline (gardenerTrajectorySparkRuns). Empty for a
	// legacy gardener with no ProgressState — the board then renders counters +
	// last-run chip only.
	Runs []gardenerRunGlyph

	// NeedsAttention flags a row an on-call operator should act on NOW: an
	// OPEN gardener whose health verdict is stalled or stuck (a runaway/wedged
	// loop). It is the top sort key (these rows pin to the top of every view)
	// and drives the row tint + badge so triage does not require switching to
	// ?filter=needs-attention. Scoped to non-closed gardeners: a closed gardener
	// cannot be operated, so it never gets the attention bump even if its last
	// trajectory verdict was stalled/stuck. (The needs-attention CHIP keeps its
	// own lifecycle-agnostic count — see gardenerVerdictNeedsAttention.)
	NeedsAttention bool

	// PourStalled surfaces the engine's pouring-but-never-worked verdict
	// (ka-0000000957), read directly off the trigger bead's stamped `pour_stall`
	// field — no graph walk in the row builder. An apply_formula gardener in this
	// state looks alive by every other signal (fresh last_run, non-zero runs)
	// yet is pouring molecules nobody works, so it also bumps NeedsAttention.
	PourStalled     bool
	PourStallDetail string

	// Liveness is the overdue / silent-gardener verdict (bug ka-0000006095 / A1),
	// derived live from the gardener's fire / budget / progress signals. An
	// overdue or silent gardener bumps NeedsAttention so a silently-dead loop
	// surfaces at the top of the list instead of behind a healthy-looking
	// next-fire. Seeded from the bead-surface signals in gardenerRowFromBead
	// (budget + cron lateness) and refined in applyLastRun once the last-agent
	// bead's last_progress_at is available (the silent-progress signal).
	Liveness gardenerLiveness
	// livenessInput carries the fetch-free liveness inputs captured at row build
	// so applyLastRun can re-derive Liveness with the last-agent bead's
	// last_progress_at without re-decoding the gardener bead. Unexported (not
	// template-facing).
	livenessInput gardenerLivenessInput

	// --- Inline attention signal (ka-0000003950). Populated ONLY for
	//     NeedsAttention rows so the dormant tail stays uncluttered. They answer
	//     "WHY does this gardener need attention" inline on the index row, so an
	//     on-call operator can triage the whole fleet without drilling into each
	//     /gardener/{id}. Sourced from the gardener's REAL open decision (the
	//     last-run agent's decision_bead_id, falling back to the daemon
	//     BlockedOnDecision predicate) or, failing a decision, the last run's
	//     wrap-up blocker. All empty on non-attention rows. ---
	//
	// AttentionKind is the chip class/label hook: "decision" (the gardener is
	// blocked on an open operator decision) | "blocker" (its last run wrapped up
	// with a blocker) | "" (no inline signal — the verdict alone explains it).
	AttentionKind string
	// AttentionSignal is the compact, truthful one-liner shown on the row: the
	// open decision's prompt, or the last run's top blocker. Clamped to a single
	// line (gardenerAttentionSignalLimit).
	AttentionSignal string
	// AttentionDecisionID is the open decision bead the row links to when the
	// signal is decision-sourced; "" for a blocker-sourced signal.
	AttentionDecisionID string

	// attentionDecisionID is the candidate open-decision id discovered during
	// enrichment — seeded from the daemon BlockedOnDecision predicate
	// (gardenerRowFromBead) and overridden by the last-run agent's
	// decision_bead_id when present (applyLastRun, the live run's own blocker).
	// lastRunBlocker is the last run's wrap-up `blockers` text. Neither is
	// rendered directly; deriveAttentionSignal projects them into the exported
	// Attention* fields, gated to NeedsAttention rows.
	attentionDecisionID string
	lastRunBlocker      string

	Enabled  bool
	IsClosed bool

	// Status drives the row's status chip: "active" | "disabled" | "closed".
	Status string

	// GoalState is the goal disposition for a CLOSED gardener: "achieved" (it
	// reached its terminal goal — loud badge), "unachieved" (it closed without
	// reaching the goal — budget/time/retired), or "" (still open). Derived from
	// the gardener's termination provenance via gardenerView.GardenerGoalState.
	GoalState string

	// TerminationReason is a short human label for WHY a closed gardener stopped
	// ("exit condition met", "run budget exhausted", "retired by operator", ...),
	// surfaced in the goal-badge tooltip. Empty for open gardeners.
	TerminationReason string

	// targetEpicID is the gardener's authoritative goal-target bead (the root
	// epic of a subtree-drain bead_query, or a specific_bead_closed target),
	// resolved from the trigger bead with NO fetch in gardenerRowFromBead. It is
	// not rendered; enrichGardenerGoalStates batch-fetches each target's CURRENT
	// status and recomputes GoalState LIVE from it (bug ka-0000007772), so a
	// closed-epic / open-epic flip after the gardener's close is reflected on the
	// index instead of the stale close-time snapshot. Empty when the gardener has
	// no single resolvable target (then GoalState keeps the snapshot value).
	targetEpicID string

	// --- Owned-epic rollup (task ka-0000001266, maps2 v2). The epic this
	//     gardener drives (gardenerOwnedEpicID: the gardener-epic:<id> label, or the
	//     legacy assign_bead field) and how far its issue-kind subtree has
	//     drained. ownedEpicID is resolved with NO fetch in gardenerRowFromBead;
	//     enrichGardenerOwnedEpics batch-counts each distinct epic's subtree and
	//     fills the exported fields. HasOwnedEpic gates rendering — false when the
	//     gardener has no owned epic, the count query failed, or the epic has no
	//     issue descendants, so a card never shows a misleading "0/0 · 0%". ---
	ownedEpicID     string
	OwnedEpicID     string
	OwnedEpicTotal  int
	OwnedEpicClosed int
	OwnedEpicOpen   int
	OwnedEpicPct    int
	HasOwnedEpic    bool

	// Kind distinguishes the trigger primitive from the legacy schedule one,
	// rendered as a small chip (mirrors triggerRowView.Kind).
	Kind string

	// TriggerSubKind is the gardener's firing SUB-kind, projected from the
	// trigger def (bridge.ParseTriggerDef, no extra fetch — the gardener bead
	// already carries its trigger fields). It refines the 2-way Cron-vs-not
	// split that .Kind + NextFire alone can express into the three primitives
	// the /maps trigger node colors by (design.md §6 card-anatomy):
	//
	//   - "cron"       — a clock fires this gardener (a parseable `cron`).
	//   - "bead_query" — a world-state predicate fires it (a `conditions` gate
	//     or a bead_event/decision_resolved entry in the `triggers` array).
	//   - "event"      — fired by some other event source with no cron/predicate.
	//
	// Empty for a legacy type=schedule gardener (the row keeps .Kind=="schedule").
	// Seeded in gardenerRowFromBead via gardenerTriggerSubKind; the /maps card
	// reads it to color the trigger node blue/red/purple.
	TriggerSubKind string

	Ago          string
	UpdatedAtRFC string

	// NextFire / NextFireRelative / NextFireError populate for enabled gardeners
	// carrying a parseable `cron` field (the default sort key). NextFire is the
	// absolute next firing ("2026-06-02 15:04 MST"); NextFireRelative is the
	// human "in 12m"; NextFireError carries a cron parse error verbatim. All
	// empty for closed/disabled gardeners or those with no cron (e.g. legacy
	// bead-trigger gardeners) — the template suppresses the slot.
	NextFire         string
	NextFireRelative string
	NextFireError    string

	// CreatedAtRFC / CreatedAgo describe when the gardener bead was created. The
	// default sort breaks next-fire ties by newest-created-first, so operators
	// see freshly-minted gardeners near the top (thread request 2026-06-02).
	CreatedAtRFC string
	CreatedAgo   string

	// nextFireAt is the parsed next-fire time used for sorting; zero when the
	// gardener has no cron or an unparseable one. createdAt is the bead creation
	// time, the secondary sort key. Neither is rendered directly.
	nextFireAt time.Time
	createdAt  time.Time

	// Href is the gardener's detail page.
	Href string

	// ExpandURL lazy-loads this gardener's invocations (the expand button target,
	// thread request 2026-06-02). HTMX swaps the response into the row's detail
	// drawer on first click.
	ExpandURL string

	// PlannedHeirs are the successors this gardener DECLARES via its `chain.to`
	// but that have not yet materialized as real gardener beads (task
	// ka-0000005273 / "C3"). Each is a planned edge the /maps card-graph renders
	// as a PENDING heir node so a drawn-but-not-yet-spawned successor is visible
	// the moment it is authored — before the C4 engine materializes it at the
	// predecessor's goal handoff. Derived with no extra fetch from the parsed
	// trigger def (bridge.ParseTriggerDef → def.Chain.Heirs). Empty for a
	// gardener with no chain.
	PlannedHeirs []gardenerPlannedHeir
}

// gardenerPlannedHeir is one declared-but-unmaterialized successor of a gardener,
// projected from a bridge.ChainTo. The synthetic ID anchors the planned edge on
// the card-graph; Label is the human phase/goal summary the pending node shows.
type gardenerPlannedHeir struct {
	// ID is a synthetic, stable per-predecessor identifier for the planned heir
	// (the real heir bead does not exist yet). It is deterministic in the
	// predecessor id + heir index so the rail render is stable across reloads.
	ID string
	// Label is the pending node's caption (phase, else a truncated terminal goal,
	// else "successor").
	Label string
	// FanOut is true when the predecessor declares more than one heir (so the
	// pending node can read "fan-out").
	FanOut bool
}

// NameDivergesFromTitle reports whether the gardener's displayed Name (its
// GardenerSpec.Name) differs from its bead Title, so the index can surface the
// underlying bead title as a subtitle rather than silently rendering only the
// spec name (bug ka-0000008054). Whitespace-insensitive; false when either is
// empty (Name already falls back to Title, so there is nothing to reconcile).
func (r gardenerRowView) NameDivergesFromTitle() bool {
	n := strings.TrimSpace(r.Name)
	t := strings.TrimSpace(r.Title)
	if n == "" || t == "" {
		return false
	}
	return n != t
}

// gardenersFilterChip is one filter chip in the strip.
type gardenersFilterChip struct {
	Label  string
	Value  string
	Count  int
	Active bool
	Href   string
}

// gardenersSummary is the compact fleet-health strip that leads the /gardeners
// page (ka-0000007180). It hoists the most operationally-relevant counts —
// how many gardeners are firing right now, how many need attention, the
// lifecycle split — to the very top so an operator reads fleet state in one
// glance before scanning the 100+ row table. Every count links back to the
// matching filter chip so the strip is also navigation. Counts are computed
// over the unfiltered roster (same source as the chip badges) so the strip and
// the chips never disagree.
type gardenersSummary struct {
	Total          int
	Running        int // a run in flight now (row.IsRunning)
	Live           int // not retired (Total − Closed) — the default-view count
	Active         int // open + enabled
	NeedsAttention int // verdict stalled or stuck
	Disabled       int
	Closed         int // retired/archived (ka-0000004655)
	GoalAchieved   int
	BasePath       string
}

// gardenerExcludedReason classifies WHY a gardener bead is deliberately kept off
// the live /gardeners roster. These are the two intentional exclusions
// loadGardenerRows applies (NOT coverage gaps): E2E/test fixtures
// (isFixtureGardenerBead, bug ka-0000008054) and foreign-namespace
// schedule-mirror artifacts (isScheduleMirrorGardenerBead, bug ka-0000002811).
type gardenerExcludedReason string

const (
	gardenerExcludedFixture        gardenerExcludedReason = "fixture"
	gardenerExcludedScheduleMirror gardenerExcludedReason = "schedule-mirror"
)

// gardenerExcluded is one gardener bead loadGardenerRows deliberately dropped from
// the live roster, retained so /gardeners can surface a "N hidden — why"
// affordance (bug ka-0000007605) instead of letting the All-chip-vs-true-
// population delta read as an unexplained coverage hole. Href points at the
// plain bead page (/{id}) — NOT /gardener/{id}, which 404s for both excluded
// classes by design.
type gardenerExcluded struct {
	ID     string
	Title  string
	Reason gardenerExcludedReason
	Href   string
}

// gardenersHidden is the "N hidden — why" affordance shown beside the chip strip
// (bug ka-0000007605). The /gardeners roster deliberately excludes two classes
// of role:gardener bead — E2E fixtures and foreign-namespace schedule-mirror
// artifacts — so the All chip (the live operational fleet) is intentionally
// SMALLER than the raw role:gardener population in beads. That delta used to be
// silent, so a CAMP audit read it as 9 unreachable gardeners and capped Coverage
// at 4/10. This view-model accounts for every hidden bead with a per-class
// breakdown and a disclosure listing each one, so an operator can see the whole
// fleet (rendered + hidden) and trust that nothing is silently dropped.
type gardenersHidden struct {
	Total           int                // FixtureCount + MirrorCount
	FixtureCount    int                // # excluded as E2E/test fixtures
	MirrorCount     int                // # excluded as schedule-mirror artifacts
	Fixtures        []gardenerExcluded // the fixture beads, for the disclosure list
	ScheduleMirrors []gardenerExcluded // the schedule-mirror beads, for the disclosure list
	// TruePopulation is Rendered + Total — the full role:gardener universe the
	// hidden count reconciles the All chip against (the page renders TruePopulation
	// − Total). Carried so the affordance can state the whole-fleet arithmetic.
	Rendered       int
	TruePopulation int
}

// gardenersCensus is the self-census shown beside the chip strip so an operator
// can tell at a glance whether the chip counts are authoritative or drifting
// (ka-0000001407). It is an INTERNAL reconciliation: the lifecycle partition
// (Active | Disabled | Closed) is mutually exclusive and exhaustive over the
// rendered roster, so Active+Disabled+Closed MUST equal Total. When it does,
// the All chip is trustworthy at a glance; when it does not, a count is
// drifting and the badge says so loudly rather than letting a silently-wrong
// number stand.
//
// This deliberately does NOT compare against the external role:gardener label
// census — that delta (the page being a superset of the label query) is the
// data-taxonomy gap tracked by bug ka-0000000919, not something this page can
// fix or should claim to. What this page CAN guarantee is that its own chips
// add up, and that is what the badge certifies.
type gardenersCensus struct {
	Total         int  // the All chip count == len(allRows)
	LifecycleSum  int  // active + disabled + closed
	Reconciles    bool // LifecycleSum == Total
	ActiveCount   int
	DisabledCount int
	ClosedCount   int
}

// handleGardeners renders /gardeners — the gardener index. Reads are viewer-scoped
// (gated by the ambient auth middleware like /triggers and /agents); no control
// actions live here (enable/disable stays on /triggers, per the gardener-page v1
// decision in gardener_page.go).
//
// Query params:
//
//	?filter= one of all|active|unachieved|goal-achieved|disabled|closed|
//	         needs-attention (default: all)
func (s *Server) handleGardeners(w http.ResponseWriter, r *http.Request) {
	filter := parseGardenersFilter(r.URL.Query().Get("filter"))
	// ?sort=spend re-orders the list by per-gardener spend descending so the
	// operator can surface wasteful gardeners at a glance (ka-0000003220). Any
	// other value keeps the default run-centric ordering.
	sortBySpend := r.URL.Query().Get("sort") == "spend"

	data := map[string]any{
		"Filter":      gardenersFilterString(filter),
		"SortBySpend": sortBySpend,
		"Chips":       []gardenersFilterChip{},
		"Rows":        []gardenerRowView{},
		"Truncated":   false,
		"Limit":       gardenersListLimit,
		"GardenHref":  s.gardenBanner(r, retireGardener, gardenGardenersTarget),
	}

	ctx := r.Context()

	allRows, excluded, truncated, err := s.loadGardenerRows(ctx)
	if err != nil {
		s.logger.Error("listing gardeners", "error", err)
		data["Error"] = "Failed to load gardeners."
		s.render(w, r, "gardeners.html", data)
		return
	}
	data["Truncated"] = truncated

	// Counts for chip badges (computed over the unfiltered set so each chip
	// shows its own total, like /triggers).
	var activeCount, disabledCount, closedCount, needsAttentionCount int
	var goalAchievedCount, unachievedCount, runningCount int
	for _, row := range allRows {
		switch row.Status {
		case "active":
			activeCount++
		case "disabled":
			disabledCount++
		case "closed":
			closedCount++
		}
		if gardenerRowNeedsAttention(row) {
			needsAttentionCount++
		}
		if row.GoalState == "achieved" {
			goalAchievedCount++
		} else {
			unachievedCount++
		}
		if row.IsRunning {
			runningCount++
		}
	}

	rows := make([]gardenerRowView, 0, len(allRows))
	for _, row := range allRows {
		if gardenersRowMatches(row, filter) {
			rows = append(rows, row)
		}
	}

	sortGardenerRows(rows)
	if sortBySpend {
		// Spend-descending override (ka-0000003220): re-sort the already-ordered
		// rows so the costliest gardeners lead. Stable so the run-centric order is
		// the tie-break among equal-spend (and unsynced, spend-0) rows.
		sortGardenerRowsBySpend(rows)
	}

	hrefFor := func(v string) string {
		if v == "" {
			return s.basePath + "/gardeners"
		}
		return s.basePath + "/gardeners?filter=" + v
	}
	// liveCount is the operational fleet (every non-retired gardener) — the
	// default view's count (ka-0000004655). Closed == archived == retired.
	liveCount := len(allRows) - closedCount
	chips := []gardenersFilterChip{
		// Live leads as the default chip (empty ?filter= lands here). Archived
		// (retired) gardeners are tucked behind their own chip at the end.
		{Label: "Live", Value: "", Count: liveCount, Active: filter == gardenersFilterLive, Href: hrefFor("")},
		{Label: "All", Value: "all", Count: len(allRows), Active: filter == gardenersFilterAll, Href: hrefFor("all")},
		{Label: "Active", Value: "active", Count: activeCount, Active: filter == gardenersFilterActive, Href: hrefFor("active")},
		{Label: "Goal not reached yet", Value: "unachieved", Count: unachievedCount, Active: filter == gardenersFilterUnachieved, Href: hrefFor("unachieved")},
		{Label: "🏁 Goal achieved", Value: "goal-achieved", Count: goalAchievedCount, Active: filter == gardenersFilterGoalAchieved, Href: hrefFor("goal-achieved")},
		{Label: "Disabled", Value: "disabled", Count: disabledCount, Active: filter == gardenersFilterDisabled, Href: hrefFor("disabled")},
		{Label: "Needs attention", Value: "needs-attention", Count: needsAttentionCount, Active: filter == gardenersFilterNeedsAttention, Href: hrefFor("needs-attention")},
		{Label: "🗄 Archived", Value: "archived", Count: closedCount, Active: filter == gardenersFilterArchived, Href: hrefFor("archived")},
	}

	data["Chips"] = chips
	data["Rows"] = rows
	data["Total"] = len(allRows)

	// Fleet-health summary strip — leads the page with signal (ka-0000007180).
	data["Summary"] = gardenersSummary{
		Total:          len(allRows),
		Running:        runningCount,
		Live:           liveCount,
		Active:         activeCount,
		NeedsAttention: needsAttentionCount,
		Disabled:       disabledCount,
		Closed:         closedCount,
		GoalAchieved:   goalAchievedCount,
		BasePath:       s.basePath,
	}

	// Self-census reconciliation badge: certify that the lifecycle partition
	// (Active | Disabled | Closed) sums to the All chip, so an operator can
	// trust the chip counts at a glance (ka-0000001407).
	lifecycleSum := activeCount + disabledCount + closedCount
	data["Census"] = gardenersCensus{
		Total:         len(allRows),
		LifecycleSum:  lifecycleSum,
		Reconciles:    lifecycleSum == len(allRows),
		ActiveCount:   activeCount,
		DisabledCount: disabledCount,
		ClosedCount:   closedCount,
	}

	// "N hidden — why" affordance (bug ka-0000007605): the All chip is the LIVE
	// operational fleet, which deliberately excludes E2E fixtures and foreign
	// schedule-mirror artifacts. Account for that delta explicitly so the page
	// shows the whole role:gardener population (rendered + hidden) and the delta
	// no longer reads as an unexplained coverage hole. Zero-valued (Total 0) when
	// nothing is excluded — the template then renders no disclosure.
	data["Hidden"] = buildGardenersHidden(len(allRows), excluded)

	s.render(w, r, "gardeners.html", data)
}

// loadGardenerRows fetches every gardener (both the modern type=trigger +
// role:gardener primitive and the legacy type=schedule + role:gardener one),
// projects each into a gardenerRowView, and enriches every row with its actual
// last-run result. It is the shared projection behind both /gardeners and the
// /maps gardener picker (ka-0000003229) so the two never drift. truncated is true
// when either primitive's list hit gardenersListLimit.
//
// Owner resolution stays bounded (no N+1): gardenerRowFromBead → gardenerViewFromBead
// memoizes the operator directory + per-owner alias lookups via gardenerOwnerCache,
// the property gardener_owner_nplus1_test.go guards. The last-run enrichment is a
// single BatchGetBeads call regardless of gardener count.
func (s *Server) loadGardenerRows(ctx context.Context) (rows []gardenerRowView, excluded []gardenerExcluded, truncated bool, err error) {
	var beads []*beadsapi.BeadDetail
	seen := map[string]bool{}
	collect := func(q beadsapi.ListBeadsQuery) error {
		res, lerr := s.daemon.ListBeadsFiltered(ctx, q)
		if lerr != nil {
			return lerr
		}
		if res == nil {
			return nil
		}
		for _, b := range res.Beads {
			if b == nil || seen[b.ID] {
				continue
			}
			// Exclude E2E/test fixture gardeners (bug ka-0000008054): a fixture
			// bead like ka-0000004925 ("[E2E fixture] gardener bead", rendered as
			// "E2E demo-drive gardener" with hardcoded synthetic runs) must never
			// pollute the live fleet. seen is still marked so the second query
			// (role=gardener field) does not re-add it. This projection is shared
			// with the /maps gardener picker, so the fixture is filtered there too.
			//
			// Excluded beads are RECORDED (not just dropped) so /gardeners can
			// account for the All-chip-vs-true-population delta with an explicit
			// "N hidden — why" affordance rather than leaving 9 gardeners looking
			// unreachable (bug ka-0000007605).
			seen[b.ID] = true
			if excludeFixtureGardener(b) {
				excluded = append(excluded, gardenerExcluded{
					ID: b.ID, Title: b.Title, Reason: gardenerExcludedFixture,
					Href: s.basePath + "/" + b.ID,
				})
				continue
			}
			// Exclude foreign-namespace config/schedule-mirror beads (bug
			// ka-0000002811): the gardenio-next config mirror dual-writes its
			// schedule beads into the gardenio namespace as role:gardener triggers.
			// Per advice ka-0000007027 the config mirror must NOT carry schedules
			// onto a live operational surface — they render as real gardeners with
			// inflated foreign run counts and skew the "active in last 7d" tally.
			// seen is still marked above so the role-field query cannot re-add it.
			if isScheduleMirrorGardenerBead(b) {
				excluded = append(excluded, gardenerExcluded{
					ID: b.ID, Title: b.Title, Reason: gardenerExcludedScheduleMirror,
					Href: s.basePath + "/" + b.ID,
				})
				continue
			}
			beads = append(beads, b)
		}
		if res.Total > gardenersListLimit {
			truncated = true
		}
		return nil
	}
	// A gardener is canonically a `role:gardener`-LABELLED type=trigger (modern)
	// or type=schedule (legacy) bead. But a gardener created the "wrong way" —
	// `role=gardener` set only as a FIELD, never the label — would otherwise be
	// invisible here, since the label query never matches it (ka-0000001950).
	// Query BOTH the label and the role=gardener field, deduping by ID, so a
	// missing label can never hide a live gardener from /gardeners again.
	for _, typ := range []string{"trigger", "schedule"} {
		// WithBlockedOnDecision opts in the daemon's open-decision predicate so a
		// needs-attention row can surface the gardener's open decision inline
		// without an extra per-row fetch (ka-0000003950).
		if cerr := collect(beadsapi.ListBeadsQuery{
			Types:                 []string{typ},
			Labels:                []string{"role:gardener"},
			Sort:                  "updated_at",
			Limit:                 gardenersListLimit,
			WithBlockedOnDecision: true,
		}); cerr != nil {
			return nil, nil, false, cerr
		}
		if cerr := collect(beadsapi.ListBeadsQuery{
			Types:                 []string{typ},
			Fields:                map[string]string{"role": "gardener"},
			Sort:                  "updated_at",
			Limit:                 gardenersListLimit,
			WithBlockedOnDecision: true,
		}); cerr != nil {
			return nil, nil, false, cerr
		}
	}

	now := s.now()
	rows = make([]gardenerRowView, 0, len(beads))
	for _, b := range beads {
		rows = append(rows, s.gardenerRowFromBead(ctx, b, now))
	}

	// Three enrichment passes read only NO-FETCH seeded fields (lastAgentID /
	// targetEpicID / ownedEpicID) and write DISJOINT row fields, so they are
	// independent and run CONCURRENTLY (bug ka-0000008005 / D3): the index's
	// enrichment cost is ~max(pass) instead of the sum. Each is internally
	// degrade-safe (logs + falls back on error), so this waits for all three but
	// never surfaces an error — a slow/failed pass leaves its own fields at their
	// pre-enrichment fallback without blocking the others.
	//
	//   - enrichGardenerLastRuns: the ACTUAL last-run result (IsRunning, inline
	//     TLDR, real outcome, completion time) via ONE BatchGetBeads over the
	//     last-agent beads. Also seeds the attention candidates (decision id
	//     override + last-run blocker) the sequential attention passes below read.
	//   - enrichGardenerGoalStates: re-derives GoalState LIVE from each goal-target
	//     bead's CURRENT status (bug ka-0000007772) via ONE BatchGetBeads,
	//     correcting the stale close-time snapshot.
	//   - enrichGardenerOwnedEpics: the per-gardener owned-epic drain rollup (task
	//     ka-0000001266), one bounded batch of CountOnly subtree queries per
	//     distinct epic.
	//
	// The writes are to distinct struct fields of the shared rows slice (distinct
	// memory locations), and each pass reads only fields seeded before this point
	// — none reads a field another concurrent pass writes — so there is no data
	// race.
	var enrichWG sync.WaitGroup
	enrichWG.Add(3)
	go func() { defer enrichWG.Done(); s.enrichGardenerLastRuns(ctx, rows, now) }()
	go func() { defer enrichWG.Done(); s.enrichGardenerGoalStates(ctx, rows) }()
	go func() { defer enrichWG.Done(); s.enrichGardenerOwnedEpics(ctx, rows) }()
	enrichWG.Wait()

	// Repair the Runs column for legacy spawn_agent / bare-trigger gardeners that
	// carry no GardenerSpec engine tally (bug ka-0000005661): for the zero-tally
	// subset only, derive spawned/completed from the gardener's iteration beads
	// via the daemon CountOnly fast path. Runs AFTER enrichGardenerLastRuns so the
	// real last-run outcome wins when available; this only lifts the no-data
	// outcome chip for spec-less rows with a closed iteration but no last-agent
	// pointer. Bounded fan-out over only the zero-tally rows (~21 today).
	s.enrichGardenerRunCounts(ctx, rows)

	// Project the discovered open-decision / last-run-blocker candidates into the
	// rendered inline-attention chip, gated to the needs-attention slice so the
	// dormant tail stays uncluttered (ka-0000003950).
	for i := range rows {
		deriveAttentionSignal(&rows[i])
	}
	// Replace the placeholder decision label with the real prompt where the
	// daemon can serve the decision bead — one bounded batch over just the
	// attention rows that carry a decision id.
	s.enrichAttentionDecisionPrompts(ctx, rows)
	return rows, excluded, truncated, nil
}

// buildGardenersHidden projects the deliberately-excluded gardener beads into the
// "N hidden — why" affordance (bug ka-0000007605). rendered is the All-chip
// count (len(allRows)); excluded is loadGardenerRows' second return. The result
// reconciles the rendered roster against the true role:gardener population
// (rendered + len(excluded)) so the page can account for every gardener bead
// instead of leaving the delta unexplained. Returns the zero value (Total 0)
// when nothing is excluded — the template then renders no disclosure.
func buildGardenersHidden(rendered int, excluded []gardenerExcluded) gardenersHidden {
	h := gardenersHidden{
		Rendered:       rendered,
		TruePopulation: rendered + len(excluded),
	}
	for _, e := range excluded {
		switch e.Reason {
		case gardenerExcludedScheduleMirror:
			h.MirrorCount++
			h.ScheduleMirrors = append(h.ScheduleMirrors, e)
		default: // fixture (and any future class) lands here
			h.FixtureCount++
			h.Fixtures = append(h.Fixtures, e)
		}
	}
	h.Total = h.FixtureCount + h.MirrorCount
	return h
}

// gardenerTriggerSubKind classifies a gardener bead into its firing SUB-kind for
// the /maps trigger-node color (design.md §6): "cron" | "bead_query" | "event".
// It parses the bead's trigger def via the shared bridge.ParseTriggerDef — no
// extra fetch, the gardener bead already carries its trigger fields.
//
// Precedence mirrors how the gardeners actually fire:
//   - A parseable `cron` means a CLOCK fires the gardener → "cron". This wins
//     even when the gardener ALSO carries a `conditions` bead_query gate, because
//     `gb gardener start` writes both: the cron is the firing cadence, the
//     conditions gate is the loop_end stop, not what fires it.
//   - Otherwise a world-state predicate fires it → "bead_query": a `conditions`
//     pre-flight gate (HasConditions), or a bead_event / decision_resolved entry
//     in the multi-source `triggers` array.
//   - Anything else (an event source with neither cron nor predicate) → "event".
//
// Returns "" for a legacy type=schedule bead (the card keeps Kind=="schedule").
func gardenerTriggerSubKind(b *beadsapi.BeadDetail) string {
	if strings.EqualFold(b.Type, "schedule") {
		return ""
	}
	if strings.TrimSpace(b.Fields["cron"]) != "" {
		return "cron"
	}
	def := bridge.ParseTriggerDef(b, "schedule")
	// A mode-flipped gardener (trigger:multi + a bead_event entry) is
	// event-FIRED — its fallback cron entry is a watchdog, not the cadence
	// authority. gardenermode.Set clears `cron`, so the clock check above
	// already fell through; classify it "event" BEFORE the generic
	// bead_event→"bead_query" fold below, so the fallback cron entry cannot
	// win and the ⚡-with-fallback shape reads as event rather than a bead
	// query. (spec 2026-07-13-gardener-mode-flip-design.md)
	if hasLabel(b, "trigger:multi") {
		for _, t := range def.Triggers {
			if t.Kind == scheduler.TriggerKindBeadEvent {
				return "event"
			}
		}
	}
	if def.HasConditions {
		return "bead_query"
	}
	for _, t := range def.Triggers {
		switch t.Kind {
		case scheduler.TriggerKindCron:
			return "cron"
		case scheduler.TriggerKindBeadEvent, scheduler.TriggerKindDecisionResolved:
			return "bead_query"
		}
	}
	return "event"
}

// plannedHeirsFromBead projects a gardener bead's declared `chain.to`
// successors (task ka-0000005273 / "C3") into pending heir edges for the
// card-graph. It parses the trigger def (no fetch — the fields are on the bead)
// and returns one entry per declared heir, each with a synthetic, deterministic
// id so the pending node is stable across reloads. Returns nil when the gardener
// declares no chain.
func plannedHeirsFromBead(b *beadsapi.BeadDetail) []gardenerPlannedHeir {
	def := bridge.ParseTriggerDef(b, "schedule")
	if def.Chain == nil {
		return nil
	}
	heirs := def.Chain.Heirs()
	if len(heirs) == 0 {
		return nil
	}
	fanOut := len(heirs) > 1
	out := make([]gardenerPlannedHeir, 0, len(heirs))
	for i, h := range heirs {
		out = append(out, gardenerPlannedHeir{
			ID:     plannedHeirID(b.ID, i),
			Label:  plannedHeirLabel(h),
			FanOut: fanOut,
		})
	}
	return out
}

// plannedHeirID builds the synthetic id for a planned heir. It is namespaced so
// it can never collide with a real ka-id (which starts "gd-"), and is
// deterministic in the predecessor id + heir index.
func plannedHeirID(predID string, idx int) string {
	return fmt.Sprintf("planned:%s:%d", predID, idx)
}

// plannedHeirLabel captions a pending heir node: its phase, else a truncated
// terminal goal, else a generic "successor".
func plannedHeirLabel(h *bridge.ChainTo) string {
	if h == nil {
		return "successor"
	}
	if p := strings.TrimSpace(h.Phase); p != "" {
		return p
	}
	if g := strings.TrimSpace(h.TerminalGoal); g != "" {
		r := []rune(g)
		if len(r) > 40 {
			return string(r[:40]) + "…"
		}
		return g
	}
	return "successor"
}

// gardenerRowFromBead projects a gardener bead into a list row. It delegates the
// spec decode + owner resolution to gardenerViewFromBead (the shared header
// helper) so the index reflects exactly what the detail page would show.
func (s *Server) gardenerRowFromBead(ctx context.Context, b *beadsapi.BeadDetail, now time.Time) gardenerRowView {
	hdr, err := s.gardenerViewFromBead(ctx, b)
	if err != nil {
		// gardenerViewFromBead only errors on an internal decode path it never
		// actually returns from today; degrade to a bead-surface-only row.
		s.logger.Error("gardeners: build header", "id", b.ID, "error", err)
	}

	name := hdr.Name
	if name == "" {
		name = hdr.Title
	}
	if name == "" {
		name = b.ID
	}

	kind := "trigger"
	if strings.EqualFold(b.Type, "schedule") {
		kind = "schedule"
	}

	status := "active"
	switch {
	case hdr.IsClosed:
		status = "closed"
	case !hdr.Enabled:
		status = "disabled"
	}

	row := gardenerRowView{
		ID:                  hdr.ID,
		Name:                name,
		Title:               hdr.Title,
		TerminalGoal:        hdr.TerminalGoal,
		Prompt:              hdr.Prompt,
		OwnerDisplay:        hdr.OwnerDisplay,
		OwnerSlug:           hdr.OwnerSlug,
		OwnerEmail:          b.OperatedByEmail,
		RunsSpawned:         hdr.RunsSpawned,
		RunsCompleted:       hdr.RunsCompleted,
		LastRunOutcome:      gardenerLastRunOutcome(hdr.RunsCompleted, hdr.ConsecutiveFailures),
		ConsecutiveFailures: hdr.ConsecutiveFailures,
		Verdict:             hdr.Trajectory.Verdict,
		VerdictKind:         hdr.Trajectory.VerdictKind,
		VerdictDetail:       hdr.Trajectory.VerdictDetail,
		// Decoded firing + stop config (task ka-0000005782 / I2.4): the same
		// wakes-when / stops-when prose the detail page shows, so the index row
		// no longer needs a raw JSON dump. No extra fetch — hdr already decoded it.
		Legible: hdr.Legible,
		// Recent-run glyphs for the /maps board strip. Already built (no fetch) by
		// buildGardenerTrajectory from the embedded ProgressState; the board reads
		// its own tail-N off this (ka-0000001644).
		Runs: hdr.Trajectory.Runs,
		// Attention emphasis is scoped to ACTIVE gardeners: a stalled/stuck verdict
		// only warrants an operator's eye when the gardener can actually run. status
		// is "active"/"disabled"/"closed" — a disabled gardener won't fire, so a
		// stale verdict on it is noise, not a call to action (ka-0000003373).
		NeedsAttention:    status == "active" && (gardenerVerdictNeedsAttention(hdr.Trajectory.VerdictKind) || b.Fields["pour_stall"] == "true"),
		Enabled:           hdr.Enabled,
		IsClosed:          hdr.IsClosed,
		Status:            status,
		GoalState:         hdr.GardenerGoalState(),
		TerminationReason: hdr.TerminationReason(),
		Kind:              kind,
		TriggerSubKind:    gardenerTriggerSubKind(b),
		Ago:               hdr.Ago,
		UpdatedAtRFC:      hdr.UpdatedAtRFC,
		CreatedAtRFC:      rfcTime(b.CreatedAt),
		CreatedAgo:        fmtTimeAgo(b.CreatedAt, now),
		createdAt:         b.CreatedAt,
		Href:              s.basePath + "/gardener/" + hdr.ID,
		ExpandURL:         s.basePath + "/gardener/" + hdr.ID + "/invocations",
		lastAgentID:       hdr.LastAgentID,
		// Seed the inline-attention decision candidate from the daemon's
		// open-decision predicate on the gardener bead (depth≤2 parent-child, or a
		// child agent's decision_bead_id). applyLastRun may override it with the
		// live run's own decision_bead_id; deriveAttentionSignal projects it into
		// the rendered chip, gated to NeedsAttention rows (ka-0000003950).
		attentionDecisionID: b.BlockedOnDecisionBeadID,
		// Goal-target for the LIVE goal-state re-derive (bug ka-0000007772).
		// Resolved with no fetch here; enrichGardenerGoalStates batch-fetches the
		// target status and overrides GoalState below.
		targetEpicID: gardenerGoalTargetID(b),
		// Owned epic for the per-card rollup (task ka-0000001266). Resolved with
		// no fetch here; enrichGardenerOwnedEpics batch-counts the subtree below.
		ownedEpicID: gardenerOwnedEpicID(b),
	}

	// Planned successors (task ka-0000005273 / "C3"): a gardener that DECLARES a
	// `chain.to` but whose heir has not yet materialized. Project each declared
	// heir into a pending edge so the /maps card-graph shows a drawn successor
	// immediately after authoring, before the C4 engine spawns it. No extra
	// fetch — the trigger def is parsed from the gardener bead we already hold.
	row.PlannedHeirs = plannedHeirsFromBead(b)

	// Pouring-but-never-worked verdict, stamped on the trigger bead by the
	// engine's pouring-stall sweep (ka-0000000957). Read straight off the field.
	if b.Fields["pour_stall"] == "true" {
		row.PourStalled = true
		detail := pickNonEmpty(b.Fields["pour_stall_molecules"], "?") + " inert molecule(s)"
		if age := b.Fields["pour_stall_oldest_age"]; age != "" {
			detail += ", oldest quiet " + age
		}
		row.PourStallDetail = detail
	}

	// Per-gardener spend roll-up (ka-0000003220): read the controller-maintained
	// gardener_spend_usd straight off the bead (no fetch). A gardener that has not
	// been rolled up yet carries no field — leave HasSpend false so the column
	// renders blank rather than a misleading "$0.00".
	applyGardenerSpend(&row, b.Fields, now)

	// Overdue / silent-gardener liveness (bug ka-0000006095 / A1). Seed the
	// bead-surface signals (budget exhaustion + cron lateness) here with no
	// fetch; the silent-progress signal needs the last-agent bead's
	// last_progress_at and is refined in applyLastRun. An overdue/silent gardener
	// bumps NeedsAttention so a silently-dead loop pins to the top of the list.
	row.livenessInput = hdr.livenessInput()
	row.Liveness = deriveGardenerLiveness(row.livenessInput, now)
	if status == "active" && row.Liveness.NeedsAttention() {
		row.NeedsAttention = true
	}

	// Next-fire: only meaningful for an enabled, non-closed gardener carrying a
	// parseable cron. Closed/disabled gardeners will not fire, and bead-trigger
	// gardeners have no cron — leave the slot empty in both cases (the template
	// suppresses it). This is the default sort key, so populate nextFireAt too.
	if status == "active" {
		cron := strings.TrimSpace(b.Fields["cron"])
		tz := b.Fields["timezone"]
		// Cadence fallback (ka-0000007640): a gardener migrated onto a `triggers:`
		// array with its legacy cron cleared keeps the cadence in the first
		// kind:cron entry. Excluded for event-mode (trigger:multi) gardeners,
		// whose cron entry is a watchdog — not a clock countdown to surface here.
		if cron == "" && !hasLabel(b, "trigger:multi") {
			cron, tz = cronFromTriggers(b.Fields["triggers"])
		}
		if cron != "" {
			nf, ferr := computeNextFire(cron, tz, now)
			if ferr != nil {
				row.NextFireError = ferr.Error()
			} else {
				row.nextFireAt = nf
				row.NextFire = nf.Format("2006-01-02 15:04 MST")
				row.NextFireRelative = fmtTimeUntil(nf, now)
			}
		}
	}

	return row
}

// sortGardenerRows applies the run-centric gardener-index ordering (epic
// ka-0000004446 — operator wants "what is happening now, then what happened most
// recently", not a status-group/next-fire view), with an attention-first pin on
// top (ka-0000002827 — needs-attention gardeners must surface at a glance in the
// default All view, not only behind ?filter=needs-attention):
//
//  0. NEEDS ATTENTION FIRST — open gardeners whose health verdict is stalled or
//     stuck pin to the very top, so an on-call operator's eye lands on the
//     runaway/wedged loops before anything else. Within this group the
//     remaining keys (running, recency, …) still apply, so the freshest /
//     most-active troubled gardener leads.
//  1. RUNNING FIRST — gardeners with a live in-flight last run float up next, so
//     an operator sees active work at a glance. Among running rows, tie-break by
//     most-recently-active (last-agent UpdatedAt desc) so the freshest live run
//     leads.
//  2. Among non-running rows, by MOST-RECENTLY-COMPLETED (LastCompletedAt desc).
//     Rows with no completed run (zero LastCompletedAt — no fetchable last-agent
//     bead, or it never closed cleanly) sink below those that have one.
//  3. Closed gardeners stay BELOW runnable ones: they cannot run again, so even a
//     recent final completion should not outrank a live or schedulable gardener.
//     This is the one status-group concession to the otherwise run-centric order
//     (epic note: "keep closed gardeners below active/disabled").
//  4. Remaining ties break by newest-created, then deterministically by ID.
//
// NeedsAttention is scoped to open gardeners (see gardenerRowFromBead), so the
// attention pin and the closed-sink never fight: a closed gardener is never
// flagged NeedsAttention, so the two ranks are consistent.
//
// Note this deliberately drops the old next-fire-ascending key: the operator
// reframed the list around run activity (running / recently-completed), not the
// schedule. Next-fire still renders as a chip; it just no longer drives order.
func sortGardenerRows(rows []gardenerRowView) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]

		// Needs-attention pins to the very top: an open gardener that is stalled
		// or stuck is what an on-call operator should act on first.
		if a.NeedsAttention != b.NeedsAttention {
			return a.NeedsAttention // needs-attention sorts before everything else
		}

		// Closed gardeners sink below any runnable (active/disabled) gardener,
		// regardless of run recency — they cannot run again.
		ac, bc := boolRank(a.IsClosed), boolRank(b.IsClosed)
		if ac != bc {
			return ac < bc
		}

		// Running first.
		if a.IsRunning != b.IsRunning {
			return a.IsRunning // running sorts before non-running
		}

		if a.IsRunning { // both running: freshest activity first
			if !a.lastActivityAt.Equal(b.lastActivityAt) {
				return a.lastActivityAt.After(b.lastActivityAt)
			}
		} else { // both not running: most-recently-completed first
			ai, aj := a.LastCompletedAt, b.LastCompletedAt
			if ai.IsZero() != aj.IsZero() {
				return !ai.IsZero() // the one WITH a completed run sorts first
			}
			if !ai.IsZero() && !ai.Equal(aj) {
				return ai.After(aj)
			}
		}

		// Newest-created first, then deterministic ID tie-break.
		if !a.createdAt.Equal(b.createdAt) {
			return a.createdAt.After(b.createdAt)
		}
		return a.ID < b.ID
	})
}

// boolRank maps a bool to a sort rank: false (0) sorts before true (1). Used to
// push closed gardeners (IsClosed=true) below runnable ones.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// gardenerLastRunOutcome derives the most-recent-run outcome from the engine-owned
// counters the gardener spec already carries — no per-row iteration fetch. The
// engine bumps ConsecutiveFailures on a failed completed run and resets it to 0
// on a successful one (see gardener_evaluator.go gardenerPollLastAgent), so a
// non-zero count means the last completed run failed; zero with at least one
// completed run means it succeeded; zero completed runs means none yet ("").
func gardenerLastRunOutcome(runsCompleted, consecutiveFailures int) string {
	switch {
	case consecutiveFailures > 0:
		return "failed"
	case runsCompleted > 0:
		return "ok"
	default:
		return ""
	}
}

// fleetGardenerIsLive reports whether a gardener row belongs to the LIVE fleet: an
// open, enabled, modern type=trigger gardener that can actually fire. Disabled
// and closed gardeners, and legacy type=schedule gardeners (which no longer fire
// after D10 removed type=schedule firing — see gardener-loop-runbook), are NOT
// live. It is a pure predicate over gardenerRowView.
//
// Relocated here from the retired /maps fleet board (ka-0000000247): the
// live/historical split it encoded is genuinely useful beyond the old map, and
// TestFleetBoardAndGardenersQueryAgree still guards that this predicate and the
// canonical loadGardenerRows projection agree on the live-gardener set.
func fleetGardenerIsLive(r gardenerRowView) bool {
	return r.Status == "active" && r.Kind == "trigger"
}

// gardenersRowMatches reports whether a row passes the active filter.
func gardenersRowMatches(row gardenerRowView, filter gardenersFilter) bool {
	switch filter {
	case gardenersFilterLive:
		// The operational fleet: everything not formally retired (ka-0000004655).
		// A retired gardener is closed, so "live" is simply "not closed".
		return row.Status != "closed"
	case gardenersFilterActive:
		return row.Status == "active"
	case gardenersFilterDisabled:
		return row.Status == "disabled"
	case gardenersFilterArchived:
		return row.Status == "closed"
	case gardenersFilterNeedsAttention:
		return gardenerRowNeedsAttention(row)
	case gardenersFilterGoalAchieved:
		return row.GoalState == "achieved"
	case gardenersFilterUnachieved:
		// "Haven't achieved the terminal goal YET": every gardener that is not a
		// confirmed achiever — the live ones still working toward it AND the
		// closed ones that stopped without reaching it.
		return row.GoalState != "achieved"
	default:
		return true
	}
}

func parseGardenersFilter(v string) gardenersFilter {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "all":
		return gardenersFilterAll
	case "active":
		return gardenersFilterActive
	case "disabled":
		return gardenersFilterDisabled
	case "archived", "closed":
		// "closed" is the legacy alias for the archived (retired) view; both
		// resolve to the same row set so old bookmarks keep working
		// (ka-0000004655).
		return gardenersFilterArchived
	case "needs-attention":
		return gardenersFilterNeedsAttention
	case "goal-achieved":
		return gardenersFilterGoalAchieved
	case "unachieved":
		return gardenersFilterUnachieved
	default:
		// Empty / unrecognized → the LIVE fleet (ka-0000004655). The page no
		// longer defaults to All, so a retired gardener stays out of view until
		// the operator clicks Archived.
		return gardenersFilterLive
	}
}

func gardenersFilterString(f gardenersFilter) string {
	switch f {
	case gardenersFilterAll:
		return "all"
	case gardenersFilterActive:
		return "active"
	case gardenersFilterDisabled:
		return "disabled"
	case gardenersFilterArchived:
		return "archived"
	case gardenersFilterNeedsAttention:
		return "needs-attention"
	case gardenersFilterGoalAchieved:
		return "goal-achieved"
	case gardenersFilterUnachieved:
		return "unachieved"
	default:
		return "live"
	}
}
