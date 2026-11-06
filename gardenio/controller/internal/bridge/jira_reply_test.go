package bridge

// Tests for the agent-facing POST /api/jira/reply endpoint (feature F2, plan
// ka-0000007401). Reuses the squawk-forwarder mocks (mockJiraSquawkDaemon,
// mockJiraSquawkJira) since the endpoint shares their client subsets.
//
// The load-bearing FSM negatives (TestJiraReply_FSMHardBlocked,
// TestJiraReply_FSMBlockedEvenWithExplicitKey) landed here with F2 and now
// live in the consolidated F5 guardrails spine
// (jira_fsm_guardrails_spine_test.go, gd-LY6qQ2vEhQ), alongside their
// squawk-path counterparts. The helpers below (newJiraReplyAPI,
// enforcedWritePolicy, replyAgentDaemon, postJiraReply) are shared with that
// spine.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func newJiraReplyAPI(daemon *mockJiraSquawkDaemon, jira *mockJiraSquawkJira, policy *WritePolicyStore) *JiraReplyAPI {
	return NewJiraReplyAPI(JiraReplyAPIConfig{
		Daemon:      daemon,
		Jira:        jira,
		Logger:      jiraSquawkTestLogger(),
		WritePolicy: policy,
	})
}

func enforcedWritePolicy(outboxEnabled bool) *WritePolicyStore {
	return NewWritePolicyStore(DefaultWritePolicy(outboxEnabled), jiraSquawkTestLogger())
}

func replyAgentDaemon(agent, jiraKey, role string) *mockJiraSquawkDaemon {
	fields := map[string]string{"role": role}
	if jiraKey != "" {
		fields["jira_key"] = jiraKey
	}
	return &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			agent: {ID: "bd-" + agent, Title: agent, Fields: fields},
		},
	}
}

func postJiraReply(t *testing.T, api *JiraReplyAPI, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/jira/reply", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestJiraReply_PostsInternalCommentOnBoundTicket(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "Fix is up in MR !99."})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].Key != "PE-42" {
		t.Errorf("comment key = %q, want PE-42", comments[0].Key)
	}
	if comments[0].Vis != CommentInternal {
		t.Errorf("visibility = %q, want internal (the default)", comments[0].Vis)
	}
	if !strings.Contains(comments[0].Text, "crew-1") || !strings.Contains(comments[0].Text, "Fix is up") {
		t.Errorf("comment body missing agent/text: %q", comments[0].Text)
	}

	var resp jiraReplyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.JiraKey != "PE-42" {
		t.Errorf("response = %+v, want OK with jira_key PE-42", resp)
	}
}

func TestJiraReply_ThreadsSourceCommentIDFromAgentBead(t *testing.T) {
	// G4b quote-link fallback (ka-0000002864): a mention-spawned agent carries
	// jira_comment_id (F3), and the reply path must hand it to the
	// source-linked comment variant so the posted ADF anchors back to the
	// originating @gardenio mention comment.
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	daemon.agentBeads["crew-1"].Fields["jira_comment_id"] = "10123"
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "threaded reply"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].SourceCommentID != "10123" {
		t.Errorf("source comment id = %q, want 10123", comments[0].SourceCommentID)
	}
}

func TestJiraReply_NoSourceCommentIDDegradesToFlat(t *testing.T) {
	// Agents without jira_comment_id (poller-spawned, manual binding) must
	// post exactly the flat comment of before — empty anchor id through the
	// source-linked variant.
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "flat reply"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].SourceCommentID != "" {
		t.Errorf("source comment id = %q, want empty for an unthreaded agent", comments[0].SourceCommentID)
	}
}

func TestJiraReply_MatchingExplicitKeyAllowed(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	// Case-insensitive match against the bound key must pass.
	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", JiraKey: "pe-42", Text: "hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(jira.getComments()) != 1 {
		t.Fatal("expected the comment to be posted")
	}
}

func TestJiraReply_RejectsNonBoundTicket(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	// Naming a DIFFERENT ticket — even a writable one — must be refused: the
	// endpoint can only comment on the agent's own bound ticket.
	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", JiraKey: "DEVOPS-7", Text: "hi"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("comment must not be posted to a non-bound ticket")
	}
}

func TestJiraReply_RejectsUnboundAgent(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "hi"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an agent with no jira_key binding", rec.Code)
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("no comment may be posted without a binding")
	}
}

