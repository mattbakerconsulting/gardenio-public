package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/scheduler"
)

// BeadStateTriggerSource implements TriggerSource for bead lifecycle event triggers.
// Unlike polling sources (JIRA, GitLab), this source is event-driven: SSE handlers
// buffer incoming bead events, and Evaluate returns matches against trigger conditions.
type BeadStateTriggerSource struct {
	logger *slog.Logger

	mu      sync.Mutex
	pending []beadStateEvent // buffered events waiting for evaluation
	// frozen, while freezing, is how many of pending the current pass sees
	// (FreezePending); later events wait for the next pass. Guarded by mu.
	frozen   int
	freezing bool
	subgraph *SubgraphResolver // optional; required for the in_subgraph_of operator (GAP-3)
	watched  *WatchedResolver  // optional; resolves the `watched` list for CEL expr conditions (F4)
	// daemon hydrates edge-event beads (dependency/comment/label/follower
	// add+remove, bead delete/transfer) whose SSE payloads carry only ids.
	// Populated by LoadTriggers. Guarded by mu.
	daemon scheduler.TriggerDaemon
	// warnedLabelGap dedups the once-per-config event_types/label-condition
	// mismatch WARN (ka-0000002382) so LoadTriggers logs it once per
	// (trigger id, event_types) rather than every reload cycle. Guarded by mu.
	warnedLabelGap map[string]bool
}

// beadStateEvent is a buffered bead lifecycle event.
type beadStateEvent struct {
	Topic string     // e.g. "beads.bead.created", "beads.bead.updated", "beads.bead.closed"
	Bead  *BeadEvent // parsed bead data
}

// beadTriggerData holds bead-state-specific fields parsed from a trigger bead.
type beadTriggerData struct {
	// EventTypes is the list of SSE event types to match (e.g. "created", "updated", "closed").
	// Empty means match all event types.
	EventTypes []string
	// Conditions is the list of field/label conditions that must all match.
	// For an assert trigger these are the event PRE-FILTER (which beads to
	// evaluate the assertion against); they suppress on a positive match the
	// way every other trigger source's conditions do.
	Conditions []beadCondition
	// Assert is the list of CEL invariant clauses for an assert trigger
	// (feature F5, ka-0000008157). Each clause is a `{"expr": "<CEL>"}`
	// predicate that is expected to hold TRUE. The trigger fires (emits a
	// match) when the invariant is VIOLATED — i.e. any clause evaluates FALSE,
	// or fails closed on a compile/eval error (a broken assertion must not
	// silently pass). Empty means this is not an assert trigger and the legacy
	// fire-on-match path applies.
	Assert []beadCondition
}

// beadCondition is a single condition in a bead state trigger.
type beadCondition struct {
	// Field is the bead field to check (e.g. "type", "status", "agent_state").
	// Special prefix "label:" checks label presence/absence.
	Field string `json:"field"`
	// Op is the comparison operator. Pure in-process ops (evaluated against
	// the bead event payload):
	//   "==", "!=", "in", "not_in", "contains", "present", "absent",
	//   "field_changed", "changed_to".
	// Time-relative (freshness) ops over an RFC3339 timestamp Field, with a Go
	// duration Value ("2h", "90m") — the scriptable-trigger JSON-DSL stopgap
	// (decision ka-0000001680), fail-closed on parse error:
	//   "older_than"  — fires when now - Field >= Value (Field is STALE).
	//   "newer_than"  — fires when now - Field <  Value (Field is FRESH).
	// Involuntary-death classifier op (ka-0000002929), keys off close_reason:
	//   "is_involuntary_death" — fires when Field (default "close_reason")
	//                            classifies as a counted involuntary mid-work
	//                            death via gardeniometrics.InvoluntaryDeathFamily
	//                            (the SAME predicate backing the
	//                            gardenio_agent_involuntary_deaths_total counter —
	//                            no new instrumentation). Value optionally narrows
	//                            to one family (e.g. "stop_gate_cooldown"); empty
	//                            matches any death. The `_involuntary` marker, NOT
	//                            the pod exit code (always 143), is the signal.
	// Daemon-backed ops (require a SubgraphResolver wired via
	// SetSubgraphResolver; introduced for GAP-3 / ka-0000007345):
	//   "in_subgraph_of"      — Value is the root bead id; matches when the
	//                           bead identified by Field (default "id") sits
	//                           in the connected subgraph rooted there.
	//   "not_in_subgraph_of"  — inverse of in_subgraph_of.
	Op string `json:"op"`
	// Value is the comparison value. For "in"/"not_in", comma-separated list.
	Value string `json:"value"`
	// Expr is a sandboxed CEL expression (feature F4, ka-0000008047). When
	// non-empty, this condition is an expression condition: Field/Op/Value are
	// ignored and the CEL predicate is evaluated against the typed context
	// (bead, event, now, watched). The expression must type-check to bool and
	// is compiled-on-write (rejected at authoring time) and cached. Evaluation
	// is cost-limited, time-bounded, I/O-free, and FAIL-CLOSED: a compile or
	// eval error treats the condition as not-matched (the trigger does not
	// fire) rather than silently passing. See trigger_expr.go.
	Expr string `json:"expr,omitempty"`
}

// isExpr reports whether this condition is a CEL expression condition (the
// `{"expr": "<CEL>"}` kind) rather than the legacy field/op/value kind.
func (c beadCondition) isExpr() bool { return strings.TrimSpace(c.Expr) != "" }

// NewBeadStateTriggerSource creates a new bead state trigger source.
func NewBeadStateTriggerSource(logger *slog.Logger) *BeadStateTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &BeadStateTriggerSource{
		logger: logger,
	}
}

func (s *BeadStateTriggerSource) Name() string { return "bead" }

// SetSubgraphResolver wires the connected-subgraph resolver used by the
// `in_subgraph_of` condition operator (GAP-3, ka-0000007345). When unset,
// any trigger condition using that operator evaluates to false and logs at
// warn level — the trigger does not fire rather than firing on every event.
// Optional so test fixtures and minimal wiring keep working.
func (s *BeadStateTriggerSource) SetSubgraphResolver(r *SubgraphResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subgraph = r
}

