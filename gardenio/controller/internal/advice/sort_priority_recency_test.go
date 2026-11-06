package advice

import (
	"reflect"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mkMatched builds a MatchedAdvice wrapping a bead with the given ordering axes.
func mkMatched(id string, priority int, updated, created time.Time) MatchedAdvice {
	return MatchedAdvice{
		Bead: &beadsapi.BeadDetail{
			ID:        id,
			Priority:  priority,
			UpdatedAt: updated,
			CreatedAt: created,
		},
	}
}

func ids(items []MatchedAdvice) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Bead.ID
	}
	return out
}

// TestSortByPriorityRecency_PriorityDominates asserts priority (ascending
// numeric = P0 critical first) outranks recency: a stale P0 sorts ahead of a
// fresh P4.
func TestSortByPriorityRecency_PriorityDominates(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)

	items := []MatchedAdvice{
		mkMatched("gd-freshP4", 4, fresh, fresh),
		mkMatched("gd-staleP0", 0, old, old),
	}
	SortByPriorityRecency(items)

	if got := ids(items); got[0] != "gd-staleP0" {
		t.Errorf("priority must dominate recency: got %v, want gd-staleP0 first", got)
	}
}

// TestSortByPriorityRecency_RecencyBreaksPriorityTie asserts that within equal
// priority the most recently UPDATED bead sorts first, and CreatedAt breaks a
// further UpdatedAt tie.
func TestSortByPriorityRecency_RecencyBreaksPriorityTie(t *testing.T) {
	t1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	items := []MatchedAdvice{
		mkMatched("gd-old", 2, t1, t1),
		mkMatched("gd-new", 2, t3, t1),
		mkMatched("gd-mid", 2, t2, t1),
	}
	SortByPriorityRecency(items)

	want := []string{"gd-new", "gd-mid", "gd-old"}
	if got := ids(items); !reflect.DeepEqual(got, want) {
		t.Errorf("recency ordering: got %v, want %v", got, want)
	}

	// UpdatedAt tie → CreatedAt breaks it (more recently created first).
	c1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	tie := []MatchedAdvice{
		mkMatched("gd-createdEarly", 2, t3, c1),
		mkMatched("gd-createdLate", 2, t3, c2),
	}
	SortByPriorityRecency(tie)
	if got := ids(tie); got[0] != "gd-createdLate" {
		t.Errorf("CreatedAt tiebreak: got %v, want gd-createdLate first", got)
	}
}

// TestSortByPriorityRecency_IDBreaksFullTie asserts the bead ID is the final
// total-order tiebreak when priority and both timestamps are identical.
func TestSortByPriorityRecency_IDBreaksFullTie(t *testing.T) {
	ts := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	items := []MatchedAdvice{
		mkMatched("gd-ccc", 2, ts, ts),
		mkMatched("gd-aaa", 2, ts, ts),
		mkMatched("gd-bbb", 2, ts, ts),
	}
	SortByPriorityRecency(items)

	want := []string{"gd-aaa", "gd-bbb", "gd-ccc"}
	if got := ids(items); !reflect.DeepEqual(got, want) {
		t.Errorf("ID tiebreak: got %v, want %v", got, want)
	}
}

// TestSortByPriorityRecency_Deterministic asserts an identical corpus always
// yields the identical order regardless of the arbitrary input order it
// arrived in (the F1 determinism contract). Two different input permutations
// of the same set must produce byte-identical output.
func TestSortByPriorityRecency_Deterministic(t *testing.T) {
	t1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	base := []MatchedAdvice{
		mkMatched("gd-a", 0, t2, t2),
		mkMatched("gd-b", 2, t2, t2),
		mkMatched("gd-c", 2, t1, t1),
		mkMatched("gd-d", 4, t2, t2),
		mkMatched("gd-e", 2, t2, t1), // same priority+updated as gd-b, older created
	}

	// forward
	fwd := append([]MatchedAdvice(nil), base...)
	SortByPriorityRecency(fwd)

	// reversed input — must sort to the same result
	rev := make([]MatchedAdvice, len(base))
	for i := range base {
		rev[len(base)-1-i] = base[i]
	}
	SortByPriorityRecency(rev)

	if !reflect.DeepEqual(ids(fwd), ids(rev)) {
		t.Errorf("non-deterministic: forward=%v reversed=%v", ids(fwd), ids(rev))
	}

	// Explicit expected order: P0 first (gd-a); then the three P2s ordered by
	// updated desc then created desc (gd-b and gd-e share updated=t2, gd-b
	// created=t2 > gd-e created=t1, then gd-c updated=t1); then P4 (gd-d).
	want := []string{"gd-a", "gd-b", "gd-e", "gd-c", "gd-d"}
	if got := ids(fwd); !reflect.DeepEqual(got, want) {
		t.Errorf("order: got %v, want %v", got, want)
	}
}
