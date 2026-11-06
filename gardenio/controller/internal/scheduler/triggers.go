package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/scheduler/conditions"
)

// Trigger kinds enumerate what fires a schedule. Per the plan-of-record
// (gardenio/plans/epic-mention-looping-gardener.md §1.2) the schedule bead
// gains a single `triggers:` JSON-array field; each entry names one source.
//
// T3 scope wires cron, manual, bead_event, decision_resolved. The remaining
// kinds (mail_received, webhook, slack_reaction) are deferred to a follow-on
// task per the plan-of-record.
const (
	TriggerKindCron             = "cron"
	TriggerKindManual           = "manual"
	TriggerKindBeadEvent        = "bead_event"
	TriggerKindDecisionResolved = "decision_resolved"
)

// Payload keys a comment.added event carries besides the hydrated bead's own
// fields. The edge event's bead is the commented bead, so its created_by is
// that bead's author, not the comment's; a match that needs to know who wrote
// the comment reads these instead (bridge.RegisterSSE sets them from the
// kbeads CommentAdded frame, gd-bo2RUmtpHb).
const (
	PayloadCommentAuthor = "comment_author"
	PayloadCommentText   = "comment_text"
)

// TriggerEntry is one source in a schedule's `triggers:` array.
//
// Match is the in-process payload predicate evaluated against the event that
// caused the fire (bead_event, decision_resolved). Preflight is the
// daemon-side world-state predicate evaluated at fire time. The two answer
// different questions: preflight asks the world, match asks the event.
//
// Throttle drops fires inside a wall-clock window since the previous fire
// from this entry. Debounce coalesces a burst of events in the configured
// window into a single fire after the burst settles.
type TriggerEntry struct {
	Kind      string                 `json:"kind"`
	Cron      string                 `json:"cron,omitempty"`
	Timezone  string                 `json:"timezone,omitempty"`
	Events    []string               `json:"events,omitempty"`
	Match     *conditions.Expression `json:"match,omitempty"`
	Preflight *conditions.Expression `json:"preflight,omitempty"`
	Throttle  string                 `json:"throttle,omitempty"`
	Debounce  string                 `json:"debounce,omitempty"`
	Label     string                 `json:"label,omitempty"`
}

// ParseTriggers parses the schedule bead's `triggers` JSON-array field.
// Returns nil (no error) on empty input.
func ParseTriggers(raw string) ([]TriggerEntry, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var entries []TriggerEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("triggers: %w", err)
	}
	return entries, nil
}

// TriggerEvent is the in-process event that the scheduler ingests from
// upstream SSE / NATS subscribers via OnBeadEvent / OnDecisionResolved.
//
// Kind names the event family ("bead_event" or "decision_resolved");
// EventType narrows it ("created"|"updated"|"closed" for bead_event,
// "resolved" for decisions). Payload is a flat string map used by field
// leaves in TriggerEntry.Match.
type TriggerEvent struct {
	Kind      string
	EventType string
	BeadID    string
	Payload   map[string]any
	Labels    []string
	Time      time.Time
}

// FireDecision is the outcome of evaluating a TriggerSet against the current
// reconcile tick.
type FireDecision struct {
	Fire       bool
	EntryIndex int
	Kind       string // matching TriggerEntry.Kind
	Cause      string // human-readable: "cron 0 9 * * *" / "bead_event closed ka-X"
	Time       time.Time
	Event      *TriggerEvent // set when Kind is event-driven
}

