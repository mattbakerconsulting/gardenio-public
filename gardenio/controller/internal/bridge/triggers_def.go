package bridge

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// TriggerDef is the parsed representation of a trigger bead's fields.
// It is source-agnostic — each TriggerSource populates it from the trigger bead,
// adding source-specific data in the SourceData field.
type TriggerDef struct {
	ID          string
	Title       string
	Source      string // e.g. "jira", "gitlab", "webhook", "bead-state"
	Action      TriggerAction
	Role        string
	Project     string
	Prompt      string
	FormulaID   string
	FormulaVars string
	// CrossPourUpstreamStep, when set, is the bead ID (or a template
	// placeholder like "{{bead_id}}") of an UPSTREAM molecule's emit step. On an
	// apply_formula fire, every entry step of the poured molecule gets a `blocks`
	// edge onto it, so closing the upstream emit step re-drives step 1 via the
	// FormulaStepDispatcher (decision ka-0000007596). Parsed from the
	// `cross_pour_upstream_step` field; empty means no cross-pour chaining.
	CrossPourUpstreamStep string
	MaxConcurrent         int
	CooldownMinutes       int
	Enabled               bool
	LastChecked           time.Time

	// LastFireError is the trigger bead's top-level `last_fire_error` field: the
	// (truncated) error string from the most recent fireAction that raised,
	// cleared on the next successful fire (bug ka-0000002991). It is the data-
	// plane witness for a gardener that MATCHES EVERY CYCLE AND FAILS — a state
	// that otherwise looks byte-identical to idle/gated because a failed fire
	// spawns no agent and so never advances any run counter. Parsed for
	// completeness; the engine writes it via stampFireError and clears it on
	// success.
	LastFireError string

	// SlackChannel is the trigger bead's top-level `slack_channel` field: the
	// Slack channel every agent this trigger spawns should route its card /
	// thread / squawks to, instead of the project's default channel. It is
	// stamped onto each spawned agent bead as `schedule_slack_channel`, which
	// the slack-bridge uses as the agent's spawn channel (bot_agents.go).
	//
	// This is the plain-schedule analogue of GardenerSpec.SlackChannel: a
	// gardener carries the channel inside its `gardener` block, but a bare
	// `action: spawn_agent` schedule (e.g. a recurring daily standup poster)
	// has no gardener block, so it declares the channel at the top level. When
	// both are set the gardener block wins (it is stamped last). Parsed from the
	// `slack_channel` field by ParseTriggerDef; empty means "use the project
	// default channel" (ka-0000003647).
	SlackChannel string

	// MaxConcurrentRuns caps how many agents THIS trigger may have live
	// (spawning/working) at once — a hard single-instance gate distinct from
	// MaxConcurrent, which is a per-evaluation-cycle fire cap (and defaults to 3).
	// It is the fix for two gardeners running concurrently off ONE schedule
	// trigger when a late/catch-up fire spawned a second run while the prior run
	// was still working (bug ka-0000003097). Parsed from the optional
	// `max_concurrent_runs` field.
	//
	// Zero means "unset": a GARDENER trigger (Gardener != nil) then defaults to an
	// effective cap of 1 (gardenerMaxConcurrentRuns) — a gardener loop is a
	// singleton controller, so one live run per trigger is the safe default. A
	// non-gardener trigger with MaxConcurrentRuns==0 is unaffected (it keeps the
	// legacy MaxConcurrent live-lease behavior). A positive value is honored
	// verbatim for both shapes. See gardenerLiveInstanceCheck.
	MaxConcurrentRuns int

	// LeaseStaleAfter bounds how long a single agent may hold this trigger's
	// max_concurrent lease before it stops counting toward the cap (bug
	// ka-0000001095). Parsed from the optional `lease_stale_hours` field; zero
	// means "use defaultLeaseStaleAfter" (3h). See liveSpawnCount /
	// gardenerAgentStillRunning and leaseStaleThreshold.
	LeaseStaleAfter time.Duration

	// CreatedAt is the trigger bead's creation time, copied verbatim from the
	// bead. It is the wall_clock baseline for gardener-shaped triggers: the
	// gardener stop evaluator measures a hard_stop/loop_end wall_clock duration
	// from this instant (scheduler.GardenerStopContext.CreatedAt →
	// StopKindWallClock, floored at the grandfather epoch). Zero means
	// wall_clock is not enforceable. Populated by ParseTriggerDef from
	// bead.CreatedAt. Before this was plumbed the gardener fire path left it
	// zero, so wall_clock was a silent no-op (ka-0000006967).
	CreatedAt time.Time

	// CreatedBy is the identity that created this trigger bead.
	// Used for permission checks and audit trail.
	CreatedBy string

	// OperatedBy is the server-resolved typed operator (slack:U…, oidc:…@…)
	// associated with the trigger bead via the operators table. This is the
	// preferred identity to stamp on daemon writes triggered by this bead,
	// because CreatedBy may be an opaque actor name that the kbeads operator
	// sanitizer rejects. Populated by ParseTriggerDef from bead.OperatedBy.
	OperatedBy string

	// AllowedProjects restricts which projects this trigger can target.
	// Empty means unrestricted. Supports wildcard "*".
	AllowedProjects []string

	// AllowedRoles restricts which agent roles this trigger can spawn.
	// Empty means unrestricted. Supports wildcard "*".
	AllowedRoles []string

	// BlockedBy lists bead IDs (typically type=decision or type=escalation)
	// that pause this trigger while any of them is still open. Parsed from the
	// `blocked_by` field — accepts either a JSON array or a comma-separated
	// string. Empty means the trigger is never blocked.
	BlockedBy []string

	// HasConditions reports whether the bead carries a non-empty `conditions`
	// pre-flight gate (the bare-shape bead_query gate that `gb gardener start`
	// writes). Used by the unbacked-gardener-prose guard to recognise a trigger
	// whose budget/goal claim IS backed by a real gate even without a gardener
	// block. Populated by ParseTriggerDef. See gardener_gate_guard.go.
	HasConditions bool

	// SourceData holds source-specific fields (e.g. JQL for jira, event filter for gitlab).
	// Each TriggerSource defines its own struct here.
	SourceData any

	// Triggers is the optional multi-source fan-in array (Phase 3b/A4). When
	// non-empty, the trigger fires whenever any entry's source matches —
	// dedup is per (trigger, entry index, source match key). When empty,
	// the trigger follows the legacy single-source path keyed by Source.
	// Parsed from the `triggers` JSON-encoded field by ParseTriggerDef.
	//
	// See ka-0000007928 (A4 design) for the firing precedence rules,
	// dedup key shape, and engine-side wiring (deferred to follow-ups).
	Triggers []scheduler.TriggerEntry

	// Gardener is the optional gardener-loop extension (Phase 3b/A3). nil
	// means the trigger is not a gardener — the common case for
	// source-only triggers. Non-nil carries the goal/stop/progress spec
	// plus runtime counters (runs_spawned, last_agent_id, ...).
	// Parsed from the `gardener` JSON-encoded field by ParseTriggerDef.
	//
	// See ka-0000001472 (A3 design) for the full field set, collision
	// check, and migration plan.
	Gardener *GardenerSpec

	// Chain is the optional gardener-chaining declaration (C1, gardener-chaining
	// design §3.1). nil means this gardener hands off to no heir. A non-nil
	// Chain.To describes the heir gardener a predecessor spawns on
	// goal_satisfied. Parsed from the `chain` JSON-encoded field by
	// ParseTriggerDef. This is the orchestration / edge half of the hybrid
	// model — the actual handoff fires agent-side in v0 (the wrap-up nudge),
	// engine-side in v1. See gardenio/docs/gardener-chaining-design-2026-06-18.md.
	Chain *ChainSpec

	// OnViolation selects which non-spawn alert(s) the assert action fires when
	// a predicate is violated (feature F5, ka-0000008157): "notify",
	// "create_bead", or "both". Only meaningful when Action is
	// TriggerActionAssert. Parsed from the `on_violation` field by
	// ParseTriggerDef; empty defaults to "notify".
	OnViolation string

	// BeadType overrides the type of the bead filed by the create_bead action
	// (and the create_bead leg of an assert on_violation). Empty defaults to
	// "task". Validated against createBeadTypeAllowlist by createTaskBead so a
	// trigger cannot file an arbitrary bead type. Parsed from the `bead_type`
	// field by ParseTriggerDef. See ka-0000007307.
	BeadType string

	// BeadPriority is the optional priority stamped on the filed bead. nil
	// means "leave unset" (server default). Parsed from the `bead_priority`
	// field by ParseTriggerDef.
	BeadPriority *int

	// ParentBead, when non-empty, is wired as a parent-child dependency
	// (filed-bead → parent) after the bead is created, so a trigger can file
	// a child of an existing epic. Parsed from the `parent` field by
	// ParseTriggerDef.
	ParentBead string

	// ExtraLabels are additional labels appended to the filed bead beyond the
	// source/trigger/project labels createTaskBead always adds. Parsed from
	// the `extra_labels` field (JSON array or comma-separated) by
	// ParseTriggerDef.
	ExtraLabels []string

	// Labels is the trigger bead's raw label set, copied verbatim from the
	// bead. Used by IsDarkGardener (dark_gardener.go) to identify monorepo-dark
	// gardeners at firing time (the bare `monorepo-dark` label is the live
	// discriminator) for the dark-only blast-radius caps (MR rate + per-agent
	// token budget). Populated by ParseTriggerDef.
	Labels []string

	// ExecutionTarget, when non-empty, is stamped verbatim onto every agent
	// bead this trigger spawns (fields.execution_target — the agent-metadata
	// layer of the controller's global < project < role < agent precedence,
	// resolveExecutionTarget in cmd/controller/podspec.go). This lets one
	// gardener opt its iterations into a non-default runtime backend (e.g.
	// "lambda_microvm") without flipping the whole role or project. An
	// unrecognized value falls back to the Kubernetes pod path controller-side,
	// so a typo can never strand the spawns. Parsed from the trigger bead's
	// `execution_target` field by ParseTriggerDef.
	ExecutionTarget string

	// GardenerTerminatedSlot/Kind and GardenerStuckEscalated mirror the bead's
	// terminate markers (gardener_terminated_slot/kind,
	// gardener_stuck_escalated). The engine only ever stamps these in the same
	// CloseBead call that closes a gardener, and its one sanctioned reopen path
	// (gardenerGrantSecondLife) clears them in the same write that reopens — so
	// an OPEN trigger still carrying any of them can only mean an out-of-band
	// bead-level reopen (an operator re-arm). gardenerRearmFromOperatorReopen
	// keys off that fingerprint to finish the re-arm with the durable reset
	// every engine revive path applies (bug ka-0000004191). Populated by
	// ParseTriggerDef.
	GardenerTerminatedSlot string
	GardenerTerminatedKind string
	GardenerStuckEscalated bool

	// WakeIntent is the operator's natural-language statement of WHEN this
	// trigger should wake — the INTENT, deliberately split from the cron/
	// conditions MECHANISM (Slack thread ka-0000007909). e.g. "Weekdays 9am PT,
	// plus whenever open P0s > 0." Until this field existed, intent and
	// mechanism were the same object (the cron IS the intent), so nothing could
	// detect when the mechanism failed to achieve the intent: a wrong cron
	// window or an always-false condition gate still gets evaluated every cycle,
	// keeps last_checked fresh, looks perfectly alive, and never actually wakes.
	//
	// WakeIntent is NOT consumed by the fire path — the engine never reads it to
	// decide whether to fire. It is read only by the periodic wake-intent
	// reconciler (a haiku agent on its OWN cron, watcher != watched), which
	// compares this intent against the mechanical config plus the deterministic
	// expected-vs-observed fire diff (scheduler.ReconcileFires) and either edits
	// the trigger to match intent, or files a bug bead when the drift is
	// impossible-to-express or habitual. Parsed verbatim from the `wake_intent`
	// field by ParseTriggerDef. Empty means "no declared intent" — the
	// reconciler skips the semantic check and falls back to the mechanical
	// liveness sweep alone.
	WakeIntent string
}

