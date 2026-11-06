package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/spawnguard"
)

// formula_step_dispatch.go — the step-role dispatcher (ka-0000001730, bug
// ka-0000001755).
//
// WHY THIS EXISTS. Pouring a formula materializes N dep-chained step beads and
// spawns exactly ONE driver agent at the MOLECULE, with the role hardcoded
// "crew" (bot_formula.go handleFormulaPour) or trig.Role defaulting to crew
// (triggers.go actionApplyFormula). That single driver is then EXPECTED to walk
// every step in one context and honor the per-step roles by reading them.
//
// Per-step roles are declared (formula.Step.Roles), poured, and stamped onto
// each step bead as a `role:<x>` label. But nothing ever READ that label to
// choose a spawn role — so the "this step needs a different role" intent was
// data nobody consumed.
//
// That is fine right up until the driver despawns. FSM-472: the repro crew
// (role monorepo-repro) emitted gate_passed=true at 03:15 and exited by
// contract; the thread that spawned it had already auto-closed
// (slack-thread-idle); the §8 fix-proof step (role monorepo-fix) never ran.
// Zero role:monorepo-fix agents existed fleet-wide. No error, no nudge, no
// decision — the gate cleared into a void, and a human noticed 50 minutes later
// by asking in Slack.
//
// The general lesson: a handoff encoded as an instruction to a live process is
// a no-op the moment that process exits. For despawn-class roles the process
// provably never outlives the gate.
//
// THE SEAM. A step bead becomes workable exactly when its last blocking dep
// closes. The daemon already publishes beads.bead.closed on every close, and a
// formula's step chain is `blocks` edges between step beads. So this dispatcher
// holds ONE long-lived SSE subscription and, for each closing step bead, walks
// its dependents: any that (a) carry a step_role, (b) opt in via
// suggest_new_agent, and (c) now have every blocking dep closed, get a
// role-matched agent spawned on them.
//
// "The dep cleared so the bead went ready" is the mechanism the fleet already
// has. This just puts an agent on it without a human in the loop.
//
// OPT-IN, NOT BLANKET. Dispatch fires only for steps whose formula sets
// suggest_new_agent — the field formula_instantiate.go has always written and
// nothing has ever read. Blanket dispatch would mint a pod per step of every
// molecule in the fleet and shatter the one-agent-walks-the-chain contract that
// most formulas rely on (a driver holding context across steps is a FEATURE for
// the seed-recipe and audit formulas). So a formula declares, per step, "this
// one deserves its own agent" — and only then does a pod appear.
//
// IDEMPOTENCY. Three independent layers, because a double-spawn here means two
// agents racing the same fix branch:
//  1. In-process dedup on step bead ID, so an SSE replay or a burst of sibling
//     closes cannot fire twice for one step.
//  2. `dispatched_agent` stamped on the step bead BEFORE the spawn — durable
//     across a restart, and the thing a human reads to see what happened.
//  3. SpawnAgent's own single-claim guard (single_claim_task), which refuses a
//     second agent on a task a live agent already holds.
//
// FAIL-OPEN, NEVER WEDGE. Every error path logs and returns. A dispatcher that
// crashes on one bad event would silently stop dispatching every other step —
// the exact failure class it exists to prevent.

// stepDispatchTopic is the daemon SSE topic the dispatcher consumes. A step
// bead becomes ready when the step it blocks on closes, and every close
// publishes here.
const stepDispatchTopic = "beads.bead.closed"

// stepDispatchUpdateTopic catches the OTHER way a blocker clears: a step going
// `partial`, which the ready-queue counts as terminal-for-progression but which
// emits an update rather than a close.
const stepDispatchUpdateTopic = "beads.bead.updated"

// stepDispatchDedupTTL bounds how long a dispatched step ID stays in the
// in-process dedup set. Long enough to absorb an SSE replay storm or a
// reconnect, short enough that the map cannot grow without bound in a
// long-lived process. The durable `dispatched_agent` field is the real guard;
// this is just the cheap first line.
const stepDispatchDedupTTL = 6 * time.Hour

// stepDispatchDaemon is the slice of the daemon client the dispatcher needs.
// Narrow by design so tests can drive it with a recording fake, and so the
// dependency reads dispatcher -> daemon.
type stepDispatchDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	GetDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
	// ListBeadsFiltered powers the backfill sweep (BackfillReadySteps): the
	// durable retry for steps the event path cannot re-drive.
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// stepRoleLister is the slice of the daemon the dispatcher uses to SOFT-validate
// a step's dispatch role against the active type:role catalog. Kept separate
// from stepDispatchDaemon (and optional — see WithRoleValidation) so the many
// existing tests need not grow a ListRoleBeads method, and so a deployment can
// run the dispatcher without role validation by simply not wiring it.
type stepRoleLister interface {
	ListRoleBeads(ctx context.Context) (map[string]beadsapi.RoleInfo, error)
}

// stepDispatchBuiltinRoles are roles that are first-class in controller/bridge
// code (seeded as role:<name> labels by init.go, used on the pod-spawn path) but
// may not have a type:role config bead — so an unbacked-role warning must NOT
// fire on them. This MUST stay equal to the controller's authoritative
// builtinRoles map (cmd/controller/project_role_cache.go); TestStepDispatchBuiltinRolesParity
// is the drift guard, mirroring gb's TestBuiltinRolesParity. Roles stay free
// strings with a soft warning by design (beadsapi.ValidateAgentRoles, design doc
// §8.5) — this set only suppresses false alarms, it never gates a spawn.
var stepDispatchBuiltinRoles = map[string]bool{
	"thread":        true,
	"crew":          true,
	"gardener":      true,
	"reviewer":      true,
	"job":           true,
	"plan-gardener": true,
}