// EntryEngine holds per-schedule throttle/debounce state and event
// buffers used while evaluating a schedule's trigger entries. Named
// EntryEngine (not TriggerEngine) to distinguish it from
// bridge.TriggerEngine — the higher-level orchestrator that loads
// trigger beads, fires actions, and manages dedup / cooldown / spawn
// authorization. This type is the lower layer: it only buffers events
// and tracks per-entry throttle / debounce windows. State is keyed on
// "<schedule_id>:<entry_index>" so a schedule with multiple entries
// gets independent windows per entry.
//
// The engine is process-local — restart wipes throttle state. Catch-up on
// restart is per-source: cron entries replay from schedule.last_run; event
// entries lose at most one debounce window's worth of buffered events
// (acceptable: SSE replays from Last-Event-ID on reconnect, and the
// scheduler's reconcile tick re-evaluates the world).
type EntryEngine struct {
	mu sync.Mutex

	// lastFire["<schedid>:<idx>"] = wall-clock of last fire from that entry.
	lastFire map[string]time.Time

	// debounce["<schedid>:<idx>"] tracks an in-flight debounce window.
	debounce map[string]*debounceState

	// pendingEvents is a global buffer of events ingested since the last
	// Reconcile drain. The TriggerSet evaluator filters per schedule.
	pendingEvents []TriggerEvent

	// frozen, while freezing, is how many of pendingEvents the current pass
	// sees (FreezeEvents); later events wait for the next pass.
	frozen   int
	freezing bool
}

type debounceState struct {
	deadline time.Time    // when the debounced fire should be allowed
	event    TriggerEvent // most recent event in the burst (last-write-wins)
}

// NewEntryEngine constructs an engine with empty state.
func NewEntryEngine() *EntryEngine {
	return &EntryEngine{
		lastFire: make(map[string]time.Time),
		debounce: make(map[string]*debounceState),
	}
}

// OnBeadEvent buffers an inbound bead lifecycle event, stamped now. Safe for
// concurrent callers (typically an SSE handler goroutine).
func (e *EntryEngine) OnBeadEvent(eventType, beadID string, payload map[string]any, labels []string) {
	e.OnBeadEventAt(eventType, beadID, payload, labels, time.Now())
}

// OnBeadEventAt is OnBeadEvent stamped at, the time the caller took the event
// in. A source whose fire keys carry the event time passes its own clock.
func (e *EntryEngine) OnBeadEventAt(eventType, beadID string, payload map[string]any, labels []string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pendingEvents = append(e.pendingEvents, TriggerEvent{
		Kind:      TriggerKindBeadEvent,
		EventType: eventType,
		BeadID:    beadID,
		Payload:   payload,
		Labels:    labels,
		Time:      at,
	})
}

// OnDecisionResolved buffers a decision-resolved event. Decision events
// surface via the bridge's decisions watcher; payload should carry at least
// `decision_id`, `raised_by_agent.task_id`, and any operator-supplied
// fields the schedule's match expression cares about.
func (e *EntryEngine) OnDecisionResolved(decisionID string, payload map[string]any, labels []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pendingEvents = append(e.pendingEvents, TriggerEvent{
		Kind:      TriggerKindDecisionResolved,
		EventType: "resolved",
		BeadID:    decisionID,
		Payload:   payload,
		Labels:    labels,
		Time:      time.Now(),
	})
}

// PendingEventCount returns the number of buffered events. For tests and
// monitoring.
func (e *EntryEngine) PendingEventCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pendingEvents)
}

// snapshotEvents returns a copy of the events this pass sees: the frozen ones
// after FreezeEvents, else every buffered one. The buffer is not cleared
// here because each schedule's TriggerSet evaluation needs to see the same
// event set; the caller drains explicitly via DrainEvents at the end of a
// reconcile pass.
func (e *EntryEngine) snapshotEvents() []TriggerEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	visible := e.pendingEvents
	if e.freezing {
		visible = visible[:e.frozen]
	}
	out := make([]TriggerEvent, len(visible))
	copy(out, visible)
	return out
}

// FreezeEvents fixes the events the coming pass sees to those buffered now.
// An event buffered during the pass waits for the next one, and the
// DrainEvents that ends the pass removes only the frozen events. Without it,
// an event that lands after a trigger was visited but before the drain is
// seen by no trigger at all (gd-IQcMk16PkH).
func (e *EntryEngine) FreezeEvents() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.frozen, e.freezing = len(e.pendingEvents), true
}

// DrainEvents ends a reconcile pass: it removes the events FreezeEvents
// froze, or every buffered event when the pass was not frozen. Call at the
// end of a reconcile pass after every schedule has had a chance to evaluate.
func (e *EntryEngine) DrainEvents() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.freezing {
		e.pendingEvents = e.pendingEvents[:0]
		return
	}
	e.pendingEvents = slices.Clone(e.pendingEvents[e.frozen:])
	e.frozen, e.freezing = 0, false
}

