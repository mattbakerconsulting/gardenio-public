package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// newSlackServerWithUsersInfo returns an httptest.Server that responds to
// `users.info` with the given email (when non-empty) and returns OK to all
// other Slack API calls. When email is "", the user lookup returns ok=true
// but with an empty profile email, forcing the rater resolver to fall back
// to a sentinel.
func newSlackServerWithUsersInfo(t *testing.T, email string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"user": map[string]any{
					"id":   "U123",
					"name": "alice",
					"profile": map[string]any{
						"email": email,
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
}

// newSlackServerWithUsersInfoErr returns an httptest.Server that returns an
// error response to `users.info` so the rater resolver must fall back to a
// sentinel.
func newSlackServerWithUsersInfoErr(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    false,
				"error": "user_not_found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
}

// findRatingBeads returns all type=rating beads in the mock daemon.
func findRatingBeads(d *mockDaemon) []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Type == ratingBeadType {
			out = append(out, b)
		}
	}
	return out
}

func seedAgentBead(d *mockDaemon, id, project string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Type:   "agent",
		Fields: map[string]string{"project": project},
	}
}

// TestMapNativeToStars covers the score-mapping rule from the plan
// (-1→1, 0→3, +1→5) and confirms out-of-range native values are clamped.
func TestMapNativeToStars(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{-1, 1},
		{0, 3},
		{1, 5},
		// Clamp semantics: any value <= -1 maps to 1, any value >= 1 maps to 5.
		{-5, 1},
		{2, 5},
		{99, 5},
	}
	for _, tt := range tests {
		got := mapNativeToStars(tt.in)
		if got != tt.want {
			t.Errorf("mapNativeToStars(%d) = %d; want %d", tt.in, got, tt.want)
		}
	}
}

// TestRatingWrite_NoHumanSurveyBeadWritten is the cutover gate: post-T7b,
// the bridge must NEVER create a type=human_survey bead from a rating
// click. The single-write path produces only a type=rating bead.
func TestRatingWrite_NoHumanSurveyBeadWritten(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	srv := newSlackServerWithUsersInfo(t, "alice@example.com")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:1",
		humanSurveyCallback("U123", "C999"))

	var surveyCount int
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == humanSurveyBeadType {
			surveyCount++
		}
	}
	daemon.mu.Unlock()
	if surveyCount != 0 {
		t.Fatalf("post-cutover: expected 0 human_survey beads; got %d", surveyCount)
	}
	if got := findRatingBeads(daemon); len(got) != 1 {
		t.Fatalf("expected 1 rating bead; got %d", len(got))
	}
}

// TestRatingWrite_CreatesRatingBead verifies that a single rating click
// writes a type=rating bead with the correctly-mapped score, source, and
// slack-context fields.
func TestRatingWrite_CreatesRatingBead(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	srv := newSlackServerWithUsersInfo(t, "alice@example.com")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:1",
		humanSurveyCallback("U123", "C999"))

	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected 1 rating bead; got %d", len(ratings))
	}
	r := ratings[0]
	if r.Fields["rated_bead_id"] != "agent-xyz" {
		t.Errorf("rated_bead_id=%q; want agent-xyz", r.Fields["rated_bead_id"])
	}
	if r.Fields["rater"] != "alice@example.com" {
		t.Errorf("rater=%q; want alice@example.com", r.Fields["rater"])
	}
	if r.Fields["slack_user_id"] != "U123" {
		t.Errorf("slack_user_id=%q; want U123", r.Fields["slack_user_id"])
	}
	if r.Fields["slack_channel"] != "C999" {
		t.Errorf("slack_channel=%q; want C999", r.Fields["slack_channel"])
	}
	if r.Fields["score"] != "5" {
		t.Errorf("score=%q; want 5 (mapped from native +1)", r.Fields["score"])
	}
	if r.Fields["score_native"] != "1" {
		t.Errorf("score_native=%q; want 1", r.Fields["score_native"])
	}
	if r.Fields["source"] != "slack-stop" {
		t.Errorf("source=%q; want slack-stop", r.Fields["source"])
	}
	if r.Fields["answered_at"] == "" {
		t.Errorf("answered_at should be set; got empty")
	}
	// Project label propagated from agent bead.
	hasAgentLabel, hasProjectLabel := false, false
	for _, lbl := range r.Labels {
		if lbl == humanSurveyAgentLabelPrefix+"agent-xyz" {
			hasAgentLabel = true
		}
		if lbl == "project:gardenio" {
			hasProjectLabel = true
		}
	}
	if !hasAgentLabel {
		t.Errorf("missing agent label %q in %v", humanSurveyAgentLabelPrefix+"agent-xyz", r.Labels)
	}
	if !hasProjectLabel {
		t.Errorf("missing project:gardenio label in %v", r.Labels)
	}
}

