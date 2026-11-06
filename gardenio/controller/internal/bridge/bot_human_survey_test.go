package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// humanSurveyCallback builds a synthetic Slack InteractionCallback that mimics
// a user clicking a rating button in the agent's Slack thread.
func humanSurveyCallback(userID, channelID string) slack.InteractionCallback {
	cb := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: userID},
	}
	cb.Channel.ID = channelID
	cb.Message.Timestamp = "9999.0001"
	cb.Message.Msg.ThreadTimestamp = "9998.0001"
	return cb
}

func TestParseHumanSurveyValue(t *testing.T) {
	tests := []struct {
		in       string
		wantID   string
		wantRate int
		wantOK   bool
	}{
		{"gd-ABC:3", "gd-ABC", 3, true},
		{"gd-with-dashes:0", "gd-with-dashes", 0, true},
		{"gd-x:5", "gd-x", 5, true},
		{"gd-x:1", "gd-x", 1, true},
		{"gd-x:-1", "gd-x", -1, true},
		{"gd-with-dashes:-1", "gd-with-dashes", -1, true},
		{"malformed", "", 0, false},
		{":3", "", 0, false},
		{"gd-x:", "", 0, false},
		{"gd-x:abc", "", 0, false},
	}
	for _, tt := range tests {
		id, rate, ok := parseHumanSurveyValue(tt.in)
		if ok != tt.wantOK || id != tt.wantID || rate != tt.wantRate {
			t.Errorf("parseHumanSurveyValue(%q) = (%q, %d, %v); want (%q, %d, %v)",
				tt.in, id, rate, ok, tt.wantID, tt.wantRate, tt.wantOK)
		}
	}
}

func TestHandleHumanSurveyRate_HappyPath_CreatesBead(t *testing.T) {
	daemon := newMockDaemon()
	// Seed agent bead so project label is picked up.
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

	cb := humanSurveyCallback("U123", "C999")
	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:1", cb)

	var created *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == ratingBeadType {
			created = b
			break
		}
	}
	// Single-write cutover: no human_survey bead must be written.
	for _, b := range daemon.beads {
		if b.Type == humanSurveyBeadType {
			t.Errorf("single-write cutover: unexpected human_survey bead %q", b.ID)
		}
	}
	daemon.mu.Unlock()
	if created == nil {
		t.Fatalf("expected a rating bead to be created")
	}
	if created.Fields["score_native"] != "1" {
		t.Errorf("score_native=%q; want %q", created.Fields["score_native"], "1")
	}
	if created.Fields["score"] != "5" {
		t.Errorf("score=%q; want %q (mapped from native +1)", created.Fields["score"], "5")
	}
	if created.Fields["rated_bead_id"] != "agent-xyz" {
		t.Errorf("rated_bead_id=%q; want %q", created.Fields["rated_bead_id"], "agent-xyz")
	}
	if created.Fields["slack_user_id"] != "U123" {
		t.Errorf("slack_user_id=%q; want %q", created.Fields["slack_user_id"], "U123")
	}
	if created.Fields["slack_channel"] != "C999" {
		t.Errorf("slack_channel=%q; want %q", created.Fields["slack_channel"], "C999")
	}
	if created.Fields["source"] != "slack-stop" {
		t.Errorf("source=%q; want %q", created.Fields["source"], "slack-stop")
	}
	// Label must include the agent label for cheap lookup.
	hasAgentLabel, hasProjectLabel := false, false
	for _, lbl := range created.Labels {
		if lbl == "agent:agent-xyz" {
			hasAgentLabel = true
		}
		if lbl == "project:gardenio" {
			hasProjectLabel = true
		}
	}
	if !hasAgentLabel {
		t.Errorf("missing agent label, got %v", created.Labels)
	}
	if !hasProjectLabel {
		t.Errorf("missing project label, got %v", created.Labels)
	}
}