// createBeadTypeAllowlist bounds the bead types a create_bead trigger may
// file. Restricting to a known set prevents a trigger bead from minting
// arbitrary (and possibly type-config-less) bead types. See ka-0000003075.
var createBeadTypeAllowlist = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
	"chore":   true,
}

// GardenerSpec is the gardener-loop extension to TriggerDef. nil means the
// trigger is not a gardener (the common case for source-only triggers). A
// non-nil pointer carries goal/stop/progress + runtime counters.
//
// Phase 3b/A3. See doc ka-0000001472 for the full spec. Stop and Progress
// are kept as raw JSON until B6 (ka-0000001562) wires the engine fire path;
// A5 stores them verbatim so dual-write and `kd show` are correct without
// committing the engine to a specific stop-spec parser.
type GardenerSpec struct {
	// --- Identity ---
	Name string `json:"name,omitempty"`

	// --- Goal + exit ---
	TerminalGoal  string `json:"terminal_goal,omitempty"`
	ExitCondition string `json:"exit_condition,omitempty"`
	OnExit        string `json:"on_exit,omitempty"`

	// Standing marks a PERPETUAL gardener — one whose terminal_goal is a
	// standing mandate ("drive READY beads -> MR -> merge", "main -> golden
	// tag") that never self-declares "done", and which is pinned to NO
	// terminal epic. For such a gardener, exhausting the resume budget on
	// zero-progress runs is NORMAL steady state during a quiet period, NOT a
	// stuck-loop to give up on. gardenerGardenerIsStanding gates the fix for bug
	// ka-0000006241: a standing gardener that spends its resume budget re-arms
	// IN PLACE forever instead of being flagged gardener-stuck-escalate + closed
	// (the give-up heuristic that systematically reaped the release-owner +
	// delivery merge-owner gardeners during quiet periods, going the release
	// lane dark). The terminal hard_stop brakes (consecutive_failures,
	// wall_clock) still close a genuinely broken standing gardener — only the
	// no-progress give-up is suppressed. A gardener is treated as standing when
	// EITHER this field is true OR the trigger bead carries the `perpetual:true`
	// label (the live release-owner + delivery gardeners already carry that
	// label, so the fix is retroactive with no data migration).
	Standing bool `json:"standing,omitempty"`

	// --- Stop + progress (structured JSON; engine in B6 decodes). ---
	Stop     json.RawMessage `json:"stop,omitempty"`
	Progress json.RawMessage `json:"progress,omitempty"`

	// --- Run budget + retry ---
	RunBudget      int    `json:"run_budget,omitempty"`
	MaxRuns        int    `json:"max_runs,omitempty"`
	RetryOnFailure bool   `json:"retry_on_failure,omitempty"`
	MaxRetries     int    `json:"max_retries,omitempty"`
	RunTimeout     string `json:"run_timeout,omitempty"`
	Timeout        string `json:"timeout,omitempty"`

	// --- Resume (terminal_goal master-gate; bead ka-0000005353) ---
	// When a premature stop kind (max_runs / wall_clock / no_progress_for /
	// consecutive_failures) fires on a gardener whose terminal_goal is unmet,
	// the fire path does NOT close it — it RE-ARMS the gardener (refills run
	// budget) up to ResumeBudget fruitless resumes, then escalates to the
	// operator. Any real progress event resets ResumesWithoutProgress to 0,
	// so a gardener making headway resumes indefinitely; only spin is bounded
	// (satisfies advice ka-0000000974).
	//
	// ResumeBudget is the max number of consecutive *fruitless* resumes
	// (resumes that produced no progress) before escalating. 0 means "use
	// the default" (defaultResumeBudget); a negative value disables resume
	// (premature stops close the gardener, the legacy behavior).
	ResumeBudget int `json:"resume_budget,omitempty"`
	// RunBudgetRefill is how much headroom to add to RunBudget on each
	// re-arm. 0 means "use the default" (defaultRunBudgetRefill).
	RunBudgetRefill int `json:"run_budget_refill,omitempty"`

	// --- Second-life budget (coarse post-closure revival; bead ka-0000004010) ---
	// The resume budget above (ResumeBudget / ResumesWithoutProgress) re-arms a
	// gardener that is STILL OPEN. Second-life is the complementary, coarser brake
	// for a gardener that has ALREADY been CLOSED on max_runs without meeting its
	// terminal_goal (the "disappointed gardener" cohort, epic ka-0000001575). The
	// second-life adjudicator (reaper §5b) may grant such a gardener another life:
	// gardenerGrantSecondLife REOPENS the same bead (audit-trail continuity over a
	// clone-fresh), resets RunsSpawned, bumps RunBudget (bounded), and increments
	// SecondLifeCount. MaxSecondLives caps resurrections — this is the mirror of
	// terminal-goals max_respawns and Matt's explicit don't-resurrect-the-doomed
	// guardrail: once SecondLifeCount >= MaxSecondLives the grant escalates to an
	// operator decision instead of reopening.
	//
	// MaxSecondLives is the resurrection cap. 0 means "use the default"
	// (defaultMaxSecondLives); a negative value disables second-life entirely (a
	// max_runs close stays closed, the legacy behavior) — an explicit opt-out for
	// gardeners that genuinely should die when their budget runs out.
	MaxSecondLives int `json:"max_second_lives,omitempty"`
	// SecondLifeCount counts the second lives already granted to this gardener.
	// Engine-owned; carried across reopens so the cap is cumulative over the
	// gardener's whole history, not reset per close.
	SecondLifeCount int `json:"second_life_count,omitempty"`

	// AgentDailyBudgetUSD is the per-agent/day token-spend ceiling (USD,
	// keyed to LiteLLM cost-tracking) for the dark agents this gardener
	// spawns (R6-FU6c, ka-0000001323). While RunBudget bounds the RUN count,
	// this bounds per-agent TOKEN spend: when a single dark agent this
	// gardener spawned crosses this ceiling within the current UTC day, the
	// engine stamps agent_state=budget_exceeded on that agent bead and
	// suppresses the gardener's further spawns. 0 means "use the default"
	// (darkDefaultAgentDailyBudgetUSD); a negative value disables the
	// per-agent budget guard for this gardener. Only enforced for dark
	// gardeners (IsDarkGardener) — non-dark gardeners ignore it.
	AgentDailyBudgetUSD float64 `json:"agent_daily_budget_usd,omitempty"`

	// --- Concurrency ---
	OneSpawnAtATime bool `json:"one_spawn_at_a_time,omitempty"`

	// MaxRunsPerDay caps the runs this gardener spawns in one UTC day (Lands
	// Track B §4.3: a steward is woken by its land's events, so a chatty land
	// must not spawn it without bound). 0 means no daily cap. When the cap is
	// hit the gardener stays open + enabled and does not fire again until the
	// next UTC day; GardenerDailyRunCapHit reports it for the land header.
	MaxRunsPerDay int `json:"max_runs_per_day,omitempty"`
	// RunsDay (YYYY-MM-DD, UTC) and RunsToday count the runs spawned on that
	// day. Engine-owned, and only kept for a gardener with MaxRunsPerDay > 0.
	RunsDay   string `json:"runs_day,omitempty"`
	RunsToday int    `json:"runs_today,omitempty"`

	// --- Runtime / derived (engine-owned, not user-authored) ---
	RunsSpawned         int    `json:"runs_spawned,omitempty"`
	RunsCompleted       int    `json:"runs_completed,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	LastAgentID         string `json:"last_agent_id,omitempty"`
	// LineageRoot is the FIRST iteration agent bead of this gardener's life —
	// the root of the succession chain (persistent-agents Phase 1, task
	// ka-0000006931, design memo §3.1). Engine-owned, carried across fires so
	// every iteration's agent bead can be stamped lineage_root_id = this value
	// (grouping the whole life-thread by indexed lookup + bounding the per-chain
	// continuation budget). Seeded at gardenerPostFire the first fire that
	// records a LastAgentID (root == that agent), then held constant. Distinct
	// from — and additive to — the schedule_prior_agent_id chain, which is left
	// untouched (its 4 readers + golden tests keep passing).
	LineageRoot      string          `json:"lineage_root,omitempty"`
	LastCheckedAgent string          `json:"last_checked_agent,omitempty"`
	RunLog           json.RawMessage `json:"run_log,omitempty"`
	ProgressState    json.RawMessage `json:"progress_state,omitempty"`
	// ResumesWithoutProgress counts consecutive re-arms that produced no
	// progress. Reset to 0 by any credited progress event; compared against
	// ResumeBudget to decide re-arm vs. escalate. Engine-owned.
	ResumesWithoutProgress int `json:"resumes_without_progress,omitempty"`
	// LastResumeReason records the premature stop kind that triggered the
	// most recent re-arm (e.g. "max_runs", "no_progress_for"). Surfaced on
	// the gardener bead for observability (monitoring task ka-0000005415).
	LastResumeReason string `json:"last_resume_reason,omitempty"`
	// LastResumeRunsSpawned snapshots RunsSpawned at the moment of the most
	// recent COUNTED resume (re-arm or escalate). The gardener pre-fire stop
	// gate calls gardenerResumeOrEscalate on EVERY reconcile tick a premature
	// stop is matched (~30s floor), so a sticky premature condition — notably
	// no_progress_for, whose ProgressState a bare re-arm does not change —
	// re-trips it each tick. Gating resume accounting on RunsSpawned having
	// advanced past this snapshot ensures ResumesWithoutProgress increments
	// only when a real run has completed since the last resume, never on a bare
	// reconcile (bug ka-0000002690). Engine-owned.
	LastResumeRunsSpawned int `json:"last_resume_runs_spawned,omitempty"`

	// RunsFinalizedAfter is a durable low-water mark on iteration finalization:
	// gardenerFinalizeRunsFromIterations only credits closed iteration beads
	// created at or after it. It is the fix for the no_progress_for wedge
	// (ka-0000003265): a no_progress_for re-arm resets ProgressState to nil to
	// give the gardener a clean window, but gardenerFinalizeRunsFromIterations
	// rebuilds ProgressState.Runs from the IMMUTABLE closed-iteration history
	// every cycle — so without a baseline the reset is silently undone on the
	// next tick, the same zero-score tail re-matches no_progress_for, and the
	// gardener deadlocks in gardenerResumeOrEscalate's bare-reconcile no-op with
	// last_checked frozen (RunsSpawned never advances, so the guard never
	// releases). Stamping this on the re-arm (and on the stalled-trigger heal)
	// makes the reset stick: finalize skips the pre-baseline zero runs, the
	// gardener fires a genuinely fresh window, and no_progress_for escalation is
	// bounded by ResumesWithoutProgress as designed. Zero value ⇒ no baseline
	// (credit all iterations, the legacy behavior). Engine-owned; always paired
	// with a ProgressState reset.
	RunsFinalizedAfter time.Time `json:"runs_finalized_after,omitzero"`

	// WallClockSince is a durable, per-bead wall_clock baseline OVERRIDE stamped
	// to now() by every REVIVAL path (gardenerRearmFromOperatorReopen,
	// gardenerGrantSecondLife). It is the fix for bug ka-0000006465: the
	// hard_stop wall_clock baseline was max(created_at, grandfather_epoch), and
	// neither term is resettable — created_at is immutable (absent from
	// UpdateBeadRequest) and the epoch is a global constant — so a gardener that
	// hard-stopped on wall_clock could NEVER be revived in place. Reopening it
	// (the runbook's documented "un-retire") re-tripped the same expired window
	// on the next reconcile, producing a reopen→re-close loop that re-stamped
	// the terminate markers. Stamping this to the revive instant advances the
	// baseline (which the scheduler computes as max(WallClockSince, created_at,
	// epoch)) past the expired window, granting a genuinely fresh wall_clock
	// while keeping the gardener's whole run/comment ledger on the SAME bead.
	//
	// Engine-owned (never user-authored); always paired with the other durable
	// reset fields on a revive. Zero ⇒ never revived, so the baseline is
	// unchanged (max(created_at, epoch), the legacy behavior) — no data
	// migration and no effect on gardeners that never hard-stopped on wall_clock.
	WallClockSince time.Time `json:"wall_clock_since,omitzero"`

	// DormantOnEscalation marks a gardener parked because its entire frontier
	// is single-threaded behind an OPEN operator escalation/decision (the
	// gardener provably cannot self-serve). A dormant gardener stays OPEN +
	// enabled so the cron preflight wake-path survives, but its fire is
	// suppressed each cycle while the blocker is unresolved — no spawning, no
	// close, no watchdog reopen-churn (bug ka-0000003082). It auto-resumes the
	// moment the operator clears the blocker. Engine-owned.
	DormantOnEscalation bool `json:"dormant_on_escalation,omitempty"`
	// DormantBlocker is the bead ID of the open operator escalation/decision
	// the gardener is parked behind. Surfaced for the resumer watchdog and the
	// beads-viewer gardener card. Empty when not dormant. Engine-owned.
	//
	// For DormantBlockerKind=="mr_merged" this holds the frontier/parent epic ID
	// whose tracked MRs are unmerged (there is no single blocker bead — the
	// blocker is "MRs not yet merged"), so the wake predicate re-checks that
	// subtree rather than a single bead's status.
	DormantBlocker string `json:"dormant_blocker,omitempty"`
	// DormantBlockerKind discriminates WHY a gardener is dormant, which selects
	// the wake predicate (ka-0000007387). Values:
	//   ""                — legacy operator-escalation dormancy (bug
	//                       ka-0000003082): wakes when NO open escalation/
	//                       decision remains on the frontier. Empty for every
	//                       pre-existing dormant gardener, so behavior is
	//                       unchanged with no data migration.
	//   "operator_wedge"  — Prong 2 proactive dormancy: the entire open frontier
	//                       is single-threaded behind open operator blockers
	//                       (actionable-child count is 0). Wakes when the
	//                       frontier is no longer fully wedged (an actionable
	//                       child appears or the epic closes).
	//   "mr_merged"       — Prong 1 declared block-until-MR-merged. Wakes when
	//                       every tracked MR under DormantBlocker (the parent
	//                       epic) is merged.
	//   "resume_budget_spent" — a plan gardener spent its resume budget with
	//                       terminal_goal unmet (gd-dNxyjFLGRJ). DormantBlocker
	//                       is the plan epic; wakes on plan activity, i.e. when
	//                       DormantActivityDigest no longer matches the plan.
	// Engine-owned; always written alongside DormantOnEscalation/DormantBlocker.
	DormantBlockerKind string `json:"dormant_blocker_kind,omitempty"`
	// DormantActivityDigest fingerprints the plan's open work at park time
	// (gardenerPlanActivityDigest) for the resume_budget_spent kind; empty for
	// every other kind and cleared on wake. Engine-owned.
	DormantActivityDigest string `json:"dormant_activity_digest,omitempty"`
	// PlanProgressDigest fingerprints the plan (gardenerPlanProgressDigest)
	// at the last counted resume. When the next premature stop finds it
	// changed, the plan moved, which is progress whatever the runs
	// self-reported: the resume does not count (gd-WE8g4FFEHm). Empty until
	// the first counted resume of a plan gardener. Engine-owned.
	PlanProgressDigest string `json:"plan_progress_digest,omitempty"`
	// DormantSince is when the gardener was parked; cleared on wake. The daily
	// dormant-gardener sweep (gardener_dormant_sweep.go, gd-kkrCdlf4iO) reads it
	// to find gardeners parked for over a day. Zero on a gardener parked before
	// this field existed; the sweep then falls back to last_run. Engine-owned.
	DormantSince time.Time `json:"dormant_since,omitzero"`

	// --- Operator overrides ---
	ForceClose string `json:"force_close,omitempty"`
	AssignBead string `json:"assign_bead,omitempty"`

	// --- Slack / operator binding ---
	SlackChannel string `json:"slack_channel,omitempty"`
	SlackUserID  string `json:"slack_user_id,omitempty"`
	UserBeadID   string `json:"user_bead_id,omitempty"`

	// --- DEPRECATED legacy watchers (parsed for migration; emit lint
	//     warnings rather than dropping the value). ---
	WatchProject string `json:"watch_project,omitempty"`
	WatchRole    string `json:"watch_role,omitempty"`
	WatchBeadID  string `json:"watch_bead_id,omitempty"`
	WatchLabels  string `json:"watch_labels,omitempty"`
	WatchTypes   string `json:"watch_types,omitempty"`
	WatchChannel string `json:"watch_channel,omitempty"`
	WatchFrom    string `json:"watch_from,omitempty"`
	TriggerMode  string `json:"trigger_mode,omitempty"`
}

// ChainSpec is the gardener-chaining declaration carried on a gardener
// (trigger) bead's `chain` JSON field (C1, gardener-chaining design §3.1).
// It is the orchestration / edge half of the hybrid chaining model: the
// declaration of WHO the heir is and WHEN the baton passes. The actual
// handoff is choreographed on a real event — agent-side in v0 (the wrap-up
// nudge creates the heir before `gb stop`), engine-side in v1.
//
// ChainSpec generalizes the single-heir `successor` block from the gardener
// succession plan (gardener-resume-observability-and-succession-plan.md §2a)
// into a named, multi-link, graph-visible relationship. `chain.to` is
// `successor` renamed; the two are NOT competing schemas — see the design
// doc §7.1 reconciliation note. A `succeeds: <predecessor>` typed mention
// (model.DepChainsTo, inverse verb) is stamped on the heir at creation for
// provenance, exactly as the succession plan specifies.
type ChainSpec struct {
	// To describes the heir gardener to spawn. Required for a meaningful
	// chain; a ChainSpec with a nil/empty To is a no-op (validated away by
	// ParseTriggerDef, which leaves TriggerDef.Chain nil in that case).
	//
	// FAN-OUT (C3, gardener-chaining design §3.3): `chain.to` may be EITHER a
	// single heir object (`{"to":{…}}`, the linear-pipeline case) OR an ARRAY
	// of heir objects (`{"to":[{…},{…}]}`, the fan-out case where one
	// predecessor's goal arms N heirs — one per sub-epic). The custom
	// UnmarshalJSON accepts both surface forms; MarshalJSON re-emits the array
	// form only when there is more than one heir, so a single-heir chain keeps
	// its exact object shape for full back-compat with C1/C2/C4. `To` always
	// points at the FIRST heir so every existing single-heir consumer reads it
	// unchanged; the full set is `Heirs()`.
	To *ChainTo `json:"to,omitempty"`

	// Extra carries the second-and-later heirs of a fan-out chain. It is
	// populated only by the array surface form and is nil for a single-heir
	// chain. Consumers should read the whole set via Heirs() rather than
	// touching Extra directly. Not JSON-tagged for the To key — the custom
	// (Un)MarshalJSON handles the object-or-array serialization of To+Extra.
	Extra []*ChainTo `json:"-"`

	// On is the predecessor-side event that arms the handoff. Default
	// "goal_satisfied" — the heir is created when the predecessor declares
	// its terminal_goal met. This mirrors the succession plan's `when` field;
	// "goal_satisfied" is the only value wired in v0.
	On string `json:"on,omitempty"`

	// Mode selects how the heir is materialized. Default "spawn" — create +
	// enable the heir gardener now (the v0 agent-side path). "arm" would
	// create a dormant successor that wakes on its own preflight; reserved
	// for v1. Unknown values are normalized to "spawn" by ParseTriggerDef.
	Mode string `json:"mode,omitempty"`
}

// chainSpecAlias is ChainSpec without the custom (Un)MarshalJSON, used to
// serialize every field EXCEPT `to` (which the custom codec owns so it can
// switch between the object and array surface forms). Keeping On/Mode on the
// alias means adding a ChainSpec field never silently drops it from the wire.
type chainSpecAlias struct {
	On   string `json:"on,omitempty"`
	Mode string `json:"mode,omitempty"`
}

// UnmarshalJSON decodes the `chain` block, accepting `to` as either a single
// heir object or an array of heir objects (fan-out, C3). The first heir lands
// in To; any remainder in Extra. A `to` that is absent, null, or an empty
// array leaves To nil (a no-op chain, dropped by ParseTriggerDef).
func (c *ChainSpec) UnmarshalJSON(data []byte) error {
	// Decode the non-`to` fields via the alias so On/Mode round-trip.
	var base chainSpecAlias
	if err := json.Unmarshal(data, &base); err != nil {
		return err
	}
	c.On = base.On
	c.Mode = base.Mode
	c.To = nil
	c.Extra = nil

	// Pull out the raw `to` value and branch on its JSON shape.
	var probe struct {
		To json.RawMessage `json:"to"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	raw := bytes.TrimSpace(probe.To)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '[' {
		var arr []*ChainTo
		if err := json.Unmarshal(raw, &arr); err != nil {
			return err
		}
		heirs := make([]*ChainTo, 0, len(arr))
		for _, h := range arr {
			if h != nil {
				heirs = append(heirs, h)
			}
		}
		if len(heirs) > 0 {
			c.To = heirs[0]
			if len(heirs) > 1 {
				c.Extra = heirs[1:]
			}
		}
		return nil
	}
	var one ChainTo
	if err := json.Unmarshal(raw, &one); err != nil {
		return err
	}
	c.To = &one
	return nil
}

