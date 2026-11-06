package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ratingsFake builds a fakeDaemon seeded with rating beads suitable for
// /ratings list-page tests. Two ratings against two distinct rated beads,
// from two distinct raters.
func ratingsFake() *fakeDaemon {
	now := time.Now()
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-rat1": {
				ID:          "gd-rat1",
				Title:       "rating: Example bead · 4★",
				Type:        "rating",
				Status:      "open",
				Labels:      []string{"project:gardenio"},
				CreatedAt:   now.Add(-3 * time.Hour),
				Description: "Solid execution.",
				Fields: map[string]string{
					"rated_bead_id": "gd-tgt1",
					"rater":         "alice@example.com",
					"score":         "4",
				},
			},
			"gd-rat2": {
				ID:        "gd-rat2",
				Title:     "rating: Other bead · 2★",
				Type:      "rating",
				Status:    "open",
				CreatedAt: now.Add(-2 * 24 * time.Hour),
				Fields: map[string]string{
					"rated_bead_id": "gd-tgt2",
					"rater":         "bob@example.com",
					"score":         "2",
				},
			},
			"gd-tgt1": {ID: "gd-tgt1", Title: "Example bead", Type: "task"},
			"gd-tgt2": {ID: "gd-tgt2", Title: "Other bead", Type: "feature"},
		},
		listResp: &beadsapi.ListBeadsResult{},
	}
}

func TestHandleRatings_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No ratings yet.") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	q := f.listAllQ[0]
	if got := q.Types; len(got) != 1 || got[0] != "rating" {
		t.Errorf("types = %v, want [rating]", got)
	}
	if q.Sort != "-created_at" {
		t.Errorf("sort = %q, want -created_at", q.Sort)
	}
}

