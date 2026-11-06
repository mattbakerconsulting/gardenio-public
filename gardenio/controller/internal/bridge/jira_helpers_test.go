package bridge

import (
	"testing"
)

func TestJiraKeyFromBead(t *testing.T) {
	tests := []struct {
		name     string
		bead     BeadEvent
		expected string
	}{
		{"from fields", BeadEvent{Fields: map[string]string{"jira_key": "PE-123"}}, "PE-123"},
		{"from labels", BeadEvent{Labels: []string{"source:jira", "jira:DEVOPS-42"}, Fields: map[string]string{}}, "DEVOPS-42"},
		{"not jira", BeadEvent{Labels: []string{"source:manual"}, Fields: map[string]string{}}, ""},
		{"jira-label no match", BeadEvent{Labels: []string{"jira-label:frontend"}, Fields: map[string]string{}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jiraKeyFromBead(tt.bead); got != tt.expected {
				t.Errorf("jiraKeyFromBead() = %q, want %q", got, tt.expected)
			}
		})
	}
}