// SetWatchedResolver wires the resolver that materializes the `watched` list
// exposed to CEL expression conditions (feature F4, ka-0000008047). When
// unset, expressions still evaluate but `watched` is an empty list — an
// expression that quantifies over watched simply under-matches rather than
// firing on every event. Optional so test fixtures and minimal wiring keep
// working.
func (s *BeadStateTriggerSource) SetWatchedResolver(w *WatchedResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watched = w
}

// SkipRuntimeBootstrap opts this source out of the engine's runtime-bootstrap
// guard. Bead-state triggers are event-driven: Evaluate drains a buffer of SSE
// events received since the last call — every match is a genuinely fresh
// event, not historical state.
func (s *BeadStateTriggerSource) SkipRuntimeBootstrap() bool { return true }

// RegisterSSE registers SSE handlers on the given stream to capture bead lifecycle events.
//
// Coverage: subscribes to bead create/update/close (full payload) plus the
// edge-shaped events catalogued in edgeEventTopics (graph mutations that
// affect a bead without carrying its full state — added under ka-0000001427).
// Edge SSE payloads carry only bead ids, so each affected bead is hydrated
// via the daemon (GetBead) before buffering so label/field/type/status
// conditions can match. Hydration fails open: a nil daemon or GetBead error
// skips the event rather than buffering an unmatchable id-only one.
func (s *BeadStateTriggerSource) RegisterSSE(stream *SSEStream) {
	beadTopics := []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"}
	for _, topic := range beadTopics {
		t := topic // capture
		stream.On(t, func(_ context.Context, data []byte) {
			bead := ParseBeadEvent(data)
			if bead == nil {
				return
			}
			s.mu.Lock()
			s.pending = append(s.pending, beadStateEvent{Topic: t, Bead: bead})
			s.mu.Unlock()
		})
	}
	for _, topic := range edgeEventTopics {
		t := topic
		stream.On(t, func(ctx context.Context, data []byte) {
			ids := ParseEdgeEventBeadIDs(t, data)
			if len(ids) == 0 {
				return
			}
			// Snapshot the daemon; LoadTriggers populates it. Fail open when
			// hydration is unavailable — an id-only BeadEvent cannot satisfy
			// type/status/label/field conditions, so buffering one would only
			// produce false negatives that look like matches never firing.
			s.mu.Lock()
			daemon := s.daemon
			s.mu.Unlock()
			hydrator, canHydrate := daemon.(beadHydrator)
			if daemon == nil || !canHydrate {
				s.logger.Debug("bead-state edge event: no hydrator available; skipping id-only event",
					"topic", t, "ids", ids)
				return
			}
			for _, id := range ids {
				detail, err := hydrator.GetBead(ctx, id)
				if err != nil || detail == nil {
					s.logger.Debug("bead-state edge event: hydration failed; skipping id-only event",
						"bead_id", id, "topic", t, "error", err)
					continue
				}
				// beadEventFromDetail (dedup.go) returns a BeadEvent value
				// carrying type/status/labels/fields — exactly the columns
				// the id-only SSE edge payload lacks. Buffer a pointer to it.
				ev := beadEventFromDetail(detail)
				s.mu.Lock()
				s.pending = append(s.pending, beadStateEvent{
					Topic: t,
					Bead:  &ev,
				})
				s.mu.Unlock()
			}
		})
	}
	s.logger.Info("bead-state triggers: registered SSE handlers",
		"bead_topics", beadTopics, "edge_topics", edgeEventTopics)
}

// LoadTriggers fetches all enabled trigger beads with label "trigger:bead".
func (s *BeadStateTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	if bd, ok := daemon.(scheduler.TriggerDaemon); ok {
		s.mu.Lock()
		s.daemon = bd
		s.mu.Unlock()
	}

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Labels:   []string{"trigger:bead"},
		Limit:    100,
	})
	if err != nil {
		return nil, err
	}

	var triggers []TriggerDef
	for _, bead := range result.Beads {
		if bead.Fields["enabled"] == "false" {
			continue
		}

		// Skip beads-viewer-e2e fixtures so the shared prod scheduler never
		// fires a test fixture (ka-0000005435). enabled is left untouched so
		// the bead still renders in beads-viewer.
		if isE2EFixture(bead) {
			s.logger.Info("bead trigger source: skipping E2E fixture (never fires in prod)",
				"trigger", bead.ID, "title", bead.Title)
			continue
		}

		t := ParseTriggerDef(bead, "bead")
		data := beadTriggerData{}

		// Parse event_types field (comma-separated list of event types).
		if et := bead.Fields["event_types"]; et != "" {
			for e := range strings.SplitSeq(et, ",") {
				e = strings.TrimSpace(e)
				if e != "" {
					data.EventTypes = append(data.EventTypes, e)
				}
			}
		}

		// Parse conditions from JSON array field.
		if cond := bead.Fields["conditions"]; cond != "" {
			var conditions []beadCondition
			if err := json.Unmarshal([]byte(cond), &conditions); err != nil {
				s.logger.Warn("bead-state trigger: invalid conditions JSON",
					"trigger", bead.ID, "error", err)
				continue
			}
			// Compile-on-load guard for CEL expression conditions: pre-compile
			// (and cache) every expr so a malformed expression that slipped past
			// authoring-time validation is caught here and the whole trigger is
			// skipped (fail-closed) rather than failing on the hot path. The
			// compiled program is memoised, so Evaluate's CompileExpr is a
			// cache hit.
			badExpr := false
			for _, c := range conditions {
				if !c.isExpr() {
					continue
				}
				if _, err := CompileExpr(c.Expr); err != nil {
					s.logger.Warn("bead-state trigger: invalid CEL expression; skipping trigger",
						"trigger", bead.ID, "expr", c.Expr, "error", err)
					badExpr = true
					break
				}
			}
			if badExpr {
				continue
			}
			data.Conditions = conditions
		}

		// Parse assert invariant clauses (feature F5, ka-0000008157). Same
		// JSON shape as conditions ([]beadCondition), but every clause must be
		// a CEL expression and is compiled-on-load (fail-closed: a malformed
		// assertion skips the whole trigger rather than firing on every event).
		if assertJSON := bead.Fields["assert"]; assertJSON != "" {
			var assertions []beadCondition
			if err := json.Unmarshal([]byte(assertJSON), &assertions); err != nil {
				s.logger.Warn("bead-state trigger: invalid assert JSON",
					"trigger", bead.ID, "error", err)
				continue
			}
			badAssert := false
			for _, c := range assertions {
				if !c.isExpr() {
					s.logger.Warn("bead-state trigger: assert clause is not a CEL expression; skipping trigger",
						"trigger", bead.ID, "clause", c)
					badAssert = true
					break
				}
				if _, err := CompileExpr(c.Expr); err != nil {
					s.logger.Warn("bead-state trigger: invalid assert CEL expression; skipping trigger",
						"trigger", bead.ID, "expr", c.Expr, "error", err)
					badAssert = true
					break
				}
			}
			if badAssert {
				continue
			}
			data.Assert = assertions
		}

		// Authoring guardrail (ka-0000002382): a `label:` condition can only be
		// satisfied by a label mutation, which the daemon delivers ONLY as the
		// `beads.label.added` / `beads.label.removed` edge topics — never as a
		// `beads.bead.updated` event (handleAddLabel/handleRemoveLabel publish
		// the edge topic alone; a label change does not bump updated_at). So a
		// trigger that gates on `label:X:present` while restricting event_types
		// to e.g. `updated` is over-constrained: the arming label-add is
		// filtered out at matchEventType BEFORE conditions/cooldown/hydration
		// ever run, and the trigger silently never fires on the label change
		// (the MR-D publish-fix stall — it advanced only because an unrelated
		// description touch later emitted a real `updated`). Warn loudly so the
		// footgun is caught in logs rather than as a dark trigger.
		s.warnLabelConditionEventTypeGap(t.ID, data)

		t.SourceData = data
		triggers = append(triggers, t)
	}
	return triggers, nil
}

