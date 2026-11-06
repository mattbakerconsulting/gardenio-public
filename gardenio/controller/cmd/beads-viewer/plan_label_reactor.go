package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/plangardener"
)

// plan_label_reactor.go — the DURABLE, actor-agnostic auto-bind seam for a
// plan-gardener (task ka-0000006412, epic ka-0000003073; follow-up to the two
// viewer seams shipped by ka-0000002863).
//
// WHY THIS EXISTS. The shipped auto-bind covered only the two VIEWER surfaces
// that promote an epic to a plan: POST /plans (handleCreatePlan) and the
// one-click hand-mark (handlePlanMark). Both bind the gardener INLINE in their
// HTTP handler. But a plan is just an epic wearing the `plan` label, and ANY
// actor can add that label straight to the daemon and bypass both handlers:
//
//   - `kd label add <epic> plan`          (the named bypass in the task title)
//   - `kd update <epic> --add-label plan`
//   - `kd label set <epic> …,plan`        (whole-label-set reconcile)
//   - a formula / bulk relabel that writes the label
//
// A plan promoted this way is born GARDENERLESS and later HARD-409s at
// handlePlanBuild's "No gardener is bound" gate (plans_lifecycle.go). A live
// census (2026-08-08) found 15/18 open plan epics ungardenered — the epic's own
// failure mode recurring through the uncovered path. This reactor closes it:
// it OBSERVES the label landing on the epic and calls the SAME shared
// plangardener.Bind the two viewer seams use, so a raw-labeled plan gets the
// byte-identical gardener a browser-created one does.
//
// THE SIGNAL — two topics, one stream. A label addition surfaces on the daemon
// in exactly two shapes, and the two named bypasses split across them, so the
// reactor consumes BOTH from one EventStream:
//
//   - beads.label.added — the FIRST-CLASS event the AddLabel endpoint emits
//     (POST /v1/beads/{id}/labels). This is what `kd label add`,
//     `kd update --add-label`, and the viewer's own handlePlanMark AddLabel
//     all produce. Its payload names the single label added, so a `plan`-add is
//     a cheap string compare before any daemon round-trip.
//   - beads.bead.updated — the whole-bead PATCH path (UpdateBead ->
//     reconcileLabels) that `kd label set` and formula/bulk relabels take.
//     reconcileLabels calls store.AddLabel directly WITHOUT the endpoint, so it
//     never emits beads.label.added; instead the update publishes a Changes map
//     carrying changes["labels"] = the full new label slice. The reactor fires
//     when that slice newly contains `plan`.
//
// COLD-SPAWN, UNLIKE THE STEP REACTOR. PlanStepReactor is nudge/wake-ONLY: it
// never cold-spawns off its noisy dependency-added firehose because a bare edge
// carries no add-step intent. This reactor is the opposite: a `plan` label
// landing on an epic IS the unambiguous intent to make it a plan, so binding
// the gardener (a cold-spawn of the durable loop) is exactly right. The two
// filters that keep it safe are the label value (`plan`, nothing else) and the
// bead shape (type=epic), both re-verified against the freshly-fetched bead.
//
// IDEMPOTENT + BEST-EFFORT, like the viewer seams. plangardener.Bind adopts an
// existing gardener instead of duplicating (so a label churn, a viewer-inline
// bind racing this reactor, or a re-add is a safe no-op), and is born paused or
// enabled per EnabledOnBind (draft/ready → paused behind the approve→build
// gate; unset/building → enabled). Every miss or failure is logged at Warn and
// swallowed so a malformed/unrelated event can never wedge the upstream pump —
// the label write already succeeded, and `gb plan gardener <id>` is the
// out-of-band recovery path.
//
// LOOP-SAFE BY CONSTRUCTION. Binding the gardener mints a type=trigger bead via
// CreateBead, whose labels are added INSIDE the create transaction — that emits
// only beads.bead.created, never a per-label beads.label.added, and the created
// trigger is not a type=epic. So the reactor's own bind can never echo back
// into it.
//
// STILL VIEWER-COUPLED, like PlanStepReactor. This runs in the beads-viewer
// process (the one place that both consumes the daemon SSE stream AND can call
// plangardener.Bind — internal/bridge cannot, because plangardener imports it, so
// a bridge-side watcher would be an import cycle). If the viewer is down, a raw
// label-add lands the label but binds no gardener until the next add within a
// live viewer; the ops backfill sibling (ka-0000008090) sweeps any that slip
// through.

// planLabelReactorTopics is the comma-separated topic filter the reactor
// subscribes to. The daemon's SSE topic matcher (server/sse.go matchesTopic)
// treats a comma-separated `topics` query as a match-any set, so ONE stream
// carries both the first-class label-added event and the whole-bead update that
// the label-set / reconcile path takes.
const planLabelReactorTopics = plangardenerLabelAddedTopic + "," + plangardenerBeadUpdatedTopic