// TestDualWrite_ScoreMapping_AllNativeValues exercises the full -1/0/+1
// scale through the actual click path so the mapping rule is verified
// end-to-end (not just in mapNativeToStars).
func TestDualWrite_ScoreMapping_AllNativeValues(t *testing.T) {
	tests := []struct {
		click       string // value passed to handleHumanSurveyRate
		wantScore   string
		wantNative  string
	}{
		{"agent-xyz:-1", "1", "-1"},
		{"agent-xyz:0", "3", "0"},
		{"agent-xyz:1", "5", "1"},
	}
	for _, tt := range tests {
		t.Run(tt.click, func(t *testing.T) {
			daemon := newMockDaemon()
			seedAgentBead(daemon, "agent-xyz", "gardenio")
			srv := newSlackServerWithUsersInfo(t, "alice@example.com")
			defer srv.Close()
			bot := newTestBot(daemon, srv)
		
			bot.handleHumanSurveyRate(context.Background(), tt.click,
				humanSurveyCallback("U123", "C999"))

			ratings := findRatingBeads(daemon)
			if len(ratings) != 1 {
				t.Fatalf("expected 1 rating bead; got %d", len(ratings))
			}
			r := ratings[0]
			if r.Fields["score"] != tt.wantScore {
				t.Errorf("score=%q; want %q", r.Fields["score"], tt.wantScore)
			}
			if r.Fields["score_native"] != tt.wantNative {
				t.Errorf("score_native=%q; want %q", r.Fields["score_native"], tt.wantNative)
			}
		})
	}
}

// TestDualWrite_IdentityResolver_Sentinel verifies that when users.info
// returns no email, the rater is the slack:<user-id> sentinel and the
// bead carries the slack_user_id for fallback lookups.
func TestDualWrite_IdentityResolver_Sentinel(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	// Empty email forces a sentinel rater.
	srv := newSlackServerWithUsersInfo(t, "")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:0",
		humanSurveyCallback("U123", "C999"))

	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected 1 rating bead; got %d", len(ratings))
	}
	r := ratings[0]
	wantRater := slackRaterSentinelPrefix + "U123"
	if r.Fields["rater"] != wantRater {
		t.Errorf("rater=%q; want %q (sentinel)", r.Fields["rater"], wantRater)
	}
	if r.Fields["slack_user_id"] != "U123" {
		t.Errorf("slack_user_id=%q; want U123 (must persist for fallback lookup)",
			r.Fields["slack_user_id"])
	}
}

// TestDualWrite_IdentityResolver_APIError covers the path where the Slack
// API returns an error response (vs. an empty email): rater must still
// fall back to the sentinel and lookup must not panic.
func TestDualWrite_IdentityResolver_APIError(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	srv := newSlackServerWithUsersInfoErr(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:0",
		humanSurveyCallback("U123", "C999"))

	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected 1 rating bead; got %d", len(ratings))
	}
	if got := ratings[0].Fields["rater"]; got != slackRaterSentinelPrefix+"U123" {
		t.Errorf("rater=%q; want %q (sentinel on API error)",
			got, slackRaterSentinelPrefix+"U123")
	}
}

