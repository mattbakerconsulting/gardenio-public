package bridge

// Tests for the agent-facing GET /api/jira/thread endpoint (feature G1, epic
// ka-0000007401) — the jira-bridge analog of the slack-bridge thread read.
// Reuses the reply-endpoint helpers (replyAgentDaemon) and the F4 history
// fake (fakeJiraHistory).

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// errFakeJira simulates an upstream Jira API failure in thread-read tests.
var errFakeJira = errors.New("fake jira down")

func newJiraThreadAPI(daemon *mockJiraSquawkDaemon, history JiraCommentHistoryClient, agentToken string) *JiraReplyAPI {
	return NewJiraReplyAPI(JiraReplyAPIConfig{
		Daemon:     daemon,
		Jira:       &mockJiraSquawkJira{},
		History:    history,
		Logger:     jiraSquawkTestLogger(),
		AgentToken: agentToken,
	})
}

func getJiraThread(t *testing.T, api *JiraReplyAPI, query, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/jira/thread"+query, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeJiraThread(t *testing.T, rec *httptest.ResponseRecorder) jiraThreadResponse {
	t.Helper()
	var resp jiraThreadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	return resp
}

func TestJiraThread_ReturnsBoundTicketCommentsOldestFirst(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	// GetComments returns newest first; the endpoint must flip the order.
	history := &fakeJiraHistory{comments: []JiraCommentEntry{
		{ID: "3", Author: JiraUser{DisplayName: "Matt"}, Body: json.RawMessage(`"newest"`), Created: "2026-07-18T12:00:00.000+0000"},
		{ID: "2", Author: JiraUser{DisplayName: "Bot"}, Body: json.RawMessage(`"middle"`), Created: "2026-07-18T11:00:00.000+0000"},
		{ID: "1", Author: JiraUser{DisplayName: "Matt"}, Body: json.RawMessage(`"oldest"`), Created: "2026-07-18T10:00:00.000+0000"},
	}}
	api := newJiraThreadAPI(daemon, history, "")

	rec := getJiraThread(t, api, "?agent=crew-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeJiraThread(t, rec)
	if !resp.OK || resp.JiraKey != "PE-42" {
		t.Fatalf("resp = %+v, want ok on PE-42", resp)
	}
	if history.gotKey != "PE-42" {
		t.Errorf("fetched key = %q, want the agent's bound ticket PE-42", history.gotKey)
	}
	if len(resp.Comments) != 3 {
		t.Fatalf("got %d comments, want 3", len(resp.Comments))
	}
	if resp.Comments[0].Text != "oldest" || resp.Comments[2].Text != "newest" {
		t.Errorf("comments not oldest-first: %+v", resp.Comments)
	}
	if resp.Comments[0].Author != "Matt" || resp.Comments[0].ID != "1" {
		t.Errorf("comment fields not mapped: %+v", resp.Comments[0])
	}
}

func TestJiraThread_LimitParamForwardedAndCapped(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	history := &fakeJiraHistory{}
	api := newJiraThreadAPI(daemon, history, "")

	if rec := getJiraThread(t, api, "?agent=crew-1&limit=5", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if history.gotMax != 5 {
		t.Errorf("limit = %d, want 5", history.gotMax)
	}

	// Over-cap limits clamp instead of passing through.
	if rec := getJiraThread(t, api, "?agent=crew-1&limit=9999", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if history.gotMax != jiraThreadMaxLimit {
		t.Errorf("limit = %d, want cap %d", history.gotMax, jiraThreadMaxLimit)
	}

	// Default when unspecified.
	if rec := getJiraThread(t, api, "?agent=crew-1", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if history.gotMax != jiraThreadDefaultLimit {
		t.Errorf("limit = %d, want default %d", history.gotMax, jiraThreadDefaultLimit)
	}

	// Garbage limit is rejected.
	if rec := getJiraThread(t, api, "?agent=crew-1&limit=zero", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for non-numeric limit", rec.Code)
	}
}

func TestJiraThread_RejectsUnknownAgentAndMissingBinding(t *testing.T) {
	// Unknown agent → 404.
	api := newJiraThreadAPI(replyAgentDaemon("crew-1", "PE-42", "crew"), &fakeJiraHistory{}, "")
	if rec := getJiraThread(t, api, "?agent=ghost", ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown agent", rec.Code)
	}

	// Missing agent param → 400.
	if rec := getJiraThread(t, api, "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing agent", rec.Code)
	}

	// Agent with no jira_key binding → 403.
	api = newJiraThreadAPI(replyAgentDaemon("crew-2", "", "crew"), &fakeJiraHistory{}, "")
	if rec := getJiraThread(t, api, "?agent=crew-2", ""); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for unbound agent", rec.Code)
	}
}

func TestJiraThread_BearerTokenGate(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	api := newJiraThreadAPI(daemon, &fakeJiraHistory{}, "sekret")

	if rec := getJiraThread(t, api, "?agent=crew-1", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without token", rec.Code)
	}
	if rec := getJiraThread(t, api, "?agent=crew-1", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with wrong token", rec.Code)
	}
	if rec := getJiraThread(t, api, "?agent=crew-1", "sekret"); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with correct token", rec.Code)
	}
}

func TestJiraThread_NoHistoryClientAnswers501(t *testing.T) {
	api := newJiraThreadAPI(replyAgentDaemon("crew-1", "PE-42", "crew"), nil, "")
	rec := getJiraThread(t, api, "?agent=crew-1", "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when history client is unwired", rec.Code)
	}
}

func TestJiraThread_JiraKeyFallsBackToNotes(t *testing.T) {
	// Agents spawned before field stamping carry jira_key only in notes —
	// same fallback the reply endpoint honors.
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"crew-legacy": {
				ID:     "bd-crew-legacy",
				Title:  "crew-legacy",
				Fields: map[string]string{"role": "crew"},
				Notes:  "jira_key: DEVOPS-7",
			},
		},
	}
	history := &fakeJiraHistory{comments: []JiraCommentEntry{
		{ID: "1", Author: JiraUser{DisplayName: "Ops"}, Body: json.RawMessage(`"hello"`), Created: "2026-07-18T10:00:00.000+0000"},
	}}
	api := newJiraThreadAPI(daemon, history, "")

	rec := getJiraThread(t, api, "?agent=crew-legacy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if history.gotKey != "DEVOPS-7" {
		t.Errorf("fetched key = %q, want notes-bound DEVOPS-7", history.gotKey)
	}
}

func TestJiraThread_FetchErrorAnswers502(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	history := &fakeJiraHistory{err: errFakeJira}
	api := newJiraThreadAPI(daemon, history, "")

	rec := getJiraThread(t, api, "?agent=crew-1", "")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on Jira fetch failure", rec.Code)
	}
	resp := decodeJiraThread(t, rec)
	if resp.OK || !strings.Contains(resp.Error, "failed to fetch") {
		t.Errorf("resp = %+v, want fetch-failure error", resp)
	}
}
