package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestGitLabTriggerSource_LoadTriggers tests loading GitLab triggers from beads.
func TestGitLabTriggerSource_LoadTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "t-gl-1", Type: "trigger", Title: "MR Watcher",
				Labels: []string{"trigger:gitlab"},
				Fields: map[string]string{
					"enabled":       "true",
					"project_path":  "Acme/Platform/monorepo",
					"state":         "opened",
					"gitlab_labels": "bug,urgent",
					"action":        "spawn_agent",
					"role":          "crew",
					"project":       "gardenio",
					"source":        "gitlab",
				},
			},
			{
				ID: "t-gl-2", Type: "trigger", Title: "Disabled",
				Labels: []string{"trigger:gitlab"},
				Fields: map[string]string{
					"enabled":      "false",
					"project_path": "Acme/other",
				},
			},
			{
				ID: "t-gl-3", Type: "trigger", Title: "No project path",
				Labels: []string{"trigger:gitlab"},
				Fields: map[string]string{
					"enabled": "true",
				},
			},
		},
	}

	source := NewGitLabTriggerSource(nil) // gitlab client not needed for LoadTriggers
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}

	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger (disabled and no-path skipped), got %d", len(triggers))
	}

	trig := triggers[0]
	if trig.ID != "t-gl-1" {
		t.Errorf("ID = %q, want t-gl-1", trig.ID)
	}
	if trig.Source != "gitlab" {
		t.Errorf("Source = %q, want gitlab", trig.Source)
	}
	if trig.Action != TriggerActionSpawnAgent {
		t.Errorf("Action = %q", trig.Action)
	}

	data, ok := trig.SourceData.(gitlabTriggerData)
	if !ok {
		t.Fatal("SourceData is not gitlabTriggerData")
	}
	if data.ProjectPath != "Acme/Platform/monorepo" {
		t.Errorf("ProjectPath = %q", data.ProjectPath)
	}
	if data.State != "opened" {
		t.Errorf("State = %q", data.State)
	}
	if data.Labels != "bug,urgent" {
		t.Errorf("Labels = %q", data.Labels)
	}
	if data.EventType != "merge_request" {
		t.Errorf("EventType = %q, want merge_request", data.EventType)
	}
}

// TestGitLabTriggerSource_LoadTriggers_Pipeline tests pipeline event type.
func TestGitLabTriggerSource_LoadTriggers_Pipeline(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "t-pip-1", Type: "trigger", Title: "Failed Pipelines",
				Labels: []string{"trigger:gitlab"},
				Fields: map[string]string{
					"enabled":              "true",
					"project_path":         "Acme/Platform/monorepo",
					"event_type":           "pipeline",
					"pipeline_status":      "failed",
					"pipeline_ref":         "main",
					"pipeline_ref_pattern": `^(main|refs/merge-requests/[0-9]+/train)$`,
					"min_duration":         "45m",
					"action":               "notify",
				},
			},
		},
	}

	source := NewGitLabTriggerSource(nil)
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}

	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}

	data, ok := triggers[0].SourceData.(gitlabTriggerData)
	if !ok {
		t.Fatal("SourceData is not gitlabTriggerData")
	}
	if data.EventType != "pipeline" {
		t.Errorf("EventType = %q, want pipeline", data.EventType)
	}
	if data.PipelineStatus != "failed" {
		t.Errorf("PipelineStatus = %q", data.PipelineStatus)
	}
	if data.PipelineRef != "main" {
		t.Errorf("PipelineRef = %q", data.PipelineRef)
	}
	if want := `^(main|refs/merge-requests/[0-9]+/train)$`; data.PipelineRefPattern != want {
		t.Errorf("PipelineRefPattern = %q, want %q", data.PipelineRefPattern, want)
	}
	if data.MinDuration != "45m" {
		t.Errorf("MinDuration = %q, want 45m", data.MinDuration)
	}
}

// TestGitLabMRToMatch tests converting a GitLabMR to a TriggerMatch.
func TestGitLabMRToMatch(t *testing.T) {
	mr := GitLabMR{
		ID:           100,
		IID:          42,
		Title:        "Fix the widget",
		State:        "opened",
		WebURL:       "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/42",
		SourceBranch: "fix/widget",
		TargetBranch: "main",
		Author:       &GitLabUser{ID: 1, Username: "alice", Name: "Alice Smith"},
		HeadPipeline: &GitLabPipRef{ID: 999, Status: "success", WebURL: "https://gitlab.com/Acme/Platform/monorepo/-/pipelines/999"},
		Labels:       []string{"bug", "urgent"},
		Description:  "Fixes the broken widget component.",
	}

	m := gitlabMRToMatch(mr, "Acme/Platform/monorepo")

	if m.Key != "Acme/Platform/monorepo!42" {
		t.Errorf("Key = %q", m.Key)
	}
	if m.Title != "Fix the widget" {
		t.Errorf("Title = %q", m.Title)
	}

	// Check template vars.
	if v := m.TemplateVars["{{gitlab_mr_title}}"]; v != "Fix the widget" {
		t.Errorf("gitlab_mr_title = %q", v)
	}
	if v := m.TemplateVars["{{gitlab_author}}"]; v != "alice" {
		t.Errorf("gitlab_author = %q", v)
	}
	if v := m.TemplateVars["{{gitlab_pipeline_status}}"]; v != "success" {
		t.Errorf("gitlab_pipeline_status = %q", v)
	}
	if v := m.TemplateVars["{{gitlab_source_branch}}"]; v != "fix/widget" {
		t.Errorf("gitlab_source_branch = %q", v)
	}
	if v := m.TemplateVars["{{gitlab_labels}}"]; v != "bug, urgent" {
		t.Errorf("gitlab_labels = %q", v)
	}
	if v := m.TemplateVars["{{gitlab_description}}"]; v != "Fixes the broken widget component." {
		t.Errorf("gitlab_description = %q", v)
	}

	// Check fields.
	if m.Fields["gitlab_key"] != "Acme/Platform/monorepo!42" {
		t.Errorf("gitlab_key field = %q", m.Fields["gitlab_key"])
	}
	if m.Fields["gitlab_author"] != "alice" {
		t.Errorf("gitlab_author field = %q", m.Fields["gitlab_author"])
	}
}

