package bridge

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestBeadIsPlan(t *testing.T) {
	tests := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{"epic with plan label", &beadsapi.BeadDetail{Type: "epic", Labels: []string{"project:gardenio", "plan"}}, true},
		{"epic without plan label", &beadsapi.BeadDetail{Type: "epic", Labels: []string{"project:gardenio"}}, false},
		{"task with plan label", &beadsapi.BeadDetail{Type: "task", Labels: []string{"plan"}}, false},
		{"bare epic", &beadsapi.BeadDetail{Type: "epic"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := beadIsPlan(tt.bead); got != tt.want {
				t.Errorf("beadIsPlan = %v, want %v", got, tt.want)
			}
		})
	}
}
