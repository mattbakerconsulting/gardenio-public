package progress

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func jsonUnmarshalBytes(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// BeadClient is the daemon surface the evaluator depends on. Kept narrow
// so unit tests can satisfy it with an in-memory mock; the real
// `*beadsapi.Client` already implements every method.
type BeadClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	// MRBeadForTask walks task → merge-request via the `produces` dep edge.
	// Used by discoverMRMerged to prefer the MR-bead source-of-truth over the
	// legacy task-bead mr_* fields. See epic ka-0000001613.
	MRBeadForTask(ctx context.Context, taskBeadID string) (*beadsapi.BeadDetail, error)
}

// EvalContext carries the per-schedule values an evaluator pass needs.
// AssignBead and ScheduleID are the template-resolution inputs;
// existing State is the dedup log so duplicate events are not credited
// twice; Now is the wall-clock used for OccurredAt fallback.
type EvalContext struct {
	AssignBead string
	ScheduleID string
	State      State
	Now        time.Time
}

// Evaluator runs daemon-side queries against the configured event
// matchers and folds new event records into the supplied state.
type Evaluator struct {
	Client BeadClient
}

// Discover walks the spec's events list and queries the daemon for
// each kind. Newly-observed events are appended to ec.State (dedup'd
// via State.AppendEvent). Returns the updated state. Daemon errors
// for one matcher are logged via the supplied logf and do not abort
// the rest of the pass — a flaky kind shouldn't wedge the loop.
func (e *Evaluator) Discover(ctx context.Context, spec Spec, ec EvalContext, logf func(format string, args ...any)) (State, error) {
	if e == nil || e.Client == nil {
		return ec.State, fmt.Errorf("progress: evaluator missing bead client")
	}
	resolved := spec.ResolveTemplates(ec.AssignBead, ec.ScheduleID)
	state := ec.State
	for _, m := range resolved.Events {
		if err := e.discoverOne(ctx, m, &state, ec.Now); err != nil {
			if logf != nil {
				logf("progress.evaluator: kind=%s err=%v", m.Kind, err)
			}
		}
	}
	return state, nil
}

// discoverOne runs the single-matcher query and appends new event
// records to state.
func (e *Evaluator) discoverOne(ctx context.Context, m EventMatcher, state *State, now time.Time) error {
	switch m.Kind {
	case EventKindChildClosed:
		return e.discoverChildClosed(ctx, m, state, now)
	case EventKindDecisionResolved:
		return e.discoverDecisionResolved(ctx, m, state, now)
	case EventKindMRMerged:
		return e.discoverMRMerged(ctx, m, state, now)
	case EventKindCrewDispatched:
		return e.discoverCrewDispatched(ctx, m, state, now)
	case EventKindWrapupField:
		// wrapup_field is finalised against RunRecord at run-end time
		// (see Scheduler.trackLastAgentStatus); the evaluator does not
		// poll it here. We still report ok so unknown-kind warnings
		// don't fire on legitimate specs.
		return nil
	default:
		return fmt.Errorf("unknown event kind %q", m.Kind)
	}
}

// discoverChildClosed queries terminal-for-progression beads under the
// configured parent and credits each one not already in the dedup log.
// Both 'closed' (clean completion) and 'partial' (completed with some
// descendant failures) are terminal — a 'partial' child satisfies a
// waits_for: all-children join just like a closed one (ka-0000007305 §2.3).
func (e *Evaluator) discoverChildClosed(ctx context.Context, m EventMatcher, state *State, now time.Time) error {
	if m.Parent == "" {
		return fmt.Errorf("child_closed requires parent")
	}
	// Only the id and closed_at are read: NoFields drops the bodies and
	// fields (an epic's escalation children carry large context/result
	// fields), about 3.4x less per call (gd-6PXLuR3juD).
	res, err := e.Client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:   m.Parent,
		Statuses: []string{"closed", "partial"},
		Limit:    256,
	}.NoFields())
	if err != nil {
		return err
	}
	if res == nil {
		return nil
	}
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		state.AppendEvent(EventRecord{
			Kind:       EventKindChildClosed,
			TargetID:   b.ID,
			OccurredAt: closedOrNow(b, now),
		})
	}
	return nil
}

// CancelReasonDuplicates is the cancel_reason value set by the operator
// when dismissing a decision the gardener filed redundantly with prior
// runs. Excluded from decision_resolved progress so a gardener cannot
// game its own no_progress_for stop counter by repeatedly raising the
// same decision (ka-0000006682).
const CancelReasonDuplicates = "cancel_duplicates"

