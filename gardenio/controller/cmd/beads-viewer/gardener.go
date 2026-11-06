package main

// Gardener Page view model + helpers (epic ka-0000001579, task ka-0000005461,
// design doc ka-0000003219 §2/§5).
//
// A gardener is a `type=trigger` bead carrying `trigger:schedule` + `role:gardener`
// labels, whose gardener-loop state lives in a single JSON-encoded `gardener`
// field decoded into bridge.GardenerSpec. Every agent iteration the gardener
// spawns is stamped by stampGardenerLoopContext (internal/bridge/triggers.go)
// with `schedule_id` = the gardener trigger bead's ID, plus the back-pointer
// chain (`schedule_prior_agent_id`) and per-run snapshot counters.
//
// This file builds:
//   - gardenerView           — the gardener-page header, from the trigger bead.
//   - gardenerIterationView   — one run in the life-thread; EMBEDS agentRowView
//                              (built by the existing agentRowFromBeadCtx, no
//                              change to that function) and overlays the parsed
//                              schedule_* context.
//   - gardenerViewFromBead    — header helper.
//   - gardenerIterations      — ordered life-thread helper (chain-walk primary,
//                              created_at fallback; run numbering + RunLabel).

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
)

// gardenerIterationPageLimit caps how many iterations a single gardenerIterations
// call materializes. It is a page size, not a correctness cap: the header's
// IterationCount uses the daemon's reported total so the count stays honest
// even when the rendered thread is truncated (design doc §3 pagination note).
const gardenerIterationPageLimit = 500

