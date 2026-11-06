package main

import (
	"testing"
)

func TestIsS3URI(t *testing.T) {
	cases := map[string]bool{
		"s3://bucket/key":            true,
		"s3://b/coop/sessions/kd/1":  true,
		"/local/path/log.jsonl":      false,
		"log.jsonl":                  false,
		"":                           false,
		"http://example.com/x.jsonl": false,
		"s3:/":                       false, // too short — not a valid s3:// prefix
	}
	for in, want := range cases {
		if got := isS3URI(in); got != want {
			t.Errorf("isS3URI(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDeathReportAgentName(t *testing.T) {
	// The explicit `agent` field wins over assignee and title.
	if got := deathReportAgentName(map[string]string{"agent": "crew-x"}, "assignee-y", "title-z"); got != "crew-x" {
		t.Errorf("agent field should win, got %q", got)
	}
	// Falls back to assignee when no agent field.
	if got := deathReportAgentName(map[string]string{}, "assignee-y", "title-z"); got != "assignee-y" {
		t.Errorf("assignee fallback failed, got %q", got)
	}
	// Falls back to title when neither agent field nor assignee.
	if got := deathReportAgentName(nil, "", "title-z"); got != "title-z" {
		t.Errorf("title fallback failed, got %q", got)
	}
}