// labelEdgeEventTypes are the SSE event-type forms (leaf, short, and full
// topic) that actually deliver a label mutation to matchEventType. A trigger
// whose conditions reference `label:` must accept at least one of these in
// event_types, or leave event_types empty (match-all), for the label change to
// wake it.
var labelEdgeEventTypes = map[string]bool{
	"label.added":         true,
	"label.removed":       true,
	"beads.label.added":   true,
	"beads.label.removed": true,
	// The bare leaf "added"/"removed" also matches label.added/removed in
	// matchEventType (leaf compare), so accept them as covering the gap too —
	// even though the docs discourage the ambiguous leaf form.
	"added":   true,
	"removed": true,
}

// warnLabelConditionEventTypeGap logs a one-shot WARN when a trigger gates on a
// `label:` condition yet its non-empty event_types filter admits none of the
// label edge topics — the over-constraint that dark-holed the MR-D publish-fix
// trigger (ka-0000002382). It is diagnostic only: it never mutates the trigger
// or suppresses loading, so an author who genuinely wants a label predicate
// gated to (say) `closed` events keeps that behavior — they just get told the
// label-add itself will not wake it. Deduped per (trigger id, event_types) via
// warnedLabelGap so a steady reload loop logs once, not every cycle.
func (s *BeadStateTriggerSource) warnLabelConditionEventTypeGap(triggerID string, data beadTriggerData) {
	// Empty event_types means match-all, which already admits label.added —
	// no gap. Nothing to warn about.
	if len(data.EventTypes) == 0 {
		return
	}
	if !conditionsReferenceLabel(data.Conditions) {
		return
	}
	for _, et := range data.EventTypes {
		if labelEdgeEventTypes[strings.TrimSpace(et)] {
			return // a label edge topic is admitted — the label-add can wake it
		}
	}

	dedupKey := triggerID + "|" + strings.Join(data.EventTypes, ",")
	s.mu.Lock()
	if s.warnedLabelGap == nil {
		s.warnedLabelGap = make(map[string]bool)
	}
	already := s.warnedLabelGap[dedupKey]
	if !already {
		s.warnedLabelGap[dedupKey] = true
	}
	s.mu.Unlock()
	if already {
		return
	}

	s.logger.Warn("bead-state trigger: label condition can never be woken by its event_types — add label.added (and/or label.removed) to event_types, or clear event_types to match all",
		"trigger", triggerID,
		"event_types", strings.Join(data.EventTypes, ","),
		"hint", "a label change is delivered only as the beads.label.added/removed edge topic, never as beads.bead.updated")
}

// conditionsReferenceLabel reports whether any legacy field/op/value condition
// keys off a bead label (Field prefixed "label:"). Expression (CEL) conditions
// are excluded: they read a bead's labels from the typed context and are not
// tied to the label-add edge topic in the same mechanical way, and their event
// coupling is the author's to reason about.
func conditionsReferenceLabel(conditions []beadCondition) bool {
	for _, c := range conditions {
		if c.isExpr() {
			continue
		}
		if strings.HasPrefix(c.Field, "label:") {
			return true
		}
	}
	return false
}

// Evaluate drains pending bead events and returns matches for the given trigger.
func (s *BeadStateTriggerSource) Evaluate(ctx context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	data, ok := trig.SourceData.(beadTriggerData)
	if !ok {
		return nil, nil
	}

	// Drain pending events.
	s.mu.Lock()
	events := s.visiblePending()
	resolver := s.subgraph
	watched := s.watched
	// Don't clear pending here — other triggers need to evaluate the same events.
	// The engine's dedup prevents re-firing for the same trigger+key combination.
	s.mu.Unlock()

	// Capture the clock once per Evaluate cycle so every time-relative
	// predicate (the older_than/newer_than ops and CEL `now`) across all
	// events in this cycle sees a single consistent instant.
	now := timeNow()

	var matches []TriggerMatch
	for _, evt := range events {
		if !matchEventType(data.EventTypes, evt.Topic) {
			continue
		}

		eventName := beadEventName(evt.Topic)
		if !s.matchConditionsCtx(ctx, data.Conditions, evt.Bead, resolver, watched, eventName, now, trig.ID) {
			continue
		}

		// Assert triggers (feature F5, ka-0000008157) invert the firing rule:
		// the `conditions` above are just the event pre-filter (which beads to
		// check), and the trigger fires only when the `assert` invariant is
		// VIOLATED. A satisfied invariant stays silent. When no assert clauses
		// are declared this is a legacy fire-on-match trigger and we emit the
		// plain match.
		if len(data.Assert) > 0 {
			violated, observed := s.evalAssert(ctx, data.Assert, evt.Bead, resolver, watched, eventName, now, trig.ID)
			if violated == "" {
				continue // invariant holds → silent
			}
			matches = append(matches, beadAssertViolationToMatch(evt, violated, observed))
			continue
		}

		matches = append(matches, beadEventToMatch(evt))
	}
	return matches, nil
}