// gardenerView is the gardener-page header. Sourced from the gardener trigger
// bead's `gardener` JSON field (decoded into bridge.GardenerSpec via the shared
// bridge.ParseTriggerDef) plus the bead's identity/status/ownership.
//
// It mirrors the triggerRowView (trigger metadata) / agentRowView (agent card)
// split that already exists in the viewer: gardenerView is the metadata header,
// gardenerIterationView is the per-run card.
type gardenerView struct {
	// --- Identity (from the trigger bead) ---
	ID    string // the trigger bead ID == the schedule_id stamped on iterations
	Title string

	// --- Goal + exit (GardenerSpec) ---
	Name          string
	TerminalGoal  string
	ExitCondition string
	OnExit        string

	// Prompt is the gardener's per-run `prompt` field — the operative instruction
	// sent verbatim to every run the gardener spawns (gardener_ops.go run-now:
	// `prompt := strings.TrimSpace(def.Prompt)`). It is a TOP-LEVEL trigger-bead
	// field (bead.Fields["prompt"]), present whether or not the bead carries a
	// gardener blob, so it is read straight off the bead — not from GardenerSpec.
	// It is the single most behavior-defining knob a gardener has, yet was never
	// surfaced on any modern gardener surface (bug ka-0000003596); the summary
	// renders it in a collapsible "Run prompt" block and the goal falls back to a
	// prompt-derived one-liner when TerminalGoal is empty.
	Prompt string

	// --- Budget + retry (GardenerSpec) ---
	RunBudget       int
	MaxRuns         int
	RetryOnFailure  bool
	MaxRetries      int
	RunTimeout      string
	Timeout         string
	OneSpawnAtATime bool

	// IsStanding marks a perpetual gardener (bug ka-0000006238) — one recognized
	// by the engine's single standing-marker discriminator (bridge.GardenerIsStanding:
	// standing:true in the blob OR the perpetual:true label). For such a gardener
	// run_budget / max_runs are NON-ENFORCED accounting — the engine exempts it
	// from both the run_budget gate and the max_runs loop_end kind, so the only
	// real ceilings are the hard_stop brakes (consecutive_failures, wall_clock).
	// It gates two surfaces: the budget-burn meter is suppressed (a burn bar
	// against a non-ceiling reads as a false "about to stop / runaway" signal),
	// and a "standing (unbounded)" chip renders in its place so the operator gets
	// the affirmative "this is by design, not a runaway" answer.
	IsStanding bool

	// --- Runtime counters (GardenerSpec — engine-owned) ---
	RunsSpawned         int
	RunsCompleted       int
	ConsecutiveFailures int
	LastAgentID         string

	// --- Resume surfacing (terminal_goal master gate, bead ka-0000005353).
	//     ResumesWithoutProgress / ResumeBudget show how close the gardener is
	//     to operator escalation; LastResumeReason names the premature stop
	//     that last re-armed it; StuckEscalated is true when the engine flagged
	//     the gardener gardener-stuck-escalate (resume budget spent, awaiting the
	//     resumer watchdog / operator). These drive the resume pill + stuck
	//     badge so an operator can SEE the gate working (decision ka-0000006918). ---
	ResumesWithoutProgress int
	ResumeBudget           int
	LastResumeReason       string
	StuckEscalated         bool

	// --- Opaque structured JSON, pretty-printed (B6 stop-spec parser
	//     deferred — design doc §2: render in a <details> block, do not
	//     block on the engine parser). Empty string when the source field
	//     is absent. ---
	StopJSON     string
	ProgressJSON string
	// StopSummary is the human-readable "stops when …" rendering of the stop
	// spec (task ka-0000000897 / I2.3), shown above the raw JSON dump so an
	// operator reads legible conditions first. Empty when the gardener has no
	// stop spec (or it failed to parse — the JSON dump still renders).
	StopSummary []string

	// --- Ownership (raw — slug/display resolution is Decision 5,
	//     ka-0000002818; this helper carries the raw spec values + the
	//     bead's already-resolved OperatedBy display/slug so the owner
	//     section can render without a second decode). ---
	OwnerDisplay string // human label (trigger bead's OperatedByDisplay)
	OwnerSlug    string // -> /u/{slug} when resolvable; "" => no link
	SlackUserID  string // GardenerSpec.SlackUserID (raw)
	UserBeadID   string // GardenerSpec.UserBeadID (raw)

	// --- Trigger-bead surface ---
	Enabled      bool
	IsClosed     bool
	Ago          string
	UpdatedAtRFC string

	// --- Schedule / cadence (task ka-0000002425). Mirrors the index row's
	//     next-fire projection so an operator can manage one gardener from its
	//     detail page without bouncing back to /gardeners. Cron is the raw
	//     schedule expression ("cadence"); NextFire is the absolute next firing
	//     ("2026-06-06 15:04 MST"); NextFireRelative is the human "fires in
	//     12m"; NextFireError carries a cron parse error verbatim. The NextFire*
	//     fields populate only for an enabled, non-closed gardener carrying a
	//     parseable cron; all empty for closed/disabled gardeners or bead-trigger
	//     gardeners with no cron (the template suppresses the strip when Cron is
	//     empty). ---
	Cron             string
	Timezone         string
	NextFire         string
	NextFireRelative string
	NextFireError    string

	// --- Overdue / silent-gardener liveness (bug ka-0000006095 / A1). The
	//     NextFire projection above is computed from the cron REGARDLESS of
	//     whether the gardener is actually firing, so an enabled gardener whose
	//     cron has silently stopped firing still shows a healthy future
	//     next-fire. Liveness is the accuracy counter-signal: derived live from
	//     the fire/progress/budget signals, it flags a gardener that should have
	//     fired again and did not (overdue) or is still spawning but making no
	//     progress (silent). Populated by computeLiveness, called by the detail
	//     page (with last_progress_at from the iteration set) and the index
	//     enrich path (from the last-agent bead). Empty (no concern) until then.
	//
	//     lastFire / cooldownMinutes are the fetch-free inputs captured during
	//     the header build so computeLiveness can run without re-decoding the
	//     bead; they are lowercase (not template-facing) — the template reads
	//     Liveness.Overdue / Liveness.Silent + the detail strings.
	Liveness        gardenerLiveness
	lastFire        time.Time
	cooldownMinutes int

	// --- Termination provenance (set on close by the engine / operator ops).
	//     TerminatedSlot is the stop slot ("loop_end" | "hard_stop" |
	//     "operator"); TerminatedKind is the stop kind ("exit_condition",
	//     "bead_query", "max_runs", "retired", ...). Both empty for open
	//     gardeners (and for closed gardeners predating the fields). These drive
	//     the goal-achievement badge: a gardener "achieved its terminal goal"
	//     when it closed via a goal-completion path (loop_end exit_condition /
	//     bead_query / specific_bead_closed), vs. exhausting its budget/time or
	//     being retired. See gardenerKindIsGoalAchieved. ---
	TerminatedSlot string
	TerminatedKind string

	// --- Live goal-state target (bug ka-0000007772). The gardener's goal is
	//     "drive THIS target to done": for a subtree-draining gardener the
	//     target is the root epic named by a loop_end/conditions bead_query's
	//     `parent`; for a single-bead gardener it is the `specific_bead_closed`
	//     bead_id. We capture the id (TargetEpicID) and its CURRENT status
	//     (TargetEpicStatus), re-fetched at render time, so GardenerGoalState
	//     re-derives the disposition LIVE rather than trusting the
	//     terminated-slot/kind snapshot stamped at gardener-close.
	//
	//     The snapshot was the whole bug: it is computed once and never
	//     re-evaluated when the epic's status flips afterward — producing
	//     FALSE-NEGATIVES ("goal not reached" though the epic later CLOSED)
	//     and FALSE-POSITIVES ("GOAL ACHIEVED" though the epic is still OPEN).
	//
	//     TargetEpicID is "" when the gardener has no parent-scoped or
	//     single-bead goal we can resolve to a single authoritative bead (e.g.
	//     an exit_condition / gate gardener), in which case GardenerGoalState
	//     falls back to the snapshot provenance. TargetEpicStatus is "" when
	//     the target could not be fetched (pruned / daemon error); the live
	//     re-derive then degrades to the snapshot rather than guessing.
	//     TargetEpicID / TargetEpicStatus are populated by
	//     populateLiveGoalState (called by the index batch path and the detail
	//     page), NOT by gardenerViewFromBead, so the bare header build stays a
	//     pure no-fetch projection.
	TargetEpicID     string
	TargetEpicStatus string

	// --- Per-gardener spend roll-up (ka-0000003220). HasSpend gates rendering;
	//     SpendLabel is the pre-formatted "$12.34"; SpendStale flags a roll-up
	//     older than gardenerSpendStaleAfter. Read straight off the
	//     controller-maintained gardener_spend_usd field — never recomputed here.
	HasSpend   bool
	SpendLabel string
	SpendStale bool
	// SpendLevel buckets the dollar roll-up for runaway-cost styling on the
	// detail header chip (bug ka-0000001009 / C1): "" (below warn), "warn"
	// (expensive), "over" (runaway). Mirrors the index row's SpendLevel so the
	// two surfaces color the $ chip identically. Empty when HasSpend is false.
	SpendLevel string
	// SpendExcludesLiveRun flags that the roll-up under-reports because a run is
	// in flight (bug ka-0000001009 / C3): total_cost_usd is written only at
	// session close, so the summed figure excludes the live run's spend. The
	// header chip marks itself approximate + annotates the exclusion. Set in
	// handleGardener once LiveCount is known (a bare gardenerViewFromBead cannot
	// know liveness without the iteration set), gated on HasSpend.
	SpendExcludesLiveRun bool

	// --- Derived from the iteration set (filled by the runs section
	//     after gardenerIterations runs, so header + thread stay
	//     consistent in one render). ---
	IterationCount int // real runs == rendered cards minus never-ran shells
	LiveCount      int // open/in_progress iterations

	// IterationsUnavailable is true when the iterations lookup TRANSIENTLY
	// FAILED (gardenerIterations returned an error), so IterationCount is 0 not
	// because the gardener has no runs but because they could not be fetched this
	// render. It gates the "older runs pruned" framing off (bug ka-0000006095 /
	// A2): a real RunsSpawned minus a spuriously-zero IterationCount is NOT
	// evidence of pruning — the data was momentarily unfetchable. Set by the
	// page/bead-view handlers from the iterations error alongside IterationsError.
	IterationsUnavailable bool

	// DidNotRunCount is the number of never-ran spawn SHELLS in the rendered
	// thread (iterations the reconciler expired in the coop queue before they
	// claimed a pod — agent_state="expired"). They are EXCLUDED from
	// IterationCount (they are not runs) but still rendered in the thread, so
	// the header annotates the Iterations chip with "(+N never ran)" to explain
	// why the thread is longer than the run count (bug ka-0000006805).
	DidNotRunCount int

	// EngineCount is the always-shown, clearly-framed SECONDARY counter line
	// rendered beneath the authoritative Iterations chip. The PRIMARY counter
	// the operator reads is the live IterationCount (== the number of rendered
	// iteration cards, which can never contradict the body). RunsSpawned /
	// RunsCompleted are the engine's monotonic lifetime tally — they keep
	// counting long after the underlying agent beads are pruned, so on a
	// long-lived gardener RunsSpawned >> IterationCount (design doc §1.3: 439
	// spawned vs 13 findable, 86% pruned).
	//
	// EngineCount is therefore framed as an explained lifetime tally, NOT a
	// drift warning: when RunsSpawned >> IterationCount it carries the word
	// "pruned"/"older" so the operator understands *why* the body is shorter
	// than the spawn count, rather than concluding the page lost runs. Set by
	// reconcileCounters after IterationCount is filled. Always non-empty when
	// the gardener has any engine activity.
	EngineCount string

	// EnginePruned is true when the engine lifetime tally exceeds the live
	// iteration count by more than gardenerPrunedGap — i.e. older iteration
	// beads have been pruned and can no longer be rendered. The template uses
	// it to style the secondary line as an explanatory note (not a warning).
	EnginePruned bool

	// EngineDerived is true when the secondary count line was DERIVED from the
	// rendered life-thread (IterationCount / LiveCount) rather than read from a
	// GardenerSpec engine snapshot — the case for legacy spawn_agent / bare-trigger
	// gardeners that carry no `gardener` blob and so have a null engine tally
	// (bug ka-0000005661). The template uses it to source-label the line "from
	// life-thread" instead of "lifetime tally", so the operator knows the count
	// is the findable-run count, not the engine's monotonic lifetime counter.
	EngineDerived bool

	// --- Cross-run progress narrative (task ka-0000003359). Parsed from the
	//     GardenerSpec ProgressState into a trajectory strip + one-line health
	//     verdict, so the header reads as direction-over-time rather than a
	//     flat counter dump. Zero-valued (HasData=false) when there is nothing
	//     to narrate; the template suppresses the strip. ---
	Trajectory gardenerTrajectory

	// --- Legible firing + stop config (task ka-0000005782 / I2.4, design
	//     ka-0000006103 / D1.2). Decodes the trigger bead's opaque cron /
	//     conditions / triggers firing fields and the GardenerSpec `stop` block
	//     into human prose — wakes-when / chasing / stops-when — replacing the
	//     raw JSON dumps the list/preview/detail surfaces used to render. Health
	//     is Trajectory above; the four read as one at-a-glance set. Populated by
	//     buildGardenerLegible in gardenerViewFromBead so all three surfaces share
	//     one decode authority. The raw StopJSON/ProgressJSON above stay reachable
	//     behind a collapsed <details> audit escape-hatch. ---
	Legible gardenerLegible

	// HistoryTruncated is set when a per-key iteration query returned a full
	// gardenerIterationPageLimit page — the daemon holds more iterations for
	// that key than the page materializes, so the rendered life-thread is
	// missing its OLDEST runs. The template surfaces a "history truncated —
	// paginate" banner so the per-key cap is observable BEFORE a gardener
	// silently crosses it (task ka-0000002835; measured headroom: largest
	// single-key gardener is 184/500 today).
	//
	// This is distinct from EnginePruned: pruned runs no longer exist as beads
	// and can never be rendered, whereas truncated runs DO still exist in the
	// daemon — they are simply beyond this page's ceiling and reachable via
	// pagination (ka-0000006972). The two can be true independently.
	HistoryTruncated bool
}

