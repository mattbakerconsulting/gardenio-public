// Package planagent is the ONE shared engine for a /plans plan's bound agent:
// resolving the plan's single plan-gardener agent, deciding live-nudge vs
// wake vs cold-spawn for an operator (or reactive) message, minting/reviving
// that agent with S3-session continuity, and the closed-plan backstop that
// refuses to re-drive a finished plan's gardener.
//
// WHY THIS EXISTS. The primitives here were born inside cmd/beads-viewer
// (package main): promptPlanAssistant / spawnPlanAssistant / wakePlanAssistant
// plus the errPlanClosedNoWake backstop. That made them reachable only from the
// viewer's HTTP handlers. The P3 reactive-collab relocation (epic ka-0000002170)
// moves nudge/wake/spawn onto ONE debounced controller-side reactor that
// consumes a daemon step-added signal and drives the plan's gardener via its OWN
// coopmux client + AgentSpawn ownership — so the decision engine can no longer
// live in package main. Lifting it here (a sibling of internal/plangardener, the
// gardener-spec builder both binders share) lets the viewer AND the controller
// reactor call byte-identical spawn/wake/prompt logic.
//
// WHAT THIS PACKAGE OWNS vs what the caller keeps. This engine is the pure
// resolve → decide → spawn/wake/nudge core. The per-plan DEBOUNCE (coalescing a
// burst of step-adds into one nudge) and the SSE SUBSCRIPTION that observes
// step-added events stay with the caller: the debounce is now durable — keyed on
// the plan epic's fields.last_nudge_at so it survives a restart and coordinates
// concurrent writers (ka-0000002855) — and the reactor owns its own stream. The
// engine also does NOT own the plan-chat transcript minting or any UI copy —
// those are viewer render concerns.
//
// AUDIT-ATTRIBUTION CALLER CONTRACT (ka-0000001801). Every bead WRITE the engine
// makes on the Spawn / Wake path (CreateBead, AddLabel, AddDependency,
// UpdateBeadFieldsTyped, UpdateBead) is attributed ENTIRELY from the caller's
// ctx — the engine deliberately does NOT stamp an operator of its own. The
// beadsapi client's sendOnce reads attribution from ctx in three channels:
// beadsapi.WithEdgeAuthOperator (the HMAC envelope), beadsapi.WithOperator
// (X-Operated-By, the human), and beadsapi.WithSystemOperator (X-System-Operator,
// a true machine sink). Since the controller REMOVED its client-wide
// system-operator default (epic ka-0000004835: "a missing human is a loud
// refusal, not a silent fallback"), that ctx is the SOLE attribution channel for
// both callers — the viewer stamps WithOperator(canonicalOIDC(email)) in its
// HTTP handler, and the controller reactor must stamp WithOperator /
// WithSystemOperator likewise before it calls Spawn/Prompt. A write entered with
// NO attribution on ctx fail-closes at the daemon's audit resolver with HTTP 400
// "operator could not be resolved" (R2, ka-0000005772), which the viewer surfaces
// as a generic 502. The engine does NOT auto-stamp the free-text `actor` argument
// as a fallback: `actor` is a bare-ish log/display string (a mixed-case email,
// or an oidc-stripped name from planReactorActor) that is NOT guaranteed to be in
// the canonical human-operator shape the daemon sanitizer accepts — stamping it
// would fork a second operator row or be rejected outright. Instead the engine
// emits a diagnostic warn (requireAuditAttribution) when a write path is entered
// with an unstamped ctx, so the fail-closed 400 is traceable to "the caller
// forgot to stamp" rather than a mystery upstream error.
//
// A plan's agent is an ordinary `agent` bead in the fable+runsc-pinned
// plan-gardener role (epic ka-0000000081 merged the former plan-assistant role
// into it), bound to the plan epic by a fields.plan_id stamp (the reverse-lookup
// key) AND an `assigned` dependency edge (the load-bearing graph edge the
// constellation/rollup read).
package planagent

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// SpawnRole is the unified, first-class plan-agent role (epic ka-0000000081;
// role bead ka-0000003712): a single fable+runsc-pinned agent that is BOTH the
// plan's watchdog gardener and its interactive agent. Every plan agent bead —
// spawned by Engine.Spawn here or by `gb plan gardener`'s trigger — carries this
// role, and the surfaces resolve the plan's agent by it alongside
// fields.plan_id. Kept in lockstep with plangardener.SpawnRole so the gardener's
// iterations and the interactive agent are one lineage.
const SpawnRole = "plan-gardener"

