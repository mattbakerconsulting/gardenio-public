package bridge

// Tests for the agent-facing GET /api/jira/issue endpoint (feature G6, epic
// ka-0000007401) — the ticket-BODY analog of GET /api/jira/thread. Reuses the
// reply-endpoint helpers (replyAgentDaemon) and mirrors the thread-read test
// shape.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeJiraIssueReader implements JiraIssueReader for issue-read tests.
type fakeJiraIssueReader struct {
	issue     *JiraIssue
	err       error
	gotKey    string
	browseURL string
}

func (f *fakeJiraIssueReader) GetIssue(_ context.Context, key string) (*JiraIssue, error) {
	f.gotKey = key
	return f.issue, f.err
}

// BrowseURL satisfies the optional browse-url interface handleGetIssue probes.
func (f *fakeJiraIssueReader) BrowseURL(key string) string {
	if f.browseURL == "" {
		return ""
	}
	return f.browseURL + "/browse/" + key
}

func newJiraIssueAPI(daemon *mockJiraSquawkDaemon, issues JiraIssueReader, agentToken string) *JiraReplyAPI {
	return NewJiraReplyAPI(JiraReplyAPIConfig{
		Daemon:     daemon,
		Jira:       &mockJiraSquawkJira{},
		Issues:     issues,
		Logger:     jiraSquawkTestLogger(),
		AgentToken: agentToken,
	})
}

func getJiraIssue(t *testing.T, api *JiraReplyAPI, query, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/jira/issue"+query, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeJiraIssue(t *testing.T, rec *httptest.ResponseRecorder) jiraIssueResponse {
	t.Helper()
	var resp jiraIssueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	return resp
}

// adfParagraphRaw builds a minimal ADF doc with one paragraph of plain text,
// marshalled to json.RawMessage for use as a JiraIssueFields.Description.
func adfParagraphRaw(t *testing.T, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(adfParagraph(text))
	if err != nil {
		t.Fatalf("marshal adf: %v", err)
	}
	return raw
}

func TestJiraIssue_ReturnsBoundTicketBody(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	reader := &fakeJiraIssueReader{
		browseURL: "https://example.atlassian.net",
		issue: &JiraIssue{
			Key: "PE-42",
			Fields: JiraIssueFields{
				Summary:     "Investigate the flaky login",
				Description: adfParagraphRaw(t, "Repro steps updated by the operator."),
				Status:      &JiraNamedRef{Name: "In Progress"},
				IssueType:   &JiraNamedRef{Name: "Bug"},
				Priority:    &JiraNamedRef{Name: "High"},
				Assignee:    &JiraUser{DisplayName: "Jane Doe"},
				Reporter:    &JiraUser{DisplayName: "Matt Baker"},
				Labels:      []string{"auth", "regression"},
				Parent:      &JiraParentRef{Key: "PE-1"},
				Created:     "2026-07-20T10:00:00.000+0000",
				Updated:     "2026-07-22T09:00:00.000+0000",
			},
		},
	}
	api := newJiraIssueAPI(daemon, reader, "")

	rec := getJiraIssue(t, api, "?agent=crew-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeJiraIssue(t, rec)
	if !resp.OK || resp.JiraKey != "PE-42" {
		t.Fatalf("resp = %+v, want ok on PE-42", resp)
	}
	if reader.gotKey != "PE-42" {
		t.Errorf("fetched key = %q, want the agent's bound ticket PE-42", reader.gotKey)
	}
	if resp.Summary != "Investigate the flaky login" {
		t.Errorf("summary = %q", resp.Summary)
	}
	if resp.Description != "Repro steps updated by the operator." {
		t.Errorf("description = %q, want ADF flattened to markdown", resp.Description)
	}
	if resp.Status != "In Progress" || resp.IssueType != "Bug" || resp.Priority != "High" {
		t.Errorf("named refs not mapped: %+v", resp)
	}
	if resp.Assignee != "Jane Doe" || resp.Reporter != "Matt Baker" {
		t.Errorf("users not mapped: assignee=%q reporter=%q", resp.Assignee, resp.Reporter)
	}
	if len(resp.Labels) != 2 || resp.Labels[0] != "auth" {
		t.Errorf("labels not mapped: %+v", resp.Labels)
	}
	if resp.Parent == nil || resp.Parent.Key != "PE-1" {
		t.Errorf("parent not mapped: %+v", resp.Parent)
	}
	if resp.BrowseURL != "https://example.atlassian.net/browse/PE-42" {
		t.Errorf("browse_url = %q", resp.BrowseURL)
	}
}

func TestJiraIssue_RejectsUnknownAgentAndMissingBinding(t *testing.T) {
	reader := &fakeJiraIssueReader{issue: &JiraIssue{Key: "PE-42"}}

	// Unknown agent → 404.
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	api := newJiraIssueAPI(daemon, reader, "")
	if rec := getJiraIssue(t, api, "?agent=ghost", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown agent: status = %d, want 404", rec.Code)
	}

	// Agent with no jira_key binding → 403.
	unbound := replyAgentDaemon("crew-2", "", "crew")
	api2 := newJiraIssueAPI(unbound, reader, "")
	if rec := getJiraIssue(t, api2, "?agent=crew-2", ""); rec.Code != http.StatusForbidden {
		t.Errorf("unbound agent: status = %d, want 403", rec.Code)
	}

	// Missing agent param → 400.
	if rec := getJiraIssue(t, api, "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing agent: status = %d, want 400", rec.Code)
	}
}

func TestJiraIssue_BearerTokenGate(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	reader := &fakeJiraIssueReader{issue: &JiraIssue{Key: "PE-42"}}
	api := newJiraIssueAPI(daemon, reader, "s3cr3t")

	if rec := getJiraIssue(t, api, "?agent=crew-1", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := getJiraIssue(t, api, "?agent=crew-1", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
	if rec := getJiraIssue(t, api, "?agent=crew-1", "s3cr3t"); rec.Code != http.StatusOK {
		t.Errorf("correct token: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestJiraIssue_NoReaderAnswers501(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	api := newJiraIssueAPI(daemon, nil, "")
	if rec := getJiraIssue(t, api, "?agent=crew-1", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("nil reader: status = %d, want 501", rec.Code)
	}
}

func TestJiraIssue_FetchErrorAnswers502(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	reader := &fakeJiraIssueReader{err: errFakeJira}
	api := newJiraIssueAPI(daemon, reader, "")
	if rec := getJiraIssue(t, api, "?agent=crew-1", ""); rec.Code != http.StatusBadGateway {
		t.Errorf("upstream error: status = %d, want 502", rec.Code)
	}
}

func TestJiraIssue_JiraKeyFallsBackToNotes(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "", "crew")
	// No jira_key field; supply it via the notes blob instead (same
	// key: value format ParseNotes honors on the thread/reply paths).
	daemon.agentBeads["crew-1"].Notes = "jira_key: DEVOPS-7"
	reader := &fakeJiraIssueReader{issue: &JiraIssue{Key: "DEVOPS-7", Fields: JiraIssueFields{Summary: "note-bound"}}}
	api := newJiraIssueAPI(daemon, reader, "")

	rec := getJiraIssue(t, api, "?agent=crew-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if reader.gotKey != "DEVOPS-7" {
		t.Errorf("fetched key = %q, want DEVOPS-7 from notes", reader.gotKey)
	}
}
