package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// newAgentFeedbackAPI builds an AgentFeedbackAPI backed by the shared
// mockDaemon and a discard logger.
func newAgentFeedbackAPI(d *mockDaemon) *AgentFeedbackAPI {
	return NewAgentFeedbackAPI(d, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// postFeedback issues a POST against the handler and returns the recorder.
func postFeedback(a *AgentFeedbackAPI, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/slack/agent-feedback", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	a.handlePostFeedback(rec, req)
	return rec
}

// TestAgentFeedback_WritesStableBead is the core always-on path: a feedback
// POST emits a type=rating bead carrying the feedback (XSS-escaped), the
// kind:slack-feedback label, the agent label, the project label, and the
// rated_bead_id provenance — independent of any survey config.
func TestAgentFeedback_WritesStableBead(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")
	api := newAgentFeedbackAPI(daemon)

	rec := postFeedback(api, agentFeedbackRequest{
		AgentBeadID:  "agent-xyz",
		Feedback:     "the spawn flow is great",
		SlackUserID:  "U123",
		SlackChannel: "C999",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q; want 200", rec.Code, rec.Body.String())
	}
	ratings := findRatingBeads(daemon)
	if len(ratings) != 1 {
		t.Fatalf("expected 1 rating bead; got %d", len(ratings))
	}
	r := ratings[0]

	if r.Type != ratingBeadType {
		t.Errorf("type=%q; want %q", r.Type, ratingBeadType)
	}
	if r.Fields["feedback"] != "the spawn flow is great" {
		t.Errorf("feedback=%q; want the spawn flow is great", r.Fields["feedback"])
	}
	if r.Fields["feedback_raw"] != "the spawn flow is great" {
		t.Errorf("feedback_raw=%q; want unescaped original", r.Fields["feedback_raw"])
	}
	if r.Fields["rated_bead_id"] != "agent-xyz" {
		t.Errorf("rated_bead_id=%q; want agent-xyz", r.Fields["rated_bead_id"])
	}
	if r.Fields["source"] != agentFeedbackSource {
		t.Errorf("source=%q; want %q", r.Fields["source"], agentFeedbackSource)
	}
	if r.Fields["score"] != "0" {
		t.Errorf("score=%q; want 0 (feedback-only sentinel)", r.Fields["score"])
	}
	if r.Fields["slack_user_id"] != "U123" {
		t.Errorf("slack_user_id=%q; want U123", r.Fields["slack_user_id"])
	}
	if r.Fields["rater"] != slackRaterSentinelPrefix+"U123" {
		t.Errorf("rater=%q; want %q", r.Fields["rater"], slackRaterSentinelPrefix+"U123")
	}
	if r.Fields["answered_at"] == "" {
		t.Errorf("answered_at must be set")
	}

	assertHasLabel(t, r.Labels, agentFeedbackLabel)
	assertHasLabel(t, r.Labels, humanSurveyAgentLabelPrefix+"agent-xyz")
	assertHasLabel(t, r.Labels, "project:gardenio")
}

// TestAgentFeedback_XSSEscaped verifies Slack special syntax in feedback is
// HTML-escaped in the `feedback` field while `feedback_raw` keeps the
// original, matching the modal path's behaviour.
func TestAgentFeedback_XSSEscaped(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")
	api := newAgentFeedbackAPI(daemon)

	raw := "ping <!channel> and <@U999>"
	rec := postFeedback(api, agentFeedbackRequest{
		AgentBeadID: "agent-xyz",
		Feedback:    raw,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; want 200", rec.Code)
	}
	r := findRatingBeads(daemon)[0]
	if strings.Contains(r.Fields["feedback"], "<!channel>") || strings.Contains(r.Fields["feedback"], "<@U999>") {
		t.Errorf("feedback not escaped: %q", r.Fields["feedback"])
	}
	if r.Fields["feedback_raw"] != raw {
		t.Errorf("feedback_raw=%q; want original unescaped %q", r.Fields["feedback_raw"], raw)
	}
}

// TestAgentFeedback_ExplicitRaterWins verifies an explicit rater (e.g. an
// authenticated viewer) takes precedence over the slack sentinel.
func TestAgentFeedback_ExplicitRaterWins(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")
	api := newAgentFeedbackAPI(daemon)

	postFeedback(api, agentFeedbackRequest{
		AgentBeadID: "agent-xyz",
		Feedback:    "looks good",
		Rater:       "oidc:alice@example.com",
		SlackUserID: "U123",
	})
	r := findRatingBeads(daemon)[0]
	if r.Fields["rater"] != "oidc:alice@example.com" {
		t.Errorf("rater=%q; want explicit oidc:alice@example.com", r.Fields["rater"])
	}
}

// TestAgentFeedback_AnonymousRater verifies the anonymous fallback when no
// identity is supplied — the bead must still be writable.
func TestAgentFeedback_AnonymousRater(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")
	api := newAgentFeedbackAPI(daemon)

	postFeedback(api, agentFeedbackRequest{
		AgentBeadID: "agent-xyz",
		Feedback:    "anon note",
	})
	r := findRatingBeads(daemon)[0]
	if r.Fields["rater"] != agentFeedbackAnonRater {
		t.Errorf("rater=%q; want %q", r.Fields["rater"], agentFeedbackAnonRater)
	}
}

// TestAgentFeedback_Validation covers the required-field and method gates.
func TestAgentFeedback_Validation(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBead(daemon, "agent-xyz", "gardenio")
	api := newAgentFeedbackAPI(daemon)

	t.Run("missing agent_bead_id", func(t *testing.T) {
		rec := postFeedback(api, agentFeedbackRequest{Feedback: "x"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status=%d; want 400", rec.Code)
		}
	})
	t.Run("blank feedback", func(t *testing.T) {
		rec := postFeedback(api, agentFeedbackRequest{AgentBeadID: "agent-xyz", Feedback: "   "})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status=%d; want 400", rec.Code)
		}
	})
	t.Run("wrong method", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/slack/agent-feedback", nil)
		rec := httptest.NewRecorder()
		api.handlePostFeedback(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status=%d; want 405", rec.Code)
		}
	})

	if got := len(findRatingBeads(daemon)); got != 0 {
		t.Errorf("no bead should be written on validation failure; got %d", got)
	}
}

// TestAgentFeedback_SourceBeadNotFound verifies a 404 when the source agent
// bead does not exist. Uses a nil-returning daemon (the shared mockDaemon
// returns a synthetic bead for unknown ids).
func TestAgentFeedback_SourceBeadNotFound(t *testing.T) {
	api := NewAgentFeedbackAPI(nilGetDaemon{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := postFeedback(api, agentFeedbackRequest{AgentBeadID: "missing", Feedback: "x"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d; want 404", rec.Code)
	}
}

// nilGetDaemon is an AgentFeedbackAPIClient whose GetBead returns (nil,nil)
// so the handler's not-found branch is exercised.
type nilGetDaemon struct{}

func (nilGetDaemon) GetBead(context.Context, string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (nilGetDaemon) CreateBead(context.Context, beadsapi.CreateBeadRequest) (string, error) {
	return "", nil
}

func assertHasLabel(t *testing.T, labels []string, want string) {
	t.Helper()
	if !slices.Contains(labels, want) {
		t.Errorf("labels %v missing %q", labels, want)
	}
}
