package progress

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestParseSpec_Empty(t *testing.T) {
	got, err := ParseSpec("")
	if err != nil {
		t.Fatalf("expected no error for empty input, got %v", err)
	}
	if !got.IsZero() {
		t.Errorf("expected zero spec, got %+v", got)
	}
}

func TestParseSpec_Valid(t *testing.T) {
	raw := `{
		"events":[
			{"kind":"child_closed","parent":"gd-EPIC"},
			{"kind":"crew_dispatched","role":"crew","parent":"gd-EPIC"}
		],
		"windows":[
			{"name":"this_run","scope":"since_run_start"},
			{"name":"last_3_runs","scope":"last_n_runs","n":3},
			{"name":"last_24h","scope":"since","duration":"24h"}
		],
		"surface":{"in_prime":true,"in_wrapup":true}
	}`
	spec, err := ParseSpec(raw)
	if err != nil {
		t.Fatalf("ParseSpec error: %v", err)
	}
	if len(spec.Events) != 2 || len(spec.Windows) != 3 {
		t.Errorf("unexpected counts: %+v", spec)
	}
	if !spec.Surface.InPrime || !spec.Surface.InWrapup {
		t.Errorf("surface flags lost: %+v", spec.Surface)
	}
}

func TestParseSpec_RejectsBadWindow(t *testing.T) {
	cases := []string{
		`{"windows":[{"name":"","scope":"since_run_start"}]}`,
		`{"windows":[{"name":"w","scope":""}]}`,
		`{"windows":[{"name":"w","scope":"weird"}]}`,
		`{"windows":[{"name":"w","scope":"last_n_runs"}]}`,
		`{"windows":[{"name":"w","scope":"since","duration":"???"}]}`,
		`{"events":[{"kind":""}]}`,
	}
	for _, raw := range cases {
		if _, err := ParseSpec(raw); err == nil {
			t.Errorf("expected error for %s", raw)
		}
	}
}

func TestResolveTemplates(t *testing.T) {
	spec, _ := ParseSpec(`{
		"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}]
	}`)
	out := spec.ResolveTemplates("gd-XYZ", "gd-SCHED")
	if out.Events[0].Parent != "gd-XYZ" {
		t.Errorf("expected parent expanded, got %q", out.Events[0].Parent)
	}
}

func TestState_AppendEvent_Dedup(t *testing.T) {
	var st State
	rec := EventRecord{Kind: EventKindChildClosed, TargetID: "gd-A", OccurredAt: time.Unix(100, 0)}
	if !st.AppendEvent(rec) {
		t.Error("expected first append to return true")
	}
	if st.AppendEvent(rec) {
		t.Error("expected duplicate append to return false")
	}
	if len(st.Events) != 1 {
		t.Errorf("expected 1 event after dedup, got %d", len(st.Events))
	}
}

func TestState_AppendRun_BoundedRetention(t *testing.T) {
	var st State
	for range MaxRuns + 10 {
		st.AppendRun(RunRecord{AgentID: "a", ProgressScore: 0})
	}
	if len(st.Runs) != MaxRuns {
		t.Errorf("expected runs bounded to %d, got %d", MaxRuns, len(st.Runs))
	}
}

func TestState_ZeroScoreRunsInLastN(t *testing.T) {
	var st State
	for _, score := range []int{2, 0, 0, 1, 0} {
		st.AppendRun(RunRecord{ProgressScore: score})
	}
	zero, observed := st.ZeroScoreRunsInLastN(3)
	if observed != 3 {
		t.Errorf("expected observed=3, got %d", observed)
	}
	if zero != 2 { // last 3 runs are scores [0, 1, 0]
		t.Errorf("expected zero=2, got %d", zero)
	}
}

func TestState_ProgressSummaryGaps(t *testing.T) {
	var st State
	// A mix: scored+summarized (conformant), scored+empty (gap),
	// scored+whitespace-only (gap), and score-0 rows (never a gap, never counted).
	st.AppendRun(RunRecord{ProgressScore: 2, ProgressSummary: "closed gd-X"})
	st.AppendRun(RunRecord{ProgressScore: 1, ProgressSummary: ""})
	st.AppendRun(RunRecord{ProgressScore: 1, ProgressSummary: "   "})
	st.AppendRun(RunRecord{ProgressScore: 0, ProgressSummary: ""})
	st.AppendRun(RunRecord{ProgressScore: 0, ProgressSummary: "ignored"})

	gaps, scored := st.ProgressSummaryGaps()
	if scored != 3 {
		t.Errorf("expected scored=3 (only non-zero scores), got %d", scored)
	}
	if gaps != 2 {
		t.Errorf("expected gaps=2 (empty + whitespace-only summaries), got %d", gaps)
	}
}

func TestState_ProgressSummaryGaps_AllConformant(t *testing.T) {
	var st State
	st.AppendRun(RunRecord{ProgressScore: 2, ProgressSummary: "did work"})
	st.AppendRun(RunRecord{ProgressScore: 0}) // score-0, no summary needed
	gaps, scored := st.ProgressSummaryGaps()
	if gaps != 0 {
		t.Errorf("expected gaps=0, got %d", gaps)
	}
	if scored != 1 {
		t.Errorf("expected scored=1, got %d", scored)
	}
}

func TestExtractWrapupProgress(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantScore   int
		wantSummary string
	}{
		{"empty", "", 0, ""},
		{"valid", `{"progress_score":2,"progress_summary":"closed 2 bugs"}`, 2, "closed 2 bugs"},
		{"score as string", `{"progress_score":"3"}`, 3, ""},
		{"missing", `{"accomplishments":"x"}`, 0, ""},
		{"invalid json", `not json`, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score, summary := ExtractWrapupProgress(tc.raw)
			if score != tc.wantScore || summary != tc.wantSummary {
				t.Errorf("got (%d, %q), want (%d, %q)",
					score, summary, tc.wantScore, tc.wantSummary)
			}
		})
	}
}