func TestJiraReply_UsesJiraKeyFromNotes(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"crew-1": {
				ID:     "bd-crew-1",
				Title:  "crew-1",
				Notes:  "jira_key: PE-77",
				Fields: map[string]string{"role": "crew"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "note-bound reply"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	comments := jira.getComments()
	if len(comments) != 1 || comments[0].Key != "PE-77" {
		t.Fatalf("expected 1 comment on PE-77, got %+v", comments)
	}
}

func TestJiraReply_PublicVisibilityDeniedByDefaultPolicy(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	// No role carries AllowPublicComment in the default policy, so a --public
	// reply must be refused even for a broad-tier role on a writable project.
	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "hi", Visibility: "public"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a public reply under the default policy", rec.Code)
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("public comment must not be posted")
	}
}

func TestJiraReply_PublicVisibilityAllowedWhenRoleGranted(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}

	// Tuned policy: crew explicitly granted public comments on PE.
	wp := DefaultWritePolicy(false)
	pub := allFieldsPolicy()
	pub.AllowPublicComment = true
	pe := wp.Projects["PE"]
	pe.Roles = map[string]RolePolicy{"crew": pub}
	wp.Projects["PE"] = pe
	api := newJiraReplyAPI(daemon, jira, NewWritePolicyStore(wp, jiraSquawkTestLogger()))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "hi", Visibility: "public"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	comments := jira.getComments()
	if len(comments) != 1 || comments[0].Vis != CommentPublic {
		t.Fatalf("expected 1 public comment, got %+v", comments)
	}
}

func TestJiraReply_RejectsUnknownRole(t *testing.T) {
	daemon := replyAgentDaemon("mystery-1", "PE-42", "not-a-role")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "mystery-1", Text: "hi"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an unknown role (fail closed)", rec.Code)
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("unknown role must not post")
	}
}

func TestJiraReply_RequestValidation(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	cases := []struct {
		name string
		req  jiraReplyRequest
		want int
	}{
		{"missing agent", jiraReplyRequest{Text: "hi"}, http.StatusBadRequest},
		{"missing text", jiraReplyRequest{Agent: "crew-1"}, http.StatusBadRequest},
		{"bogus visibility", jiraReplyRequest{Agent: "crew-1", Text: "hi", Visibility: "loud"}, http.StatusBadRequest},
		{"unknown agent", jiraReplyRequest{Agent: "ghost", Text: "hi"}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJiraReply(t, api, tc.req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("no invalid request may reach Jira")
	}

	// Wrong method.
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jira/reply", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}

func TestJiraReply_TruncatesLongText(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	long := strings.Repeat("x", jiraReplyMaxLen+500)
	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: long})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if !strings.Contains(comments[0].Text, "...") {
		t.Error("expected truncation marker in long comment")
	}
	// The header + attribution ride on top of the capped text; the text itself
	// must have been cut to the cap.
	if len(comments[0].Text) > jiraReplyMaxLen+200 {
		t.Errorf("comment length %d far exceeds cap %d", len(comments[0].Text), jiraReplyMaxLen)
	}
}

func TestJiraReply_GBFSMInternalAllowedWithOutboxOff(t *testing.T) {
	// The bug (ka-0000007628): a @gardenio mention on a GBFSM ticket spawns an
	// agent (role jira-mention), but the spawned agent's `gb jira reply` was
	// 403'd because the reply gate was still coupled to the outbox write toggle
	// (JIRA_OUTBOX_ENABLED, OFF in prod) — even though spawn had already been
	// decoupled (ka-0000001940). An internal reply is not an FSM/outbox write, so
	// it must succeed in BOTH toggle states. This asserts the prod configuration
	// exactly: outbox OFF.
	for _, outboxOn := range []bool{false, true} {
		name := "outbox-off"
		if outboxOn {
			name = "outbox-on"
		}
		t.Run(name, func(t *testing.T) {
			daemon := replyAgentDaemon("jira-gbfsm-1", "GBFSM-2", "jira-mention")
			jira := &mockJiraSquawkJira{}
			api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(outboxOn))

			rec := postJiraReply(t, api, jiraReplyRequest{Agent: "jira-gbfsm-1", Text: "on it — triaged, MR up."})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — a GBFSM internal reply must not be coupled to the outbox toggle (body: %s)",
					rec.Code, rec.Body.String())
			}
			comments := jira.getComments()
			if len(comments) != 1 || comments[0].Key != "GBFSM-2" {
				t.Fatalf("expected 1 comment on GBFSM-2, got %+v", comments)
			}
			if comments[0].Vis != CommentInternal {
				t.Errorf("visibility = %q, want internal (the default)", comments[0].Vis)
			}
		})
	}
}