// The two daemon SSE topics the reactor keys on. Named as consts (not inlined)
// so the dispatch switch and the subscription string can never drift apart.
const (
	plangardenerLabelAddedTopic  = "beads.label.added"
	plangardenerBeadUpdatedTopic = "beads.bead.updated"
)

// planLabelReactorReconnectWait mirrors PlanStepReactor's: how long the upstream
// loop waits before re-opening the stream after it closes (daemon restart,
// blip). A missed event during the gap only means a missed auto-bind — the
// label still landed, and the ops backfill / a later label churn / the explicit
// "Enable gardener" button all recover it — so no backfill is needed here.
const planLabelReactorReconnectWait = beadFeedReconnectWait

// planGardenerBinder is the narrow slice of *Server the reactor drives: resolve
// a bead that just gained the `plan` label and idempotently bind its durable
// gardener. Satisfied by *Server (bindPlanGardenerForEpic below); kept as an
// interface so the reactor is unit-testable against a stub and the dependency
// direction reads reactor -> server.
type planGardenerBinder interface {
	// reactToPlanLabel resolves whether beadID is now a plan epic (type=epic
	// carrying `plan`) and, if so, idempotently binds its durable plan-gardener.
	// Best-effort: it swallows its own errors (logged) so a bad event never
	// wedges the upstream pump.
	reactToPlanLabel(ctx context.Context, beadID string)
}

// PlanLabelReactor observes the daemon label-mutation stream and cold-binds the
// durable plan-gardener when an epic gains the `plan` label by ANY writer — the
// raw `kd label add`, `kd label set`, `kd update --add-label`, a formula
// relabel, or the viewer's own hand-mark. One upstream stream, reconnecting
// like PlanStepReactor / BeadFeedHub.
//
// A nil *PlanLabelReactor is safe: Start/Close are no-ops, so a deployment with
// no daemon (nil client) simply runs without the durable auto-bind seam.
type PlanLabelReactor struct {
	daemon binderDaemon
	binder planGardenerBinder
	logger *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

// binderDaemon is the daemon surface the reactor's upstream pump needs — just
// the event stream. Satisfied by *beadsapi.Client (and the test fake).
type binderDaemon interface {
	EventStream(ctx context.Context, topics string) (<-chan beadsapi.SSEEvent, error)
}

// NewPlanLabelReactor builds a reactor over the daemon client + the server that
// owns the bind machinery. Returns nil when daemon or binder is nil (nothing to
// observe / nothing to drive), which every method tolerates — mirroring
// NewPlanStepReactor's nil-when-unwired contract.
func NewPlanLabelReactor(daemon binderDaemon, binder planGardenerBinder, logger *slog.Logger) *PlanLabelReactor {
	if daemon == nil || binder == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PlanLabelReactor{
		daemon: daemon,
		binder: binder,
		logger: logger,
		done:   make(chan struct{}),
	}
}

// Start launches the upstream consumer loop in a background goroutine. Safe to
// call on a nil reactor (no-op).
func (p *PlanLabelReactor) Start() {
	if p == nil {
		return
	}
	// Event-driven, no human behind the stream: an explicit system read.
	ctx, cancel := context.WithCancel(beadsapi.WithSystemRead(context.Background()))
	p.cancel = cancel
	go p.run(ctx)
}

// run is the upstream pump: (re)connect to the daemon SSE stream and dispatch
// every label-mutation event until the context is canceled. On stream close it
// waits planLabelReactorReconnectWait and retries, so a daemon restart
// self-heals — identical to PlanStepReactor.run.
func (p *PlanLabelReactor) run(ctx context.Context) {
	defer close(p.done)
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := p.daemon.EventStream(ctx, planLabelReactorTopics)
		if err != nil {
			p.logger.Warn("plan label reactor: opening upstream stream — retrying", "error", err)
			if !sleepCtx(ctx, planLabelReactorReconnectWait) {
				return
			}
			continue
		}
		p.logger.Info("plan label reactor: upstream stream open", "topics", planLabelReactorTopics)
		p.pump(ctx, events)
		if ctx.Err() != nil {
			return
		}
		p.logger.Info("plan label reactor: upstream stream closed — reconnecting")
		if !sleepCtx(ctx, planLabelReactorReconnectWait) {
			return
		}
	}
}

// Close cancels the upstream consumer loop and waits for it to drain. Safe to
// call on a nil reactor (no-op). Mirrors PlanStepReactor.Close.
func (p *PlanLabelReactor) Close() {
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
}