// EventReady reports whether at least one buffered event would produce a match
// for this trigger — read-only: it consumes no event and records no fire. The
// bridge engine's cooldown gate calls this so a bead-state trigger with a real
// matching event pending can bypass the per-trigger cooldown, while cron/idle
// re-eval stays cooldown-gated.
//
// Fixes the cooldown/event race for the plain bead source (ka-0000001376), the
// exact hole EventReady already closed for MultiTriggerSource (bug
// ka-0000005988): without it, a bead_event arriving while last_checked is inside
// the 30s cooldown floor is buffered, never consumed (evalOne cooldown-skips the
// trigger BEFORE Evaluate runs), then dropped at cycle end — a silently lost
// fire. This bit the trigger-action-fidelity E2E as a flake (a live staging
// trigger-engine stamps last_checked on the test trigger, racing the probe's
// EvaluateSource) and is a live event-loss vector in prod all the same.
//
// It mirrors Evaluate's pre-filter — event-type then conditions — so a pending
// event only bypasses cooldown when it genuinely matches. It deliberately does
// NOT run the assert-invariant leg: an assert trigger fires on VIOLATION, so a
// matching-but-satisfied event would (correctly) produce no fire, yet the event
// is still pending and must be evaluated THIS cycle rather than drained-and-lost
// — so a matched pre-filter is sufficient reason to bypass cooldown. Evaluate
// still applies the assert leg and stays silent when the invariant holds.
func (s *BeadStateTriggerSource) EventReady(ctx context.Context, trig TriggerDef) bool {
	data, ok := trig.SourceData.(beadTriggerData)
	if !ok {
		return false
	}

	s.mu.Lock()
	events := s.visiblePending()
	resolver := s.subgraph
	watched := s.watched
	s.mu.Unlock()

	// Single consistent instant for any time-relative predicate, matching
	// Evaluate.
	now := timeNow()

	for _, evt := range events {
		if !matchEventType(data.EventTypes, evt.Topic) {
			continue
		}
		eventName := beadEventName(evt.Topic)
		if s.matchConditionsCtx(ctx, data.Conditions, evt.Bead, resolver, watched, eventName, now, trig.ID) {
			return true
		}
	}
	return false
}

// RecoveredKeys returns the assert violation dedup keys whose invariant now
// HOLDS again for a pending event, so the engine can re-arm them — drop them
// from its `seen` set — and a future violation of the same clause re-alerts once
// per episode instead of staying suppressed until the 24h seen-key TTL evicts it
// (bug ka-0000006401). It is the recovery half of the episode-dedup contract that
// assertViolationKey's poll-stable key sets up, mirroring the once-per-episode
// re-arm of SweepGardenerLiveness.markFresh.
//
// Read-only and side-effect free, exactly like EventReady: it re-scans the same
// pending buffer Evaluate reads (draining nothing), applies the same event-type
// + conditions pre-filter, and for each pre-filtered event whose assert invariant
// is SATISFIED returns the would-be violation key of EVERY assert clause. Because
// clearing a key that was never set is a harmless no-op, emitting all clause keys
// needs no memory of which clause previously fired. Returns nil for non-assert
// triggers (the fast path for the vast majority of bead triggers).
func (s *BeadStateTriggerSource) RecoveredKeys(ctx context.Context, trig TriggerDef) []string {
	data, ok := trig.SourceData.(beadTriggerData)
	if !ok || len(data.Assert) == 0 {
		return nil
	}

	s.mu.Lock()
	events := s.visiblePending()
	resolver := s.subgraph
	watched := s.watched
	s.mu.Unlock()

	// Single consistent instant for any time-relative predicate, matching
	// Evaluate.
	now := timeNow()

	var recovered []string
	for _, evt := range events {
		if !matchEventType(data.EventTypes, evt.Topic) {
			continue
		}
		eventName := beadEventName(evt.Topic)
		if !s.matchConditionsCtx(ctx, data.Conditions, evt.Bead, resolver, watched, eventName, now, trig.ID) {
			continue
		}
		if violated, _ := s.evalAssert(ctx, data.Assert, evt.Bead, resolver, watched, eventName, now, trig.ID); violated != "" {
			continue // still violating → nothing to re-arm
		}
		// Invariant holds for this bead this cycle → re-arm every clause's episode
		// key. Use c.Expr verbatim (the exact string evalAssert returns as the
		// violated clause) so the key matches the one beadAssertViolationToMatch
		// minted, both funnelled through assertViolationKey.
		for _, c := range data.Assert {
			if strings.TrimSpace(c.Expr) == "" {
				continue
			}
			recovered = append(recovered, assertViolationKey(evt.Bead.ID, c.Expr))
		}
	}
	return recovered
}

// evalAssert evaluates the assert invariant clauses against a bead event and
// reports the first VIOLATION (feature F5, ka-0000008157). Each clause is a CEL
// predicate expected to hold TRUE; the invariant is violated when any clause
// evaluates FALSE or fails closed (compile/eval error, non-bool result,
// watched-resolution failure) — a broken assertion must fire, not silently
// pass. Returns the violated clause's source string (empty when the invariant
// holds across every clause) plus a human-readable rendering of the observed
// field values to surface in the alert.
func (s *BeadStateTriggerSource) evalAssert(ctx context.Context, clauses []beadCondition, bead *BeadEvent, resolver *SubgraphResolver, watched *WatchedResolver, eventType string, now time.Time, triggerID string) (violated string, observed string) {
	_ = resolver // assert clauses are CEL-only; resolver is unused but kept for signature parity
	for _, c := range clauses {
		// matchExprCondition is FAIL-CLOSED: it returns false on a compile or
		// eval error. For an assert that "false" means VIOLATED — exactly the
		// fire-because-broken semantic we want (the inverse of a suppress-gate
		// condition, where fail-closed means do-not-fire).
		held := s.matchExprCondition(ctx, c, bead, watched, eventType, now, triggerID)
		if !held {
			return c.Expr, observedValues(c.Expr, bead)
		}
	}
	return "", ""
}

// visiblePending returns the events this pass sees: the frozen ones after
// FreezePending, else every buffered one. Caller holds mu.
func (s *BeadStateTriggerSource) visiblePending() []beadStateEvent {
	if s.freezing {
		return s.pending[:s.frozen]
	}
	return s.pending
}

