// Package bridge provides the shared trigger evaluation framework.
//
// The trigger framework allows different event sources (JIRA, GitLab, webhooks,
// bead-state changes, etc.) to define trigger beads that fire actions when their
// conditions are met. Each source implements TriggerSource to provide its own
// evaluation logic, while the shared engine handles dedup, cooldown,
// max_concurrent guards, action dispatch, and template expansion.
package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/pausegate"
	"gardenio/controller/internal/spawnguard"
)

// TriggerAction is the action to take when a trigger fires.
type TriggerAction string

const (
	TriggerActionSpawnAgent   TriggerAction = "spawn_agent"
	TriggerActionApplyFormula TriggerAction = "apply_formula"
	TriggerActionNotify       TriggerAction = "notify"
	TriggerActionCreateBead   TriggerAction = "create_bead"
	// TriggerActionAssert is the assert action (feature F5, ka-0000008157): it
	// fires an alert BECAUSE a predicate was violated, rather than firing on a
	// positive match the way notify/create_bead do. The bead-state source emits
	// a match for an assert trigger only when its `assert` invariant is violated
	// (any clause FALSE, or fail-closed on a compile/eval error); this action
	// then fans out to notify and/or create_bead per the trigger's `on_violation`
	// modifier. Like notify/create_bead it spawns no agent and bypasses the
	// SpawnGuard.
	TriggerActionAssert TriggerAction = "assert"
	// TriggerActionWakePROwner tells the agent that owns a conflicting PR
	// (its task's live crew, else its plan's gardener) what conflicts and how
	// to fix it, and comments the same on the task (trigger_pr_conflict_wake.go,
	// gd-xTJWewKG71).
	TriggerActionWakePROwner TriggerAction = "wake_pr_owner"
)

// triggerCooldown is the floor for time between evaluating the same trigger.
// Per-trigger cooldown_minutes (TriggerDef.CooldownMinutes) raises this floor.
const triggerCooldown = 30 * time.Second

// defaultLeaseStaleAfter bounds how long a single agent bead may hold a
// trigger's max_concurrent lease before it stops counting against the cap. A
// spawn-producing trigger enforces max_concurrent as a LIVE lease seeded from
// liveSpawnCount (ka-0000003120); without an age bound, ONE agent that never
// reaches a terminal state holds a max_concurrent=1 lane's only lease forever,
// so every subsequent cron fire defers ("max_concurrent reached") while
// last_checked keeps reading fresh — the lane goes dark yet looks healthy to
// every liveness/dark-gardener check (bug ka-0000001095: gardener-delivery lost
// ~23 fires over 7h49m, gardener-bugfix ~18 over 9h07m; both ended only when an
// operator noticed).
//
// 3h is chosen from the measured distribution: legitimate long trigger-spawned
// runs cluster at 106-124m, while the two confirmed prod outages ran 469m and
// 547m — 3h separates them cleanly with margin on both sides. It mirrors the
// gardener-autoblock-audit.py detector's `--lease-stale-hours` default (3.0), so
// the engine's release threshold and the hygiene sweep's alert threshold agree.
// Per-trigger override via the `lease_stale_hours` field (0/empty = default).
//
// Staleness is measured on agent updated_at, the only available proxy (there is
// no agent heartbeat field). A genuinely-working-but-quiet agent past the
// threshold could be double-spawned, but that is bounded: max_concurrent still
// caps fresh spawns, and the threshold sits well above the real long-run
// cluster. A zero/absent updated_at is treated as NOT stale so a fresh fixture
// or a not-yet-stamped bead can never be released (fail toward keeping the
// lease held).
const defaultLeaseStaleAfter = 3 * time.Hour

// Inner-key eviction horizon and sweep cadence for the TriggerEngine.seen dedup
// sets (ka-0000003049). pruneStaleTriggers only drops whole triggers that no
// longer load; the INNER map[dedupKey]bool was never aged, so a long-lived
// broad bead-state trigger accumulated one permanent entry per distinct
// bead-update transition it ever observed — a per-field-change firehose that
// OOMs the single-replica engine.
const (
	// triggerSeenKeyTTL is how long a dedup key is retained in the in-memory
	// seen set after the GC sweep first observes it. It must be comfortably
	// longer than any legitimate re-fire window: an evicted-then-re-observed
	// key would re-fire, but that re-fire is caught by the pre-fire CatchUpKeys
	// refresh (evalOne, ka-0000000151) against persisted daemon-side state, so
	// eviction is safe — it degrades to at most a redundant daemon round-trip,
	// never a duplicate action. 24h is far beyond the sub-minute SSE/poll
	// cadence these keys dedup against, matching the sibling seenSet TTL
	// (ka-0000008059).
	triggerSeenKeyTTL = 24 * time.Hour
	// triggerSeenGCInterval throttles the sweep so the O(total keys) walk runs
	// at most once per interval rather than every Evaluate() cycle (which polls
	// every ~30s).
	triggerSeenGCInterval = 5 * time.Minute
	// triggerSeenKeyMaxPerTrigger is a hard per-trigger safety cap enforced at
	// sweep time: if a single trigger's inner set exceeds it, the oldest
	// observed keys are evicted down to the cap even before they reach the TTL,
	// bounding worst-case memory between sweeps for a pathological firehose.
	triggerSeenKeyMaxPerTrigger = 50_000
)

// leaseAgeCeilingFactor multiplies the (per-trigger or default) lease-stale
// threshold to get a SECOND, churn-immune lease bound keyed on the agent's
// created_at instead of its updated_at (bug ka-0000002023).
//
// The updated_at bound (leaseHolderStale) releases a lease holder that has gone
// QUIET. But a LIVELOCKED agent is not quiet: it re-boots its prompt on a tight
// self-loop (gardener-bugfix-dispatch-jd5f re-ran boot→prompt→idle every ~10.5m
// for 15h), and SyncAll bumps updated_at every cycle it is non-idle
// (ka-0000003615), so updated_at never ages past the threshold and the
// updated_at bound never fires. The lane stayed dark 15h with 23 open bugs while
// last_checked read fresh — invisible to every updated_at-based check.
//
// created_at cannot be refreshed by churn: the trigger mints the agent bead once
// at spawn, so age-from-created_at grows monotonically regardless of how busy
// the livelock keeps the pod. This is the exact churn-immunity argument the
// reap-side runtime ceiling makes (ka-0000000716, keyed on created_at not
// updated_at); this is its NON-DESTRUCTIVE lease-side twin — it only stops the
// holder counting toward max_concurrent so the lane resumes firing; it never
// touches the wedged agent bead or its pod. The reap side (or the immediate
// operator `gb agent kill`) ends the wedged pod itself.
//
// The factor is 2× the release threshold (6h at the 3h default) so the ceiling
// only releases what is provably long past a legitimate run — the legitimate
// long-run cluster is 106–124m, and 6h sits 3× above it with margin — mirroring
// the reap-side ceiling's 2×-the-release-threshold choice. A zero/absent
// created_at is treated as NOT over the ceiling (fail toward keeping the lease
// held, symmetric with the updated_at bound).
const leaseAgeCeilingFactor = 2