// TestDualWrite_IdentityResolver_CachesEmail verifies the LRU cache: a
// second click for the same Slack user must not hit users.info again.
func TestDualWrite_IdentityResolver_CachesEmail(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	var usersInfoCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			usersInfoCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"user": map[string]any{
					"id":      "U123",
					"profile": map[string]any{"email": "alice@example.com"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	ctx := context.Background()
	cb := humanSurveyCallback("U123", "C999")
	bot.handleHumanSurveyRate(ctx, "agent-xyz:1", cb)
	bot.handleHumanSurveyRate(ctx, "agent-xyz:0", cb)
	bot.handleHumanSurveyRate(ctx, "agent-xyz:-1", cb)

	if got := usersInfoCalls.Load(); got != 1 {
		t.Errorf("users.info calls=%d; want 1 (LRU cache must hit on repeats)", got)
	}
}

// TestDualWrite_Idempotent_ReclickUpdates verifies that clicking the
// same agent_bead_id with a different rating from the same rater
// updates the existing rating bead in place — no duplicates.
func TestDualWrite_Idempotent_ReclickUpdates(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")
	srv := newSlackServerWithUsersInfo(t, "alice@example.com")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	ctx := context.Background()
	cb := humanSurveyCallback("U123", "C999")
	bot.handleHumanSurveyRate(ctx, "agent-xyz:1", cb)  // first click → score 5
	bot.handleHumanSurveyRate(ctx, "agent-xyz:-1", cb) // re-click → score 1

	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected exactly 1 rating bead after re-click; got %d", len(ratings))
	}
	r := ratings[0]
	if r.Fields["score"] != "1" {
		t.Errorf("score=%q; want 1 (updated)", r.Fields["score"])
	}
	if r.Fields["score_native"] != "-1" {
		t.Errorf("score_native=%q; want -1 (updated)", r.Fields["score_native"])
	}
}

// TestDualWrite_Idempotent_DifferentRaters_SeparateBeads verifies that
// two different Slack users produce two independent rating beads.
func TestDualWrite_Idempotent_DifferentRaters_SeparateBeads(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	// Stub users.info to return distinct emails per user id.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			user := r.FormValue("user")
			email := user + "@example.com"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"user": map[string]any{
					"id":      user,
					"profile": map[string]any{"email": email},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	ctx := context.Background()
	bot.handleHumanSurveyRate(ctx, "agent-xyz:1",
		humanSurveyCallback("U123", "C999"))
	bot.handleHumanSurveyRate(ctx, "agent-xyz:-1",
		humanSurveyCallback("U456", "C999"))

	ratings := findRatingBeads(daemon)
	if len(ratings) != 2 {
		t.Fatalf("expected 2 rating beads (one per rater); got %d", len(ratings))
	}
	seen := map[string]bool{}
	for _, r := range ratings {
		seen[r.Fields["rater"]] = true
	}
	if !seen["U123@example.com"] || !seen["U456@example.com"] {
		t.Errorf("expected raters {U123@example.com, U456@example.com}; got %v", seen)
	}
}

// TestDualWrite_SentinelToEmail_RewritesRater is the identity-drift
// recovery test: a first click landed when users.info failed (rater is
// the sentinel `slack:U123`); a second click resolves the email and
// must rewrite the existing bead's `rater` to the email — found via
// the slack_user_id fallback lookup.
func TestDualWrite_SentinelToEmail_RewritesRater(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	// Pre-seed an existing rating bead with the sentinel rater (as if the
	// first click happened when Slack was returning errors).
	daemon.mu.Lock()
	daemon.beads["rating-1"] = &beadsapi.BeadDetail{
		ID:   "rating-1",
		Type: ratingBeadType,
		Labels: []string{
			humanSurveyAgentLabelPrefix + "agent-xyz",
			"project:gardenio",
		},
		Fields: map[string]string{
			"rated_bead_id": "agent-xyz",
			"rater":         slackRaterSentinelPrefix + "U123",
			"slack_user_id": "U123",
			"score":         "5",
			"score_native":  "1",
			"source":        "slack-stop",
		},
	}
	daemon.mu.Unlock()

	// Now Slack works again — users.info returns the real email.
	srv := newSlackServerWithUsersInfo(t, "alice@example.com")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:-1",
		humanSurveyCallback("U123", "C999"))

	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected exactly 1 rating bead (sentinel→email rewrites in place); got %d",
			len(ratings))
	}
	r := ratings[0]
	if r.ID != "rating-1" {
		t.Errorf("expected the existing bead to be rewritten (id=rating-1); got %q", r.ID)
	}
	if r.Fields["rater"] != "alice@example.com" {
		t.Errorf("rater=%q; want alice@example.com (sentinel must be rewritten in place)",
			r.Fields["rater"])
	}
	// Updated score reflects the new click.
	if r.Fields["score"] != "1" {
		t.Errorf("score=%q; want 1 (updated)", r.Fields["score"])
	}
}

// TestDualWrite_EmailToSentinel_DoesNotRegress is the inverse safety
// check: if a bead already has an email rater, a later click that
// happens to fall back to the sentinel (e.g. transient API failure)
// must not regress `rater` back to slack:<id>.
func TestDualWrite_EmailToSentinel_DoesNotRegress(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	// Pre-existing bead with the resolved email rater.
	daemon.mu.Lock()
	daemon.beads["rating-1"] = &beadsapi.BeadDetail{
		ID:   "rating-1",
		Type: ratingBeadType,
		Labels: []string{
			humanSurveyAgentLabelPrefix + "agent-xyz",
			"project:gardenio",
		},
		Fields: map[string]string{
			"rated_bead_id": "agent-xyz",
			"rater":         "alice@example.com",
			"slack_user_id": "U123",
			"score":         "5",
			"score_native":  "1",
			"source":        "slack-stop",
		},
	}
	daemon.mu.Unlock()

	// users.info now fails — but we must not regress the rater field.
	srv := newSlackServerWithUsersInfoErr(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:0",
		humanSurveyCallback("U123", "C999"))

	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected 1 rating bead (sentinel-find via slack_user_id fallback); got %d",
			len(ratings))
	}
	r := ratings[0]
	if r.Fields["rater"] != "alice@example.com" {
		t.Errorf("rater=%q; want alice@example.com (must not regress to sentinel)",
			r.Fields["rater"])
	}
	// But the score (and other non-rater fields) does update.
	if r.Fields["score"] != "3" {
		t.Errorf("score=%q; want 3 (updated even when rater is sentinel)", r.Fields["score"])
	}
}

