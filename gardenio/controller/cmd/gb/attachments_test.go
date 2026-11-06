package main

import "testing"

func TestLooksLikeJiraKey(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"PE-7001", true},
		{"DEVOPS-42", true},
		{"A-1", true},
		{"pe-7001", false},  // lowercase
		{"PE7001", false},   // no dash
		{"PE-", false},      // no number
		{"-7001", false},    // no prefix
		{"bd-task-1", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := attJiraKeyRe.MatchString(tt.input)
			if got != tt.want {
				t.Errorf("attJiraKeyRe.MatchString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatSize(tt.input)
			if got != tt.want {
				t.Errorf("formatSize(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