// TestHandleHumanSurveyRate_ScoreMarshaledAsJSONNumber is a regression test
// for the bug where the numeric rating was sent as a JSON string in the
// CreateBead payload. The kbeads daemon validates score / score_native as
// integer fields and would reject string-encoded values with HTTP 400.
func TestHandleHumanSurveyRate_ScoreMarshaledAsJSONNumber(t *testing.T) {
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

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:1",
		humanSurveyCallback("U123", "C999"))

	daemon.mu.Lock()
	raw := daemon.lastCreateBeadRawFields
	daemon.mu.Unlock()
	if len(raw) == 0 {
		t.Fatalf("expected CreateBead to receive a fields payload")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("fields payload not valid JSON: %v\nraw=%s", err, raw)
	}
	for _, key := range []string{"score", "score_native"} {
		v, ok := decoded[key]
		if !ok {
			t.Fatalf("%s field missing from CreateBead payload: %s", key, raw)
		}
		// JSON numbers decode into float64. String-encoded ints would
		// decode as string, which is exactly the bug this test guards.
		if _, ok := v.(float64); !ok {
			t.Fatalf("%s must be a JSON number; got %T (%v) in payload %s",
				key, v, v, raw)
		}
	}
}

func TestHandleHumanSurveyRate_IdempotentReclick_UpdatesExisting(t *testing.T) {
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

	ctx := context.Background()
	cb := humanSurveyCallback("U123", "C999")

	// First click: creates bead.
	bot.handleHumanSurveyRate(ctx, "agent-xyz:1", cb)
	// Second click from same user, different rating: should update, not duplicate.
	bot.handleHumanSurveyRate(ctx, "agent-xyz:-1", cb)

	var ratings []*beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == ratingBeadType {
			ratings = append(ratings, b)
		}
	}
	daemon.mu.Unlock()
	if len(ratings) != 1 {
		t.Fatalf("expected exactly 1 rating bead after re-click, got %d", len(ratings))
	}
	if ratings[0].Fields["score_native"] != "-1" {
		t.Errorf("score_native=%q; want %q (updated)", ratings[0].Fields["score_native"], "-1")
	}
}

func TestHandleHumanSurveyRate_DifferentRaters_CreateSeparateBeads(t *testing.T) {
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

	ctx := context.Background()
	bot.handleHumanSurveyRate(ctx, "agent-xyz:1",
		humanSurveyCallback("U123", "C999"))
	bot.handleHumanSurveyRate(ctx, "agent-xyz:0",
		humanSurveyCallback("U456", "C999"))

	count := 0
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == ratingBeadType {
			count++
		}
	}
	daemon.mu.Unlock()
	if count != 2 {
		t.Fatalf("expected 2 separate rating beads (one per rater), got %d", count)
	}
}

func TestHandleHumanSurveyRate_MalformedValue_NoBeadCreated(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "not-a-valid-value",
		humanSurveyCallback("U123", "C999"))

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == ratingBeadType || b.Type == humanSurveyBeadType {
			t.Fatalf("malformed value should not create a rating bead; got %+v", b)
		}
	}
}