// effectiveCooldown returns the cooldown to apply for a given trigger:
// max(triggerCooldown, CooldownMinutes minutes). Triggers without an explicit
// cooldown still observe the 30s floor.
func effectiveCooldown(trig TriggerDef) time.Duration {
	return max(triggerCooldown, time.Duration(trig.CooldownMinutes)*time.Minute)
}

// ctxWithTriggerOperator stamps the operated_by header for daemon writes made
// during a trigger fire. Triggers fire autonomously, so the right operator is
// the human who created the trigger when one exists. Preference order:
//  1. Existing operator already on context (do not override).
//  2. trig.OperatedBy — server-resolved typed operator from the trigger bead's
//     operated_by FK (populated by ParseTriggerDef from bead.OperatedBy).
//     This is the canonical typed identity even when the bead's CreatedBy
//     is an opaque actor like "e2e-rwx".
//  3. trig.CreatedBy when typed (`slack:`, `oidc:`, `jira:`, `gitlab:` …).
//
// When no typed human operator is available, ctx is returned unchanged so the
// caller's fleet-token identity flows through audit.go's auto-attribution
// (audit.go fleet branch — ka-0000005187 / decision ka-0000001054). The
// previous `system:<source>-trigger` sentinel was rejected by the kbeads
// operator sanitizer (ka-0000002601) and broke jira-bridge spawn_agent fires.
func ctxWithTriggerOperator(ctx context.Context, trig TriggerDef) context.Context {
	if op := beadsapi.OperatorFromContext(ctx); op != "" {
		return ctx
	}
	if isTypedOperator(trig.OperatedBy) {
		return beadsapi.WithOperator(ctx, trig.OperatedBy)
	}
	if isTypedOperator(trig.CreatedBy) {
		return beadsapi.WithOperator(ctx, trig.CreatedBy)
	}
	return ctx
}

// ctxWithTriggerOperatorOrDefault runs ctxWithTriggerOperator and then, when
// the resolved operator is still NOT a human (the trigger bead's author is a
// sentinel/agent, or it resolved to a typed-but-non-human id like
// jira:<accountId>), falls back to the engine's configured DefaultOperator.
//
// This closes the gap the @-mention JiraAgentResolver already covers but the
// JQL/bead/schedule fireAction path did not: a trigger authored by a sentinel
// (e.g. operated_by=system:legacy-pre-attribution) spawned agents with no
// walkable human, so the pod booted with BEADS_OPERATED_BY unset and every
// kbeads write fail-closed with HTTP 400 "operator could not be resolved"
// (ka-0000006023). When e.defaultOperator is "" (the env knob unset) this is
// exactly ctxWithTriggerOperator — zero behavior change.
//
// The fallback fires on "not human", not merely "empty": a typed sentinel from
// trig.OperatedBy IS placed on ctx by ctxWithTriggerOperator, so an empty-only
// check would never reach the default for the very case that motivated it.
// e.defaultOperator is validated human-shaped at construction, so it is always
// a value the daemon accepts.
func (e *TriggerEngine) ctxWithTriggerOperatorOrDefault(ctx context.Context, trig TriggerDef) context.Context {
	ctx = ctxWithTriggerOperator(ctx, trig)
	if e == nil || e.defaultOperator == "" {
		return ctx
	}
	if IsHumanOperator(beadsapi.OperatorFromContext(ctx)) {
		return ctx
	}
	return beadsapi.WithOperator(ctx, e.defaultOperator)
}

// isTypedOperator reports whether s looks like a typed operator id of the
// form "<provider>:<value>" (slack, jira, gitlab, oidc, system, etc.).
func isTypedOperator(s string) bool {
	if s == "" {
		return false
	}
	idx := strings.Index(s, ":")
	return idx > 0 && idx < len(s)-1
}

// TriggerBeadClient is the subset of beadsapi.Client needed by the trigger engine.
type TriggerBeadClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// UpdateBeadFieldsTyped patches fields with their proper JSON types. Required
	// so gardenerPostFire can mirror the INTEGER-typed run counters as numbers;
	// see the gardenerDaemon interface (gardener_evaluator.go) for the bug this
	// prevents (ka-0000007976).
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
	// GetBead is used by the blocked_by gate to look up referenced
	// decision/escalation beads and check whether they are still open.
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	// AddDependency writes a typed edge from beadID → dependsOnID. The apply_formula
	// trigger action uses this to stamp `triggered-by` lineage from the seed bead
	// onto the trigger and source beads (epic ka-0000007507).
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	// AddComment writes a comment on a bead. Used by the gardener stop path
	// (Phase 3b/B6) to stamp a structured "terminated: <slot>:<kind>" line
	// on the trigger bead before closing it.
	AddComment(ctx context.Context, beadID, author, text string) error
	// AddLabel stamps a real label on a bead via POST /v1/beads/{id}/labels.
	// The gardener escalate paths use it for gardener-stuck-escalate — the
	// resumer watchdog's enumeration handle, which must be a genuine label
	// (fires beads.label.added; queryable via kd list --label), NOT a
	// "labels" key smuggled through the custom-fields PATCH, which the
	// closed type:trigger schema rejects (bug ka-0000007275).
	AddLabel(ctx context.Context, beadID, label string) error
	// CloseBead closes a bead with optional final field updates. Used by the
	// gardener stop path to auto-close a trigger bead whose stop spec fired.
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	// UpdateBead patches mutable bead attributes (status, assignee, ...). Used
	// by the schedule source's wedged-trigger self-heal (ka-0000006681) to
	// release a gardener trigger that an erroneous claim left stuck in_progress
	// back to open so the scheduler resumes evaluating it.
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	// MRBeadForTask walks task → merge-request via the `produces` dep edge.
	// Required by the gardener progress evaluator (Phase 3b/B7-c2) so the
	// `mr_merged` event kind sources its truth from the MR bead rather than
	// the legacy task-bead mr_* fields. The production *beadsapi.Client
	// implements it; mocks stub it (typically returning nil, nil).
	MRBeadForTask(ctx context.Context, taskBeadID string) (*beadsapi.BeadDetail, error)

	// Dependency-graph reads used by the pouring-but-never-worked stall sweep
	// (ka-0000000957, gardener_pouring_stall.go) to find an apply_formula
	// trigger's poured molecules (its `triggered-by` dependents) and classify
	// each open molecule's step subtree. All three are pure reads implemented
	// by the production *beadsapi.Client; mocks stub them (the sweep degrades to
	// a no-op if a daemon somehow lacks them).
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// triggersPauseTitle is the well-known title for the gate bead that pauses ALL
// triggers across every project. When an open gate bead with this title exists,
// the trigger engine skips evaluation for every trigger.
const triggersPauseTitle = "triggers-pause"

