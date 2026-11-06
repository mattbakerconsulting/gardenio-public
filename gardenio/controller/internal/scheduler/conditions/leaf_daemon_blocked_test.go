package conditions

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stubBlockedResolver extends stubBeadClient with the batch dep + bead lookups
// the exclude_blocked_by_open_types filter needs. It exercises the
// externally-blocked exclusion path in evalBeadQuery (ka-0000002198).
//
//   - rows is what ListBeadsFiltered returns (Beads + Total). Total defaults to
//     len(rows) unless totalOverride is set (to simulate a truncated scan).
//   - depsByChild maps child-id → its FORWARD deps.
//   - beadsByID maps blocker-id → its detail (type + status).
//
// depErr / beadErr force the respective batch call to fail so the fail-safe
// direction can be asserted.
type stubBlockedResolver struct {
	rows          []*beadsapi.BeadDetail
	totalOverride int
	depsByChild   map[string][]beadsapi.Dependency
	beadsByID     map[string]*beadsapi.BeadDetail
	depErr        error
	beadErr       error

	depCalls  int
	beadCalls int
}

func (s *stubBlockedResolver) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	total := s.totalOverride
	if total == 0 {
		total = len(s.rows)
	}
	return &beadsapi.ListBeadsResult{Beads: s.rows, Total: total}, nil
}

func (s *stubBlockedResolver) BatchGetDependencies(_ context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	s.depCalls++
	if s.depErr != nil {
		return nil, s.depErr
	}
	if direction != "forward" {
		return nil, errors.New("unexpected direction " + direction)
	}
	out := make(map[string][]beadsapi.Dependency, len(ids))
	for _, id := range ids {
		out[id] = s.depsByChild[id]
	}
	return out, nil
}

