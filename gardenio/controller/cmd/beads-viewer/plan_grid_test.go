package main

import (
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestBuildPlanGrid_Flat proves an unphased plan yields a single untitled row of
// its issue children, in the same display order (explicit step_order overlay,
// then creation order) the detail timeline and the now/next chip use — so a
// square's #N matches the timeline — and that non-issue children (bundle,
// decision) never become squares.
func TestBuildPlanGrid_Flat(t *testing.T) {
	tm := func(day int) time.Time { return time.Date(2026, 7, day, 0, 0, 0, 0, time.UTC) }
	plan := &beadsapi.BeadDetail{ID: "gd-plan", Type: "epic"}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-t1":  {ID: "gd-t1", Type: "task", Status: "closed", Title: "First", CreatedAt: tm(1)},
		"gd-t2":  {ID: "gd-t2", Type: "task", Status: "in_progress", Title: "Second", CreatedAt: tm(2)},
		"gd-t3":  {ID: "gd-t3", Type: "bug", Status: "open", Title: "Third", CreatedAt: tm(3)},
		"gd-bun": {ID: "gd-bun", Type: "bundle", Status: "open"},
		"gd-dec": {ID: "gd-dec", Type: "decision", Status: "open"},
	}
	grid := buildPlanGrid(plan, nil, []string{"gd-t1", "gd-t2", "gd-t3", "gd-bun", "gd-dec"}, nil, beads, nil)
	if len(grid) != 1 {
		t.Fatalf("flat plan: got %d rows, want 1", len(grid))
	}
	row := grid[0]
	if row.Title != "" {
		t.Errorf("flat plan row title = %q, want empty", row.Title)
	}
	if len(row.Squares) != 3 {
		t.Fatalf("flat plan: got %d squares, want 3 (bundle + decision excluded)", len(row.Squares))
	}
	want := []planGridSquare{
		{ID: "gd-t1", Number: 1, Title: "First", Status: "closed"},
		{ID: "gd-t2", Number: 2, Title: "Second", Status: "in_progress"},
		{ID: "gd-t3", Number: 3, Title: "Third", Status: "open"},
	}
	for i, w := range want {
		if row.Squares[i] != w {
			t.Errorf("square[%d] = %+v, want %+v", i, row.Squares[i], w)
		}
	}
}

// TestBuildPlanGrid_StepOrder proves the flat row honors the plan epic's
// fields.step_order overlay (a reordered step sorts to its listed position, and
// #N follows the display order) so the grid never transposes relative to the
// timeline.
func TestBuildPlanGrid_StepOrder(t *testing.T) {
	tm := func(day int) time.Time { return time.Date(2026, 7, day, 0, 0, 0, 0, time.UTC) }
	plan := &beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Fields: map[string]string{
		"step_order": `["gd-t3","gd-t1"]`,
	}}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-t1": {ID: "gd-t1", Type: "task", Status: "open", Title: "One", CreatedAt: tm(1)},
		"gd-t2": {ID: "gd-t2", Type: "task", Status: "open", Title: "Two", CreatedAt: tm(2)},
		"gd-t3": {ID: "gd-t3", Type: "task", Status: "open", Title: "Three", CreatedAt: tm(3)},
	}
	grid := buildPlanGrid(plan, nil, []string{"gd-t1", "gd-t2", "gd-t3"}, nil, beads, nil)
	if len(grid) != 1 {
		t.Fatalf("got %d rows, want 1", len(grid))
	}
	gotIDs := []string{grid[0].Squares[0].ID, grid[0].Squares[1].ID, grid[0].Squares[2].ID}
	wantIDs := []string{"gd-t3", "gd-t1", "gd-t2"} // listed first (t3,t1), then unlisted by creation (t2)
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] || grid[0].Squares[i].Number != i+1 {
			t.Errorf("square[%d] = %s #%d, want %s #%d", i, gotIDs[i], grid[0].Squares[i].Number, wantIDs[i], i+1)
		}
	}
}