// schedulerPauseTitleForTriggers is the gate title that silences the cron
// firing of schedule-flavored trigger sources (trigger:schedule and
// schedule-mirror) — the engine treats it the way `/schedule off` treats
// type=schedule beads. Aliased to pausegate.SchedulerPauseTitle so the
// `/schedule` command, the /plans master-switch banner, and this fire-time read
// share ONE string constant (ka-0000004048). The value is unchanged.
const schedulerPauseTitleForTriggers = pausegate.SchedulerPauseTitle

// scheduleSourceNames lists trigger source names whose firing should be
// silenced by `/schedule off` (the scheduler-pause gate bead and per-project
// schedules_enabled=false). These sources fire schedule-shaped triggers on
// cron and share the schedule subsystem's pause controls — they are gated
// in addition to the existing triggers-pause / triggers_enabled controls.
// Gardener triggers get the same gates whatever source loads them
// (scheduleGatedGardener), so an event-mode plan gardener on trigger:multi
// honours `/schedule off` too.
var scheduleSourceNames = map[string]bool{
	"schedule":        true,
	"schedule-mirror": true,
}

// TriggerNotifier is an optional callback for trigger notifications.
//
// channel is the destination Slack channel for THIS notification. Empty means
// "use the notifier's default channel" — every internal watchdog caller passes
// "" and keeps its historical default-channel behavior; only the notify-action
// path (actionNotify) threads a per-trigger channel through, sourced from the
// trigger bead's `slack_channel` field (ka-0000008204). A NewNoticeNotifier
// writes it onto the notice bead's channel field so slack-bridge posts there
// instead of its single project-default channel.
type TriggerNotifier func(ctx context.Context, triggerTitle, message, channel string)

// TriggerMatch represents a single item matched by a trigger evaluation.
// Each source produces TriggerMatch values from its evaluation logic.
type TriggerMatch struct {
	// Key is the unique dedup key for this match (e.g. JIRA issue key, commit SHA).
	Key string
	// Title is a human-readable title for the matched item.
	Title string
	// Fields are source-specific metadata to store on created beads.
	Fields map[string]string
	// Labels are additional labels to add to created beads.
	Labels []string
	// TemplateVars are key→value pairs available for template expansion.
	// Keys should use the pattern "{{source_field}}" (e.g. "{{jira_key}}").
	TemplateVars map[string]string

	// orphanTaskID, set by refireOrphanedMatches, is the open task bead an
	// earlier fire of this match filed without spawning an agent.
	// actionSpawnAgent spawns onto it instead of filing a second task.
	orphanTaskID string
}

// MatchOperatorResolver maps a TriggerMatch to a HUMAN operator id (slack:… /
// oidc:…) derived from the matched item — e.g. a Jira ticket's reporter mapped
// to a fleet user. It returns "" when the match carries no resolvable human, so
// the engine falls through to its DefaultOperator / fleet attribution. Wired
// per-source (the jira evaluator resolves the reporter); unset engines behave
// exactly as before. See ka-0000001781.
type MatchOperatorResolver func(ctx context.Context, m TriggerMatch) string

// TriggerSource is the interface that each trigger source must implement.
// It handles loading trigger definitions and evaluating them against the source.
type TriggerSource interface {
	// Name returns the source identifier (e.g. "jira", "gitlab").
	// Used to filter trigger beads by label "trigger:<name>".
	Name() string

	// LoadTriggers fetches all enabled trigger beads for this source.
	LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error)

	// Evaluate runs the trigger's source-specific query and returns matches.
	// For example, a JIRA source runs JQL; a GitLab source checks MR events.
	Evaluate(ctx context.Context, trig TriggerDef) ([]TriggerMatch, error)

	// CatchUpKeys returns the dedup keys for existing beads created by this trigger.
	// This prevents re-firing on restart. The engine calls this once per trigger
	// during CatchUp.
	CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error)
}

// bootstrapOptOutSource is an optional interface a TriggerSource may implement
// to opt out of the runtime-bootstrap guard. Poll-based sources (JIRA, GitLab)
// that return all currently-matching items need bootstrap to suppress firing on
// pre-existing events when a trigger is first created. Event-driven sources
// (slack reactions, bead state) drain a buffer of events that have arrived
// since the last call, so every match is genuinely new — they should opt out.
type bootstrapOptOutSource interface {
	// SkipRuntimeBootstrap returns true for event-driven sources whose Evaluate
	// returns only fresh events (not historical state). Default (not implementing
	// this interface) is to require bootstrap.
	SkipRuntimeBootstrap() bool
}

// sourceNeedsBootstrap reports whether the engine should apply the
// runtime-bootstrap guard to this source. True by default; false only when the
// source explicitly opts out.
func sourceNeedsBootstrap(source TriggerSource) bool {
	if opt, ok := source.(bootstrapOptOutSource); ok {
		return !opt.SkipRuntimeBootstrap()
	}
	return true
}