// recordFire stamps the wall-clock time the named entry fired.
func (e *EntryEngine) recordFire(scheduleID string, entryIndex int, t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastFire[stateKey(scheduleID, entryIndex)] = t
}

// inThrottleWindow reports whether the named entry has fired within the
// throttle window. Returns false when throttle is unset or zero.
func (e *EntryEngine) inThrottleWindow(scheduleID string, entryIndex int, throttle time.Duration, now time.Time) bool {
	if throttle <= 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	last, ok := e.lastFire[stateKey(scheduleID, entryIndex)]
	if !ok || last.IsZero() {
		return false
	}
	return now.Sub(last) < throttle
}

// scheduleDebounce records or extends a debounce window for a burst of
// events; returns true if the debounce deadline has passed (the burst has
// settled and the entry should fire). When debounce is unset or zero the
// caller should fire immediately.
func (e *EntryEngine) tickDebounce(scheduleID string, entryIndex int, debounce time.Duration, evt TriggerEvent, now time.Time) (fire bool, payload TriggerEvent) {
	if debounce <= 0 {
		return true, evt
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	key := stateKey(scheduleID, entryIndex)
	state, ok := e.debounce[key]
	if !ok || state == nil {
		// First event of the burst: open the window.
		e.debounce[key] = &debounceState{
			deadline: now.Add(debounce),
			event:    evt,
		}
		return false, TriggerEvent{}
	}
	// Burst continues — last-write-wins payload, slide deadline forward.
	state.event = evt
	state.deadline = now.Add(debounce)
	return false, TriggerEvent{}
}

// drainSettledDebounce returns the most-recent buffered event for any
// debounce window whose deadline is at or before now, then clears that
// window. Caller should fire once per returned event.
func (e *EntryEngine) drainSettledDebounce(scheduleID string, entryIndex int, now time.Time) (TriggerEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := stateKey(scheduleID, entryIndex)
	state, ok := e.debounce[key]
	if !ok || state == nil {
		return TriggerEvent{}, false
	}
	if now.Before(state.deadline) {
		return TriggerEvent{}, false
	}
	evt := state.event
	delete(e.debounce, key)
	return evt, true
}

// peekSettledDebounce reports whether the named entry has a debounce window
// whose deadline is at or before now, WITHOUT clearing it. It is the read-only
// counterpart to drainSettledDebounce, used by HasPendingEventMatch so the
// cooldown-bypass probe can tell that a settled burst is about to fire without
// consuming it.
func (e *EntryEngine) peekSettledDebounce(scheduleID string, entryIndex int, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	state, ok := e.debounce[stateKey(scheduleID, entryIndex)]
	if !ok || state == nil {
		return false
	}
	return !now.Before(state.deadline)
}

func stateKey(scheduleID string, entryIndex int) string {
	return fmt.Sprintf("%s:%d", scheduleID, entryIndex)
}

// matchesEventType returns true when filter is empty (match-all) or
// contains topic. Topics may be passed as either short names ("closed") or
// fully-qualified topics ("beads.bead.closed"); both are accepted.
func matchesEventType(filter []string, topic string) bool {
	if len(filter) == 0 {
		return true
	}
	short := topic
	if idx := strings.LastIndex(topic, "."); idx >= 0 {
		short = topic[idx+1:]
	}
	for _, f := range filter {
		if f == topic || f == short {
			return true
		}
	}
	return false
}

// withLabels copies payload and overlays a "labels" key with the event's
// label slice. Done out-of-place so concurrent matchers can't see partial
// state through a shared map.
func withLabels(payload map[string]any, labels []string) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		out[k] = v
	}
	asAny := make([]any, len(labels))
	for i, l := range labels {
		asAny[i] = l
	}
	out["labels"] = asAny
	return out
}

// Exported wrappers around the engine's private state methods. The bridge's
// multi-source TriggerSource (Phase 3b/B6) lives in a different package and
// needs access to throttle/debounce bookkeeping without dragging in
// *Scheduler. The wrappers are intentionally thin so the in-package callers
// can keep using the lowercase forms.

