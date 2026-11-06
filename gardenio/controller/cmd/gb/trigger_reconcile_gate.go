// Package main — gate-evaluation half of `gb trigger reconcile`.
//
// scheduler.ReconcileFires deliberately computes the UNCONDITIONAL cron diff:
// it ignores conditions / cooldown / blocked_by / enabled so intent and
// mechanism stay separable (docs/wake-intent-reconciler.md, "Scope boundary
// (load-bearing)"). That purity is correct — but it means a condition-gated
// trigger reports a huge "missed" count that is really just its pre-flight gate
// declining to fire on an empty (post-exclusion) lane. The reconciler AGENT is
// then supposed to reason about whether the gate explains the misses before
// calling it drift — but the deterministic report gave it only the raw
// `conditions` JSON string, so it had to eyeball the excludes by hand and got it
// wrong: it filed "habitual drift" bugs (ka-0000000876 intake groomer,
// ka-0000005279 rescope) against two triggers that were firing perfectly — every
// "missed" fire was a legitimate gated no-fire (all lane beads carried an
// exclude_labels label, or were blocked_by an open decision).
//
// This file adds a LIVE gate probe to the report WITHOUT folding the gate into
// ReconcileFires. It evaluates the trigger's bead_query condition(s) against the
// current bead store exactly as the schedule source does at fire time, and — for
// each bead_query carrying an exclude subtraction — reports the raw
// (excludes-stripped) count beside the effective (post-subtraction) count. With
// `would_fire_now=false`, `effective_count=0`, and `raw_count>0`, the agent can
// deterministically classify "the gate legitimately explains the misses → IN
// SYNC" instead of manufacturing a phantom drift bug. The probe is a snapshot of
// NOW, not the historical window (fire history is unattributable to a
// point-in-time gate), so it is an explanatory signal, never a fire-count.
//
// Reuses the exact raw-vs-effective pattern proven in the dark-lane detector
// (internal/bridge/trigger_dark_lane.go classifyConditionSuppressed), and the
// same fail-soft contract: any query/parse error leaves the probe absent rather
// than manufacturing a signal.
package main

import (
	"context"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"

	"github.com/spf13/cobra"
)

// gateEval is the live pre-flight gate snapshot attached to a reconcileReport.
// It answers the ONE question ReconcileFires cannot: given the current bead
// store, would this trigger's conditions let it fire right now — and if not, is
// that because an exclude subtraction floored a genuinely non-empty lane to 0?
type gateEval struct {
	// WouldFireNow is the AND of every condition evaluated against the current
	// store — exactly scheduler.EvaluateConditions, the engine's fire-time gate.
	// false means a cron slot landing now would be a legitimate gated no-fire.
	WouldFireNow bool `json:"would_fire_now"`
	// Evaluated is false when the gate could not be evaluated (parse/daemon
	// error, OR an event-payload gate that has no store meaning — see
	// EventPayloadGate): WouldFireNow is then meaningless and the agent should
	// fall back to reading `conditions` by hand. Explicit so a false WouldFireNow
	// is never confused with an un-evaluated gate.
	Evaluated bool `json:"evaluated"`
	// EventPayloadGate is true when the trigger's conditions include a field leaf
	// (`{"field":...}`) — a match against an in-process event/bead payload, not a
	// store query. Such a gate CANNOT be evaluated as a point-in-time store
	// snapshot: there is no payload to match, so it is meaningless to ask "would
	// this fire NOW". This is set (with Evaluated=false) instead of returning a
	// bogus WouldFireNow, because the legacy scheduler.Condition shape silently
	// drops the `field` key and collapses every such condition to a no-op
	// "no constraint" leaf that evaluates TRUE — the phantom would_fire_now=true
	// that misclassified event-only trigger ka-0000002857 as impossible drift
	// (ka-0000001670). A drift audit for an event-driven trigger must judge its
	// wake_intent against its event-match config + observed fires, NOT this gate.
	EventPayloadGate bool `json:"event_payload_gate,omitempty"`
	// Error carries the fail-soft reason when Evaluated is false.
	Error string `json:"error,omitempty"`
	// Leaves is the per-bead_query raw-vs-effective breakdown. Only bead_query
	// conditions carrying an exclude_labels / exclude_blocked_by_open_types
	// subtraction appear (those are the only leaves whose raw and effective
	// counts can diverge and thus the only ones that explain a gated miss).
	Leaves []gateLeaf `json:"leaves,omitempty"`
}

