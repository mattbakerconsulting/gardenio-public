package server

import (
	"testing"
)

// TestCreateBead_ReturnsPersistedLabels is the R2 (ka-0000002898) guarantee: the
// bead createBead returns carries the labels read back FROM the store within the
// create transaction, not the raw input echo. A caller (the /plans
// invisible-orphan guard) can therefore trust the create return value directly,
// with no separate GetBead round-trip.
func TestCreateBead_ReturnsPersistedLabels(t *testing.T) {
	s, ms, _ := newTestServer()

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "plan epic",
		Type:   "epic",
		Labels: []string{"plan", "project:gardenio"},
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	// The returned labels are exactly what landed in the labels table.
	if got := ms.labels[bead.ID]; !equalStringSet(bead.Labels, got) {
		t.Errorf("returned labels = %v, want the persisted set %v", bead.Labels, got)
	}
	if !containsStr(bead.Labels, "plan") || !containsStr(bead.Labels, "project:gardenio") {
		t.Errorf("returned labels = %v, want to include plan + project:gardenio", bead.Labels)
	}
}

// TestCreateBead_ReturnedLabelsDeduped proves the returned set reflects the
// committed rows, not the input: duplicate input labels collapse via the
// AddLabel ON CONFLICT (mirrored by the mock), and the returned bead shows the
// deduped set a later read would see — the whole point of folding the readback
// into the transaction.
func TestCreateBead_ReturnedLabelsDeduped(t *testing.T) {
	s, _, _ := newTestServer()

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "dupe labels",
		Type:   "task",
		Labels: []string{"project:gardenio", "project:gardenio", "urgent"},
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	seen := map[string]int{}
	for _, l := range bead.Labels {
		seen[l]++
	}
	if seen["project:gardenio"] != 1 {
		t.Errorf("returned labels = %v, want project:gardenio exactly once (deduped readback)", bead.Labels)
	}
	if seen["urgent"] != 1 {
		t.Errorf("returned labels = %v, want urgent present exactly once", bead.Labels)
	}
}

func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