// SnapshotEvents returns a snapshot of buffered events without draining the
// buffer. See snapshotEvents for semantics.
func (e *EntryEngine) SnapshotEvents() []TriggerEvent { return e.snapshotEvents() }

// InThrottleWindow reports whether the named entry has fired within the
// throttle window. See inThrottleWindow for semantics.
func (e *EntryEngine) InThrottleWindow(scheduleID string, entryIndex int, throttle time.Duration, now time.Time) bool {
	return e.inThrottleWindow(scheduleID, entryIndex, throttle, now)
}

// RecordFire stamps the wall-clock time the named entry fired.
func (e *EntryEngine) RecordFire(scheduleID string, entryIndex int, t time.Time) {
	e.recordFire(scheduleID, entryIndex, t)
}

// TickDebounce records or extends a debounce window. See tickDebounce.
func (e *EntryEngine) TickDebounce(scheduleID string, entryIndex int, debounce time.Duration, evt TriggerEvent, now time.Time) (bool, TriggerEvent) {
	return e.tickDebounce(scheduleID, entryIndex, debounce, evt, now)
}

// DrainSettledDebounce returns the most-recent buffered event for any
// debounce window whose deadline is at or before now. See drainSettledDebounce.
func (e *EntryEngine) DrainSettledDebounce(scheduleID string, entryIndex int, now time.Time) (TriggerEvent, bool) {
	return e.drainSettledDebounce(scheduleID, entryIndex, now)
}

// TriggerEvalContext is the per-trigger input needed to evaluate a TriggerSet
// without depending on *Scheduler. The bridge's multi-source TriggerSource
// (Phase 3b/B6) populates this from a TriggerDef.
type TriggerEvalContext struct {
	// TriggerID is the schedule or trigger bead ID used to key throttle /
	// debounce state and the firing log line. Must be non-empty.
	TriggerID string
	// Project is the default project for daemon-side condition evaluation
	// (conditions.Env.DefaultProject). Empty when the trigger is project-less.
	Project string
	// Timezone is the default tz for cron entries that don't set their own.
	Timezone string
	// Triggers is the multi-source fan-in array. Iterated in order; first
	// firing entry wins.
	Triggers []TriggerEntry
	// LastRun is the canonical last-fire time used to suppress repeat fires
	// for a cron entry (mirrors Schedule.LastRun). Zero means "never fired".
	LastRun time.Time
}

// EvaluateTriggers fans-in a TriggerEvalContext.Triggers slice and returns
// the first firing entry's FireDecision. Free-function counterpart to
// (*Scheduler).evaluateTriggerSet so callers without a Scheduler (the
// bridge's multi-source TriggerSource) can reuse the same throttle /
// debounce / cron / event semantics.
//
// The match-expression and ancestor-resolution dependencies are supplied via
// daemon (which must implement both conditions.BeadClient and
// conditions.AncestorClient — *beadsapi.Client does).
func EvaluateTriggers(ctx context.Context, engine *EntryEngine, daemon TriggerDaemon, logger *slog.Logger, tctx TriggerEvalContext, now time.Time) FireDecision {
	if engine == nil || logger == nil {
		return FireDecision{}
	}
	events := engine.snapshotEvents()
	for i := range tctx.Triggers {
		entry := tctx.Triggers[i]
		throttle := parseDurationField(entry.Throttle, 0)
		debounce := parseDurationField(entry.Debounce, 0)

		if engine.inThrottleWindow(tctx.TriggerID, i, throttle, now) {
			continue
		}

		switch entry.Kind {
		case TriggerKindCron:
			dec := EvaluateCronEntry(tctx, entry, i, now, logger)
			if !dec.Fire {
				continue
			}
			// Preflight is the daemon-side world-state gate: when present,
			// the cron floor only fires if the world predicate holds (e.g.
			// the epic still has open children). Without this, a cron entry
			// fires unconditionally and the documented "no-op floor" never
			// no-ops (ka-0000007730). Fail-closed: a nil daemon or evaluator
			// error suppresses the fire so a flaky daemon can't auto-spawn.
			if entry.Preflight != nil {
				if !cronPreflightHolds(ctx, daemon, logger, tctx, entry, i) {
					continue
				}
			}
			return dec
		case TriggerKindManual:
			continue
		case TriggerKindBeadEvent, TriggerKindDecisionResolved:
			if dec := EvaluateEventEntry(ctx, engine, daemon, logger, tctx, entry, i, events, debounce, now); dec.Fire {
				return dec
			}
		default:
			logger.Debug("trigger entry: unknown kind, skipping",
				"trigger", tctx.TriggerID, "kind", entry.Kind, "index", i)
			continue
		}
	}
	return FireDecision{}
}