// RoleLabel is stamped on every plan agent bead and is the sole role label the
// resolver keys on (alongside fields.plan_id). The legacy role:plan-assistant
// resolution alias was hard-removed (operator decision ka-0000007411); a
// closed/historical plan whose sole agent was a pre-rename plan-assistant
// iteration renders "no agent", the accepted tradeoff.
const RoleLabel = "role:" + SpawnRole

// PlanIDField is the agent-bead field that binds an agent to its plan epic. It
// is the reverse-lookup key (plan -> agent), mirroring how a thread agent
// carries slack_thread_ts. task_id is also set (to the same epic) so the
// agent's activity lane + rollups treat the plan as its task, but plan_id is the
// canonical, purpose-named binding.
const PlanIDField = "plan_id"

// PlanLabel is the /plans marker label on the plan epic itself (mirrors
// cmd/beads-viewer's planLabel + plangardener.PlanLabel).
const PlanLabel = "plan"

// queuedState is the agent_state the controller stamps when it DECLINED a spawn
// because the project account pool is fully exhausted (cmd/controller SpawnBlocked
// path). A woken-but-deferred agent, distinct from an idle parked one
// (ka-0000003525).
const queuedState = "queued"

// staleThreshold is how long after its last update a plan agent bead is still
// considered live. Mirrors cmd/beads-viewer's agentStaleThreshold + the /agents
// isAlive predicate so the engine's liveness verdict matches the surfaces.
const staleThreshold = time.Hour

// aliveStates is the agent_state bucket that counts as "alive" for a plan agent
// — the same set cmd/beads-viewer's agentStateAlive and the /agents isAlive
// predicate use. A live agent can be nudged directly; a non-live one is parked
// (frozen or stopped) and its next message wakes it via the spawn/thaw path.
var aliveStates = map[string]struct{}{"working": {}, "spawning": {}, "yielding": {}}

// ErrPlanClosedNoWake is returned by Spawn when it refuses to spawn or wake a
// plan gardener because the plan epic is already CLOSED. It is the backstop for
// the reactive add-step / dispatch replay loop (ka-0000000753): a closed plan
// kept re-driving its gardener (each respawn replaying the stale original
// add-step message) into a no-op loop. Every spawn/wake of the plan gardener
// funnels through Spawn, so guarding here stops the loop regardless of which
// caller triggered it. Prompt maps this to a graceful no-op delivery (no work is
// lost — the plan is done), never a hard error.
var ErrPlanClosedNoWake = errPlanClosed{}

type errPlanClosed struct{}

func (errPlanClosed) Error() string {
	return "plan is closed: not spawning or waking its gardener"
}

// DaemonClient is the daemon surface the engine needs — a subset of the beads
// HTTP client. cmd/beads-viewer's beadAPI and the controller reactor's daemon
// client both satisfy it, so the same engine drives both.
type DaemonClient interface {
	GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddLabel(ctx context.Context, beadID, label string) error
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
}

// NudgeClient is the coopmux surface Prompt needs to deliver a message to a
// LIVE plan agent sub-second. cmd/beads-viewer's screenClient and the controller
// reactor's own coopmux client both satisfy it. A nil NudgeClient (or a Disabled
// one) means "no coopmux in this deployment" — Prompt degrades to the spawn/thaw
// path so the message is never lost.
type NudgeClient interface {
	Disabled() bool
	Nudge(ctx context.Context, sessionID, message string) error
}

// NameGen mints a fresh, unique agent name for a first-contact cold spawn. The
// caller injects its own generator (cmd/beads-viewer's generateAgentName) so the
// engine does not carry a crypto/rand dependency of its own and both callers
// share one naming scheme.
type NameGen func(project, prompt string) (string, error)