// gateLeaf is the raw-vs-effective picture for one exclude-bearing bead_query
// condition: RawCount matching beads ignoring the excludes, EffectiveCount the
// count the engine actually gates on (after the exclude subtractions +
// closed-root short-circuit). RawCount>0 while EffectiveCount fails the op is
// the signature of a legitimately gated lane.
type gateLeaf struct {
	Op                        string   `json:"op"`
	Value                     string   `json:"value"`
	Labels                    []string `json:"labels,omitempty"`
	ExcludeLabels             []string `json:"exclude_labels,omitempty"`
	ExcludeBlockedByOpenTypes []string `json:"exclude_blocked_by_open_types,omitempty"`
	// RawCount is the match count with the exclude subtractions stripped (every
	// other filter — types/statuses/labels/parent — preserved).
	RawCount int `json:"raw_count"`
	// EffectiveCount is the post-subtraction count the pre-flight fires on.
	EffectiveCount int `json:"effective_count"`
	// EffectiveWouldFire is CompareCount(EffectiveCount, Op, Value) — whether
	// this leaf alone would pass its own threshold.
	EffectiveWouldFire bool `json:"effective_would_fire"`
	// SubtractionExplainsMiss is true when the excludes are the sole reason this
	// leaf does not fire: the effective count fails the op while the raw count
	// (subtractions ignored) would pass it. This is the deterministic "a gate
	// explains the misses" signal the reconciler agent keys on.
	SubtractionExplainsMiss bool `json:"subtraction_explains_miss"`
}

// gateClient is the daemon capability the gate probe needs. *beadsapi.Client
// satisfies it; the reconcile command's package-level `daemon` is one.
type gateClient interface {
	conditions.BeadClient
}

// buildGateEval evaluates a trigger's pre-flight conditions against the current
// bead store and returns the live gate snapshot, or nil when the trigger has no
// conditions field (nothing to gate on — an unconditional cron whose misses are
// genuine). Fail-soft: on any parse/daemon error it returns a gateEval with
// Evaluated=false and the reason, never nil-with-signal and never an error.
func buildGateEval(ctx context.Context, client gateClient, conditionsRaw, project string) *gateEval {
	raw := strings.TrimSpace(conditionsRaw)
	if raw == "" {
		return nil // unconditional trigger — no gate to explain a miss
	}

	// Event-payload gate guard (ka-0000001670). A field-leaf condition
	// (`{"field":...}`) matches an in-process event/bead payload, not a store
	// query — a store snapshot with no payload cannot evaluate it. The legacy
	// scheduler.Condition shape (parsed just below) has NO `field` member, so it
	// silently drops the field key and every such condition collapses to a no-op
	// "no constraint" leaf that evaluates TRUE — producing a phantom
	// would_fire_now=true. Detect that BEFORE the lossy parse (using the full
	// Expression parser that preserves field leaves) and report the gate as
	// un-evaluable instead of emitting a garbage verdict. This is the deterministic
	// "don't read an event-driven trigger's gate as a store gate" signal the
	// reconciler needs to stop misclassifying event-only triggers as impossible
	// drift.
	if exprs, perr := conditions.ParseExpressions(raw); perr == nil {
		for _, e := range exprs {
			if e.ReferencesEventPayload() {
				return &gateEval{
					Evaluated:        false,
					EventPayloadGate: true,
					Error: "conditions match an event/bead payload (field leaf), " +
						"not a store query — would_fire_now has no meaning for an " +
						"event-driven trigger; audit wake_intent against the event-match " +
						"config + observed fires instead",
				}
			}
		}
	}

	conds, err := conditions.ParseConditions(raw)
	if err != nil {
		return &gateEval{Evaluated: false, Error: "parse conditions: " + err.Error()}
	}
	if len(conds) == 0 {
		return nil
	}

	g := &gateEval{}

	// WouldFireNow: the exact engine gate — every condition AND-ed, evaluated
	// against the live store (scheduler.EvaluateConditions is what
	// trigger_source_schedule.go calls before firing).
	met, err := scheduler.EvaluateConditions(ctx, client, conds, project)
	if err != nil {
		return &gateEval{Evaluated: false, Error: "evaluate conditions: " + err.Error()}
	}
	g.Evaluated = true
	g.WouldFireNow = met

	// Per-leaf raw-vs-effective breakdown for exclude-bearing bead_query leaves.
	// Mirrors internal/bridge/trigger_dark_lane.go classifyConditionSuppressed.
	for _, c := range conds {
		if c.Type != "bead_query" {
			continue
		}
		if len(c.ExcludeLabels) == 0 && len(c.ExcludeBlockedByOpenTypes) == 0 {
			// No subtraction → raw == effective; this leaf can never explain a
			// gated miss, so it adds no signal. Skip it to keep the report tight.
			continue
		}

		leaf := c.ToExpression().Leaf
		effCount, err := conditions.EvaluateBeadQueryCount(ctx, client, leaf, project)
		if err != nil {
			// Fail-soft: a single leaf's probe failing degrades that leaf to
			// absent, not the whole gate. WouldFireNow above already succeeded.
			continue
		}

		rawLeaf := leaf
		rawLeaf.ExcludeLabels = nil
		rawLeaf.ExcludeBlockedByOpenTypes = nil
		rawCount, err := conditions.EvaluateBeadQueryCount(ctx, client, rawLeaf, project)
		if err != nil {
			continue
		}

		effFire := conditions.CompareCount(effCount, c.Op, c.Value)
		rawFire := conditions.CompareCount(rawCount, c.Op, c.Value)
		g.Leaves = append(g.Leaves, gateLeaf{
			Op:                        c.Op,
			Value:                     c.Value,
			Labels:                    c.Labels,
			ExcludeLabels:             c.ExcludeLabels,
			ExcludeBlockedByOpenTypes: c.ExcludeBlockedByOpenTypes,
			RawCount:                  rawCount,
			EffectiveCount:            effCount,
			EffectiveWouldFire:        effFire,
			// The excludes are the SOLE reason this leaf is quiet: effective
			// fails its op while raw would pass it.
			SubtractionExplainsMiss: !effFire && rawFire,
		})
	}

	return g
}