// --- evaluator (mock client) ---

type mockClient struct {
	beads map[string][]*beadsapi.BeadDetail // keyed by query signature
	// mrBeadByTask: task bead ID → MR bead returned by MRBeadForTask. Lets
	// tests exercise both the MR-bead-present and field-fallback paths in
	// discoverMRMerged (epic ka-0000001613).
	mrBeadByTask map[string]*beadsapi.BeadDetail
	queries      []beadsapi.ListBeadsQuery // every list call, in order
	mrCalls      []string                  // every MRBeadForTask task id, in order
}

func (m *mockClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.queries = append(m.queries, q)
	key := mockKey(q)
	beads := m.beads[key]
	return &beadsapi.ListBeadsResult{Beads: beads, Total: len(beads)}, nil
}

func (m *mockClient) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	for _, beads := range m.beads {
		for _, b := range beads {
			if b.ID == id {
				return b, nil
			}
		}
	}
	return nil, nil
}

func (m *mockClient) MRBeadForTask(_ context.Context, taskBeadID string) (*beadsapi.BeadDetail, error) {
	m.mrCalls = append(m.mrCalls, taskBeadID)
	if m.mrBeadByTask == nil {
		return nil, nil
	}
	return m.mrBeadByTask[taskBeadID], nil
}

func mockKey(q beadsapi.ListBeadsQuery) string {
	return strings.Join([]string{
		strings.Join(q.Types, ","),
		strings.Join(q.Statuses, ","),
		strings.Join(q.Labels, ","),
		q.Parent,
	}, "|")
}

func TestEvaluator_DiscoverMRMerged_MRBeadPreferred(t *testing.T) {
	// Two gardener-labeled task beads: one has an MR bead linked via `produces`
	// with mr_outcome=merged (counted); the other has an MR bead that is still
	// open (not counted). Verifies the MR-bead path is consulted first.
	mc := &mockClient{
		beads: map[string][]*beadsapi.BeadDetail{},
		mrBeadByTask: map[string]*beadsapi.BeadDetail{
			"gd-T1": {ID: "gd-MR1", Type: "merge-request", Status: "closed", Fields: map[string]string{"mr_outcome": "merged"}, UpdatedAt: time.Unix(110, 0)},
			"gd-T2": {ID: "gd-MR2", Type: "merge-request", Status: "open", Fields: map[string]string{"mr_state": "opened"}, UpdatedAt: time.Unix(120, 0)},
		},
	}
	mc.beads[mockKey(beadsapi.ListBeadsQuery{Labels: []string{"gardener:cov"}})] = []*beadsapi.BeadDetail{
		{ID: "gd-T1", Type: "task", Status: "closed", UpdatedAt: time.Unix(110, 0)},
		{ID: "gd-T2", Type: "task", Status: "in_progress", UpdatedAt: time.Unix(120, 0)},
	}
	ev := &Evaluator{Client: mc}
	spec, _ := ParseSpec(`{"events":[{"kind":"mr_merged","author_label":"gardener:cov"}]}`)
	state, err := ev.Discover(context.Background(), spec, EvalContext{Now: time.Unix(300, 0)}, nil)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(state.Events) != 1 {
		t.Fatalf("expected 1 mr_merged event, got %d: %+v", len(state.Events), state.Events)
	}
	if state.Events[0].TargetID != "gd-T1" {
		t.Errorf("expected TargetID gd-T1, got %s", state.Events[0].TargetID)
	}
}

func TestEvaluator_DiscoverMRMerged_NoMRBeadDoesNotCount(t *testing.T) {
	// No MR bead linked — task-bead `merged` / `mr_state` fields are no
	// longer consulted (epic ka-0000001613 / task ka-0000008134 drops the
	// fallback). The evaluator should record zero mr_merged events.
	mc := &mockClient{
		beads:        map[string][]*beadsapi.BeadDetail{},
		mrBeadByTask: map[string]*beadsapi.BeadDetail{}, // nothing linked
	}
	mc.beads[mockKey(beadsapi.ListBeadsQuery{Labels: []string{"gardener:cov"}})] = []*beadsapi.BeadDetail{
		{ID: "gd-T1", Type: "task", Status: "closed", Fields: map[string]string{"mr_state": "merged"}, UpdatedAt: time.Unix(110, 0)},
		{ID: "gd-T2", Type: "task", Status: "closed", Fields: map[string]string{"merged": "true"}, UpdatedAt: time.Unix(120, 0)},
		{ID: "gd-T3", Type: "task", Status: "in_progress", Fields: map[string]string{"mr_state": "opened"}, UpdatedAt: time.Unix(130, 0)},
	}
	ev := &Evaluator{Client: mc}
	spec, _ := ParseSpec(`{"events":[{"kind":"mr_merged","author_label":"gardener:cov"}]}`)
	state, err := ev.Discover(context.Background(), spec, EvalContext{Now: time.Unix(300, 0)}, nil)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(state.Events) != 0 {
		t.Fatalf("expected 0 mr_merged events when no MR beads linked, got %d: %+v", len(state.Events), state.Events)
	}
}