// TriggerEngine is the shared trigger evaluation engine. It manages dedup state,
// cooldown tracking, and action dispatch for all trigger sources.
type TriggerEngine struct {
	daemon   TriggerBeadClient
	notifier TriggerNotifier
	logger   *slog.Logger
	guard    *spawnguard.SpawnGuard
	sources  map[string]TriggerSource

	// promptRenderers re-render a trigger's prompt at fire time, keyed by a
	// marker label on the trigger bead (trigger_prompt_render.go).
	promptRenderers map[string]PromptRenderer

	// requireHumanMode gates whether an autonomous trigger/gardener spawn that
	// resolves no human operator is allowed (warn) or refused (enforce). Read
	// once from GARDENIO_REQUIRE_HUMAN_OPERATOR at construction. See
	// human_operator.go (epic ka-0000004835, task ka-0000002839).
	requireHumanMode HumanOperatorMode

	// defaultOperator is the designated HUMAN operator (slack:U… / oidc:…@…) a
	// fire attributes to when the trigger bead carries no walkable human author
	// (its operated_by/created_by are a sentinel/agent). Read once from
	// TRIGGER_ENGINE_DEFAULT_OPERATOR at construction and validated human-shaped
	// (non-human values are dropped to ""). Empty ⇒ no fallback, preserving the
	// pre-existing behavior. Mirrors JiraAgentResolver.defaultOperator for the
	// JQL/bead/schedule fireAction path (ka-0000006023).
	defaultOperator string

	// matchOperatorResolver, when non-nil, maps a TriggerMatch to a HUMAN
	// operator id derived from the matched item itself — e.g. the Jira ticket
	// reporter mapped to a fleet human via the UserDirectory. It fires in
	// fireAction ONLY when the trigger bead's own author is not a human, and
	// takes precedence over defaultOperator. This closes the gap where a
	// sentinel-authored JQL auto-spawn stamped fleet:jira-bridge instead of the
	// ticket reporter (ka-0000001781). Source-agnostic: the jira evaluator wires
	// a reporter resolver; other engines leave it nil (behavior unchanged).
	matchOperatorResolver MatchOperatorResolver

	mu   sync.Mutex
	seen map[string]map[string]bool // trigger bead ID → set of dedup keys already processed

	// seenAt mirrors seen for inner-key GC (ka-0000003049): trigger bead ID →
	// dedup key → the first time gcSeenKeys OBSERVED that key. It is
	// reconcile-based on purpose — the ~10 sites that write seen need no
	// instrumentation; the periodic sweep stamps any key it has not seen before
	// and evicts keys older than triggerSeenKeyTTL from BOTH seen and seenAt.
	// Without this the inner set grows one PERMANENT entry per distinct dedup
	// key a trigger ever observes; for a broad bead-state `updated` trigger the
	// key folds in a changes fingerprint, so that is one entry per bead
	// field-change across the whole fleet, forever, for the life of the
	// process — monotonic heap growth that OOMs the single-replica engine and
	// stalls its Evaluate() deadman loop. lastSeenGC throttles the sweep.
	seenAt     map[string]map[string]time.Time
	lastSeenGC time.Time

	// orphanRefireAt records, per trigger and fire key, when the engine last
	// re-fired a window whose task bead had no agent, so a spawn that keeps
	// failing retries on orphanRefireBackoff rather than every cycle
	// (gd-CZnK7jeKSG, trigger_orphan_fire.go).
	orphanRefireAt map[string]map[string]time.Time

	// ignoreCooldown is a TEST-ONLY seam (mirrors SeedSeen). Trigger IDs added
	// via IgnoreCooldown skip the per-trigger cooldown gate in evalOne, so a
	// single deliberate probe fire is not suppressed by a `last_checked` stamp
	// written by ANOTHER evaluator sharing the same daemon. The cross-env
	// cooldown-starvation race (ka-0000006237) is a real production hazard the
	// engine WARN-detects but cannot avoid on a shared daemon; in the E2E
	// namespace a live trigger-engine (30s poll) stamps last_checked on the
	// e2e's freshly-created trigger:bead beads on every non-firing poll, which
	// cooldown-starved cmd/trigger-probe's one-shot fire and flaked
	// test-trigger-action-fidelity (ka-0000007982). The probe — a test-only
	// binary that never ships in a product image — opts each trigger it fires
	// out of the gate so its deliberate injected event always evaluates. Never
	// set by the production trigger-engine/bridge, so live cooldown throttling
	// is unchanged.
	ignoreCooldown map[string]bool

	// lastObserved / selfStamped back the out-of-band last_checked detector
	// (ka-0000006237 Layer 1b). lastObserved is the last_checked value seen
	// on each trigger at its previous evalOne call; selfStamped is the time
	// THIS engine last wrote last_checked for the trigger. When a trigger is
	// cooldown-skipped and its last_checked advanced since the previous cycle
	// without this engine writing it, a foreign evaluator (e.g. a sandbox
	// bridge misconfigured onto this daemon) is racing us for the fire window
	// — the signal that would have caught the 2026-07-01 outage in one cycle
	// instead of 414.
	lastObserved map[string]time.Time
	selfStamped  map[string]time.Time

	// checkStamps backs the last_checked write throttle (gd-Mfr8MRXbYG). Per
	// trigger it remembers the last evaluation time and the last bead write
	// (time + outcome), so a steady-state evaluation that would only re-stamp
	// the same outcome with a newer timestamp skips the PATCH. Every PATCH of a
	// trigger bead emits a MutationUpdate carrying the whole fields blob
	// (30-190KB for a gardener), so ~100 no-op stamps an hour per trigger
	// dominated the kbeads event stream. In-memory only: a restart just writes
	// once more.
	checkStamps map[string]checkStamp

	// starve backs the per-trigger cooldown-starvation SYMPTOM detector
	// (Alert B of epic ka-0000005107, trigger_starve.go): consecutive
	// cooldown-skip streaks vs cycles that got past the gate, published as
	// gardenio_trigger_starved / gardenio_trigger_cooldown_skip_streak. The
	// thresholds and clock are engine fields so tests can compress time.
	starve            map[string]*triggerStarveState
	starveMinCycles   int
	starveMinDuration time.Duration
	starveRecheckTTL  time.Duration
	now               func() time.Time

	// warnedGate tracks trigger IDs already warned for unbacked gardener prose
	// (prompt claims run_budget/terminal_goal but no gardener block / conditions
	// pre-flight). Warn-once per trigger so the log isn't spammed every cycle.
	// See gardener_gate_guard.go and bug ka-0000006735.
	warnedGate map[string]bool

	// Engine-side gardener-liveness heartbeat invariant (B2, ka-0000001541).
	// When livenessEnabled, SweepGardenerLiveness runs once per Evaluate cycle
	// and alerts (notify and/or create_bead, never spawn) on any enabled
	// gardener whose last_checked has gone stale. See gardener_liveness.go.
	livenessEnabled    bool
	livenessNotify     bool
	livenessCreateBead bool
	livenessState      *livenessSweepState

	// livenessWatchIDs is an operator-configured allow-list of gardener bead ids
	// swept across ALL statuses every cycle (bug ka-0000004451), independent of
	// the open-only roster the main sweep uses. It is the steady-state poll that
	// closes the transition-only blind spot: a watched gardener that self-closed
	// (max_runs / no_progress_for) or was disabled emits no future lifecycle
	// event and drops out of the open roster, so an event-driven assert that
	// misses the one close transition (SSE gap, reconciler-watermark bootstrap,
	// engine restart) never fires again. Polling the explicit id list re-checks
	// current status every cycle regardless of any event. Empty ⇒ the pass is a
	// no-op (default), so it floods nobody — only the operator-named ids are
	// swept across closed/disabled, not the ~130 fleet-wide closed+enabled pool
	// the main sweep's SCOPE BOUNDARY deliberately excludes.
	livenessWatchIDs []string
	// livenessAllowListState is a SEPARATE per-episode dedup map for the
	// all-status allow-list pass. It must not share livenessState: the main
	// sweep calls livenessState.prune(live) each cycle with only its open-roster
	// ids, which would wipe the allow-list's (closed/disabled) entries every
	// cycle and re-alert forever. Its own state prunes against its own id set.
	livenessAllowListState *livenessSweepState

	// Engine-side CHECK-liveness sweep for NON-gardener triggers
	// (ka-0000004245, trigger_check_liveness.go). The gardener sweep above is
	// gated on isGardenerTrigger, so a plain intake trigger whose last_checked
	// froze — because the engine dropped it, or because the stamp WRITE is
	// being 403'd — was watched by nothing. That is how three prod jira lanes
	// sat with a frozen liveness signal for 11 days reading enabled=true.
	// Alert-only (WARN + optional notify), never spawns, files no bead.
	// checkLivenessState is a DEDICATED dedup map: sharing livenessState would
	// let the two sweeps' prune(live) calls clobber each other's episodes.
	checkLivenessEnabled bool
	checkLivenessNotify  bool
	checkLivenessState   *livenessSweepState

	// "Pouring-but-never-worked" stall sweep (ka-0000000957). The sibling of
	// the liveness sweep for the ORTHOGONAL failure: an apply_formula/gardener
	// trigger keeps pouring molecules on cadence (last_checked fresh, so the
	// liveness sweep reads it as alive) while the poured molecules accumulate
	// open with unworked step beads. When pouringStallEnabled,
	// SweepPouringButNeverWorked runs once per Evaluate cycle and alerts
	// (notify and/or create_bead, never spawn). See gardener_pouring_stall.go.
	pouringStallEnabled    bool
	pouringStallNotify     bool
	pouringStallCreateBead bool
	pouringStallState      *pouringStallSweepState

	// Dark-lane-by-concurrency detector (bug ka-0000002023). The sibling of the
	// liveness sweep for the orthogonal failure: a spawn-producing trigger that
	// is enabled and actively evaluated (last_checked fresh) yet has not FIRED in
	// many cadences because a live-but-wedged agent pins its max_concurrent lease
	// — a lane that looks healthy on every dashboard while spawning nothing. When
	// darkLaneEnabled, SweepDarkLaneByConcurrency runs once per Evaluate cycle and
	// alerts (notify and/or create_bead, never spawn). See trigger_dark_lane.go.
	darkLaneEnabled    bool
	darkLaneNotify     bool
	darkLaneCreateBead bool
	darkLaneState      *darkLaneSweepState

	// sweepRoster is the current Evaluate cycle's shared open-trigger roster
	// (trigger_sweep_roster.go); nil outside Evaluate's sweep phase.
	sweepRoster *sweepRosterMemo

	// iterations caches each gardener's closed iteration agents across cycles
	// for the two iteration reconcilers in evalOne (gardener_iteration_cache.go).
	// nil (an engine built without NewTriggerEngine) lists them every call.
	iterations *closedIterationCache

	// Credential-wedged-agent detector (bug ka-0000000592,
	// trigger_credential_wedge.go). The fourth sibling of the silent-failure
	// sweeps, for the credential-exhaustion dimension of the "invisible to
	// updated_at" gap: an agent parked in agent_state=rate_limited/budget_exceeded
	// with zero completed turns while SyncAll keeps churning its updated_at fresh.
	// When credWedgeEnabled, SweepCredentialWedge runs once per Evaluate cycle and
	// WARN-logs + one-shot-comments on the wedged agent bead (never spawns/kills).
	credWedgeEnabled    bool
	credWedgeNotify     bool
	credWedgeCreateBead bool
	credWedgeState      *credWedgeSweepState

	// Closed+enabled dark-trigger DRAIN sweep (bug ka-0000002082,
	// closed_enabled_drain.go). Unlike the three ALERT sweeps above, this one is
	// the CAPACITY fix for the closed+enabled class the liveness SCOPE BOUNDARY
	// hands to trigger-reviver: the agent could only file ONE archive decision
	// per 4h cycle against a 137-deep pool, so the operator queue grew
	// monotonically. When closedEnabledDrainEnabled, SweepClosedEnabledDrain runs
	// (throttled to once per hour) and (1) auto-archives closed+enabled triggers
	// whose gardener-epic is CLOSED — capped, commented, reversible — and (2) files
	// ONE grouped archive decision for the unanchored cohort, deduped to one open
	// at a time and executed on resolution. It NEVER spawns and never touches an
	// open-epic-anchored lane.
	closedEnabledDrainEnabled bool
	closedEnabledDrainNotify  bool
	closedEnabledDrainState   *closedEnabledDrainSweepState

	closedEpicReaperEnabled bool
	closedEpicReaperNotify  bool
	closedEpicReaperState   *closedEpicReaperSweepState

	// Uncovered-open-lane ROLL-UP report (bug ka-0000002082 fix 4,
	// uncovered_open_lane_rollup.go). The read-only counterpart of the drain
	// sweep: where SweepClosedEnabledDrain archives the low-value rows, this one
	// surfaces the class it DELIBERATELY leaves untouched — closed+enabled
	// triggers anchored to an OPEN gardener-epic that still has live children and
	// no live successor gardener. Those are genuinely-uncovered lanes that should
	// be revived, not archived. Rather than one revival-review decision per lane
	// (which re-created the file-faster-than-resolved backlog), it maintains ONE
	// living type=doc report of the whole cohort, refreshed daily and updated in
	// place. Files no decisions and mutates no triggers.
	uncoveredOpenLaneRollupEnabled bool
	uncoveredOpenLaneRollupNotify  bool
	uncoveredOpenLaneRollupState   *uncoveredOpenLaneRollupState

	// Age-based ESCALATION of stale revival-review decisions (bug ka-0000002082
	// fix 3, stale_revival_review_escalate.go). The third leg of the same bug:
	// where the drain sweep reduces the INFLOW of these decisions and the roll-up
	// decouples the uncovered-open lanes, this one raises the priority of the
	// revival-review decisions that ARE filed once they go stale (>7d→P1, >14d→P0)
	// so a 10-day-old decision does not sit at P2 behind newer work. Mutates only
	// the priority attribute; creates/closes/resolves no decisions.
	staleRevivalReviewEscalateEnabled bool
	staleRevivalReviewEscalateNotify  bool
	staleRevivalReviewEscalateState   *staleRevivalReviewEscalateState

	// Daily dormant-gardener sweep (gd-kkrCdlf4iO, gardener_dormant_sweep.go):
	// files one deduped operator escalation naming gardeners dormant over 24h
	// whose live dormancy probe says they have actionable children.
	dormantGardenerSweepEnabled bool
	dormantGardenerSweepNotify  bool
	dormantGardenerSweepState   *dormantGardenerSweepState

	// Quiet-gardener sweep (gd-bo2RUmtpHb, gardener_quiet_sweep.go): wakes an
	// event-mode plan gardener that has not run in 2h while its plan has a
	// ready, unassigned step, notes it on the plan, and escalates a second
	// wake of the same gardener within a day. Off until
	// EnableQuietGardenerSweep; nil state means off.
	quietGardenerSweepNotify bool
	quietGardenerSweepState  *quietGardenerSweepState
}

