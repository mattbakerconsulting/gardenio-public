package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fixtureJiraServer is a more realistic JIRA mock server that:
// - Routes to the correct API endpoint (search, get issue)
// - Validates JQL in search requests
// - Returns fixture data from JSON files or programmatic responses
// - Tracks which requests were made (for assertion)
// - Supports simulating errors and rate limits
type fixtureJiraServer struct {
	mu       sync.Mutex
	server   *httptest.Server
	requests []jiraRequest

	// Routing: JQL pattern → fixture file
	searchRoutes map[string]searchRoute

	// Default behavior for unmatched searches
	defaultSearchFixture string

	// Error simulation
	forceError     bool
	forceErrorCode int
	forceErrorBody string

	// Rate limit simulation
	rateLimitAfter int // return 429 after this many requests (0 = unlimited)
	requestCount   int
}

type jiraRequest struct {
	Method string
	Path   string
	JQL    string
	Fields string
}

type searchRoute struct {
	fixtureFile string // path relative to testdata/
}

func newFixtureJiraServer() *fixtureJiraServer {
	s := &fixtureJiraServer{
		searchRoutes: make(map[string]searchRoute),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *fixtureJiraServer) Close() {
	s.server.Close()
}

func (s *fixtureJiraServer) URL() string {
	return s.server.URL
}

// OnSearch registers a fixture file to return when a search JQL contains the given substring.
func (s *fixtureJiraServer) OnSearch(jqlContains string, fixtureFile string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchRoutes[jqlContains] = searchRoute{fixtureFile: fixtureFile}
}

// Requests returns all requests made to the server.
func (s *fixtureJiraServer) Requests() []jiraRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]jiraRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *fixtureJiraServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requestCount++
	count := s.requestCount

	// Rate limit simulation.
	if s.rateLimitAfter > 0 && count > s.rateLimitAfter {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errorMessages":["Rate limit exceeded"]}`))
		return
	}

	// Error simulation.
	if s.forceError {
		code := s.forceErrorCode
		body := s.forceErrorBody
		s.mu.Unlock()
		if code == 0 {
			code = http.StatusInternalServerError
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
		return
	}
	s.mu.Unlock()

	// Route based on path.
	switch {
	case strings.HasPrefix(r.URL.Path, "/rest/api/3/search"):
		s.handleSearch(w, r)
	case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
		s.handleGetIssue(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"errorMessages":["Not found: %s"]}`, r.URL.Path)
	}
}

func (s *fixtureJiraServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	jql := r.URL.Query().Get("jql")
	fields := r.URL.Query().Get("fields")

	s.mu.Lock()
	s.requests = append(s.requests, jiraRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		JQL:    jql,
		Fields: fields,
	})

	// Find matching route.
	var matched *searchRoute
	for pattern, route := range s.searchRoutes {
		if strings.Contains(jql, pattern) {
			r := route
			matched = &r
			break
		}
	}

	defaultFixture := s.defaultSearchFixture
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if matched != nil {
		data, err := loadFixture(matched.fixtureFile)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"errorMessages":["fixture load error: %s"]}`, err)
			return
		}
		_, _ = w.Write(data)
		return
	}

	// Use default fixture.
	if defaultFixture != "" {
		data, err := loadFixture(defaultFixture)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"errorMessages":["default fixture load error: %s"]}`, err)
			return
		}
		_, _ = w.Write(data)
		return
	}

	// No route matched — return empty.
	_, _ = w.Write([]byte(`{"issues":[],"maxResults":50,"startAt":0,"total":0}`))
}

func (s *fixtureJiraServer) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
	s.mu.Lock()
	s.requests = append(s.requests, jiraRequest{
		Method: r.Method,
		Path:   r.URL.Path,
	})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"key":"%s","id":"99999","fields":{"summary":"Mocked issue %s"}}`, key, key)
}

// loadFixture reads a fixture file from the testdata directory.
func loadFixture(name string) ([]byte, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	testdataDir := filepath.Join(filepath.Dir(thisFile), "testdata")
	return os.ReadFile(filepath.Join(testdataDir, name))
}

// --- Integration Tests ---