// TestBuildPlanGrid_Phased proves a phased plan yields one row per phase (in
// phase_order, labeled by the phase title), a trailing "Other" row for the
// unphased remainder, an empty phase renders labeled but square-less, and a
// step double-parented under a phase renders under the phase — never again in
// the flat remainder (mirroring renderedStepRollup / the timeline).
func TestBuildPlanGrid_Phased(t *testing.T) {
	plan := &beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Fields: map[string]string{
		"phase_order": `["gd-pR","gd-pI","gd-pEmpty"]`,
	}}
	direct := map[string]*beadsapi.BeadDetail{
		"gd-pR":     {ID: "gd-pR", Type: "epic", Status: "closed", Title: "Research"},
		"gd-pI":     {ID: "gd-pI", Type: "epic", Status: "in_progress", Title: "Implementation"},
		"gd-pEmpty": {ID: "gd-pEmpty", Type: "epic", Status: "open", Title: "Validation"},
		"gd-dup":    {ID: "gd-dup", Type: "task", Status: "open", Title: "Dup"}, // also under pR
		"gd-loose":  {ID: "gd-loose", Type: "task", Status: "open", Title: "Loose"},
		"gd-bun":    {ID: "gd-bun", Type: "bundle", Status: "open"},
	}
	grand := map[string]*beadsapi.BeadDetail{
		"gd-r1":  {ID: "gd-r1", Type: "task", Status: "closed", Title: "R1"},
		"gd-dup": {ID: "gd-dup", Type: "task", Status: "open", Title: "Dup"},
		"gd-i1":  {ID: "gd-i1", Type: "task", Status: "blocked", Title: "I1"},
	}
	order := []string{"gd-pR", "gd-pI", "gd-pEmpty"}
	bySub := map[string][]string{
		"gd-pR":     {"gd-r1", "gd-dup"},
		"gd-pI":     {"gd-i1"},
		"gd-pEmpty": nil,
	}
	directIDs := []string{"gd-pR", "gd-pI", "gd-pEmpty", "gd-dup", "gd-loose", "gd-bun"}
	grid := buildPlanGrid(plan, order, directIDs, bySub, direct, grand)

	// 3 phase rows + 1 "Other" row for the loose step.
	if len(grid) != 4 {
		t.Fatalf("phased plan: got %d rows, want 4", len(grid))
	}
	if grid[0].Title != "Research" || len(grid[0].Squares) != 2 {
		t.Errorf("row 0 = %q/%d squares, want Research/2", grid[0].Title, len(grid[0].Squares))
	}
	if grid[1].Title != "Implementation" || len(grid[1].Squares) != 1 || grid[1].Squares[0].Status != "blocked" {
		t.Errorf("row 1 = %q/%d squares, want Implementation/1 blocked", grid[1].Title, len(grid[1].Squares))
	}
	if grid[2].Title != "Validation" || len(grid[2].Squares) != 0 {
		t.Errorf("row 2 (empty phase) = %q/%d squares, want Validation/0", grid[2].Title, len(grid[2].Squares))
	}
	// The trailing remainder row is labeled "Other" and holds ONLY the loose task
	// — gd-dup was claimed by the phase and must not reappear here.
	if grid[3].Title != "Other" || len(grid[3].Squares) != 1 || grid[3].Squares[0].ID != "gd-loose" {
		t.Errorf("row 3 (remainder) = %q/%v, want Other/[gd-loose]", grid[3].Title, grid[3].Squares)
	}
}

// TestBuildPlanGrid_Empty proves a plan with no rendered steps yields no grid
// (nil), so the card falls through to its "No steps yet" state.
func TestBuildPlanGrid_Empty(t *testing.T) {
	plan := &beadsapi.BeadDetail{ID: "gd-plan", Type: "epic"}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-bun": {ID: "gd-bun", Type: "bundle", Status: "open"},
	}
	if grid := buildPlanGrid(plan, nil, []string{"gd-bun"}, nil, beads, nil); grid != nil {
		t.Errorf("no-step plan: got %d rows, want nil", len(grid))
	}
}

// TestTallyPlanGrid proves the live step-status breakdown (ka-0000000851) counts
// in_progress and blocked squares across every grid row — spanning phases — and
// leaves open/closed/deferred out of the tally (those read off the progress bar).
func TestTallyPlanGrid(t *testing.T) {
	grid := []planGridPhase{
		{Title: "Research", Squares: []planGridSquare{
			{ID: "gd-r1", Status: "closed"},
			{ID: "gd-r2", Status: "in_progress"},
		}},
		{Title: "Build", Squares: []planGridSquare{
			{ID: "gd-b1", Status: "in_progress"},
			{ID: "gd-b2", Status: "blocked"},
			{ID: "gd-b3", Status: "open"},
			{ID: "gd-b4", Status: "deferred"},
		}},
		{Title: "Verify", Squares: []planGridSquare{
			{ID: "gd-v1", Status: "blocked"},
		}},
	}
	got := tallyPlanGrid(grid)
	if got.InProgress != 2 {
		t.Errorf("InProgress = %d, want 2 (across phases)", got.InProgress)
	}
	if got.Blocked != 2 {
		t.Errorf("Blocked = %d, want 2 (across phases)", got.Blocked)
	}
}