// MarshalJSON re-emits the chain block. `to` is written as a bare object when
// there is a single heir (byte-identical to the C1 shape) and as an array only
// when the chain fans out to more than one, so round-tripping a single-heir
// chain never rewrites its surface form.
func (c ChainSpec) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	if c.On != "" {
		out["on"] = c.On
	}
	if c.Mode != "" {
		out["mode"] = c.Mode
	}
	heirs := c.Heirs()
	switch {
	case len(heirs) == 1:
		out["to"] = heirs[0]
	case len(heirs) > 1:
		out["to"] = heirs
	}
	return json.Marshal(out)
}

// Heirs returns every heir the chain declares (To first, then Extra), skipping
// nils. A single-heir chain returns a one-element slice; a no-op chain returns
// nil. This is the canonical accessor — fan-out-aware consumers iterate it
// instead of touching To/Extra directly.
func (c *ChainSpec) Heirs() []*ChainTo {
	if c == nil || c.To == nil {
		return nil
	}
	out := make([]*ChainTo, 0, 1+len(c.Extra))
	out = append(out, c.To)
	for _, h := range c.Extra {
		if h != nil {
			out = append(out, h)
		}
	}
	return out
}

// ChainTo describes the heir gardener a predecessor hands off to (the `to`
// sub-object of `chain`). Field semantics follow gardener-chaining design
// §3.1 and reconcile 1:1 with the succession plan's `successor` block §2a.
type ChainTo struct {
	// Phase is a human label for the next phase (e.g. "impl", "verify").
	Phase string `json:"phase,omitempty"`

	// TerminalGoal is the heir gardener's terminal goal. Supports the same
	// {{epic}} / {{same}} template tokens the design doc uses; expansion is
	// the heir-creator's (agent's) responsibility in v0.
	TerminalGoal string `json:"terminal_goal,omitempty"`

	// PromptAdvice is the advice bead ID (gd-…) carrying the heir gardener's
	// prompt, so the heir is authored from a versioned prompt rather than an
	// inlined blob.
	PromptAdvice string `json:"prompt_advice,omitempty"`

	// Epic is the epic the heir gardener drives. Empty or the literal
	// "{{same}}" means inherit the predecessor's epic (the default).
	Epic string `json:"epic,omitempty"`

	// Inherit lists the gardener-block / binding fields the heir copies from
	// the predecessor (e.g. "project", "slack_channel", "operated_by",
	// "slack_thread"). Empty applies the design default inherit set.
	Inherit []string `json:"inherit,omitempty"`

	// Role is the heir bead's role. Empty defaults to "gardener".
	Role string `json:"role,omitempty"`

	// JoinParent expresses a fan-IN / JOIN (C3, gardener-chaining design §3.3):
	// the umbrella epic whose subtree must fully drain before this heir may
	// fire. When set, the heir's preflight bead_query becomes a "==0 open under
	// <JoinParent>" gate INSTEAD of the default ">0 open under my own epic"
	// gate, so a "ship" heir waits for ALL of {impl-A, impl-B, impl-C} to close.
	// Empty keeps the default own-epic preflight. Joins need no new primitive —
	// they are just a different preflight parent (design §3.3).
	JoinParent string `json:"join_parent,omitempty"`

	// BranchDecisionKey expresses a conditional BRANCH (C3, design §3.3): the
	// decision bead id whose resolution wakes this heir. When set, the heir is
	// authored with a decision_resolved trigger keyed on this decision instead
	// of firing on the predecessor's plain goal handoff, so a research gardener
	// that closes with a `gb decision` routes to branch-A or branch-B by the
	// matching decision key. Empty keeps the default goal-handoff arming.
	BranchDecisionKey string `json:"branch_decision_key,omitempty"`
}