// TestEvaluator_DiscoverMRMerged_ScopedToParent pins gd-6PXLuR3juD: a
// parent-scoped mr_merged matcher lists only that parent's children. It used
// to drop Parent and list the newest 256 beads of the whole daemon, crediting
// a merged MR anywhere to every gardener.
func TestEvaluator_DiscoverMRMerged_ScopedToParent(t *testing.T) {
	merged := func(id string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: id, Type: "merge-request", Status: "closed", Fields: map[string]string{"mr_outcome": "merged"}}
	}
	mc := &mockClient{
		beads:        map[string][]*beadsapi.BeadDetail{},
		mrBeadByTask: map[string]*beadsapi.BeadDetail{"gd-MINE": merged("gd-MR1"), "gd-OTHER": merged("gd-MR2")},
	}
	mc.beads[mockKey(beadsapi.ListBeadsQuery{Parent: "gd-EPIC"})] = []*beadsapi.BeadDetail{
		{ID: "gd-MINE", Type: "task", Status: "closed"},
	}
	mc.beads[mockKey(beadsapi.ListBeadsQuery{})] = []*beadsapi.BeadDetail{
		{ID: "gd-MINE", Type: "task", Status: "closed"},
		{ID: "gd-OTHER", Type: "task", Status: "closed"},
	}
	ev := &Evaluator{Client: mc}
	spec, _ := ParseSpec(`{"events":[{"kind":"mr_merged","parent":"{{assign_bead}}"}]}`)
	state, err := ev.Discover(context.Background(), spec, EvalContext{AssignBead: "gd-EPIC", Now: time.Unix(300, 0)}, nil)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(state.Events) != 1 || state.Events[0].TargetID != "gd-MINE" {
		t.Fatalf("want only gd-MINE credited, got %+v", state.Events)
	}
	if len(mc.queries) != 1 || mc.queries[0].Parent != "gd-EPIC" {
		t.Fatalf("mr_merged list must carry Parent=gd-EPIC, got %+v", mc.queries)
	}
}

// TestEvaluator_DiscoverMRMerged_SkipsCreditedWalk pins that a bead already
// credited as mr_merged is not walked to its MR bead again on later passes.
func TestEvaluator_DiscoverMRMerged_SkipsCreditedWalk(t *testing.T) {
	mc := &mockClient{
		beads: map[string][]*beadsapi.BeadDetail{},
		mrBeadByTask: map[string]*beadsapi.BeadDetail{
			"gd-T1": {ID: "gd-MR1", Type: "merge-request", Status: "closed", Fields: map[string]string{"mr_outcome": "merged"}},
		},
	}
	mc.beads[mockKey(beadsapi.ListBeadsQuery{Parent: "gd-EPIC"})] = []*beadsapi.BeadDetail{
		{ID: "gd-T1", Type: "task", Status: "closed"},
		{ID: "gd-T2", Type: "task", Status: "open"},
	}
	ev := &Evaluator{Client: mc}
	spec, _ := ParseSpec(`{"events":[{"kind":"mr_merged","parent":"gd-EPIC"}]}`)
	state, err := ev.Discover(context.Background(), spec, EvalContext{Now: time.Unix(300, 0)}, nil)
	if err != nil || len(state.Events) != 1 {
		t.Fatalf("first pass: err=%v events=%+v", err, state.Events)
	}
	mc.mrCalls = nil
	if _, err := ev.Discover(context.Background(), spec, EvalContext{State: state, Now: time.Unix(400, 0)}, nil); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !slices.Equal(mc.mrCalls, []string{"gd-T2"}) {
		t.Errorf("second pass must walk only the uncredited gd-T2, walked %v", mc.mrCalls)
	}
}

// TestEvaluator_ListsAreLean pins the gd-6PXLuR3juD projection: every
// discovery list asks fields_only for the keys it reads (none, or the two
// decision_resolved filters on) and omits description/notes.
func TestEvaluator_ListsAreLean(t *testing.T) {
	mc := &mockClient{beads: map[string][]*beadsapi.BeadDetail{}}
	ev := &Evaluator{Client: mc}
	spec, _ := ParseSpec(`{"events":[
		{"kind":"child_closed","parent":"gd-EPIC"},
		{"kind":"mr_merged","parent":"gd-EPIC"},
		{"kind":"decision_resolved"},
		{"kind":"crew_dispatched","parent":"gd-EPIC","role":"crew"}]}`)
	if _, err := ev.Discover(context.Background(), spec, EvalContext{Now: time.Unix(300, 0)}, nil); err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(mc.queries) != 4 {
		t.Fatalf("want 4 list calls, got %d", len(mc.queries))
	}
	for i, q := range mc.queries {
		if len(q.FieldsOnly) == 0 || !slices.Equal(q.Omit, []string{"description", "notes"}) {
			t.Errorf("query %d is not lean: fields_only=%v omit=%v", i, q.FieldsOnly, q.Omit)
		}
	}
	if !slices.Equal(mc.queries[2].FieldsOnly, []string{"cancel_reason", "raised_by_agent"}) {
		t.Errorf("decision_resolved must keep the fields it filters on, got %v", mc.queries[2].FieldsOnly)
	}
}

func TestEvaluator_DiscoverChildClosed(t *testing.T) {
	mc := &mockClient{beads: map[string][]*beadsapi.BeadDetail{}}
	mc.beads[mockKey(beadsapi.ListBeadsQuery{Parent: "gd-EPIC", Statuses: []string{"closed", "partial"}})] = []*beadsapi.BeadDetail{
		{ID: "gd-A", Type: "task", Status: "closed", UpdatedAt: time.Unix(100, 0)},
		{ID: "gd-B", Type: "task", Status: "closed", UpdatedAt: time.Unix(200, 0)},
	}
	ev := &Evaluator{Client: mc}
	spec, _ := ParseSpec(`{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}]}`)
	state, err := ev.Discover(context.Background(), spec, EvalContext{
		AssignBead: "gd-EPIC",
		ScheduleID: "gd-SCHED",
		Now:        time.Unix(300, 0),
	}, nil)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(state.Events) != 2 {
		t.Errorf("expected 2 events, got %d: %+v", len(state.Events), state.Events)
	}

	// Re-discover should be a no-op (dedup).
	state2, err := ev.Discover(context.Background(), spec, EvalContext{
		AssignBead: "gd-EPIC",
		ScheduleID: "gd-SCHED",
		State:      state,
		Now:        time.Unix(400, 0),
	}, nil)
	if err != nil {
		t.Fatalf("Discover #2 error: %v", err)
	}
	if len(state2.Events) != 2 {
		t.Errorf("dedup failed; got %d events", len(state2.Events))
	}
}