// TestIntegration_FSMEscalationTriage tests the full flow of:
// FSM ticket escalated → trigger evaluates JQL → 3 tickets found → agents spawned with realistic data
func TestIntegration_FSMEscalationTriage(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	jira.OnSearch("project = FSM", "jira_search_fsm_escalations.json")

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trig-fsm-1",
				Type:   "trigger",
				Status: "open",
				Title:  "Triage FSM escalations",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = FSM AND status = 'Escalate to Engineering' ORDER BY created DESC",
					"action":         "spawn_agent",
					"role":           "crew",
					"project":        "monorepo",
					"prompt":         "Triage {{jira_key}}: {{jira_summary}}. Priority: {{jira_priority}}. Reporter: {{jira_reporter}}.",
					"enabled":        "true",
					"max_concurrent": "10",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// Should spawn 3 agents (one per FSM ticket in the fixture).
	if len(daemon.spawnedNames) != 3 {
		t.Fatalf("expected 3 spawns for 3 FSM escalations, got %d", len(daemon.spawnedNames))
	}

	// Verify beads were created with realistic data.
	if len(daemon.createdBeads) != 3 {
		t.Fatalf("expected 3 created beads, got %d", len(daemon.createdBeads))
	}

	// Verify first bead (FSM-1042) has correct title.
	bead0 := daemon.createdBeads[0]
	if bead0.Title != "[FSM-1042] Unable to access study data export - timeout error for sponsor admin users" {
		t.Errorf("bead[0] title = %q", bead0.Title)
	}

	// Verify labels include source:jira and project:monorepo.
	hasLabel := func(labels []string, want string) bool {
		for _, l := range labels {
			if l == want {
				return true
			}
		}
		return false
	}
	if !hasLabel(bead0.Labels, "source:jira") {
		t.Error("bead[0] missing source:jira label")
	}
	if !hasLabel(bead0.Labels, "project:monorepo") {
		t.Error("bead[0] missing project:monorepo label")
	}

	// Verify prompt template expansion with realistic data.
	// The last spawned agent prompt corresponds to the last fixture issue (FSM-1035).
	lastPrompt := daemon.lastPrompt
	if !strings.Contains(lastPrompt, "FSM-1035") {
		t.Errorf("last prompt should reference FSM-1035, got: %s", lastPrompt)
	}
	if !strings.Contains(lastPrompt, "PDF report generation fails") {
		t.Errorf("last prompt should contain summary, got: %s", lastPrompt)
	}
	if !strings.Contains(lastPrompt, "Critical") {
		t.Errorf("last prompt should contain priority, got: %s", lastPrompt)
	}

	// Verify the JIRA server received the correct search request.
	reqs := jira.Requests()
	if len(reqs) == 0 {
		t.Fatal("no JIRA requests recorded")
	}
	if !strings.Contains(reqs[0].JQL, "project = FSM") {
		t.Errorf("JIRA search JQL = %q", reqs[0].JQL)
	}
}

// TestIntegration_PEBugAutoFix tests the flow of:
// PE critical bugs found → trigger with apply_formula → beads created with formula metadata
func TestIntegration_PEBugAutoFix(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	jira.OnSearch("project = PE", "jira_search_pe_critical_bugs.json")

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trig-pe-autofix",
				Type:   "trigger",
				Status: "open",
				Title:  "Auto-fix PE critical bugs",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = PE AND issuetype = Bug AND priority in (Critical, High) AND labels = gardenio-eligible AND assignee is EMPTY",
					"action":         "apply_formula",
					"project":        "monorepo",
					"formula_id":     "ka-0000001189",
					"formula_vars":   `{"TICKET": "{{jira_key}}", "BRANCH": "agent/fix-{{jira_key_lower}}"}`,
					"enabled":        "true",
					"max_concurrent": "5",
				},
			},
			// Formula bead the trigger resolves via GetBead after D6 removed
			// the pending_formula runner — the trigger now instantiates the
			// molecule directly. A single-step formula is enough to assert
			// that 2 matches => 2 molecules + 2 step beads.
			testFormulaBead("ka-0000001189", "TICKET"),
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// 2 PE bugs → 2 molecules + 2 step beads = 4 created beads, plus one
	// DRIVER agent per molecule so the poured work is never inert
	// (ka-0000000070).
	if len(daemon.createdBeads) != 4 {
		t.Fatalf("expected 4 created beads (2 molecules + 2 steps), got %d", len(daemon.createdBeads))
	}
	if len(daemon.spawnedNames) != 2 {
		t.Errorf("expected 2 driver spawns for apply_formula (one per molecule), got %d", len(daemon.spawnedNames))
	}

	// Each match should produce a molecule (created first) and at least one
	// step bead. The molecule type is "molecule"; step types come from the
	// formula step definition (default "task").
	molCount := 0
	for _, b := range daemon.createdBeads {
		if b.Type == "molecule" {
			molCount++
		}
	}
	if molCount != 2 {
		t.Errorf("expected 2 molecules, got %d (created types=%v)", molCount, beadTypes(daemon.createdBeads))
	}
}