// defaultChainInherit is the inherit set applied when ChainTo.Inherit is
// empty (gardener-chaining design §3.1). These are the bindings an heir
// gardener almost always needs to keep the workflow coherent.
//
// thread_id carries the wish (thread head) ka-id so an heir chained from a
// predecessor bound to a wish stays bound to that same wish (ka-0000006315).
// "slack_thread" is a legacy alias kept for back-compat — it maps to no real
// bead field (the live thread binding lives on slack_thread_channel/ts and the
// wish pointer on thread_id), so it carries nothing on its own.
var defaultChainInherit = []string{"project", "slack_channel", "operated_by", "thread_id", "slack_thread"}

// chainToHasHeir reports whether a single heir names something meaningful — at
// least one of the identity-bearing fields set. A bare `{}` heir is treated as
// "no heir".
func chainToHasHeir(t *ChainTo) bool {
	if t == nil {
		return false
	}
	return strings.TrimSpace(t.Phase) != "" ||
		strings.TrimSpace(t.TerminalGoal) != "" ||
		strings.TrimSpace(t.PromptAdvice) != "" ||
		strings.TrimSpace(t.Epic) != ""
}

// hasHeir reports whether the chain names at least one meaningful heir. A bare
// `{}` / `{"to":{}}` / `{"to":[]}` chain block is treated as "no chain" so
// consumers can rely on TriggerDef.Chain != nil meaning "this gardener really
// hands off". For a fan-out chain, ANY meaningful heir keeps the chain.
func (c *ChainSpec) hasHeir() bool {
	if c == nil {
		return false
	}
	for _, h := range c.Heirs() {
		if chainToHasHeir(h) {
			return true
		}
	}
	return false
}

