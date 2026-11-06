// Stop-condition spec for schedule beads.
//
// Per gardenio/plans/epic-mention-looping-gardener.md §2.2 / §T4:
//
// A schedule bead may carry a `stop` JSON field with three slots:
//
//   - loop_end: any entry firing closes the schedule (no more runs).
//   - run_end:  any entry firing ends the current run (debits a run, loop continues).
//   - hard_stop: any entry firing kills the running pod and closes the schedule.
//
// Each slot is `{any: [<entry>, ...]}`. There is no `all:` for stop slots —
// "close only when ALL of these are true" is a wedge waiting to happen.
//
// Precedence: hard_stop > loop_end > run_end.
//
// This file defines the parse-side types and constants. Evaluation of the
// spec against a live gardener lives in gardener_stop.go (bridge calls
// EvaluateGardenerStopSpec). The legacy controller-side scheduler RunLoop was
// removed in Phase 3b/D10b; trigger-engine + bridge gardener hooks own all
// firing and stop evaluation now.

package scheduler

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Stop slot kinds (kind names used in StopEntry.Kind).
const (
	StopKindMaxRuns                  = "max_runs"
	StopKindWallClock                = "wall_clock"
	StopKindBeadQuery                = "bead_query"
	StopKindSpecificBeadClosed       = "specific_bead_closed"
	StopKindGatePresent              = "gate_present"
	StopKindGateAbsent               = "gate_absent"
	StopKindNoProgressFor            = "no_progress_for"
	StopKindConsecutiveFailures      = "consecutive_failures"
	StopKindManual                   = "manual"
	StopKindManualForce              = "manual_force"
	StopKindYieldedThenWokenThenIdle = "yielded_then_woken_then_idle"
	StopKindNoActionableState        = "no_actionable_state"
	// StopKindMRMerged is an author-declarable "block until MR merged" loop_end
	// marker (ka-0000007387). Unlike every other loop_end kind it is NOT lowered
	// by the generic stop evaluator into a terminate/resume decision: the
	// gardener fire path intercepts it in a dedicated pre-fire gate
	// (gardenerBlockOnMR) that PARKS the gardener dormant while any tracked MR
	// under the declared parent is unmerged, and wakes it when all are merged.
	// The generic evaluator therefore treats it as always not-met (see
	// EvaluateGardenerStopEntry) so it can never route through the ordinary
	// loop_end close/resume path. It exists as a StopKind (not merely a progress
	// event) so a gardener author can DECLARE "sleep until these MRs merge" as a
	// first-class intent — which mr_merged-as-a-progress-event never allowed
	// (that only ever reset the no-progress counter, never stopped or slept a
	// loop). The Parent field on the entry scopes which subtree's MRs are
	// tracked; empty Parent falls back to the gardener's frontier epic.
	StopKindMRMerged = "mr_merged"
	// StopKindSpecificMRMerged is the first-class "stop when THIS MR is merged"
	// loop_end marker (task ka-0000005598, the `--stop-on-mr-merged <mr_url>`
	// flag). Unlike mr_merged — a subtree PARK that blocks-until-merged and never
	// closes — this is a TERMINAL close scoped to one exact MR URL: the gardener's
	// goal was "get this one MR merged", so once that MR's mirror bead reports
	// merged the loop is genuinely DONE and closes as success. The target URL
	// rides the StopEntry.MRURL field; the evaluator resolves it to the canonical
	// type=merge-request mirror bead (MRBeadByURL) and matches mr_outcome/mr_state
	// == "merged", exactly as the progress evaluator's mr_merged kind does. It is
	// a terminal kind (see stopKindIsTerminal), so a match routes through the
	// ordinary loop_end close path (gardenerTerminate) rather than resume-or-
	// escalate — reaching the merge is completion, not premature exhaustion.
	StopKindSpecificMRMerged = "specific_mr_merged"
	// StopKindGoalSatisfied is the gardener-loop master gate: the loop
	// truly ends only when the gardener's freshest run self-declared its
	// terminal_goal met (`goal_satisfied:"yes"` in the wrap-up). It is the
	// canonical "gardener declares itself done" signal. See
	// stopKindIsTerminal — goal_satisfied is the prototypical *terminal*
	// kind, as opposed to the *premature* kinds (max_runs, wall_clock,
	// no_progress_for, consecutive_failures) that mean "out of
	// runs/time/patience", not "done".
	StopKindGoalSatisfied = "goal_satisfied"
)

// stopKindIsTerminal reports whether a stop kind means the gardener's goal is
// genuinely complete (or an operator explicitly willed termination), as
// opposed to a *premature* exhaustion signal (out of runs / time / patience /
// crashing). Terminal kinds close the gardener; premature kinds are converted
// by the gardener fire path into resume-or-escalate so an unmet terminal_goal
// is never silently abandoned (bead ka-0000005353). Unknown kinds are treated
// as terminal (fail-safe: an unrecognized stop closes rather than loops
// forever).
func stopKindIsTerminal(kind string) bool {
	switch kind {
	case StopKindMaxRuns,
		StopKindWallClock,
		StopKindNoProgressFor,
		StopKindConsecutiveFailures,
		// mr_merged is a park-not-close marker (ka-0000007387): if it ever
		// reaches the generic terminate/resume fork (it should not — the
		// dedicated gardenerBlockOnMR gate handles it first and the evaluator
		// returns not-met), classify it premature so it can never silently
		// CLOSE a gardener whose MRs merged. Premature routing re-arms rather
		// than terminates, which is strictly safer than an accidental close.
		StopKindMRMerged:
		return false
	default:
		return true
	}
}

// Stop slot names.
const (
	StopSlotLoopEnd  = "loop_end"
	StopSlotRunEnd   = "run_end"
	StopSlotHardStop = "hard_stop"
)