// TestRatingWrite_FeedbackOnly_FromExistingRating simulates the
// feedback-modal submit path on an agent that already has a rating
// click on file. The update branch of handleFeedbackSubmission must
// update the existing rating bead with the feedback field while
// preserving the prior score.
func TestRatingWrite_FeedbackOnly_FromExistingRating(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")
	// Pre-existing rating bead from a prior +1 click by the same rater.
	daemon.mu.Lock()
	daemon.beads["rating-1"] = &beadsapi.BeadDetail{
		ID:   "rating-1",
		Type: ratingBeadType,
		Labels: []string{
			humanSurveyAgentLabelPrefix + "agent-xyz",
			"project:gardenio",
		},
		Fields: map[string]string{
			"rated_bead_id": "agent-xyz",
			"rater":         "rater@example.com",
			"slack_user_id": "U-rater",
			"score":         "5",
			"score_native":  "1",
			"source":        "slack-stop",
		},
	}
	daemon.mu.Unlock()

	srv := newSlackServerWithUsersInfo(t, "rater@example.com")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	cb := slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		User: slack.User{ID: "U-rater"},
	}
	cb.View.CallbackID = humanSurveyFeedbackCallbackID
	cb.View.PrivateMetadata = "agent-xyz|C999|9998.0001"
	cb.View.State = &slack.ViewState{}
	cb.View.State.Values = map[string]map[string]slack.BlockAction{
		"feedback": {"feedback_input": slack.BlockAction{Value: "looked good"}},
	}

	bot.handleFeedbackSubmission(context.Background(), cb)

	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected 1 rating bead from feedback-only dual-write; got %d", len(ratings))
	}
	r := ratings[0]
	if r.Fields["score"] != "0" {
		t.Errorf("score=%q; want 0 (feedback-only path overwrites score with 0)", r.Fields["score"])
	}
	if r.Fields["source"] != "slack-feedback-only" {
		t.Errorf("source=%q; want slack-feedback-only", r.Fields["source"])
	}
	if r.Fields["feedback"] != "looked good" {
		t.Errorf("feedback=%q; want %q", r.Fields["feedback"], "looked good")
	}
	// score_native is preserved from the pre-existing rating click — the
	// feedback-only update branch does not touch it.
	if r.Fields["score_native"] != "1" {
		t.Errorf("score_native=%q; want %q (preserved from prior click)",
			r.Fields["score_native"], "1")
	}
	if r.Fields["rater"] != "rater@example.com" {
		t.Errorf("rater=%q; want rater@example.com", r.Fields["rater"])
	}
}

// TestDualWrite_FeedbackOnly_NewBead simulates the feedback-modal
// submit when no prior human_survey bead exists for the rater. The
// "create" branch of handleFeedbackSubmission must also dual-write
// (with the flag on) a rating bead.
func TestDualWrite_FeedbackOnly_NewBead(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")

	srv := newSlackServerWithUsersInfo(t, "rater@example.com")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	cb := slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		User: slack.User{ID: "U-rater"},
	}
	cb.View.CallbackID = humanSurveyFeedbackCallbackID
	cb.View.PrivateMetadata = "agent-xyz|C999|9998.0001"
	cb.View.State = &slack.ViewState{}
	cb.View.State.Values = map[string]map[string]slack.BlockAction{
		"feedback": {"feedback_input": slack.BlockAction{Value: "first feedback"}},
	}

	bot.handleFeedbackSubmission(context.Background(), cb)

	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected 1 rating bead from feedback-only create path; got %d", len(ratings))
	}
	r := ratings[0]
	if r.Fields["score"] != "0" {
		t.Errorf("score=%q; want 0", r.Fields["score"])
	}
	if r.Fields["source"] != "slack-feedback-only" {
		t.Errorf("source=%q; want slack-feedback-only", r.Fields["source"])
	}
	if r.Fields["feedback"] != "first feedback" {
		t.Errorf("feedback=%q; want %q", r.Fields["feedback"], "first feedback")
	}
}

