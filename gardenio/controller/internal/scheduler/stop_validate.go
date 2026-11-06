// Semantic validation of a gardener `stop` spec against ENGINE semantics.
//
// ParseStopSpec (stop.go) only proves the JSON is structurally decodable — it
// happily accepts a spec the engine will silently ignore: a typo'd kind
// ("max_run"), a wall_clock whose duration does not parse, a
// consecutive_failures whose value is not a positive int, a no_progress_for
// with runs<=0, a bead_query with an unknown op, an unknown on_loop_end action.
// Every one of those decodes fine yet is DEAD config: EvaluateGardenerStopEntry
// returns not-met (fail-open) for it, so the gardener never stops the way its
// author declared. The failure is SILENT — nothing warns the operator that the
// stop entry they just saved does nothing.
//
// ValidateStopSpec is the semantic gate that closes that gap: it mirrors, entry
// by entry, exactly what EvaluateGardenerStopEntry (gardener_stop.go) requires to
// evaluate a kind to a real true/false — so a spec that PASSES this validation
// is one every entry of which the engine can actually act on. It is the shared
// source of truth for the beads-viewer config-save path (task ka-0000002260,
// I2.5 under epic ka-0000001611 "Gardener Configuration Surface"): "validate
// submitted specs against engine semantics so the UI never persists a spec the
// engine chokes on."
//
// It deliberately does NOT re-impose the bounded-stop guard (run_budget + a
// bounded hard_stop wall_clock) — that lives in bridge.ValidateGardenerBounds
// and is a SEPARATE, policy-level check applied at authoring time. This
// validator answers a narrower question: "is every entry in this stop spec one
// the engine understands and can evaluate?"

package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// stopKindValid reports whether kind is a StopKind the engine's
// EvaluateGardenerStopEntry has a real case for. It is the closed set of the
// StopKind* constants — an unknown kind decodes fine but the evaluator logs
// "unknown kind, treating as not-met", so a stop entry declaring it is dead.
func stopKindValid(kind string) bool {
	switch kind {
	case StopKindMaxRuns,
		StopKindWallClock,
		StopKindBeadQuery,
		StopKindSpecificBeadClosed,
		StopKindGatePresent,
		StopKindGateAbsent,
		StopKindNoProgressFor,
		StopKindConsecutiveFailures,
		StopKindManual,
		StopKindManualForce,
		StopKindYieldedThenWokenThenIdle,
		StopKindNoActionableState,
		StopKindMRMerged,
		StopKindSpecificMRMerged,
		StopKindGoalSatisfied:
		return true
	default:
		return false
	}
}

// stopBeadQueryOps is the set of comparison operators
// conditions.CompareCount accepts for a bead_query stop entry. A bead_query
// with any other op evaluates to not-met (CompareCount's default arm), so the
// entry is silently inert. Mirror that op set here so the validator rejects
// exactly what the engine cannot act on.
var stopBeadQueryOps = map[string]bool{
	">": true, ">=": true, "<": true, "<=": true,
	"==": true, "eq": true, "!=": true, "ne": true,
}

// validOnLoopEndActions is the closed set of on_loop_end cleanup actions the
// stop spec documents (stop.go StopSpec.OnLoopEnd). Empty means "close" (the
// default), so "" is accepted. Any other value is a typo that would fall
// through to the default close path, not the action the author intended.
var validOnLoopEndActions = map[string]bool{
	"":               true,
	"close":          true,
	"notify_slack":   true,
	"spawn_followup": true,
}