// beadTypes is a tiny test helper for diagnostic messages — lists the Type
// field of each created bead so failures are easier to triage.
func beadTypes(reqs []beadsapi.CreateBeadRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Type
	}
	return out
}

// TestIntegration_MultipleTriggers tests evaluating multiple triggers in one cycle,
// each matching different JQL and taking different actions.
func TestIntegration_MultipleTriggers(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	jira.OnSearch("project = FSM", "jira_search_fsm_escalations.json")
	jira.OnSearch("project = PE", "jira_search_pe_critical_bugs.json")

	var (
		notifyMu   sync.Mutex
		notifyMsgs []string
	)

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			// Trigger 1: FSM → spawn agents
			{
				ID:     "gd-trig-multi-1",
				Type:   "trigger",
				Status: "open",
				Title:  "FSM triage",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = FSM AND status = 'Escalate to Engineering'",
					"action":         "spawn_agent",
					"role":           "crew",
					"project":        "monorepo",
					"prompt":         "Triage {{jira_key}}",
					"enabled":        "true",
					"max_concurrent": "10",
				},
			},
			// Trigger 2: PE → notify only
			{
				ID:     "gd-trig-multi-2",
				Type:   "trigger",
				Status: "open",
				Title:  "PE bug watcher",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = PE AND issuetype = Bug AND priority = Critical",
					"action":  "notify",
					"prompt":  "Critical PE bug: {{jira_key}} — {{jira_summary}}",
					"enabled": "true",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Notifier: func(_ context.Context, _, msg string, _ string) {
			notifyMu.Lock()
			notifyMsgs = append(notifyMsgs, msg)
			notifyMu.Unlock()
		},
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	spawnCount := len(daemon.spawnedNames)
	beadCount := len(daemon.createdBeads)
	daemon.mu.Unlock()

	notifyMu.Lock()
	notifyCount := len(notifyMsgs)
	notifyMu.Unlock()

	// 3 FSM tickets → 3 agents spawned (with beads).
	if spawnCount != 3 {
		t.Errorf("expected 3 spawns from FSM trigger, got %d", spawnCount)
	}
	if beadCount != 3 {
		t.Errorf("expected 3 beads from FSM trigger (notify doesn't create beads), got %d", beadCount)
	}

	// 2 PE tickets → 2 notifications.
	if notifyCount != 2 {
		t.Errorf("expected 2 notifications from PE trigger, got %d", notifyCount)
	}

	// Verify JIRA received 2 search requests (one per trigger).
	reqs := jira.Requests()
	if len(reqs) != 2 {
		t.Errorf("expected 2 JIRA search requests, got %d", len(reqs))
	}
}