// stepRoleCacheTTL bounds how long a resolved role catalog is reused before the
// next dispatch re-lists it. Role beads change rarely and the catalog is small,
// so a few minutes keeps a backfill burst (which can dispatch many steps in one
// 120s sweep) to at most one ListRoleBeads round-trip while staying fresh enough
// that a newly-landed role bead stops warning within one TTL.
const stepRoleCacheTTL = 5 * time.Minute

// stepDispatchBackfillLimit bounds how many candidate step beads one backfill
// sweep pulls. The candidate set is naturally small (open beads that opted into
// their own agent), so this is a generous ceiling; hitting it is logged as a
// possible truncation rather than silently dropping the tail.
const stepDispatchBackfillLimit = 500

// stepDispatchBackfillSentinel is the "unblocked_by" marker the backfill sweep
// passes instead of a real closing bead ID: a swept step has no single
// triggering close (its blockers cleared at some earlier, unobserved moment).
// stepDispatchPrompt special-cases it so the spawn prompt does not tell the
// agent a bead literally named "backfill" closed.
const stepDispatchBackfillSentinel = "backfill"

// FormulaStepDispatcher spawns a role-matched agent when a formula step bead's
// blocking deps clear. See the file header for the failure this closes.
//
// A nil *FormulaStepDispatcher is safe: Register is a no-op, so a deployment
// without a daemon simply runs without step dispatch.
type FormulaStepDispatcher struct {
	daemon stepDispatchDaemon
	guard  *spawnguard.SpawnGuard
	logger *slog.Logger

	mu         sync.Mutex
	dispatched map[string]time.Time // step bead ID -> when we dispatched it

	// now is overridable in tests so dedup expiry is deterministic.
	now func() time.Time

	// roleLister soft-validates a step's dispatch role against the active
	// type:role catalog. nil disables validation (the dispatcher runs exactly as
	// before). Wired via WithRoleValidation.
	roleLister stepRoleLister
	// roleCache/roleCacheAt memoize the last-listed role set so a backfill burst
	// costs at most one ListRoleBeads per stepRoleCacheTTL. Guarded by mu.
	roleCache   map[string]beadsapi.RoleInfo
	roleCacheAt time.Time

	// defaultOperator is the designated HUMAN operator (slack:U… / oidc:…@…) the
	// dispatcher attributes a spawn to when the step bead carries no walkable
	// human operator in its operated_by field. The TriggerEngine analogue is
	// e.defaultOperator (ctxWithTriggerOperatorOrDefault). Empty means "no
	// fallback configured" — the dispatcher then attributes only from the step's
	// own operated_by field, exactly as the pre-fix behavior. Validated
	// human-shaped by WithDefaultOperator, so it is always a value the daemon
	// accepts. Guarded by nothing: set once at construction, never mutated.
	defaultOperator string
}

// NewFormulaStepDispatcher builds a dispatcher over the daemon client.
// A nil daemon returns nil, matching the nil-safe contract above.
//
// guard is the shared fleet spawn rate-limiter / circuit breaker. Pass the
// SAME instance the trigger engine holds: a second, uncoordinated guard would
// let the two spawn paths each burn a full budget. A nil guard disables rate
// limiting (test convenience only — production wiring always passes one).
func NewFormulaStepDispatcher(daemon stepDispatchDaemon, guard *spawnguard.SpawnGuard, logger *slog.Logger) *FormulaStepDispatcher {
	if daemon == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	// Seed the spawn counter to a zero baseline so its series name is present in
	// the store the instant this pod is first scraped — before any formula step
	// has actually been dispatched. Without this, the CounterVec publishes no
	// series until the first (rare, bursty) spawn, so a freshly-restarted
	// trigger-engine pod reads as "spawned_total is dark" to the dashboard C5
	// existence check — the loop bug ka-0000003699 re-found three times. See
	// seedFormulaStepDispatchMetrics (formula_step_dispatch_metrics.go).
	seedFormulaStepDispatchMetrics()
	// Same treatment for the latency families (ka-0000006705): a HistogramVec
	// publishes no series until its first child is created either, and a
	// zero-observation child cannot distort any quantile — see
	// seedFormulaStepDispatchLatencyMetrics.
	seedFormulaStepDispatchLatencyMetrics()
	return &FormulaStepDispatcher{
		daemon:     daemon,
		guard:      guard,
		logger:     logger,
		dispatched: make(map[string]time.Time),
		now:        time.Now,
	}
}

// WithRoleValidation enables SOFT dispatch-time role validation: before spawning,
// the dispatcher checks the step's resolved role against the active type:role
// catalog and logs a WARN (plus a bounded decline-reason facet on the spawn
// metric) when the role is neither a builtin nor backed by a role bead. It NEVER
// blocks the spawn — a role whose config bead has not landed yet, or a listing
// error, both fail OPEN (design doc §8.5, mirroring roleWarning/ValidateAgentRoles).
//
// WHY IT LIVES HERE. Dispatch happens in trigger-engine, but the only existing
// role-validation warning (refreshRoleCache → ValidateAgentRoles) runs in the
// CONTROLLER process — so a typo'd step role currently spawns a pod with no
// role-bead overrides and trigger-engine says nothing. This closes that gap for
// the loss-prevention component whose G1 blind spot was itself a silent one.
//
// Returns the receiver for chaining. A nil lister leaves validation off; a nil
// dispatcher (nil-safe contract) is a no-op.
func (d *FormulaStepDispatcher) WithRoleValidation(lister stepRoleLister) *FormulaStepDispatcher {
	if d == nil {
		return d
	}
	d.roleLister = lister
	return d
}