func TestBuildHumanSurveyBlocks_ButtonRange(t *testing.T) {
	blocks := buildHumanSurveyBlocks("agent-xyz", -1, 1, "stop")
	// header, actions, hint
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}
	actions, ok := blocks[1].(*slack.ActionBlock)
	if !ok {
		t.Fatalf("block[1] must be ActionBlock, got %T", blocks[1])
	}
	if actions.Elements == nil {
		t.Fatalf("actions block has no elements")
	}
	// 3 rating buttons (-1, 0, +1) + 1 "Add feedback" button.
	if got := len(actions.Elements.ElementSet); got != 4 {
		t.Errorf("expected 4 action elements (3 rating buttons + feedback button), got %d", got)
	}
	// Last element is the feedback button.
	feedbackBtn, ok := actions.Elements.ElementSet[3].(*slack.ButtonBlockElement)
	if !ok {
		t.Fatalf("expected last element to be ButtonBlockElement, got %T", actions.Elements.ElementSet[3])
	}
	if feedbackBtn.ActionID != humanSurveyFeedbackActionID {
		t.Errorf("feedback button ActionID=%q; want %q", feedbackBtn.ActionID, humanSurveyFeedbackActionID)
	}
	if feedbackBtn.Value != "agent-xyz" {
		t.Errorf("feedback button Value=%q; want %q", feedbackBtn.Value, "agent-xyz")
	}
	if feedbackBtn.Style != "" {
		t.Errorf("feedback button Style=%q; want unstyled", feedbackBtn.Style)
	}
	if actions.BlockID != humanSurveyActionsBlockID {
		t.Errorf("stop source: BlockID=%q want %q", actions.BlockID, humanSurveyActionsBlockID)
	}

	// Button values must encode the integer rating (including negative
	// values) so parseHumanSurveyValue round-trips through the click.
	wantValues := []string{"agent-xyz:-1", "agent-xyz:0", "agent-xyz:1"}
	for i, want := range wantValues {
		btn, ok := actions.Elements.ElementSet[i].(*slack.ButtonBlockElement)
		if !ok {
			t.Fatalf("element[%d] must be ButtonBlockElement, got %T", i, actions.Elements.ElementSet[i])
		}
		if btn.Value != want {
			t.Errorf("button[%d].Value=%q; want %q", i, btn.Value, want)
		}
	}

	// Min-rating button is danger; max is primary; 0 is unstyled.
	first := actions.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if first.Style != slack.StyleDanger {
		t.Errorf("min-rating button style=%q; want danger", first.Style)
	}
	mid := actions.Elements.ElementSet[1].(*slack.ButtonBlockElement)
	if mid.Style != "" {
		t.Errorf("0 button style=%q; want unstyled", mid.Style)
	}
	last := actions.Elements.ElementSet[2].(*slack.ButtonBlockElement)
	if last.Style != slack.StylePrimary {
		t.Errorf("max-rating button style=%q; want primary", last.Style)
	}
}

func TestBuildHumanSurveyBlocks_ManualSourceBlockID(t *testing.T) {
	blocks := buildHumanSurveyBlocks("agent-xyz", -1, 1, "manual")
	actions, ok := blocks[1].(*slack.ActionBlock)
	if !ok {
		t.Fatalf("block[1] must be ActionBlock, got %T", blocks[1])
	}
	if actions.BlockID != humanSurveyActionsManualBlockID {
		t.Errorf("manual source: BlockID=%q want %q", actions.BlockID, humanSurveyActionsManualBlockID)
	}
}

func TestHumanSurveyButtonLabel(t *testing.T) {
	tests := []struct {
		n, min, max int
		want        string
	}{
		// Canonical −1/0/+1 scale gets emoji affordances.
		{-1, -1, 1, ":thumbsdown: −1"},
		{0, -1, 1, ":neutral_face: 0"},
		{1, -1, 1, ":thumbsup: +1"},
		// Other ranges fall back to signed integers so the sign is
		// unambiguous when the scale is non-standard.
		{0, 0, 5, "0"},
		{3, 0, 5, "+3"},
		{5, 0, 5, "+5"},
		{-2, -3, 3, "−2"},
		{2, -3, 3, "+2"},
	}
	for _, tt := range tests {
		got := humanSurveyButtonLabel(tt.n, tt.min, tt.max)
		if got != tt.want {
			t.Errorf("humanSurveyButtonLabel(%d, %d, %d) = %q; want %q",
				tt.n, tt.min, tt.max, got, tt.want)
		}
	}
}