// EvaluateTriggersAll is the multi-decision fan-in: it walks the same entries
// in the same order as EvaluateTriggers and returns ALL firing decisions for
// the first entry that fires (a cron entry yields one decision; an event entry
// yields one decision per distinct matching buffered event). This is what the
// multi-source and schedule-mirror sources use so N distinct events in a single
// reconcile cycle each fire exactly once instead of being collapsed into one
// (ka-0000000952).
//
// "First firing entry wins" is preserved: iteration stops at the first entry
// that produces any decision, matching EvaluateTriggers' single-decision
// contract for cron/manual ordering.
func EvaluateTriggersAll(ctx context.Context, engine *EntryEngine, daemon TriggerDaemon, logger *slog.Logger, tctx TriggerEvalContext, now time.Time) []FireDecision {
	if engine == nil || logger == nil {
		return nil
	}
	events := engine.snapshotEvents()
	for i := range tctx.Triggers {
		entry := tctx.Triggers[i]
		throttle := parseDurationField(entry.Throttle, 0)
		debounce := parseDurationField(entry.Debounce, 0)

		if engine.inThrottleWindow(tctx.TriggerID, i, throttle, now) {
			continue
		}

		switch entry.Kind {
		case TriggerKindCron:
			dec := EvaluateCronEntry(tctx, entry, i, now, logger)
			if !dec.Fire {
				continue
			}
			if entry.Preflight != nil {
				if !cronPreflightHolds(ctx, daemon, logger, tctx, entry, i) {
					continue
				}
			}
			return []FireDecision{dec}
		case TriggerKindManual:
			continue
		case TriggerKindBeadEvent, TriggerKindDecisionResolved:
			if decs := EvaluateEventEntryAll(ctx, engine, daemon, logger, tctx, entry, i, events, debounce, now); len(decs) > 0 {
				return decs
			}
		default:
			logger.Debug("trigger entry: unknown kind, skipping",
				"trigger", tctx.TriggerID, "kind", entry.Kind, "index", i)
			continue
		}
	}
	return nil
}

// EvaluateCronEntry is the free-function counterpart to
// (*Scheduler).evaluateCronEntry. Takes the trigger context explicitly so it
// can be called without a *Scheduler.
func EvaluateCronEntry(tctx TriggerEvalContext, entry TriggerEntry, idx int, now time.Time, logger *slog.Logger) FireDecision {
	if entry.Cron == "" {
		logger.Warn("trigger entry: cron missing expression",
			"trigger", tctx.TriggerID, "index", idx)
		return FireDecision{}
	}
	tz := entry.Timezone
	if tz == "" {
		tz = tctx.Timezone
	}
	loc := time.UTC
	if tz != "" {
		if parsed, err := time.LoadLocation(tz); err == nil {
			loc = parsed
		} else {
			logger.Warn("trigger entry: invalid timezone, using UTC",
				"trigger", tctx.TriggerID, "index", idx, "timezone", tz, "error", err)
		}
	}
	expr, err := ParseCron(entry.Cron)
	if err != nil {
		logger.Warn("trigger entry: invalid cron expression",
			"trigger", tctx.TriggerID, "index", idx, "cron", entry.Cron, "error", err)
		return FireDecision{}
	}
	nowLocal := now.In(loc)
	if !expr.InWindow(nowLocal) {
		return FireDecision{}
	}
	lastFire := expr.Prev(nowLocal).UTC()
	if lastFire.IsZero() {
		return FireDecision{}
	}
	if !tctx.LastRun.IsZero() && !tctx.LastRun.Before(lastFire) {
		return FireDecision{}
	}
	return FireDecision{
		Fire:       true,
		EntryIndex: idx,
		Kind:       TriggerKindCron,
		Cause:      fmt.Sprintf("cron %s", entry.Cron),
		Time:       lastFire,
	}
}

