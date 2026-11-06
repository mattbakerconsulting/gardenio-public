// Tests for the type:rating built-in (gd-XPdJCTFzib) — covers create,
// list-by-field-equality (idempotent upsert lookup), and validation of the
// score 1–5 bound from the human-ratings plan-of-record.
package server

import (
	"encoding/json"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// seedRatedBead inserts a non-rating target bead into the mock store so a
// CreateBead(type=rating) request can resolve it during preCreateRating.
func seedRatedBead(ms *mockStore, id, owner string) *model.Bead {
	b := &model.Bead{
		ID: id, Title: "target", Type: "task", Kind: "issue", Status: "open",
		Owner: owner,
	}
	ms.beads[id] = b
	return b
}

func TestCreateBead_Rating_Valid(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedRatedBead(ms, "gd-target", "")

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "rating: gd-target · 4★",
		Type:   "rating",
		Fields: []byte(`{"rated_bead_id":"gd-target","score":4,"rater":"alice@example.com"}`),
	})
	if err != nil {
		t.Fatalf("create rating: %v", err)
	}
	if resp.Bead.Type != "rating" {
		t.Errorf("type=%q, want rating", resp.Bead.Type)
	}
	if resp.Bead.Kind != "data" {
		t.Errorf("kind=%q, want data", resp.Bead.Kind)
	}
	if _, ok := ms.beads[resp.Bead.Id]; !ok {
		t.Error("rating bead was not stored")
	}

	// The `rates` dep edge must be wired automatically per plan §2.2.
	deps := ms.deps[resp.Bead.Id]
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep on rating bead, got %d", len(deps))
	}
	if deps[0].DependsOnID != "gd-target" {
		t.Errorf("dep depends_on=%q, want gd-target", deps[0].DependsOnID)
	}
	if deps[0].Type != model.DepRates {
		t.Errorf("dep type=%q, want rates", deps[0].Type)
	}
	// Regression for ka-0000007722: the auto-wired `rates` dep must carry
	// the operator FK populated by resolveAuditOperatorRequired so the
	// NOT NULL constraint on deps.operated_by_id (migration 022) is
	// satisfied. Before the fix this field was unset and prod INSERTs
	// failed with NOT NULL violations, surfacing as HTTP 500 in the
	// beads-viewer ratings spec (gardenio ratings.spec.js).
	if deps[0].OperatedByID == nil {
		t.Error("rates dep OperatedByID is nil; would violate deps.operated_by_id NOT NULL in Postgres")
	}
}

func TestCreateBead_Rating_RejectsScoreOutOfRange(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedRatedBead(ms, "gd-target", "")

	// T4a-2: schema relaxed to score>=0; -1 still rejected by min, 6/99 by max.
	// score=0 is now schema-valid but gated at runtime to source=slack-feedback-only
	// (covered separately in TestCreateBead_Rating_ScoreZeroRequiresFeedbackOnly).
	for _, score := range []int{-1, 6, 99} {
		_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
			Title:  "bad",
			Type:   "rating",
			Fields: []byte(`{"rated_bead_id":"gd-target","rater":"a@b","score":` + itoa(score) + `}`),
		})
		if err == nil {
			t.Errorf("score=%d should fail validation", score)
			continue
		}
		if !strings.Contains(err.Error(), "score") {
			t.Errorf("score=%d error should mention 'score', got: %v", score, err)
		}
	}
}