// HistoryLimit exposes the per-key iteration page ceiling to the template so the
// truncation banner can name the exact cap without hardcoding it in the HTML.
func (gardenerView) HistoryLimit() int { return gardenerIterationPageLimit }

// NameDivergesFromTitle reports whether the gardener's displayed Name (its
// GardenerSpec.Name) differs from its bead Title, so the detail header can
// surface the underlying bead title rather than silently rendering only the
// spec name (bug ka-0000008054). Whitespace-insensitive; false when either is
// empty (the header already falls back to Title when Name is empty, so there is
// nothing to reconcile).
func (v gardenerView) NameDivergesFromTitle() bool {
	n := strings.TrimSpace(v.Name)
	t := strings.TrimSpace(v.Title)
	if n == "" || t == "" {
		return false
	}
	return n != t
}

// The gardener run-count reconciliation model (gardenerPrunedGap,
// reconcileCounters, fmtEngineCount) lives in gardener_counters.go.

// gardenerIterationView (one run in the gardener's life-thread) and the iteration
// ordering / run-numbering logic live in gardener_timeline.go (Q4, ka-0000004816).
// gardenerIterations below delegates to that canonical layer via
// orderGardenerIterations so the header and the thread share one ordering authority.

// gardenerViewFromBead builds the gardener-page header from a gardener trigger
// bead. It decodes the `gardener` field via the shared bridge.ParseTriggerDef
// (the same path the engine uses), so the header never drifts from the engine's
// understanding of the spec. A bead with no/invalid `gardener` field yields a
// header with zero GardenerSpec values (still a valid, if sparse, gardener view).
//
// IterationCount / LiveCount are left at zero here; the runs section fills them
// after gardenerIterations runs (design doc §2).
func (s *Server) gardenerViewFromBead(ctx context.Context, b *beadsapi.BeadDetail) (gardenerView, error) {
	// Resolve the owner slug on the trigger bead (best-effort; nil-safe) so the
	// header owner chip links to /u/{slug} like agent/trigger rows do.
	s.populateOperatedBySlug(ctx, b)

	v := gardenerView{
		ID:           b.ID,
		Title:        b.Title,
		Enabled:      b.Fields["enabled"] != "false", // trigger:schedule default-on
		IsClosed:     statusOpenSortKey(b.Status) != 0,
		Ago:          fmtTimeAgo(b.UpdatedAt, s.now()),
		UpdatedAtRFC: rfcTime(b.UpdatedAt),
		OwnerDisplay: b.OperatedByDisplay,
		OwnerSlug:    b.OperatedBySlug,

		TerminatedSlot: b.Fields["gardener_terminated_slot"],
		TerminatedKind: b.Fields["gardener_terminated_kind"],

		// Per-run prompt — the operative instruction (bug ka-0000003596). A
		// top-level trigger-bead field, present with or without a gardener blob, so
		// it is read straight off the bead here (before the spec decode below,
		// which may early-return for a bare-cron gardener).
		Prompt: strings.TrimSpace(b.Fields["prompt"]),
	}

	// Per-gardener spend roll-up (ka-0000003220): read the controller-maintained
	// gardener_spend_usd straight off the bead so the detail header shows the same
	// figure /gardeners does. No fetch, no recompute.
	if raw, ok := b.Fields[gardenerSpendField]; ok && raw != "" {
		spend := parseCostField(raw)
		v.HasSpend = true
		v.SpendLabel = fmtUSD(spend)
		v.SpendLevel = gardenerSpendLevel(spend) // runaway-cost bucket (C1)
		if syncedAt := b.Fields[gardenerSpendSyncedAtField]; syncedAt != "" {
			if t, perr := time.Parse(time.RFC3339, syncedAt); perr == nil {
				v.SpendStale = s.now().Sub(t) > gardenerSpendStaleAfter
			}
		}
	}

	// Schedule / cadence projection (task ka-0000002425). Mirror the index
	// row's logic so the detail header can answer "when does this fire next, on
	// what schedule" without bouncing back to /gardeners. Next-fire is only
	// meaningful for an enabled, non-closed gardener carrying a parseable cron;
	// closed/disabled gardeners will not fire and bead-trigger gardeners have no
	// cron — leave the NextFire* slots empty in those cases (the template
	// suppresses the relative/absolute fire while still surfacing the raw cron
	// cadence when present).
	// Fetch-free liveness inputs (bug ka-0000006095 / A1): the last actual fire
	// (freshest of last_outcome_at / last_run) and the cooldown, captured here so
	// computeLiveness can run without re-decoding the bead. last_progress_at is
	// supplied later by the caller from the iteration / last-agent bead.
	v.lastFire = gardenerLastFire(b.Fields)
	v.cooldownMinutes = gardenerCooldownMinutes(b.Fields)

	v.Cron = strings.TrimSpace(b.Fields["cron"])
	v.Timezone = strings.TrimSpace(b.Fields["timezone"])
	switch {
	case v.Cron != "":
		// Timed gardener: a clock cron fires it. Compute next-fire for an
		// enabled, open gardener (closed/disabled never fire — the template
		// then shows a "not scheduled" note beside the raw cadence).
		if v.Enabled && !v.IsClosed {
			now := s.now()
			if nf, ferr := computeNextFire(v.Cron, v.Timezone, now); ferr != nil {
				v.NextFireError = ferr.Error()
			} else {
				v.NextFire = nf.Format("2006-01-02 15:04 MST")
				v.NextFireRelative = fmtTimeUntil(nf, now)
			}
		}
	case hasLabel(b, "trigger:multi"):
		// Event-mode gardener (gardenermode.Set → trigger:multi + a bead_event
		// entry, cleared `cron`): the fallback cron entry is a watchdog, not
		// the cadence authority. Surface its expression in the cadence slot so
		// the operator can read the guaranteed minimum tempo, but leave
		// NextFire* empty — the gardener wakes on subtree activity, so a clock
		// countdown would misrepresent when it fires (NextFire may stay empty
		// in v1; spec 2026-07-13-gardener-mode-flip-design.md). Only a gardener
		// that actually carries a bead_event entry gets the fallback-cadence
		// treatment; a bare trigger:multi with only a cron entry falls through
		// (its cron sits in the field above, not here).
		d := bridge.ParseTriggerDef(b, "schedule")
		hasEvent := false
		var fallbackCron, fallbackTZ string
		for _, t := range d.Triggers {
			if t.Kind == scheduler.TriggerKindBeadEvent {
				hasEvent = true
			}
			if fallbackCron == "" && t.Kind == scheduler.TriggerKindCron && strings.TrimSpace(t.Cron) != "" {
				fallbackCron = strings.TrimSpace(t.Cron)
				fallbackTZ = strings.TrimSpace(t.Timezone)
			}
		}
		if hasEvent && fallbackCron != "" {
			v.Cron = fallbackCron
			if fallbackTZ != "" {
				v.Timezone = fallbackTZ
			}
		}
	default:
		// Cadence fallback (ka-0000007640): a schedule-source gardener migrated
		// onto a `triggers:` array with its legacy cron/timezone cleared keeps
		// its cadence in the first kind:cron entry. This branch is reached only
		// when the legacy cron is empty AND the gardener is not event-mode
		// (trigger:multi handled above), so a cron entry here IS the clock
		// cadence — surface it and compute next-fire like a timed gardener.
		if fbCron, fbTZ := cronFromTriggers(b.Fields["triggers"]); fbCron != "" {
			v.Cron = fbCron
			if fbTZ != "" {
				v.Timezone = fbTZ
			}
			if v.Enabled && !v.IsClosed {
				now := s.now()
				if nf, ferr := computeNextFire(v.Cron, v.Timezone, now); ferr != nil {
					v.NextFireError = ferr.Error()
				} else {
					v.NextFire = nf.Format("2006-01-02 15:04 MST")
					v.NextFireRelative = fmtTimeUntil(nf, now)
				}
			}
		}
	}

	// Decode the gardener spec through the shared parser. Non-gardener or
	// malformed `gardener` JSON yields a nil Gardener; we return the header
	// populated only from the bead surface (no error — the page can still
	// render the identity + ownership).
	def := bridge.ParseTriggerDef(b, "schedule")
	// Standing (perpetual) recognition (bug ka-0000006238) — computed from the
	// SAME engine discriminator that gates the run_budget/max_runs exemption, so
	// the surface can never disagree with the engine on which gardeners are
	// unbounded. Read before the nil-spec early return: a bare-cron gardener can
	// still carry the perpetual:true label (recognized off def.Labels).
	v.IsStanding = bridge.GardenerIsStanding(def)
	spec := def.Gardener
	if spec == nil {
		// A bare bead-trigger / legacy schedule gardener carries no GardenerSpec
		// blob, but its FIRING config (cron / conditions / triggers) still lives
		// on the bead — decode it so wakes-when / chasing render even without a
		// spec. The stop section is simply empty for these (no `stop` block to
		// decode). Without this the whole legible strip was blank for every
		// bare-cron gardener (they showed only the ⏰ glyph with no summary).
		v.Legible = buildGardenerLegible(b, nil, &v)
		return v, nil
	}

	v.Name = spec.Name
	v.TerminalGoal = spec.TerminalGoal
	v.ExitCondition = spec.ExitCondition
	v.OnExit = spec.OnExit
	v.RunBudget = spec.RunBudget
	v.MaxRuns = spec.MaxRuns
	v.RetryOnFailure = spec.RetryOnFailure
	v.MaxRetries = spec.MaxRetries
	v.RunTimeout = spec.RunTimeout
	v.Timeout = spec.Timeout
	v.OneSpawnAtATime = spec.OneSpawnAtATime
	v.RunsSpawned = spec.RunsSpawned
	v.RunsCompleted = spec.RunsCompleted
	v.ConsecutiveFailures = spec.ConsecutiveFailures
	v.LastAgentID = spec.LastAgentID
	v.ResumesWithoutProgress = spec.ResumesWithoutProgress
	v.LastResumeReason = spec.LastResumeReason
	// ResumeBudget==0 means "use the engine default"; surface the effective
	// value so the pill reads N/5 rather than N/0. A negative budget (resume
	// disabled) is shown verbatim. Keep this in sync with defaultResumeBudget
	// in internal/bridge/gardener_evaluator.go.
	if spec.ResumeBudget == 0 {
		v.ResumeBudget = 5
	} else {
		v.ResumeBudget = spec.ResumeBudget
	}
	v.StuckEscalated = b.Fields["gardener_stuck_escalated"] == "true" ||
		slices.Contains(b.Labels, "gardener-stuck-escalate")
	v.SlackUserID = spec.SlackUserID
	v.UserBeadID = spec.UserBeadID
	v.StopJSON = prettyRawJSON(spec.Stop)
	v.ProgressJSON = prettyRawJSON(spec.ProgressState)
	// Human-readable stop-condition summary (task ka-0000000897 / I2.3): the
	// legible "stops when …" lines that replace the opaque JSON dump as the
	// primary reading surface. The raw StopJSON <details> stays as the escape
	// hatch. A parse miss yields no summary lines (the JSON dump still renders).
	if stopSpec, err := scheduler.ParseStopSpec(string(spec.Stop)); err == nil {
		v.StopSummary = summarizeStopSpec(stopSpec)
	}

	// Parse the opaque ProgressState into a human trajectory strip + a
	// one-line health verdict (task ka-0000003359). The raw <details> JSON
	// dump above stays as the audit/escape hatch; this is the narrative the
	// operator reads first.
	v.Trajectory = buildGardenerTrajectory(spec)

	// Decode the opaque firing (cron/conditions/triggers) + stop config into
	// human prose — wakes-when / chasing / stops-when (task ka-0000005782 /
	// I2.4). Built here off the same bead + spec the header already holds so
	// every surface (list/preview/detail) shares one decode authority. The raw
	// StopJSON above stays as the collapsed audit escape-hatch.
	v.Legible = buildGardenerLegible(b, spec, &v)

	// Resolve the owner in the three-tier priority order (UserBeadID →
	// SlackUserID → trigger operated_by_id/operated_by). The bead-surface
	// values set above are tier 3; override them only when an earlier tier
	// yields a display so the chip reflects the gardener's owning user, not
	// just whoever last operated the trigger bead (design doc §6).
	if d, sl := s.resolveGardenerOwner(ctx, b, spec.UserBeadID, spec.SlackUserID); d != "" {
		v.OwnerDisplay = d
		v.OwnerSlug = sl
	}

	return v, nil
}