// TestEvaluator_DiscoverChildClosed_CountsPartial asserts that a child in
// 'partial' status counts toward a child_closed join exactly like a 'closed'
// child. 'partial' is terminal-for-progression (ka-0000007305 §2.3), so a
// waits_for: all-children barrier must not stall waiting on a step that
// completed-with-failures.
func TestEvaluator_DiscoverChildClosed_CountsPartial(t *testing.T) {
	mc := &mockClient{beads: map[string][]*beadsapi.BeadDetail{}}
	mc.beads[mockKey(beadsapi.ListBeadsQuery{Parent: "gd-EPIC", Statuses: []string{"closed", "partial"}})] = []*beadsapi.BeadDetail{
		{ID: "gd-A", Type: "task", Status: "closed", UpdatedAt: time.Unix(100, 0)},
		{ID: "gd-B", Type: "task", Status: "partial", UpdatedAt: time.Unix(200, 0)},
	}
	ev := &Evaluator{Client: mc}
	spec, _ := ParseSpec(`{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}]}`)
	state, err := ev.Discover(context.Background(), spec, EvalContext{
		AssignBead: "gd-EPIC",
		ScheduleID: "gd-SCHED",
		Now:        time.Unix(300, 0),
	}, nil)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(state.Events) != 2 {
		t.Errorf("expected 2 child_closed events (1 closed + 1 partial), got %d: %+v", len(state.Events), state.Events)
	}
	gotPartial := false
	for _, e := range state.Events {
		if e.TargetID == "gd-B" {
			gotPartial = true
		}
	}
	if !gotPartial {
		t.Errorf("partial child gd-B should count toward child_closed; events=%+v", state.Events)
	}
}

func TestCountByWindow_SinceRunStart(t *testing.T) {
	spec, _ := ParseSpec(`{
		"events":[{"kind":"child_closed"}],
		"windows":[{"name":"this_run","scope":"since_run_start"}]
	}`)
	state := State{
		CurrentRunStart: time.Unix(150, 0),
		Events: []EventRecord{
			{Kind: EventKindChildClosed, TargetID: "gd-A", OccurredAt: time.Unix(100, 0)},
			{Kind: EventKindChildClosed, TargetID: "gd-B", OccurredAt: time.Unix(200, 0)},
		},
	}
	counts := CountByWindow(spec, state, time.Unix(300, 0))
	if counts["this_run"][EventKindChildClosed] != 1 {
		t.Errorf("expected 1 event in this_run, got %d", counts["this_run"][EventKindChildClosed])
	}
}

func TestCountByWindow_LastNRuns(t *testing.T) {
	spec, _ := ParseSpec(`{
		"events":[{"kind":"child_closed"}],
		"windows":[{"name":"last_3_runs","scope":"last_n_runs","n":3}]
	}`)
	state := State{
		Runs: []RunRecord{
			{StartedAt: time.Unix(100, 0), ProgressScore: 1},
			{StartedAt: time.Unix(200, 0), ProgressScore: 1},
			{StartedAt: time.Unix(300, 0), ProgressScore: 1},
			{StartedAt: time.Unix(400, 0), ProgressScore: 1},
		},
		Events: []EventRecord{
			{Kind: EventKindChildClosed, TargetID: "gd-A", OccurredAt: time.Unix(150, 0)},
			{Kind: EventKindChildClosed, TargetID: "gd-B", OccurredAt: time.Unix(250, 0)},
			{Kind: EventKindChildClosed, TargetID: "gd-C", OccurredAt: time.Unix(350, 0)},
		},
	}
	counts := CountByWindow(spec, state, time.Unix(500, 0))
	// Last 3 runs start at unix=200, so events at 150 are excluded.
	if counts["last_3_runs"][EventKindChildClosed] != 2 {
		t.Errorf("expected 2 events in last_3_runs, got %d",
			counts["last_3_runs"][EventKindChildClosed])
	}
}

func TestRenderPrimeBlock(t *testing.T) {
	spec, _ := ParseSpec(`{
		"events":[
			{"kind":"child_closed","parent":"gd-EPIC"},
			{"kind":"crew_dispatched","parent":"gd-EPIC"}
		],
		"windows":[
			{"name":"this_run","scope":"since_run_start"}
		],
		"surface":{"in_prime":true}
	}`)
	state := State{
		CurrentRunStart: time.Unix(100, 0).UTC(),
		Events: []EventRecord{
			{Kind: EventKindChildClosed, TargetID: "gd-A", OccurredAt: time.Unix(150, 0).UTC()},
		},
	}
	out := RenderPrimeBlock(spec, state, time.Unix(200, 0).UTC())
	if !strings.Contains(out, "## Loop Progress") {
		t.Errorf("expected header, got: %s", out)
	}
	if !strings.Contains(out, "child_closed") {
		t.Errorf("expected child_closed line, got: %s", out)
	}
	if !strings.Contains(out, "×1") {
		t.Errorf("expected count of 1, got: %s", out)
	}
	if !strings.Contains(out, "gd-A") {
		t.Errorf("expected target id, got: %s", out)
	}
}