// discoverDecisionResolved counts closed decisions. When
// RaisedByAgentSubtreeOf is set, we restrict to decisions whose
// "raised_by_agent" field names an agent within that subtree (best
// effort — empty subtree filter accepts all closed decisions).
//
// Decisions cancelled with cancel_reason=cancel_duplicates are
// excluded — they signal "nothing new" from the operator, not forward
// progress (ka-0000006682).
func (e *Evaluator) discoverDecisionResolved(ctx context.Context, m EventMatcher, state *State, now time.Time) error {
	res, err := e.Client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"closed"},
		Limit:    256,
	}.OnlyFields("cancel_reason", "raised_by_agent"))
	if err != nil {
		return err
	}
	if res == nil {
		return nil
	}
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		if b.Fields["cancel_reason"] == CancelReasonDuplicates {
			continue
		}
		if m.RaisedByAgentSubtreeOf != "" {
			raisedBy := b.Fields["raised_by_agent"]
			if raisedBy == "" {
				continue
			}
			// Best-effort subtree check: skip when the daemon hasn't
			// stamped raised_by_agent. Strict subtree validation would
			// need an IsAncestor call per decision; we keep the
			// evaluator cheap and let the operator narrow via
			// raised_by_agent labelling if accuracy matters.
		}
		state.AppendEvent(EventRecord{
			Kind:       EventKindDecisionResolved,
			TargetID:   b.ID,
			OccurredAt: closedOrNow(b, now),
		})
	}
	return nil
}

// discoverMRMerged counts beads whose linked MR bead is merged, scoped to
// the children of Parent and/or beads carrying AuthorLabel.
//
// Parent used to be ignored (gd-6PXLuR3juD): every plan gardener's
// {"kind":"mr_merged","parent":"{{assign_bead}}"} listed the newest 256
// beads of the whole daemon, full rows, and credited any merged MR among
// them to the gardener. With 10 gardeners on a 30 s cycle that one list was
// most of the trigger-engine's GET /v1/beads bytes, and each pass walked
// MRBeadForTask for all 256 beads.
func (e *Evaluator) discoverMRMerged(ctx context.Context, m EventMatcher, state *State, now time.Time) error {
	var labels []string
	if m.AuthorLabel != "" {
		labels = []string{m.AuthorLabel}
	}
	res, err := e.Client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent: m.Parent,
		Labels: labels,
		Limit:  256,
	}.NoFields())
	if err != nil {
		return err
	}
	if res == nil {
		return nil
	}
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		// Already credited: skip the MR walk, AppendEvent would drop it.
		if state.HasEvent(EventKindMRMerged, b.ID) {
			continue
		}
		if !e.taskHasMergedMR(ctx, b) {
			continue
		}
		state.AppendEvent(EventRecord{
			Kind:       EventKindMRMerged,
			TargetID:   b.ID,
			OccurredAt: closedOrNow(b, now),
		})
	}
	return nil
}

// taskHasMergedMR reports whether the task bead has a merged MR. Reads the
// MR-bead source of truth (walks `produces` dep, checks
// mr_outcome=="merged"). The legacy task-bead `merged` / `mr_state` fields
// are no longer written (epic ka-0000001613 / task ka-0000008134), so the
// MR bead is the only canonical source. Returns false when no MR bead is
// linked yet or the lookup errors.
func (e *Evaluator) taskHasMergedMR(ctx context.Context, b *beadsapi.BeadDetail) bool {
	mrBead, err := e.Client.MRBeadForTask(ctx, b.ID)
	if err != nil || mrBead == nil {
		return false
	}
	return mrBead.Fields["mr_outcome"] == "merged" || mrBead.Fields["mr_state"] == "merged"
}

// discoverCrewDispatched counts agent beads with role: matching the
// matcher's Role under the configured Parent.
func (e *Evaluator) discoverCrewDispatched(ctx context.Context, m EventMatcher, state *State, now time.Time) error {
	q := beadsapi.ListBeadsQuery{
		Types: []string{"agent"},
		Limit: 256,
	}
	if m.Parent != "" {
		q.Parent = m.Parent
	}
	if m.Role != "" {
		q.Labels = append(q.Labels, "role:"+m.Role)
	}
	res, err := e.Client.ListBeadsFiltered(ctx, q.NoFields())
	if err != nil {
		return err
	}
	if res == nil {
		return nil
	}
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		state.AppendEvent(EventRecord{
			Kind:       EventKindCrewDispatched,
			TargetID:   b.ID,
			OccurredAt: created(b, now),
		})
	}
	return nil
}

// CountByWindow returns a (window-name → kind → count) map for the
// supplied state and spec. Kinds in the result include every kind
// listed in the spec; counts are 0 when no matching events fall in
// the window.
//
// Window semantics:
//   - since_run_start  — events with OccurredAt >= state.CurrentRunStart
//   - last_n_runs      — events with OccurredAt >= the n-th-from-last
//     RunRecord's StartedAt; falls back to all
//     events when fewer than n runs are recorded.
//   - since            — events with OccurredAt >= now - duration.
func CountByWindow(spec Spec, state State, now time.Time) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, w := range spec.Windows {
		since := windowSince(w, state, now)
		row := map[string]int{}
		for _, m := range spec.Events {
			row[m.Kind] = state.EventsSince(since, m.Kind)
		}
		out[w.Name] = row
	}
	return out
}