// FreezePending fixes the events the coming full Evaluate pass sees to those
// buffered now. An event that arrives during the pass waits for the next one
// instead of being drained unseen by the triggers already visited
// (gd-IQcMk16PkH).
func (s *BeadStateTriggerSource) FreezePending() {
	s.mu.Lock()
	s.frozen, s.freezing = len(s.pending), true
	s.mu.Unlock()
}

// DrainPending clears the events the pass saw: those FreezePending froze, or
// the whole buffer when the pass was not frozen. Call after all triggers have
// been evaluated in a cycle to prevent unbounded growth.
func (s *BeadStateTriggerSource) DrainPending() {
	s.mu.Lock()
	if s.freezing {
		s.pending = slices.Clone(s.pending[s.frozen:])
		s.frozen, s.freezing = 0, false
	} else {
		s.pending = s.pending[:0]
	}
	s.mu.Unlock()
}

// InjectBeadEvent appends a synthetic bead event to the pending buffer. Used
// by the reconciler (ka-0000004769) to replay bead mutations that the SSE
// stream missed — for example after a postgres failover that wiped the
// daemon's in-memory ring buffer. The topic is preserved on the buffered
// event so handlers and the dedup-key derivation in beadEventToMatch can
// distinguish reconciled events ("beads.bead.reconciled") from live SSE
// deliveries.
func (s *BeadStateTriggerSource) InjectBeadEvent(topic string, bead *BeadEvent) {
	if bead == nil || topic == "" {
		return
	}
	s.mu.Lock()
	s.pending = append(s.pending, beadStateEvent{Topic: topic, Bead: bead})
	s.mu.Unlock()
}

