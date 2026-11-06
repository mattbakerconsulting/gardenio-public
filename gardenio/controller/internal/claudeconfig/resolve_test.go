package claudeconfig

import "testing"

func TestProjectCompatible(t *testing.T) {
	cases := []struct {
		name    string
		labels  []string
		project string
		want    bool
	}{
		{"no labels", nil, "gardenio", true},
		{"matching single", []string{"project:gardenio"}, "gardenio", true},
		{"non-matching single", []string{"project:gardenio"}, "gascity", false},
		{"empty project means all", []string{"project:gardenio"}, "", true},
		{"group prefix stripped", []string{"g1:project:gardenio"}, "gardenio", true},
		// MatchesSubscriptions requires EVERY project: label to be subscribed
		// (internal/advice/matching.go:218-220). This mirrored helper was OR,
		// so a two-project bead resolved for a viewer in only one of them.
		{"two projects, only one matches", []string{"project:gardenio", "project:gascity"}, "gardenio", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectCompatible(tc.labels, tc.project); got != tc.want {
				t.Errorf("projectCompatible(%v, %q) = %v, want %v", tc.labels, tc.project, got, tc.want)
			}
		})
	}
}