// windowSince returns the time threshold for one window's "since"
// boundary. Returns zero time when the window resolves to "all events".
func windowSince(w Window, state State, now time.Time) time.Time {
	switch w.Scope {
	case WindowScopeSinceRunStart:
		return state.CurrentRunStart
	case WindowScopeSince:
		d, err := w.ParseDuration()
		if err != nil || d <= 0 {
			return time.Time{}
		}
		return now.Add(-d)
	case WindowScopeLastNRuns:
		if w.N <= 0 || len(state.Runs) == 0 {
			return time.Time{}
		}
		idx := len(state.Runs) - w.N
		if idx < 0 {
			return time.Time{}
		}
		// Use StartedAt of the first run in the window.
		if !state.Runs[idx].StartedAt.IsZero() {
			return state.Runs[idx].StartedAt
		}
		return state.Runs[idx].ClosedAt
	default:
		return time.Time{}
	}
}

// FinalizeRun reads the supplied wrap-up JSON and appends a closed-run
// record to state. Empty wrap-up still records a run (with score=0)
// so no_progress_for can fire on missing wrap-ups. The agentState
// argument is the agent's `agent_state` field at close time.
//
// Event-aware no-progress reconciliation (bug ka-0000006914): progress_score
// is purely self-reported, so a run that merged an MR / closed a child /
// resolved a decision but whose wrap-up omitted or zeroed progress_score
// would record a score-0 RunRecord and falsely count toward no_progress_for.
// To reconcile the two signals, when the self-reported score is 0 but a
// credited progress event (CreditedProgressKinds) occurred inside this run's
// [startedAt, closedAt] window, the recorded ProgressScore is lifted to 1.
// This keeps ZeroScoreRunsInLastN (and every downstream consumer) correct
// without re-plumbing them. cancel_duplicates decision events never reach
// state.Events (evaluator filters them — ka-0000006682), so the anti-gaming
// guard is preserved.
//
// CALLER ORDERING CONTRACT: state.Events for the just-closed run must already
// be discovered before FinalizeRun is called, or the rescue cannot see them.
// The bridge fire path reorders gardenerDiscoverProgress ahead of
// gardenerAgentTracker in evalOne for exactly this reason (triggers.go).
func FinalizeRun(state *State, agentID, wrapupJSON, agentState string, startedAt, closedAt time.Time) {
	score, summary := ExtractWrapupProgress(wrapupJSON)
	if score == 0 && state.HasCreditedEventInWindow(startedAt, closedAt) {
		score = 1
	}
	state.AppendRun(RunRecord{
		AgentID:         agentID,
		StartedAt:       startedAt,
		ClosedAt:        closedAt,
		ProgressScore:   score,
		ProgressSummary: summary,
		AgentState:      agentState,
		GoalSatisfied:   ExtractWrapupGoalSatisfied(wrapupJSON),
	})
}

// ExtractWrapupGoalSatisfied decodes the `goal_satisfied` field
// ("yes" | "no" | "") out of a gardener's wrap-up payload. This is the
// run's self-declaration that its terminal_goal is (not) met, and is
// the load-bearing signal for the goal_satisfied stop kind. Missing or
// invalid input returns "" (treated as "not declared done"). The
// returned value is trimmed and lowercased so callers can compare
// against "yes"/"no" directly.
func ExtractWrapupGoalSatisfied(raw string) string {
	if raw == "" {
		return ""
	}
	var blob map[string]any
	if err := jsonUnmarshalBytes([]byte(raw), &blob); err != nil {
		return ""
	}
	if v, ok := blob["goal_satisfied"].(string); ok {
		return strings.ToLower(strings.TrimSpace(v))
	}
	return ""
}

// ExtractWrapupProgress decodes `progress_score` (int) and
// `progress_summary` (string) out of a gardener's wrap-up payload.
// Missing or invalid fields return the zero value (0 / "") so
// trackLastAgentStatus can still record the run.
func ExtractWrapupProgress(raw string) (int, string) {
	if raw == "" {
		return 0, ""
	}
	var blob map[string]any
	if err := jsonUnmarshalBytes([]byte(raw), &blob); err != nil {
		return 0, ""
	}
	score := 0
	summary := ""
	if v, ok := blob["progress_score"]; ok {
		switch vv := v.(type) {
		case float64:
			score = int(vv)
		case int:
			score = vv
		case string:
			if n, err := strconv.Atoi(vv); err == nil {
				score = n
			}
		}
	}
	if v, ok := blob["progress_summary"].(string); ok {
		summary = v
	}
	return score, summary
}

// closedOrNow prefers the bead's closed_at timestamp when parseable,
// falling back to UpdatedAt and finally now.
func closedOrNow(b *beadsapi.BeadDetail, now time.Time) time.Time {
	if b == nil {
		return now
	}
	if b.ClosedAt != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, b.ClosedAt); err == nil {
				return t
			}
		}
	}
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return now
}

// created prefers the bead's CreatedAt when set, falling back to now.
func created(b *beadsapi.BeadDetail, now time.Time) time.Time {
	if b != nil && !b.CreatedAt.IsZero() {
		return b.CreatedAt
	}
	return now
}

// EventKinds returns the unique kinds referenced by the spec's events,
// preserving spec order.
func EventKinds(spec Spec) []string {
	var out []string
	for _, m := range spec.Events {
		if !slices.Contains(out, m.Kind) {
			out = append(out, m.Kind)
		}
	}
	return out
}