// cronPreflightHolds evaluates a cron entry's daemon-side preflight predicate
// and returns true only if it holds. Fail-closed: a nil daemon or any
// evaluator error returns false (suppresses the fire), mirroring the legacy
// ScheduleTriggerSource conditions gate. Callers must check entry.Preflight !=
// nil before calling (a nil preflight means "no gate" and should fire).
func cronPreflightHolds(ctx context.Context, daemon TriggerDaemon, logger *slog.Logger, tctx TriggerEvalContext, entry TriggerEntry, idx int) bool {
	if daemon == nil {
		logger.Warn("trigger entry: cron preflight present but no daemon, suppressing fire",
			"trigger", tctx.TriggerID, "index", idx)
		return false
	}
	env := conditions.Env{
		Bead:           daemon,
		Ancestor:       daemon,
		DefaultProject: tctx.Project,
	}
	ok, err := conditions.Evaluate(ctx, *entry.Preflight, env)
	if err != nil {
		logger.Warn("trigger entry: cron preflight evaluation failed, suppressing fire",
			"trigger", tctx.TriggerID, "index", idx, "error", err)
		return false
	}
	if !ok {
		logger.Debug("trigger entry: cron preflight not met, skipping",
			"trigger", tctx.TriggerID, "index", idx)
	}
	return ok
}

// PreflightHolds reports whether a per-entry preflight expression holds, with
// the same fail-closed semantics as a cron entry's gate: a nil daemon or an
// evaluator error is "does not hold". A nil preflight holds. The multi source
// uses it to gate a plan gardener's first fire on the watchdog's open-children
// gate (gd-Vk2ADepJZ2).
func PreflightHolds(ctx context.Context, daemon TriggerDaemon, logger *slog.Logger, tctx TriggerEvalContext, preflight *conditions.Expression, idx int) bool {
	if preflight == nil {
		return true
	}
	return cronPreflightHolds(ctx, daemon, logger, tctx, TriggerEntry{Preflight: preflight}, idx)
}

// EvaluateEventEntry is the free-function counterpart to
// (*Scheduler).evaluateEventEntry. It returns the FIRST firing decision for the
// entry (or a zero-value non-firing decision). Callers that must not drop
// distinct concurrent events should use EvaluateEventEntryAll instead — the
// single-decision form collapses N matching buffered events into one fire.
func EvaluateEventEntry(ctx context.Context, engine *EntryEngine, daemon TriggerDaemon, logger *slog.Logger, tctx TriggerEvalContext, entry TriggerEntry, idx int, events []TriggerEvent, debounce time.Duration, now time.Time) FireDecision {
	decisions := EvaluateEventEntryAll(ctx, engine, daemon, logger, tctx, entry, idx, events, debounce, now)
	if len(decisions) == 0 {
		return FireDecision{}
	}
	return decisions[0]
}