// TestTallyPlanGrid_None proves a grid with nothing in flight or wedged yields a
// zero tally, so the card renders no breakdown chips (they gate on non-zero).
func TestTallyPlanGrid_None(t *testing.T) {
	grid := []planGridPhase{
		{Squares: []planGridSquare{
			{ID: "gd-1", Status: "open"},
			{ID: "gd-2", Status: "closed"},
			{ID: "gd-3", Status: "deferred"},
		}},
	}
	got := tallyPlanGrid(grid)
	if got != (planStepTally{}) {
		t.Errorf("tally = %+v, want zero", got)
	}
}

// TestGridStepMeta proves the crew-face step-context map (ka-0000004037, iter-15)
// flattens the grid to step-id → (title, number) using the SAME per-group #N the
// squares carry — so a crew face's "#N" tag agrees with the "#N" square on the
// same card. Empty grid → nil; a blank-id square is skipped.
func TestGridStepMeta(t *testing.T) {
	if gridStepMeta(nil) != nil {
		t.Errorf("gridStepMeta(nil) must be nil (no grid → no meta)")
	}
	grid := []planGridPhase{
		{Title: "Research", Squares: []planGridSquare{
			{ID: "gd-r1", Number: 1, Title: "Scope it", Status: "closed"},
			{ID: "gd-r2", Number: 2, Title: "Design it", Status: "in_progress"},
		}},
		{Title: "Build", Squares: []planGridSquare{
			{ID: "gd-b1", Number: 1, Title: "Wire the route", Status: "in_progress"},
			{ID: "", Number: 9, Title: "phantom", Status: "open"}, // blank id skipped
		}},
	}
	meta := gridStepMeta(grid)
	if len(meta) != 3 {
		t.Fatalf("gridStepMeta size = %d, want 3 (blank-id square skipped)", len(meta))
	}
	if got := meta["gd-r2"]; got.Number != 2 || got.Title != "Design it" {
		t.Errorf("gd-r2 meta = %+v, want #2 \"Design it\"", got)
	}
	// Per-group numbering: gd-b1 is #1 in its OWN phase, matching the grid square.
	if got := meta["gd-b1"]; got.Number != 1 || got.Title != "Wire the route" {
		t.Errorf("gd-b1 meta = %+v, want #1 \"Wire the route\" (per-group #N)", got)
	}
	if _, ok := meta[""]; ok {
		t.Errorf("a blank-id square must not enter the meta map")
	}
}

// TestPlanStepsNotStarted proves the segmented progress bar's fourth lifecycle
// bucket (ka-0000007226): the not-started remainder = total − closed −
// inProgress − blocked. The key property the segmented bar leans on is that the
// four buckets PARTITION StepsTotal exactly, so the bar can never overflow its
// track. The table pins that partition plus the negative-clamp edge (a
// double-parented step inflates total past the sum of the status buckets).
func TestPlanStepsNotStarted(t *testing.T) {
	cases := []struct {
		name                               string
		total, closed, inProgress, blocked int
		want                               int
	}{
		// A well-formed plan: the four buckets partition total exactly — the
		// invariant the drift-free flex-grow bar relies on.
		{"partition_exact", 10, 4, 2, 1, 3},
		// All done: no remainder — the bar is one full green segment.
		{"all_done", 5, 5, 0, 0, 0},
		// Fresh plan, nothing started: the whole bar is the gray not-started
		// segment.
		{"all_todo", 6, 0, 0, 0, 6},
		// Fully in flight / wedged with none open: no not-started segment (the
		// bar ends on an in-progress/blocked segment, which is why the CSS rounds
		// the LAST child generically, not a pinned not-started class).
		{"no_todo_all_active", 4, 0, 3, 1, 0},
		// Degenerate double-parent: total inflated past the status-bucket sum, so
		// the raw subtraction goes negative and MUST clamp — a negative flex-grow
		// would break the bar. (renderedStepRollup counts a two-phase step twice
		// in total; each grid tally counts it once.)
		{"double_parent_clamps", 3, 2, 2, 0, 0},
		{"empty", 0, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planStepsNotStarted(tc.total, tc.closed, tc.inProgress, tc.blocked)
			if got != tc.want {
				t.Errorf("planStepsNotStarted(%d,%d,%d,%d) = %d, want %d",
					tc.total, tc.closed, tc.inProgress, tc.blocked, got, tc.want)
			}
			if got < 0 {
				t.Errorf("not-started must never be negative (would break flex-grow), got %d", got)
			}
			// For the well-formed rows, assert the partition invariant directly:
			// the four buckets sum back to total. (Skip the degenerate double-parent
			// row, whose whole point is that the raw sum exceeds total.)
			if tc.name != "double_parent_clamps" {
				if sum := tc.closed + tc.inProgress + tc.blocked + got; sum != tc.total {
					t.Errorf("buckets must partition total: %d+%d+%d+%d = %d, want %d",
						tc.closed, tc.inProgress, tc.blocked, got, sum, tc.total)
				}
			}
		})
	}
}