// Config parameterizes NewEngine. Daemon and NameGen are required; Nudge may be
// nil (no coopmux — Prompt degrades to spawn/thaw); Logger and Now default when
// nil.
type Config struct {
	Daemon  DaemonClient
	Nudge   NudgeClient
	NameGen NameGen
	Logger  *slog.Logger
	Now     func() time.Time
}

// Engine is the shared plan-agent resolve/spawn/wake/prompt core. Construct it
// with NewEngine. It is a lightweight value (interface refs + funcs) — a caller
// with request-scoped clients may build one per call.
type Engine struct {
	daemon  DaemonClient
	nudge   NudgeClient
	nameGen NameGen
	logger  *slog.Logger
	now     func() time.Time
}

// NewEngine builds an Engine, defaulting Logger to slog.Default() and Now to
// time.Now when the Config leaves them nil.
func NewEngine(cfg Config) *Engine {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Engine{
		daemon:  cfg.Daemon,
		nudge:   cfg.Nudge,
		nameGen: cfg.NameGen,
		logger:  logger,
		now:     now,
	}
}

// Resolve locates the plan epic's bound agent bead, if any. It resolves the
// binding two ways and unions them so the lookup is robust to either edge being
// written first: (a) the `assigned` reverse-dependency edge (the graph edge
// Spawn writes), and (b) any agent bead carrying fields.plan_id == epicID (the
// purpose-named binding). Returns nil when the plan has no agent yet.
// Best-effort: a daemon error degrades to "no agent".
func (e *Engine) Resolve(ctx context.Context, epicID string) *beadsapi.BeadDetail {
	candidates := make([]string, 0, 4)
	seen := make(map[string]struct{})
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		candidates = append(candidates, id)
	}

	// (a) assigned reverse-dep edges (agent -> epic).
	if revDeps, err := e.daemon.GetReverseDependencies(ctx, epicID); err == nil {
		for _, d := range revDeps {
			if d.Type == "assigned" && d.BeadID != "" {
				add(d.BeadID)
			}
		}
	} else {
		e.logger.Debug("plan agent: reverse deps", "epic", epicID, "error", err)
	}

	// (b) fields.plan_id binding — the purpose-named reverse lookup, scoped to
	// the plan-gardener role label. A daemon that can't filter on an arbitrary
	// field just returns nothing here; the assigned-edge path above still finds
	// the agent.
	if res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"agent"},
		Labels: []string{RoleLabel},
		Fields: map[string]string{PlanIDField: epicID},
		Limit:  10,
	}); err == nil && res != nil {
		for _, b := range res.Beads {
			if b != nil {
				add(b.ID)
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}
	beads, err := e.daemon.BatchGetBeads(ctx, candidates)
	if err != nil {
		e.logger.Debug("plan agent: batch get", "epic", epicID, "error", err)
		return nil
	}
	// Prefer a live agent; else the most recently updated one (freshest session
	// to resume). A plan should have at most one agent, but a respawn can briefly
	// leave a stale terminal bead beside the new one.
	var best *beadsapi.BeadDetail
	for _, id := range candidates {
		b := beads[id]
		if b == nil || b.Type != "agent" || !IsPlanAssistantBead(b, epicID) {
			continue
		}
		best = e.PickBetter(best, b)
	}
	return best
}

// PickBetter returns the agent bead that should represent a plan when more than
// one binds to it: a live one beats a parked one, and among same-liveness beads
// the freshest (latest UpdatedAt) wins. Exposed so a bulk index join can apply
// the identical preference the single-plan Resolve does, keeping the index chip
// and the detail lane from disagreeing about which agent is "the" one.
func (e *Engine) PickBetter(cur, cand *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	if cur == nil {
		return cand
	}
	if cand == nil {
		return cur
	}
	curLive, candLive := e.IsLive(cur), e.IsLive(cand)
	if curLive != candLive {
		if candLive {
			return cand
		}
		return cur
	}
	if cand.UpdatedAt.After(cur.UpdatedAt) {
		return cand
	}
	return cur
}

// IsLive mirrors the /agents liveness predicate (agent_link.go isAlive):
// agent_state in the alive bucket AND recently updated (or no timestamp yet). A
// live agent can be nudged directly; a non-live one is parked (frozen or
// stopped) and its next message wakes it via the spawn/thaw path.
func (e *Engine) IsLive(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if _, ok := aliveStates[b.Fields["agent_state"]]; !ok {
		return false
	}
	if b.UpdatedAt.IsZero() {
		return true
	}
	return e.now().Sub(b.UpdatedAt) < staleThreshold
}

// IsQueued reports whether the plan's bound agent was WOKEN but its spawn
// DECLINED because the project account pool is fully exhausted — the controller
// stamps agent_state=queued on that path and files no escalation, so the
// reconciler silently re-attempts when a rate-limit window frees up. This is a
// distinct, non-live state that must NOT collapse to the idle "parked" chip:
// parked is an agent nobody woke, queued is a woken one waiting on capacity with
// no other feedback (ka-0000003525). It is not in aliveStates, so IsLive already
// returns false for it — callers check this BEFORE falling through to parked.
func IsQueued(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Fields["agent_state"] == queuedState
}

// IsPlanAssistantBead reports whether an agent bead is THIS plan's bound agent:
// it carries the role:plan-gardener label AND is bound to the epic via
// fields.plan_id / fields.task_id. The role-label check keeps a same-name
// non-plan agent from being mistaken for the plan's agent when the assigned edge
// is shared; the field check scopes it to the right plan.
func IsPlanAssistantBead(b *beadsapi.BeadDetail, epicID string) bool {
	if b == nil || !slices.Contains(b.Labels, RoleLabel) {
		return false
	}
	if b.Fields[PlanIDField] == epicID || b.Fields["task_id"] == epicID {
		return true
	}
	return false
}

// requireAuditAttribution checks that the write ctx carries an operator on one
// of the three channels the beadsapi client's sendOnce reads (edge-auth
// envelope, X-Operated-By, or X-System-Operator) and warns when it does not. It
// is the diagnostic half of the AUDIT-ATTRIBUTION CALLER CONTRACT (see the
// package doc + ka-0000001801): the engine attributes its Spawn/Wake writes
// ENTIRELY from the caller's ctx and never auto-stamps, so a caller that forgot
// to stamp (the future controller reactor is the compile-asserted new caller)
// would otherwise hit the daemon's R2 fail-closed 400 as an opaque upstream
// error. Logging here at the exact write boundary makes that root cause
// traceable to "operator missing on ctx" instead. It never blocks the write —
// the daemon is the authority on whether attribution can still be resolved
// server-side (e.g. an agent-bead fallback / spawn-chain walk); this is purely a
// visibility aid. `where` names the write path (spawn / wake) for the log line.
func (e *Engine) requireAuditAttribution(ctx context.Context, where, epicID, actor string) {
	if beadsapi.EdgeAuthOperatorFromContext(ctx) != "" ||
		beadsapi.OperatorFromContext(ctx) != "" ||
		beadsapi.SystemOperatorFromContext(ctx) != "" {
		return
	}
	e.logger.Warn("plan_agent_write_unattributed",
		"event", "plan_agent_write_unattributed",
		"where", where,
		"epic", epicID,
		"actor", actor,
		"detail", "no operator on ctx (WithOperator/WithSystemOperator/WithEdgeAuthOperator); "+
			"daemon audit resolver may fail-closed with HTTP 400 (R2, ka-0000005772)")
}

// projectFromLabels returns the value of the first "project:<name>" label, or
// empty if none is present. Mirrors cmd/beads-viewer's projectFromLabels.
func projectFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			return v
		}
	}
	return ""
}

// isPlanOpen reports whether a plan epic status still counts as open work.
// Mirrors cmd/beads-viewer's statusOpenSortKey==0 test: open / in_progress /
// unset are open, everything else (closed, …) is done.
func isPlanOpen(status string) bool {
	switch status {
	case "open", "in_progress", "":
		return true
	default:
		return false
	}
}