func TestRenderPrimeBlock_NoEventsOrWindows(t *testing.T) {
	if got := RenderPrimeBlock(Spec{}, State{}, time.Now()); got != "" {
		t.Errorf("expected empty render, got %q", got)
	}
}

// TestRenderPrimeBlock_SelfReportContract verifies the wrap-up self-report
// contract line is appended exactly when the schedule surfaces progress in the
// wrap-up (spec.Surface.InWrapup), and omitted otherwise (tune ka-0000007473).
func TestRenderPrimeBlock_SelfReportContract(t *testing.T) {
	base := `{
		"events":[{"kind":"child_closed","parent":"gd-EPIC"}],
		"windows":[{"name":"this_run","scope":"since_run_start"}],
		"surface":{"in_prime":true%s}
	}`
	state := State{CurrentRunStart: time.Unix(100, 0).UTC()}

	withWrapup, _ := ParseSpec(fmt.Sprintf(base, `,"in_wrapup":true`))
	out := RenderPrimeBlock(withWrapup, state, time.Unix(200, 0).UTC())
	if !strings.Contains(out, "Self-report contract") {
		t.Errorf("expected self-report contract when in_wrapup=true, got: %s", out)
	}
	if !strings.Contains(out, "progress_score") {
		t.Errorf("expected contract to name progress_score, got: %s", out)
	}

	noWrapup, _ := ParseSpec(fmt.Sprintf(base, ``))
	out = RenderPrimeBlock(noWrapup, state, time.Unix(200, 0).UTC())
	if strings.Contains(out, "Self-report contract") {
		t.Errorf("expected no self-report contract when in_wrapup=false, got: %s", out)
	}
}

// TestRenderPrimeBlock_SelfReportContract_ReportOnly verifies that a
// report-only sentinel gardener — one whose ONLY progress event is a
// `wrapup_field` self-report, with no delegation kind (child_closed /
// mr_merged / decision_resolved) — gets the report-only contract wording,
// which credits completing the run's own deliverable as >= 1 progress rather
// than the delegator wording that would floor its score at 0 every cycle and
// wedge it in a no_progress_for deadlock (ka-0000007551 / ka-0000005781).
func TestRenderPrimeBlock_SelfReportContract_ReportOnly(t *testing.T) {
	state := State{CurrentRunStart: time.Unix(100, 0).UTC()}

	reportOnly, err := ParseSpec(`{
		"events":[{"kind":"wrapup_field","field":"progress_score","op":">=","value":"1"}],
		"windows":[{"name":"this_run","scope":"since_run_start"}],
		"surface":{"in_prime":true,"in_wrapup":true}
	}`)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	out := RenderPrimeBlock(reportOnly, state, time.Unix(200, 0).UTC())
	if !strings.Contains(out, "Self-report contract") {
		t.Errorf("expected self-report contract for report-only gardener, got: %s", out)
	}
	if !strings.Contains(out, "report-only loop") {
		t.Errorf("expected report-only wording crediting the run's deliverable, got: %s", out)
	}
	if strings.Contains(out, "a child closed, an MR merged, or a decision resolved") {
		t.Errorf("report-only gardener must NOT get the delegator wording that floors its score, got: %s", out)
	}

	// A spec that references a delegation kind keeps the delegator wording.
	delegator, err := ParseSpec(`{
		"events":[{"kind":"child_closed","parent":"gd-EPIC"}],
		"windows":[{"name":"this_run","scope":"since_run_start"}],
		"surface":{"in_prime":true,"in_wrapup":true}
	}`)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	out = RenderPrimeBlock(delegator, state, time.Unix(200, 0).UTC())
	if !strings.Contains(out, "a child closed, an MR merged, or a decision resolved") {
		t.Errorf("expected delegator wording for a spec with a credited kind, got: %s", out)
	}
	if strings.Contains(out, "report-only loop") {
		t.Errorf("delegator gardener must not get report-only wording, got: %s", out)
	}
}

// TestEvaluator_DiscoverDecisionResolved_ExcludesCancelDuplicates verifies
// that decisions cancelled with cancel_reason=cancel_duplicates are not
// counted as decision_resolved progress events. Without this filter a
// gardener can game its own no_progress_for stop counter by repeatedly
// raising the same decision and having the operator dismiss each one
// (ka-0000006682).
func TestEvaluator_DiscoverDecisionResolved_ExcludesCancelDuplicates(t *testing.T) {
	mc := &mockClient{beads: map[string][]*beadsapi.BeadDetail{}}
	mc.beads[mockKey(beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"closed"},
	})] = []*beadsapi.BeadDetail{
		// Normal resolution — counts.
		{ID: "gd-D1", Type: "decision", Status: "closed",
			Fields:    map[string]string{"chosen": "option-a"},
			UpdatedAt: time.Unix(100, 0)},
		// Cancelled as duplicate — must NOT count.
		{ID: "gd-D2", Type: "decision", Status: "closed",
			Fields:    map[string]string{"chosen": "dismissed", "cancel_reason": "cancel_duplicates"},
			UpdatedAt: time.Unix(110, 0)},
		// Cancelled for a different reason — still counts.
		{ID: "gd-D3", Type: "decision", Status: "closed",
			Fields:    map[string]string{"chosen": "dismissed", "cancel_reason": "stale"},
			UpdatedAt: time.Unix(120, 0)},
	}
	ev := &Evaluator{Client: mc}
	spec, _ := ParseSpec(`{"events":[{"kind":"decision_resolved"}]}`)
	state, err := ev.Discover(context.Background(), spec, EvalContext{Now: time.Unix(300, 0)}, nil)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(state.Events) != 2 {
		t.Fatalf("expected 2 decision_resolved events, got %d: %+v", len(state.Events), state.Events)
	}
	got := map[string]bool{}
	for _, ev := range state.Events {
		got[ev.TargetID] = true
	}
	if !got["gd-D1"] {
		t.Errorf("expected gd-D1 (normal close) to count")
	}
	if got["gd-D2"] {
		t.Errorf("expected gd-D2 (cancel_duplicates) to be excluded")
	}
	if !got["gd-D3"] {
		t.Errorf("expected gd-D3 (cancel_reason=stale) to count — only cancel_duplicates is excluded")
	}
}