// TestIntegration_DedupAcrossRestarts tests that CatchUp correctly prevents
// re-firing for issues that already have beads from a previous run.
func TestIntegration_DedupAcrossRestarts(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	jira.OnSearch("project = FSM", "jira_search_fsm_escalations.json")

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			// Trigger bead.
			{
				ID:     "gd-trig-dedup-1",
				Type:   "trigger",
				Status: "open",
				Title:  "FSM triage",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = FSM AND status = 'Escalate to Engineering'",
					"action":         "spawn_agent",
					"project":        "monorepo",
					"enabled":        "true",
					"max_concurrent": "10",
				},
			},
			// Existing task beads from a previous trigger run.
			{
				ID:     "gd-existing-1042",
				Type:   "task",
				Status: "in_progress",
				Title:  "[FSM-1042] Unable to access study data export",
				Labels: []string{"source:jira", "trigger:gd-trig-dedup-1"},
				Fields: map[string]string{"jira_key": "FSM-1042"},
			},
			{
				ID:     "gd-existing-1038",
				Type:   "task",
				Status: "closed",
				Title:  "[FSM-1038] Calendar view not loading",
				Labels: []string{"source:jira", "trigger:gd-trig-dedup-1"},
				Fields: map[string]string{"jira_key": "FSM-1038"},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// Simulate restart: CatchUp scans existing beads.
	eval.CatchUp(context.Background())

	// Now evaluate — only FSM-1035 should fire (the other two are in the seen set).
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn (only FSM-1035 is new), got %d", len(daemon.spawnedNames))
	}
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	if daemon.createdBeads[0].Title != "[FSM-1035] PDF report generation fails for studies with > 500 patients" {
		t.Errorf("bead title = %q", daemon.createdBeads[0].Title)
	}
}

// TestIntegration_EmptyJQLResults tests that triggers handle zero results gracefully.
func TestIntegration_EmptyJQLResults(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	jira.OnSearch("project = FSM", "jira_search_empty.json")

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trig-empty",
				Type:   "trigger",
				Status: "open",
				Title:  "FSM triage",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = FSM AND status = 'Escalate to Engineering'",
					"action":  "spawn_agent",
					"project": "monorepo",
					"enabled": "true",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected 0 spawns for empty results, got %d", len(daemon.spawnedNames))
	}
	if len(daemon.createdBeads) != 0 {
		t.Errorf("expected 0 beads for empty results, got %d", len(daemon.createdBeads))
	}

	// Should still have updated last_checked.
	if len(daemon.updatedIDs) == 0 {
		t.Error("expected last_checked update even with empty results")
	}
}

// TestIntegration_JiraServerError tests that trigger evaluation handles JIRA API errors
// without crashing and without creating partial state.
func TestIntegration_JiraServerError(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	jira.forceError = true
	jira.forceErrorCode = http.StatusServiceUnavailable
	jira.forceErrorBody = `{"errorMessages":["JIRA is temporarily unavailable"]}`

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trig-err",
				Type:   "trigger",
				Status: "open",
				Title:  "Error trigger",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = PE",
					"action":  "spawn_agent",
					"project": "monorepo",
					"enabled": "true",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// Should not panic.
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// No beads or agents should have been created.
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected 0 spawns on JIRA error, got %d", len(daemon.spawnedNames))
	}
	if len(daemon.createdBeads) != 0 {
		t.Errorf("expected 0 beads on JIRA error, got %d", len(daemon.createdBeads))
	}
}

// TestIntegration_RateLimitHandling tests that the trigger evaluator degrades gracefully
// when JIRA returns 429 rate limit responses.
func TestIntegration_RateLimitHandling(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	// First request succeeds, subsequent requests get rate-limited.
	jira.OnSearch("project = FSM", "jira_search_fsm_escalations.json")
	jira.rateLimitAfter = 1

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trig-rl-1",
				Type:   "trigger",
				Status: "open",
				Title:  "FSM triage",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = FSM AND status = 'Escalate to Engineering'",
					"action":         "spawn_agent",
					"project":        "monorepo",
					"enabled":        "true",
					"max_concurrent": "10",
				},
			},
			{
				ID:     "gd-trig-rl-2",
				Type:   "trigger",
				Status: "open",
				Title:  "PE watcher",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = PE AND issuetype = Bug",
					"action":  "spawn_agent",
					"project": "monorepo",
					"enabled": "true",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// First trigger should succeed (3 FSM tickets), second should fail (rate limited).
	if len(daemon.spawnedNames) != 3 {
		t.Errorf("expected 3 spawns from first trigger (before rate limit), got %d", len(daemon.spawnedNames))
	}
}