// gardenerIterations fetches the gardener's spawned agent iterations
// (schedule_id == scheduleID) and returns them as an ordered life-thread, plus
// a truncated flag: true when any per-key query filled an entire
// gardenerIterationPageLimit page, meaning the daemon holds more iterations for
// that key than this page materializes and the OLDEST runs are missing from the
// rendered thread (task ka-0000002835 — make the per-key cap observable before a
// gardener silently crosses it).
//
// Ordering authority (design doc §5, Decision 4):
//   - PRIMARY: the schedule_prior_agent_id chain — each iteration points at the
//     previous run's agent bead. Walk the chain to assign run numbers 1..N
//     root-forward.
//   - FALLBACK: when the chain is broken (a prior id references an iteration not
//     in the set, or the walked chain is shorter than the set), fall back to
//     created_at ascending for the whole set and mark affected cards ByTime so
//     the operator knows ordering is heuristic.
//
// The returned slice is newest-first (operators care about the live/last run
// first); RunNumber is computed root-forward and displayed on each card
// regardless of thread order.
//
// Lookup note: the daemon-side indexed field filter (Decision 2, child
// ka-0000000412) forwards ListBeadsQuery.Fields into the daemon's
// field_filters, backed by the partial B-tree index from daemon migration 031.
//
// Dual-key match (ka-0000007051): a gardener's iterations carry the gardener's id
// under one of TWO field keys depending on the spawn path — the bridge trigger
// path stamps trigger_id (modern gardeners), the legacy scheduler path stamps
// schedule_id. The daemon Fields filter is AND-equality across entries, so we
// cannot OR the two keys in a single query; we issue one query per key and
// union the results (deduping by bead id). Ordering/run-numbering is delegated
// to the canonical timeline layer (orderGardenerIterations, Q4).
func (s *Server) gardenerIterations(ctx context.Context, scheduleID string, now time.Time) ([]gardenerIterationView, bool, error) {
	if scheduleID == "" {
		return nil, false, nil
	}

	// Query both owner keys and union. Mirrors the dual-key ownership check in
	// reconciler.closeOrphanedMirrorTask: accept either trigger_id (bridge
	// path) or schedule_id (legacy path) == this gardener's id.
	beads := make([]*beadsapi.BeadDetail, 0, gardenerIterationPageLimit)
	seen := make(map[string]bool)
	truncated := false
	for _, key := range []string{fieldTriggerID, fieldScheduleID} {
		res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:  []string{"agent"},
			Fields: map[string]string{key: scheduleID},
			// Newest-first so a full page drops the OLDEST runs, not the live
			// end of the thread (gd-Ahi1H2jR9a). orderGardenerIterations does
			// the ordering; the sort only picks which iterations make the page.
			Sort:  "-created_at",
			Limit: gardenerIterationPageLimit,
		})
		if err != nil {
			return nil, false, err
		}
		// A per-key page that comes back full means the daemon holds at least as
		// many iterations for this key as the cap — the page sliced off the
		// oldest runs. Surface that so the per-key ceiling is observable on the
		// page (task ka-0000002835) rather than silently truncating. We test the
		// raw per-key result length (not the unioned/deduped total) because the
		// cap is applied PER QUERY; either key hitting it truncates that key's
		// slice of the thread.
		if len(res.Beads) >= gardenerIterationPageLimit {
			truncated = true
		}
		// Defensive client-side filter: even with the daemon field filter,
		// guard against a fixture/daemon that returns adjacent agents (e.g. the
		// test fakeDaemon serves all type=agent beads from liveAgentsResp).
		for _, b := range res.Beads {
			if b == nil || b.ID == "" || seen[b.ID] {
				continue
			}
			if b.Fields[fieldTriggerID] == scheduleID || b.Fields[fieldScheduleID] == scheduleID {
				seen[b.ID] = true
				beads = append(beads, b)
			}
		}
	}
	if len(beads) == 0 {
		return nil, truncated, nil
	}

	// Resolve owner slugs in one batch so each iteration card's operator chip
	// links like the /agents rows.
	s.populateOperatedBySlugs(ctx, beads)

	// The head hint (engine's newest-run pointer, re-derived here from the
	// iteration set) anchors the chain walk; orderGardenerIterations walks
	// schedule_prior_agent_id from there, falling back to created_at ordering
	// when the chain is broken. The build closure reads each iteration's own
	// project so the per-card project chip stays correct across a mixed-project
	// gardener (a single shared project would mislabel every row).
	return orderGardenerIterations(beads, lastAgentID(beads), func(b *beadsapi.BeadDetail) agentRowView {
		return s.agentRowFromBeadCtx(ctx, b, b.Fields["project"], now)
	}), truncated, nil
}

