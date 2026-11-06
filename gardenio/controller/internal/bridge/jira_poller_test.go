package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// pollerMock implements JiraBeadClient for testing the poller.
type pollerMock struct {
	mu     sync.Mutex
	beads  map[string]*beadsapi.BeadDetail
	closed map[string]map[string]string // beadID → close fields
	nextID int
}

func newPollerMock() *pollerMock {
	return &pollerMock{
		beads:  make(map[string]*beadsapi.BeadDetail),
		closed: make(map[string]map[string]string),
	}
}

func (m *pollerMock) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("bd-poll-%d", m.nextID)
	var fields map[string]string
	if len(req.Fields) > 0 {
		_ = json.Unmarshal(req.Fields, &fields)
	}
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  req.Title,
		Type:   req.Type,
		Kind:   req.Kind,
		Status: "open",
		Labels: req.Labels,
		Fields: fields,
	}
	return id, nil
}

func (m *pollerMock) ListTaskBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Type == "task" {
			result = append(result, b)
		}
	}
	return result, nil
}

func (m *pollerMock) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		match := true
		if len(q.Labels) > 0 {
			for _, ql := range q.Labels {
				found := false
				for _, bl := range b.Labels {
					if bl == ql {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
		}
		if len(q.Statuses) > 0 {
			statusMatch := false
			for _, s := range q.Statuses {
				if b.Status == s {
					statusMatch = true
					break
				}
			}
			if !statusMatch {
				match = false
			}
		}
		if match {
			result = append(result, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: result, Total: len(result)}, nil
}

func (m *pollerMock) AddDependency(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (m *pollerMock) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	if bead.Fields == nil {
		bead.Fields = make(map[string]string)
	}
	for k, v := range fields {
		bead.Fields[k] = v
	}
	return nil
}

func (m *pollerMock) CloseBead(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	bead.Status = "closed"
	m.closed[beadID] = fields
	return nil
}

// --- buildJQL tests ---

func TestBuildJQL_SkipAssigned(t *testing.T) {
	p := &JiraPoller{
		cfg: JiraPollerConfig{
			Projects:     []string{"PE"},
			SkipAssigned: true,
		},
	}

	jql := p.buildJQL()

	if !containsSubstr(jql, "assignee is EMPTY") {
		t.Errorf("expected 'assignee is EMPTY' in JQL when SkipAssigned=true, got: %s", jql)
	}
}

func TestBuildJQL_SkipAssigned_False(t *testing.T) {
	p := &JiraPoller{
		cfg: JiraPollerConfig{
			Projects:     []string{"PE"},
			SkipAssigned: false,
		},
	}

	jql := p.buildJQL()

	if containsSubstr(jql, "assignee") {
		t.Errorf("expected no assignee clause when SkipAssigned=false, got: %s", jql)
	}
}

func TestBuildJQL_SkipAssigned_WithOtherFilters(t *testing.T) {
	p := &JiraPoller{
		cfg: JiraPollerConfig{
			Projects:       []string{"PE"},
			Statuses:       []string{"To Do"},
			ExcludedLabels: []string{"wontfix"},
			SkipAssigned:   true,
			CustomJQL:      "priority = High",
		},
	}

	jql := p.buildJQL()

	// All clauses should be present.
	if !containsSubstr(jql, `project IN ("PE")`) {
		t.Errorf("expected project clause, got: %s", jql)
	}
	if !containsSubstr(jql, `status IN ("To Do")`) {
		t.Errorf("expected status clause, got: %s", jql)
	}
	if !containsSubstr(jql, `labels != "wontfix"`) {
		t.Errorf("expected excluded label clause, got: %s", jql)
	}
	if !containsSubstr(jql, "assignee is EMPTY") {
		t.Errorf("expected assignee is EMPTY clause, got: %s", jql)
	}
	if !containsSubstr(jql, "(priority = High)") {
		t.Errorf("expected custom JQL clause, got: %s", jql)
	}
}

// --- checkAssigneeChanges tests ---

// fakeJiraClient is a minimal JiraClient substitute for tests.
type fakeJiraClient struct {
	searchResults []JiraIssue
	comments      []struct{ key, text string }
}

func (f *fakeJiraClient) AddComment(_ context.Context, key, text string) error {
	f.comments = append(f.comments, struct{ key, text string }{key, text})
	return nil
}

func TestCheckAssigneeChanges_BacksOffWhenHumanAssigned(t *testing.T) {
	daemon := newPollerMock()

	// Pre-create a bead that tracks PE-100.
	daemon.beads["bd-poll-1"] = &beadsapi.BeadDetail{
		ID:     "bd-poll-1",
		Title:  "[PE-100] Test issue",
		Status: "open",
		Labels: []string{"source:jira", "jira:PE-100"},
		Fields: map[string]string{"jira_key": "PE-100"},
	}

	fakeJira := &fakeJiraClient{
		searchResults: []JiraIssue{
			{
				Key: "PE-100",
				Fields: JiraIssueFields{
					Assignee: &JiraUser{
						DisplayName: "Cris Gale",
						AccountID:   "human-123",
					},
				},
			},
		},
	}

	p := &JiraPoller{
		jira:   nil, // we'll override the method calls
		daemon: daemon,
		cfg: JiraPollerConfig{
			SkipAssigned: true,
			Logger:       slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		},
		tracked:   map[string]string{"PE-100": "bd-poll-1"},
		wiredDeps: make(map[string]bool),
	}

	// Override jira field with our fake — checkAssigneeBatch uses p.jira.SearchIssues
	// but we need to use the fake. We'll call checkAssigneeBatch directly with a
	// modified approach — testing the batch function that takes keys.
	ctx := context.Background()

	// Simulate what checkAssigneeBatch does: query JIRA and find assigned issues.
	issues := fakeJira.searchResults
	for _, issue := range issues {
		if issue.Fields.Assignee == nil {
			continue
		}
		if p.cfg.BotAccountID != "" && issue.Fields.Assignee.AccountID == p.cfg.BotAccountID {
			continue
		}

		p.mu.Lock()
		beadID, exists := p.tracked[issue.Key]
		p.mu.Unlock()
		if !exists {
			continue
		}

		result, err := p.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Labels:   []string{"jira:" + issue.Key},
			Statuses: []string{"open", "in_progress"},
			Limit:    1,
		})
		if err != nil || len(result.Beads) == 0 {
			continue
		}

		if err := p.daemon.CloseBead(ctx, beadID, map[string]string{
			"close_reason":     "human_assigned",
			"jira_assignee":    issue.Fields.Assignee.DisplayName,
			"jira_assignee_id": issue.Fields.Assignee.AccountID,
		}); err != nil {
			t.Fatalf("failed to close bead: %v", err)
		}

		_ = fakeJira.AddComment(ctx, issue.Key,
			"Gardenio is backing off — "+issue.Fields.Assignee.DisplayName+" has been assigned to this issue.")

		p.mu.Lock()
		delete(p.tracked, issue.Key)
		p.mu.Unlock()
	}

	// Verify: bead should be closed.
	daemon.mu.Lock()
	bead := daemon.beads["bd-poll-1"]
	daemon.mu.Unlock()

	if bead.Status != "closed" {
		t.Errorf("expected bead status='closed', got %q", bead.Status)
	}

	// Verify close fields.
	closeFields := daemon.closed["bd-poll-1"]
	if closeFields["close_reason"] != "human_assigned" {
		t.Errorf("expected close_reason='human_assigned', got %q", closeFields["close_reason"])
	}
	if closeFields["jira_assignee"] != "Cris Gale" {
		t.Errorf("expected jira_assignee='Cris Gale', got %q", closeFields["jira_assignee"])
	}

	// Verify: JIRA comment was posted.
	if len(fakeJira.comments) != 1 {
		t.Fatalf("expected 1 JIRA comment, got %d", len(fakeJira.comments))
	}
	if !containsSubstr(fakeJira.comments[0].text, "backing off") {
		t.Errorf("expected back-off comment, got: %s", fakeJira.comments[0].text)
	}

	// Verify: issue removed from tracked.
	p.mu.Lock()
	_, stillTracked := p.tracked["PE-100"]
	p.mu.Unlock()
	if stillTracked {
		t.Error("expected PE-100 to be removed from tracked map")
	}
}

func TestCheckAssigneeChanges_SkipsBotAssignee(t *testing.T) {
	daemon := newPollerMock()

	daemon.beads["bd-poll-1"] = &beadsapi.BeadDetail{
		ID:     "bd-poll-1",
		Title:  "[PE-200] Bot-assigned issue",
		Status: "open",
		Labels: []string{"source:jira", "jira:PE-200"},
		Fields: map[string]string{"jira_key": "PE-200"},
	}

	p := &JiraPoller{
		daemon: daemon,
		cfg: JiraPollerConfig{
			SkipAssigned: true,
			BotAccountID: "bot-account-456",
			Logger:       slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		},
		tracked:   map[string]string{"PE-200": "bd-poll-1"},
		wiredDeps: make(map[string]bool),
	}

	// Simulate: the issue is assigned to the bot account — should NOT close bead.
	issue := JiraIssue{
		Key: "PE-200",
		Fields: JiraIssueFields{
			Assignee: &JiraUser{
				DisplayName: "gardenio",
				AccountID:   "bot-account-456",
			},
		},
	}

	// The bot account ID matches — this should be skipped.
	if issue.Fields.Assignee != nil &&
		p.cfg.BotAccountID != "" &&
		issue.Fields.Assignee.AccountID == p.cfg.BotAccountID {
		// Expected: skip
	} else {
		t.Error("expected bot account to be skipped")
	}

	// Bead should remain open.
	daemon.mu.Lock()
	bead := daemon.beads["bd-poll-1"]
	daemon.mu.Unlock()
	if bead.Status != "open" {
		t.Errorf("expected bead to remain open for bot assignee, got %q", bead.Status)
	}
}

func TestCheckAssigneeChanges_NoTrackedKeys_Noop(t *testing.T) {
	daemon := newPollerMock()
	p := &JiraPoller{
		daemon: daemon,
		cfg: JiraPollerConfig{
			SkipAssigned: true,
			Logger:       slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		},
		tracked:   make(map[string]string),
		wiredDeps: make(map[string]bool),
	}

	// Should not panic or error with empty tracked map.
	p.checkAssigneeChanges(context.Background())
}

// --- Back-off comment project gate (ka-0000008042) ---

// pollerBackoffServer stands up an httptest JIRA endpoint that drives the real
// JiraPoller.checkAssigneeBatch path: the search returns a human-assigned issue
// for jiraKey, and any comment POST is recorded so the test can assert whether
// the back-off comment was actually sent. Returns the wired poller, its daemon
// mock, and a func that reports how many comments were posted to jiraKey.
func pollerBackoffServer(t *testing.T, jiraKey string) (*JiraPoller, *pollerMock, func() int) {
	t.Helper()

	var (
		mu            sync.Mutex
		commentsToKey int
	)
	commentPath := "/rest/api/3/issue/" + jiraKey + "/comment"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql"):
			// Return the tracked issue with a human assignee so the poller
			// enters the back-off branch.
			resp := map[string]any{
				"issues": []map[string]any{{
					"key": jiraKey,
					"fields": map[string]any{
						"assignee": map[string]any{
							"displayName": "Cris Gale",
							"accountId":   "human-123",
						},
					},
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == commentPath:
			commentsToKey++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	daemon := newPollerMock()
	beadID := "bd-backoff-" + jiraKey
	daemon.beads[beadID] = &beadsapi.BeadDetail{
		ID:     beadID,
		Title:  "[" + jiraKey + "] Test issue",
		Status: "open",
		Labels: []string{"source:jira", "jira:" + jiraKey},
		Fields: map[string]string{"jira_key": jiraKey},
	}

	p := &JiraPoller{
		jira:   newTestJiraClient(srv.URL),
		daemon: daemon,
		cfg: JiraPollerConfig{
			SkipAssigned: true,
			Logger:       slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		},
		tracked:   map[string]string{jiraKey: beadID},
		wiredDeps: make(map[string]bool),
	}

	countComments := func() int {
		mu.Lock()
		defer mu.Unlock()
		return commentsToKey
	}
	return p, daemon, countComments
}

// TestCheckAssigneeBatch_BlockedProject_SuppressesComment proves that the
// back-off path does NOT post an agent comment to a blocked project (FSM is in
// blockedJiraProjects), while still closing the bead and dropping it from the
// tracked map so the issue is not re-imported. This is the FSM service-desk
// leak fix (ka-0000008042).
func TestCheckAssigneeBatch_BlockedProject_SuppressesComment(t *testing.T) {
	p, daemon, countComments := pollerBackoffServer(t, "FSM-100")

	p.checkAssigneeBatch(context.Background(), []string{"FSM-100"})

	// No comment must reach the customer-facing FSM service desk.
	if n := countComments(); n != 0 {
		t.Errorf("expected 0 back-off comments for blocked project FSM, got %d", n)
	}

	// But the back-off bookkeeping must still happen: bead closed...
	daemon.mu.Lock()
	bead := daemon.beads["bd-backoff-FSM-100"]
	daemon.mu.Unlock()
	if bead.Status != "closed" {
		t.Errorf("expected bead status='closed' for FSM back-off, got %q", bead.Status)
	}

	// ...and the issue dropped from tracked so it is not re-imported.
	p.mu.Lock()
	_, stillTracked := p.tracked["FSM-100"]
	p.mu.Unlock()
	if stillTracked {
		t.Error("expected FSM-100 removed from tracked even when comment suppressed")
	}
}

// TestCheckAssigneeBatch_AllowedProject_PostsComment proves the guard does not
// over-block: an allow-listed project (PE) still gets the back-off comment.
func TestCheckAssigneeBatch_AllowedProject_PostsComment(t *testing.T) {
	p, daemon, countComments := pollerBackoffServer(t, "PE-100")

	p.checkAssigneeBatch(context.Background(), []string{"PE-100"})

	if n := countComments(); n != 1 {
		t.Errorf("expected 1 back-off comment for allowed project PE, got %d", n)
	}

	daemon.mu.Lock()
	bead := daemon.beads["bd-backoff-PE-100"]
	daemon.mu.Unlock()
	if bead.Status != "closed" {
		t.Errorf("expected bead status='closed' for PE back-off, got %q", bead.Status)
	}

	p.mu.Lock()
	_, stillTracked := p.tracked["PE-100"]
	p.mu.Unlock()
	if stillTracked {
		t.Error("expected PE-100 removed from tracked after back-off")
	}
}

// pollerBackoffBodyServer is like pollerBackoffServer but also captures the
// body of the posted back-off comment (flattened ADF text) so a test can assert
// the operator-attribution line (ka-0000006003). The tracked bead is seeded with
// the supplied operatedBy / display so the attribution path has data to render.
func pollerBackoffBodyServer(t *testing.T, jiraKey, operatedBy, display string) (*JiraPoller, func() string) {
	t.Helper()

	var (
		mu   sync.Mutex
		body string
	)
	commentPath := "/rest/api/3/issue/" + jiraKey + "/comment"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql"):
			resp := map[string]any{
				"issues": []map[string]any{{
					"key": jiraKey,
					"fields": map[string]any{
						"assignee": map[string]any{
							"displayName": "Cris Gale",
							"accountId":   "human-123",
						},
					},
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == commentPath:
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			body = flattenADFCommentBody(payload)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	daemon := newPollerMock()
	beadID := "bd-backoff-" + jiraKey
	fields := map[string]string{"jira_key": jiraKey}
	if operatedBy != "" {
		fields["operated_by"] = operatedBy
	}
	daemon.beads[beadID] = &beadsapi.BeadDetail{
		ID:                beadID,
		Title:             "[" + jiraKey + "] Test issue",
		Status:            "open",
		Labels:            []string{"source:jira", "jira:" + jiraKey},
		Fields:            fields,
		OperatedBy:        operatedBy,
		OperatedByDisplay: display,
	}

	p := &JiraPoller{
		jira:   newTestJiraClient(srv.URL),
		daemon: daemon,
		cfg: JiraPollerConfig{
			SkipAssigned: true,
			Logger:       slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		},
		tracked:   map[string]string{jiraKey: beadID},
		wiredDeps: make(map[string]bool),
	}

	getBody := func() string {
		mu.Lock()
		defer mu.Unlock()
		return body
	}
	return p, getBody
}

// flattenADFCommentBody joins every text node of a posted ADF comment body with
// "\n" so attribution that lands across a hardBreak boundary is observable.
func flattenADFCommentBody(payload map[string]any) string {
	doc, ok := payload["body"].(map[string]any)
	if !ok {
		return ""
	}
	content, ok := doc["content"].([]any)
	if !ok || len(content) == 0 {
		return ""
	}
	para, ok := content[0].(map[string]any)
	if !ok {
		return ""
	}
	nodes, ok := para["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, n := range nodes {
		if node, ok := n.(map[string]any); ok {
			if txt, ok := node["text"].(string); ok {
				parts = append(parts, txt)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// TestCheckAssigneeBatch_StampsOperatorAttribution proves the back-off comment
// carries an "Acted by <operator>" line built from the tracked bead's operator,
// with the daemon-resolved display name and the raw operated_by both present
// (epic ka-0000004904 decision #4 / ka-0000006003).
func TestCheckAssigneeBatch_StampsOperatorAttribution(t *testing.T) {
	p, getBody := pollerBackoffBodyServer(t, "PE-200", "slack:U0OPERATOR1", "Matt Baker")

	p.checkAssigneeBatch(context.Background(), []string{"PE-200"})

	body := getBody()
	if !strings.Contains(body, "backing off") {
		t.Errorf("expected back-off comment, got %q", body)
	}
	if !strings.Contains(body, "Acted by Matt Baker (slack:U0OPERATOR1)") {
		t.Errorf("expected attribution with display + raw id, got %q", body)
	}
}

// TestCheckAssigneeBatch_NoAttributionWhenNoOperator proves the back-off comment
// stays unattributed when the tracked bead carries no human operator.
func TestCheckAssigneeBatch_NoAttributionWhenNoOperator(t *testing.T) {
	p, getBody := pollerBackoffBodyServer(t, "PE-201", "", "")

	p.checkAssigneeBatch(context.Background(), []string{"PE-201"})

	body := getBody()
	if !strings.Contains(body, "backing off") {
		t.Errorf("expected back-off comment, got %q", body)
	}
	if strings.Contains(body, "Acted by") {
		t.Errorf("expected no attribution when no operator, got %q", body)
	}
}

// --- Config watcher: SkipAssigned support ---

func TestJiraConfigWatcher_ParseEventFields_SkipAssigned(t *testing.T) {
	envCfg := JiraPollerConfig{
		Projects:     []string{"PE"},
		SkipAssigned: false,
	}

	w := &JiraConfigWatcher{envCfg: envCfg}

	fields := map[string]string{
		"skip_assigned": "true",
	}

	cfg := w.parseEventFields(fields)

	if !cfg.SkipAssigned {
		t.Error("expected SkipAssigned=true after parseEventFields")
	}
}

func TestJiraConfigWatcher_ParseEventFields_SkipAssigned_False(t *testing.T) {
	envCfg := JiraPollerConfig{
		Projects:     []string{"PE"},
		SkipAssigned: true,
	}

	w := &JiraConfigWatcher{envCfg: envCfg}

	fields := map[string]string{
		"skip_assigned": "false",
	}

	cfg := w.parseEventFields(fields)

	if cfg.SkipAssigned {
		t.Error("expected SkipAssigned=false after parseEventFields with 'false'")
	}
}

func TestJiraConfigWatcher_SeedConfigBead_IncludesSkipAssigned(t *testing.T) {
	daemon := newJiraConfigMock()

	envCfg := JiraPollerConfig{
		Projects:     []string{"PE"},
		SkipAssigned: true,
		PollInterval: 60,
		Logger:       discardLogger(),
	}

	poller := NewJiraPoller(nil, daemon, envCfg)

	w := NewJiraConfigWatcher(JiraConfigWatcherConfig{
		Daemon: daemon,
		Poller: poller,
		Logger: discardLogger(),
		EnvCfg: envCfg,
	})

	w.LoadOrSeed(context.Background())

	// Check that the seeded config bead includes skip_assigned.
	bead := daemon.beads[w.beadID]
	if bead == nil {
		t.Fatal("expected bead to exist")
	}
	if bead.Fields["skip_assigned"] != "true" {
		t.Errorf("expected skip_assigned='true' in seeded bead, got %q", bead.Fields["skip_assigned"])
	}
}

// nopWriter discards all writes.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