// TestGitLabMRToMatch_Minimal tests MR match with nil optional fields.
func TestGitLabMRToMatch_Minimal(t *testing.T) {
	mr := GitLabMR{
		IID:   7,
		Title: "Bare MR",
		State: "merged",
	}

	m := gitlabMRToMatch(mr, "org/repo")

	if m.Key != "org/repo!7" {
		t.Errorf("Key = %q", m.Key)
	}
	// Should not panic with nil Author/HeadPipeline.
	if _, ok := m.TemplateVars["{{gitlab_author}}"]; ok {
		t.Error("did not expect gitlab_author in vars for nil author")
	}
	if _, ok := m.TemplateVars["{{gitlab_pipeline_status}}"]; ok {
		t.Error("did not expect gitlab_pipeline_status in vars for nil pipeline")
	}
	if v := m.TemplateVars["{{gitlab_description}}"]; v != "" {
		t.Errorf("gitlab_description = %q, want empty", v)
	}
}

// TestGitLabPipelineToMatch tests converting a GitLabPipeline to a TriggerMatch.
func TestGitLabPipelineToMatch(t *testing.T) {
	pip := GitLabPipeline{
		ID:     555,
		Status: "failed",
		Ref:    "main",
		SHA:    "abc123",
		WebURL: "https://gitlab.com/org/repo/-/pipelines/555",
	}

	m := gitlabPipelineToMatch(pip, "org/repo")

	if m.Key != "org/repo/pipeline/555" {
		t.Errorf("Key = %q", m.Key)
	}
	if m.TemplateVars["{{gitlab_pipeline_status}}"] != "failed" {
		t.Errorf("pipeline_status = %q", m.TemplateVars["{{gitlab_pipeline_status}}"])
	}
	if m.TemplateVars["{{gitlab_pipeline_ref}}"] != "main" {
		t.Errorf("pipeline_ref = %q", m.TemplateVars["{{gitlab_pipeline_ref}}"])
	}
	if m.TemplateVars["{{gitlab_pipeline_sha}}"] != "abc123" {
		t.Errorf("pipeline_sha = %q", m.TemplateVars["{{gitlab_pipeline_sha}}"])
	}
	if m.Fields["gitlab_pipeline_status"] != "failed" {
		t.Errorf("field pipeline_status = %q", m.Fields["gitlab_pipeline_status"])
	}
}

// TestGitLabTriggerSource_CatchUpKeys tests the catch-up key recovery.
func TestGitLabTriggerSource_CatchUpKeys(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "b1", Type: "task",
				Labels: []string{"trigger:t-gl-1"},
				Fields: map[string]string{"gitlab_key": "org/repo!10"},
			},
			{
				ID: "b2", Type: "task",
				Labels: []string{"trigger:t-gl-1"},
				Fields: map[string]string{"gitlab_key": "org/repo!11"},
			},
			{
				ID: "b3", Type: "task",
				Labels: []string{"trigger:other"},
				Fields: map[string]string{"gitlab_key": "org/repo!99"},
			},
		},
	}

	source := NewGitLabTriggerSource(nil)
	keys, err := source.CatchUpKeys(context.Background(), daemon, "t-gl-1")
	if err != nil {
		t.Fatalf("CatchUpKeys: %v", err)
	}

	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}
	if !keySet["org/repo!10"] || !keySet["org/repo!11"] {
		t.Errorf("unexpected keys: %v", keys)
	}
}

// TestGitLabTriggerSource_CatchUpKeys_AgentDedup covers the apply_formula /
// spawn_agent durable-dedup path (ka-0000000963): those actions file NO issue
// bead, so the only durable fire record is the driver AGENT bead (type=agent,
// kind=config) carrying gitlab_key + trigger_id. CatchUpKeys must harvest those
// keys too — otherwise an apply_formula gitlab trigger has no restart-durable
// dedup and re-fires a fresh molecule on every engine restart / seen-key TTL
// eviction (the FSM-475 !1996 verify-fix re-fire storm). A completed (closed)
// driver still counts: the key was already fired regardless of agent lifecycle.
func TestGitLabTriggerSource_CatchUpKeys_AgentDedup(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			// apply_formula driver agent — running, no issue bead was filed.
			{
				ID: "a1", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio", "role:monorepo-verify"},
				Fields: map[string]string{"gitlab_key": "org/repo!20", "trigger_id": "t-gl-1"},
			},
			// apply_formula driver agent — already COMPLETED (closed). Still a
			// durable "already fired" record; must dedup so a merged+verified MR
			// is not re-poured after the driver finishes.
			{
				ID: "a2", Type: "agent", Status: "closed",
				Labels: []string{"project:gardenio", "role:monorepo-verify"},
				Fields: map[string]string{"gitlab_key": "org/repo!21", "trigger_id": "t-gl-1"},
			},
			// Driver agent spawned by a DIFFERENT trigger — must be ignored.
			{
				ID: "a3", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"},
				Fields: map[string]string{"gitlab_key": "org/repo!22", "trigger_id": "t-other"},
			},
			// file_bead issue for the SAME trigger — its key must merge in, and
			// a key shared with an agent bead must not be double-counted.
			{
				ID: "b1", Type: "task", Status: "open",
				Labels: []string{"trigger:t-gl-1"},
				Fields: map[string]string{"gitlab_key": "org/repo!20"},
			},
		},
	}

	source := NewGitLabTriggerSource(nil)
	keys, err := source.CatchUpKeys(context.Background(), daemon, "t-gl-1")
	if err != nil {
		t.Fatalf("CatchUpKeys: %v", err)
	}

	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 deduped keys (org/repo!20 shared, org/repo!21), got %d: %v", len(keys), keys)
	}
	if !keySet["org/repo!20"] || !keySet["org/repo!21"] {
		t.Errorf("missing expected agent/issue keys: %v", keys)
	}
	if keySet["org/repo!22"] {
		t.Errorf("leaked a key from a different trigger's agent bead: %v", keys)
	}
}

