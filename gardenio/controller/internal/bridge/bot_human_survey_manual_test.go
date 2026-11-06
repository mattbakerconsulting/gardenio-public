package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

func TestParseRateCommand(t *testing.T) {
	tests := []struct {
		in     string
		wantID string
		wantOK bool
	}{
		{"rate gd-ABC", "gd-ABC", true},
		{"RATE gd-ABC", "gd-ABC", true},             // case-insensitive keyword
		{"rate   gd-ABC   ", "gd-ABC", true},        // extra whitespace
		{"rate gd-ABC please take a look", "gd-ABC", true}, // trailing note ignored
		{"rate", "", false},                         // no arg
		{"rate  ", "", false},                       // whitespace-only arg
		{"", "", false},                             // empty
		{"kill gd-ABC", "", false},                  // not the rate keyword
		{"rategd-ABC", "", false},                   // not separated from keyword
		{"  rate  gd-ABC", "gd-ABC", true},          // leading whitespace
	}
	for _, tt := range tests {
		gotID, gotOK := parseRateCommand(tt.in)
		if gotID != tt.wantID || gotOK != tt.wantOK {
			t.Errorf("parseRateCommand(%q) = (%q, %v); want (%q, %v)",
				tt.in, gotID, gotOK, tt.wantID, tt.wantOK)
		}
	}
}

func TestHumanSurveyPromptExpired(t *testing.T) {
	// 2026-04-23T12:00:00Z
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	window := 168 * time.Hour

	tests := []struct {
		name    string
		ts      string
		wantExp bool
	}{
		{"empty is expired", "", true},
		{"malformed is expired", "not-a-ts", true},
		{"zero seconds is expired", "0.0000", true},
		{
			"1 hour ago is fresh",
			// now (1776945600) - 1h = 1776942000
			"1776942000.000100",
			false,
		},
		{
			"6 days ago is fresh",
			// now - 6d = 1776427200
			"1776427200.000100",
			false,
		},
		{
			"8 days ago is expired",
			// now - 8d = 1776254400
			"1776254400.000100",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := humanSurveyPromptExpired(tt.ts, now, window)
			if got != tt.wantExp {
				t.Errorf("humanSurveyPromptExpired(%q) = %v; want %v", tt.ts, got, tt.wantExp)
			}
		})
	}
}

// TestPostManualHumanSurveyPrompt_DedupSkipsFresh verifies that the manual
// trigger no-ops when the agent bead already has a recent
// human_survey_prompt_ts set.
func TestPostManualHumanSurveyPrompt_DedupSkipsFresh(t *testing.T) {
	daemon := newMockDaemon()

	// Fresh ts = 1 hour ago.
	freshTS := time.Now().Add(-1 * time.Hour).Unix()
	daemon.mu.Lock()
	daemon.beads["agent-xyz"] = &beadsapi.BeadDetail{
		ID:   "agent-xyz",
		Type: "agent",
		Fields: map[string]string{
			"project":                     "gardenio",
			humanSurveyPromptTSField:      tsFromUnix(freshTS),
		},
	}
	daemon.mu.Unlock()

	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.postManualHumanSurveyPrompt(context.Background(), "agent-xyz",
		"C999", "9998.0001", "U-rater")

	// No fields should have been updated on the agent bead (no new ts).
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	got := daemon.beads["agent-xyz"].Fields[humanSurveyPromptTSField]
	if got != tsFromUnix(freshTS) {
		t.Errorf("agent bead prompt_ts=%q; want original %q (dedup should no-op)",
			got, tsFromUnix(freshTS))
	}
	// No human_survey bead should have been created either.
	for _, b := range daemon.beads {
		if b.Type == humanSurveyBeadType {
			t.Errorf("expected no human_survey bead from dedup skip, got %+v", b)
		}
	}
}

// TestPostManualHumanSurveyPrompt_ExpiredRepostsAndPersists verifies that
// an expired prompt ts is overwritten by a freshly posted one.
func TestPostManualHumanSurveyPrompt_ExpiredRepostsAndPersists(t *testing.T) {
	daemon := newMockDaemon()

	// Expired ts = 8 days ago.
	expiredTS := time.Now().Add(-8 * 24 * time.Hour).Unix()
	daemon.mu.Lock()
	daemon.beads["agent-xyz"] = &beadsapi.BeadDetail{
		ID:   "agent-xyz",
		Type: "agent",
		Fields: map[string]string{
			"project":                     "gardenio",
			humanSurveyPromptTSField:      tsFromUnix(expiredTS),
		},
	}
	daemon.mu.Unlock()

	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.postManualHumanSurveyPrompt(context.Background(), "agent-xyz",
		"C999", "9998.0001", "U-rater")

	// The fake Slack server returns message_ts=1234.5678; the dedup
	// field should have been overwritten with that fresh value.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	got := daemon.beads["agent-xyz"].Fields[humanSurveyPromptTSField]
	if got == tsFromUnix(expiredTS) {
		t.Errorf("agent bead prompt_ts was not updated past expired=%q", got)
	}
	if got == "" {
		t.Errorf("agent bead prompt_ts is empty; expected fresh ts from fake Slack")
	}
}