// TestCreateBead_Rating_ScoreZeroRequiresFeedbackOnly covers the T4a-2 runtime
// rule: score=0 is the Slack feedback-only sentinel, and is rejected when paired
// with any other source. Schema-level min was relaxed from 1 to 0.
// Plan: gardenio/plans/beads-viewer-human-ratings-slack-fold.md §2, §3.3.
func TestCreateBead_Rating_ScoreZeroRequiresFeedbackOnly(t *testing.T) {
	t.Run("score=0 with source=slack-feedback-only is accepted", func(t *testing.T) {
		srv, ms, ctx := testCtx(t)
		seedRatedBead(ms, "gd-target", "")
		resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
			Title: "feedback-only",
			Type:  "rating",
			Fields: []byte(`{
				"rated_bead_id": "gd-target",
				"rater":         "alice@example.com",
				"score":         0,
				"score_native":  0,
				"source":        "slack-feedback-only",
				"feedback":      "the agent rambled"
			}`),
		})
		if err != nil {
			t.Fatalf("score=0 + source=slack-feedback-only should pass: %v", err)
		}
		if resp.Bead.Type != "rating" {
			t.Errorf("type=%q, want rating", resp.Bead.Type)
		}
	})

	t.Run("score=0 without source is rejected", func(t *testing.T) {
		srv, ms, ctx := testCtx(t)
		seedRatedBead(ms, "gd-target", "")
		_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
			Title:  "bad",
			Type:   "rating",
			Fields: []byte(`{"rated_bead_id":"gd-target","rater":"a@b","score":0}`),
		})
		if err == nil {
			t.Fatal("score=0 with no source should be rejected")
		}
		if !strings.Contains(err.Error(), "slack-feedback-only") {
			t.Errorf("error should mention the rule, got: %v", err)
		}
	})

	t.Run("score=0 with source=slack-stop is rejected", func(t *testing.T) {
		srv, ms, ctx := testCtx(t)
		seedRatedBead(ms, "gd-target", "")
		_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
			Title:  "bad",
			Type:   "rating",
			Fields: []byte(`{"rated_bead_id":"gd-target","rater":"a@b","score":0,"source":"slack-stop"}`),
		})
		if err == nil {
			t.Fatal("score=0 with source=slack-stop should be rejected")
		}
	})
}

// TestCreateBead_Rating_AcceptsSlackFields covers the T4a-2 additive optional
// fields: score_native, source, slack_channel, slack_thread_ts, slack_user_id,
// feedback. A Slack-source rating with the full kit lands cleanly.
// Plan: gardenio/plans/beads-viewer-human-ratings-slack-fold.md §2.
func TestCreateBead_Rating_AcceptsSlackFields(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedRatedBead(ms, "gd-agent", "")

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "rating: gd-agent · 5★",
		Type:  "rating",
		Fields: []byte(`{
			"rated_bead_id":   "gd-agent",
			"rater":           "alice@example.com",
			"score":           5,
			"score_native":    1,
			"source":          "slack-stop",
			"slack_channel":   "C0123456789",
			"slack_thread_ts": "1700000000.123456",
			"slack_user_id":   "U0OPERATOR1",
			"feedback":        "great session"
		}`),
	})
	if err != nil {
		t.Fatalf("slack-shaped rating should validate: %v", err)
	}

	stored := ms.beads[resp.Bead.Id]
	var fm map[string]any
	if err := json.Unmarshal(stored.Fields, &fm); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	for _, name := range []string{"score_native", "source", "slack_channel", "slack_thread_ts", "slack_user_id", "feedback"} {
		if _, ok := fm[name]; !ok {
			t.Errorf("stored fields missing %q", name)
		}
	}
	if got, want := fm["source"], "slack-stop"; got != want {
		t.Errorf("source=%v, want %v", got, want)
	}
	if got, want := fm["score_native"], float64(1); got != want {
		t.Errorf("score_native=%v, want %v", got, want)
	}
}

// TestCreateBead_Rating_AcceptsAnsweredAtAndFeedbackRaw covers gd-nDwMiTy412 —
// the Slack bridge dual-write writes `answered_at` (RFC3339 timestamp) and
// `feedback_raw` (untrimmed feedback text) alongside the canonical fields.
// Both are additive and must validate without the "unknown field" rejection.
func TestCreateBead_Rating_AcceptsAnsweredAtAndFeedbackRaw(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedRatedBead(ms, "gd-agent", "")

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "rating: gd-agent · 5★",
		Type:  "rating",
		Fields: []byte(`{
			"rated_bead_id":   "gd-agent",
			"rater":           "alice@example.com",
			"score":           5,
			"source":          "slack-stop",
			"answered_at":     "2026-05-03T18:00:00Z",
			"feedback_raw":    "  great session  ",
			"feedback":        "great session"
		}`),
	})
	if err != nil {
		t.Fatalf("rating with answered_at + feedback_raw should validate: %v", err)
	}

	stored := ms.beads[resp.Bead.Id]
	var fm map[string]any
	if err := json.Unmarshal(stored.Fields, &fm); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if got, want := fm["answered_at"], "2026-05-03T18:00:00Z"; got != want {
		t.Errorf("answered_at=%v, want %v", got, want)
	}
	if got, want := fm["feedback_raw"], "  great session  "; got != want {
		t.Errorf("feedback_raw=%v, want %v", got, want)
	}
}