// pump drains one upstream stream, dispatching each event, until it closes or
// the context is canceled.
func (p *PlanLabelReactor) pump(ctx context.Context, events <-chan beadsapi.SSEEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			p.dispatch(ctx, evt)
		}
	}
}

// planLabelAddedEvent is the minimal shape decoded from a beads.label.added SSE
// payload (kbeads events.LabelAdded). Kept local so the viewer doesn't import
// the daemon's internal package — the same pattern plan_step_reactor.go uses.
type planLabelAddedEvent struct {
	BeadID string `json:"bead_id"`
	Label  string `json:"label"`
}

// planBeadUpdatedEvent is the minimal shape decoded from a beads.bead.updated
// SSE payload. Only the bead id/type and the Changes map are read: the reactor
// fires solely when changes["labels"] newly contains `plan`, so an unrelated
// field patch on any bead is dropped before any daemon round-trip.
type planBeadUpdatedEvent struct {
	Bead    *planUpdatedBead `json:"bead"`
	Changes map[string]any   `json:"changes,omitempty"`
}

type planUpdatedBead struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// dispatch routes one SSE event by its topic to the cheap in-process filter for
// that shape, and hands a candidate bead id to the binder. Malformed payloads
// and non-`plan` mutations are dropped silently — before any daemon round-trip.
func (p *PlanLabelReactor) dispatch(ctx context.Context, evt beadsapi.SSEEvent) {
	switch evt.Event {
	case plangardenerLabelAddedTopic:
		var payload planLabelAddedEvent
		if err := json.Unmarshal(evt.Data, &payload); err != nil {
			return
		}
		// Only a `plan`-label add is a candidate. Every other label the fleet
		// adds (project:, role:, squawk, …) lands here too — this is the
		// hottest filter, so it precedes the id check and any fetch.
		if strings.TrimSpace(payload.Label) != planLabel {
			return
		}
		beadID := strings.TrimSpace(payload.BeadID)
		if beadID == "" {
			return
		}
		p.binder.reactToPlanLabel(ctx, beadID)

	case plangardenerBeadUpdatedTopic:
		var payload planBeadUpdatedEvent
		if err := json.Unmarshal(evt.Data, &payload); err != nil || payload.Bead == nil {
			return
		}
		// Cheap shape gate before the label diff: only an epic can be a plan, so
		// a non-epic update (the overwhelming majority) is dropped without even
		// inspecting the Changes map.
		if payload.Bead.Type != "epic" {
			return
		}
		if !changesLabelsContainPlan(payload.Changes) {
			return
		}
		beadID := strings.TrimSpace(payload.Bead.ID)
		if beadID == "" {
			return
		}
		p.binder.reactToPlanLabel(ctx, beadID)
	}
}

// changesLabelsContainPlan reports whether a beads.bead.updated Changes map
// carries a labels transition whose new slice contains `plan`. The daemon
// publishes changes["labels"] = the FULL new label slice (beads.go UpdateBead),
// so this both confirms a label change happened AND that the resulting set is a
// plan — a strictly-cheaper, allocation-free scan than diffing against the old
// set. It is intentionally level-triggered: a re-write that keeps `plan` still
// resolves to a bind attempt, which plangardener.Bind's idempotent-adopt makes a
// safe no-op.
//
// The value arrives as a []any of JSON strings (json.Unmarshal into map[string]
// any), so each element is asserted to string before the compare; a labels
// entry of any other shape (absent, null, wrong type) yields false — fail-safe.
func changesLabelsContainPlan(changes map[string]any) bool {
	if changes == nil {
		return false
	}
	raw, ok := changes["labels"]
	if !ok {
		return false
	}
	labels, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, l := range labels {
		if s, ok := l.(string); ok && s == planLabel {
			return true
		}
	}
	return false
}