// TestRaterIfNotSentinel guards the small helper that prevents
// findExistingRating from matching a sentinel value via the email path.
func TestRaterIfNotSentinel(t *testing.T) {
	if got := raterIfNotSentinel("alice@example.com", false); got != "alice@example.com" {
		t.Errorf("non-sentinel: got %q; want alice@example.com", got)
	}
	if got := raterIfNotSentinel(slackRaterSentinelPrefix+"U123", true); got != "" {
		t.Errorf("sentinel: got %q; want empty (so the email-path lookup is skipped)", got)
	}
}

// TestOperatorFromSlackUser_ResolvesEmail covers the happy-path: the
// helper stamps `email` (not the raw `slack:U…` id) so the operated_by
// column on downstream daemon writes records a stable identity.
func TestOperatorFromSlackUser_ResolvesEmail(t *testing.T) {
	daemon := newMockDaemon()
	srv := newSlackServerWithUsersInfo(t, "alice@example.com")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	got := bot.operatorFromSlackUser(context.Background(), "U123")
	// The users.info fallback must return the oidc:-prefixed email, not a
	// bare email. The daemon's isHumanOperator rejects bare emails with HTTP
	// 400 "operator could not be resolved", which silently kills mention-spawn
	// on boxes whose UserDirectory has no matching user bead (ka-0000001053).
	if got != "oidc:alice@example.com" {
		t.Errorf("operatorFromSlackUser=%q; want oidc:alice@example.com", got)
	}
}

// TestOperatorFromSlackUser_FallsBackToSentinel covers the failure path:
// when users.info returns no email the helper still produces a stable
// identity (the `slack:<user-id>` sentinel) so the operated_by column
// is never empty.
func TestOperatorFromSlackUser_FallsBackToSentinel(t *testing.T) {
	daemon := newMockDaemon()
	srv := newSlackServerWithUsersInfo(t, "")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	got := bot.operatorFromSlackUser(context.Background(), "U123")
	if got != slackRaterSentinelPrefix+"U123" {
		t.Errorf("operatorFromSlackUser=%q; want %q (sentinel)",
			got, slackRaterSentinelPrefix+"U123")
	}
}

// TestOperatorFromSlackUser_PrefersUserDirectory is the R3 regression
// test (gd-V1AHgYdDgQ). When the local UserDirectory has a fleet user
// bead matching the Slack id, the helper must return `oidc:<email>` —
// even before consulting the Slack users.info API. This mirrors the
// jira-bridge (ka-0000006070) and gitlab-bridge (ka-0000004627)
// attribution fixes; without it, slack-thread spawns stamp the
// `slack:<UID>` sentinel even when the human is fully resolved
// (fields.user_bead_id was set, fields.operated_by was not).
func TestOperatorFromSlackUser_PrefersUserDirectory(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a fleet user bead that maps slack_id → email.
	daemon.mu.Lock()
	daemon.beads["user-matt"] = &beadsapi.BeadDetail{
		ID:     "user-matt",
		Title:  "Matt Baker",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"email":    "matt@example.com",
			"slack_id": "U-MATT",
		},
	}
	daemon.mu.Unlock()

	// Slack server would return a different email if hit — assertion
	// below proves we resolved via UserDirectory, not Slack API.
	srv := newSlackServerWithUsersInfo(t, "wrong-email-from-slack@example.com")
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	if err := bot.UserDirectory().Refresh(context.Background()); err != nil {
		t.Fatalf("UserDirectory.Refresh: %v", err)
	}

	got := bot.operatorFromSlackUser(context.Background(), "U-MATT")
	if got != "oidc:matt@example.com" {
		t.Errorf("operatorFromSlackUser=%q; want oidc:matt@example.com (UserDirectory must win over Slack API)", got)
	}
}

// TestOperatorFromSlackUser_EmptyUserID covers the truly-anonymous case:
// no Slack user id at all → fleet:slack-bridge sentinel. R3 requires a
// non-empty operator on every write (ka-0000002887).
func TestOperatorFromSlackUser_EmptyUserID(t *testing.T) {
	daemon := newMockDaemon()
	srv := newSlackServerWithUsersInfo(t, "")
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	got := bot.operatorFromSlackUser(context.Background(), "")
	if got != "fleet:slack-bridge" {
		t.Errorf("operatorFromSlackUser(\"\")=%q; want fleet:slack-bridge", got)
	}
}