func TestHandleHumanSurveyRate_NegativeRating_PersistedAsNumber(t *testing.T) {
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

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:-1",
		humanSurveyCallback("U123", "C999"))

	daemon.mu.Lock()
	raw := daemon.lastCreateBeadRawFields
	daemon.mu.Unlock()
	if len(raw) == 0 {
		t.Fatalf("expected CreateBead to receive a fields payload")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("fields payload not valid JSON: %v\nraw=%s", err, raw)
	}
	scoreNative, ok := decoded["score_native"]
	if !ok {
		t.Fatalf("score_native field missing from CreateBead payload: %s", raw)
	}
	rNum, ok := scoreNative.(float64)
	if !ok {
		t.Fatalf("score_native must be a JSON number; got %T (%v)", scoreNative, scoreNative)
	}
	if rNum != -1 {
		t.Fatalf("score_native=%v; want -1", rNum)
	}
}

// TestHandleHumanSurveyRate_ConfirmationIncludesFeedbackButton verifies
// that a rating-click confirmation includes an "Add feedback" button so
// the rater can follow up with free-text feedback after the rating is
// recorded. Without this button, the only way to leave feedback is to
// click the pre-rating feedback button BEFORE clicking a rating — counter-intuitive.
func TestHandleHumanSurveyRate_ConfirmationIncludesFeedbackButton(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["agent-xyz"] = &beadsapi.BeadDetail{
		ID:     "agent-xyz",
		Type:   "agent",
		Fields: map[string]string{"project": "gardenio"},
	}
	daemon.mu.Unlock()

	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:4",
		humanSurveyCallback("U123", "C999"))

	body := captured.bodyForPath("/chat.update")
	if body == "" {
		t.Fatal("expected a chat.update call after rating click")
	}
	// action_id for the feedback button — must be present on the
	// rewritten confirmation message so the rater can follow up.
	if !containsUnescaped(body, humanSurveyFeedbackActionID) {
		t.Errorf("expected feedback action_id %q in rewritten blocks; body=%s",
			humanSurveyFeedbackActionID, body)
	}
	// Value must be the agent bead ID (not empty), so the handler
	// openFeedbackModal receives it via action.Value.
	if !containsUnescaped(body, "agent-xyz") {
		t.Errorf("expected agent_bead_id in feedback button value; body=%s", body)
	}
	// Rating confirmation text should still be there.
	if !containsUnescaped(body, "Rating") {
		t.Errorf("expected rating confirmation text in blocks; body=%s", body)
	}
}

// TestHandleHumanSurveyRate_IdempotentReclick_OmitsFeedbackButtonWhenFeedbackExists
// verifies that if feedback has already been left on the existing survey
// bead, a re-click of the rating does not offer the "Add feedback"
// button again (no double-feedback UX).
func TestHandleHumanSurveyRate_IdempotentReclick_OmitsFeedbackButtonWhenFeedbackExists(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["agent-xyz"] = &beadsapi.BeadDetail{
		ID:     "agent-xyz",
		Type:   "agent",
		Fields: map[string]string{"project": "gardenio"},
	}
	// Pre-existing rating with feedback already left.
	daemon.beads["rating-1"] = &beadsapi.BeadDetail{
		ID:     "rating-1",
		Type:   ratingBeadType,
		Labels: []string{humanSurveyAgentLabelPrefix + "agent-xyz"},
		Fields: map[string]string{
			"rated_bead_id": "agent-xyz",
			"slack_user_id": "U123",
			"rater":         "slack:U123",
			"score":         "5",
			"score_native":  "1",
			"feedback":      "already submitted",
			"source":        "slack-stop",
		},
	}
	daemon.mu.Unlock()

	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.handleHumanSurveyRate(context.Background(), "agent-xyz:2",
		humanSurveyCallback("U123", "C999"))

	body := captured.bodyForPath("/chat.update")
	if body == "" {
		t.Fatal("expected a chat.update call after rating re-click")
	}
	// No feedback button because feedback already exists on the bead.
	if containsUnescaped(body, humanSurveyFeedbackActionID) {
		t.Errorf("feedback button should NOT appear when feedback already exists; body=%s", body)
	}
}