// TriggerEngineConfig holds configuration for the trigger engine.
type TriggerEngineConfig struct {
	Daemon   TriggerBeadClient
	Notifier TriggerNotifier
	Logger   *slog.Logger
	// Guard is the shared runaway-protection gate. nil disables the check,
	// preserving prior behavior.
	Guard *spawnguard.SpawnGuard

	// LivenessEnabled turns on the engine-side gardener-liveness heartbeat
	// sweep (B2, ka-0000001541). When false (the default), SweepGardenerLiveness
	// is a no-op so existing engine wirings are unchanged until they opt in.
	LivenessEnabled bool
	// LivenessNotify and LivenessCreateBead select which alert leg(s) the
	// sweep fires on a dark gardener. Both default false; an enabled sweep with
	// neither set still logs but raises no external alert. Typical config is
	// LivenessNotify=true (Slack via the notice publisher) with
	// LivenessCreateBead optional for a durable bead trail.
	LivenessNotify     bool
	LivenessCreateBead bool
	// CheckLivenessEnabled turns on the NON-gardener trigger check-liveness
	// sweep (ka-0000004245). CheckLivenessNotify routes the alert to Slack via
	// the notice publisher. Defaults on: the sweep is read-only, idempotent,
	// one alert per dark episode, and spawns nothing.
	CheckLivenessEnabled bool
	CheckLivenessNotify  bool

	// LivenessWatchIDs is an operator-configured allow-list of gardener bead ids
	// the liveness sweep additionally checks across ALL statuses every cycle
	// (bug ka-0000004451, SweepGardenerLivenessAllowList). It is the steady-state
	// poll parity the residual task ka-0000000484 named as the only path that
	// truly matches the retired poll-every-cycle guard ka-0000006629: the main
	// open-only sweep cannot see a watched gardener that self-closed or was
	// disabled, and the event-driven assert misses a single close transition on
	// any SSE/reconciler gap and then never re-fires. Empty (the default) ⇒ the
	// all-status pass is a no-op. Only these named ids are swept across
	// closed/disabled — NOT the ~130 fleet-wide closed+enabled pool the main
	// sweep's SCOPE BOUNDARY excludes to avoid a flood.
	LivenessWatchIDs []string

	// PouringStallEnabled turns on the engine-side "pouring-but-never-worked"
	// stall sweep (ka-0000000957). When false (the default),
	// SweepPouringButNeverWorked is a no-op so existing engine wirings are
	// unchanged until they opt in. This is the sibling of the liveness sweep
	// for the orthogonal failure mode (molecules pouring on cadence but their
	// step beads never worked) that the FLAT gardener hit — a gardener that looks
	// alive (last_checked fresh) while delivering nothing.
	PouringStallEnabled bool
	// PouringStallNotify and PouringStallCreateBead select which alert leg(s)
	// the sweep fires on a stalled trigger. Both default false; an enabled
	// sweep with neither set still logs but raises no external alert. Typical
	// config is PouringStallNotify=true (Slack via the notice publisher) with
	// PouringStallCreateBead optional for a durable bead trail. The sweep NEVER
	// spawns an agent — it only surfaces the stall.
	PouringStallNotify     bool
	PouringStallCreateBead bool

	// DarkLaneEnabled turns on the engine-side dark-lane-by-concurrency detector
	// (bug ka-0000002023). When false (the default), SweepDarkLaneByConcurrency is
	// a no-op so existing engine wirings are unchanged until they opt in. This is
	// the sibling of the liveness sweep for the orthogonal failure mode: a
	// spawn-producing trigger enabled and actively evaluated (last_checked fresh)
	// yet not FIRING for many cadences because a wedged agent pins its
	// max_concurrent lease — the silent 15h dark-lane that motivated the bug.
	DarkLaneEnabled bool
	// DarkLaneNotify and DarkLaneCreateBead select which alert leg(s) the sweep
	// fires on a suppressed lane. Both default false; an enabled sweep with
	// neither set still logs but raises no external alert. Typical config is
	// DarkLaneNotify=true (Slack via the notice publisher) with DarkLaneCreateBead
	// optional for a durable bead trail. The sweep NEVER spawns.
	DarkLaneNotify     bool
	DarkLaneCreateBead bool

	// CredWedgeEnabled turns on the engine-side credential-wedged-agent detector
	// (bug ka-0000000592). When false (the default), SweepCredentialWedge is a
	// no-op so existing engine wirings are unchanged until they opt in. This is
	// the fourth sibling of the silent-failure sweeps, for the credential-
	// exhaustion dimension of the "invisible to updated_at" gap ka-0000002023
	// named: an agent parked in agent_state=rate_limited/budget_exceeded with zero
	// completed turns (zero output tokens, zero cost) while SyncAll keeps churning
	// its updated_at fresh, so every updated_at-based check reads it as alive.
	CredWedgeEnabled bool
	// CredWedgeNotify and CredWedgeCreateBead select which OPTIONAL alert leg(s)
	// the sweep fires. The one-shot agent-bead comment is ALWAYS written (the
	// primary bead-navigable signal). Both default false; an enabled sweep with
	// neither still WARN-logs and comments. Typical config is CredWedgeNotify=true
	// (Slack via the notice publisher) with CredWedgeCreateBead optional for a
	// durable bead trail. The sweep NEVER spawns or kills.
	CredWedgeNotify     bool
	CredWedgeCreateBead bool

	// ClosedEnabledDrainEnabled turns on the closed+enabled dark-trigger DRAIN
	// sweep (bug ka-0000002082, closed_enabled_drain.go). When false (the
	// default), SweepClosedEnabledDrain is a no-op so existing engine wirings are
	// unchanged until they opt in. Unlike the three ALERT sweeps this one MUTATES
	// beads (auto-archives retired-lineage triggers, files + executes grouped
	// archive decisions), so it is throttled to once per hour, capped per run, and
	// gated behind global triggers/gardeners-pause. Every mutation is reversible
	// (enabled=true re-arms) and audited.
	ClosedEnabledDrainEnabled bool
	// ClosedEnabledDrainNotify routes a one-line per-run summary to Slack via the
	// notice publisher when the sweep did something (archived, executed a
	// decision, or filed one). Default false.
	ClosedEnabledDrainNotify bool

	// ClosedEpicReaperEnabled turns on the gardener-reaper sweep (task
	// ka-0000000102, gardener_epic_reaper.go). When false (the default),
	// SweepClosedEpicGardeners is a no-op so existing engine wirings are unchanged
	// until they opt in. It auto-closes OPEN gardener triggers whose pinned epic is
	// CLOSED (audit ka-0000001071 categories A/C — the enabled-but-inert and
	// dead-disabled gardeners no other sweep reaps). Like the drain sweep it
	// MUTATES beads, so it is throttled to once per hour, capped per run, and gated
	// behind global gardeners/triggers-pause. Every close is reversible (reopen +
	// `gb trigger enable` re-arms) and audited (a comment per close). Shipped
	// default-off so an operator confirms the dry-run "would reap" log lines match
	// only genuine retirements before arming a destructive close path (rollout
	// discipline task ka-0000003021).
	ClosedEpicReaperEnabled bool
	// ClosedEpicReaperNotify routes a one-line per-run summary to Slack via the
	// notice publisher when the sweep reaped at least one gardener. Default false.
	ClosedEpicReaperNotify bool

	// UncoveredOpenLaneRollupEnabled turns on the uncovered-open-lane roll-up
	// report (bug ka-0000002082 fix 4, uncovered_open_lane_rollup.go). When false
	// (the default), SweepUncoveredOpenLaneRollup is a no-op so existing engine
	// wirings are unchanged until they opt in. Unlike the drain/reaper sweeps it
	// mutates NO triggers and files NO decisions — it is a pure read + one
	// idempotent living-doc upsert, throttled to once per day and pause-aware.
	UncoveredOpenLaneRollupEnabled bool
	// UncoveredOpenLaneRollupNotify routes a one-line per-run summary to Slack via
	// the notice publisher when the report lists at least one uncovered lane.
	// Default false.
	UncoveredOpenLaneRollupNotify bool

	// StaleRevivalReviewEscalateEnabled turns on the age-based escalation of stale
	// revival-review decisions (bug ka-0000002082 fix 3,
	// stale_revival_review_escalate.go). When false (the default),
	// SweepStaleRevivalReviewEscalate is a no-op so existing engine wirings are
	// unchanged until they opt in. It raises the priority of an OPEN
	// `trigger-reviver-revival-review` decision that has gone stale (>7d→P1,
	// >14d→P0) so it surfaces above newer work in the operator queue. Mutates only
	// the priority attribute (reversible, audited); creates/closes/resolves no
	// decisions. Throttled to once per hour and gated behind global
	// triggers/gardeners-pause.
	StaleRevivalReviewEscalateEnabled bool
	// StaleRevivalReviewEscalateNotify routes a one-line per-run summary to Slack
	// via the notice publisher when the sweep escalated at least one decision.
	// Default false.
	StaleRevivalReviewEscalateNotify bool

	// DormantGardenerSweepEnabled turns on the daily dormant-gardener sweep
	// (gd-kkrCdlf4iO, gardener_dormant_sweep.go). When false (the default),
	// SweepDormantGardeners is a no-op. Once a day it files ONE operator
	// escalation, deduped while open, naming every gardener dormant over 24h
	// whose live ReconcileDormancyProbe says NOT parked with actionable
	// children. It never wakes, spawns or edits a gardener.
	DormantGardenerSweepEnabled bool
	// DormantGardenerSweepNotify routes a one-line Slack notice via the notice
	// publisher when the sweep files or updates its escalation. Default false.
	DormantGardenerSweepNotify bool

	// DefaultOperator is an explicit designated HUMAN operator the engine
	// attributes a fire to when the trigger bead has no walkable human author.
	// When empty, the engine reads TRIGGER_ENGINE_DEFAULT_OPERATOR from the
	// environment instead. A non-human value (in either source) is dropped so
	// the engine never stamps an operator the daemon fails closed on. Mirrors
	// JiraAgentResolverConfig.DefaultOperator (ka-0000006023).
	DefaultOperator string

	// MatchOperatorResolver, when non-nil, resolves a HUMAN operator from a
	// matched item (e.g. the Jira ticket reporter → fleet human). It is
	// consulted in fireAction before the DefaultOperator fallback and only when
	// the trigger bead's own author is not a human. Source-specific wirings set
	// it (the jira evaluator maps the reporter); leave it nil to preserve prior
	// behavior. See ka-0000001781.
	MatchOperatorResolver MatchOperatorResolver

	// Cooldown-starvation detector thresholds (Alert B of epic ka-0000005107,
	// trigger_starve.go). Zero values take the package defaults
	// (defaultStarveMinCycles / defaultStarveMinDuration /
	// defaultStarveRecheckTTL); tests lower them to drive the detector
	// without wall-clock waits.
	StarveMinCycles   int
	StarveMinDuration time.Duration
	StarveRecheckTTL  time.Duration
	// Clock is the injectable time source for the starvation detector.
	// nil = time.Now.
	Clock func() time.Time
}