// lastAgentID returns the head hint for the chain walk: the gardener's
// GardenerSpec.LastAgentID is not on the iteration beads, so we derive the most
// likely head as the iteration nothing else points at. orderGardenerIterations
// independently re-derives the head when this hint is empty or stale, so this
// is only a best-effort accelerator, not an authority.
func lastAgentID(beads []*beadsapi.BeadDetail) string {
	pointedAt := make(map[string]bool, len(beads))
	for _, b := range beads {
		if prior := b.Fields[fieldSchedulePriorAgentID]; prior != "" {
			pointedAt[prior] = true
		}
	}
	head := ""
	for _, b := range beads {
		if b != nil && b.ID != "" && !pointedAt[b.ID] {
			if head != "" {
				return "" // ambiguous; let the timeline layer decide
			}
			head = b.ID
		}
	}
	return head
}

// gardenerGoalAchievementKinds are the stop kinds that mean a gardener CLOSED
// BECAUSE IT FINISHED ITS WORK — i.e. it reached the terminal goal — rather
// than being cut off by a budget/time guard or retired by hand. These are the
// "loud, this gardener WON" set:
//
//   - exit_condition       — the gardener's ExitCondition matched (the canonical
//     "goal reached" signal; gardenerEvaluateExitCondition).
//   - bead_query           — a loop_end bead_query stop matched (e.g. "the root
//     epic and all its children are closed").
//   - specific_bead_closed — a named target bead closed (the goal was "drive
//     THIS bead to done").
//
// Everything else is "ended without (provably) achieving the goal":
// max_runs / wall_clock / consecutive_failures (guard rails), no_progress_for /
// no_actionable_state / yielded_then_woken_then_idle (gave up), manual /
// manual_force / retired (operator pulled the plug). We classify conservatively:
// only the kinds above count as achievement so the loud badge never overstates.
var gardenerGoalAchievementKinds = map[string]bool{
	"exit_condition":       true,
	"bead_query":           true,
	"specific_bead_closed": true,
}