// EvaluateEventEntryAll collects ALL currently-matching (non-throttled,
// non-debounced) buffered events for an event-kind entry and returns one
// FireDecision per event, mirroring BeadStateTriggerSource.Evaluate.
//
// This fixes ka-0000000952: the prior single-decision path overwrote `matched`
// on each loop iteration, so only the LAST-arrived matching event survived and
// returned exactly one FireDecision. Because the runner drains the whole event
// buffer after every Evaluate pass, every earlier matching event was
// PERMANENTLY LOST — an action:create_bead/notify/spawn per-event entry that
// saw two distinct matching events in one ~30s window fired for only one of
// them. Emitting one decision per event lets the routing layer's per-event
// dedup key (source match key = "<event_type>:<bead_id>") fire each distinct
// event exactly once, as it already intends to.
//
// A settled debounce window still yields exactly ONE decision: the debounce
// contract collapses a burst into a single representative event by design. A
// live debounce window returns no decisions (the event fires when it settles).
func EvaluateEventEntryAll(ctx context.Context, engine *EntryEngine, daemon TriggerDaemon, logger *slog.Logger, tctx TriggerEvalContext, entry TriggerEntry, idx int, events []TriggerEvent, debounce time.Duration, now time.Time) []FireDecision {
	if settled, ok := engine.drainSettledDebounce(tctx.TriggerID, idx, now); ok {
		return []FireDecision{{
			Fire:       true,
			EntryIndex: idx,
			Kind:       entry.Kind,
			Cause:      eventCause(entry, settled),
			Time:       settled.Time,
			Event:      &settled,
		}}
	}

	var matched []TriggerEvent
	for i := range events {
		evt := events[i]
		if !eventMatchesEntry(ctx, daemon, logger, tctx, entry, idx, evt) {
			continue
		}
		matched = append(matched, evt)
	}
	if len(matched) == 0 {
		return nil
	}
	if debounce > 0 {
		// A debounce window intentionally collapses a burst into one fire; feed
		// the most-recent matching event so the settled event carries the latest
		// state, matching the prior single-decision behavior.
		engine.tickDebounce(tctx.TriggerID, idx, debounce, matched[len(matched)-1], now)
		return nil
	}
	decisions := make([]FireDecision, 0, len(matched))
	for i := range matched {
		evtCopy := matched[i]
		decisions = append(decisions, FireDecision{
			Fire:       true,
			EntryIndex: idx,
			Kind:       entry.Kind,
			Cause:      eventCause(entry, evtCopy),
			Time:       evtCopy.Time,
			Event:      &evtCopy,
		})
	}
	return decisions
}

// eventMatchesEntry reports whether a single buffered event satisfies an
// event-kind trigger entry: same Kind, an allowed EventType (for bead_event),
// and a passing Match expression. It is the shared predicate used by both the
// firing path (EvaluateEventEntry) and the read-only readiness probe
// (HasPendingEventMatch) so the two never disagree about what "a matching
// buffered event" means. A Match-evaluation error is treated as non-match and
// logged, mirroring the firing path's drop-on-error behavior.
func eventMatchesEntry(ctx context.Context, daemon TriggerDaemon, logger *slog.Logger, tctx TriggerEvalContext, entry TriggerEntry, idx int, evt TriggerEvent) bool {
	if evt.Kind != entry.Kind {
		return false
	}
	if entry.Kind == TriggerKindBeadEvent && !matchesEventType(entry.Events, evt.EventType) {
		return false
	}
	if entry.Match == nil {
		return true
	}
	payload := evt.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	if len(evt.Labels) > 0 {
		payload = withLabels(payload, evt.Labels)
	}
	env := conditions.Env{
		Payload:        payload,
		DefaultProject: tctx.Project,
	}
	if daemon != nil {
		env.Bead = daemon
		env.Ancestor = daemon
	}
	ok, err := conditions.Evaluate(ctx, *entry.Match, env)
	if err != nil {
		logger.Warn("trigger entry: match evaluation failed, dropping event",
			"trigger", tctx.TriggerID, "index", idx, "kind", entry.Kind, "error", err)
		return false
	}
	return ok
}