func TestFinalizeRun(t *testing.T) {
	var st State
	FinalizeRun(&st, "agent-1", `{"progress_score":2,"progress_summary":"x"}`, "done",
		time.Unix(100, 0), time.Unix(200, 0))
	if len(st.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(st.Runs))
	}
	r := st.Runs[0]
	if r.ProgressScore != 2 || r.ProgressSummary != "x" || r.AgentState != "done" {
		t.Errorf("unexpected run record: %+v", r)
	}
}

// TestFinalizeRun_EventAwareRescue covers the event-aware no-progress
// reconciliation (bug ka-0000006914): a self-reported score-0 run is lifted to
// score 1 when a credited progress event landed inside its run window, but
// genuinely barren runs and out-of-window events leave the score at 0.
func TestFinalizeRun_EventAwareRescue(t *testing.T) {
	const wrapZero = `{"progress_score":0}`
	cases := []struct {
		name      string
		events    []EventRecord
		wrapup    string
		startedAt time.Time
		closedAt  time.Time
		wantScore int
	}{
		{
			name:      "mr_merged_in_window_rescues",
			events:    []EventRecord{{Kind: EventKindMRMerged, TargetID: "gd-MR1", OccurredAt: time.Unix(150, 0)}},
			wrapup:    wrapZero,
			startedAt: time.Unix(100, 0), closedAt: time.Unix(200, 0),
			wantScore: 1,
		},
		{
			name:      "child_closed_in_window_rescues",
			events:    []EventRecord{{Kind: EventKindChildClosed, TargetID: "gd-C1", OccurredAt: time.Unix(150, 0)}},
			wrapup:    wrapZero,
			startedAt: time.Unix(100, 0), closedAt: time.Unix(200, 0),
			wantScore: 1,
		},
		{
			name:      "decision_resolved_in_window_rescues",
			events:    []EventRecord{{Kind: EventKindDecisionResolved, TargetID: "gd-D1", OccurredAt: time.Unix(150, 0)}},
			wrapup:    wrapZero,
			startedAt: time.Unix(100, 0), closedAt: time.Unix(200, 0),
			wantScore: 1,
		},
		{
			name:      "no_events_stays_barren",
			events:    nil,
			wrapup:    wrapZero,
			startedAt: time.Unix(100, 0), closedAt: time.Unix(200, 0),
			wantScore: 0,
		},
		{
			name:      "event_before_window_not_counted",
			events:    []EventRecord{{Kind: EventKindMRMerged, TargetID: "gd-MR1", OccurredAt: time.Unix(50, 0)}},
			wrapup:    wrapZero,
			startedAt: time.Unix(100, 0), closedAt: time.Unix(200, 0),
			wantScore: 0,
		},
		{
			name:      "event_after_window_not_counted",
			events:    []EventRecord{{Kind: EventKindMRMerged, TargetID: "gd-MR1", OccurredAt: time.Unix(250, 0)}},
			wrapup:    wrapZero,
			startedAt: time.Unix(100, 0), closedAt: time.Unix(200, 0),
			wantScore: 0,
		},
		{
			name:      "crew_dispatched_does_not_rescue",
			events:    []EventRecord{{Kind: EventKindCrewDispatched, TargetID: "gd-A1", OccurredAt: time.Unix(150, 0)}},
			wrapup:    wrapZero,
			startedAt: time.Unix(100, 0), closedAt: time.Unix(200, 0),
			wantScore: 0,
		},
		{
			name:      "nonzero_score_untouched",
			events:    nil,
			wrapup:    `{"progress_score":3}`,
			startedAt: time.Unix(100, 0), closedAt: time.Unix(200, 0),
			wantScore: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := State{Events: tc.events}
			FinalizeRun(&st, "agent-1", tc.wrapup, "done", tc.startedAt, tc.closedAt)
			if len(st.Runs) != 1 {
				t.Fatalf("expected 1 run, got %d", len(st.Runs))
			}
			if st.Runs[0].ProgressScore != tc.wantScore {
				t.Fatalf("got score %d, want %d", st.Runs[0].ProgressScore, tc.wantScore)
			}
		})
	}
}

// TestHasCreditedEventInWindow exercises the window-membership helper directly,
// including the open-ended (zero closedAt) and unbounded-lower (zero startedAt)
// boundary cases.
func TestHasCreditedEventInWindow(t *testing.T) {
	st := State{Events: []EventRecord{
		{Kind: EventKindMRMerged, TargetID: "gd-MR1", OccurredAt: time.Unix(150, 0)},
	}}
	if !st.HasCreditedEventInWindow(time.Unix(100, 0), time.Unix(200, 0)) {
		t.Error("event at 150 should be inside [100,200]")
	}
	if st.HasCreditedEventInWindow(time.Unix(160, 0), time.Unix(200, 0)) {
		t.Error("event at 150 should be outside [160,200]")
	}
	if !st.HasCreditedEventInWindow(time.Unix(100, 0), time.Time{}) {
		t.Error("zero closedAt should treat window as open-ended")
	}
	if !st.HasCreditedEventInWindow(time.Time{}, time.Unix(200, 0)) {
		t.Error("zero startedAt should drop the lower bound")
	}
	empty := State{}
	if empty.HasCreditedEventInWindow(time.Unix(100, 0), time.Unix(200, 0)) {
		t.Error("empty events should never match")
	}
}