// Normalize fills in ChainSpec defaults (On, Mode, and per-heir Role/Inherit)
// so downstream consumers — the wrap-up nudge composer, the cycle guard, and
// the engine spawn loop — see a fully-populated spec regardless of how terse
// the author was. It normalizes EVERY heir (fan-out aware). It is idempotent.
// ParseTriggerDef calls it after decode.
func (c *ChainSpec) Normalize() {
	if c == nil {
		return
	}
	if strings.TrimSpace(c.On) == "" {
		c.On = "goal_satisfied"
	}
	switch strings.ToLower(strings.TrimSpace(c.Mode)) {
	case "arm":
		c.Mode = "arm"
	default:
		c.Mode = "spawn"
	}
	for _, h := range c.Heirs() {
		if strings.TrimSpace(h.Role) == "" {
			h.Role = "gardener"
		}
		if len(h.Inherit) == 0 {
			h.Inherit = append([]string(nil), defaultChainInherit...)
		}
	}
}

// HasNonEmptyTriggers reports whether the given `triggers` field value parses to
// a non-empty triggers[] array — i.e. the bead is a MULTI-trigger (event/cron
// fan-in) bead, not a bare single-cron schedule. It uses the same parser the
// engine uses at fire time (scheduler.ParseTriggers via ParseTriggerDef), so the
// authoring-time label decision matches runtime dispatch exactly.
//
// Mislabel guardrail (bug ka-0000001688): a `trigger:schedule` bead only fires
// its flat `cron` field — its triggers[] array is the exclusive domain of the
// trigger:multi source (trigger_source_multi.go) and is silently dropped by the
// schedule source (trigger_source_schedule.go). A gardener authored with an empty
// cron AND a non-empty triggers[] but labelled trigger:schedule therefore never
// fires its event wakes. TriggerSourceLabelForFields promotes such a bead to
// trigger:multi at the single label-assembly chokepoint so the class cannot
// recur.
func HasNonEmptyTriggers(triggersField string) bool {
	if strings.TrimSpace(triggersField) == "" {
		return false
	}
	entries, err := scheduler.ParseTriggers(triggersField)
	return err == nil && len(entries) > 0
}

