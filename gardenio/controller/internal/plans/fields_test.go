package plans

import "testing"

// TestReorderStep is the step-level analogue of TestReorderPhase: a listed step
// swaps one slot; boundaries and absent ids are no-ops; the input is never
// mutated.
func TestReorderStep(t *testing.T) {
	order := []string{"a", "b", "c"}
	if got, ok := ReorderStep(order, "b", "up"); !ok || got[0] != "b" || got[1] != "a" {
		t.Errorf("move b up = %v (ok=%v), want [b a c]", got, ok)
	}
	if got, ok := ReorderStep(order, "b", "down"); !ok || got[1] != "c" || got[2] != "b" {
		t.Errorf("move b down = %v (ok=%v), want [a c b]", got, ok)
	}
	if _, ok := ReorderStep(order, "a", "up"); ok {
		t.Error("moving first up should be a no-op")
	}
	if _, ok := ReorderStep(order, "c", "down"); ok {
		t.Error("moving last down should be a no-op")
	}
	if _, ok := ReorderStep(order, "z", "up"); ok {
		t.Error("moving an absent id should be a no-op")
	}
	if order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Errorf("ReorderStep mutated its input: %v", order)
	}
}

func TestReorderPhase(t *testing.T) {
	order := []string{"a", "b", "c"}
	if got, ok := ReorderPhaseResolved(order, order, "b", "up"); !ok || got[0] != "b" || got[1] != "a" {
		t.Errorf("move b up = %v (ok=%v), want [b a c]", got, ok)
	}
	if got, ok := ReorderPhaseResolved(order, order, "b", "down"); !ok || got[1] != "c" || got[2] != "b" {
		t.Errorf("move b down = %v (ok=%v), want [a c b]", got, ok)
	}
	// Boundaries and missing ids are no-ops.
	if _, ok := ReorderPhaseResolved(order, order, "a", "up"); ok {
		t.Error("moving first up should be a no-op")
	}
	if _, ok := ReorderPhaseResolved(order, order, "c", "down"); ok {
		t.Error("moving last down should be a no-op")
	}
	if _, ok := ReorderPhaseResolved(order, order, "z", "up"); ok {
		t.Error("moving an absent id should be a no-op")
	}
	// The input slice must not be mutated.
	if order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Errorf("ReorderPhaseResolved mutated its input: %v", order)
	}
}

// TestReorderPhaseResolved_InterleavedStale is the ka-0000002075 regression at the
// pure-helper level: when a stale id sits BETWEEN two rendered phases in the raw
// phase_order, the swap must run over the RESOLVED (rendered) subset and splice
// back into the raw array preserving the stale id in place — a single move must
// actually transpose the two rendered phases, not silently absorb into the stale
// slot (the old raw-neighbor swap made the first click a visible no-op).
func TestReorderPhaseResolved_InterleavedStale(t *testing.T) {
	// Raw order: A, <stale>, B. Rendered subset: A, B.
	order := []string{"gd-A", "gd-stale", "gd-B"}
	resolved := []string{"gd-A", "gd-B"}

	// Move A DOWN: rendered [A,B] → [B,A]. Spliced back preserving the stale id at
	// its original index 1 → [B, stale, A]. Pre-fix (raw-neighbor swap) this would
	// have produced [stale, A, B], which re-renders as [A, B] — a no-op.
	got, ok := ReorderPhaseResolved(order, resolved, "gd-A", "down")
	if !ok {
		t.Fatal("moving A down should succeed")
	}
	if len(got) != 3 || got[0] != "gd-B" || got[1] != "gd-stale" || got[2] != "gd-A" {
		t.Errorf("A down = %v, want [gd-B gd-stale gd-A] (stale pinned at index 1)", got)
	}

	// Symmetric: move B UP is the same transposition from the other phase.
	got, ok = ReorderPhaseResolved(order, resolved, "gd-B", "up")
	if !ok {
		t.Fatal("moving B up should succeed")
	}
	if len(got) != 3 || got[0] != "gd-B" || got[1] != "gd-stale" || got[2] != "gd-A" {
		t.Errorf("B up = %v, want [gd-B gd-stale gd-A]", got)
	}

	// Resolved boundaries are no-ops even though a stale id remains in the raw
	// array beyond the rendered phase — the arrows would not be shown.
	if _, ok := ReorderPhaseResolved(order, resolved, "gd-A", "up"); ok {
		t.Error("moving the first rendered phase up should be a no-op")
	}
	if _, ok := ReorderPhaseResolved(order, resolved, "gd-B", "down"); ok {
		t.Error("moving the last rendered phase down should be a no-op")
	}
	// An id that is not a rendered phase (the stale id itself, or an unknown id)
	// is a no-op.
	if _, ok := ReorderPhaseResolved(order, resolved, "gd-stale", "up"); ok {
		t.Error("reordering a non-rendered (stale) id should be a no-op")
	}
	if _, ok := ReorderPhaseResolved(order, resolved, "gd-zzz", "down"); ok {
		t.Error("reordering an absent id should be a no-op")
	}
	// The input slice must not be mutated.
	if order[0] != "gd-A" || order[1] != "gd-stale" || order[2] != "gd-B" {
		t.Errorf("ReorderPhaseResolved mutated its input: %v", order)
	}

	// Defensive: a duplicate id in the raw array swaps its FIRST occurrence (the
	// one resolvedPhaseIDs numbered), matching the rendered #N.
	dupOrder := []string{"gd-A", "gd-B", "gd-A"} // second gd-A is a dedup dropout
	dupResolved := []string{"gd-A", "gd-B"}
	got, ok = ReorderPhaseResolved(dupOrder, dupResolved, "gd-A", "down")
	if !ok || len(got) != 3 || got[0] != "gd-B" || got[1] != "gd-A" || got[2] != "gd-A" {
		t.Errorf("dup A down = %v (ok=%v), want [gd-B gd-A gd-A] (first occurrence swapped)", got, ok)
	}
}