// TestReconcileAsyncProgress covers the async-attribution reconciliation
// (tune task ka-0000001254, rubric §D1): a score-0 run is lifted to 1 when a
// credited event lands in its attribution interval [StartedAt, nextRun.StartedAt)
// — crucially INCLUDING the gap AFTER the run's own closed_at, which
// FinalizeRun's [startedAt, closedAt] rescue cannot reach.
func TestReconcileAsyncProgress(t *testing.T) {
	// Two delegating runs: run A [100,150] dispatched crew, run B started at 300.
	// The crew's child closes at 200 — AFTER A's closed_at (150) but BEFORE B
	// started (300). FinalizeRun credited neither (200 ∉ [100,150], 200 ∉ [300,…]),
	// so both recorded score 0. Reconciliation must attribute the 200 event to A.
	runA := RunRecord{AgentID: "a", StartedAt: time.Unix(100, 0), ClosedAt: time.Unix(150, 0), ProgressScore: 0}
	runB := RunRecord{AgentID: "b", StartedAt: time.Unix(300, 0), ClosedAt: time.Unix(350, 0), ProgressScore: 0}
	st := State{
		Runs:   []RunRecord{runA, runB},
		Events: []EventRecord{{Kind: EventKindChildClosed, TargetID: "gd-C1", OccurredAt: time.Unix(200, 0)}},
	}
	lifted := st.ReconcileAsyncProgress()
	if lifted != 1 {
		t.Fatalf("expected 1 run lifted, got %d", lifted)
	}
	if st.Runs[0].ProgressScore != 1 {
		t.Errorf("run A (owns [100,300)) should be lifted to 1, got %d", st.Runs[0].ProgressScore)
	}
	if st.Runs[1].ProgressScore != 0 {
		t.Errorf("run B should stay 0 (no event in its interval), got %d", st.Runs[1].ProgressScore)
	}
	// Idempotent: a second pass lifts nothing new.
	if again := st.ReconcileAsyncProgress(); again != 0 {
		t.Errorf("expected idempotent second pass to lift 0, got %d", again)
	}
}

// TestReconcileAsyncProgress_IntervalAttribution pins the half-open interval
// semantics — an event exactly at the next run's StartedAt credits the NEXT run,
// never the prior one, so no event is double-counted.
func TestReconcileAsyncProgress_IntervalAttribution(t *testing.T) {
	cases := []struct {
		name      string
		eventAt   time.Time
		wantScore [2]int // {runA, runB}
	}{
		{"before_A_start_credits_neither", time.Unix(50, 0), [2]int{0, 0}},
		{"in_A_window_credits_A", time.Unix(120, 0), [2]int{1, 0}},
		{"in_gap_after_A_close_credits_A", time.Unix(200, 0), [2]int{1, 0}},
		{"exactly_at_B_start_credits_B", time.Unix(300, 0), [2]int{0, 1}},
		{"in_B_window_credits_B", time.Unix(320, 0), [2]int{0, 1}},
		{"after_B_start_open_ended_credits_B", time.Unix(9999, 0), [2]int{0, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := State{
				Runs: []RunRecord{
					{AgentID: "a", StartedAt: time.Unix(100, 0), ClosedAt: time.Unix(150, 0)},
					{AgentID: "b", StartedAt: time.Unix(300, 0), ClosedAt: time.Unix(350, 0)},
				},
				Events: []EventRecord{{Kind: EventKindChildClosed, TargetID: "gd-C1", OccurredAt: tc.eventAt}},
			}
			st.ReconcileAsyncProgress()
			if st.Runs[0].ProgressScore != tc.wantScore[0] || st.Runs[1].ProgressScore != tc.wantScore[1] {
				t.Errorf("event@%v: got scores [%d,%d], want %v",
					tc.eventAt.Unix(), st.Runs[0].ProgressScore, st.Runs[1].ProgressScore, tc.wantScore)
			}
		})
	}
}

// TestReconcileAsyncProgress_Guards covers the non-lifting paths: a non-zero
// score is never lowered, crew_dispatched never rescues, and a run with no
// StartedAt is skipped (ka-0000001254).
func TestReconcileAsyncProgress_Guards(t *testing.T) {
	t.Run("nonzero_score_untouched", func(t *testing.T) {
		st := State{
			Runs:   []RunRecord{{AgentID: "a", StartedAt: time.Unix(100, 0), ProgressScore: 3}},
			Events: []EventRecord{{Kind: EventKindChildClosed, TargetID: "gd-C1", OccurredAt: time.Unix(120, 0)}},
		}
		if st.ReconcileAsyncProgress() != 0 {
			t.Error("a run with a self-reported score must not be re-scored")
		}
		if st.Runs[0].ProgressScore != 3 {
			t.Errorf("score lowered to %d; must stay 3", st.Runs[0].ProgressScore)
		}
	})
	t.Run("crew_dispatched_does_not_rescue", func(t *testing.T) {
		st := State{
			Runs:   []RunRecord{{AgentID: "a", StartedAt: time.Unix(100, 0)}},
			Events: []EventRecord{{Kind: EventKindCrewDispatched, TargetID: "gd-A1", OccurredAt: time.Unix(120, 0)}},
		}
		if st.ReconcileAsyncProgress() != 0 {
			t.Error("crew_dispatched is not a credited outcome and must not rescue")
		}
	})
	t.Run("zero_startedAt_skipped", func(t *testing.T) {
		st := State{
			Runs:   []RunRecord{{AgentID: "a"}}, // no StartedAt → no attributable interval
			Events: []EventRecord{{Kind: EventKindChildClosed, TargetID: "gd-C1", OccurredAt: time.Unix(120, 0)}},
		}
		if st.ReconcileAsyncProgress() != 0 {
			t.Error("a run with a zero StartedAt has no interval and must be skipped")
		}
	})
	t.Run("intermediate_zero_startedAt_does_not_open_interval", func(t *testing.T) {
		// A malformed middle run (zero StartedAt) must not make run A's interval
		// falsely open-ended: the upper bound skips forward to run C's start, so
		// an event at t=500 (after C started) credits C, not A.
		st := State{
			Runs: []RunRecord{
				{AgentID: "a", StartedAt: time.Unix(100, 0)},
				{AgentID: "b"}, // malformed: no StartedAt
				{AgentID: "c", StartedAt: time.Unix(400, 0)},
			},
			Events: []EventRecord{{Kind: EventKindChildClosed, TargetID: "gd-C1", OccurredAt: time.Unix(500, 0)}},
		}
		st.ReconcileAsyncProgress()
		if st.Runs[0].ProgressScore != 0 {
			t.Errorf("run A [100,400) must not swallow the t=500 event, got score %d", st.Runs[0].ProgressScore)
		}
		if st.Runs[2].ProgressScore != 1 {
			t.Errorf("run C [400,+inf) owns the t=500 event, want score 1, got %d", st.Runs[2].ProgressScore)
		}
	})
}