// WithDefaultOperator sets the fallback HUMAN operator the dispatcher attributes
// a spawn to when the step bead carries no walkable human operator of its own.
// This is the FormulaStepDispatcher analogue of the TriggerEngine's
// DefaultOperator (ctxWithTriggerOperatorOrDefault): both close the same failure
// mode — an autonomous spawn whose daemon-client context carries no X-Operated-By
// header, so createBead's resolveAuditOperatorRequired fail-closes with
// HTTP 400 "operator could not be resolved" (ka-0000000302, mirroring
// ka-0000006023).
//
// A non-human value (empty, sentinel, jira:/gitlab: provider id, bare name) is
// dropped to "" so a misconfigured knob can never re-introduce the "stamped a
// non-human operator that fails closed downstream" bug — identical guard to
// TriggerDefaultOperatorFromEnv. When "", the dispatcher attributes solely from
// the step's own operated_by field (the pre-fix behavior, zero change).
//
// Returns the receiver for chaining. A nil dispatcher (nil-safe contract) is a
// no-op.
func (d *FormulaStepDispatcher) WithDefaultOperator(op string) *FormulaStepDispatcher {
	if d == nil {
		return d
	}
	op = strings.TrimSpace(op)
	if op != "" && !IsHumanOperator(op) {
		d.logger.Warn("step dispatch: ignoring non-human default operator",
			"value", op)
		op = ""
	}
	d.defaultOperator = op
	return d
}

// Register subscribes the dispatcher to the SSE topics that can unblock a step.
// The caller owns the stream's lifecycle; Register only attaches handlers.
//
// Two topics, because there are two ways a blocker stops blocking:
//   - beads.bead.closed — the common case, a step finishing.
//   - beads.bead.updated — a step going `partial`, which the ready-queue also
//     treats as terminal-for-progression. That transition emits an UPDATE, not a
//     close, so subscribing to closes alone would silently strand every pipeline
//     that uses partial to mean "done enough to proceed".
//
// The updated handler filters to terminal statuses before doing any work, so
// the fleet-wide update firehose costs one string compare per event.
func (d *FormulaStepDispatcher) Register(stream *SSEStream) {
	if d == nil || stream == nil {
		return
	}
	stream.On(stepDispatchTopic, func(ctx context.Context, data []byte) {
		bead := ParseBeadEvent(data)
		if bead == nil || bead.ID == "" {
			return
		}
		d.OnStepClosed(ctx, bead.ID)
	})
	stream.On(stepDispatchUpdateTopic, func(ctx context.Context, data []byte) {
		bead := ParseBeadEvent(data)
		if bead == nil || bead.ID == "" || !stepDispatchBlockerCleared(bead.Status) {
			return
		}
		d.OnStepClosed(ctx, bead.ID)
	})
	d.logger.Info("formula step dispatcher registered",
		"topics", []string{stepDispatchTopic, stepDispatchUpdateTopic})
}

// OnStepClosed reacts to closedID closing: every step bead blocked on it is
// re-evaluated for dispatch. Exported so the dispatcher can be driven directly
// by tests and by a future reconciler backfill without an SSE stream.
//
// Best-effort throughout: errors are logged, never propagated, so one bad bead
// cannot wedge the stream.
func (d *FormulaStepDispatcher) OnStepClosed(ctx context.Context, closedID string) {
	if d == nil || closedID == "" {
		return
	}

	// Who was blocked on this? GetReverseDependencies returns rows reading
	// "BeadID depends on closedID".
	dependents, err := d.daemon.GetReverseDependencies(ctx, closedID)
	if err != nil {
		d.recordDecline(declineListDepsFailed)
		d.logger.Warn("step dispatch: failed to list dependents",
			"closed_bead", closedID, "error", err)
		return
	}

	for _, dep := range dependents {
		if !stepDispatchIsBlockingDep(dep.Type) || dep.BeadID == "" {
			continue
		}
		d.considerStep(ctx, dep.BeadID, closedID)
	}
}

