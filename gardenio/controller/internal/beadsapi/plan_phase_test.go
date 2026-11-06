package beadsapi

import "testing"

func TestIsPhaseContainer(t *testing.T) {
	cases := []struct {
		name string
		bead *BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{"epic", &BeadDetail{Type: "epic"}, true},         // hand-authored phase
		{"molecule", &BeadDetail{Type: "molecule"}, true}, // poured phase (ka-0000000759)
		{"task", &BeadDetail{Type: "task"}, false},
		{"bug", &BeadDetail{Type: "bug"}, false},
		{"feature", &BeadDetail{Type: "feature"}, false},
		{"bundle", &BeadDetail{Type: "bundle"}, false},
		{"empty-type", &BeadDetail{Type: ""}, false},
	}
	for _, tc := range cases {
		if got := IsPhaseContainer(tc.bead); got != tc.want {
			t.Errorf("IsPhaseContainer(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