// TestIntegration_TemplateExpansionRealistic tests template expansion with the full
// set of fields from a realistic JIRA fixture.
func TestIntegration_TemplateExpansionRealistic(t *testing.T) {
	// Build a realistic issue matching the fixture data.
	issue := JiraIssue{
		Key: "FSM-1042",
		Fields: JiraIssueFields{
			Summary:   "Unable to access study data export - timeout error for sponsor admin users",
			Status:    &JiraNamedRef{Name: "Escalate to Engineering"},
			IssueType: &JiraNamedRef{Name: "Service Request"},
			Priority:  &JiraNamedRef{Name: "High"},
			Reporter:  &JiraUser{DisplayName: "Sarah Chen"},
			Assignee:  nil,
			Labels:    []string{"customer-reported", "data-export", "gsk"},
			Description: json.RawMessage(`{
				"type": "doc",
				"version": 1,
				"content": [{"type": "paragraph", "content": [{"type": "text", "text": "Customer reported timeout on export."}]}]
			}`),
		},
	}

	tests := []struct {
		name     string
		template string
		check    func(t *testing.T, result string)
	}{
		{
			name:     "full triage prompt",
			template: "Triage {{jira_key}}: {{jira_summary}}. Priority: {{jira_priority}}. Type: {{jira_type}}. Reporter: {{jira_reporter}}. Assignee: {{jira_assignee}}.",
			check: func(t *testing.T, result string) {
				t.Helper()
				expected := "Triage FSM-1042: Unable to access study data export - timeout error for sponsor admin users. Priority: High. Type: Service Request. Reporter: Sarah Chen. Assignee: Unassigned."
				if result != expected {
					t.Errorf("got:\n  %s\nwant:\n  %s", result, expected)
				}
			},
		},
		{
			name:     "branch name with lowercase key",
			template: "agent/fix-{{jira_key_lower}}",
			check: func(t *testing.T, result string) {
				t.Helper()
				if result != "agent/fix-fsm-1042" {
					t.Errorf("got %q", result)
				}
			},
		},
		{
			name:     "formula vars JSON",
			template: `{"TICKET": "{{jira_key}}", "BRANCH": "agent/fix-{{jira_key_lower}}", "PRIORITY": "{{jira_priority}}"}`,
			check: func(t *testing.T, result string) {
				t.Helper()
				var vars map[string]string
				if err := json.Unmarshal([]byte(result), &vars); err != nil {
					t.Fatalf("formula vars not valid JSON: %v\nraw: %s", err, result)
				}
				if vars["TICKET"] != "FSM-1042" {
					t.Errorf("TICKET = %q", vars["TICKET"])
				}
				if vars["BRANCH"] != "agent/fix-fsm-1042" {
					t.Errorf("BRANCH = %q", vars["BRANCH"])
				}
				if vars["PRIORITY"] != "High" {
					t.Errorf("PRIORITY = %q", vars["PRIORITY"])
				}
			},
		},
		{
			name:     "labels expansion",
			template: "Labels: {{jira_labels}}",
			check: func(t *testing.T, result string) {
				t.Helper()
				if result != "Labels: customer-reported, data-export, gsk" {
					t.Errorf("got %q", result)
				}
			},
		},
		{
			name:     "url expansion",
			template: "See {{jira_url}}",
			check: func(t *testing.T, result string) {
				t.Helper()
				if result != "See https://acme.atlassian.net/browse/FSM-1042" {
					t.Errorf("got %q", result)
				}
			},
		},
		{
			name:     "description expansion",
			template: "Issue: {{jira_description}}",
			check: func(t *testing.T, result string) {
				t.Helper()
				if !strings.Contains(result, "Customer reported timeout on export.") {
					t.Errorf("description not expanded, got %q", result)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := expandIssueTemplate(tt.template, issue)
			tt.check(t, result)
		})
	}
}

// TestIntegration_CreateBeadFieldsRealistic tests that the task bead created from
// a realistic fixture issue contains the expected fields and labels.
func TestIntegration_CreateBeadFieldsRealistic(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	jira.OnSearch("project = PE", "jira_search_pe_critical_bugs.json")

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trig-fields-1",
				Type:   "trigger",
				Status: "open",
				Title:  "PE bug importer",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = PE AND issuetype = Bug AND priority = Critical",
					"action":         "create_bead",
					"project":        "monorepo",
					"enabled":        "true",
					"max_concurrent": "10",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 2 {
		t.Fatalf("expected 2 beads, got %d", len(daemon.createdBeads))
	}

	bead := daemon.createdBeads[0]

	// Verify type.
	if bead.Type != "task" {
		t.Errorf("bead type = %q, want task", bead.Type)
	}

	// Verify created_by.
	if bead.CreatedBy != "trigger:gd-trig-fields-1" {
		t.Errorf("bead created_by = %q", bead.CreatedBy)
	}

	// Verify labels.
	expectedLabels := []string{"source:jira", "jira:PE-7842", "trigger:gd-trig-fields-1", "project:monorepo"}
	for _, want := range expectedLabels {
		found := false
		for _, got := range bead.Labels {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing label %q in %v", want, bead.Labels)
		}
	}

	// Verify fields JSON contains JIRA metadata.
	var fields map[string]string
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("bead fields not valid JSON: %v", err)
	}
	if fields["jira_key"] != "PE-7842" {
		t.Errorf("fields.jira_key = %q", fields["jira_key"])
	}
	if fields["jira_status"] != "Open" {
		t.Errorf("fields.jira_status = %q", fields["jira_status"])
	}
	if fields["jira_type"] != "Bug" {
		t.Errorf("fields.jira_type = %q", fields["jira_type"])
	}
	if fields["jira_priority"] != "Critical" {
		t.Errorf("fields.jira_priority = %q", fields["jira_priority"])
	}
	if fields["trigger_name"] != "PE bug importer" {
		t.Errorf("fields.trigger_name = %q", fields["trigger_name"])
	}
}

