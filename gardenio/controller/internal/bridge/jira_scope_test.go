package bridge

import "testing"

// TestJiraWriteInScope proves the bead→ticket scoping predicate: a write is in
// scope only when the bound key and target key are both non-empty and name the
// same issue (case-insensitive). Everything else fails closed (ka-0000001679).
func TestJiraWriteInScope(t *testing.T) {
	cases := []struct {
		name   string
		bound  string
		target string
		want   bool
	}{
		{"exact match", "PE-1234", "PE-1234", true},
		{"case-insensitive match", "pe-1234", "PE-1234", true},
		{"whitespace trimmed", "  PE-1234 ", "PE-1234", true},
		{"different ticket", "PE-1234", "PE-9999", false},
		{"different project", "PE-1234", "DEVOPS-1234", false},
		{"FSM target while bound to PE", "PE-1", "FSM-1", false},
		{"empty bound (unbound bead)", "", "PE-1234", false},
		{"empty target", "PE-1234", "", false},
		{"both empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jiraWriteInScope(c.bound, c.target); got != c.want {
				t.Errorf("jiraWriteInScope(%q, %q) = %v, want %v", c.bound, c.target, got, c.want)
			}
		})
	}
}

// TestBoundJiraKeyFromFields proves binding extraction prefers the jira_key
// field, falls back to a jira:<KEY> label, and ignores jira-label:<x> (imported
// Jira labels, not the issue key).
func TestBoundJiraKeyFromFields(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		labels []string
		want   string
	}{
		{
			name:   "field wins",
			fields: map[string]string{"jira_key": "PE-1"},
			labels: []string{"jira:PE-2"},
			want:   "PE-1",
		},
		{
			name:   "label fallback",
			fields: map[string]string{},
			labels: []string{"source:jira", "jira:PE-42"},
			want:   "PE-42",
		},
		{
			name:   "jira-label ignored",
			fields: nil,
			labels: []string{"jira-label:approved", "jira-label:urgent"},
			want:   "",
		},
		{
			name:   "no binding",
			fields: map[string]string{"role": "crew"},
			labels: []string{"project:gardenio"},
			want:   "",
		},
		{
			name:   "field with whitespace trimmed",
			fields: map[string]string{"jira_key": "  DEVOPS-7 "},
			want:   "DEVOPS-7",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := boundJiraKeyFromFields(c.fields, c.labels); got != c.want {
				t.Errorf("boundJiraKeyFromFields(%v, %v) = %q, want %q",
					c.fields, c.labels, got, c.want)
			}
		})
	}
}