// TestGitLabTriggerSource_EngineIntegration tests GitLab source through the engine.
func TestGitLabTriggerSource_EngineIntegration(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// Use a mock source that mimics GitLab behavior.
	source := &mockTriggerSource{
		name: "gitlab",
		triggers: []TriggerDef{
			{
				ID: "t-gl-1", Title: "MR Watcher", Source: "gitlab",
				Action: TriggerActionCreateBead, MaxConcurrent: 10, Project: "gardenio",
				LastChecked: pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-gl-1": {
				{
					Key:   "Acme/Platform/monorepo!42",
					Title: "Fix the widget",
					Fields: map[string]string{
						"gitlab_key":    "Acme/Platform/monorepo!42",
						"gitlab_mr_url": "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/42",
					},
					Labels: []string{"gitlab:Acme/Platform/monorepo!42"},
					TemplateVars: map[string]string{
						"{{gitlab_mr_title}}": "Fix the widget",
						"{{gitlab_author}}":   "alice",
					},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.createdBeads) != 1 {
		daemon.mu.Unlock()
		t.Fatalf("expected 1 bead, got %d", len(daemon.createdBeads))
	}

	bead := daemon.createdBeads[0]
	if bead.Title != "[Acme/Platform/monorepo!42] Fix the widget" {
		t.Errorf("bead title = %q", bead.Title)
	}
	daemon.createdBeads = nil
	daemon.mu.Unlock()

	// Dedup: second eval should produce nothing.
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.createdBeads) != 0 {
		t.Errorf("expected 0 beads on second eval, got %d", len(daemon.createdBeads))
	}
	daemon.mu.Unlock()
}

// TestGitLabTriggerSource_TemplateExpansion tests template vars from GitLab MRs.
func TestGitLabTriggerSource_TemplateExpansion(t *testing.T) {
	mr := GitLabMR{
		IID:          42,
		Title:        "Fix the widget",
		SourceBranch: "fix/widget",
		TargetBranch: "main",
		Author:       &GitLabUser{Username: "alice"},
	}

	m := gitlabMRToMatch(mr, "org/repo")

	tmpl := "Review MR !{{gitlab_mr_iid}} by {{gitlab_author}}: {{gitlab_mr_title}} ({{gitlab_source_branch}} → {{gitlab_target_branch}})"
	got := ExpandTemplate(tmpl, m.TemplateVars)
	want := "Review MR !42 by alice: Fix the widget (fix/widget → main)"
	if got != want {
		t.Errorf("ExpandTemplate = %q, want %q", got, want)
	}
}

// TestGitLabTriggerSource_Name verifies the source name used for registration.
func TestGitLabTriggerSource_Name(t *testing.T) {
	if got := NewGitLabTriggerSource(nil).Name(); got != "gitlab" {
		t.Errorf("Name() = %q, want gitlab", got)
	}
}

// TestGitLabTriggerSource_EvaluateMergeRequests exercises Evaluate -> evaluateMergeRequests
// against a fake GitLab REST server, asserting the request the source issues and the
// matches it produces.
func TestGitLabTriggerSource_EvaluateMergeRequests(t *testing.T) {
	var gotQuery map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/org%2Frepo/merge_requests" &&
			r.URL.Path != "/api/v4/projects/org/repo/merge_requests" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		gotQuery = map[string]string{
			"state":           r.URL.Query().Get("state"),
			"labels":          r.URL.Query().Get("labels"),
			"author_username": r.URL.Query().Get("author_username"),
			"target_branch":   r.URL.Query().Get("target_branch"),
			"per_page":        r.URL.Query().Get("per_page"),
		}
		resp := []GitLabMR{
			{
				IID: 42, Title: "Fix the widget", State: "opened",
				SourceBranch: "fix/widget", TargetBranch: "main",
				WebURL: "https://gitlab.com/org/repo/-/merge_requests/42",
				Author: &GitLabUser{Username: "alice", Name: "Alice"},
				Labels: []string{"bug", "urgent"},
			},
			{
				IID: 43, Title: "Add gizmo", State: "opened",
				SourceBranch: "feat/gizmo", TargetBranch: "main",
				WebURL: "https://gitlab.com/org/repo/-/merge_requests/43",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		ID:     "t-gl-mr",
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:  "org/repo",
			State:        "opened",
			Labels:       "bug, urgent",
			AuthorUser:   "alice",
			TargetBranch: "main",
			EventType:    "merge_request",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}

	// Filters from the trigger data must be threaded into the REST query.
	if gotQuery["state"] != "opened" {
		t.Errorf("state query = %q, want opened", gotQuery["state"])
	}
	if gotQuery["labels"] != "bug,urgent" {
		t.Errorf("labels query = %q, want bug,urgent (trimmed + comma-joined)", gotQuery["labels"])
	}
	if gotQuery["author_username"] != "alice" {
		t.Errorf("author_username query = %q, want alice", gotQuery["author_username"])
	}
	if gotQuery["target_branch"] != "main" {
		t.Errorf("target_branch query = %q, want main", gotQuery["target_branch"])
	}
	if gotQuery["per_page"] != "50" {
		t.Errorf("per_page query = %q, want 50", gotQuery["per_page"])
	}

	if matches[0].Key != "org/repo!42" {
		t.Errorf("match[0].Key = %q, want org/repo!42", matches[0].Key)
	}
	if matches[0].TemplateVars["{{gitlab_author}}"] != "alice" {
		t.Errorf("match[0] author = %q", matches[0].TemplateVars["{{gitlab_author}}"])
	}
	if matches[1].Key != "org/repo!43" {
		t.Errorf("match[1].Key = %q, want org/repo!43", matches[1].Key)
	}
}

// TestGitLabTriggerSource_EvaluateMergeRequests_DefaultState verifies that when no
// state filter is set, evaluateMergeRequests defaults to "opened".
func TestGitLabTriggerSource_EvaluateMergeRequests_DefaultState(t *testing.T) {
	var gotState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitLabMR{})
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath: "org/repo",
			EventType:   "merge_request",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matches))
	}
	if gotState != "opened" {
		t.Errorf("default state query = %q, want opened", gotState)
	}
}