// gardenerKindIsGoalAchieved reports whether a (slot, kind) termination pair
// represents the gardener reaching its terminal goal. Operator-initiated closes
// (slot=="operator", e.g. kind=="retired") are never achievements regardless of
// kind. Goal-completion is only credited on the loop_end slot — a hard_stop is a
// kill, not a finish.
func gardenerKindIsGoalAchieved(slot, kind string) bool {
	if slot == "operator" {
		return false
	}
	if slot != "" && slot != "loop_end" {
		return false
	}
	return gardenerGoalAchievementKinds[kind]
}

// GardenerGoalState is the three-way goal disposition rendered on the index and
// detail pages: "achieved" (the gardener reached its terminal goal — loud
// badge), "unachieved" (closed WITHOUT reaching it — budget/time/retired), or
// "" (still open / running — no disposition yet).
//
// Live re-derive (bug ka-0000007772). When the gardener's goal resolves to a
// single authoritative target bead (TargetEpicID set — the root epic of a
// subtree-draining bead_query, or a specific_bead_closed target) and we have a
// CURRENT status for it (TargetEpicStatus set), the disposition is derived
// LIVE from that status, NOT from the terminated-slot/kind snapshot captured at
// gardener-close:
//
//   - target CLOSED  ⇒ "achieved" — the goal IS reached, regardless of whether
//     the gardener is still open (it may simply not have fired its loop_end yet)
//     or closed with a non-goal kind. Fixes the FALSE-NEGATIVE ("goal not
//     reached" though the epic CLOSED).
//   - target OPEN    ⇒ the goal is NOT reached. The epic's OWN status is
//     authoritative over any subtree count: a malformed (reversed) parent-child
//     edge can make a subtree CTE report 0 open descendants even while the epic
//     is open (this exact data bug wedged ka-0000000398), so we NEVER credit
//     "achieved" while the target epic is itself open. For a still-running
//     gardener an open target yields "" (no disposition yet); for a closed
//     gardener it yields "unachieved". Fixes the FALSE-POSITIVE ("GOAL ACHIEVED"
//     though the epic is OPEN).
//
// Fallback. With no resolvable target (TargetEpicID == "") or no fetched status
// (TargetEpicStatus == "" — pruned / daemon error), we fall back to the
// snapshot path: a closed gardener is "achieved" iff its terminated slot/kind is
// a goal-completion path, else "unachieved"; an open gardener has no
// disposition. populateLiveGoalState fills the target fields; a bare header
// (no fetch) therefore behaves exactly as before this fix.
func (v gardenerView) GardenerGoalState() string {
	if live, ok := gardenerLiveGoalState(v.TargetEpicStatus, v.IsClosed); ok {
		return live
	}
	return gardenerSnapshotGoalState(v.IsClosed, v.TerminatedSlot, v.TerminatedKind)
}