// reactToPlanLabel is the *Server half of the seam (satisfies planGardenerBinder).
// Given a bead that just gained the `plan` label (via any writer), it re-fetches
// the bead, verifies it is a plan epic, and idempotently binds its durable
// plan-gardener via the SAME shared plangardener.Bind the viewer's create /
// hand-mark / "Enable gardener" seams use.
//
// Re-fetch, don't trust the event: the label-added payload carries only the
// bead id + label, and the update payload carries only id/type — neither has
// the labels the bind's plan-shape guard and project inheritance need. A fresh
// GetBead also collapses a burst of adds (label churn, a viewer-inline bind that
// already ran) to the current truth: plangardener.Bind then adopts whatever
// gardener exists rather than minting a second.
//
// Best-effort throughout: every resolution miss or bind failure is logged at
// Warn and swallowed so a malformed/unrelated event can never wedge the upstream
// pump. Attribution INHERITS THE PLAN EPIC'S HUMAN when it has one — the event
// itself carries no operator (a raw `kd label add` may be an agent, a formula, a
// bulk relabel), but the epic does, and the gardener this bind creates is only
// able to spawn if its operated_by is human (see the block at the bind call).
// Only an epic with no walkable human falls back to WithSystemOperator, which
// the daemon audit resolver collapses to the allowlisted system:machine sink
// rather than fail-closing on a missing human.
func (s *Server) reactToPlanLabel(ctx context.Context, beadID string) {
	// No human is on this path (the event came off the daemon stream), so the
	// viewer acts as ITSELF: its machine principal, when configured. Under
	// write-bearer enforcement this is what lets the bind's CreateBead go out
	// at all; the daemon attributes the trigger to machine:<clientId>.
	ctx = s.machineCtx(ctx)
	epic, err := s.daemon.GetBead(ctx, beadID)
	if err != nil || epic == nil {
		// A fetch miss (deleted between event and read, transient error) is not
		// actionable — the label write already succeeded; recovery is the ops
		// backfill or the next label churn.
		return
	}
	// Re-verify the plan shape against the FRESH bead, not the event: only a
	// type=epic carrying `plan` is a /plans plan. This fail-closes a spurious
	// event (a non-epic that a bead-updated race surfaced, an epic whose `plan`
	// label was removed again before this read) — and it is exactly the guard
	// plangardener.Bind itself re-asserts, so a slip here is caught there too.
	if epic.Type != "epic" || !hasLabel(epic, planLabel) {
		return
	}

	// Attribution: PREFER THE PLAN EPIC'S OWN HUMAN. The label-add event itself
	// carries no operator, but the epic it landed on almost always does — a
	// plan is authored by a person, and that person is the right owner of its
	// gardener. Inheriting them is not cosmetic: the trigger engine reads the
	// gardener trigger bead's operated_by to attribute its autonomous fires
	// (ctxWithTriggerOperator, bridge/triggers.go), and a NON-HUMAN value there
	// makes every spawn_agent CreateBead fail closed with HTTP 400 "operator
	// could not be resolved" — a gardener that matches every cycle and spawns
	// nothing, with no decline reason recorded anywhere (bug ka-0000004529;
	// three gardeners were dark this way, incl. ka-0000007776 and ka-0000007386).
	//
	// Only when the epic carries no walkable human (bulk relabel of a
	// machine-authored epic) do we fall back to the system-machine sink, which
	// the daemon audit resolver collapses to system:machine rather than
	// hard-400ing this bind's own CreateBead (epic ka-0000004835).
	// bridge.IsHumanOperator is the same predicate the daemon and the trigger
	// engine apply, so a value accepted here is a value they accept.
	operator := strings.TrimSpace(epic.OperatedBy)
	var bindCtx context.Context
	if bridge.IsHumanOperator(operator) {
		bindCtx = beadsapi.WithOperator(ctx, operator)
	} else {
		operator = ""
		bindCtx = beadsapi.WithSystemOperator(ctx, planLabelReactorSystemOperator)
	}
	res, err := plangardener.Bind(bindCtx, s.daemon, epic, plangardener.BindSpec{
		Operator:  operator,
		CreatedBy: planLabelReactorSystemOperator,
	})
	if err != nil {
		s.logger.Warn("plan label reactor: bind", "plan", epic.ID, "error", err)
		return
	}
	if res.Adopted {
		s.logger.Info("plan label reactor: adopted existing gardener",
			"plan", epic.ID, "gardener", res.ID, "spawn_role", res.SpawnRole)
		return
	}
	s.logger.Info("plan label reactor: bound durable gardener",
		"plan", epic.ID, "gardener", res.ID, "enabled", res.Enabled, "cron", res.Cron)
}

// planLabelReactorSystemOperator is the source label attributed to a reactive
// auto-bind's CreateBead. It is a machine origin (no human triggered it — the
// label may have been added by an agent, a formula, or a bulk relabel), so it
// rides the WithSystemOperator channel, which the daemon collapses to the
// allowlisted system:machine sink.
const planLabelReactorSystemOperator = "plan-label-autobind"

// Compile-time proof that *Server satisfies the reactor's narrow binder
// interface — the same assertion pattern the step reactor relies on implicitly
// via NewPlanStepReactor(daemon, srv, …).
var _ planGardenerBinder = (*Server)(nil)

// Compile-time proof that the viewer's daemon surface (beadAPI) satisfies the
// shared plangardener.Bind client interface — s.daemon is what reactToPlanLabel
// passes to Bind. If a future plangardener.Client change breaks the viewer
// client this fails to compile instead of silently forking the reactive bind
// from the viewer/CLI ones.
var _ plangardener.Client = beadAPI(nil)