// TestGitLabTriggerSource_EvaluateMergeRequests_Error verifies that a GitLab API
// error is propagated out of Evaluate.
func TestGitLabTriggerSource_EvaluateMergeRequests_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"403 Forbidden"}`))
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath: "org/repo",
			EventType:   "merge_request",
		},
	}

	if _, err := source.Evaluate(context.Background(), trig); err == nil {
		t.Fatal("expected error from forbidden response, got nil")
	}
}

// TestGitLabTriggerSource_EvaluatePipelines exercises Evaluate -> evaluatePipelines
// against a fake GitLab REST server, asserting the request query and produced matches.
func TestGitLabTriggerSource_EvaluatePipelines(t *testing.T) {
	var gotRef, gotStatus, gotUpdatedAfter string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/org%2Frepo/pipelines" &&
			r.URL.Path != "/api/v4/projects/org/repo/pipelines" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		gotRef = r.URL.Query().Get("ref")
		gotStatus = r.URL.Query().Get("status")
		gotUpdatedAfter = r.URL.Query().Get("updated_after")
		resp := []GitLabPipeline{
			{ID: 555, Status: "failed", Ref: "main", SHA: "abc123", WebURL: "https://gitlab.com/org/repo/-/pipelines/555"},
			{ID: 556, Status: "failed", Ref: "main", SHA: "def456", WebURL: "https://gitlab.com/org/repo/-/pipelines/556"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:    "org/repo",
			EventType:      "pipeline",
			PipelineRef:    "main",
			PipelineStatus: "failed",
			PipelineMaxAge: 90 * time.Minute,
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if gotRef != "main" {
		t.Errorf("ref query = %q, want main", gotRef)
	}
	if gotStatus != "failed" {
		t.Errorf("status query = %q, want failed", gotStatus)
	}
	// The recency window must be threaded into updated_after so the source
	// never returns pre-existing failures older than the window. Assert the
	// timestamp is present and within the expected ballpark of now-90m.
	if gotUpdatedAfter == "" {
		t.Fatal("updated_after query missing — recency window not threaded")
	}
	ua, perr := time.Parse(time.RFC3339, gotUpdatedAfter)
	if perr != nil {
		t.Fatalf("updated_after %q not RFC3339: %v", gotUpdatedAfter, perr)
	}
	if delta := time.Since(ua) - 90*time.Minute; delta < -2*time.Minute || delta > 2*time.Minute {
		t.Errorf("updated_after = %s (age %s), want ~90m ago", gotUpdatedAfter, time.Since(ua))
	}
	if matches[0].Key != "org/repo/pipeline/555" {
		t.Errorf("match[0].Key = %q, want org/repo/pipeline/555", matches[0].Key)
	}
	if matches[0].TemplateVars["{{gitlab_pipeline_status}}"] != "failed" {
		t.Errorf("match[0] status = %q", matches[0].TemplateVars["{{gitlab_pipeline_status}}"])
	}
	if matches[1].Key != "org/repo/pipeline/556" {
		t.Errorf("match[1].Key = %q, want org/repo/pipeline/556", matches[1].Key)
	}
}

// TestParsePipelineMaxAge covers the pipeline_max_age field parser: empty and
// invalid/negative yield 0 (engine applies defaultPipelineMaxAge), valid
// durations parse through.
func TestParsePipelineMaxAge(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"  ", 0},
		{"abc", 0}, // unparseable ⇒ default
		{"-5m", 0}, // negative ⇒ default (never "unbounded")
		{"0", 0},   // zero ⇒ default
		{"2h", 2 * time.Hour},
		{"90m", 90 * time.Minute},
		{"720h", 720 * time.Hour},
	}
	for _, c := range cases {
		if got := parsePipelineMaxAge(c.in); got != c.want {
			t.Errorf("parsePipelineMaxAge(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_DefaultWindow verifies that a
// pipeline trigger with no explicit pipeline_max_age still bounds the query
// with updated_after (defaultPipelineMaxAge), so a re-enabled trigger with an
// empty seen-map cannot fire against pre-existing failures.
func TestGitLabTriggerSource_EvaluatePipelines_DefaultWindow(t *testing.T) {
	var gotUpdatedAfter string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUpdatedAfter = r.URL.Query().Get("updated_after")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitLabPipeline{})
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:    "org/repo",
			EventType:      "pipeline",
			PipelineStatus: "failed",
			// PipelineMaxAge intentionally unset ⇒ default window applies.
		},
	}

	if _, err := source.Evaluate(context.Background(), trig); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if gotUpdatedAfter == "" {
		t.Fatal("updated_after query missing — default recency window not applied")
	}
	ua, perr := time.Parse(time.RFC3339, gotUpdatedAfter)
	if perr != nil {
		t.Fatalf("updated_after %q not RFC3339: %v", gotUpdatedAfter, perr)
	}
	if delta := time.Since(ua) - defaultPipelineMaxAge; delta < -2*time.Minute || delta > 2*time.Minute {
		t.Errorf("updated_after = %s (age %s), want ~%s ago", gotUpdatedAfter, time.Since(ua), defaultPipelineMaxAge)
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_RefPattern verifies that
// pipeline_ref_pattern is applied client-side AFTER the fetch and BEFORE
// matches are returned: refs that do not match the regexp never become a
// TriggerMatch (so they never create a task bead / spawn an agent), while
// matching refs pass through. This is the exact monorepo case — keep main +
// merge-train refs, drop the .../merge, .../head, and push-branch pipelines
// that make up ~91% of failed-pipeline firings.
func TestGitLabTriggerSource_EvaluatePipelines_RefPattern(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := []GitLabPipeline{
			{ID: 1, Status: "failed", Ref: "main"},
			{ID: 2, Status: "failed", Ref: "refs/merge-requests/42/train"},
			{ID: 3, Status: "failed", Ref: "refs/merge-requests/42/merge"},
			{ID: 4, Status: "failed", Ref: "refs/merge-requests/42/head"},
			{ID: 5, Status: "failed", Ref: "feature/some-branch"},
			{ID: 6, Status: "failed", Ref: "refs/merge-requests/99/train"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:        "org/repo",
			EventType:          "pipeline",
			PipelineStatus:     "failed",
			PipelineRefPattern: `^(main|refs/merge-requests/[0-9]+/train)$`,
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// Only pipelines 1, 2, 6 (main + the two /train refs) should survive.
	if len(matches) != 3 {
		t.Fatalf("expected 3 matches after ref-pattern filter, got %d: %+v", len(matches), matches)
	}
	gotKeys := map[string]bool{}
	for _, m := range matches {
		gotKeys[m.Key] = true
	}
	for _, want := range []string{"org/repo/pipeline/1", "org/repo/pipeline/2", "org/repo/pipeline/6"} {
		if !gotKeys[want] {
			t.Errorf("expected match %q to survive the filter, got keys %v", want, gotKeys)
		}
	}
	for _, unwanted := range []string{"org/repo/pipeline/3", "org/repo/pipeline/4", "org/repo/pipeline/5"} {
		if gotKeys[unwanted] {
			t.Errorf("expected match %q to be filtered out, but it survived", unwanted)
		}
	}
}

// floatPtr is a tiny helper to build *float64 for pipeline durations in tests.
func floatPtr(f float64) *float64 { return &f }

// pipelineDetailServer stands up a fake GitLab REST endpoint that serves BOTH
// the list-pipelines endpoint (which, like the real API, omits `duration`) and
// the per-pipeline DETAIL endpoint (which carries `duration`). durations maps a
// pipeline ID to its detail-endpoint duration in seconds; a nil entry means the
// detail endpoint returns a pipeline with a null duration. detailHits counts how
// many times the detail endpoint was called so a test can assert the detail
// fetch only happens for candidates that passed the cheap filters.
func pipelineDetailServer(t *testing.T, list []GitLabPipeline, durations map[int]*float64, detailHits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Detail endpoint: /api/v4/projects/<enc>/pipelines/<id>
		if strings.Contains(r.URL.Path, "/pipelines/") {
			*detailHits++
			parts := strings.Split(r.URL.Path, "/pipelines/")
			id, _ := strconv.Atoi(parts[len(parts)-1])
			// Echo back a detail object carrying the mapped duration.
			var base GitLabPipeline
			for _, p := range list {
				if p.ID == id {
					base = p
					break
				}
			}
			base.ID = id
			base.Duration = durations[id]
			_ = json.NewEncoder(w).Encode(base)
			return
		}
		// List endpoint. Strip `duration` to mirror the real API, which never
		// returns it on the list shape.
		stripped := make([]GitLabPipeline, len(list))
		copy(stripped, list)
		for i := range stripped {
			stripped[i].Duration = nil
		}
		_ = json.NewEncoder(w).Encode(stripped)
	}))
}

// TestGitLabTriggerSource_EvaluatePipelines_MinDuration verifies the slow-pipeline
// threshold: a pipeline becomes a match ONLY if its DETAIL-endpoint duration is
// at/over min_duration. GitLab's list endpoint omits duration, so this also
// proves the engine fetches pipeline detail to get an authoritative number
// rather than reading the always-nil list-shaped duration (which would make the
// trigger silently never fire).
func TestGitLabTriggerSource_EvaluatePipelines_MinDuration(t *testing.T) {
	list := []GitLabPipeline{
		{ID: 1, Status: "success", Ref: "refs/merge-requests/10/train"}, // 20m — under
		{ID: 2, Status: "success", Ref: "refs/merge-requests/11/train"}, // 50m — over
		{ID: 3, Status: "success", Ref: "refs/merge-requests/12/train"}, // 45m — exactly at threshold (inclusive)
		{ID: 4, Status: "success", Ref: "refs/merge-requests/13/train"}, // null duration — fail closed
	}
	durations := map[int]*float64{
		1: floatPtr(20 * 60),
		2: floatPtr(50 * 60),
		3: floatPtr(45 * 60),
		4: nil,
	}
	detailHits := 0
	server := pipelineDetailServer(t, list, durations, &detailHits)
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:    "org/repo",
			EventType:      "pipeline",
			PipelineStatus: "success",
			MinDuration:    "45m",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// Only pipelines 2 (50m) and 3 (45m, inclusive) are at/over the threshold.
	gotKeys := map[string]bool{}
	for _, m := range matches {
		gotKeys[m.Key] = true
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches at/over 45m, got %d: %v", len(matches), gotKeys)
	}
	for _, want := range []string{"org/repo/pipeline/2", "org/repo/pipeline/3"} {
		if !gotKeys[want] {
			t.Errorf("expected %q to fire (>= 45m), got %v", want, gotKeys)
		}
	}
	for _, unwanted := range []string{"org/repo/pipeline/1", "org/repo/pipeline/4"} {
		if gotKeys[unwanted] {
			t.Errorf("expected %q to be filtered (under threshold / null duration), but it fired", unwanted)
		}
	}
	// A detail fetch must happen for each of the 4 candidates (all passed the
	// cheap status filter), and only then.
	if detailHits != 4 {
		t.Errorf("expected 4 detail fetches (one per candidate), got %d", detailHits)
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_MinDurationComposesRefPattern proves
// the operator's exact ask — "wake me only on a SLOW GREEN merge train" — is
// expressible by composing status=success + ref_pattern + min_duration, and
// that the ref filter runs FIRST so no detail fetch is spent on a ref that could
// never match.
func TestGitLabTriggerSource_EvaluatePipelines_MinDurationComposesRefPattern(t *testing.T) {
	list := []GitLabPipeline{
		{ID: 1, Status: "success", Ref: "refs/merge-requests/10/train"}, // train, 50m — fires
		{ID: 2, Status: "success", Ref: "refs/merge-requests/10/merge"}, // NOT a train — ref-filtered, no detail fetch
		{ID: 3, Status: "success", Ref: "main"},                         // main — also ref-filtered (train-only pattern)
	}
	durations := map[int]*float64{
		1: floatPtr(50 * 60),
	}
	detailHits := 0
	server := pipelineDetailServer(t, list, durations, &detailHits)
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:        "org/repo",
			EventType:          "pipeline",
			PipelineStatus:     "success",
			PipelineRefPattern: `^refs/merge-requests/[0-9]+/train$`,
			MinDuration:        "45m",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].Key != "org/repo/pipeline/1" {
		t.Fatalf("expected only the slow train (pipeline/1) to fire, got %+v", matches)
	}
	// Only pipeline 1 (the sole train ref) survives the ref filter, so exactly
	// one detail fetch is spent — the /merge and main refs never cost one.
	if detailHits != 1 {
		t.Errorf("expected 1 detail fetch (only the ref that passed the ref filter), got %d", detailHits)
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_MinDurationNoFetchWhenUnset verifies
// that with no min_duration the engine spends ZERO detail fetches — the feature
// is free when unused (pre-feature behaviour preserved).
func TestGitLabTriggerSource_EvaluatePipelines_MinDurationNoFetchWhenUnset(t *testing.T) {
	list := []GitLabPipeline{
		{ID: 1, Status: "failed", Ref: "main"},
		{ID: 2, Status: "failed", Ref: "main"},
	}
	detailHits := 0
	server := pipelineDetailServer(t, list, map[int]*float64{}, &detailHits)
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:    "org/repo",
			EventType:      "pipeline",
			PipelineStatus: "failed",
			// MinDuration intentionally unset.
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches (no threshold), got %d", len(matches))
	}
	if detailHits != 0 {
		t.Errorf("expected ZERO detail fetches when min_duration unset, got %d", detailHits)
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_InvalidMinDuration verifies the
// fail-CLOSED posture for min_duration: an unparseable value makes evaluate
// return an error and ZERO matches, BEFORE the API is hit — so a typo can never
// turn a slow-pipeline trigger into one that fires on everything.
func TestGitLabTriggerSource_EvaluatePipelines_InvalidMinDuration(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitLabPipeline{{ID: 1, Status: "success", Ref: "main"}})
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:    "org/repo",
			EventType:      "pipeline",
			PipelineStatus: "success",
			MinDuration:    "not-a-duration",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err == nil {
		t.Fatal("expected error from invalid min_duration, got nil")
	}
	if matches != nil {
		t.Errorf("expected zero matches on invalid min_duration (fail closed), got %v", matches)
	}
	if hit {
		t.Error("expected invalid min_duration to fail closed before the GitLab fetch, but the API was called")
	}
}

// TestParseMinDuration covers the min_duration parser: empty ⇒ (0,false,nil);
// valid positive ⇒ (d,true,nil); unparseable / non-positive ⇒ error so the
// caller fails closed.
func TestParseMinDuration(t *testing.T) {
	cases := []struct {
		in      string
		wantD   time.Duration
		wantSet bool
		wantErr bool
	}{
		{"", 0, false, false},
		{"  ", 0, false, false},
		{"45m", 45 * time.Minute, true, false},
		{"1h30m", 90 * time.Minute, true, false},
		{"abc", 0, false, true},
		{"0", 0, false, true},
		{"-5m", 0, false, true},
	}
	for _, c := range cases {
		d, set, err := parseMinDuration(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseMinDuration(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if d != c.wantD || set != c.wantSet {
			t.Errorf("parseMinDuration(%q) = (%v, %v), want (%v, %v)", c.in, d, set, c.wantD, c.wantSet)
		}
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_InvalidRefPattern verifies the
// fail-CLOSED posture: an unparseable pipeline_ref_pattern makes evaluate
// return an error and ZERO matches, so a typo can never silently reopen the
// spawn firehose. The engine logs the error loudly and drops all matches
// (see triggers.go: an Evaluate error yields zero new matches).
func TestGitLabTriggerSource_EvaluatePipelines_InvalidRefPattern(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitLabPipeline{{ID: 1, Status: "failed", Ref: "main"}})
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:        "org/repo",
			EventType:          "pipeline",
			PipelineStatus:     "failed",
			PipelineRefPattern: "[unterminated", // invalid regexp
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err == nil {
		t.Fatal("expected error from invalid pipeline_ref_pattern, got nil")
	}
	if matches != nil {
		t.Errorf("expected zero matches on invalid pattern (fail closed), got %v", matches)
	}
	// The compile must fail BEFORE the API is even hit — no point spending a
	// GitLab call for a trigger that can only fail closed.
	if hit {
		t.Error("expected invalid regexp to fail closed before the GitLab fetch, but the API was called")
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_Error verifies that a GitLab API error
// is propagated out of the pipeline evaluation path.
func TestGitLabTriggerSource_EvaluatePipelines_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"500 Internal Server Error"}`))
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath: "org/repo",
			EventType:   "pipeline",
		},
	}

	if _, err := source.Evaluate(context.Background(), trig); err == nil {
		t.Fatal("expected error from 500 response, got nil")
	}
}