func (s *stubBlockedResolver) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	s.beadCalls++
	if s.beadErr != nil {
		return nil, s.beadErr
	}
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if b, ok := s.beadsByID[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

func row(id string) *beadsapi.BeadDetail { return &beadsapi.BeadDetail{ID: id} }

func blocksDep(child, blocker string) beadsapi.Dependency {
	return beadsapi.Dependency{BeadID: child, DependsOnID: blocker, Type: "blocks"}
}

// TestEvaluate_BeadQuery_ExcludeBlockedByOpenTypes reproduces the gardener
// loop_end bug (ka-0000002198): an epic's only open child is legitimately
// blocked on an open operator escalation. Without the filter the loop_end
// bead_query (==0) never fires and the gardener loops forever, scoring
// STALE-STOP. With exclude_blocked_by_open_types:["escalation"] the blocked
// child is subtracted, the effective count is 0, and loop_end fires.
func TestEvaluate_BeadQuery_ExcludeBlockedByOpenTypes(t *testing.T) {
	const (
		child = "gd-H6Jodnv2Ar"
		esc   = "ka-0000001499"
		epic  = "gd-sd7Hxka19a"
	)

	// loop_end: "close when the subtree has 0 open task/bug/epic children",
	// now opting into the externally-blocked exclusion.
	loopEnd := Leaf{
		Type:                      "bead_query",
		Op:                        "==",
		Value:                     "0",
		Parent:                    epic,
		Types:                     []string{"task", "bug", "epic"},
		Statuses:                  []string{"open", "in_progress", "blocked"},
		ExcludeBlockedByOpenTypes: []string{"escalation", "decision"},
	}

	t.Run("open child blocked by open escalation is excluded → loop_end fires", func(t *testing.T) {
		bc := &stubBlockedResolver{
			rows:        []*beadsapi.BeadDetail{row(child)},
			depsByChild: map[string][]beadsapi.Dependency{child: {blocksDep(child, esc)}},
			beadsByID:   map[string]*beadsapi.BeadDetail{esc: {ID: esc, Type: "escalation", Status: "open"}},
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !got {
			t.Fatal("expected loop_end MET (effective count 0) when the only open child is blocked by an open escalation")
		}
		if bc.depCalls != 1 || bc.beadCalls != 1 {
			t.Errorf("expected one dep-batch + one bead-batch call, got dep=%d bead=%d", bc.depCalls, bc.beadCalls)
		}
	})

	t.Run("child blocked by CLOSED escalation still counts → loop_end does NOT fire", func(t *testing.T) {
		bc := &stubBlockedResolver{
			rows:        []*beadsapi.BeadDetail{row(child)},
			depsByChild: map[string][]beadsapi.Dependency{child: {blocksDep(child, esc)}},
			beadsByID:   map[string]*beadsapi.BeadDetail{esc: {ID: esc, Type: "escalation", Status: "closed"}},
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected loop_end NOT met: a closed blocker is not a live wait, so the child is unblocked work")
		}
	})

	t.Run("child blocked by a NON-configured open type still counts", func(t *testing.T) {
		// Blocked by an ordinary open task, not an escalation/decision — this
		// is real work-in-progress, must NOT be excluded.
		const blockerTask = "gd-OtherTask"
		bc := &stubBlockedResolver{
			rows:        []*beadsapi.BeadDetail{row(child)},
			depsByChild: map[string][]beadsapi.Dependency{child: {blocksDep(child, blockerTask)}},
			beadsByID:   map[string]*beadsapi.BeadDetail{blockerTask: {ID: blockerTask, Type: "task", Status: "open"}},
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected loop_end NOT met: a task-typed blocker is not in the configured exclusion set")
		}
	})

	t.Run("mixed: one blocked child excluded, one live child keeps the loop open", func(t *testing.T) {
		const live = "gd-LiveWork"
		bc := &stubBlockedResolver{
			rows: []*beadsapi.BeadDetail{row(child), row(live)},
			depsByChild: map[string][]beadsapi.Dependency{
				child: {blocksDep(child, esc)},
				// live has no blocking deps.
			},
			beadsByID: map[string]*beadsapi.BeadDetail{esc: {ID: esc, Type: "escalation", Status: "open"}},
		}
		// Effective count = 2 - 1 = 1, so "==0" is NOT met (loop continues).
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected loop_end NOT met while one live (unblocked) child remains")
		}
	})

	t.Run("empty exclusion set keeps legacy count-only (no dep fetch)", func(t *testing.T) {
		legacy := loopEnd
		legacy.ExcludeBlockedByOpenTypes = nil
		bc := &stubBlockedResolver{
			rows:        []*beadsapi.BeadDetail{row(child)},
			depsByChild: map[string][]beadsapi.Dependency{child: {blocksDep(child, esc)}},
			beadsByID:   map[string]*beadsapi.BeadDetail{esc: {ID: esc, Type: "escalation", Status: "open"}},
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: legacy}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected legacy behaviour: count 1, ==0 unmet")
		}
		if bc.depCalls != 0 || bc.beadCalls != 0 {
			t.Errorf("expected NO batch calls without the filter, got dep=%d bead=%d", bc.depCalls, bc.beadCalls)
		}
	})

	t.Run("client without batch capability keeps full count (fail-safe)", func(t *testing.T) {
		// stubBeadClient does NOT implement blockedByResolver → the exclusion
		// is skipped and the count stays at 1 (loop continues).
		bc := &stubBeadClient{fn: count(1)}
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected fail-safe full count (loop_end NOT met) when client lacks batch capability")
		}
	})

	t.Run("dep-fetch error keeps full count (fail-safe)", func(t *testing.T) {
		bc := &stubBlockedResolver{
			rows:   []*beadsapi.BeadDetail{row(child)},
			depErr: errors.New("daemon down"),
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected fail-safe full count on dep-fetch error")
		}
	})

	t.Run("blocker-fetch error keeps full count (fail-safe)", func(t *testing.T) {
		bc := &stubBlockedResolver{
			rows:        []*beadsapi.BeadDetail{row(child)},
			depsByChild: map[string][]beadsapi.Dependency{child: {blocksDep(child, esc)}},
			beadErr:     errors.New("daemon down"),
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected fail-safe full count on blocker-fetch error")
		}
	})

	t.Run("truncated scan (total > scanned) keeps full count (fail-safe)", func(t *testing.T) {
		// Daemon reports 600 matches but only 500 rows fit the scan cap: we
		// cannot know whether the un-scanned rows are blocked, so exclude none.
		bc := &stubBlockedResolver{
			rows:          []*beadsapi.BeadDetail{row(child)},
			totalOverride: beadQueryBlockedScanCap + 100,
			depsByChild:   map[string][]beadsapi.Dependency{child: {blocksDep(child, esc)}},
			beadsByID:     map[string]*beadsapi.BeadDetail{esc: {ID: esc, Type: "escalation", Status: "open"}},
		}
		// The gate is ">0" here (fire while there's open work); truncated scan
		// must keep the full (large) count so the gardener does not over-stop.
		leaf := loopEnd
		leaf.Op = ">"
		leaf.Value = "0"
		got, err := Evaluate(context.Background(), Expression{Leaf: leaf}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !got {
			t.Fatal("expected >0 MET (full count kept) when the scan was truncated")
		}
		if bc.depCalls != 0 {
			t.Errorf("expected no dep fetch on truncated scan, got %d", bc.depCalls)
		}
	})

	t.Run("closed root epic still wins over the exclusion filter", func(t *testing.T) {
		// A client that is BOTH a blockedByResolver and a beadStatusClient:
		// the closed-root short-circuit forces count 0 regardless, and
		// loop_end (==0) fires terminally.
		bc := &stubBlockedStatusResolver{
			stubBlockedResolver: stubBlockedResolver{
				rows:        []*beadsapi.BeadDetail{row(child)},
				depsByChild: map[string][]beadsapi.Dependency{child: nil}, // unblocked
			},
			statuses: map[string]string{epic: "closed"},
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !got {
			t.Fatal("expected loop_end MET (closed root forces count 0)")
		}
	})
}

// stubBlockedStatusResolver combines both optional capabilities so the
// closed-root short-circuit and the externally-blocked filter can be exercised
// together.
type stubBlockedStatusResolver struct {
	stubBlockedResolver
	statuses map[string]string
}

func (s *stubBlockedStatusResolver) GetBeadStatus(_ context.Context, beadID string) (string, error) {
	return s.statuses[beadID], nil
}