func TestHandleRatings_NonEmpty_RendersRows(t *testing.T) {
	f := ratingsFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-rat1"], f.beads["gd-rat2"]},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"gd-rat1", "gd-rat2",
		"Example bead", "Other bead",
		"alice@example.com",
		"bob@example.com",
		`href="/gd-tgt1"`,
		`href="/gd-tgt2"`,
		"★", // rendered stars
		// Source-filter strip now renders through the shared filter_strip partial
		// (ka-0000006647 / audit F7): the default "All" chip is active
		// (`chip chip-active`) and carries the standardized aria-current="true"
		// cue for in-page filter toggles (ka-0000002684 / audit F3).
		`class="chip chip-active"`,
		`aria-current="true">All</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ratings page missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "No ratings yet.") {
		t.Errorf("unexpected empty-state when ratings present")
	}
}

func TestHandleRatings_ProjectFilter_AppliesLabel(t *testing.T) {
	f := ratingsFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	q := f.listAllQ[0]
	wantLabel := "project:gardenio"
	if !slices.Contains(q.Labels, wantLabel) {
		t.Errorf("labels = %v, want to contain %q", q.Labels, wantLabel)
	}
}

func TestHandleRatings_RaterFilter_FiltersInMemory(t *testing.T) {
	f := ratingsFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-rat1"], f.beads["gd-rat2"]},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings?rater=alice@example.com", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "gd-rat1") {
		t.Errorf("expected alice's rating in body\nbody=%s", body)
	}
	if strings.Contains(body, "gd-rat2") {
		t.Errorf("bob's rating should be filtered out\nbody=%s", body)
	}
}

func TestHandleRatings_DaemonError_ShowsBanner(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with error banner, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load ratings.") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

// TestSubmitRating_Create_HappyPath covers the first-time rating: validates
// score, creates a rating bead with the right fields, wires the rates
// dependency, returns 200 with the success-state widget.
func TestSubmitRating_Create_HappyPath(t *testing.T) {
	f := newFake()
	f.beads["gd-tgt"] = &beadsapi.BeadDetail{
		ID:     "gd-tgt",
		Title:  "Target bead",
		Type:   "task",
		Status: "open",
		Labels: []string{"project:gardenio"},
	}
	f.createResp = "gd-rat-new"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("score", "4")
	form.Set("rationale", "Looked clean.")
	req := httptest.NewRequest("POST", "/b/gd-tgt/rating", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "rater@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d. body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", f.createCalls)
	}
	if f.addDepCalls != 1 {
		t.Fatalf("expected 1 AddDependency call, got %d", f.addDepCalls)
	}
	if f.addDepLastBead != "gd-rat-new" || f.addDepLastDep != "gd-tgt" || f.addDepLastType != "rates" {
		t.Errorf("AddDependency args wrong: bead=%q dep=%q type=%q",
			f.addDepLastBead, f.addDepLastDep, f.addDepLastType)
	}
	if f.createLastReq.Type != "rating" {
		t.Errorf("create type = %q, want rating", f.createLastReq.Type)
	}
	if f.createLastReq.CreatedBy != "rater@example.com" {
		t.Errorf("create createdBy = %q, want rater@example.com", f.createLastReq.CreatedBy)
	}
	if f.createLastReq.Description != "Looked clean." {
		t.Errorf("create description = %q, want rationale text", f.createLastReq.Description)
	}
	// Project label should be propagated from rated bead.
	wantProject := "project:gardenio"
	if !slices.Contains(f.createLastReq.Labels, wantProject) {
		t.Errorf("create labels = %v, want to contain %q", f.createLastReq.Labels, wantProject)
	}
	// Fields blob must carry typed score (integer), rater, rated_bead_id.
	var fields map[string]any
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatalf("fields not valid JSON: %v", err)
	}
	scoreNum, ok := fields["score"].(float64)
	if !ok {
		t.Fatalf("fields.score = %#v (%T), want JSON number", fields["score"], fields["score"])
	}
	if int(scoreNum) != 4 {
		t.Errorf("fields.score = %v, want 4", scoreNum)
	}
	if fields["rater"] != "rater@example.com" {
		t.Errorf("fields.rater = %q, want rater@example.com", fields["rater"])
	}
	if fields["rated_bead_id"] != "gd-tgt" {
		t.Errorf("fields.rated_bead_id = %q, want gd-tgt", fields["rated_bead_id"])
	}
}

// TestSubmitRating_Update_Idempotent verifies that re-rating finds the
// existing rating bead via reverse-deps walk and updates instead of creating
// a second one.
func TestSubmitRating_Update_Idempotent(t *testing.T) {
	f := newFake()
	f.beads["gd-tgt"] = &beadsapi.BeadDetail{
		ID: "gd-tgt", Title: "Target", Type: "task", Status: "open",
	}
	f.beads["gd-rat-existing"] = &beadsapi.BeadDetail{
		ID: "gd-rat-existing", Title: "rating: Target · 3★", Type: "rating",
		Status: "open",
		Fields: map[string]string{
			"rated_bead_id": "gd-tgt",
			"rater":         "rater@example.com",
			"score":         "3",
		},
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-tgt": {{BeadID: "gd-rat-existing", DependsOnID: "gd-tgt", Type: "rates"}},
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("score", "5")
	form.Set("rationale", "Even better on retry.")
	form.Set("axes_correctness", "5")
	form.Set("tags", "thorough, fast")
	req := httptest.NewRequest("POST", "/b/gd-tgt/rating", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "rater@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d. body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("expected 0 CreateBead on update, got %d", f.createCalls)
	}
	if f.addDepCalls != 0 {
		t.Errorf("expected 0 AddDependency on update, got %d", f.addDepCalls)
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 UpdateBead, got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-rat-existing" {
		t.Errorf("update target = %q, want gd-rat-existing", f.updateLastID)
	}
	if f.updateLastReq.Description == nil || *f.updateLastReq.Description != "Even better on retry." {
		t.Errorf("update description not propagated: %+v", f.updateLastReq.Description)
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("expected 1 UpdateBeadFieldsTyped on re-rate, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-rat-existing" {
		t.Errorf("update-fields target = %q, want gd-rat-existing", f.updateFieldsLastID)
	}
	if got := f.updateFieldsLastVal["score"]; got != 5 {
		t.Errorf("update-fields score = %v (%T), want integer 5", got, got)
	}
	tags, ok := f.updateFieldsLastVal["tags"].([]string)
	if !ok || len(tags) != 2 || tags[0] != "thorough" || tags[1] != "fast" {
		t.Errorf("update-fields tags = %#v, want []string{thorough, fast}", f.updateFieldsLastVal["tags"])
	}
	axes, ok := f.updateFieldsLastVal["axes"].(map[string]int)
	if !ok || axes["correctness"] != 5 {
		t.Errorf("update-fields axes = %#v, want map[correctness:5]", f.updateFieldsLastVal["axes"])
	}
}

// TestRatingWidget_Standalone verifies the htmx fragment endpoint.
func TestRatingWidget_Standalone(t *testing.T) {
	f := newFake()
	f.beads["gd-tgt"] = &beadsapi.BeadDetail{
		ID: "gd-tgt", Title: "Target", Type: "task", Status: "open",
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/b/gd-tgt/rating", nil)
	req.Header.Set("X-Auth-Request-Email", "rater@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="rating-widget"`,
		`hx-post="/b/gd-tgt/rating"`,
		`name="score"`,
		"rater@example.com",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("widget missing %q\nbody=%s", want, body)
		}
	}
}