// TestGitLabTriggerSource_Evaluate_BadSourceData verifies that Evaluate returns no
// matches and no error when SourceData is not gitlabTriggerData.
func TestGitLabTriggerSource_Evaluate_BadSourceData(t *testing.T) {
	source := NewGitLabTriggerSource(nil)
	trig := TriggerDef{Source: "gitlab", SourceData: "not-gitlab-data"}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate with bad source data: %v", err)
	}
	if matches != nil {
		t.Errorf("expected nil matches, got %v", matches)
	}
}

// TestGitLabTriggerSource_EvaluateDedupAcrossPolls drives two consecutive Evaluate
// passes through the engine's dedup layer: the same GitLab pipeline must only
// produce a bead on the first poll, not the second.
func TestGitLabTriggerSource_EvaluateDedupAcrossPolls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := []GitLabPipeline{
			{ID: 700, Status: "failed", Ref: "main", SHA: "sha700", WebURL: "https://gitlab.com/org/repo/-/pipelines/700"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := gitlabTriggerData{ProjectPath: "org/repo", EventType: "pipeline", PipelineStatus: "failed"}

	first, err := source.evaluatePipelines(context.Background(), trig)
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	second, err := source.evaluatePipelines(context.Background(), trig)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}

	// The source itself is stateless: both polls return the same match key.
	// Dedup is the engine's responsibility (covered by EngineIntegration), but
	// the match key must be stable across polls for that dedup to work.
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected 1 match per poll, got %d and %d", len(first), len(second))
	}
	if first[0].Key != second[0].Key {
		t.Errorf("match key not stable across polls: %q vs %q", first[0].Key, second[0].Key)
	}
	if first[0].Key != "org/repo/pipeline/700" {
		t.Errorf("match key = %q, want org/repo/pipeline/700", first[0].Key)
	}
}