// printGate renders the live gate snapshot for a human spot-check. No-op when
// the trigger has no conditions (nil gate). The JSON gate block is the machine
// surface the reconciler agent reads; this is for operators eyeballing a report.
func printGate(cmd *cobra.Command, g *gateEval) {
	if g == nil {
		return
	}
	if !g.Evaluated {
		if g.EventPayloadGate {
			cmd.Printf("  gate: (event-payload gate — not store-evaluable; %s)\n", g.Error)
			return
		}
		cmd.Printf("  gate: (could not evaluate — %s)\n", g.Error)
		return
	}
	cmd.Printf("  gate: would_fire_now=%v", g.WouldFireNow)
	if !g.WouldFireNow {
		cmd.Printf("  → a cron slot landing now is a legitimate gated no-fire, not a miss")
	}
	cmd.Println()
	for _, l := range g.Leaves {
		note := ""
		if l.SubtractionExplainsMiss {
			note = fmt.Sprintf("  ← excludes floor a non-empty lane to 0 (raw %d would fire, effective %d does not) — misses are gated, not drift",
				l.RawCount, l.EffectiveCount)
		}
		cmd.Printf("    bead_query %s %s: raw=%d effective=%d%s%s\n",
			l.Op, l.Value, l.RawCount, l.EffectiveCount,
			gateSubtractionSummary(l), note)
	}
}

// gateSubtractionSummary names the exclude filters on a leaf for the human line.
func gateSubtractionSummary(l gateLeaf) string {
	var parts []string
	if len(l.ExcludeLabels) > 0 {
		parts = append(parts, "exclude_labels="+strings.Join(l.ExcludeLabels, ","))
	}
	if len(l.ExcludeBlockedByOpenTypes) > 0 {
		parts = append(parts, "exclude_blocked_by_open_types="+strings.Join(l.ExcludeBlockedByOpenTypes, ","))
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, " ") + "]"
}

// compile-time proof the production daemon client satisfies the probe interface.
var _ gateClient = (*beadsapi.Client)(nil)