// TestCreateBead_Rating_PreExistingViewerShapeStillValid: rows authored by the
// beads-viewer (no Slack fields, score in 1..5) must continue to validate after
// T4a-2 — the change is additive and the relaxed min=0 does not exclude any
// previously-valid value.
func TestCreateBead_Rating_PreExistingViewerShapeStillValid(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedRatedBead(ms, "gd-target", "")

	// Sweep the originally-valid 1..5 range.
	for _, score := range []int{1, 2, 3, 4, 5} {
		_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
			Title:  "viewer-shape",
			Type:   "rating",
			Fields: []byte(`{"rated_bead_id":"gd-target","rater":"r-` + itoa(score) + `@x","score":` + itoa(score) + `}`),
		})
		if err != nil {
			t.Errorf("legacy viewer rating with score=%d must still validate: %v", score, err)
		}
	}
	_ = ms
}

func TestCreateBead_Rating_RequiresFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	cases := []struct {
		name   string
		fields string
	}{
		{"missing rated_bead_id", `{"score":3,"rater":"a@b"}`},
		{"missing score", `{"rated_bead_id":"gd-x","rater":"a@b"}`},
		{"missing rater", `{"rated_bead_id":"gd-x","score":3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
				Title: "r", Type: "rating", Fields: []byte(tc.fields),
			})
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestCreateBead_Rating_RejectsUnknownAxisField(t *testing.T) {
	srv, _, ctx := testCtx(t)
	// Stray top-level field outside the schema is rejected by ValidateTypeFields.
	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "r",
		Type:   "rating",
		Fields: []byte(`{"rated_bead_id":"gd-x","score":3,"rater":"a@b","mystery":"oops"}`),
	})
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

// TestListBeads_Rating_FieldEqualityLookup is the idempotent-upsert lookup
// that beads-viewer uses on POST: find the (rated_bead_id, rater) pair to
// decide create-vs-update. Plan §5.3.
func TestListBeads_Rating_FieldEqualityLookup(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Two ratings for the same target by different raters, plus an unrelated
	// rating to confirm the filter scopes correctly.
	ms.beads["gd-r-1"] = &model.Bead{
		ID: "gd-r-1", Title: "r1", Type: "rating", Kind: "data", Status: "open",
		Fields: []byte(`{"rated_bead_id":"gd-target","score":"5","rater":"alice@example.com"}`),
	}
	ms.beads["gd-r-2"] = &model.Bead{
		ID: "gd-r-2", Title: "r2", Type: "rating", Kind: "data", Status: "open",
		Fields: []byte(`{"rated_bead_id":"gd-target","score":"3","rater":"bob@example.com"}`),
	}
	ms.beads["gd-r-3"] = &model.Bead{
		ID: "gd-r-3", Title: "r3", Type: "rating", Kind: "data", Status: "open",
		Fields: []byte(`{"rated_bead_id":"gd-other","score":"4","rater":"alice@example.com"}`),
	}

	resp, err := srv.ListBeads(ctx, &beadsv1.ListBeadsRequest{
		Type: []string{"rating"},
		FieldFilters: map[string]string{
			"rated_bead_id": "gd-target",
			"rater":         "alice@example.com",
		},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Beads) != 1 {
		t.Fatalf("got %d beads, want 1", len(resp.Beads))
	}
	if resp.Beads[0].Id != "gd-r-1" {
		t.Errorf("got id=%q, want gd-r-1", resp.Beads[0].Id)
	}
}

// TestRating_DepRatesType asserts the rating dep-type constant is non-empty
// and accepted by the extensible IsValid check, so AddDependency callers
// can wire `rating-bead --rates--> rated-bead` (plan §2.2).
func TestRating_DepRatesType(t *testing.T) {
	if model.DepRates != "rates" {
		t.Errorf("DepRates=%q, want 'rates'", model.DepRates)
	}
	if !model.DepRates.IsValid() {
		t.Error("DepRates should be a valid dependency type")
	}
}

// TestCreateBead_Rating_IdempotentUpsert verifies the (rater, rated_bead_id)
// upsert lookup from plan §5.3: a second CreateBead from the same rater on
// the same target updates the existing rating instead of creating a new one.
func TestCreateBead_Rating_IdempotentUpsert(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedRatedBead(ms, "gd-target", "")

	first, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "rating: gd-target · 3★",
		Type:   "rating",
		Fields: []byte(`{"rated_bead_id":"gd-target","score":3,"rater":"alice@example.com"}`),
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Count rating beads after first create.
	ratingsBefore := 0
	for _, b := range ms.beads {
		if b.Type == "rating" {
			ratingsBefore++
		}
	}
	if ratingsBefore != 1 {
		t.Fatalf("after first create: got %d rating beads, want 1", ratingsBefore)
	}

	second, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "rating: gd-target · 5★",
		Type:   "rating",
		Fields: []byte(`{"rated_bead_id":"gd-target","score":5,"rater":"alice@example.com"}`),
	})
	if err != nil {
		t.Fatalf("second create (upsert): %v", err)
	}

	if second.Bead.Id != first.Bead.Id {
		t.Errorf("upsert created a new bead: first=%q second=%q", first.Bead.Id, second.Bead.Id)
	}

	// Still exactly one rating bead.
	ratingsAfter := 0
	for _, b := range ms.beads {
		if b.Type == "rating" {
			ratingsAfter++
		}
	}
	if ratingsAfter != 1 {
		t.Errorf("after upsert: got %d rating beads, want 1", ratingsAfter)
	}

	// Stored fields reflect the latest score (5).
	stored := ms.beads[first.Bead.Id]
	var fm map[string]any
	if err := json.Unmarshal(stored.Fields, &fm); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if got, want := fm["score"], float64(5); got != want {
		t.Errorf("score=%v, want %v", got, want)
	}
}

// TestCreateBead_Rating_AllowsSelfRating — rating your own bead is fine; the
// daemon does not gate rater identity against the rated bead's owner /
// assignee / created_by. The beads-viewer UI may still surface self-rates
// distinctly, but the daemon-level write path accepts them.
func TestCreateBead_Rating_AllowsSelfRating(t *testing.T) {
	cases := []struct {
		name  string
		setup func(ms *mockStore)
	}{
		{
			"owner match",
			func(ms *mockStore) {
				seedRatedBead(ms, "gd-self", "alice@example.com")
			},
		},
		{
			"assignee match",
			func(ms *mockStore) {
				b := seedRatedBead(ms, "gd-self", "")
				b.Assignee = "alice@example.com"
			},
		},
		{
			"created_by match",
			func(ms *mockStore) {
				b := seedRatedBead(ms, "gd-self", "")
				b.CreatedBy = "alice@example.com"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ms, ctx := testCtx(t)
			tc.setup(ms)
			resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
				Title:  "self-rating",
				Type:   "rating",
				Fields: []byte(`{"rated_bead_id":"gd-self","score":4,"rater":"alice@example.com"}`),
			})
			if err != nil {
				t.Fatalf("self-rating should be allowed, got error: %v", err)
			}
			if resp.Bead.Type != "rating" {
				t.Errorf("type=%q, want rating", resp.Bead.Type)
			}
		})
	}
}

// TestCreateBead_Rating_RejectsRateOfRating prevents the rate-of-rating loop
// (plan §3.5): a rating bead cannot itself be the target of another rating.
func TestCreateBead_Rating_RejectsRateOfRating(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-r-existing"] = &model.Bead{
		ID: "gd-r-existing", Title: "existing rating", Type: "rating", Kind: "data",
		Status: "open",
		Fields: []byte(`{"rated_bead_id":"gd-target","score":4,"rater":"alice@example.com"}`),
	}
	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "rate-of-rating",
		Type:   "rating",
		Fields: []byte(`{"rated_bead_id":"gd-r-existing","score":3,"rater":"bob@example.com"}`),
	})
	if err == nil {
		t.Fatal("expected rate-of-rating rejection, got nil")
	}
	if !strings.Contains(err.Error(), "rating") {
		t.Errorf("error should mention 'rating' loop, got: %v", err)
	}
}

// TestCreateBead_Rating_RejectsMissingRatedBead — the rated bead must exist.
func TestCreateBead_Rating_RejectsMissingRatedBead(t *testing.T) {
	srv, _, ctx := testCtx(t)
	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "missing target",
		Type:   "rating",
		Fields: []byte(`{"rated_bead_id":"gd-nope","score":4,"rater":"alice@example.com"}`),
	})
	if err == nil {
		t.Fatal("expected missing-rated-bead rejection, got nil")
	}
	if !strings.Contains(err.Error(), "gd-nope") {
		t.Errorf("error should mention the missing id, got: %v", err)
	}
}

// itoa avoids strconv import noise in the small fields-builder above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