// ValidateStopSpec checks a raw `stop` JSON blob against engine semantics and
// returns a non-nil error naming the FIRST offending entry (slot, index, kind,
// and why) when any entry is one the engine could not evaluate. An empty blob
// is valid (no constraints). A structurally-undecodable blob returns the parse
// error verbatim.
//
// The per-kind requirements mirror EvaluateGardenerStopEntry exactly:
//
//   - wall_clock: duration must time.ParseDuration to a positive value.
//   - consecutive_failures: value must parse to a positive int.
//   - no_progress_for: runs must be > 0 (duration-based no_progress_for is not
//     supported on the trigger gardener path — the evaluator treats runs<=0 as
//     not-met).
//   - bead_query: op must be a CompareCount-recognized operator, and value must
//     parse to an int (CompareCount parses value as the threshold).
//   - specific_bead_closed: bead_id must be non-empty.
//   - gate_present / gate_absent: value (the gate title) must be non-empty.
//   - max_runs / goal_satisfied / mr_merged / manual / manual_force /
//     no_actionable_state / yielded_then_woken_then_idle: no required fields
//     (manual/manual_force default their reason_field/label when empty).
//
// hard_stop, loop_end, and run_end are all validated (every slot the engine
// evaluates). on_loop_end must be a recognized action.
func ValidateStopSpec(raw string) error {
	spec, err := ParseStopSpec(raw)
	if err != nil {
		return err
	}
	if spec.IsZero() {
		return nil
	}
	if !validOnLoopEndActions[spec.OnLoopEnd] {
		return fmt.Errorf("stop: on_loop_end %q is not a recognized action (want one of: close, notify_slack, spawn_followup)", spec.OnLoopEnd)
	}
	for _, s := range []struct {
		name    string
		entries []StopEntry
	}{
		{StopSlotHardStop, spec.HardStop.Any},
		{StopSlotLoopEnd, spec.LoopEnd.Any},
		{StopSlotRunEnd, spec.RunEnd.Any},
	} {
		for i, e := range s.entries {
			if err := validateStopEntry(e); err != nil {
				return fmt.Errorf("stop: %s[%d]: %w", s.name, i, err)
			}
		}
	}
	return nil
}

// validateStopEntry validates a single stop entry against the engine's per-kind
// evaluation requirements. Returns nil when the engine could act on the entry.
func validateStopEntry(e StopEntry) error {
	kind := strings.TrimSpace(e.Kind)
	if kind == "" {
		return fmt.Errorf("missing kind")
	}
	if !stopKindValid(kind) {
		return fmt.Errorf("unknown kind %q (the engine would treat it as never-met)", kind)
	}
	switch kind {
	case StopKindWallClock:
		d, err := time.ParseDuration(strings.TrimSpace(e.Duration))
		if err != nil {
			return fmt.Errorf("wall_clock duration %q does not parse (%v)", e.Duration, err)
		}
		if d <= 0 {
			return fmt.Errorf("wall_clock duration %q must be positive", e.Duration)
		}
	case StopKindConsecutiveFailures:
		n, err := strconv.Atoi(strings.TrimSpace(e.Value))
		if err != nil {
			return fmt.Errorf("consecutive_failures value %q must be an integer", e.Value)
		}
		if n <= 0 {
			return fmt.Errorf("consecutive_failures value %q must be a positive integer", e.Value)
		}
	case StopKindNoProgressFor:
		if e.Runs <= 0 {
			return fmt.Errorf("no_progress_for requires runs>0 (duration-based no_progress_for is not supported on gardeners)")
		}
	case StopKindBeadQuery:
		op := strings.TrimSpace(e.Op)
		if !stopBeadQueryOps[op] {
			return fmt.Errorf("bead_query op %q is not a recognized comparison (want one of: >, >=, <, <=, ==, !=)", e.Op)
		}
		if _, err := strconv.Atoi(strings.TrimSpace(e.Value)); err != nil {
			return fmt.Errorf("bead_query value %q must be an integer threshold", e.Value)
		}
	case StopKindSpecificBeadClosed:
		if strings.TrimSpace(e.BeadID) == "" {
			return fmt.Errorf("specific_bead_closed requires a bead_id")
		}
	case StopKindSpecificMRMerged:
		if strings.TrimSpace(e.MRURL) == "" {
			return fmt.Errorf("specific_mr_merged requires an mr_url")
		}
	case StopKindGatePresent, StopKindGateAbsent:
		if strings.TrimSpace(e.Value) == "" {
			return fmt.Errorf("%s requires a value (the gate title)", kind)
		}
	}
	return nil
}
