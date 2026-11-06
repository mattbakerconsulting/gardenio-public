package main

import "testing"

func TestMatchesSubscriptions(t *testing.T) {
	tests := []struct {
		name     string
		labels   []string
		subs     map[string]bool
		expected bool
	}{
		{
			name:     "global matches global",
			labels:   []string{"global"},
			subs:     map[string]bool{"global": true},
			expected: true,
		},
		{
			name:     "role matches role",
			labels:   []string{"role:reviewer"},
			subs:     map[string]bool{"global": true, "role:reviewer": true},
			expected: true,
		},
		{
			name:     "role does not match different role",
			labels:   []string{"role:reviewer"},
			subs:     map[string]bool{"global": true, "role:crew": true},
			expected: false,
		},
		{
			name:     "project required but missing",
			labels:   []string{"project:monorepo", "global"},
			subs:     map[string]bool{"global": true, "project:gardenio": true},
			expected: false,
		},
		{
			name:     "project matches",
			labels:   []string{"project:gardenio", "global"},
			subs:     map[string]bool{"global": true, "project:gardenio": true},
			expected: true,
		},
		{
			name:     "grouped labels AND within group",
			labels:   []string{"g0:project:gardenio", "g0:role:reviewer"},
			subs:     map[string]bool{"global": true, "project:gardenio": true, "role:reviewer": true},
			expected: true,
		},
		{
			name:     "grouped labels AND fails when partial",
			labels:   []string{"g0:project:gardenio", "g0:role:reviewer"},
			subs:     map[string]bool{"global": true, "project:gardenio": true},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesSubscriptions(tt.labels, tt.subs)
			if got != tt.expected {
				t.Errorf("matchesSubscriptions(%v, %v) = %v, want %v", tt.labels, tt.subs, got, tt.expected)
			}
		})
	}
}

func TestStripGroupPrefix(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"global", "global"},
		{"g0:role:reviewer", "role:reviewer"},
		{"g12:project:gardenio", "project:gardenio"},
		{"gardenio", "gardenio"}, // not a group prefix
	}

	for _, tt := range tests {
		got := stripGroupPrefix(tt.input)
		if got != tt.expected {
			t.Errorf("stripGroupPrefix(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestIsTargetingLabel(t *testing.T) {
	tests := []struct {
		label    string
		expected bool
	}{
		{"global", true},
		{"project:gardenio", true},
		{"role:reviewer", true},
		{"agent:foo", true},
		{"slack-thread", false},
		{"topic:config", false},
	}

	for _, tt := range tests {
		got := isTargetingLabel(tt.label)
		if got != tt.expected {
			t.Errorf("isTargetingLabel(%q) = %v, want %v", tt.label, got, tt.expected)
		}
	}
}

func TestCategorizeScope(t *testing.T) {
	tests := []struct {
		labels        []string
		expectedScope string
		expectedTgt   string
	}{
		{[]string{"global"}, "global", ""},
		{[]string{"project:gardenio"}, "project", "gardenio"},
		{[]string{"role:reviewer"}, "role", "reviewer"},
		{[]string{"agent:foo"}, "agent", "foo"},
		{[]string{"project:gardenio", "role:reviewer"}, "role", "reviewer"},
	}

	for _, tt := range tests {
		scope, target := categorizeScope(tt.labels)
		if scope != tt.expectedScope || target != tt.expectedTgt {
			t.Errorf("categorizeScope(%v) = (%q, %q), want (%q, %q)",
				tt.labels, scope, target, tt.expectedScope, tt.expectedTgt)
		}
	}
}

func TestSingularize(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"threads", "thread"},
		{"crew", "crew"},
		{"reviewers", "reviewer"},
	}

	for _, tt := range tests {
		got := singularize(tt.input)
		if got != tt.expected {
			t.Errorf("singularize(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
