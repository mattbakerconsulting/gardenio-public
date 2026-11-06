package progress

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// State is the rolling progress state stored as JSON in the schedule
// bead's `progress_state` field. The scheduler appends to it on every
// reconcile pass: a RunRecord whenever an agent finishes, and an
// EventRecord for every newly-observed progress event.
//
// State is intentionally an opaque blob from the operator's
// perspective — its shape may evolve. ParseState returns a zero State
// for empty input so a fresh schedule starts clean.
type State struct {
	// Runs is appended to whenever trackLastAgentStatus observes the
	// most-recent agent close. Newest-last (i.e. Runs[len-1] is the
	// freshest record). Bounded by MaxRuns at append time.
	Runs []RunRecord `json:"runs,omitempty"`

	// Events is the dedup log: every progress event the evaluator
	// has previously credited, keyed by (Kind, TargetID). Used to
	// avoid double-counting a child_closed across reconcile ticks.
	// Newest-last; bounded by MaxEvents at append time.
	Events []EventRecord `json:"events,omitempty"`

	// CurrentRunStart marks the spawn time of the agent currently
	// running (or the most-recent one). Used as the lower bound for
	// the `since_run_start` window and surfaced in the prime block.
	CurrentRunStart time.Time `json:"current_run_start,omitzero"`
}

// RunRecord is one closed-run summary. Stored newest-last in
// State.Runs. The fields that matter for stop-condition evaluation
// are AgentID (for fingerprinting) and ProgressScore (drives
// no_progress_for).
type RunRecord struct {
	AgentID         string    `json:"agent_id,omitempty"`
	StartedAt       time.Time `json:"started_at,omitzero"`
	ClosedAt        time.Time `json:"closed_at,omitzero"`
	ProgressScore   int       `json:"progress_score"`
	ProgressSummary string    `json:"progress_summary,omitempty"`
	AgentState      string    `json:"agent_state,omitempty"` // "done" | "failed" | ...
	// GoalSatisfied mirrors the run's self-declared `goal_satisfied`
	// wrap-up field ("yes" | "no" | ""). It is the gardener-loop-level
	// signal for "I have reached my terminal_goal": the goal_satisfied
	// stop kind fires only when the freshest run recorded "yes". Distinct
	// from ProgressScore (which measures "did this run move forward",
	// not "is the whole goal done").
	GoalSatisfied string `json:"goal_satisfied,omitempty"`
}