// TestBeadShow_RendersRatingWidget verifies the per-bead rating widget shows
// up inline on a non-rating bead's detail page.
func TestBeadShow_RendersRatingWidget(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	req.Header.Set("X-Auth-Request-Email", "rater@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="rating-widget"`,
		`hx-post="/b/gd-aaa/rating"`,
		"no ratings yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bead_show missing rating-widget marker %q\nbody=%s", want, body)
		}
	}
}

// TestBeadShow_RatingTypeBead_RendersRatingCard verifies the per-type rating
// detail renders without the widget (rating beads can't be rated themselves).
func TestBeadShow_RatingTypeBead_RendersRatingCard(t *testing.T) {
	f := newFake()
	f.beads["gd-rat1"] = &beadsapi.BeadDetail{
		ID:        "gd-rat1",
		Title:     "rating: Example bead · 4★",
		Type:      "rating",
		Status:    "open",
		CreatedAt: time.Now().Add(-1 * time.Hour),
		Fields: map[string]string{
			"rated_bead_id": "gd-aaa",
			"rater":         "alice@example.com",
			"score":         "4",
			"axes":          `{"correctness":5,"scope":3}`,
			"tags":          "thorough, fast",
		},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/gd-rat1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="rating-card"`,
		"alice@example.com",
		"4/5",
		`href="/gd-aaa"`,
		"correctness",
		"scope",
		"thorough",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rating-type bead show missing %q\nbody=%s", want, body)
		}
	}
	// The widget must NOT render on rating-type pages — they cannot be re-rated.
	if strings.Contains(body, `id="rating-widget"`) {
		t.Errorf("rating-type bead should not render rating-widget\nbody=%s", body)
	}
}

// ratingsSlackFoldFake seeds a fakeDaemon with mixed web + slack-* rating
// rows for filter and column tests.
func ratingsSlackFoldFake() *fakeDaemon {
	now := time.Now()
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-rat-web": {
				ID: "gd-rat-web", Title: "rating: web · 4★", Type: "rating", Status: "open",
				Labels: []string{"project:gardenio"}, CreatedAt: now.Add(-1 * time.Hour),
				Description: "Web rating.",
				Fields: map[string]string{
					"rated_bead_id": "gd-tgt-w", "rater": "alice@example.com",
					"score": "4", "source": "web",
				},
			},
			"gd-rat-slack-pos": {
				ID: "gd-rat-slack-pos", Title: "rating: slack +1", Type: "rating", Status: "open",
				CreatedAt: now.Add(-2 * time.Hour),
				Fields: map[string]string{
					"rated_bead_id": "gd-tgt-s", "rater": "bob@example.com",
					"score": "5", "score_native": "1", "source": "slack-stop",
					"slack_user_id": "U0OPERATOR1",
				},
			},
			"gd-rat-slack-neg": {
				ID: "gd-rat-slack-neg", Title: "rating: slack -1", Type: "rating", Status: "open",
				CreatedAt: now.Add(-3 * time.Hour),
				Fields: map[string]string{
					"rated_bead_id": "gd-tgt-s", "rater": "carol@example.com",
					"score": "1", "score_native": "-1", "source": "slack-manual",
				},
			},
			"gd-rat-slack-fb": {
				ID: "gd-rat-slack-fb", Title: "rating: slack feedback", Type: "rating", Status: "open",
				CreatedAt: now.Add(-4 * time.Hour),
				Fields: map[string]string{
					"rated_bead_id": "gd-tgt-s", "rater": "dave@example.com",
					"score": "0", "score_native": "0", "source": "slack-feedback-only",
					"feedback": "Could be tighter.",
				},
			},
			"gd-tgt-w": {ID: "gd-tgt-w", Title: "Web target", Type: "task"},
			"gd-tgt-s": {ID: "gd-tgt-s", Title: "Slack-rated agent", Type: "agent"},
		},
		listResp: &beadsapi.ListBeadsResult{},
	}
}

// TestHandleRatings_RendersSourceColumn verifies the Source column shows
// both web and slack badges and that slack rows render their native score.
func TestHandleRatings_RendersSourceColumn(t *testing.T) {
	f := ratingsSlackFoldFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			f.beads["gd-rat-web"],
			f.beads["gd-rat-slack-pos"],
			f.beads["gd-rat-slack-neg"],
			f.beads["gd-rat-slack-fb"],
		},
		Total: 4,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`rating-source-web`,   // badge class for the web row
		`rating-source-slack`, // badge class for slack rows
		`(&#43;1)`,            // formatted positive native (HTML-escaped)
		`(−1)`,                // typographic minus for negative native
		`feedback only`,       // suppressed-stars rendering
		`Could be tighter.`,   // feedback snippet falls through to description slot
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ratings page missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleRatings_SourceFilter_Slack only keeps slack-* rows when ?source=slack.
func TestHandleRatings_SourceFilter_Slack(t *testing.T) {
	f := ratingsSlackFoldFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			f.beads["gd-rat-web"],
			f.beads["gd-rat-slack-pos"],
			f.beads["gd-rat-slack-neg"],
		},
		Total: 3,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings?source=slack", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "gd-rat-web") {
		t.Errorf("source=slack should hide web rows\nbody=%s", body)
	}
	for _, want := range []string{"gd-rat-slack-pos", "gd-rat-slack-neg"} {
		if !strings.Contains(body, want) {
			t.Errorf("source=slack should show %q\nbody=%s", want, body)
		}
	}
}

// TestHandleRatings_SourceFilter_Web matches both source=web and unset
// (legacy) rows under ?source=web.
func TestHandleRatings_SourceFilter_Web(t *testing.T) {
	f := ratingsSlackFoldFake()
	f.beads["gd-rat-legacy"] = &beadsapi.BeadDetail{
		ID: "gd-rat-legacy", Title: "rating: legacy", Type: "rating", Status: "open",
		CreatedAt: time.Now().Add(-5 * time.Hour),
		Fields: map[string]string{
			"rated_bead_id": "gd-tgt-w", "rater": "eve@example.com",
			"score": "3", // no source field
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			f.beads["gd-rat-web"],
			f.beads["gd-rat-slack-pos"],
			f.beads["gd-rat-legacy"],
		},
		Total: 3,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings?source=web", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "gd-rat-slack-pos") {
		t.Errorf("source=web should hide slack rows\nbody=%s", body)
	}
	if !strings.Contains(body, "gd-rat-web") {
		t.Errorf("source=web should show source=web row\nbody=%s", body)
	}
	if !strings.Contains(body, "gd-rat-legacy") {
		t.Errorf("source=web should include rows with no source field\nbody=%s", body)
	}
}

// TestRatingWidget_DualAggregateAndSlackBadge covers Phase 2 §4.2: the
// per-bead widget surfaces a slack badge on each slack row plus a secondary
// native-scale aggregate underneath the star aggregate. score=0 rows do not
// render stars.
func TestRatingWidget_DualAggregateAndSlackBadge(t *testing.T) {
	f := newFake()
	f.beads["gd-tgt"] = &beadsapi.BeadDetail{
		ID: "gd-tgt", Title: "Target", Type: "agent", Status: "open",
	}
	f.beads["gd-rat-pos"] = &beadsapi.BeadDetail{
		ID: "gd-rat-pos", Type: "rating", Status: "open",
		CreatedAt: time.Now().Add(-1 * time.Hour),
		Fields: map[string]string{
			"rated_bead_id": "gd-tgt", "rater": "bob@example.com",
			"score": "5", "score_native": "1", "source": "slack-stop",
		},
	}
	f.beads["gd-rat-neg"] = &beadsapi.BeadDetail{
		ID: "gd-rat-neg", Type: "rating", Status: "open",
		CreatedAt: time.Now().Add(-2 * time.Hour),
		Fields: map[string]string{
			"rated_bead_id": "gd-tgt", "rater": "carol@example.com",
			"score": "1", "score_native": "-1", "source": "slack-manual",
		},
	}
	f.beads["gd-rat-fb"] = &beadsapi.BeadDetail{
		ID: "gd-rat-fb", Type: "rating", Status: "open",
		CreatedAt: time.Now().Add(-3 * time.Hour),
		Fields: map[string]string{
			"rated_bead_id": "gd-tgt", "rater": "dave@example.com",
			"score": "0", "score_native": "0", "source": "slack-feedback-only",
		},
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-tgt": {
			{BeadID: "gd-rat-pos", DependsOnID: "gd-tgt", Type: "rates"},
			{BeadID: "gd-rat-neg", DependsOnID: "gd-tgt", Type: "rates"},
			{BeadID: "gd-rat-fb", DependsOnID: "gd-tgt", Type: "rates"},
		},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/b/gd-tgt/rating", nil)
	req.Header.Set("X-Auth-Request-Email", "viewer@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`Slack scale:`,        // secondary aggregate label
		`(n=3)`,               // 3 rows contributed score_native
		`rating-source-slack`, // slack badge in recent list
		`(&#43;1)`,            // formatted native (html/template escapes +)
		`(−1)`,                // typographic minus
		`feedback only`,       // score=0 row suppressed stars
	} {
		if !strings.Contains(body, want) {
			t.Errorf("widget missing %q\nbody=%s", want, body)
		}
	}
	// Star histogram must reflect star scores 1..5 only — score=0 should NOT
	// land in a histogram bucket.
	if strings.Count(body, `<th scope="row">`) != 5 {
		t.Errorf("expected 5 histogram rows; body=%s", body)
	}
}

// TestAggregateRatings_NativeScale validates that a mix of star+native rows
// produces a non-zero NativeCount and a signed native mean.
func TestAggregateRatings_NativeScale(t *testing.T) {
	mk := func(score, native string) *beadsapi.BeadDetail {
		f := map[string]string{"score": score}
		if native != "" {
			f["score_native"] = native
		}
		return &beadsapi.BeadDetail{Type: "rating", Fields: f}
	}
	beads := []*beadsapi.BeadDetail{
		mk("5", "1"), mk("5", "1"), mk("3", "0"), mk("1", "-1"), mk("4", ""),
	}
	agg := aggregateRatings(beads)
	if agg.Count != 5 {
		t.Errorf("Count = %d, want 5", agg.Count)
	}
	if agg.NativeCount != 4 {
		t.Errorf("NativeCount = %d, want 4 (one row had no native)", agg.NativeCount)
	}
	// (1+1+0-1)/4 = 0.25 -> "+0.3" (1dp rounding via FormatFloat)
	if agg.NativeMean < 0.2 || agg.NativeMean > 0.3 {
		t.Errorf("NativeMean = %f, want ~0.25", agg.NativeMean)
	}
	if !strings.HasPrefix(agg.NativeMeanLabel, "+") {
		t.Errorf("NativeMeanLabel = %q, want signed +", agg.NativeMeanLabel)
	}
}

// TestAggregateRatings_FeedbackOnlyExcludedFromStars confirms a score=0
// feedback-only row does not skew the star histogram or count.
func TestAggregateRatings_FeedbackOnlyExcludedFromStars(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{Type: "rating", Fields: map[string]string{"score": "5", "score_native": "1"}},
		{Type: "rating", Fields: map[string]string{"score": "0", "score_native": "0", "source": "slack-feedback-only"}},
	}
	agg := aggregateRatings(beads)
	if agg.Count != 1 {
		t.Errorf("star Count = %d, want 1 (score=0 excluded)", agg.Count)
	}
	if agg.NativeCount != 2 {
		t.Errorf("NativeCount = %d, want 2", agg.NativeCount)
	}
}