// TestPostManualHumanSurveyPrompt_NoPriorPostsAndPersists verifies the
// happy path: agent bead has no human_survey_prompt_ts → post + persist.
func TestPostManualHumanSurveyPrompt_NoPriorPostsAndPersists(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["agent-xyz"] = &beadsapi.BeadDetail{
		ID:     "agent-xyz",
		Type:   "agent",
		Fields: map[string]string{"project": "gardenio"},
	}
	daemon.mu.Unlock()

	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.postManualHumanSurveyPrompt(context.Background(), "agent-xyz",
		"C999", "9998.0001", "U-rater")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	got := daemon.beads["agent-xyz"].Fields[humanSurveyPromptTSField]
	if got == "" {
		t.Errorf("expected human_survey_prompt_ts to be persisted after post; got empty")
	}
}

// TestHandleFeedbackSubmission_EscapesXSS verifies that user-supplied
// feedback containing Slack special syntax is HTML-escaped before being
// stored on the bead, so `<!channel>` / `<@Uxx>` / `<http://evil>` can't
// fire when the feedback is rendered back into Slack.
func TestHandleFeedbackSubmission_EscapesXSS(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["agent-xyz"] = &beadsapi.BeadDetail{
		ID:     "agent-xyz",
		Type:   "agent",
		Fields: map[string]string{"project": "gardenio"},
	}
	daemon.mu.Unlock()

	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	raw := "ping <!channel> and <@U123> and <http://evil.example>"
	cb := slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		User: slack.User{ID: "U-rater"},
	}
	cb.View.CallbackID = humanSurveyFeedbackCallbackID
	cb.View.PrivateMetadata = "agent-xyz|C999|9998.0001"
	cb.View.State = &slack.ViewState{}
	cb.View.State.Values = map[string]map[string]slack.BlockAction{
		"feedback": {"feedback_input": slack.BlockAction{Value: raw}},
	}

	bot.handleFeedbackSubmission(context.Background(), cb)

	// A feedback-only rating bead was created (no prior rating).
	var found *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == ratingBeadType {
			found = b
			break
		}
	}
	daemon.mu.Unlock()
	if found == nil {
		t.Fatalf("expected a rating bead to be created")
	}

	got := found.Fields["feedback"]
	// Escaped form must not contain raw `<` or `>`.
	for _, bad := range []string{"<!channel>", "<@U123>", "<http://evil.example>"} {
		if strings.Contains(got, bad) {
			t.Errorf("feedback field contains un-escaped Slack syntax %q: %q", bad, got)
		}
	}
	// And the raw text is preserved in feedback_raw for downstream use.
	if found.Fields["feedback_raw"] != raw {
		t.Errorf("feedback_raw=%q; want %q", found.Fields["feedback_raw"], raw)
	}
	// slack_user_id is recorded.
	if found.Fields["slack_user_id"] != "U-rater" {
		t.Errorf("slack_user_id=%q; want %q", found.Fields["slack_user_id"], "U-rater")
	}
	// fake Slack returns no email → rater is the sentinel form.
	if found.Fields["rater"] != "slack:U-rater" {
		t.Errorf("rater=%q; want %q", found.Fields["rater"], "slack:U-rater")
	}
	if found.Fields["source"] != "slack-feedback-only" {
		t.Errorf("source=%q; want %q", found.Fields["source"], "slack-feedback-only")
	}
}

// TestHandleFeedbackSubmission_UpdatesExistingBead verifies that
// submitting feedback for a (agent, rater) pair that already has a
// rating bead updates that bead's feedback field instead of creating a
// duplicate.
func TestHandleFeedbackSubmission_UpdatesExistingBead(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["agent-xyz"] = &beadsapi.BeadDetail{
		ID:     "agent-xyz",
		Type:   "agent",
		Fields: map[string]string{"project": "gardenio"},
	}
	// Pre-existing rating bead from a prior rating click.
	daemon.beads["rating-1"] = &beadsapi.BeadDetail{
		ID:     "rating-1",
		Type:   ratingBeadType,
		Labels: []string{humanSurveyAgentLabelPrefix + "agent-xyz"},
		Fields: map[string]string{
			"rated_bead_id": "agent-xyz",
			"slack_user_id": "U-rater",
			"rater":         "slack:U-rater",
			"score":         "5",
			"score_native":  "1",
			"source":        "slack-stop",
		},
	}
	daemon.mu.Unlock()

	srv := newFakeSlackServer(t)
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

	// Exactly one rating bead should still exist (the pre-existing one, updated).
	count := 0
	var rating *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == ratingBeadType {
			count++
			rating = b
		}
	}
	daemon.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected exactly 1 rating bead, got %d", count)
	}
	if rating.ID != "rating-1" {
		t.Errorf("updated bead=%q; want %q (existing bead)", rating.ID, "rating-1")
	}
	if rating.Fields["feedback"] != "looked good" {
		t.Errorf("feedback=%q; want %q", rating.Fields["feedback"], "looked good")
	}
	// score_native is preserved on feedback-only update (the branch
	// omits it from updateFields so the existing native score survives).
	if rating.Fields["score_native"] != "1" {
		t.Errorf("score_native=%q; want %q (preserved)", rating.Fields["score_native"], "1")
	}
}

// tsFromUnix returns a Slack-style ts string for a given unix second.
func tsFromUnix(secs int64) string {
	return timeTS(secs)
}

// timeTS renders a Slack ts string "<seconds>.000100".
func timeTS(secs int64) string {
	return itoa64(secs) + ".000100"
}

func itoa64(n int64) string {
	// Small helper: avoid importing strconv just for this.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