// StopEntry is one stop condition. The set of populated fields depends on
// Kind; unused fields are zero-valued.
type StopEntry struct {
	Kind string `json:"kind"`

	// Duration is the wall-clock budget for kind=wall_clock and the time window
	// for kind=no_progress_for when Runs is unset (Go duration string, e.g.
	// "90m", "6h" — the `no_progress_for: <minutes>` form).
	Duration string `json:"duration,omitempty"`
	// When scopes a wall_clock entry to a particular run state.
	// "yielded" means "only count time while the run is yielding"; empty means
	// "count from run start". Used by the legacy run_timeout lowering.
	When string `json:"when,omitempty"`

	// Value is the threshold for kind=consecutive_failures and the gate title
	// for kind=gate_present / gate_absent.
	Value string `json:"value,omitempty"`
	// Op is the comparison operator for kind=bead_query.
	Op string `json:"op,omitempty"`

	// BeadID is the target for kind=specific_bead_closed.
	BeadID string `json:"bead_id,omitempty"`

	// MRURL is the canonical merge-request URL targeted by
	// kind=specific_mr_merged (the `--stop-on-mr-merged <mr_url>` flag). The
	// evaluator resolves it to the type=merge-request mirror bead via MRBeadByURL
	// and fires when that bead reports merged.
	MRURL string `json:"mr_url,omitempty"`

	// Bead-query fields (kind=bead_query).
	Project  string   `json:"project,omitempty"`
	Types    []string `json:"types,omitempty"`
	Statuses []string `json:"statuses,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	// ExcludeBlockedByOpenTypes (kind=bead_query) subtracts from the count any
	// matched child that is blocked_by an OPEN bead of a type in this set (e.g.
	// ["escalation","decision"]). Lets a gardener loop_end distinguish
	// open+waiting-on-operator from open+stalled so it does not loop forever on
	// legitimately-blocked children (ka-0000002198). Empty = legacy count-only.
	ExcludeBlockedByOpenTypes []string `json:"exclude_blocked_by_open_types,omitempty"`

	// Runs is the rolling window for kind=no_progress_for (count of runs). When
	// set (>0), the LAST `runs` finalized runs must all be barren (score 0) to
	// fire. When unset, the evaluator falls back to the Duration field for a
	// time-windowed no_progress_for (ka-0000005141): every run that closed within
	// the last `duration` must be barren. Runs takes precedence when both are set.
	Runs int `json:"runs,omitempty"`

	// ReasonField names the schedule bead field whose presence triggers
	// kind=manual.
	ReasonField string `json:"reason_field,omitempty"`
	// Label names a schedule bead label whose presence triggers
	// kind=manual_force.
	Label string `json:"label,omitempty"`
}

// UnmarshalJSON tolerates a JSON number for the Value field by canonicalizing
// it to its string form. Schedule authors hand-roll the `stop` block and the
// `value` field carries different shapes across kinds (counts for
// consecutive_failures, gate titles for gate_present, bead-query thresholds,
// etc.); insisting on a quoted string for numeric counts is a foot-gun that
// silently drops the entry. Accept both forms — string and JSON number — and
// canonicalize on the way in so the rest of the evaluator can keep treating
// Value as a string.
func (e *StopEntry) UnmarshalJSON(data []byte) error {
	type alias StopEntry
	aux := struct {
		Value any `json:"value,omitempty"`
		*alias
	}{
		alias: (*alias)(e),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	switch v := aux.Value.(type) {
	case nil:
		e.Value = ""
	case string:
		e.Value = v
	case float64:
		e.Value = strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		e.Value = strconv.FormatBool(v)
	default:
		return fmt.Errorf("stop entry value: unsupported JSON type %T", v)
	}
	return nil
}

// StopSlot is one of the three slots (loop_end / run_end / hard_stop). Per
// §2.3 only `any:` composition is supported.
type StopSlot struct {
	Any []StopEntry `json:"any,omitempty"`
}

// StopSpec is the parsed `stop` JSON field on a schedule bead.
type StopSpec struct {
	LoopEnd  StopSlot `json:"loop_end"`
	RunEnd   StopSlot `json:"run_end"`
	HardStop StopSlot `json:"hard_stop"`

	// OnLoopEnd selects the cleanup action when loop_end fires:
	//   "close" (default) | "notify_slack" | "spawn_followup".
	OnLoopEnd string `json:"on_loop_end,omitempty"`
}

// IsZero reports whether the spec has no entries in any slot.
func (s StopSpec) IsZero() bool {
	return len(s.LoopEnd.Any) == 0 && len(s.RunEnd.Any) == 0 && len(s.HardStop.Any) == 0
}

// ParseStopSpec parses a JSON `stop` block. Empty input returns a zero spec
// with no error so callers can treat "no field" as "no constraints".
func ParseStopSpec(raw string) (StopSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return StopSpec{}, nil
	}
	var spec StopSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return StopSpec{}, err
	}
	return spec, nil
}

// StopMatch is the result of evaluating a stop slot — names the slot and the
// kind that fired, used for the structured "terminated: <slot>:<kind>"
// comment.
type StopMatch struct {
	Slot string
	Kind string
	// Premature is true when a loop_end entry fired on a *premature* kind
	// (out of runs/time/patience) rather than a terminal one
	// (goal_satisfied / operator-willed). The gardener fire path uses this
	// to decide whether to close the gardener (terminal) or convert the
	// stop into resume-or-escalate (premature) — see gardenerPreFireCheck.
	// hard_stop matches are never premature in this sense (they are an
	// emergency brake), so Premature is meaningful only for loop_end.
	Premature bool
}