// TestFormatNativeScore covers the inline rendering helper.
func TestFormatNativeScore(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1, "+1"},
		{0, "0"},
		{-1, "−1"}, // U+2212
	}
	for _, tc := range cases {
		if got := formatNativeScore(tc.n); got != tc.want {
			t.Errorf("formatNativeScore(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestMatchesSourceFilter covers the post-list filter shape.
func TestMatchesSourceFilter(t *testing.T) {
	cases := []struct {
		row, filter string
		want        bool
	}{
		{"", "", true},
		{"web", "", true},
		{"slack-stop", "slack", true},
		{"slack-feedback-only", "slack", true},
		{"slack-backfill-stop", "slack", true},
		{"web", "slack", false},
		{"web", "web", true},
		{"", "web", true}, // legacy unset row counts as web
		{"slack-stop", "web", false},
		{"slack-stop", "slack-stop", true},
		{"slack-stop", "slack-manual", false},
	}
	for _, tc := range cases {
		if got := matchesSourceFilter(tc.row, tc.filter); got != tc.want {
			t.Errorf("matchesSourceFilter(%q,%q) = %v, want %v", tc.row, tc.filter, got, tc.want)
		}
	}
}

// TestNavLink_RatingsPresent verifies the layout's nav has a Ratings link so
// the tab is reachable from any page.
func TestNavLink_RatingsPresent(t *testing.T) {
	// Ratings is a vibe-only nav link after gb-ux #3 (ka-0000003340); render as
	// a vibe-mode viewer so the gated link is present.
	f := newFake()
	f.userResp = vibeUserResp()
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/ratings"`) {
		t.Errorf("layout missing /ratings nav link\nbody=%s", body)
	}
}

// TestHandleRatings_RatedHydrationIsBatched is a HARD call-count regression
// gate for the /ratings N+1 (perf audit ka-0000002433, sibling of the
// /gardeners regression fixed in MR !3442). The list page resolves each row's
// rated-bead title/type; doing that with a per-row GetBead is a serial
// daemon-RTT chain that scales with the rating count. The handler MUST
// instead hydrate the distinct rated ids in a single BatchGetBeads call.
//
// Seeds a large synthetic rating set and asserts: (1) zero per-row GetBead,
// (2) exactly one BatchGetBeads, (3) titles still render. The GetBead==0
// assertion is what fails if the N+1 is reintroduced.
func TestHandleRatings_RatedHydrationIsBatched(t *testing.T) {
	const n = 300
	now := time.Now()
	beads := make(map[string]*beadsapi.BeadDetail, 2*n)
	list := make([]*beadsapi.BeadDetail, 0, n)
	for i := range n {
		ratingID := "gd-rat" + strconv.Itoa(i)
		targetID := "gd-tgt" + strconv.Itoa(i)
		beads[targetID] = &beadsapi.BeadDetail{ID: targetID, Title: "Target " + strconv.Itoa(i), Type: "task"}
		rb := &beadsapi.BeadDetail{
			ID:        ratingID,
			Title:     "rating " + strconv.Itoa(i),
			Type:      "rating",
			Status:    "open",
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
			Fields: map[string]string{
				"rated_bead_id": targetID,
				"rater":         "alice@example.com",
				"score":         "4",
			},
		}
		beads[ratingID] = rb
		list = append(list, rb)
	}
	f := &fakeDaemon{beads: beads}
	f.listResp = &beadsapi.ListBeadsResult{Beads: list, Total: n}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// HARD gate: per-row GetBead is the N+1. It must be zero.
	if f.getBeadCalls != 0 {
		t.Errorf("GetBead calls=%d, want 0 (rated-bead hydration must be batched, not per-row)", f.getBeadCalls)
	}
	// Exactly one batch fetch covering every distinct rated id.
	if f.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls=%d, want 1", f.batchGetBeadsCalls)
	}
	if f.batchGetBeadsLastN != n {
		t.Errorf("BatchGetBeads ids=%d, want %d (one per distinct rated bead)", f.batchGetBeadsLastN, n)
	}
	// Title still resolves through the batch path.
	if body := w.Body.String(); !strings.Contains(body, "Target 0") {
		t.Errorf("rated title missing — batch hydration did not feed the row view")
	}
}

// TestHandleRatings_BatchDedupsRepeatedRatedBeads verifies the batch carries
// only DISTINCT rated ids: when many ratings target the same bead, the single
// BatchGetBeads call must not re-list that id once per row.
func TestHandleRatings_BatchDedupsRepeatedRatedBeads(t *testing.T) {
	now := time.Now()
	beads := map[string]*beadsapi.BeadDetail{
		"gd-shared": {ID: "gd-shared", Title: "Shared target", Type: "task"},
	}
	list := make([]*beadsapi.BeadDetail, 0, 50)
	for i := range 50 {
		id := "gd-rat" + strconv.Itoa(i)
		rb := &beadsapi.BeadDetail{
			ID:        id,
			Type:      "rating",
			Status:    "open",
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
			Fields: map[string]string{
				"rated_bead_id": "gd-shared",
				"rater":         "alice@example.com",
				"score":         "5",
			},
		}
		beads[id] = rb
		list = append(list, rb)
	}
	f := &fakeDaemon{beads: beads}
	f.listResp = &beadsapi.ListBeadsResult{Beads: list, Total: 50}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/ratings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.getBeadCalls != 0 {
		t.Errorf("GetBead calls=%d, want 0", f.getBeadCalls)
	}
	if f.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls=%d, want 1", f.batchGetBeadsCalls)
	}
	if f.batchGetBeadsLastN != 1 {
		t.Errorf("BatchGetBeads ids=%d, want 1 (distinct rated ids only)", f.batchGetBeadsLastN)
	}
}