// HasPendingEventMatch reports whether the trigger has at least one event-kind
// entry that WOULD fire from a currently-buffered event — without consuming the
// event, recording a fire, or mutating debounce state. It exists to break the
// cooldown/event race (bug ka-0000005988): the bridge's per-trigger cooldown
// gate runs BEFORE Evaluate and returns early, so a bead_event that lands while
// a multi-source trigger is mid-cooldown is buffered, never consumed, then
// drained at cycle end — silently dropped. The bridge engine calls this probe
// from its cooldown gate so an event-eligible trigger bypasses cooldown when a
// real matching event is waiting; cron/idle re-eval continues to honor cooldown.
//
// Semantics, kept deliberately in lockstep with EvaluateTriggers:
//   - Throttled entries (per-entry throttle window still open) are skipped —
//     the per-entry throttle is the right rate-limiter for events, so a
//     throttled entry is genuinely not ready and must not bypass cooldown.
//   - A settled debounce window counts as ready: that event is about to fire on
//     the next Evaluate regardless of cooldown.
//   - Cron and manual entries never make the trigger event-ready; only
//     bead_event / decision_resolved entries can. This keeps cooldown fully in
//     charge of cron/idle re-eval.
//
// It is read-only: snapshotEvents does not drain, the throttle/debounce checks
// only read state, and no fire is recorded.
func HasPendingEventMatch(ctx context.Context, engine *EntryEngine, daemon TriggerDaemon, logger *slog.Logger, tctx TriggerEvalContext, now time.Time) bool {
	if engine == nil || logger == nil {
		return false
	}
	events := engine.snapshotEvents()
	for i := range tctx.Triggers {
		entry := tctx.Triggers[i]
		switch entry.Kind {
		case TriggerKindBeadEvent, TriggerKindDecisionResolved:
		default:
			continue
		}
		throttle := parseDurationField(entry.Throttle, 0)
		if engine.inThrottleWindow(tctx.TriggerID, i, throttle, now) {
			continue
		}
		// A debounce window that has already settled will fire on the next
		// Evaluate regardless of cooldown — treat it as ready. peekSettledDebounce
		// is read-only (it does not delete the window like drainSettledDebounce).
		if engine.peekSettledDebounce(tctx.TriggerID, i, now) {
			return true
		}
		for j := range events {
			if eventMatchesEntry(ctx, daemon, logger, tctx, entry, i, events[j]) {
				return true
			}
		}
	}
	return false
}

// HasDueCronEntry reports whether any cron entry in tctx.Triggers would fire
// right now (in its schedule window and past tctx.LastRun) — read-only: it
// consumes no state and records no fire. The bridge engine's cooldown gate
// calls this so a cron-due trigger can bypass the per-trigger cooldown, the
// cron sibling of HasPendingEventMatch (bug ka-0000005988).
//
// Without the bypass, a cooldown that divides 24h evenly (e.g.
// cooldown_minutes=240) phase-locks the trigger's evaluation slots: the engine
// stamps last_checked on EVERY evaluation (fire or not), so evaluations recur
// at fixed wall-clock offsets, and when none of those offsets lands inside the
// cron's active window (EvaluateCronEntry's InWindow suppresses catch-up fires
// outside it) the trigger never fires again while looking perfectly alive —
// the aliasing that kept gardener ka-0000005204 dark for 6 days
// (bug ka-0000004307).
//
// Preflight is deliberately NOT evaluated here (it needs daemon queries; the
// real Evaluate applies it fail-closed). A due-but-preflight-blocked entry
// costs one extra evaluation per engine cycle for the duration of the window,
// bounded by the window's length. Per-entry throttle IS respected so a
// throttled entry cannot hold the bypass open.
func HasDueCronEntry(engine *EntryEngine, logger *slog.Logger, tctx TriggerEvalContext, now time.Time) bool {
	if engine == nil || logger == nil {
		return false
	}
	for i := range tctx.Triggers {
		entry := tctx.Triggers[i]
		if entry.Kind != TriggerKindCron {
			continue
		}
		throttle := parseDurationField(entry.Throttle, 0)
		if engine.inThrottleWindow(tctx.TriggerID, i, throttle, now) {
			continue
		}
		if EvaluateCronEntry(tctx, entry, i, now, logger).Fire {
			return true
		}
	}
	return false
}

// TriggerDaemon is the daemon-side surface needed by free-function trigger
// evaluators — the union of conditions.BeadClient (for daemon-side leaves on
// the Match expression) and conditions.AncestorClient (for in_subtree_of).
// *beadsapi.Client satisfies it transparently; tests can stub it.
type TriggerDaemon interface {
	conditions.BeadClient
	conditions.AncestorClient
}

func eventCause(entry TriggerEntry, evt TriggerEvent) string {
	switch entry.Kind {
	case TriggerKindBeadEvent:
		return fmt.Sprintf("bead_event %s %s", evt.EventType, evt.BeadID)
	case TriggerKindDecisionResolved:
		return fmt.Sprintf("decision_resolved %s", evt.BeadID)
	default:
		return entry.Kind
	}
}