// TriggerSourceLabelForFields returns the trigger:* source label a trigger bead
// should carry given its source label and field map. It promotes a bare
// trigger:schedule label to trigger:multi when the bead's `triggers` field holds
// a non-empty array (HasNonEmptyTriggers). Every other source label (and a
// schedule bead with no triggers[]) is returned unchanged, so the helper is a
// safe no-op for the common single-cron case.
//
// The `triggers` field is read via the same string accessor the kbeads HTTP API
// stores it under; callers pass either a map[string]any (CLI authoring) or a
// map[string]string (engine heir spawn) flattened to a string.
func TriggerSourceLabelForFields(sourceLabel, triggersField string) string {
	if sourceLabel == "trigger:schedule" && HasNonEmptyTriggers(triggersField) {
		return "trigger:multi"
	}
	return sourceLabel
}

// ParseTriggerDef parses the common fields from a trigger bead into a TriggerDef.
// Source-specific fields should be parsed by the TriggerSource implementation.
func ParseTriggerDef(bead *beadsapi.BeadDetail, source string) TriggerDef {
	t := TriggerDef{
		ID:                    bead.ID,
		Title:                 bead.Title,
		Source:                source,
		Action:                TriggerAction(bead.Fields["action"]),
		Role:                  bead.Fields["role"],
		Project:               bead.Fields["project"],
		Prompt:                bead.Fields["prompt"],
		FormulaID:             bead.Fields["formula_id"],
		FormulaVars:           bead.Fields["formula_vars"],
		CrossPourUpstreamStep: strings.TrimSpace(bead.Fields["cross_pour_upstream_step"]),
		MaxConcurrent:         triggerParseInt(bead.Fields["max_concurrent"], 3),
		MaxConcurrentRuns:     triggerParseInt(bead.Fields["max_concurrent_runs"], 0),
		CooldownMinutes:       triggerParseInt(bead.Fields["cooldown_minutes"], 5),
		Enabled:               bead.Fields["enabled"] != "false",
		CreatedAt:             bead.CreatedAt,
		CreatedBy:             bead.CreatedBy,
		OperatedBy:            bead.OperatedBy,
		AllowedProjects:       parseStringList(bead.Fields["allowed_projects"]),
		AllowedRoles:          parseStringList(bead.Fields["allowed_roles"]),
		BlockedBy:             parseStringList(bead.Fields["blocked_by"]),
		HasConditions:         strings.TrimSpace(bead.Fields["conditions"]) != "",
		OnViolation:           strings.TrimSpace(bead.Fields["on_violation"]),
		BeadType:              strings.TrimSpace(bead.Fields["bead_type"]),
		ParentBead:            strings.TrimSpace(bead.Fields["parent"]),
		ExtraLabels:           parseStringList(bead.Fields["extra_labels"]),
		ExecutionTarget:       strings.TrimSpace(bead.Fields["execution_target"]),
		SlackChannel:          strings.TrimSpace(bead.Fields["slack_channel"]),
		Labels:                bead.Labels,

		GardenerTerminatedSlot: strings.TrimSpace(bead.Fields["gardener_terminated_slot"]),
		GardenerTerminatedKind: strings.TrimSpace(bead.Fields["gardener_terminated_kind"]),
		GardenerStuckEscalated: bead.Fields["gardener_stuck_escalated"] == "true",

		LastFireError: strings.TrimSpace(bead.Fields["last_fire_error"]),

		WakeIntent: strings.TrimSpace(bead.Fields["wake_intent"]),
	}
	if raw := strings.TrimSpace(bead.Fields["bead_priority"]); raw != "" {
		if p, err := strconv.Atoi(raw); err == nil {
			t.BeadPriority = &p
		}
	}
	if bead.Fields["last_checked"] != "" {
		t.LastChecked, _ = time.Parse(time.RFC3339, bead.Fields["last_checked"])
	}
	// Optional per-trigger lease-staleness override (bug ka-0000001095). Zero
	// (empty/non-numeric/<=0/non-finite) leaves LeaseStaleAfter unset so
	// leaseStaleThreshold falls back to the 3h default. Mirrors
	// gardener-autoblock-audit.py's --lease-stale-hours knob.
	t.LeaseStaleAfter = leaseStaleHoursField(bead.Fields)
	if t.Action == "" {
		t.Action = TriggerActionSpawnAgent
	}

	// A4: parse optional triggers[] array for multi-source fan-in. On parse
	// error or empty, fall back to the legacy single-source path — don't
	// drop the bead. Engine wiring for the multi-source path is deferred to
	// a follow-up bead.
	if raw := strings.TrimSpace(bead.Fields["triggers"]); raw != "" {
		if entries, err := scheduler.ParseTriggers(raw); err == nil && len(entries) > 0 {
			t.Triggers = entries
		}
	}

	// A3: parse optional gardener block. Empty / invalid JSON yields a nil
	// pointer (non-gardener trigger). The engine-side gardener fire path
	// lands in B6 (ka-0000001562); A5 only persists + exposes the spec.
	if raw := strings.TrimSpace(bead.Fields["gardener"]); raw != "" {
		var cap GardenerSpec
		if err := json.Unmarshal([]byte(raw), &cap); err == nil {
			t.Gardener = &cap
		}
	}

	// C1: parse the optional gardener-chaining declaration. A `chain` block
	// with no meaningful heir (`to` absent / empty) is dropped to nil so
	// downstream consumers can treat "has a chain" as "has a real heir". On
	// JSON error we leave Chain nil rather than dropping the whole trigger.
	if raw := strings.TrimSpace(bead.Fields["chain"]); raw != "" {
		var ch ChainSpec
		if err := json.Unmarshal([]byte(raw), &ch); err == nil && ch.hasHeir() {
			ch.Normalize()
			t.Chain = &ch
		}
	}

	return t
}

// triggerParseInt parses a string as an integer, returning defVal if empty or invalid.
func triggerParseInt(s string, defVal int) int {
	if s == "" {
		return defVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return defVal
	}
	return v
}