// PendingCount returns the number of buffered events (for testing/monitoring).
func (s *BeadStateTriggerSource) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// CatchUpKeys returns dedup keys for existing beads created by this trigger.
func (s *BeadStateTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open", "in_progress", "closed"},
		Labels:   []string{"trigger:" + triggerID},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}

	var keys []string
	for _, bead := range result.Beads {
		if key := bead.Fields["bead_event_key"]; key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// matchEventType checks if the SSE topic matches the trigger's event type filter.
// Accepts three forms for backward compatibility and ergonomics:
//   - full topic:  "beads.dependency.added"
//   - short form:  "dependency.added" (everything after "beads.")
//   - leaf only:   "added"
//
// Authors should prefer the short form for edge topics ("dependency.added",
// "label.added") because the leaf alone is ambiguous across many edge events.
func matchEventType(eventTypes []string, topic string) bool {
	if len(eventTypes) == 0 {
		return true // no filter means match all
	}

	leaf := topic
	if idx := strings.LastIndex(topic, "."); idx >= 0 {
		leaf = topic[idx+1:]
	}
	short := strings.TrimPrefix(topic, "beads.")

	for _, et := range eventTypes {
		if et == leaf || et == topic || et == short {
			return true
		}
	}
	return false
}

// beadEventName derives the event name surfaced to template vars, the dedup
// key, and the CEL `event.type` context from an SSE topic. For the original
// three full-payload topics (beads.bead.{created,updated,closed,reconciled})
// it preserves the historical leaf-only form ("created") so existing dedup
// state and template variables keep working. For edge-event topics added
// later, the leaf alone is ambiguous (dependency.added, label.added,
// comment.added, follower.added all collapse to "added"), so it uses the
// longer "<topic-tail>" form, e.g. "dependency.added".
func beadEventName(topic string) string {
	switch topic {
	case "beads.bead.created", "beads.bead.updated", "beads.bead.closed", "beads.bead.reconciled":
		if idx := strings.LastIndex(topic, "."); idx >= 0 {
			return topic[idx+1:]
		}
		return topic
	default:
		return strings.TrimPrefix(topic, "beads.")
	}
}

// matchConditionsCtx is the ctx-aware variant of matchConditions used by
// Evaluate. It resolves daemon-backed operators (in_subgraph_of) via the
// optional SubgraphResolver, evaluates CEL expression conditions
// ({"expr": "<CEL>"}) via the optional WatchedResolver, and falls back to
// matchOneCondition for the pure in-process ops. All three kinds AND together:
// every condition must match for the trigger to fire.
func (s *BeadStateTriggerSource) matchConditionsCtx(ctx context.Context, conditions []beadCondition, bead *BeadEvent, resolver *SubgraphResolver, watched *WatchedResolver, eventType string, now time.Time, triggerID string) bool {
	for _, c := range conditions {
		// Expression conditions are dispatched on the presence of Expr, not on
		// Op, so a `{"expr": "..."}` condition needs no Op at all.
		if c.isExpr() {
			if !s.matchExprCondition(ctx, c, bead, watched, eventType, now, triggerID) {
				return false
			}
			continue
		}
		switch c.Op {
		case "in_subgraph_of", "not_in_subgraph_of":
			if !s.matchSubgraphCondition(ctx, c, bead, resolver, triggerID) {
				return false
			}
			continue
		}
		if !matchOneCondition(c, bead) {
			return false
		}
	}
	return true
}

// matchExprCondition evaluates a CEL expression condition (feature F4,
// ka-0000008047). It compiles (cache-hit on the hot path) the expression,
// resolves the bounded `watched` list rooted at the event bead, and evaluates
// the predicate against the typed context. FAIL-CLOSED: a compile error, a
// watched-resolution error, or an eval error/timeout/cost-overrun all return
// false — the trigger does not fire rather than firing on a broken assertion.
func (s *BeadStateTriggerSource) matchExprCondition(ctx context.Context, c beadCondition, bead *BeadEvent, watched *WatchedResolver, eventType string, now time.Time, triggerID string) bool {
	compiled, err := CompileExpr(c.Expr)
	if err != nil {
		// Should be unreachable: expressions are compile-on-write rejected at
		// authoring time. If a malformed expression reaches here (e.g. a bead
		// hand-edited around the CLI), fail closed and log loudly.
		s.logger.Warn("bead-state trigger: CEL expression failed to compile at eval time; treating as no-match", // OK: "eval time" is descriptive prose in a log message, not a shell eval invocation
			"trigger", triggerID, "expr", c.Expr, "error", err)
		return false
	}

	// Resolve the bounded watched list rooted at the event bead. A resolver
	// error fails the condition closed; a nil resolver yields an empty list
	// (an expression that does not reference `watched` still evaluates).
	var watchedBeads []*BeadEvent
	if watched != nil && bead != nil {
		wb, werr := watched.Resolve(ctx, bead.ID)
		if werr != nil {
			s.logger.Warn("bead-state trigger: watched-list resolution failed for CEL expr; treating as no-match",
				"trigger", triggerID, "root", bead.ID, "error", werr)
			return false
		}
		watchedBeads = wb
	}

	res := compiled.Eval(ctx, bead, eventType, now, watchedBeads)
	if res.Err != nil {
		s.logger.Warn("bead-state trigger: CEL expr evaluation failed; treating as no-match (fail-closed)",
			"trigger", triggerID, "expr", c.Expr, "error", res.Err)
		return false
	}
	return res.Matched
}

// matchSubgraphCondition evaluates the in_subgraph_of / not_in_subgraph_of
// operators against the bead event using the resolver. The condition fires
// when the bead identified by `c.Field` (default "id") sits in (or outside,
// for not_in_subgraph_of) the connected subgraph rooted at `c.Value`. A
// missing resolver, missing root, or resolver error yields false — the
// trigger does not fire rather than firing on every event.
func (s *BeadStateTriggerSource) matchSubgraphCondition(ctx context.Context, c beadCondition, bead *BeadEvent, resolver *SubgraphResolver, triggerID string) bool {
	if resolver == nil {
		s.logger.Warn("bead-state trigger: in_subgraph_of op needs SubgraphResolver but none is configured; treating as no-match",
			"trigger", triggerID, "field", c.Field, "root", c.Value)
		return false
	}
	if c.Value == "" {
		s.logger.Warn("bead-state trigger: in_subgraph_of needs a non-empty value (root bead id)",
			"trigger", triggerID, "field", c.Field)
		return false
	}
	candidateField := c.Field
	if candidateField == "" {
		candidateField = "id"
	}
	candidate := getBeadField(candidateField, bead)
	if candidate == "" {
		return c.Op == "not_in_subgraph_of"
	}
	in, err := resolver.IsInSubgraph(ctx, c.Value, candidate)
	if err != nil {
		s.logger.Warn("bead-state trigger: subgraph resolution failed",
			"trigger", triggerID, "root", c.Value, "candidate", candidate, "error", err)
		return false
	}
	if c.Op == "not_in_subgraph_of" {
		return !in
	}
	return in
}

// matchOneCondition evaluates a single condition against a bead event.
func matchOneCondition(c beadCondition, bead *BeadEvent) bool {
	// Diff-aware operators query the BeadUpdated Changes map so authors can
	// fire on real field transitions (e.g. status open -> in_progress) rather
	// than every BeadUpdated event. Evaluated before the label-prefix branch
	// because the field operand is a plain bead field name (which may itself
	// be "label:..." — diff-aware label transitions are not supported yet).
	switch c.Op {
	case "field_changed":
		return changeMapHas(bead.Changes, c.Field)
	case "changed_to":
		newVal, ok := changeMapLookup(bead.Changes, c.Field)
		return ok && newVal == c.Value
	}

	// Label conditions use special "label:" prefix.
	if labelKey, ok := strings.CutPrefix(c.Field, "label:"); ok {
		return matchLabelCondition(c.Op, labelKey, bead.Labels)
	}

	// Get the field value from the bead.
	fieldVal := getBeadField(c.Field, bead)

	switch c.Op {
	case "==", "eq":
		return fieldVal == c.Value
	case "!=", "ne":
		return fieldVal != c.Value
	case "in":
		for v := range strings.SplitSeq(c.Value, ",") {
			if strings.TrimSpace(v) == fieldVal {
				return true
			}
		}
		return false
	case "not_in":
		for v := range strings.SplitSeq(c.Value, ",") {
			if strings.TrimSpace(v) == fieldVal {
				return false
			}
		}
		return true
	case "contains":
		return strings.Contains(fieldVal, c.Value)
	case "present":
		return fieldVal != ""
	case "absent":
		return fieldVal == ""
	case "older_than", "newer_than":
		// Time-relative (freshness) operators — the scriptable-trigger JSON-DSL
		// stopgap from decision ka-0000001680 ("JSON and plan cel in parallel"),
		// shippable ahead of the full CEL evaluator (F4 ka-0000008047). Field is
		// a timestamp bead field written in RFC3339 (e.g. last_checked, see
		// triggers.go); Value is a Go duration ("2h", "90m"). older_than fires
		// when now - ts >= duration (the field is STALE); newer_than is the
		// inverse (FRESH). Fail-closed: a missing field, an unparseable timestamp,
		// or an unparseable duration yields false so a broken freshness assertion
		// never silently passes — mirroring the in_subgraph_of fail-closed stance.
		return matchTimeRelative(c.Op, fieldVal, c.Value)
	case "is_involuntary_death":
		// Involuntary-death classifier op (ka-0000002929): fires when the
		// close_reason field denotes a counted involuntary mid-work death. It
		// delegates to gardeniometrics.InvoluntaryDeathFamily — the SAME predicate
		// that gates the gardenio_agent_involuntary_deaths_total counter — so a
		// forensics gardener keyed on this op fires on EXACTLY the population the
		// metric counts (no drift by construction, no new instrumentation). Field
		// defaults to "close_reason"; Value optionally narrows to a single family
		// (e.g. "stop_gate_cooldown") so distinct gardeners can target distinct
		// death shapes. The `_involuntary` marker — NOT the pod exit code, which
		// is always 143 — is the discriminator, exactly as the counter requires.
		reason := fieldVal
		if c.Field == "" {
			// getBeadField("") resolved to "" above; default to close_reason.
			reason = getBeadField("close_reason", bead)
		}
		return matchInvoluntaryDeath(reason, c.Value)
	default:
		return false
	}
}

// matchInvoluntaryDeath evaluates the is_involuntary_death operator. reasonVal
// is the bead field value (the close_reason). wantFamily, when non-empty,
// requires the classified family to equal it — so a single family (e.g.
// "context_cancelled") can be targeted; an empty wantFamily matches any counted
// death. Fail-closed on an empty/unclassified reason (returns false), mirroring
// the counter, which increments nothing when InvoluntaryDeathFamily returns
// not-ok.
func matchInvoluntaryDeath(reasonVal, wantFamily string) bool {
	family, ok := gardeniometrics.InvoluntaryDeathFamily(reasonVal)
	if !ok {
		return false
	}
	if wantFamily != "" {
		return family == wantFamily
	}
	return true
}

// matchTimeRelative evaluates the older_than / newer_than operators. tsVal is
// the bead field value (expected RFC3339); dur is a Go duration string. Returns
// false on any parse failure or empty input (fail-closed). now is captured via
// timeNow so tests can pin it.
func matchTimeRelative(op, tsVal, dur string) bool {
	if tsVal == "" || dur == "" {
		return false
	}
	d, err := time.ParseDuration(dur)
	if err != nil || d < 0 {
		return false
	}
	ts, err := parseBeadTimestamp(tsVal)
	if err != nil {
		return false
	}
	age := timeNow().Sub(ts)
	if op == "older_than" {
		return age >= d
	}
	// newer_than
	return age < d
}

// timeNow is the clock used by the time-relative operators; overridable in
// tests. Defaults to wall-clock UTC-agnostic time.Now (Sub is timezone-safe).
var timeNow = time.Now

// parseBeadTimestamp parses a bead timestamp field. Bead timestamps are written
// in RFC3339 (e.g. last_checked at triggers.go), but tolerate the
// space-separated "2006-01-02 15:04:05" form that kd's human output uses so an
// author who copy-pastes a displayed timestamp into a test still gets a parse.
func parseBeadTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02 15:04:05", s)
}