// TestIntegration_MaxConcurrentWithRealisticData tests that max_concurrent correctly
// limits spawns even when many issues match, using fixture data.
func TestIntegration_MaxConcurrentWithRealisticData(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	// FSM fixture has 3 issues.
	jira.OnSearch("project = FSM", "jira_search_fsm_escalations.json")

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trig-limit-1",
				Type:   "trigger",
				Status: "open",
				Title:  "Limited FSM triage",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = FSM AND status = 'Escalate to Engineering'",
					"action":         "spawn_agent",
					"project":        "monorepo",
					"enabled":        "true",
					"max_concurrent": "1",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jira.URL()),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// First eval: only 1 of 3 tickets should spawn (max_concurrent=1).
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("eval 1: expected 1 spawn (max_concurrent=1), got %d", len(daemon.spawnedNames))
	}
	daemon.mu.Unlock()

	// Second eval: 1 more ticket.
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.spawnedNames) != 2 {
		t.Fatalf("eval 2: expected 2 total spawns, got %d", len(daemon.spawnedNames))
	}
	daemon.mu.Unlock()

	// Third eval: last ticket.
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.spawnedNames) != 3 {
		t.Fatalf("eval 3: expected 3 total spawns, got %d", len(daemon.spawnedNames))
	}
	daemon.mu.Unlock()

	// Fourth eval: nothing new.
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.spawnedNames) != 3 {
		t.Errorf("eval 4: expected still 3 spawns (all deduped), got %d", len(daemon.spawnedNames))
	}
	daemon.mu.Unlock()
}

// TestFixtureServer_RequestTracking verifies the fixture server correctly
// tracks all requests for test assertions.
func TestFixtureServer_RequestTracking(t *testing.T) {
	jira := newFixtureJiraServer()
	defer jira.Close()

	jira.defaultSearchFixture = "jira_search_empty.json"

	client := newTestJiraClient(jira.URL())
	_, _ = client.SearchIssues(context.Background(), "project = FOO", []string{"summary"}, 10)
	_, _ = client.SearchIssues(context.Background(), "project = BAR AND type = Bug", []string{"summary", "status"}, 20)

	reqs := jira.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 tracked requests, got %d", len(reqs))
	}

	if reqs[0].JQL != "project = FOO" {
		t.Errorf("req[0].JQL = %q", reqs[0].JQL)
	}
	if reqs[1].JQL != "project = BAR AND type = Bug" {
		t.Errorf("req[1].JQL = %q", reqs[1].JQL)
	}
}