// TestGitLabMRToMatch_MatchKeyGen verifies the match-key generation format for MRs
// across a range of project paths and IIDs.
func TestGitLabMRToMatch_MatchKeyGen(t *testing.T) {
	cases := []struct {
		projectPath string
		iid         int
		want        string
	}{
		{"org/repo", 1, "org/repo!1"},
		{"Acme/Platform/monorepo", 42, "Acme/Platform/monorepo!42"},
		{"a/b/c/d", 9999, "a/b/c/d!9999"},
	}
	for _, tc := range cases {
		m := gitlabMRToMatch(GitLabMR{IID: tc.iid, Title: "x"}, tc.projectPath)
		if m.Key != tc.want {
			t.Errorf("gitlabMRToMatch(%q, %d).Key = %q, want %q", tc.projectPath, tc.iid, m.Key, tc.want)
		}
		// The dedup label mirrors the key.
		if len(m.Labels) != 1 || m.Labels[0] != "gitlab:"+tc.want {
			t.Errorf("Labels = %v, want [gitlab:%s]", m.Labels, tc.want)
		}
		if m.Fields["gitlab_key"] != tc.want {
			t.Errorf("gitlab_key field = %q, want %q", m.Fields["gitlab_key"], tc.want)
		}
	}
}

// TestGitLabPipelineToMatch_MatchKeyGen verifies the match-key generation format for
// pipelines and that field/label/title content is derived from the pipeline.
func TestGitLabPipelineToMatch_MatchKeyGen(t *testing.T) {
	m := gitlabPipelineToMatch(GitLabPipeline{ID: 123, Status: "success", Ref: "release/v2"}, "org/repo")
	if m.Key != "org/repo/pipeline/123" {
		t.Errorf("Key = %q, want org/repo/pipeline/123", m.Key)
	}
	if len(m.Labels) != 1 || m.Labels[0] != "gitlab:org/repo/pipeline/123" {
		t.Errorf("Labels = %v", m.Labels)
	}
	if m.Fields["gitlab_pipeline_id"] != "123" {
		t.Errorf("gitlab_pipeline_id field = %q, want 123", m.Fields["gitlab_pipeline_id"])
	}
	want := "Pipeline #123 (success) on release/v2 — org/repo"
	if m.Title != want {
		t.Errorf("Title = %q, want %q", m.Title, want)
	}
}