// NewTriggerEngine creates a new shared trigger engine.
func NewTriggerEngine(cfg TriggerEngineConfig) *TriggerEngine {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Prefer an explicit config override (validated human-shaped) and otherwise
	// fall back to the TRIGGER_ENGINE_DEFAULT_OPERATOR env. Both paths drop a
	// non-human value to "" so a typo can never re-introduce ka-0000002128.
	defaultOperator := strings.TrimSpace(cfg.DefaultOperator)
	if defaultOperator != "" && !IsHumanOperator(defaultOperator) {
		logger.Warn("triggers: ignoring non-human TriggerEngineConfig.DefaultOperator",
			"value", defaultOperator)
		defaultOperator = ""
	}
	if defaultOperator == "" {
		defaultOperator = TriggerDefaultOperatorFromEnv()
	}
	starveMinCycles := cfg.StarveMinCycles
	if starveMinCycles <= 0 {
		starveMinCycles = defaultStarveMinCycles
	}
	starveMinDuration := cfg.StarveMinDuration
	if starveMinDuration <= 0 {
		starveMinDuration = defaultStarveMinDuration
	}
	starveRecheckTTL := cfg.StarveRecheckTTL
	if starveRecheckTTL <= 0 {
		starveRecheckTTL = defaultStarveRecheckTTL
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &TriggerEngine{
		daemon:                 cfg.Daemon,
		notifier:               cfg.Notifier,
		logger:                 logger,
		guard:                  cfg.Guard,
		requireHumanMode:       HumanOperatorModeFromEnv(),
		defaultOperator:        defaultOperator,
		matchOperatorResolver:  cfg.MatchOperatorResolver,
		sources:                make(map[string]TriggerSource),
		iterations:             newClosedIterationCache(clock),
		seen:                   make(map[string]map[string]bool),
		seenAt:                 make(map[string]map[string]time.Time),
		orphanRefireAt:         make(map[string]map[string]time.Time),
		ignoreCooldown:         make(map[string]bool),
		lastObserved:           make(map[string]time.Time),
		selfStamped:            make(map[string]time.Time),
		checkStamps:            make(map[string]checkStamp),
		starve:                 make(map[string]*triggerStarveState),
		starveMinCycles:        starveMinCycles,
		starveMinDuration:      starveMinDuration,
		starveRecheckTTL:       starveRecheckTTL,
		now:                    clock,
		warnedGate:             make(map[string]bool),
		livenessEnabled:        cfg.LivenessEnabled,
		livenessNotify:         cfg.LivenessNotify,
		livenessCreateBead:     cfg.LivenessCreateBead,
		livenessState:          newLivenessSweepState(),
		livenessWatchIDs:       sanitizeWatchIDs(cfg.LivenessWatchIDs),
		livenessAllowListState: newLivenessSweepState(),
		checkLivenessEnabled:   cfg.CheckLivenessEnabled,
		checkLivenessNotify:    cfg.CheckLivenessNotify,
		checkLivenessState:     newLivenessSweepState(),
		pouringStallEnabled:    cfg.PouringStallEnabled,
		pouringStallNotify:     cfg.PouringStallNotify,
		pouringStallCreateBead: cfg.PouringStallCreateBead,
		pouringStallState:      newPouringStallSweepState(),
		darkLaneEnabled:        cfg.DarkLaneEnabled,
		darkLaneNotify:         cfg.DarkLaneNotify,
		darkLaneCreateBead:     cfg.DarkLaneCreateBead,
		darkLaneState:          newDarkLaneSweepState(),

		credWedgeEnabled:    cfg.CredWedgeEnabled,
		credWedgeNotify:     cfg.CredWedgeNotify,
		credWedgeCreateBead: cfg.CredWedgeCreateBead,
		credWedgeState:      newCredWedgeSweepState(),

		closedEnabledDrainEnabled: cfg.ClosedEnabledDrainEnabled,
		closedEnabledDrainNotify:  cfg.ClosedEnabledDrainNotify,
		closedEnabledDrainState:   newClosedEnabledDrainSweepState(),

		closedEpicReaperEnabled: cfg.ClosedEpicReaperEnabled,
		closedEpicReaperNotify:  cfg.ClosedEpicReaperNotify,
		closedEpicReaperState:   newClosedEpicReaperSweepState(),

		uncoveredOpenLaneRollupEnabled: cfg.UncoveredOpenLaneRollupEnabled,
		uncoveredOpenLaneRollupNotify:  cfg.UncoveredOpenLaneRollupNotify,
		uncoveredOpenLaneRollupState:   newUncoveredOpenLaneRollupState(),

		staleRevivalReviewEscalateEnabled: cfg.StaleRevivalReviewEscalateEnabled,
		staleRevivalReviewEscalateNotify:  cfg.StaleRevivalReviewEscalateNotify,
		staleRevivalReviewEscalateState:   newStaleRevivalReviewEscalateState(),

		dormantGardenerSweepEnabled: cfg.DormantGardenerSweepEnabled,
		dormantGardenerSweepNotify:  cfg.DormantGardenerSweepNotify,
		dormantGardenerSweepState:   newDormantGardenerSweepState(),
	}
}

// RegisterSource adds a trigger source to the engine.
func (e *TriggerEngine) RegisterSource(source TriggerSource) {
	e.sources[source.Name()] = source
}