// TestExtractWrapupGoalSatisfied covers the goal_satisfied wrap-up extraction
// that backs the gardener-loop master gate (bead ka-0000005353).
func TestExtractWrapupGoalSatisfied(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"yes", `{"goal_satisfied":"yes"}`, "yes"},
		{"no", `{"goal_satisfied":"no"}`, "no"},
		{"trims_and_lowercases", `{"goal_satisfied":" YES "}`, "yes"},
		{"missing", `{"progress_score":1}`, ""},
		{"empty_input", ``, ""},
		{"bad_json", `{not json`, ""},
		{"non_string", `{"goal_satisfied":true}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractWrapupGoalSatisfied(tc.raw); got != tc.want {
				t.Fatalf("raw=%q: got %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestFinalizeRun_CapturesGoalSatisfied confirms FinalizeRun threads the
// goal_satisfied wrap-up field onto the RunRecord.
func TestFinalizeRun_CapturesGoalSatisfied(t *testing.T) {
	var st State
	FinalizeRun(&st, "gd-agent", `{"goal_satisfied":"yes","progress_score":2}`, "done", time.Time{}, time.Time{})
	if len(st.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(st.Runs))
	}
	if st.Runs[0].GoalSatisfied != "yes" {
		t.Errorf("expected GoalSatisfied=yes, got %q", st.Runs[0].GoalSatisfied)
	}
}

// TestProgressYield covers the rubric §D1 numerator/denominator computation
// (task ka-0000004982): the "score>=1 OR credited-event" union over the last N
// runs. This is the measurement the 2026-08-26 scorecard got wrong by reading
// run_log (score-less) or state.Events (empty for a wrapup_field-only spec)
// instead of progress_state.runs.
func TestProgressYield(t *testing.T) {
	// run A [100,150] self-reported score 0 but a child closed at 200 (in its
	// [100,300) attribution interval) → event-credited.
	// run B [300,350] self-reported score 2 → self-reported.
	// run C [400,450] self-reported score 0, no event → uncredited.
	st := State{
		Runs: []RunRecord{
			{AgentID: "a", StartedAt: time.Unix(100, 0), ClosedAt: time.Unix(150, 0), ProgressScore: 0},
			{AgentID: "b", StartedAt: time.Unix(300, 0), ClosedAt: time.Unix(350, 0), ProgressScore: 2},
			{AgentID: "c", StartedAt: time.Unix(400, 0), ClosedAt: time.Unix(450, 0), ProgressScore: 0},
		},
		Events: []EventRecord{{Kind: EventKindChildClosed, TargetID: "gd-C1", OccurredAt: time.Unix(200, 0)}},
	}
	prog, scored, self, event := st.ProgressYield(0)
	if scored != 3 {
		t.Fatalf("scoredRuns = %d, want 3", scored)
	}
	if prog != 2 {
		t.Errorf("progressRuns = %d, want 2 (A event-credited + B self-reported)", prog)
	}
	if self != 1 || event != 1 {
		t.Errorf("self/event = %d/%d, want 1/1", self, event)
	}
}

// TestProgressYield_Window pins that n<=0 counts all runs and n>0 counts only
// the most-recent n, and that ProgressYield never mutates the state (unlike
// ReconcileAsyncProgress, which lifts scores in place).
func TestProgressYield_Window(t *testing.T) {
	var runs []RunRecord
	for i := 0; i < 5; i++ {
		runs = append(runs, RunRecord{AgentID: "old", ProgressScore: 0})
	}
	for i := 0; i < 3; i++ {
		runs = append(runs, RunRecord{AgentID: "new", ProgressScore: 1})
	}
	st := State{Runs: runs}

	prog, scored, _, _ := st.ProgressYield(3)
	if scored != 3 || prog != 3 {
		t.Errorf("window=3: got %d/%d, want 3/3", prog, scored)
	}
	prog, scored, _, _ = st.ProgressYield(0)
	if scored != 8 || prog != 3 {
		t.Errorf("window=all: got %d/%d, want 3/8", prog, scored)
	}
	// ProgressYield must not mutate scores (no in-place lift).
	if st.Runs[0].ProgressScore != 0 {
		t.Errorf("ProgressYield mutated run score to %d, want 0", st.Runs[0].ProgressScore)
	}
}
