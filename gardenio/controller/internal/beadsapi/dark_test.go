package beadsapi

import "testing"

func TestIsDark(t *testing.T) {
	cases := []struct {
		name    string
		labels  []string
		project string
		want    bool
	}{
		{"bare monorepo-dark label", []string{"role:crew", "monorepo-dark"}, "gardenio", true},
		{"dark:true label", []string{"dark:true"}, "gardenio", true},
		{"project:monorepo-dark label", []string{"project:monorepo-dark"}, "gardenio", true},
		{"dark project field", nil, "monorepo-dark", true},
		{"dark project field padded", nil, " monorepo-dark ", true},
		{"plain agent", []string{"role:crew", "project:gardenio"}, "gardenio", false},
		{"no signals", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDark(tc.labels, tc.project); got != tc.want {
				t.Errorf("IsDark(%v, %q) = %v, want %v", tc.labels, tc.project, got, tc.want)
			}
		})
	}
}