// gardenerLiveGoalState derives the goal disposition from the target bead's
// CURRENT status (bug ka-0000007772). ok is false when no live status is
// available (targetStatus == "") — the caller must then fall back to the
// close-time snapshot. When ok is true:
//
//   - target CLOSED ⇒ "achieved" (the goal IS reached, even if the gardener is
//     still open or closed on a non-goal kind).
//   - target OPEN ⇒ the epic's OWN open status is authoritative (never trust a
//     0-subtree count over an open epic — reversed parent-child edges can zero
//     a subtree CTE while the epic is open, the ka-0000000398 data bug):
//     "unachieved" for a closed gardener, "" (no disposition yet) for a still
//     running one.
//
// Pure (no receiver, no fetch) so both the live header path and the batched
// index enrich path share one authority.
func gardenerLiveGoalState(targetStatus string, gardenerClosed bool) (string, bool) {
	if strings.TrimSpace(targetStatus) == "" {
		return "", false
	}
	if gardenerStatusIsClosed(targetStatus) {
		return "achieved", true
	}
	if gardenerClosed {
		return "unachieved", true
	}
	return "", true
}

// gardenerSnapshotGoalState is the legacy close-time provenance derivation used
// as a fallback when no live target status is available: a closed gardener is
// "achieved" iff its terminated slot/kind is a goal-completion path, else
// "unachieved"; an open gardener has no disposition.
func gardenerSnapshotGoalState(gardenerClosed bool, slot, kind string) string {
	if !gardenerClosed {
		return ""
	}
	if gardenerKindIsGoalAchieved(slot, kind) {
		return "achieved"
	}
	return "unachieved"
}

// gardenerStatusIsClosed reports whether a bead status string represents a
// closed (terminal-done) bead. Mirrors the daemon's active-status set
// (beadsapi: open/in_progress/blocked/deferred are active) — anything outside
// that set, and specifically "closed", is treated as closed. Empty is NOT
// closed (an unfetched/unknown status must never be read as "done").
func gardenerStatusIsClosed(status string) bool {
	switch strings.TrimSpace(status) {
	case "", "open", "in_progress", "blocked", "deferred":
		return false
	default:
		return true
	}
}

// gardenerGoalTargetID extracts the gardener's authoritative goal-target bead id
// from its trigger bead — the single bead whose closure means "goal reached".
// Two shapes, in priority order:
//
//  1. The modern `gardener.stop` block: a loop_end bead_query entry's `parent`
//     (subtree-drain goal → the root epic) or a specific_bead_closed entry's
//     `bead_id` (single-bead goal).
//  2. The legacy `conditions` pre-flight gate (what `gb gardener start <epic>`
//     writes, and what the affected gardeners in bug ka-0000007772 carry): a
//     bead_query condition's `parent`.
//
// Returns "" when no single authoritative target can be resolved (e.g. an
// exit_condition/gate gardener, or a project-wide query with no parent). The
// caller then leaves the live re-derive off and falls back to the snapshot.
// When multiple candidate targets exist we take the first parent/bead_id we
// find — gardeners in practice carry exactly one root-scoped goal entry.
func gardenerGoalTargetID(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}

	// Shape 1: the gardener.stop block.
	def := bridge.ParseTriggerDef(b, "schedule")
	if def.Gardener != nil && len(def.Gardener.Stop) > 0 {
		if spec, err := scheduler.ParseStopSpec(string(def.Gardener.Stop)); err == nil {
			if id := gardenerTargetFromStopSlot(spec.LoopEnd); id != "" {
				return id
			}
		}
	}

	// Shape 2: the legacy conditions pre-flight gate.
	if raw := strings.TrimSpace(b.Fields["conditions"]); raw != "" {
		if conds, err := scheduler.ParseConditions(raw); err == nil {
			for _, c := range conds {
				if c.Type == scheduler.StopKindBeadQuery && strings.TrimSpace(c.Parent) != "" {
					return strings.TrimSpace(c.Parent)
				}
			}
		}
	}
	return ""
}