// changeMapHas reports whether the BeadUpdated Changes map carries an entry
// for the named field. Returns false when the map is nil (e.g. on created /
// closed events that do not populate Changes).
func changeMapHas(changes map[string]any, field string) bool {
	if changes == nil {
		return false
	}
	_, ok := changes[field]
	return ok
}

// changeMapLookup returns the stringified new value for `field` from the
// Changes map. The second return is false when the field was not in the map.
// Non-string scalars (numbers, bools) are coerced via fmt-style stringification
// so that authors can write `op:changed_to value:in_progress` regardless of
// whether the payload arrived as JSON string or number.
func changeMapLookup(changes map[string]any, field string) (string, bool) {
	if changes == nil {
		return "", false
	}
	v, ok := changes[field]
	if !ok {
		return "", false
	}
	return stringifyChangeValue(v), true
}

// changesFingerprint returns a deterministic short string summarising the
// Changes map so that distinct field transitions hash to distinct dedup keys.
// Keys are sorted; values are stringified by stringifyChangeValue. Returns an
// empty string for a nil/empty map (caller is expected to skip the
// fingerprint suffix in that case).
func changesFingerprint(changes map[string]any) string {
	if len(changes) == 0 {
		return ""
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(stringifyChangeValue(changes[k]))
	}
	return b.String()
}

// stringifyChangeValue coerces an arbitrary JSON-decoded Changes value to the
// string form used by condition Value matching. nil renders as the empty
// string so that `changed_to value:""` can match an explicit clear.
func stringifyChangeValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	case float64:
		// json.Unmarshal decodes numbers as float64 by default. Use the
		// integer form when the value is whole so status codes / priorities
		// don't render as "2.000000".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

// matchLabelCondition checks label presence/absence.
func matchLabelCondition(op, labelKey string, labels []string) bool {
	// Check for exact label match or prefix match (e.g. "project:" matches "project:gardenio").
	hasLabel := false
	for _, l := range labels {
		if l == labelKey || (strings.HasSuffix(labelKey, ":") && strings.HasPrefix(l, labelKey)) {
			hasLabel = true
			break
		}
	}

	switch op {
	case "present", "==":
		return hasLabel
	case "absent", "!=":
		return !hasLabel
	default:
		return hasLabel // default to presence check
	}
}

// getBeadField extracts a field value from a BeadEvent, checking both top-level
// fields and the extensible Fields map.
func getBeadField(field string, bead *BeadEvent) string {
	switch field {
	case "type":
		return bead.Type
	case "status":
		return bead.Status
	case "title":
		return bead.Title
	case "assignee":
		return bead.Assignee
	case "created_by":
		return bead.CreatedBy
	case "closed_by":
		return bead.ClosedBy
	case "id":
		return bead.ID
	default:
		// Check extensible fields map.
		if bead.Fields != nil {
			return bead.Fields[field]
		}
		return ""
	}
}

// beadEventToMatch converts a buffered bead event to a TriggerMatch.
func beadEventToMatch(evt beadStateEvent) TriggerMatch {
	bead := evt.Bead

	// Event name surfaced to template vars and the dedup key. See beadEventName.
	eventName := beadEventName(evt.Topic)

	// Dedup key: bead ID + event type to prevent re-firing on same event.
	// For updated events we additionally fold a stable fingerprint of the
	// Changes map into the key so distinct field transitions (status
	// open→in_progress, then in_progress→closed) each get to fire once,
	// rather than collapsing on the first updated event for the bead. This is
	// required for the field_changed / changed_to operators — without it, a
	// trigger that fires on `status changed_to in_progress` could match only
	// once per bead ID even across unrelated later updates.
	key := bead.ID + ":" + eventName
	if eventName == "updated" && len(bead.Changes) > 0 {
		if fp := changesFingerprint(bead.Changes); fp != "" {
			key = key + ":" + fp
		}
	}

	vars := map[string]string{
		"{{bead_id}}":       bead.ID,
		"{{bead_type}}":     bead.Type,
		"{{bead_title}}":    bead.Title,
		"{{bead_status}}":   bead.Status,
		"{{bead_event}}":    eventName,
		"{{bead_assignee}}": bead.Assignee,
	}

	// Add all extensible fields as template vars.
	for k, v := range bead.Fields {
		vars["{{"+k+"}}"] = v
	}

	// Surface stringified changed-field values so prompts can reference the
	// new value (e.g. {{changed_status}}) of a transition that fired the
	// trigger. Skipped when Changes is empty.
	for k, v := range bead.Changes {
		vars["{{changed_"+k+"}}"] = stringifyChangeValue(v)
	}

	// Add label list.
	vars["{{bead_labels}}"] = strings.Join(bead.Labels, ",")

	// Surface the classified involuntary-death family (ka-0000002929) so a
	// forensics gardener fired by the is_involuntary_death op can name the death
	// shape in its prompt (e.g. {{death_family}}=stop_gate_cooldown) without
	// re-parsing close_reason. Only added when the reason actually classifies as
	// a counted death — the SAME predicate the metric counter uses — so ordinary
	// bead-event matches carry no spurious var.
	if family, ok := gardeniometrics.InvoluntaryDeathFamily(bead.Fields["close_reason"]); ok {
		vars["{{death_family}}"] = family
	}

	fields := map[string]string{
		"bead_event_key": key,
		"source_bead_id": bead.ID,
		"source_event":   eventName,
	}

	return TriggerMatch{
		Key:          key,
		Title:        bead.Title,
		Fields:       fields,
		Labels:       []string{"source-bead:" + bead.ID},
		TemplateVars: vars,
	}
}