func TestJiraReply_GBFSMPublicStillDeniedWithOutboxOff(t *testing.T) {
	// Decoupling reply from the outbox toggle must NOT weaken the public-comment
	// ceiling: no shipped role holds AllowPublicComment, so a --public reply on
	// GBFSM stays 403 regardless of the toggle.
	daemon := replyAgentDaemon("jira-gbfsm-1", "GBFSM-2", "jira-mention")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "jira-gbfsm-1", Text: "hi", Visibility: "public"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a public GBFSM reply must stay denied (the AllowPublicComment ceiling)", rec.Code)
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("public comment must not be posted")
	}
}

func TestJiraReply_UnknownProjectStillDenied(t *testing.T) {
	// An agent bound to a project absent from the policy (never Writable, so not
	// spawnable and not reply-eligible) is refused — the fail-closed posture is
	// preserved by the reply-scoped gates too.
	daemon := replyAgentDaemon("crew-1", "ZZ-9", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(true))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "hi"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an unknown (non-reply-eligible) project", rec.Code)
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("unknown-project reply must not be posted")
	}
}

func TestJiraReply_BearerTokenAuth(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := NewJiraReplyAPI(JiraReplyAPIConfig{
		Daemon:      daemon,
		Jira:        jira,
		Logger:      jiraSquawkTestLogger(),
		WritePolicy: enforcedWritePolicy(false),
		AgentToken:  "sekrit",
	})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	post := func(auth string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(jiraReplyRequest{Agent: "crew-1", Text: "hi"})
		req := httptest.NewRequest(http.MethodPost, "/api/jira/reply", bytes.NewReader(raw))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := post("Bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("unauthenticated requests must not post")
	}
	if rec := post("Bearer sekrit"); rec.Code != http.StatusOK {
		t.Errorf("valid token: status = %d, want 200", rec.Code)
	}
	if len(jira.getComments()) != 1 {
		t.Fatal("authenticated request should post")
	}
}

// TestJiraReply_ContentRejectionReturns422 is the handler half of ka-0000000661
// BUG 1: when Jira rejects the comment BODY (a 4xx — e.g. invalid ADF the
// renderer produced), the reply endpoint must return an actionable 422, NOT a
// blanket 502. A 502 reads as "the bridge is down" and pushes agents onto the
// direct-API fallback (advice ka-0000001317); a 422 tells the caller to
// downgrade to plain text and retry.
func TestJiraReply_ContentRejectionReturns422(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{err: &JiraAPIError{
		Method:     http.MethodPost,
		Path:       "/rest/api/3/issue/PE-42/comment",
		StatusCode: http.StatusBadRequest,
		Body:       `{"errorMessages":["INVALID_INPUT"]}`,
	}}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "**`x`** — nope"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a Jira content rejection (body: %s)", rec.Code, rec.Body.String())
	}
	var resp jiraReplyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "plain text") {
		t.Errorf("response = %+v, want an actionable 'retry as plain text' error", resp)
	}
}

// TestJiraReply_UpstreamOutageReturns502 is the complement: a genuine upstream
// failure (Jira 5xx, throttling, or a transport error) must still surface as a
// 502 so callers treat it as a transient outage worth retrying — the
// classification must NOT collapse everything to 422.
func TestJiraReply_UpstreamOutageReturns502(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"jira-500", &JiraAPIError{Method: http.MethodPost, Path: "/p", StatusCode: http.StatusInternalServerError}},
		{"jira-429", &JiraAPIError{Method: http.MethodPost, Path: "/p", StatusCode: http.StatusTooManyRequests}},
		{"transport", errTestTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
			jira := &mockJiraSquawkJira{err: tc.err}
			api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

			rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "hi"})
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 for %s (body: %s)", rec.Code, tc.name, rec.Body.String())
			}
		})
	}
}

