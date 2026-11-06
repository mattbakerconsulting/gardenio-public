package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadShow_Rating_FocusedView verifies the focused rating-detail view:
// generic body sections are suppressed, feedback renders, and Siblings,
// RaterRecent, and References are populated from the daemon.
func TestBeadShow_Rating_FocusedView(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.beads["gd-rat-self"] = &beadsapi.BeadDetail{
		ID:        "gd-rat-self",
		Title:     "rating: agent · feedback",
		Type:      "rating",
		Status:    "open",
		Notes:     "SHOULD-NOT-RENDER scratchpad",
		Labels:    []string{"project:gardenio"},
		CreatedAt: now.Add(-1 * time.Hour),
		Fields: map[string]string{
			"rated_bead_id": "gd-target",
			"rater":         "alice@example.com",
			"score":         "0",
			"score_native":  "0",
			"source":        "slack-feedback-only",
			"feedback":      "FEEDBACK-MARKER could be tighter.",
		},
	}
	f.beads["gd-rat-sib"] = &beadsapi.BeadDetail{
		ID: "gd-rat-sib", Title: "rating: agent · 5★", Type: "rating",
		CreatedAt: now.Add(-30 * time.Minute),
		Fields: map[string]string{
			"rated_bead_id": "gd-target",
			"rater":         "bob@example.com",
			"score":         "5",
			"source":        "web",
		},
	}
	f.beads["gd-rat-other"] = &beadsapi.BeadDetail{
		ID: "gd-rat-other", Title: "rating: feature · 3★", Type: "rating",
		CreatedAt: now.Add(-2 * time.Hour),
		Fields: map[string]string{
			"rated_bead_id": "gd-other-target",
			"rater":         "alice@example.com",
			"score":         "3",
			"source":        "web",
		},
	}
	f.beads["gd-target"] = &beadsapi.BeadDetail{ID: "gd-target", Title: "Target Agent", Type: "agent"}
	f.beads["gd-other-target"] = &beadsapi.BeadDetail{ID: "gd-other-target", Title: "Other Target", Type: "feature"}

	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-target": {
			{BeadID: "gd-rat-self", DependsOnID: "gd-target", Type: ratingDepType},
			{BeadID: "gd-rat-sib", DependsOnID: "gd-target", Type: ratingDepType},
		},
		"gd-rat-self": {
			{BeadID: "gd-followup", DependsOnID: "gd-rat-self", Type: "blocks"},
			{BeadID: "gd-target", DependsOnID: "gd-rat-self", Type: ratingDepType},
		},
	}
	f.mentionedBy = map[string][]beadsapi.BeadReferenceSummary{
		"gd-rat-self": {{ID: "gd-mentioner", Title: "Mentioner Bead", Type: "task", Status: "open", SourceField: "description"}},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		f.beads["gd-rat-self"],
		f.beads["gd-rat-other"],
		f.beads["gd-rat-sib"],
	}}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-rat-self", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, mustNotContain := range []string{
		"SHOULD-NOT-RENDER",
		"<h2>Metadata</h2>",
		"<h3>Notes</h3>",
		"<h3>Comments</h3>",
		`id="rating-widget"`,
	} {
		if strings.Contains(body, mustNotContain) {
			t.Errorf("rating focused view should not contain %q", mustNotContain)
		}
	}

	for _, mustContain := range []string{
		`id="rating-card"`,
		"FEEDBACK-MARKER could be tighter.",
		"Slack feedback",
		`href="/gd-target"`,
		"Other ratings on this bead",
		"gd-rat-sib",
		"Recent ratings by alice@example.com",
		"gd-rat-other",
		"Beads that reference this rating",
		"gd-followup",
		"gd-mentioner",
	} {
		if !strings.Contains(body, mustContain) {
			t.Errorf("focused view missing %q\nbody=%s", mustContain, body)
		}
	}

	if c := strings.Count(body, "gd-target"); c < 1 {
		t.Errorf("expected at least one gd-target link (rated-bead), got %d", c)
	}
}