// beadAssertViolationToMatch builds a TriggerMatch for a VIOLATED assertion
// (feature F5, ka-0000008157). It starts from the standard bead-event match
// (so every {{bead_*}} / extensible-field var is available to the alert) and
// layers on the violation-specific vars: {{violated}} (the CEL clause that
// failed) and {{values}} (the observed field values referenced by that clause).
// The violated clause is folded into the dedup key so that distinct clause
// violations on the same bead+event each fire exactly once rather than
// collapsing onto the first.
func beadAssertViolationToMatch(evt beadStateEvent, violated, observed string) TriggerMatch {
	m := beadEventToMatch(evt)

	m.TemplateVars["{{violated}}"] = violated
	m.TemplateVars["{{values}}"] = observed

	// Episode-stable violation dedup key (bug ka-0000006401). Build it from the
	// bead ID + violated-clause fingerprint ALONE — deliberately NOT layered onto
	// the beadEventToMatch base key. That base key folds a fingerprint of the
	// event's CHANGED FIELD VALUES (for a watched trigger: last_checked /
	// last_outcome_at, which the engine restamps on every ~5min poll) into the
	// key, so a single CONTINUOUS violation minted a fresh never-seen key every
	// poll and the seen-set never deduped it — re-alerting forever (measured: 36
	// identical Slack alerts in 7h on ka-0000002431). Keying on ID+clause collapses
	// a continuous violation to ONE alert per episode; the engine re-arms it by
	// dropping the key from `seen` when the invariant holds again (RecoveredKeys),
	// matching the once-per-dark-episode behaviour of SweepGardenerLiveness. A
	// DIFFERENT clause still gets a distinct fingerprint → distinct key, preserving
	// the multi-clause property the original fold existed to protect.
	if violated != "" {
		m.Key = assertViolationKey(evt.Bead.ID, violated)
		m.Fields["bead_event_key"] = m.Key
		m.Fields["assert_violated"] = violated
	}
	if observed != "" {
		m.Fields["assert_values"] = observed
	}
	return m
}

// assertViolationKey builds the episode-stable dedup key for a violated assert
// clause (bug ka-0000006401): the watched bead's ID plus a fingerprint of the
// violated clause, and NOTHING that varies poll-to-poll. It is the sole source
// of the assert violation key so beadAssertViolationToMatch (which mints it) and
// RecoveredKeys (which re-arms it) cannot drift. See beadAssertViolationToMatch
// for why the changed-field-values segment must be excluded.
func assertViolationKey(beadID, violated string) string {
	return beadID + ":violated:" + assertClauseFingerprint(violated)
}

// assertClauseFingerprint returns a short, stable, dedup-safe token for a CEL
// clause string so it can be folded into a TriggerMatch dedup key without
// dragging the full (possibly long, colon-laden) expression into the key.
func assertClauseFingerprint(expr string) string {
	var h uint32 = 2166136261 // FNV-1a 32-bit
	for i := 0; i < len(expr); i++ {
		h ^= uint32(expr[i])
		h *= 16777619
	}
	return strconv.FormatUint(uint64(h), 16)
}

// observedValues renders the bead field values referenced by a CEL clause into
// a compact "field=value" string for the alert's {{values}} var. It scans the
// clause for `bead.<field>` references (and `bead["<field>"]` bracket form),
// resolving each against the event bead via beadFieldValue. Best-effort: a
// field that does not resolve is rendered as "<field>=<unset>". When the clause
// references no bead fields (e.g. a pure watched-quantifier), it falls back to
// a small set of identifying fields so the alert is never empty.
func observedValues(expr string, bead *BeadEvent) string {
	if bead == nil {
		return ""
	}
	fields := assertReferencedFields(expr)
	if len(fields) == 0 {
		// No direct bead.<field> reference (e.g. a watched-only clause). Fall
		// back to identifying fields so the alert still carries context.
		fields = []string{"id", "type", "status"}
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		v := getBeadField(f, bead)
		if v == "" {
			v = "<unset>"
		}
		parts = append(parts, f+"="+v)
	}
	return strings.Join(parts, ", ")
}

// assertReferencedFields extracts the distinct `bead.<field>` (and
// `bead["<field>"]`) field names referenced by a CEL clause, preserving
// first-seen order. Deliberately simple: it recognises the two canonical bead
// accessor forms the assert authoring surface documents; anything fancier in
// the clause just yields the identifying-fields fallback in observedValues.
func assertReferencedFields(expr string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(f string) {
		if f == "" || seen[f] {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	for i := 0; i < len(expr); {
		idx := strings.Index(expr[i:], "bead")
		if idx < 0 {
			break
		}
		j := i + idx + len("bead")
		// Ensure "bead" is a standalone identifier (not e.g. "beadx").
		prev := byte(' ')
		if i+idx > 0 {
			prev = expr[i+idx-1]
		}
		if isIdentByte(prev) {
			i = j
			continue
		}
		if j < len(expr) && expr[j] == '.' {
			k := j + 1
			start := k
			for k < len(expr) && isIdentByte(expr[k]) {
				k++
			}
			add(expr[start:k])
			i = k
			continue
		}
		if j < len(expr) && expr[j] == '[' {
			// bead["field"] or bead['field']
			k := j + 1
			if k < len(expr) && (expr[k] == '"' || expr[k] == '\'') {
				quote := expr[k]
				k++
				start := k
				for k < len(expr) && expr[k] != quote {
					k++
				}
				add(expr[start:k])
				i = k
				continue
			}
		}
		i = j
	}
	return out
}

// isIdentByte reports whether b can appear in a CEL identifier (letters,
// digits, underscore). Used to delimit field-name extraction in
// assertReferencedFields.
func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
