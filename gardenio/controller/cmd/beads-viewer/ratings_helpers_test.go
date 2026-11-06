package main

import (
	"html/template"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestRenderStars(t *testing.T) {
	cases := []struct {
		score int
		want  string
	}{
		{0, "☆☆☆☆☆"},
		{1, "★☆☆☆☆"},
		{3, "★★★☆☆"},
		{5, "★★★★★"},
		{-1, "☆☆☆☆☆"},
		{99, "★★★★★"},
	}
	for _, tc := range cases {
		got := renderStars(tc.score)
		if got != tc.want {
			t.Errorf("renderStars(%d) = %q, want %q", tc.score, got, tc.want)
		}
	}
}

func TestParseRatingTags(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"a, b, c", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
		{" leading ,trailing ", []string{"leading", "trailing"}},
	}
	for _, tc := range cases {
		got := parseRatingTags(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("parseRatingTags(%q) = %v, want %v", tc.raw, got, tc.want)
			continue
		}
		for i, v := range got {
			if v != tc.want[i] {
				t.Errorf("parseRatingTags(%q)[%d] = %q, want %q", tc.raw, i, v, tc.want[i])
			}
		}
	}
}

func TestAggregateRatings(t *testing.T) {
	scores := []string{"5", "5", "4", "3", "1"}
	beads := make([]*beadsapi.BeadDetail, len(scores))
	for i, s := range scores {
		beads[i] = &beadsapi.BeadDetail{
			ID:     "gd-r" + s,
			Type:   "rating",
			Fields: map[string]string{"score": s},
		}
	}
	agg := aggregateRatings(beads)
	if agg.Count != 5 {
		t.Errorf("Count = %d, want 5", agg.Count)
	}
	if agg.Mean < 3.5 || agg.Mean > 3.7 {
		t.Errorf("Mean = %f, want ~3.6", agg.Mean)
	}
	if agg.MeanLabel != "3.6" {
		t.Errorf("MeanLabel = %q, want 3.6", agg.MeanLabel)
	}
	if agg.Histogram[4] != 2 { // two 5★
		t.Errorf("Histogram[5*] = %d, want 2", agg.Histogram[4])
	}
	if agg.Histogram[0] != 1 { // one 1★
		t.Errorf("Histogram[1*] = %d, want 1", agg.Histogram[0])
	}
}

func TestAggregateRatings_Empty(t *testing.T) {
	agg := aggregateRatings(nil)
	if agg.Count != 0 {
		t.Errorf("Count = %d, want 0", agg.Count)
	}
	if agg.MeanLabel != "—" {
		t.Errorf("MeanLabel = %q, want em-dash", agg.MeanLabel)
	}
}

func TestRatingSourceLabel(t *testing.T) {
	cases := map[string]string{
		"":                     "web",
		"   ":                  "web",
		"web":                  "web",
		"slack-stop":           "slack-stop",
		"slack-feedback-only":  "slack-feedback-only",
	}
	for in, want := range cases {
		if got := ratingSourceLabel(in); got != want {
			t.Errorf("ratingSourceLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildSiblingViews(t *testing.T) {
	render := func(s string) template.HTML { return template.HTML(s) }
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	siblings := []*beadsapi.BeadDetail{
		{ID: "gd-self", Type: "rating", Fields: map[string]string{"score": "5", "rated_bead_id": "gd-target"}, CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "gd-old", Type: "rating", Fields: map[string]string{"score": "3", "rated_bead_id": "gd-target"}, CreatedAt: now.Add(-72 * time.Hour)},
		{ID: "gd-new", Type: "rating", Fields: map[string]string{"score": "4", "rated_bead_id": "gd-target"}, CreatedAt: now.Add(-1 * time.Minute)},
		nil, // skipped
	}
	got := buildSiblingViews(siblings, "gd-self", render, now, 5)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (self + nil dropped); got %+v", len(got), got)
	}
	if got[0].BeadID != "gd-new" {
		t.Errorf("first should be newest (gd-new); got %q", got[0].BeadID)
	}
	if got[1].BeadID != "gd-old" {
		t.Errorf("second should be older (gd-old); got %q", got[1].BeadID)
	}
	// Limit honored.
	got2 := buildSiblingViews(siblings, "gd-self", render, now, 1)
	if len(got2) != 1 {
		t.Errorf("limit=1 returned %d rows", len(got2))
	}
}

func TestBuildRatingReferences(t *testing.T) {
	revDeps := []beadsapi.Dependency{
		{BeadID: "gd-actor", DependsOnID: "gd-rating", Type: "blocks"},
		{BeadID: "gd-self-edge", DependsOnID: "gd-rating", Type: "rates"}, // must be filtered
		{BeadID: "gd-actor", DependsOnID: "gd-rating", Type: "related"},   // dup BeadID, dropped
	}
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-mentioner", Title: "T", Type: "task", Status: "open", SourceField: "description"},
		{ID: "gd-actor", Title: "T2", Type: "task"}, // dup w/ revDeps, dropped
	}
	got := buildRatingReferences(revDeps, mentions, 10)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2; got %+v", len(got), got)
	}
	if got[0].BeadID != "gd-actor" || got[0].Kind != "dep" || got[0].DepType != "blocks" {
		t.Errorf("first ref: %+v", got[0])
	}
	if got[1].BeadID != "gd-mentioner" || got[1].Kind != "mention" || got[1].SourceField != "description" {
		t.Errorf("second ref: %+v", got[1])
	}
	// Self-edge excluded.
	for _, r := range got {
		if r.BeadID == "gd-self-edge" {
			t.Errorf("rates self-edge leaked into references: %+v", r)
		}
	}
	// Limit honored.
	got2 := buildRatingReferences(revDeps, mentions, 1)
	if len(got2) != 1 {
		t.Errorf("limit=1 returned %d rows", len(got2))
	}
}

func TestIsSelfRating(t *testing.T) {
	cases := []struct {
		name  string
		bead  *beadsapi.BeadDetail
		email string
		want  bool
	}{
		{"empty email", &beadsapi.BeadDetail{Assignee: "x"}, "", false},
		{"nil bead", nil, "x", false},
		{"assignee match", &beadsapi.BeadDetail{Assignee: "alice@example.com"}, "alice@example.com", true},
		{"actor match", &beadsapi.BeadDetail{Fields: map[string]string{"actor": "alice@example.com"}}, "alice@example.com", true},
		{"case insensitive", &beadsapi.BeadDetail{Assignee: "Alice@Example.com"}, "alice@example.com", true},
		{"no match", &beadsapi.BeadDetail{Assignee: "bob@example.com"}, "alice@example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isSelfRating(tc.bead, tc.email)
			if got != tc.want {
				t.Errorf("isSelfRating = %v, want %v", got, tc.want)
			}
		})
	}
}