// BackfillReadySteps is the durable retry the SSE path cannot provide. It lists
// open, opted-in, un-dispatched formula step beads and re-considers each one,
// so a step whose dispatch was refused (spawn guard) or whose unblocking close
// event was missed (SSE outage, restart, rolling-update handoff) still gets an
// agent once its blockers clear and the guard budget refills.
//
// WHY THE EVENT PATH ISN'T ENOUGH. OnStepClosed only fires when a blocker
// closes. In a LINEAR step chain there is exactly one such event per step, and
// it does not repeat — so a transient refusal at that instant strands the step
// with no second chance (the FSM-472 failure class). This sweep is the second
// chance: it is time-driven, not event-driven.
//
// SAFE TO RUN REPEATEDLY. considerStep re-checks every gate (opt-in, role,
// dispatched_agent stamp, assignee, all-blockers-cleared) and the durable
// dispatched_agent stamp plus SpawnAgent's single-claim guard make a repeat
// dispatch a no-op. A candidate that is still blocked is simply skipped this
// pass and reconsidered next pass. Best-effort: a list error logs and returns.
//
// The server-side filter does the heavy narrowing — suggest_new_agent=true and
// status=open — so the candidate set is small and the per-bead considerStep
// round-trips stay bounded. closedID is reported as "backfill" in the spawn
// prompt/log since there is no single triggering close.
func (d *FormulaStepDispatcher) BackfillReadySteps(ctx context.Context) {
	if d == nil {
		return
	}

	res, err := d.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses: []string{"open"},
		Fields:   map[string]string{"suggest_new_agent": "true"},
		Limit:    stepDispatchBackfillLimit,
	})
	if err != nil {
		d.logger.Warn("step dispatch backfill: failed to list candidate steps", "error", err)
		return
	}
	if res == nil {
		return
	}
	if len(res.Beads) >= stepDispatchBackfillLimit {
		d.logger.Warn("step dispatch backfill: candidate set hit the limit — tail not swept this pass",
			"limit", stepDispatchBackfillLimit)
	}

	considered := 0
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		// Cheap pre-filter: a step already carrying the durable stamp or an
		// assignee is settled — skip the considerStep round-trips. considerStep
		// re-checks both anyway, so this is purely to save daemon calls on the
		// common already-handled case.
		if b.Fields != nil && strings.TrimSpace(b.Fields["dispatched_agent"]) != "" {
			continue
		}
		if strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		considered++
		d.considerStep(ctx, b.ID, stepDispatchBackfillSentinel)
	}
	d.logger.Debug("step dispatch backfill: swept ready steps",
		"candidates", len(res.Beads), "considered", considered)
}

// stepDispatchIsBlockingDep reports whether a dep type is a WAIT edge.
//
// Mirrors the daemon ready-queue's blockingDepTypes
// (kbeads/internal/store/postgres/queries.go): "blocks", "blocked-by",
// "depends-on". Deliberately excludes parent-child — a step's edge to its
// still-open molecule is a hierarchy edge, not a wait edge, and treating it as
// blocking would mean no step ever dispatches.
//
// Kept in lockstep with the ready-queue on purpose: this dispatcher's whole
// premise is "the bead just became ready", so any divergence would spawn for
// beads the fleet does not consider workable, or skip ones it does.
func stepDispatchIsBlockingDep(depType string) bool {
	switch depType {
	case "blocks", "blocked-by", "depends-on":
		return true
	}
	return false
}

// stepDispatchBlockerCleared reports whether a blocker's status no longer holds
// the dependent back. Mirrors model.Status.IsTerminalForProgression: CLOSED or
// PARTIAL. `partial` matters — it is how a deliberately-incomplete step signals
// "done enough to proceed", and treating it as still-blocking would strand
// exactly the pipelines that use it.
func stepDispatchBlockerCleared(status string) bool {
	return status == "closed" || status == "partial"
}

// considerStep evaluates one candidate step bead and dispatches if every gate
// passes. Each early return is a reason NOT to spawn, logged at debug so a
// "why didn't my step fire?" investigation has a trail.
func (d *FormulaStepDispatcher) considerStep(ctx context.Context, stepID, closedID string) {
	// Cheap in-process dedup first — avoids a daemon round-trip on the common
	// case of an SSE replay.
	if d.alreadyDispatched(stepID) {
		d.recordDecline(declineAlreadyDispatchedProcess)
		d.logger.Debug("step dispatch: skipping, already dispatched this process",
			"step", stepID)
		return
	}

	step, err := d.daemon.GetBead(ctx, stepID)
	if err != nil || step == nil {
		d.recordDecline(declineHydrateFailed)
		d.logger.Warn("step dispatch: failed to hydrate step bead",
			"step", stepID, "error", err)
		return
	}

	reason, code, ok := stepDispatchEligible(step)
	if !ok {
		d.recordDecline(code)
		d.logger.Debug("step dispatch: step not eligible",
			"step", stepID, "reason", reason)
		return
	}

	// The expensive gate last: are ALL blocking deps closed, or just this one?
	// A step blocked on three siblings must not fire on the first close.
	blocked, ready, err := d.stillBlocked(ctx, stepID)
	if err != nil {
		d.recordDecline(declineBlockCheckFailed)
		d.logger.Warn("step dispatch: failed to check blocking deps",
			"step", stepID, "error", err)
		return
	}
	if blocked != "" {
		d.recordDecline(declineStillBlocked)
		d.logger.Debug("step dispatch: step still blocked",
			"step", stepID, "blocked_by", blocked)
		return
	}

	// ready carries the instant this step became workable — the by-product of
	// the blocking-dep hydration just done, handed to dispatch so a successful
	// spawn can close the readiness -> spawn interval (ka-0000006705).
	d.dispatch(ctx, step, closedID, ready)
}