// TestGitLabTriggerSource_EvaluateMergeRequests_NoLabelsNoFilters verifies the
// query when only a project path is set: empty optional filters are omitted, but
// state still defaults to opened.
func TestGitLabTriggerSource_EvaluateMergeRequests_NoLabelsNoFilters(t *testing.T) {
	var present []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, k := range []string{"labels", "author_username", "target_branch"} {
			if r.URL.Query().Get(k) != "" {
				present = append(present, k)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitLabMR{})
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source:     "gitlab",
		SourceData: gitlabTriggerData{ProjectPath: "org/repo", State: "merged"},
	}

	if _, err := source.Evaluate(context.Background(), trig); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	sort.Strings(present)
	if len(present) != 0 {
		t.Errorf("expected no optional filters in query, got %v", present)
	}
}

// commitStatusPipelineServer stands up a fake GitLab REST endpoint serving BOTH
// the list-pipelines endpoint and the per-SHA commit-statuses endpoint
// (/repository/commits/<sha>/statuses) that the ignore_failed_statuses filter
// consults. statusesBySHA maps a pipeline's SHA to the commit statuses returned
// for it; statusHits counts commit-status fetches so a test can assert the fetch
// happens only for failed candidates when the filter is configured.
func commitStatusPipelineServer(t *testing.T, list []GitLabPipeline, statusesBySHA map[string][]GitLabCommitStatus, statusHits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/repository/commits/") {
			*statusHits++
			rest := strings.SplitN(r.URL.Path, "/repository/commits/", 2)[1]
			sha := strings.TrimSuffix(rest, "/statuses")
			_ = json.NewEncoder(w).Encode(statusesBySHA[sha])
			return
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
}