// containsUnescaped checks for substring presence tolerating URL
// percent-encoding (form bodies arrive with `+` for space etc, but
// action_id and agent_bead_id are plain ASCII that pass through).
func containsUnescaped(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// --- handlePostPrompt validation (ka-0000005949) ---

// captureSlackPostMessage spins up a fake Slack server that captures the form
// values posted to chat.postMessage so tests can assert what the bridge sent.
type captureSlackPostMessage struct {
	srv  *httptest.Server
	last url.Values // form values from the most recent chat.postMessage call
}

func newCaptureSlackPostMessage(t *testing.T) *captureSlackPostMessage {
	t.Helper()
	c := &captureSlackPostMessage{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat.postMessage") {
			_ = r.ParseForm()
			c.last = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C-X","ts":"1700000000.000900"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// newSurveyAPI returns a HumanSurveyAPI wired to the given fake Slack server.
func newSurveyAPI(srv *httptest.Server) *HumanSurveyAPI {
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	return NewHumanSurveyAPI(api, nil, nil, slog.Default())
}

// fakeAgentCardResolver implements AgentCardResolver for tests.
type fakeAgentCardResolver struct {
	cards map[string]MessageRef
}

func (f *fakeAgentCardResolver) AgentCardLocation(agent string) (string, string, bool) {
	if f == nil {
		return "", "", false
	}
	ref, ok := f.cards[agent]
	if !ok {
		return "", "", false
	}
	return ref.ChannelID, ref.Timestamp, true
}

// newSurveyAPIWithResolver wires HumanSurveyAPI with a card-location resolver.
func newSurveyAPIWithResolver(srv *httptest.Server, resolver AgentCardResolver) *HumanSurveyAPI {
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	return NewHumanSurveyAPI(api, nil, resolver, slog.Default())
}

// postPromptForm runs handlePostPrompt against an in-memory recorder and
// returns the response.
func postPromptForm(api *HumanSurveyAPI, body any) *httptest.ResponseRecorder {
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/slack/human-survey/prompt", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handlePostPrompt(w, req)
	return w
}

// TestHandlePostPrompt_AcceptsEmptyThreadTS: when the agent has no Slack
// thread (JIRA-spawned, see ka-0000005949) the bridge MUST accept an empty
// thread_ts and post non-threaded to the channel.
func TestHandlePostPrompt_AcceptsEmptyThreadTS(t *testing.T) {
	cap := newCaptureSlackPostMessage(t)
	api := newSurveyAPI(cap.srv)

	resp := postPromptForm(api, map[string]any{
		"agent_bead_id":    "gd-jira-1",
		"channel":          "C-FALLBACK",
		"thread_ts":        "",
		"rating_scale_max": 1,
		"min_rating":       -1,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with empty thread_ts, got %d body=%s", resp.Code, resp.Body.String())
	}
	if cap.last == nil {
		t.Fatalf("chat.postMessage was not called")
	}
	if got := cap.last.Get("channel"); got != "C-FALLBACK" {
		t.Errorf("channel=%q; want C-FALLBACK", got)
	}
	// thread_ts must NOT be sent when empty — slack.MsgOptionTS is skipped.
	if got := cap.last.Get("thread_ts"); got != "" {
		t.Errorf("thread_ts=%q; want empty (non-threaded post)", got)
	}
}

// TestHandlePostPrompt_ThreadedWhenTSProvided preserves the existing
// behavior — when thread_ts is supplied, the message threads under it.
func TestHandlePostPrompt_ThreadedWhenTSProvided(t *testing.T) {
	cap := newCaptureSlackPostMessage(t)
	api := newSurveyAPI(cap.srv)

	resp := postPromptForm(api, map[string]any{
		"agent_bead_id":    "gd-thread-1",
		"channel":          "C-THREAD",
		"thread_ts":        "1700000000.000001",
		"rating_scale_max": 1,
		"min_rating":       -1,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	if got := cap.last.Get("thread_ts"); got != "1700000000.000001" {
		t.Errorf("thread_ts=%q; want 1700000000.000001", got)
	}
}

// TestHandlePostPrompt_RejectsMissingChannelOrAgentID: validation requires
// agent_bead_id, plus a resolvable channel (either supplied or via the
// AgentCardResolver). With no resolver, missing channel → 400.
func TestHandlePostPrompt_RejectsMissingChannelOrAgentID(t *testing.T) {
	cap := newCaptureSlackPostMessage(t)
	api := newSurveyAPI(cap.srv)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing channel", map[string]any{"agent_bead_id": "gd-1"}},
		{"missing agent_bead_id", map[string]any{"channel": "C-X"}},
		{"both missing", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postPromptForm(api, tc.body)
			if resp.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", resp.Code)
			}
		})
	}
}

// TestHandlePostPrompt_ResolvesChannelFromAgentCard: when the request omits
// channel but the AgentCardResolver knows where the agent's card lives, the
// bridge MUST post the survey in that channel and thread under the card.
// This is the canonical fix for ka-0000002021 — surveys land where the
// operator already sees the agent (e.g. fsm-triage channel).
func TestHandlePostPrompt_ResolvesChannelFromAgentCard(t *testing.T) {
	cap := newCaptureSlackPostMessage(t)
	resolver := &fakeAgentCardResolver{
		cards: map[string]MessageRef{
			"gd-jira-fsm-1": {ChannelID: "C-FSM-TRIAGE", Timestamp: "1700000000.001234"},
		},
	}
	api := newSurveyAPIWithResolver(cap.srv, resolver)

	resp := postPromptForm(api, map[string]any{
		"agent_bead_id":    "gd-jira-fsm-1",
		"channel":          "", // agent has no thread coords
		"thread_ts":        "",
		"rating_scale_max": 1,
		"min_rating":       -1,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 with resolver-provided channel, got %d body=%s", resp.Code, resp.Body.String())
	}
	if cap.last == nil {
		t.Fatalf("chat.postMessage was not called")
	}
	if got := cap.last.Get("channel"); got != "C-FSM-TRIAGE" {
		t.Errorf("channel=%q; want C-FSM-TRIAGE (resolved from agent card)", got)
	}
	if got := cap.last.Get("thread_ts"); got != "1700000000.001234" {
		t.Errorf("thread_ts=%q; want 1700000000.001234 (threaded under card)", got)
	}
}

// TestHandlePostPrompt_ResolverMissBubbles400: when the request omits
// channel and the AgentCardResolver has no card for the agent, the bridge
// must return 400. Agent-side soft-skips on this signal.
func TestHandlePostPrompt_ResolverMissBubbles400(t *testing.T) {
	cap := newCaptureSlackPostMessage(t)
	resolver := &fakeAgentCardResolver{cards: map[string]MessageRef{}}
	api := newSurveyAPIWithResolver(cap.srv, resolver)

	resp := postPromptForm(api, map[string]any{
		"agent_bead_id": "gd-orphan",
		"channel":       "",
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when no card and no channel, got %d body=%s",
			resp.Code, resp.Body.String())
	}
}

// TestHandlePostPrompt_AgentSuppliedChannelWins: when the agent supplies a
// channel directly (thread-based agents), the resolver is not consulted.
func TestHandlePostPrompt_AgentSuppliedChannelWins(t *testing.T) {
	cap := newCaptureSlackPostMessage(t)
	resolver := &fakeAgentCardResolver{
		cards: map[string]MessageRef{
			"gd-thread-1": {ChannelID: "C-WRONG", Timestamp: "1700000000.000999"},
		},
	}
	api := newSurveyAPIWithResolver(cap.srv, resolver)

	resp := postPromptForm(api, map[string]any{
		"agent_bead_id": "gd-thread-1",
		"channel":       "C-RIGHT",
		"thread_ts":     "1700000000.000111",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	if got := cap.last.Get("channel"); got != "C-RIGHT" {
		t.Errorf("channel=%q; want C-RIGHT (agent-supplied wins over resolver)", got)
	}
	if got := cap.last.Get("thread_ts"); got != "1700000000.000111" {
		t.Errorf("thread_ts=%q; want 1700000000.000111", got)
	}
}
