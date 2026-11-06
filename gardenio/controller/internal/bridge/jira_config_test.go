package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// jiraConfigMock implements both JiraBeadClient and JiraConfigClient for
// testing the config watcher and poller together.
type jiraConfigMock struct {
	mu     sync.Mutex
	beads  map[string]*beadsapi.BeadDetail
	nextID int
}

func newJiraConfigMock() *jiraConfigMock {
	return &jiraConfigMock{
		beads: make(map[string]*beadsapi.BeadDetail),
	}
}

func (m *jiraConfigMock) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("bd-cfg-%d", m.nextID)
	var fields map[string]string
	if len(req.Fields) > 0 {
		_ = json.Unmarshal(req.Fields, &fields)
	}
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  req.Title,
		Type:   req.Type,
		Status: "open",
		Fields: fields,
	}
	return id, nil
}

func (m *jiraConfigMock) ListTaskBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
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

func (m *jiraConfigMock) AddDependency(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (m *jiraConfigMock) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
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

func (m *jiraConfigMock) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		match := true
		if len(q.Types) > 0 {
			typeMatch := false
			for _, t := range q.Types {
				if b.Type == t {
					typeMatch = true
					break
				}
			}
			if !typeMatch {
				match = false
			}
		}
		if match {
			result = append(result, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: result, Total: len(result)}, nil
}

func (m *jiraConfigMock) CloseBead(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	bead.Status = "closed"
	return nil
}

func (m *jiraConfigMock) UpdateBeadDescription(_ context.Context, _, _ string) error {
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestJiraConfigWatcher_ParseEventFields_OverridesEnv(t *testing.T) {
	envCfg := JiraPollerConfig{
		Projects:       []string{"PE", "DEVOPS"},
		Statuses:       []string{"To Do"},
		IssueTypes:     []string{"Bug"},
		FixVersions:    nil,
		RequiredLabels: nil,
		ExcludedLabels: nil,
		PollInterval:   60 * time.Second,
	}

	w := &JiraConfigWatcher{envCfg: envCfg}

	fields := map[string]string{
		"fix_versions":    "2.5.0",
		"required_labels": "approved-to-work",
		"excluded_labels": "blocked,deferred",
		"custom_jql":      "priority = High",
		"poll_interval":   "30s",
	}

	cfg := w.parseEventFields(fields)

	// Env defaults should survive.
	if len(cfg.Projects) != 2 || cfg.Projects[0] != "PE" {
		t.Errorf("expected Projects from env, got %v", cfg.Projects)
	}

	// Overrides should be applied.
	if len(cfg.FixVersions) != 1 || cfg.FixVersions[0] != "2.5.0" {
		t.Errorf("expected FixVersions=[2.5.0], got %v", cfg.FixVersions)
	}
	if len(cfg.RequiredLabels) != 1 || cfg.RequiredLabels[0] != "approved-to-work" {
		t.Errorf("expected RequiredLabels=[approved-to-work], got %v", cfg.RequiredLabels)
	}
	if len(cfg.ExcludedLabels) != 2 || cfg.ExcludedLabels[0] != "blocked" {
		t.Errorf("expected ExcludedLabels=[blocked,deferred], got %v", cfg.ExcludedLabels)
	}
	if cfg.CustomJQL != "priority = High" {
		t.Errorf("expected CustomJQL='priority = High', got %q", cfg.CustomJQL)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("expected PollInterval=30s, got %v", cfg.PollInterval)
	}
}

func TestJiraConfigWatcher_ParseEventFields_EmptyFieldsKeepDefaults(t *testing.T) {
	envCfg := JiraPollerConfig{
		Projects:    []string{"PE"},
		Statuses:    []string{"To Do"},
		FixVersions: []string{"1.0.0"},
	}

	w := &JiraConfigWatcher{envCfg: envCfg}

	// Empty fields map — should keep env defaults.
	cfg := w.parseEventFields(map[string]string{})

	if len(cfg.Projects) != 1 || cfg.Projects[0] != "PE" {
		t.Errorf("expected Projects from env, got %v", cfg.Projects)
	}
	if len(cfg.FixVersions) != 1 || cfg.FixVersions[0] != "1.0.0" {
		t.Errorf("expected FixVersions from env, got %v", cfg.FixVersions)
	}
}

func TestJiraConfigWatcher_LoadOrSeed_SeedsWhenNoBeadExists(t *testing.T) {
	daemon := newJiraConfigMock()

	envCfg := JiraPollerConfig{
		Projects:     []string{"PE"},
		Statuses:     []string{"To Do"},
		IssueTypes:   []string{"Bug"},
		PollInterval: 60 * time.Second,
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

	// Should have created a config bead.
	if w.beadID == "" {
		t.Error("expected config bead to be created")
	}

	// Verify the bead was created with the right title.
	bead := daemon.beads[w.beadID]
	if bead == nil {
		t.Fatal("expected bead to exist")
	}
	if bead.Title != "jira-poller" {
		t.Errorf("expected title 'jira-poller', got %q", bead.Title)
	}
	if bead.Type != "jira-poller" {
		t.Errorf("expected type 'jira-poller', got %q", bead.Type)
	}
}

func TestJiraConfigWatcher_LoadOrSeed_AppliesExistingBead(t *testing.T) {
	daemon := newJiraConfigMock()

	// Pre-create a jira-poller bead.
	daemon.beads["config-1"] = &beadsapi.BeadDetail{
		ID:     "config-1",
		Title:  "jira-poller",
		Type:   "jira-poller",
		Status: "open",
		Fields: map[string]string{
			"projects":        "PE,DEVOPS",
			"fix_versions":    "2.5.0",
			"required_labels": "approved-to-work",
			"poll_interval":   "30s",
		},
	}

	envCfg := JiraPollerConfig{
		Projects:     []string{"PE"},
		PollInterval: 60 * time.Second,
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

	// Should have found the existing bead.
	if w.beadID != "config-1" {
		t.Errorf("expected beadID='config-1', got %q", w.beadID)
	}

	// Should have pushed an update to the poller (draining the config channel).
	select {
	case cfg := <-poller.configCh:
		if len(cfg.FixVersions) != 1 || cfg.FixVersions[0] != "2.5.0" {
			t.Errorf("expected FixVersions=[2.5.0], got %v", cfg.FixVersions)
		}
	default:
		t.Error("expected config update on poller.configCh")
	}
}

func TestBuildJQL_ExcludedLabels(t *testing.T) {
	p := &JiraPoller{
		cfg: JiraPollerConfig{
			Projects:       []string{"PE"},
			ExcludedLabels: []string{"wontfix", "duplicate"},
		},
	}

	jql := p.buildJQL()

	if !containsSubstr(jql, `labels != "wontfix"`) {
		t.Errorf("expected excluded label 'wontfix' in JQL, got: %s", jql)
	}
	if !containsSubstr(jql, `labels != "duplicate"`) {
		t.Errorf("expected excluded label 'duplicate' in JQL, got: %s", jql)
	}
}

func TestBuildJQL_CustomJQL(t *testing.T) {
	p := &JiraPoller{
		cfg: JiraPollerConfig{
			Projects:  []string{"PE"},
			CustomJQL: "priority = High",
		},
	}

	jql := p.buildJQL()

	if !containsSubstr(jql, "(priority = High)") {
		t.Errorf("expected custom JQL clause in JQL, got: %s", jql)
	}
}

func TestUpdateConfig_SendsToChannel(t *testing.T) {
	envCfg := JiraPollerConfig{
		Projects:     []string{"PE"},
		PollInterval: 60 * time.Second,
		Logger:       discardLogger(),
	}
	p := NewJiraPoller(nil, nil, envCfg)

	newCfg := JiraPollerConfig{
		Projects:     []string{"PE", "DEVOPS"},
		FixVersions:  []string{"2.5.0"},
		PollInterval: 30 * time.Second,
	}

	p.UpdateConfig(newCfg)

	select {
	case cfg := <-p.configCh:
		if len(cfg.Projects) != 2 {
			t.Errorf("expected 2 projects, got %d", len(cfg.Projects))
		}
		// Logger should be preserved from the original config.
		if cfg.Logger == nil {
			t.Error("expected Logger to be preserved from original config")
		}
	default:
		t.Error("expected config on configCh")
	}
}

func TestBuildJQL_FilterProjects_ScopesFixVersionAndLabels(t *testing.T) {
	p := &JiraPoller{
		cfg: JiraPollerConfig{
			Projects:       []string{"PE", "DEVOPS"},
			Statuses:       []string{"To Do"},
			FixVersions:    []string{"2.5.0"},
			RequiredLabels: []string{"approved-to-work"},
			FilterProjects: []string{"PE"},
		},
	}

	jql := p.buildJQL()

	// PE should have fixVersion and label filters applied.
	if !containsSubstr(jql, `project IN ("PE")`) {
		t.Errorf("expected PE in filtered project clause, got: %s", jql)
	}
	if !containsSubstr(jql, `fixVersion IN ("2.5.0")`) {
		t.Errorf("expected fixVersion scoped to PE, got: %s", jql)
	}
	if !containsSubstr(jql, `labels = "approved-to-work"`) {
		t.Errorf("expected required label scoped to PE, got: %s", jql)
	}

	// DEVOPS should pass through without fixVersion/label filters.
	if !containsSubstr(jql, `project IN ("DEVOPS")`) {
		t.Errorf("expected DEVOPS in unfiltered project clause, got: %s", jql)
	}

	// The overall structure should be an OR between filtered and unfiltered projects.
	if !containsSubstr(jql, " OR ") {
		t.Errorf("expected OR clause between filtered and unfiltered projects, got: %s", jql)
	}
}

func TestBuildJQL_FilterProjects_Empty_FallsBackToGlobal(t *testing.T) {
	p := &JiraPoller{
		cfg: JiraPollerConfig{
			Projects:       []string{"PE", "DEVOPS"},
			FixVersions:    []string{"2.5.0"},
			RequiredLabels: []string{"approved-to-work"},
			// FilterProjects is empty — global behavior.
		},
	}

	jql := p.buildJQL()

	// Should apply filters globally (original behavior).
	if !containsSubstr(jql, `project IN ("PE","DEVOPS")`) {
		t.Errorf("expected global project clause, got: %s", jql)
	}
	if !containsSubstr(jql, `fixVersion IN ("2.5.0")`) {
		t.Errorf("expected global fixVersion filter, got: %s", jql)
	}
	if containsSubstr(jql, " OR ") {
		t.Errorf("expected no OR clause with empty FilterProjects, got: %s", jql)
	}
}

func TestBuildJQL_FilterProjects_AllProjectsFiltered(t *testing.T) {
	p := &JiraPoller{
		cfg: JiraPollerConfig{
			Projects:       []string{"PE"},
			FixVersions:    []string{"2.5.0"},
			FilterProjects: []string{"PE"},
		},
	}

	jql := p.buildJQL()

	// When all projects are in FilterProjects, no OR clause needed.
	if containsSubstr(jql, " OR ") {
		t.Errorf("expected no OR clause when all projects are filtered, got: %s", jql)
	}
	if !containsSubstr(jql, `project IN ("PE")`) {
		t.Errorf("expected PE project clause, got: %s", jql)
	}
	if !containsSubstr(jql, `fixVersion IN ("2.5.0")`) {
		t.Errorf("expected fixVersion filter, got: %s", jql)
	}
}

func TestJiraConfigWatcher_ParseEventFields_FilterProjects(t *testing.T) {
	envCfg := JiraPollerConfig{
		Projects: []string{"PE", "DEVOPS"},
	}

	w := &JiraConfigWatcher{envCfg: envCfg}

	fields := map[string]string{
		"filter_projects": "PE",
	}

	cfg := w.parseEventFields(fields)

	if len(cfg.FilterProjects) != 1 || cfg.FilterProjects[0] != "PE" {
		t.Errorf("expected FilterProjects=[PE], got %v", cfg.FilterProjects)
	}
}

// containsSubstr reports whether s contains substr.
func containsSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