// stepDispatchEligible reports whether a hydrated bead is a formula step that
// opted into its own agent and is not already claimed. When ok is false it
// returns a human-readable reason (for the debug log) AND a bounded reason code
// from the declineReason* enum (for the decline counter). When ok is true both
// strings are empty.
//
// Split out as a pure function so the (many) gates are testable without a
// daemon.
func stepDispatchEligible(step *beadsapi.BeadDetail) (reason, code string, ok bool) {
	if step.Status != "open" {
		return "status is " + step.Status + ", not open", declineNotOpen, false
	}
	if step.Fields == nil {
		return "no fields (not a poured formula step)", declineNoFields, false
	}
	if step.Fields["source_step_id"] == "" {
		return "no source_step_id (not a poured formula step)", declineNotFormulaStep, false
	}
	if !stepDispatchFieldTrue(step.Fields["suggest_new_agent"]) {
		return "suggest_new_agent not set — formula did not opt this step in", declineNotOptedIn, false
	}
	if stepDispatchRole(step) == "" {
		return "no step role — nothing to dispatch to", declineNoRole, false
	}
	// Durable idempotency: a prior dispatch (this process or a previous one)
	// stamped the agent name here.
	if a := strings.TrimSpace(step.Fields["dispatched_agent"]); a != "" {
		return "already dispatched to " + a, declineAlreadyStamped, false
	}
	// Someone is already on it — a live driver walking the chain inline, or a
	// human. Don't second-guess them.
	if strings.TrimSpace(step.Assignee) != "" {
		return "already assigned to " + step.Assignee, declineAlreadyAssigned, false
	}
	return "", "", true
}

// stepDispatchRole resolves the step's dispatch role: the step_role field this
// change introduced, falling back to the `role:<x>` label that formula pours
// have always stamped. The label fallback matters for steps poured BEFORE this
// change — a live molecule mid-flight during a deploy still dispatches — and
// for any writer that stamps the label without the field.
func stepDispatchRole(step *beadsapi.BeadDetail) string {
	if step.Fields != nil {
		if r := strings.TrimSpace(step.Fields["step_role"]); r != "" {
			return r
		}
	}
	for _, l := range step.Labels {
		if r, ok := strings.CutPrefix(l, "role:"); ok {
			if r = strings.TrimSpace(r); r != "" {
				return r
			}
		}
	}
	return ""
}

// stepDispatchFieldTrue reports whether a field value means true. Fields cross
// the wire as strings, and the daemon has historically rendered booleans as
// both "true" and "1" depending on write path, so accept both rather than
// silently declining to dispatch on a spelling difference.
func stepDispatchFieldTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// stepReadiness is when a step became workable — the left edge of the dispatch
// latency interval (ka-0000006705). It is a by-product of the blocking-dep
// hydration stillBlocked already does, so establishing it costs no extra daemon
// round-trip.
//
// Exactly one of the two fields is meaningful: a non-zero `at` is a resolved
// readiness instant; a zero `at` means readiness is UNKNOWN and `reason` says
// why, so the spawn can be accounted for on the unmeasured counter instead of
// silently vanishing from the histogram's _count.
type stepReadiness struct {
	// at is the daemon-side instant the LAST blocking dep cleared — the moment
	// the step became dispatchable regardless of whether any process noticed.
	at time.Time
	// reason is a bounded latencyUnmeasured* code, set only when at is zero.
	reason string
}

// stillBlocked returns the ID of the first open blocking dep, or "" if the step
// is fully unblocked. A dep whose bead cannot be hydrated is treated as still
// blocking: fail CLOSED here, because spawning early on a half-read graph puts
// an agent on work whose inputs do not exist yet.
//
// On the fully-unblocked path it also returns the step's readiness instant:
// max(cleared-at) over the blocking deps it just hydrated. The MAX is the right
// reduction — a step blocked on three siblings becomes workable when the LAST
// one clears, not the first — and it makes the readiness edge identical on both
// dispatch paths, including a backfill sweep that has no triggering close event
// to read a timestamp from.
//
// Readiness is reported UNKNOWN rather than guessed in the two cases where a max
// would be a fiction: a step with no blocking dep at all (nothing to measure
// from), and a cleared blocker carrying no usable timestamp (the max would
// silently drop a dep that may well have been the last to clear).
func (d *FormulaStepDispatcher) stillBlocked(ctx context.Context, stepID string) (string, stepReadiness, error) {
	deps, err := d.daemon.GetDependencies(ctx, stepID)
	if err != nil {
		return "", stepReadiness{}, fmt.Errorf("getting dependencies: %w", err)
	}
	var (
		readyAt      time.Time
		blockingDeps int
		timeMissing  bool
	)
	for _, dep := range deps {
		if !stepDispatchIsBlockingDep(dep.Type) || dep.DependsOnID == "" {
			continue
		}
		blockingDeps++
		blocker, err := d.daemon.GetBead(ctx, dep.DependsOnID)
		if err != nil || blocker == nil {
			d.logger.Warn("step dispatch: treating unreadable blocker as still-blocking",
				"step", stepID, "blocker", dep.DependsOnID, "error", err)
			return dep.DependsOnID, stepReadiness{}, nil
		}
		if !stepDispatchBlockerCleared(blocker.Status) {
			return dep.DependsOnID, stepReadiness{}, nil
		}
		clearedAt := stepDispatchBlockerClearedAt(blocker)
		if clearedAt.IsZero() {
			timeMissing = true
			continue
		}
		if clearedAt.After(readyAt) {
			readyAt = clearedAt
		}
	}
	switch {
	case blockingDeps == 0:
		return "", stepReadiness{reason: latencyUnmeasuredNoBlockingDeps}, nil
	case timeMissing:
		return "", stepReadiness{reason: latencyUnmeasuredBlockerTimeMissing}, nil
	default:
		return "", stepReadiness{at: readyAt}, nil
	}
}

