package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestFilterByTeam(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-1", Title: "Alice", Fields: map[string]string{"team": "platform"}},
		{ID: "gd-2", Title: "Bob", Fields: map[string]string{"team": "frontend"}},
		{ID: "gd-3", Title: "Carol", Fields: map[string]string{"team": "Platform"}},
		{ID: "gd-4", Title: "Dana", Fields: map[string]string{"team": ""}},
	}

	result := filterByTeam(beads, "platform")
	if len(result) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(result))
	}
	if result[0].Title != "Alice" {
		t.Errorf("expected Alice, got %s", result[0].Title)
	}
	if result[1].Title != "Carol" {
		t.Errorf("expected Carol, got %s", result[1].Title)
	}
}

func TestFilterByTeam_NoMatch(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-1", Title: "Alice", Fields: map[string]string{"team": "platform"}},
	}

	result := filterByTeam(beads, "infra")
	if len(result) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(result))
	}
}

func TestFilterByTeam_Empty(t *testing.T) {
	result := filterByTeam(nil, "platform")
	if len(result) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(result))
	}
}

func TestPrintUserDetail_DoesNotPanic(t *testing.T) {
	// Verify printUserDetail handles missing fields without panicking.
	bead := &beadsapi.BeadDetail{
		ID:     "gd-test",
		Title:  "Test User",
		Status: "open",
		Fields: map[string]string{
			"slack_id":        "U123",
			"gitlab_username": "testuser",
		},
	}
	// Should not panic.
	printUserDetail(bead)
}

func TestPrintUserDetail_AllFields(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-full",
		Title:  "Full User",
		Status: "open",
		Fields: map[string]string{
			"slack_id":         "U999",
			"slack_channel_id": "D999",
			"gitlab_username":  "fulluser",
			"github_username":  "fullgh",
			"email":            "full@example.com",
			"jira_user":        "full.user",
			"pronouns":         "they/them",
			"team":             "platform",
			"capabilities":     `["admin"]`,
			"allowed_projects": `["gardenio"]`,
			"allowed_roles":    `["crew","gardener"]`,
		},
	}
	// Should not panic with all fields populated.
	printUserDetail(bead)
}