// TestGitLabTriggerSource_EvaluatePipelines_IgnoreFailedStatuses is the core
// case: a pipeline whose ONLY required (allow_failure=false) failed commit status
// is an ignored external gate (Meticulous) is SUPPRESSED, while a pipeline with a
// real required CI-job failure — even alongside the same ignored gate — still
// fires. An allow_failure=true copy of the gate never counts.
func TestGitLabTriggerSource_EvaluatePipelines_IgnoreFailedStatuses(t *testing.T) {
	list := []GitLabPipeline{
		{ID: 1, Status: "failed", Ref: "refs/merge-requests/10/merge", SHA: "aaa"}, // Meticulous-only → suppressed
		{ID: 2, Status: "failed", Ref: "refs/merge-requests/11/merge", SHA: "bbb"}, // real job failure + Meticulous → fires
		{ID: 3, Status: "failed", Ref: "refs/merge-requests/12/merge", SHA: "ccc"}, // Meticulous failed-required, jobs green → suppressed
		{ID: 4, Status: "failed", Ref: "refs/merge-requests/13/merge", SHA: "ddd"}, // Meticulous only, but allow_failure=true job also red → still suppressed
	}
	statusesBySHA := map[string][]GitLabCommitStatus{
		"aaa": {
			{Name: "changes", Status: "success", AllowFailure: false},
			{Name: "Meticulous Tests", Status: "failed", AllowFailure: false},
		},
		"bbb": {
			{Name: "study-web:unit", Status: "failed", AllowFailure: false},
			{Name: "Meticulous Tests", Status: "failed", AllowFailure: false},
		},
		"ccc": {
			{Name: "changes", Status: "success", AllowFailure: false},
			{Name: "study-web:build", Status: "success", AllowFailure: false},
			{Name: "Meticulous Tests", Status: "failed", AllowFailure: false},
		},
		"ddd": {
			{Name: "Meticulous Tests", Status: "failed", AllowFailure: false},
			{Name: "study-web:meticulous", Status: "failed", AllowFailure: true},
		},
	}
	statusHits := 0
	server := commitStatusPipelineServer(t, list, statusesBySHA, &statusHits)
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:          "org/repo",
			EventType:            "pipeline",
			PipelineStatus:       "failed",
			IgnoreFailedStatuses: "Meticulous Tests",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	gotKeys := map[string]bool{}
	for _, m := range matches {
		gotKeys[m.Key] = true
	}
	if !gotKeys["org/repo/pipeline/2"] {
		t.Errorf("expected pipeline/2 (real job failure) to fire, got %v", gotKeys)
	}
	for _, unwanted := range []string{"org/repo/pipeline/1", "org/repo/pipeline/3", "org/repo/pipeline/4"} {
		if gotKeys[unwanted] {
			t.Errorf("expected %q (Meticulous-only failure) suppressed, but it fired", unwanted)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 match (pipeline/2), got %d: %v", len(matches), gotKeys)
	}
	// One commit-status fetch per failed candidate (all 4 are failed).
	if statusHits != 4 {
		t.Errorf("expected 4 commit-status fetches (one per failed candidate), got %d", statusHits)
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_IgnoreFailedStatusesNoFetchWhenUnset
// proves the filter is free when unused: no ignore_failed_statuses ⇒ zero
// commit-status fetches and every failed pipeline matches (pre-feature behaviour).
func TestGitLabTriggerSource_EvaluatePipelines_IgnoreFailedStatusesNoFetchWhenUnset(t *testing.T) {
	list := []GitLabPipeline{
		{ID: 1, Status: "failed", Ref: "main", SHA: "aaa"},
		{ID: 2, Status: "failed", Ref: "main", SHA: "bbb"},
	}
	statusHits := 0
	server := commitStatusPipelineServer(t, list, map[string][]GitLabCommitStatus{}, &statusHits)
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:    "org/repo",
			EventType:      "pipeline",
			PipelineStatus: "failed",
			// IgnoreFailedStatuses intentionally unset.
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches (filter inert), got %d", len(matches))
	}
	if statusHits != 0 {
		t.Errorf("expected ZERO commit-status fetches when ignore_failed_statuses unset, got %d", statusHits)
	}
}

// TestGitLabTriggerSource_EvaluatePipelines_IgnoreFailedStatusesFailSafe verifies
// the fail-SAFE posture: when the commit-status fetch errors, or when NO required
// commit status is failed (the failure originates outside commit statuses), the
// pipeline is NOT suppressed — an analyst still spawns. Under-suppression is the
// safe direction for this filter.
func TestGitLabTriggerSource_EvaluatePipelines_IgnoreFailedStatusesFailSafe(t *testing.T) {
	list := []GitLabPipeline{
		{ID: 1, Status: "failed", Ref: "main", SHA: "err"},   // fetch errors → still fires
		{ID: 2, Status: "failed", Ref: "main", SHA: "green"}, // no required failure visible → still fires
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/repository/commits/err/") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if strings.Contains(r.URL.Path, "/repository/commits/") {
			// green: only allow_failure=true statuses are red, so nothing required failed.
			_ = json.NewEncoder(w).Encode([]GitLabCommitStatus{
				{Name: "changes", Status: "success", AllowFailure: false},
				{Name: "study-web:meticulous", Status: "failed", AllowFailure: true},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	defer server.Close()

	source := NewGitLabTriggerSource(newTestGitLabClient(server.URL))
	trig := TriggerDef{
		Source: "gitlab",
		SourceData: gitlabTriggerData{
			ProjectPath:          "org/repo",
			EventType:            "pipeline",
			PipelineStatus:       "failed",
			IgnoreFailedStatuses: "Meticulous Tests",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected both pipelines to fire (fail safe), got %d: %+v", len(matches), matches)
	}
}

// TestParseIgnoreStatuses covers the parser: comma-split, trim, lower-case, drop
// blanks; empty/blank ⇒ empty set (filter inert).
func TestParseIgnoreStatuses(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]struct{}
	}{
		{"", map[string]struct{}{}},
		{"   ", map[string]struct{}{}},
		{"Meticulous Tests", map[string]struct{}{"meticulous tests": {}}},
		{" Meticulous Tests , CI/e2e ,, ", map[string]struct{}{"meticulous tests": {}, "ci/e2e": {}}},
	}
	for _, c := range cases {
		got := parseIgnoreStatuses(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseIgnoreStatuses(%q) size %d, want %d (%v)", c.in, len(got), len(c.want), got)
			continue
		}
		for k := range c.want {
			if _, ok := got[k]; !ok {
				t.Errorf("parseIgnoreStatuses(%q) missing %q; got %v", c.in, k, got)
			}
		}
	}
}