// stepDispatchBlockerClearedAt returns the instant a cleared blocker stopped
// blocking, or the zero time when the bead carries no usable timestamp.
//
// closed_at is the precise answer for the common case. The `partial` clearance
// (stepDispatchBlockerCleared) is NOT a close, so it leaves closed_at empty and
// updated_at is the best available stand-in — accurate to the transition unless
// the bead was edited afterwards, which for a terminal step is rare and only
// ever makes the measured latency look SHORTER, never longer. Preferring a
// slight understatement over dropping the sample keeps the recovery-path tail
// visible, which is the tail an operator is looking for.
func stepDispatchBlockerClearedAt(blocker *beadsapi.BeadDetail) time.Time {
	if blocker == nil {
		return time.Time{}
	}
	if ts := stepDispatchParseTime(blocker.ClosedAt); !ts.IsZero() {
		return ts
	}
	return blocker.UpdatedAt
}

// stepDispatchTimeFormats mirrors beadsapi's (unexported) updatedAtFormats so a
// closed_at string is parsed exactly the way the client parses created_at /
// updated_at — the daemon emits RFC3339, the bare forms cover older rows.
var stepDispatchTimeFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
}

// stepDispatchParseTime parses a daemon timestamp string, returning the zero
// time for empty or unparseable input.
func stepDispatchParseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range stepDispatchTimeFormats {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// dispatch spawns the role-matched agent for a step that cleared every gate.
//
// Field-stamp ordering is deliberate: `dispatched_agent` is written BEFORE the
// spawn. If the stamp fails we abort rather than spawn, because an unstamped
// spawn is one a restart would repeat. If the SPAWN then fails we clear the
// stamp so a later close-event or a reconciler can retry — a stamp with no
// agent behind it is worse than no stamp, since it silently suppresses every
// future attempt.
func (d *FormulaStepDispatcher) dispatch(ctx context.Context, step *beadsapi.BeadDetail, closedID string, ready stepReadiness) {
	role := stepDispatchRole(step)
	project := strings.TrimSpace(step.Fields["step_project"])
	if project == "" {
		project = stepDispatchProjectFromLabels(step.Labels)
	}

	// SOFT role validation (fail-open): warn + count when the role has no
	// backing config, but never block the spawn. Done here, just before the
	// point of no return, so the warning names the exact step that is about to
	// mint a misconfigured pod.
	d.warnUnbackedRole(ctx, step.ID, role)

	agentName := generateAgentName(project, step.Title)

	// Mark in-process FIRST so a concurrent event for the same step (two
	// siblings closing at once) cannot double-spawn while we're mid-flight.
	if !d.claim(step.ID) {
		d.recordDecline(declineLostRace)
		d.logger.Debug("step dispatch: lost the in-process race, another goroutine has it",
			"step", step.ID)
		return
	}

	// Fleet spawn rate limit / circuit breaker — the same gate every other
	// spawn path passes through. Checked AFTER the in-process claim so a
	// rate-limited step is releasable and retryable, and BEFORE the durable
	// stamp so a refusal leaves no trace to clean up.
	//
	// On refusal we release the in-process claim and leave the step un-stamped,
	// so it stays a live backfill candidate. The event path alone does NOT
	// recover this: in a LINEAR step chain the only close event is the one
	// blocker that just cleared, and it will not fire again — so "retry on a
	// later close event" strands a rate-limited step exactly the way FSM-472
	// stranded the fix step. BackfillReadySteps is the durable retry: it
	// re-scans un-dispatched, fully-unblocked opted-in steps on a timer and
	// re-considers them once the guard budget refills (ka-0000006285).
	if d.guard != nil {
		if ok, reason := d.guard.Allow(); !ok {
			d.release(step.ID)
			d.recordDecline(declineGuardRefused)
			d.logger.Warn("step dispatch: spawn guard refused — step left for the backfill sweep to retry",
				"step", step.ID, "role", role, "reason", reason)
			return
		}
	}

	if err := d.daemon.UpdateBeadFields(ctx, step.ID, map[string]string{
		"dispatched_agent": agentName,
		"dispatched_by":    "formula-step-dispatcher",
	}); err != nil {
		d.release(step.ID)
		d.recordDecline(declineStampFailed)
		d.logger.Warn("step dispatch: failed to stamp dispatched_agent — not spawning",
			"step", step.ID, "agent_name", agentName, "error", err)
		return
	}

	prompt := stepDispatchPrompt(step, closedID)
	extraFields := map[string]string{
		"spawned_by":     "formula-step-dispatcher",
		"source":         "formula_step",
		"molecule_id":    step.Fields["molecule_id"],
		"source_formula": step.Fields["source_formula"],
		"source_step_id": step.Fields["source_step_id"],
		// The step is single-claim work: this dispatcher and a still-live
		// molecule driver must not both put an agent on it.
		"single_claim_task": "1",
	}
	if op := strings.TrimSpace(step.Fields["operated_by"]); op != "" {
		extraFields["operated_by"] = op
	}

	// Attribute the spawn to a HUMAN operator on the daemon-client CONTEXT, not
	// just as the operated_by bead field above.
	//
	// THE BUG (ka-0000000302). The daemon's createBead fail-closes on every
	// audit-bearing write when no human operator can be resolved
	// (resolveAuditOperatorRequired, R2 ka-0000005772). It resolves the operator
	// from the X-Operated-By REQUEST HEADER — carried on ctx via
	// beadsapi.WithOperator — NOT from the operated_by FIELD in the bead payload.
	// So stamping operated_by in extraFields (above) is necessary for the spawned
	// pod to inherit BEADS_OPERATED_BY, but it does NOTHING for the create call's
	// own authorization: SpawnAgent → CreateBead sends no X-Operated-By and the
	// daemon 400s "operator could not be resolved". The dispatched_agent stamp
	// just above SUCCEEDS because updateBead does not require an operator — which
	// is exactly why the failure only surfaces at the spawn, and why the step
	// then retries forever (the stamp rolls back, the sweep re-finds it).
	//
	// FIX. Set the operator on ctx before the spawn, mirroring the trigger
	// engine's ctxWithTriggerOperatorOrDefault (triggers.go): prefer the step's
	// own operated_by when it is human-shaped, else the configured default
	// operator, else leave ctx untouched (the daemon then applies its own
	// spawn-chain / fleet attribution exactly as before). A non-human step field
	// never poisons the header — IsHumanOperator gates it — so a sentinel
	// operated_by cannot re-trigger the sanitizer-reject path.
	ctx = d.ctxWithStepOperator(ctx, step)

	agentID, err := d.daemon.SpawnAgent(ctx, agentName, project, step.ID, role, prompt, extraFields)
	if err != nil {
		// Roll the stamp back so this step is retryable. Best-effort: if the
		// rollback itself fails the step is stuck, which we log loudly because
		// it needs a human.
		d.release(step.ID)
		d.recordDecline(declineSpawnFailed)
		if clearErr := d.daemon.UpdateBeadFields(ctx, step.ID, map[string]string{
			"dispatched_agent": "",
			"dispatched_by":    "",
		}); clearErr != nil {
			d.logger.Error("step dispatch: spawn FAILED and stamp rollback FAILED — step is wedged until a human clears dispatched_agent",
				"step", step.ID, "agent_name", agentName, "spawn_error", err, "rollback_error", clearErr)
		} else {
			d.logger.Warn("step dispatch: spawn failed, stamp rolled back — step will retry on the next close event",
				"step", step.ID, "agent_name", agentName, "error", err)
		}
		return
	}

	// Record only on success — a refused or failed spawn must not consume
	// budget (matches the Allow/Record contract every other spawn path uses).
	if d.guard != nil {
		d.guard.Record()
	}

	// The spawn counter, faceted by the same identifiers the log line carries.
	// This is the affirmative half of the observability pair: "did the
	// dispatcher spawn anything today, and for which lane?" It reconciles
	// against count(beads with dispatched_by=formula-step-dispatcher), the field
	// stamped just above (advice ka-0000005924).
	d.recordSpawn(role, step.Fields["source_formula"], project)

	// Close the readiness -> spawn interval on the latency histogram, or account
	// for it as unmeasurable (ka-0000006705). Recorded only on the SUCCESS path,
	// after the agent provably exists, so a refused or failed dispatch never
	// contributes a sample — the histogram's _count then reconciles against the
	// spawn counter (advice ka-0000005924), and "how long did a ready step wait
	// for its agent?" is a p50/p95 read rather than a log dive.
	path := stepDispatchPathFor(closedID)
	d.recordDispatchLatency(path, ready)

	d.logger.Info("step dispatch: spawned role-matched agent for ready formula step",
		"step", step.ID, "step_id", step.Fields["source_step_id"],
		"molecule", step.Fields["molecule_id"], "formula", step.Fields["source_formula"],
		"role", role, "project", project,
		"agent", agentID, "agent_name", agentName, "unblocked_by", closedID)
}

// ctxWithStepOperator returns ctx carrying the human operator the spawn should
// be attributed to via the X-Operated-By header (beadsapi.WithOperator). This is
// what makes the daemon's createBead accept the agent-bead write — the field
// alone does not (see the call site in dispatch()).
//
// Resolution order, mirroring ctxWithTriggerOperatorOrDefault (triggers.go):
//  1. An operator ALREADY on ctx wins — never override a caller's explicit
//     attribution (matches ctxWithTriggerOperator's first guard).
//  2. The step bead's own operated_by field, when human-shaped. This is the
//     common case: instantiateFormula propagates the pour's operated_by onto
//     every step, so a real §7→§8 pour carries slack:U… here.
//  3. The configured default operator (WithDefaultOperator), when set.
//  4. Otherwise ctx is returned UNCHANGED, so the daemon falls back to its own
//     spawn-chain / fleet auto-attribution exactly as it did pre-fix — a
//     deployment that wires neither a step operated_by nor a default operator
//     sees zero behavior change (it just keeps 400ing until one is supplied,
//     which is strictly no worse than today).
//
// The human-shape gate on the step field is deliberate: a sentinel or
// provider-only operated_by (system:*, jira:<id>) must NOT be placed on the
// header, or the daemon's sanitizeOperatedByHeader rejects it and drops audit
// records (ka-0000004577). IsHumanOperator is the exact mirror of the daemon's
// isHumanOperator, so a value this accepts is a value the daemon accepts.
func (d *FormulaStepDispatcher) ctxWithStepOperator(ctx context.Context, step *beadsapi.BeadDetail) context.Context {
	if beadsapi.OperatorFromContext(ctx) != "" {
		return ctx
	}
	if step != nil {
		if op := strings.TrimSpace(step.Fields["operated_by"]); IsHumanOperator(op) {
			return beadsapi.WithOperator(ctx, op)
		}
	}
	if d.defaultOperator != "" {
		return beadsapi.WithOperator(ctx, d.defaultOperator)
	}
	return ctx
}

// warnUnbackedRole emits one WARN and a bounded decline-reason facet when the
// step's dispatch role is neither a builtin nor backed by an active type:role
// bead. It is purely a SIGNAL — it does not (and must not) prevent the spawn.
//
// Fail-open in three ways, all deliberate: (1) no lister wired -> silent (role
// validation is opt-in); (2) the role IS backed -> silent; (3) listing the role
// catalog errors -> silent (a role-listing hiccup must never make a dispatch
// louder than a real config gap, and it must never gate a spawn — same contract
// as roleWarning). Only a role we could positively confirm is unbacked warns.
func (d *FormulaStepDispatcher) warnUnbackedRole(ctx context.Context, stepID, role string) {
	if d.roleLister == nil {
		return
	}
	role = strings.TrimSpace(role)
	if role == "" || stepDispatchBuiltinRoles[role] {
		return
	}
	roles, ok := d.roleCatalog(ctx)
	if !ok {
		return // fail-open on a listing error — never louder than a real gap
	}
	if _, backed := roles[role]; backed {
		return
	}
	d.recordDecline(declineRoleUnbacked)
	d.logger.Warn("step dispatch: dispatching a role with no backing type:role bead — the pod gets no role overrides (likely a typo'd step role)",
		"step", stepID, "role", role)
}

// roleCatalog returns the active type:role catalog, memoized for stepRoleCacheTTL
// so a backfill burst does not re-list per dispatch. ok is false only when the
// catalog is unknown (never listed AND this list errored); a previously-cached
// catalog is reused across a transient list error rather than forcing a fail-open
// gap. Holds mu for the whole check-and-refresh so concurrent dispatchers share
// one refresh.
func (d *FormulaStepDispatcher) roleCatalog(ctx context.Context) (map[string]beadsapi.RoleInfo, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.roleCache != nil && d.now().Sub(d.roleCacheAt) < stepRoleCacheTTL {
		return d.roleCache, true
	}
	roles, err := d.roleLister.ListRoleBeads(ctx)
	if err != nil {
		// Reuse a stale-but-valid catalog if we have one; otherwise report
		// unknown so the caller fails open.
		if d.roleCache != nil {
			return d.roleCache, true
		}
		d.logger.Debug("step dispatch: role catalog unavailable — skipping role validation this dispatch",
			"error", err)
		return nil, false
	}
	d.roleCache = roles
	d.roleCacheAt = d.now()
	return roles, true
}

// stepDispatchPrompt builds the spawn prompt. The step's own description is the
// work contract (formulas write full phase instructions there), so the prompt
// points at the bead rather than duplicating it — a 6KB description inlined
// into BOAT_PROMPT would just be read twice.
func stepDispatchPrompt(step *beadsapi.BeadDetail, closedID string) string {
	// A backfill dispatch has no single triggering close — its blockers cleared
	// earlier and the sweep found it ready — so the prompt must not claim a bead
	// named "backfill" closed.
	backfill := closedID == stepDispatchBackfillSentinel

	var sb strings.Builder
	if backfill {
		sb.WriteString("You were spawned because a formula step is ready: all its blocking steps have completed.\n\n")
	} else {
		sb.WriteString("You were spawned because a formula step became ready: its blocking step ")
		sb.WriteString(closedID)
		sb.WriteString(" closed.\n\n")
	}
	sb.WriteString("Your work is step `")
	sb.WriteString(step.Fields["source_step_id"])
	sb.WriteString("` of formula ")
	sb.WriteString(step.Fields["source_formula"])
	sb.WriteString(", tracked on bead ")
	sb.WriteString(step.ID)
	sb.WriteString(".\n\n")
	sb.WriteString("Read that bead (`kd show ")
	sb.WriteString(step.ID)
	sb.WriteString("`) — its description is the full contract for this step, including the gate you must clear. ")
	if !backfill {
		sb.WriteString("Read the upstream step ")
		sb.WriteString(closedID)
		sb.WriteString(" for the inputs it emitted. ")
	}
	if mol := step.Fields["molecule_id"]; mol != "" {
		sb.WriteString("The molecule ")
		sb.WriteString(mol)
		sb.WriteString(" holds the applied vars and sibling steps. ")
	}
	sb.WriteString("Claim the bead, do the work, close it when the gate passes.")
	return sb.String()
}

// stepDispatchProjectFromLabels recovers the project from a `project:<x>` label
// when the step bead carries no explicit step_project field (steps poured
// before this change, or a formula that inherits the molecule's project).
func stepDispatchProjectFromLabels(labels []string) string {
	for _, l := range labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// claim records stepID as dispatched, returning false if it was already
// claimed within the TTL. Also opportunistically evicts expired entries so the
// map stays bounded without a separate sweeper goroutine.
func (d *FormulaStepDispatcher) claim(stepID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if at, ok := d.dispatched[stepID]; ok && now.Sub(at) < stepDispatchDedupTTL {
		return false
	}
	for id, at := range d.dispatched {
		if now.Sub(at) >= stepDispatchDedupTTL {
			delete(d.dispatched, id)
		}
	}
	d.dispatched[stepID] = now
	return true
}

// release drops a claim so a failed dispatch can be retried.
func (d *FormulaStepDispatcher) release(stepID string) {
	d.mu.Lock()
	delete(d.dispatched, stepID)
	d.mu.Unlock()
}

// alreadyDispatched reports whether stepID holds a live claim.
func (d *FormulaStepDispatcher) alreadyDispatched(stepID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	at, ok := d.dispatched[stepID]
	return ok && d.now().Sub(at) < stepDispatchDedupTTL
}