// errTestTransport stands in for a non-Jira-API failure (transport error,
// timeout): it is NOT a *JiraAPIError, so classifyReplyPostError must fall
// through to 502.
var errTestTransport = errors.New("dial tcp: connection refused")

// -- Dedup guard (ka-0000007243, Slack threadReplyDeduper parity) --------------
//
// POST /api/jira/reply posts on a possibly customer-facing ticket, and
// classifyReplyPostError deliberately tells agents to RETRY on a transient
// failure. Without a dedup guard, a retry whose first attempt actually landed
// (a lost 200 between bridge and CLI) posts a DUPLICATE comment. These tests
// pin the endpoint-level contract: an identical retry posts exactly once and
// still returns success; a distinct reply is never collapsed; and a FAILED
// post releases its reservation so a legitimate retry still gets through.

// jiraReplyFailFirst wraps the shared mock to fail the first N posts with a
// transient error, then delegate to the real mock. It lets a test drive the
// "first attempt fails, agent retries" flow the dedup abort path guards.
type jiraReplyFailFirst struct {
	*mockJiraSquawkJira
	remainingFails int
	failErr        error
}

func (m *jiraReplyFailFirst) AddMarkdownCommentWithVisibilityAndSourceLink(ctx context.Context, key, text string, vis CommentVisibility, sourceCommentID string) error {
	if m.remainingFails > 0 {
		m.remainingFails--
		return m.failErr
	}
	return m.mockJiraSquawkJira.AddMarkdownCommentWithVisibilityAndSourceLink(ctx, key, text, vis, sourceCommentID)
}

func TestJiraReply_DuplicateRetryPostsOnce(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	req := jiraReplyRequest{Agent: "crew-1", Text: "Fix is up in MR !99."}

	// First post lands.
	if rec := postJiraReply(t, api, req); rec.Code != http.StatusOK {
		t.Fatalf("first reply status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// Identical retry (agent re-invoked / lost 200) is collapsed, still 200.
	rec := postJiraReply(t, api, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (collapsed, body: %s)", rec.Code, rec.Body.String())
	}
	var resp jiraReplyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.JiraKey != "PE-42" {
		t.Errorf("collapsed retry response = %+v, want OK with jira_key PE-42", resp)
	}
	if n := len(jira.getComments()); n != 1 {
		t.Fatalf("expected exactly 1 comment after a duplicate retry, got %d", n)
	}
}

func TestJiraReply_DistinctRepliesBothPost(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	if rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "first update"}); rec.Code != http.StatusOK {
		t.Fatalf("first reply status = %d, want 200", rec.Code)
	}
	if rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "second, different update"}); rec.Code != http.StatusOK {
		t.Fatalf("second reply status = %d, want 200", rec.Code)
	}
	if n := len(jira.getComments()); n != 2 {
		t.Fatalf("two distinct replies must both post, got %d comments", n)
	}
}

// TestJiraReply_FailedPostAllowsRetry proves the abort path: a transient
// failure releases the reservation so the agent's retry (which
// classifyReplyPostError invites) posts, rather than being swallowed by a slot
// that never produced a comment.
func TestJiraReply_FailedPostAllowsRetry(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	inner := &mockJiraSquawkJira{}
	jira := &jiraReplyFailFirst{
		mockJiraSquawkJira: inner,
		remainingFails:     1,
		failErr:            &JiraAPIError{Method: http.MethodPost, Path: "/p", StatusCode: http.StatusInternalServerError},
	}
	api := NewJiraReplyAPI(JiraReplyAPIConfig{
		Daemon:      daemon,
		Jira:        jira,
		Logger:      jiraSquawkTestLogger(),
		WritePolicy: enforcedWritePolicy(false),
	})

	req := jiraReplyRequest{Agent: "crew-1", Text: "Fix is up in MR !99."}

	// First attempt hits the transient failure -> 502, nothing posted.
	if rec := postJiraReply(t, api, req); rec.Code != http.StatusBadGateway {
		t.Fatalf("first attempt status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := len(jira.getComments()); n != 0 {
		t.Fatalf("failed post must not leave a comment, got %d", n)
	}
	// Retry: the reservation was released, so it posts.
	if rec := postJiraReply(t, api, req); rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (reservation released, body: %s)", rec.Code, rec.Body.String())
	}
	if n := len(jira.getComments()); n != 1 {
		t.Fatalf("retry after a failed post must land exactly 1 comment, got %d", n)
	}
}