// gardenerTargetFromStopSlot returns the goal-target bead id from a stop slot's
// entries: the first specific_bead_closed bead_id, else the first bead_query
// parent. Empty when neither is present.
func gardenerTargetFromStopSlot(slot scheduler.StopSlot) string {
	for _, e := range slot.Any {
		if e.Kind == scheduler.StopKindSpecificBeadClosed && strings.TrimSpace(e.BeadID) != "" {
			return strings.TrimSpace(e.BeadID)
		}
	}
	for _, e := range slot.Any {
		if e.Kind == scheduler.StopKindBeadQuery && strings.TrimSpace(e.Parent) != "" {
			return strings.TrimSpace(e.Parent)
		}
	}
	return ""
}

// populateLiveGoalState fills v.TargetEpicID + v.TargetEpicStatus so
// GardenerGoalState can re-derive the goal disposition LIVE from the target
// bead's CURRENT status rather than the close-time snapshot (bug
// ka-0000007772). It resolves the target from the trigger bead, then fetches
// that bead's current status via the daemon.
//
// Best-effort and nil/error-safe: a gardener with no resolvable target leaves
// both fields empty (GardenerGoalState falls back to the snapshot); a fetch
// failure leaves TargetEpicStatus empty (same fallback) so a daemon hiccup can
// never flip a badge to a wrong-but-confident verdict. The single GetBead here
// is per-gardener (the detail page); the index path batches via
// enrichGardenerGoalStates instead to avoid N+1.
func (s *Server) populateLiveGoalState(ctx context.Context, v *gardenerView, b *beadsapi.BeadDetail) {
	target := gardenerGoalTargetID(b)
	if target == "" {
		return
	}
	v.TargetEpicID = target
	tb, err := s.daemon.GetBead(ctx, target)
	if err != nil || tb == nil {
		if err != nil {
			s.logger.Debug("gardener live goal-state: fetch target failed",
				"gardener", b.ID, "target", target, "error", err)
		}
		return
	}
	v.TargetEpicStatus = tb.Status
}

// gardenerTerminationReason renders a short human label for a (slot, kind)
// termination pair, used in the goal-badge tooltip so an operator can see WHY a
// gardener stopped without decoding the raw kind string. Empty when neither
// field is set (a closed gardener predating the provenance fields).
func gardenerTerminationReason(slot, kind string) string {
	switch kind {
	case "exit_condition":
		return "exit condition met"
	case "bead_query":
		return "target bead query satisfied"
	case "specific_bead_closed":
		return "target bead closed"
	case "max_runs":
		return "run budget exhausted"
	case "wall_clock":
		return "wall-clock budget elapsed"
	case "consecutive_failures":
		return "too many consecutive failures"
	case "no_progress_for":
		return "no progress (stalled)"
	case "no_actionable_state":
		return "no actionable state"
	case "yielded_then_woken_then_idle":
		return "yielded then went idle"
	case "manual", "manual_force":
		return "manually stopped"
	case "retired":
		return "retired by operator"
	case "":
		if slot != "" {
			return slot
		}
		return ""
	default:
		// Unknown kind: show it verbatim (prefixed by slot when present) so a
		// future engine kind is still legible rather than silently dropped.
		if slot != "" && slot != "loop_end" {
			return slot + ": " + kind
		}
		return kind
	}
}

// TerminationReason is the template-facing accessor for gardenerTerminationReason.
func (v gardenerView) TerminationReason() string {
	return gardenerTerminationReason(v.TerminatedSlot, v.TerminatedKind)
}

// computeLiveness derives the overdue / silent verdict (bug ka-0000006095 / A1)
// and stamps it on v.Liveness. It combines the fetch-free inputs captured at
// header build (lastFire, cooldownMinutes, Cron/Timezone, the engine run-budget
// counters) with the caller-supplied last_progress_at heartbeat — the freshest
// genuine progress across the gardener's runs, sourced from the iteration /
// last-agent beads the caller already fetched. `now` is passed in so the
// derivation stays testable and clock-controlled. Callers invoke it AFTER the
// schedule strip + spec counters are populated so Cron/MaxRuns/RunsSpawned are
// final.
func (v *gardenerView) computeLiveness(lastProgress time.Time, now time.Time) {
	in := v.livenessInput()
	in.LastProgress = lastProgress
	v.Liveness = deriveGardenerLiveness(in, now)
}

// livenessInput assembles the fetch-free liveness inputs captured on the header
// (LastProgress left zero — the caller supplies it from the run set). Split out
// so the index row can carry the input and re-derive liveness in applyLastRun
// once the last-agent bead's last_progress_at is available.
func (v gardenerView) livenessInput() gardenerLivenessInput {
	return gardenerLivenessInput{
		Enabled:         v.Enabled,
		IsClosed:        v.IsClosed,
		Cron:            v.Cron,
		Timezone:        v.Timezone,
		CooldownMinutes: v.cooldownMinutes,
		RunsSpawned:     v.RunsSpawned,
		MaxRuns:         v.MaxRuns,
		LastFire:        v.lastFire,
	}
}

// prettyRawJSON pretty-prints a raw JSON message for display in a <details>
// block. Empty/nil input yields "" (so the template can collapse the section);
// otherwise it delegates to prettyJSON, which falls through verbatim on a parse
// miss so we never drop the operator's data.
func prettyRawJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return prettyJSON(string(raw))
}