// EventRecord is one credited progress event. Kind is one of
// EventKind*; TargetID is the bead/MR/decision identifier; OccurredAt
// is the daemon-side timestamp (or now() when the daemon does not
// surface a precise one).
type EventRecord struct {
	Kind       string    `json:"kind"`
	TargetID   string    `json:"target_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Bounded retention. Events further back than MaxEvents are dropped;
// runs further back than MaxRuns are dropped. Picked large enough
// that the longest realistic window (last_24h with a busy schedule
// firing every 5 min ⇒ ~288 runs) fits comfortably while staying
// well under the bead-field size budget.
const (
	MaxRuns   = 64
	MaxEvents = 512
)

// ParseState parses an opaque `progress_state` JSON field. Empty
// input returns a zero state with no error.
func ParseState(raw string) (State, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return State{}, nil
	}
	var st State
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return State{}, err
	}
	return st, nil
}

// Marshal serializes the state for storage on the schedule bead.
func (s State) Marshal() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// AppendRun adds one closed-run record, dropping the oldest entries
// once the slice exceeds MaxRuns.
func (s *State) AppendRun(rec RunRecord) {
	s.Runs = append(s.Runs, rec)
	if len(s.Runs) > MaxRuns {
		s.Runs = s.Runs[len(s.Runs)-MaxRuns:]
	}
}

// AppendEvent adds one credited event, dropping the oldest entries
// once the slice exceeds MaxEvents. Returns true when the event was
// new (not already present in the dedup log under (Kind, TargetID)).
func (s *State) AppendEvent(rec EventRecord) bool {
	for _, e := range s.Events {
		if e.Kind == rec.Kind && e.TargetID == rec.TargetID && rec.TargetID != "" {
			return false
		}
	}
	s.Events = append(s.Events, rec)
	if len(s.Events) > MaxEvents {
		s.Events = s.Events[len(s.Events)-MaxEvents:]
	}
	return true
}

// HasEvent reports whether an event of kind for targetID is already in the
// dedup log, i.e. whether AppendEvent would drop it.
func (s State) HasEvent(kind, targetID string) bool {
	for _, e := range s.Events {
		if e.Kind == kind && e.TargetID == targetID && targetID != "" {
			return true
		}
	}
	return false
}

// ZeroScoreRunsInLastN counts how many of the last n RunRecords have
// ProgressScore == 0. Returns (count, observed) where observed is
// min(n, len(s.Runs)) — callers compare count to n directly to fire
// `no_progress_for` only when there are enough runs to judge.
func (s State) ZeroScoreRunsInLastN(n int) (int, int) {
	if n <= 0 || len(s.Runs) == 0 {
		return 0, 0
	}
	tail := s.Runs
	if len(tail) > n {
		tail = tail[len(tail)-n:]
	}
	zero := 0
	for _, r := range tail {
		if r.ProgressScore == 0 {
			zero++
		}
	}
	return zero, len(tail)
}

// ZeroScoreRunsSince counts how many runs closed at or after cutoff have
// ProgressScore == 0. Returns (zero, observed) where observed is the number of
// runs whose ClosedAt is >= cutoff — callers fire the DURATION-based
// `no_progress_for` only when observed >= 1 && zero == observed (every run in
// the window was barren). This is the time-windowed counterpart to
// ZeroScoreRunsInLastN: instead of "the last N runs", it scans "every run that
// closed within the last <duration>", so a gardener firing on a slow cadence
// still trips after a sustained wall-clock stretch of no forward progress even
// when the raw run COUNT is small.
//
// A run with a zero ClosedAt (never finalized a close time) is skipped — it
// cannot be placed in the window, and the DurationUnsupported→Supported change
// must never let an un-timestamped run manufacture or suppress a wedge verdict.
// Because the score read here is the post-FinalizeRun score, a run rescued by an
// in-window credited event (mr_merged / child_closed / decision_resolved) already
// carries a non-zero score and is correctly counted as progress.
func (s State) ZeroScoreRunsSince(cutoff time.Time) (zero, observed int) {
	for _, r := range s.Runs {
		if r.ClosedAt.IsZero() || r.ClosedAt.Before(cutoff) {
			continue
		}
		observed++
		if r.ProgressScore == 0 {
			zero++
		}
	}
	return zero, observed
}

// ProgressSummaryGaps counts recorded runs where progress_score is set
// (ProgressScore != 0) but progress_summary is empty — the co-require the
// wrap-up enforcer (wrapup_progress.go) pairs, which holds only under
// in_wrapup+hard enforcement. --force stops and soft schedules leak past it,
// so a run can land a non-zero score with no one-line justification
// (audit finding F2, ka-0000005466). Returns (gaps, scored) where scored is
// the number of runs with a non-zero score — callers phrase the conformance
// note as "gaps/scored". A zero score is excluded: progress_summary is only
// required to justify a score that claims forward motion, so a clean score-0
// run with no summary is conformant, not a gap.
func (s State) ProgressSummaryGaps() (gaps, scored int) {
	for _, r := range s.Runs {
		if r.ProgressScore == 0 {
			continue
		}
		scored++
		if strings.TrimSpace(r.ProgressSummary) == "" {
			gaps++
		}
	}
	return gaps, scored
}

// CreditedProgressKinds is the set of event kinds that count as real
// forward progress when reconciling a self-reported score-0 run against
// discovered events (bug ka-0000006914). A run that merged an MR, closed
// a child bead, or resolved a decision made progress even when its
// wrap-up omitted/zeroed progress_score — so no_progress_for must not
// treat such a run as barren.
//
// crew_dispatched and wrapup_field are deliberately excluded: dispatching
// crew is not yet an outcome, and wrapup_field is just the self-report by
// another name. cancel_duplicates decision events never enter state.Events
// at all (the evaluator filters them — ka-0000006682), so the anti-gaming
// guard is preserved here by construction.
var CreditedProgressKinds = []string{
	EventKindMRMerged,
	EventKindChildClosed,
	EventKindDecisionResolved,
}

// HasCreditedEventInWindow reports whether any credited progress event
// (CreditedProgressKinds) has OccurredAt within the inclusive window
// [startedAt, closedAt]. A zero closedAt is treated as "open-ended"
// (matches any event at or after startedAt); a zero startedAt drops the
// lower bound. Used by FinalizeRun to rescue a score-0 run whose run
// window contains a real outcome event.
func (s State) HasCreditedEventInWindow(startedAt, closedAt time.Time) bool {
	for _, e := range s.Events {
		if !slices.Contains(CreditedProgressKinds, e.Kind) {
			continue
		}
		if !startedAt.IsZero() && e.OccurredAt.Before(startedAt) {
			continue
		}
		if !closedAt.IsZero() && e.OccurredAt.After(closedAt) {
			continue
		}
		return true
	}
	return false
}

// hasCreditedEventInInterval reports whether any credited progress event
// (CreditedProgressKinds) has OccurredAt within the HALF-OPEN attribution
// interval [lower, upperExclusive). A zero upperExclusive is treated as
// open-ended (matches any event at or after lower). This differs from
// HasCreditedEventInWindow in that the upper bound is EXCLUSIVE — the interval
// belongs to the run whose StartedAt is `lower`, and the next run owns its own
// StartedAt instant, so an event exactly at the next run's start must credit the
// next run, not this one. Used by ReconcileAsyncProgress.
func (s State) hasCreditedEventInInterval(lower, upperExclusive time.Time) bool {
	for _, e := range s.Events {
		if !slices.Contains(CreditedProgressKinds, e.Kind) {
			continue
		}
		if !lower.IsZero() && e.OccurredAt.Before(lower) {
			continue
		}
		if !upperExclusive.IsZero() && !e.OccurredAt.Before(upperExclusive) {
			continue // at/after the next run's start — belongs to that run
		}
		return true
	}
	return false
}

// ReconcileAsyncProgress re-scores already-recorded score-0 runs when a credited
// progress event (CreditedProgressKinds) has since been discovered that falls in
// the run's ATTRIBUTION interval [run.StartedAt, nextRun.StartedAt) — the span
// during which that run was the live one. Lifts each matching score-0 run to 1
// and returns the number of runs lifted.
//
// Why this exists (async-attribution gap, gardener-effectiveness-rubric §D1): a
// healthy DELEGATING gardener dispatches crew and closes score-0 at its own
// wrap-up; the dispatched crew then close the epic's children ASYNCHRONOUSLY,
// often in the gap AFTER the dispatching run's closed_at but BEFORE the next run
// starts. That child_closed / mr_merged / decision_resolved event therefore
// lands in NEITHER run's [startedAt, closedAt] rescue window, so both runs record
// a false score-0 and D1 (progress yield) collapses to a C/D grade even while D2
// (throughput) reads 5 — the exact First Mate symptom (ka-0000000498: D2=5, D1=1).
// FinalizeRun's rescue cannot see the event at finalize time (it has not happened
// yet) and never revisits a recorded run; this pass does, on the later reconcile
// cycle once the event has been discovered into state.Events.
//
// The rubric's rule — "an event credits the run that most recently started
// before it" — is exactly half-open interval attribution: run i owns
// [Runs[i].StartedAt, Runs[i+1].StartedAt). The freshest (last) run owns an
// open-ended interval [StartedAt, +inf), matching FinalizeRun's zero-closedAt
// semantics. Because each event falls in exactly one interval, no event credits
// two runs.
//
// Idempotent and monotone: it only lifts 0 -> 1, never lowers a self-reported or
// previously-rescued score, so re-running it every cycle credits no run twice.
// cancel_duplicates decision events never enter state.Events (the evaluator
// filters them — ka-0000006682), so the no_progress_for anti-gaming guard is
// preserved here by construction. Runs with a zero StartedAt are skipped (no
// attributable interval).
func (s *State) ReconcileAsyncProgress() int {
	lifted := 0
	for i := range s.Runs {
		r := &s.Runs[i]
		if r.ProgressScore != 0 || r.StartedAt.IsZero() {
			continue
		}
		// Upper bound: the StartedAt of the next run that actually started
		// (exclusive). Skip any intermediate record with a zero StartedAt so a
		// single malformed run cannot make this interval falsely open-ended and
		// over-credit run i. The freshest started run has no successor and so
		// owns an open-ended interval (zero upper).
		var upper time.Time
		for j := i + 1; j < len(s.Runs); j++ {
			if !s.Runs[j].StartedAt.IsZero() {
				upper = s.Runs[j].StartedAt
				break
			}
		}
		if s.hasCreditedEventInInterval(r.StartedAt, upper) {
			r.ProgressScore = 1
			lifted++
		}
	}
	return lifted
}

// ProgressYield computes the rubric's D1 progress-yield numerator/denominator
// over the last n RunRecords (n<=0 counts every recorded run). Returns
// (progressRuns, scoredRuns, selfReported, eventCredited):
//
//   - scoredRuns   — the number of runs in the window (the D1 denominator).
//   - progressRuns — runs that made forward progress (the D1 numerator). A run
//     counts when its ProgressScore >= 1 (self-reported OR async-rescued by
//     ReconcileAsyncProgress) OR a credited progress event
//     (CreditedProgressKinds) falls in its half-open attribution interval
//     [StartedAt, nextStartedAt) — the exact "OR credited" union the rubric §D1
//     formula (`yield = progress_runs / scored_runs`) specifies.
//   - selfReported — subset credited by ProgressScore >= 1.
//   - eventCredited — subset credited ONLY by a window event (score was 0).
//
// The two sub-counts sum to progressRuns and exist so a caller can explain WHY
// D1 is non-zero (self-attested throughput vs. async child closures) without
// re-deriving it. This is the load-bearing measurement fix for the D1-reads-0
// artifact (task ka-0000004982): the per-run progress_score lives HERE in
// progress_state.runs, not in the gardener's run_log (whose entries carry no
// score), and the event fallback covers the delegating-gardener async gap. A
// caller reading only run_log or only state.Events sees a false 0; this method
// reads both the way the rubric demands.
func (s State) ProgressYield(n int) (progressRuns, scoredRuns, selfReported, eventCredited int) {
	runs := s.Runs
	if n > 0 && len(runs) > n {
		runs = runs[len(runs)-n:]
	}
	for i := range runs {
		r := runs[i]
		scoredRuns++
		if r.ProgressScore >= 1 {
			progressRuns++
			selfReported++
			continue
		}
		// score-0 run: credit it if a real outcome event landed in its
		// attribution interval [StartedAt, nextStartedAt) — the async
		// delegating-gardener case ReconcileAsyncProgress also rescues. Skip
		// runs with no StartedAt (no attributable interval).
		if r.StartedAt.IsZero() {
			continue
		}
		var upper time.Time
		for j := i + 1; j < len(runs); j++ {
			if !runs[j].StartedAt.IsZero() {
				upper = runs[j].StartedAt
				break
			}
		}
		if s.hasCreditedEventInInterval(r.StartedAt, upper) {
			progressRuns++
			eventCredited++
		}
	}
	return progressRuns, scoredRuns, selfReported, eventCredited
}

// EventsSince returns the count of events with kind in `kinds` whose
// OccurredAt is >= since. An empty kinds slice matches all kinds.
func (s State) EventsSince(since time.Time, kinds ...string) int {
	wantAll := len(kinds) == 0
	wantSet := map[string]struct{}{}
	for _, k := range kinds {
		wantSet[k] = struct{}{}
	}
	n := 0
	for _, e := range s.Events {
		if e.OccurredAt.Before(since) {
			continue
		}
		if wantAll {
			n++
			continue
		}
		if _, ok := wantSet[e.Kind]; ok {
			n++
		}
	}
	return n
}
