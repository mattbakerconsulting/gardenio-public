package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// commentRecord tracks a comment added to a bead.
type commentRecord struct {
	BeadID string
	Author string
	Text   string
}

// mockGitLabDaemon implements GitLabBeadClient for testing.
type mockGitLabDaemon struct {
	mu         sync.Mutex
	beads      map[string]*beadsapi.BeadDetail
	agentBeads []*beadsapi.BeadDetail
	comments   []commentRecord
	// mrUpserts records every UpsertMergeRequestBead call. nextMRBeadID is used
	// to mint deterministic IDs for new MR beads.
	mrUpserts    []mrUpsertRecord
	nextMRBeadID int
	// deps records every AddDependency call.
	deps []depRecord
	// listAllCalls / listForMRCalls count the two task-listing paths so tests
	// can assert the note webhook uses the targeted per-MR lookup rather than
	// dragging the whole task corpus into memory (ka-0000007917).
	listAllCalls   int
	listForMRCalls int
	listForMRArgs  []string
}

// mrUpsertRecord captures the arguments to a mock UpsertMergeRequestBead call.
type mrUpsertRecord struct {
	Fields beadsapi.MergeRequestBeadFields
	Closed bool
	BeadID string
}

func newMockGitLabDaemon() *mockGitLabDaemon {
	return &mockGitLabDaemon{beads: make(map[string]*beadsapi.BeadDetail)}
}

func (m *mockGitLabDaemon) ListTaskBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		// Mirror the real client which filters to type=task — non-task beads
		// (e.g. type=merge-request created by UpsertMergeRequestBead) must not
		// leak into iteration paths that expect a task bead.
		if b.Type != "" && b.Type != "task" {
			continue
		}
		result = append(result, b)
	}
	return result, nil
}

func (m *mockGitLabDaemon) ListTaskBeadsAll(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	m.listAllCalls++
	m.mu.Unlock()
	return m.ListTaskBeads(context.Background())
}

// ListTaskBeadsForMR mirrors the daemon's server-side fields.mr_url equality
// predicate so tests exercise the same narrowing the real client performs.
func (m *mockGitLabDaemon) ListTaskBeadsForMR(_ context.Context, mrURL string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	m.listForMRCalls++
	m.listForMRArgs = append(m.listForMRArgs, mrURL)
	m.mu.Unlock()
	if mrURL == "" {
		return nil, nil
	}
	all, err := m.ListTaskBeads(context.Background())
	if err != nil {
		return nil, err
	}
	var result []*beadsapi.BeadDetail
	for _, b := range all {
		if b.Fields["mr_url"] == mrURL {
			result = append(result, b)
		}
	}
	return result, nil
}

// listCallCounts returns (listAllCalls, listForMRCalls, listForMRArgs).
func (m *mockGitLabDaemon) listCallCounts() (int, int, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	args := make([]string, len(m.listForMRArgs))
	copy(args, m.listForMRArgs)
	return m.listAllCalls, m.listForMRCalls, args
}

func (m *mockGitLabDaemon) ListActiveAgentBeadDetails(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*beadsapi.BeadDetail, len(m.agentBeads))
	copy(result, m.agentBeads)
	return result, nil
}

func (m *mockGitLabDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	for k, v := range fields {
		bead.Fields[k] = v
	}
	return nil
}

func (m *mockGitLabDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.beads[beadID]; !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	m.comments = append(m.comments, commentRecord{BeadID: beadID, Author: author, Text: text})
	return nil
}

func (m *mockGitLabDaemon) UpsertMergeRequestBead(_ context.Context, f beadsapi.MergeRequestBeadFields, closed bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Find an existing MR bead by mr_url.
	var existing *beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Type == "merge-request" && b.Fields["mr_url"] == f.MRURL {
			existing = b
			break
		}
	}
	var beadID string
	if existing == nil {
		m.nextMRBeadID++
		beadID = fmt.Sprintf("mr-bead-%d", m.nextMRBeadID)
		fields := map[string]string{}
		mergeMRFields(fields, f)
		status := "open"
		if closed {
			status = "closed"
			if _, ok := fields["mr_outcome"]; !ok {
				if f.MRState == "merged" {
					fields["mr_outcome"] = "merged"
				} else {
					fields["mr_outcome"] = "closed"
				}
			}
		}
		m.beads[beadID] = &beadsapi.BeadDetail{
			ID:     beadID,
			Title:  f.Title,
			Type:   "merge-request",
			Status: status,
			Fields: fields,
		}
	} else {
		beadID = existing.ID
		mergeMRFields(existing.Fields, f)
		if closed && existing.Status != "closed" {
			existing.Status = "closed"
			if _, ok := existing.Fields["mr_outcome"]; !ok {
				if f.MRState == "merged" {
					existing.Fields["mr_outcome"] = "merged"
				} else {
					existing.Fields["mr_outcome"] = "closed"
				}
			}
		}
	}
	m.mrUpserts = append(m.mrUpserts, mrUpsertRecord{Fields: f, Closed: closed, BeadID: beadID})
	return beadID, nil
}

func (m *mockGitLabDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deps = append(m.deps, depRecord{BeadID: beadID, DependsOnID: dependsOnID, DepType: depType, CreatedBy: createdBy})
	return nil
}

// MRBeadForTask walks recorded `produces` deps from the task bead to its
// type=merge-request bead, mirroring beadsapi.Client.MRBeadForTask.
func (m *mockGitLabDaemon) MRBeadForTask(_ context.Context, taskBeadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.deps {
		if d.BeadID != taskBeadID || d.DepType != "produces" {
			continue
		}
		if b, ok := m.beads[d.DependsOnID]; ok && b.Type == "merge-request" {
			return b, nil
		}
	}
	return nil, nil
}

// mergeMRFields projects MergeRequestBeadFields into the bead fields map,
// matching the behavior of beadsapi.mrFieldsMap for tests.
func mergeMRFields(out map[string]string, f beadsapi.MergeRequestBeadFields) {
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("mr_url", f.MRURL)
	set("mr_iid", f.MRIID)
	set("project_path", f.ProjectPath)
	set("source_branch", f.SourceBranch)
	set("target_branch", f.TargetBranch)
	set("mr_state", f.MRState)
	set("mr_outcome", f.MROutcome)
	set("mr_pipeline_status", f.PipelineStatus)
	set("mr_pipeline_url", f.PipelineURL)
	set("author", f.Author)
	// operated_by is set-once: mirror beadsapi.Client.UpsertMergeRequestBead —
	// never overwrite a non-empty operated_by already on the bead (ka-0000004473).
	if out["operated_by"] == "" {
		set("operated_by", f.OperatedBy)
	}
	set("mr_title", f.Title)
	set("gitlab_mr_iid", f.MRIID)
	set("gitlab_project_id", f.GitLabProjectID)
	set("gitlab_mr_source_branch", f.SourceBranch)
	set("mr_approved", f.MRApproved)
	set("mr_approvers", f.MRApprovers)
	set("mr_has_review_comments", f.MRHasReviewComments)
	if f.MRState == "merged" {
		out["mr_merged"] = "true"
	} else if f.MRState == "closed" {
		out["mr_merged"] = "false"
	}
}

func (m *mockGitLabDaemon) getMRUpserts() []mrUpsertRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]mrUpsertRecord, len(m.mrUpserts))
	copy(result, m.mrUpserts)
	return result
}

func (m *mockGitLabDaemon) getDeps() []depRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]depRecord, len(m.deps))
	copy(result, m.deps)
	return result
}

func (m *mockGitLabDaemon) getBead(id string) *beadsapi.BeadDetail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beads[id]
}

func (m *mockGitLabDaemon) getComments() []commentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]commentRecord, len(m.comments))
	copy(result, m.comments)
	return result
}

func TestGitLabWebhookHandler_MergeEvent(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Title:  "Fix auth",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	handler := GitLabWebhookHandler(nil, daemon, "test-secret", slog.Default())

	event := map[string]any{
		"object_kind": "merge_request",
		"object_attributes": map[string]any{
			"iid":               42,
			"state":             "merged",
			"action":            "merge",
			"url":               "https://gitlab.com/org/repo/-/merge_requests/42",
			"target_project_id": 99,
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Task bead carries no MR-derived fields beyond mr_url. The legacy
	// mr_state/mr_merged/gitlab_mr_iid writes were dropped under epic
	// ka-0000001613 / task ka-0000008134; the residual gitlab_project_id +
	// mr_pipeline_status/url writes were dropped under task ka-0000000586.
	// The MR bead is the sole source of truth.
	bead := daemon.getBead("bead-1")
	if got := bead.Fields["gitlab_project_id"]; got != "" {
		t.Errorf("task-bead gitlab_project_id should no longer be written, got %q", got)
	}
	if got := bead.Fields["mr_merged"]; got != "" {
		t.Errorf("task-bead mr_merged should no longer be written, got %q", got)
	}
	if got := bead.Fields["mr_state"]; got != "" {
		t.Errorf("task-bead mr_state should no longer be written, got %q", got)
	}
	if got := bead.Fields["gitlab_mr_iid"]; got != "" {
		t.Errorf("task-bead gitlab_mr_iid should no longer be written, got %q", got)
	}
	if got := bead.Fields["mr_pipeline_status"]; got != "" {
		t.Errorf("task-bead mr_pipeline_status should no longer be written, got %q", got)
	}
	if got := bead.Fields["mr_pipeline_url"]; got != "" {
		t.Errorf("task-bead mr_pipeline_url should no longer be written, got %q", got)
	}

	// MR bead is the source of truth for merge state.
	mrBead, err := daemon.MRBeadForTask(context.Background(), "bead-1")
	if err != nil || mrBead == nil {
		t.Fatalf("expected MR bead linked to task: bead=%v err=%v", mrBead, err)
	}
	if mrBead.Fields["mr_state"] != "merged" {
		t.Errorf("mr bead mr_state=%s, want merged", mrBead.Fields["mr_state"])
	}
	if mrBead.Fields["mr_outcome"] != "merged" {
		t.Errorf("mr bead mr_outcome=%s, want merged", mrBead.Fields["mr_outcome"])
	}
	if mrBead.Fields["gitlab_mr_iid"] != "42" {
		t.Errorf("mr bead gitlab_mr_iid=%s, want 42", mrBead.Fields["gitlab_mr_iid"])
	}
	if mrBead.Fields["gitlab_project_id"] != "99" {
		t.Errorf("mr bead gitlab_project_id=%s, want 99", mrBead.Fields["gitlab_project_id"])
	}
}

func TestGitLabWebhookHandler_InvalidSecret(t *testing.T) {
	handler := GitLabWebhookHandler(nil, newMockGitLabDaemon(), "real-secret", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Gitlab-Token", "wrong-secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestGitLabWebhookHandler_EmptySecretRejects(t *testing.T) {
	// When no webhook secret is configured, all requests must be rejected (fail-closed).
	handler := GitLabWebhookHandler(nil, newMockGitLabDaemon(), "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 when secret is empty, got %d", w.Code)
	}
}

func TestGitLabWebhookHandler_IgnoreNonMerge(t *testing.T) {
	handler := GitLabWebhookHandler(nil, newMockGitLabDaemon(), "secret", slog.Default())

	event := map[string]any{
		"object_kind": "merge_request",
		"object_attributes": map[string]any{
			"action": "open",
			"url":    "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Should be ignored, no bead updates.
}

func TestGitLabWebhookHandler_NoMatchingBead(t *testing.T) {
	daemon := newMockGitLabDaemon()
	// No beads with matching mr_url.
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/other/repo/-/merge_requests/99"},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "merge_request",
		"object_attributes": map[string]any{
			"action": "merge",
			"url":    "https://gitlab.com/org/repo/-/merge_requests/42",
			"iid":    42,
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Bead should not be updated.
	bead := daemon.getBead("bead-1")
	if bead.Fields["mr_merged"] != "" {
		t.Errorf("expected mr_merged empty, got %s", bead.Fields["mr_merged"])
	}
}

func TestGitLabWebhookHandler_AlreadyMerged(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:   "bead-1",
		Type: "task",
		Fields: map[string]string{
			"mr_url":    "https://gitlab.com/org/repo/-/merge_requests/42",
			"mr_merged": "true",
		},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "merge_request",
		"object_attributes": map[string]any{
			"action": "merge",
			"url":    "https://gitlab.com/org/repo/-/merge_requests/42",
			"iid":    42,
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Should be a no-op since already merged.
}

func TestGitLabWebhookHandler_PipelineEvent(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Title:  "Fix auth",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "pipeline",
		"object_attributes": map[string]any{
			"id":     123,
			"status": "failed",
			"url":    "https://gitlab.com/org/repo/-/pipelines/123",
		},
		"merge_request": map[string]any{
			"iid": 42,
			"url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Task bead must NOT carry pipeline fields. The MR bead is the sole
	// source of truth for mr_pipeline_status / mr_pipeline_url; the residual
	// task-bead writes were dropped per task ka-0000000586.
	bead := daemon.getBead("bead-1")
	if got := bead.Fields["mr_pipeline_status"]; got != "" {
		t.Errorf("task-bead mr_pipeline_status should no longer be written, got %q", got)
	}
	if got := bead.Fields["mr_pipeline_url"]; got != "" {
		t.Errorf("task-bead mr_pipeline_url should no longer be written, got %q", got)
	}

	// MR bead is upserted with the pipeline state.
	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d MR-bead upserts, want 1", len(upserts))
	}
	if upserts[0].Fields.PipelineStatus != "failed" {
		t.Errorf("upserted MR PipelineStatus=%q, want failed", upserts[0].Fields.PipelineStatus)
	}
	if upserts[0].Fields.PipelineURL != "https://gitlab.com/org/repo/-/pipelines/123" {
		t.Errorf("upserted MR PipelineURL=%q, want pipeline URL", upserts[0].Fields.PipelineURL)
	}
}

// TestGitLabWebhookHandler_PipelineEvent_ForwardsMRState pins the W-A fix
// (ka-0000005367): the pipeline webhook MUST propagate merge_request.state
// (plus source/target branches) onto the MR bead so a pipeline event that
// beats or stands in for a merge_request event still leaves mr_state
// populated. Before the fix, MR beads first seen via a pipeline event had
// no mr_state field at all (32 such beads in the audit window).
func TestGitLabWebhookHandler_PipelineEvent_ForwardsMRState(t *testing.T) {
	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "pipeline",
		"object_attributes": map[string]any{
			"id":     321,
			"status": "running",
			"url":    "https://gitlab.com/org/repo/-/pipelines/321",
		},
		"merge_request": map[string]any{
			"iid":           99,
			"url":           "https://gitlab.com/org/repo/-/merge_requests/99",
			"title":         "WIP: thing",
			"state":         "opened",
			"source_branch": "feat/thing",
			"target_branch": "main",
		},
	}
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d MR-bead upserts, want 1", len(upserts))
	}
	if got := upserts[0].Fields.MRState; got != "opened" {
		t.Errorf("upserted MR MRState=%q, want opened", got)
	}
	if got := upserts[0].Fields.SourceBranch; got != "feat/thing" {
		t.Errorf("upserted MR SourceBranch=%q, want feat/thing", got)
	}
	if got := upserts[0].Fields.TargetBranch; got != "main" {
		t.Errorf("upserted MR TargetBranch=%q, want main", got)
	}
	if upserts[0].Closed {
		t.Errorf("Closed=true, want false for opened state")
	}
}

// TestGitLabWebhookHandler_PipelineEvent_ClosesOnTerminalState pins that
// a pipeline event observing a merged or closed MR drives the MR bead to
// the terminal state (status=closed, mr_outcome set) even if no
// merge_request webhook fires for that transition. This is the second leg
// of the W-A fix (ka-0000005367).
func TestGitLabWebhookHandler_PipelineEvent_ClosesOnTerminalState(t *testing.T) {
	cases := []struct {
		name        string
		state       string
		wantOutcome string
	}{
		{"merged", "merged", "merged"},
		{"closed", "closed", "closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockGitLabDaemon()
			handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

			event := map[string]any{
				"object_kind": "pipeline",
				"object_attributes": map[string]any{
					"id":     888,
					"status": "success",
					"url":    "https://gitlab.com/org/repo/-/pipelines/888",
				},
				"merge_request": map[string]any{
					"iid":   77,
					"url":   "https://gitlab.com/org/repo/-/merge_requests/77",
					"state": tc.state,
				},
			}
			body, _ := json.Marshal(event)
			req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
			req.Header.Set("X-Gitlab-Token", "secret")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			upserts := daemon.getMRUpserts()
			if len(upserts) != 1 {
				t.Fatalf("got %d upserts, want 1", len(upserts))
			}
			if !upserts[0].Closed {
				t.Errorf("Closed=false, want true for %s state", tc.state)
			}
			if got := upserts[0].Fields.MROutcome; got != tc.wantOutcome {
				t.Errorf("MROutcome=%q, want %q", got, tc.wantOutcome)
			}
			if got := upserts[0].Fields.MRState; got != tc.state {
				t.Errorf("MRState=%q, want %q", got, tc.state)
			}
		})
	}
}

func TestGitLabWebhookHandler_ApprovalEvent(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Title:  "Fix auth",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "alice"},
		"object_attributes": map[string]any{
			"iid":    42,
			"action": "approved",
			"url":    "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	bead := daemon.getBead("bead-1")
	if bead.Fields["mr_approved"] != "true" {
		t.Errorf("mr_approved=%s, want true", bead.Fields["mr_approved"])
	}
	if bead.Fields["mr_approvers"] != "alice" {
		t.Errorf("mr_approvers=%s, want alice", bead.Fields["mr_approvers"])
	}

	// Second approval by bob.
	event2 := map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "bob"},
		"object_attributes": map[string]any{
			"iid":    42,
			"action": "approved",
			"url":    "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body2, _ := json.Marshal(event2)
	req2 := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body2))
	req2.Header.Set("X-Gitlab-Token", "secret")
	w2 := httptest.NewRecorder()

	handler.ServeHTTP(w2, req2)

	bead = daemon.getBead("bead-1")
	if bead.Fields["mr_approvers"] != "alice,bob" {
		t.Errorf("mr_approvers=%s, want alice,bob", bead.Fields["mr_approvers"])
	}

	// Unapproval by alice.
	event3 := map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "alice"},
		"object_attributes": map[string]any{
			"iid":    42,
			"action": "unapproved",
			"url":    "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body3, _ := json.Marshal(event3)
	req3 := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body3))
	req3.Header.Set("X-Gitlab-Token", "secret")
	w3 := httptest.NewRecorder()

	handler.ServeHTTP(w3, req3)

	bead = daemon.getBead("bead-1")
	if bead.Fields["mr_approved"] != "false" {
		t.Errorf("mr_approved=%s, want false", bead.Fields["mr_approved"])
	}
	if bead.Fields["mr_approvers"] != "bob" {
		t.Errorf("mr_approvers=%s, want bob", bead.Fields["mr_approvers"])
	}
}

func TestGitLabWebhookHandler_NoteEvent(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Title:  "Fix auth",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "reviewer-alice"},
		"object_attributes": map[string]any{
			"note":          "This function needs error handling",
			"noteable_type": "MergeRequest",
			"system":        false,
			"position": map[string]any{
				"new_path": "pkg/auth/handler.go",
				"new_line": 42,
			},
		},
		"merge_request": map[string]any{
			"iid": 42,
			"url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	bead := daemon.getBead("bead-1")
	if bead.Fields["mr_has_review_comments"] != "true" {
		t.Errorf("mr_has_review_comments=%s, want true", bead.Fields["mr_has_review_comments"])
	}

	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].Author != "gitlab-bridge" {
		t.Errorf("comment author=%s, want gitlab-bridge", comments[0].Author)
	}
	if !strings.Contains(comments[0].Text, "reviewer-alice") {
		t.Errorf("comment should mention reviewer, got: %s", comments[0].Text)
	}
	if !strings.Contains(comments[0].Text, "pkg/auth/handler.go:42") {
		t.Errorf("comment should contain file:line, got: %s", comments[0].Text)
	}
	if !strings.Contains(comments[0].Text, "This function needs error handling") {
		t.Errorf("comment should contain note text, got: %s", comments[0].Text)
	}
}

func TestGitLabWebhookHandler_NoteEvent_SystemNote(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "system"},
		"object_attributes": map[string]any{
			"note":          "approved this merge request",
			"noteable_type": "MergeRequest",
			"system":        true,
		},
		"merge_request": map[string]any{
			"iid": 42,
			"url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// System notes should be ignored — no comments or field updates.
	comments := daemon.getComments()
	if len(comments) != 0 {
		t.Errorf("expected 0 comments for system note, got %d", len(comments))
	}
	bead := daemon.getBead("bead-1")
	if bead.Fields["mr_has_review_comments"] != "" {
		t.Errorf("mr_has_review_comments should be empty for system note, got %s", bead.Fields["mr_has_review_comments"])
	}
}

func TestGitLabWebhookHandler_NoteEvent_BotUsername(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		BotUsername:   "gardenio-bot",
		Logger:        slog.Default(),
	})

	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "gardenio-bot"},
		"object_attributes": map[string]any{
			"note":          "I've addressed this in the latest commit.",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{
			"iid": 42,
			"url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Bot comments should be ignored — no comments or field updates.
	comments := daemon.getComments()
	if len(comments) != 0 {
		t.Errorf("expected 0 comments for bot user, got %d", len(comments))
	}
	bead := daemon.getBead("bead-1")
	if bead.Fields["mr_has_review_comments"] != "" {
		t.Errorf("mr_has_review_comments should be empty for bot note, got %s", bead.Fields["mr_has_review_comments"])
	}
}

func TestExtractBotAgentName(t *testing.T) {
	tests := []struct {
		note string
		want string
	}{
		{"**gardenio agent** `worker-1`:\n\nI fixed the issue.", "worker-1"},
		{"**gardenio agent** `my-agent`:", "my-agent"},
		{"I've addressed this in the latest commit.", ""},
		{"**gardenio agent** ``:", ""},
		{"some random text", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractBotAgentName(tt.note)
		if got != tt.want {
			t.Errorf("extractBotAgentName(%q) = %q, want %q", tt.note, got, tt.want)
		}
	}
}

func TestGitLabWebhookHandler_NoteEvent_BotAgentSelfComment(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Type:     "task",
		Assignee: "worker-1",
		Fields:   map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	var nudged bool
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		BotUsername:   "gardenio-bot",
		Logger:        slog.Default(),
		Nudge: func(_ context.Context, _, _ string) error {
			nudged = true
			return nil
		},
	})

	// Agent worker-1 posts a squawk that echoes back as a webhook.
	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "gardenio-bot"},
		"object_attributes": map[string]any{
			"note":          "**gardenio agent** `worker-1`:\n\nI pushed a fix.",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{
			"iid": 42,
			"url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Comment should be recorded on the bead but no nudge sent.
	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Errorf("expected 1 comment (record-keeping), got %d", len(comments))
	}
	if nudged {
		t.Error("should NOT nudge the agent for its own squawk echo")
	}
}

func TestGitLabWebhookHandler_NoteEvent_BotAgentInterAgentComment(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Type:     "task",
		Assignee: "worker-1",
		Fields:   map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	var nudgedAgent string
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		BotUsername:   "gardenio-bot",
		Logger:        slog.Default(),
		Nudge: func(_ context.Context, agent, _ string) error {
			nudgedAgent = agent
			return nil
		},
	})

	// Agent worker-2 (different agent) posts a comment on worker-1's MR.
	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "gardenio-bot"},
		"object_attributes": map[string]any{
			"note":          "**gardenio agent** `worker-2`:\n\nYou have a nil pointer on line 42.",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{
			"iid": 42,
			"url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// worker-1 should be nudged about worker-2's comment.
	if nudgedAgent != "worker-1" {
		t.Errorf("expected nudge to worker-1, got %q", nudgedAgent)
	}
}

func TestGitLabWebhookHandler_NoteEvent_NoMatchingBead(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/other/repo/-/merge_requests/99"},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "alice"},
		"object_attributes": map[string]any{
			"note":          "looks good",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{
			"iid": 42,
			"url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// No matching bead — no comments added.
	comments := daemon.getComments()
	if len(comments) != 0 {
		t.Errorf("expected 0 comments for non-matching MR, got %d", len(comments))
	}
}

func TestGitLabWebhookHandler_PipelineEvent_NoMR(t *testing.T) {
	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	event := map[string]any{
		"object_kind": "pipeline",
		"object_attributes": map[string]any{
			"id":     456,
			"status": "success",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// No MR in event — should be a no-op.
}

func TestGitLabWebhookHandler_NoteEvent_NudgesAgent(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Title:    "Fix auth",
		Type:     "task",
		Assignee: "agent-worker-1",
		Fields:   map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
	}

	var nudgedAgent, nudgedMessage string
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge: func(_ context.Context, agent, msg string) error {
			nudgedAgent = agent
			nudgedMessage = msg
			return nil
		},
		Logger: slog.Default(),
	})

	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "reviewer-bob"},
		"object_attributes": map[string]any{
			"note":          "Please handle the error on line 42",
			"noteable_type": "MergeRequest",
			"system":        false,
			"discussion_id": "disc-abc123",
			"position": map[string]any{
				"new_path": "pkg/auth/handler.go",
				"new_line": 42,
				"old_path": "pkg/auth/handler.go",
				"old_line": 40,
			},
		},
		"merge_request": map[string]any{
			"iid":   42,
			"url":   "https://gitlab.com/org/repo/-/merge_requests/42",
			"title": "Fix auth token refresh",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if nudgedAgent != "agent-worker-1" {
		t.Errorf("nudged agent=%q, want agent-worker-1", nudgedAgent)
	}

	// Verify rich nudge message content.
	if !strings.Contains(nudgedMessage, "reviewer-bob") {
		t.Errorf("nudge message should contain reviewer name, got: %s", nudgedMessage)
	}
	if !strings.Contains(nudgedMessage, "pkg/auth/handler.go:42") {
		t.Errorf("nudge message should contain file:line, got: %s", nudgedMessage)
	}
	if !strings.Contains(nudgedMessage, "Please handle the error on line 42") {
		t.Errorf("nudge message should contain comment text, got: %s", nudgedMessage)
	}
	if !strings.Contains(nudgedMessage, "disc-abc123") {
		t.Errorf("nudge message should contain discussion ID, got: %s", nudgedMessage)
	}
	if !strings.Contains(nudgedMessage, "Fix auth token refresh") {
		t.Errorf("nudge message should contain MR title, got: %s", nudgedMessage)
	}
	if !strings.Contains(nudgedMessage, "bead-1") {
		t.Errorf("nudge message should contain bead ID, got: %s", nudgedMessage)
	}
	if !strings.Contains(nudgedMessage, "[old line 40]") {
		t.Errorf("nudge message should contain old line context, got: %s", nudgedMessage)
	}
}

// TestGitLabWebhookHandler_NoteEvent_UsesTargetedMRLookup pins the note path to
// a per-MR, server-side-filtered task lookup.
//
// It previously called ListTaskBeadsAll and filtered in Go, which fetched the
// ENTIRE task corpus at full BeadDetail on EVERY MR comment. In prod that was
// 10k rows out of 52,371 matches into a 128Mi container: the gitlab-bridge
// OOMKilled 19 times in 19h, and each crash dropped in-flight webhook
// deliveries that GitLab does not retry — so @gardenio mentions were lost. The
// 10k cap also silently truncated the match set, a correctness bug independent
// of the memory spike. See ka-0000007917 and advice ka-0000003555.
func TestGitLabWebhookHandler_NoteEvent_UsesTargetedMRLookup(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/42"

	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Title:    "Fix auth",
		Type:     "task",
		Assignee: "agent-worker-1",
		Fields:   map[string]string{"mr_url": mrURL},
	}
	// A task bound to a DIFFERENT MR must never receive this comment.
	daemon.beads["bead-other"] = &beadsapi.BeadDetail{
		ID:       "bead-other",
		Title:    "Unrelated work",
		Type:     "task",
		Assignee: "agent-worker-2",
		Fields:   map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/99"},
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge:         func(_ context.Context, _, _ string) error { return nil },
		Logger:        slog.Default(),
	})

	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "reviewer-bob"},
		"object_attributes": map[string]any{
			"note":          "@gardeniobot please look",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{"iid": 42, "url": mrURL},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	listAll, listForMR, args := daemon.listCallCounts()
	if listAll != 0 {
		t.Errorf("note path called ListTaskBeadsAll %d times, want 0 — the "+
			"unbounded whole-corpus fetch is what OOMKilled the bridge (ka-0000007917)", listAll)
	}
	if listForMR != 1 {
		t.Fatalf("note path called ListTaskBeadsForMR %d times, want 1", listForMR)
	}
	if args[0] != mrURL {
		t.Errorf("ListTaskBeadsForMR called with %q, want %q", args[0], mrURL)
	}

	// The comment must land on the bead bound to THIS MR, and only that one.
	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1: %+v", len(comments), comments)
	}
	if comments[0].BeadID != "bead-1" {
		t.Errorf("comment landed on %q, want bead-1", comments[0].BeadID)
	}
}

func TestGitLabWebhookHandler_NoteEvent_SpawnsAgentWhenNudgeFails(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Title:    "Fix auth",
		Type:     "task",
		Assignee: "dead-agent",
		Fields:   map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42"},
		Labels:   []string{"project:monorepo"},
	}

	// Nudge fails (agent is dead).
	nudgeFailed := false
	nudge := func(_ context.Context, _, _ string) error {
		nudgeFailed = true
		return fmt.Errorf("agent not found")
	}

	// Create an agent resolver backed by a mock resolver daemon.
	// No agent bead for "dead-agent" → resolver will spawn a new one.
	resolverDaemon := newMockResolverDaemon()
	resolver := newTestAgentResolver(resolverDaemon, nil)

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge:         nudge,
		AgentResolver: resolver,
		Logger:        slog.Default(),
	})

	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "reviewer-alice"},
		"object_attributes": map[string]any{
			"note":          "This null check is wrong",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{
			"iid": 42,
			"url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !nudgeFailed {
		t.Error("expected nudge to be called and fail")
	}
	// The resolver should have spawned an agent via the mock daemon.
	spawned := resolverDaemon.getSpawned()
	if len(spawned) == 0 {
		t.Error("expected agent to be spawned via resolver")
	} else {
		if spawned[0].Fields["project"] != "monorepo" {
			t.Errorf("spawn project = %q, want monorepo", spawned[0].Fields["project"])
		}
	}
}

func TestBuildReviewNudgeMessage(t *testing.T) {
	nc := noteContext{
		Author:       "alice",
		Note:         "Fix the null check",
		DiscussionID: "disc-xyz",
		Position: &notePosition{
			NewPath: "src/main.go",
			NewLine: 100,
			OldPath: "src/main.go",
			OldLine: 98,
		},
		MR: &webhookMR{
			IID:   5,
			URL:   "https://gitlab.com/org/repo/-/merge_requests/5",
			Title: "Add null checks",
		},
	}

	msg := buildReviewNudgeMessage(nc, "bead-42")

	expected := []string{
		`"Add null checks"`,
		"@alice",
		"File: src/main.go:100",
		"[old line 98]",
		"Comment: Fix the null check",
		"Discussion ID: disc-xyz",
		"MR: https://gitlab.com/org/repo/-/merge_requests/5",
		"Bead: bead-42",
	}
	for _, want := range expected {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\ngot: %s", want, msg)
		}
	}
}

func TestBuildReviewNudgeMessage_NoPosition(t *testing.T) {
	nc := noteContext{
		Author: "bob",
		Note:   "LGTM",
		MR: &webhookMR{
			URL: "https://gitlab.com/org/repo/-/merge_requests/10",
		},
	}

	msg := buildReviewNudgeMessage(nc, "bead-99")

	if strings.Contains(msg, "File:") {
		t.Errorf("message should not contain File when position is nil, got: %s", msg)
	}
	if !strings.Contains(msg, "Comment: LGTM") {
		t.Errorf("message missing comment, got: %s", msg)
	}
	if strings.Contains(msg, "Discussion ID:") {
		t.Errorf("message should not contain Discussion ID when empty, got: %s", msg)
	}
}

// TestIntegration_WebhookToAgentSpawn exercises the full flow:
// GitLab webhook → bead comment stored → mr_has_review_comments set → agent spawned.
func TestIntegration_WebhookToAgentSpawn(t *testing.T) {
	// Set up mock beads daemon with a task bead that has an MR URL.
	daemon := newMockGitLabDaemon()
	daemon.beads["gd-task-1"] = &beadsapi.BeadDetail{
		ID:       "gd-task-1",
		Title:    "Implement auth refactor",
		Type:     "task",
		Assignee: "auth-refactor-bot",
		Fields:   map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/55"},
		Labels:   []string{"project:gardenio"},
	}

	// Nudge will fail (agent is dead) — this forces the resolver to spawn.
	nudge := func(_ context.Context, _, _ string) error {
		return fmt.Errorf("agent pod not running")
	}

	// Set up mock resolver daemon (no agent bead → will spawn).
	resolverDaemon := newMockResolverDaemon()
	resolver := newTestAgentResolver(resolverDaemon, nil)

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "test-secret",
		BotUsername:   "gardenio-bot",
		Nudge:         nudge,
		AgentResolver: resolver,
		Logger:        slog.Default(),
	})

	// Simulate GitLab webhook: a reviewer posts a comment on the MR.
	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "code-reviewer"},
		"object_attributes": map[string]any{
			"note":          "@gardenio-bot The error handling in `handleAuth` is incomplete — missing the nil check for `session`.",
			"noteable_type": "MergeRequest",
			"system":        false,
			"position": map[string]any{
				"new_path": "internal/auth/handler.go",
				"new_line": 42,
				"old_path": "internal/auth/handler.go",
				"old_line": 40,
			},
			"discussion_id": "disc-abc123",
		},
		"merge_request": map[string]any{
			"iid":   55,
			"url":   "https://gitlab.com/org/repo/-/merge_requests/55",
			"title": "Refactor auth module",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "test-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify: comment was stored on the bead.
	comments := daemon.getComments()
	if len(comments) == 0 {
		t.Fatal("expected at least one comment stored on bead")
	}
	found := false
	for _, c := range comments {
		if c.BeadID == "gd-task-1" && strings.Contains(c.Text, "nil check") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected comment containing 'nil check' on bead gd-task-1")
	}

	// Verify: mr_has_review_comments was set to true.
	bead := daemon.getBead("gd-task-1")
	if bead.Fields["mr_has_review_comments"] != "true" {
		t.Errorf("mr_has_review_comments = %q, want true", bead.Fields["mr_has_review_comments"])
	}

	// Verify: agent was spawned via the resolver.
	spawned := resolverDaemon.getSpawned()
	if len(spawned) == 0 {
		t.Fatal("expected agent to be spawned via resolver")
	}
	agent := spawned[0]
	if agent.Fields["agent"] != "auth-refactor-bot" {
		t.Errorf("spawned agent name = %q, want auth-refactor-bot", agent.Fields["agent"])
	}
	if agent.Fields["project"] != "gardenio" {
		t.Errorf("spawned agent project = %q, want gardenio", agent.Fields["project"])
	}
	if agent.Fields["task_id"] != "gd-task-1" {
		t.Errorf("spawned agent task_id = %q, want gd-task-1", agent.Fields["task_id"])
	}
	if agent.Fields["spawn_source"] != "gitlab-mr-review" {
		t.Errorf("spawn_source = %q, want gitlab-mr-review", agent.Fields["spawn_source"])
	}
}

// TestIntegration_BotNoteSelfFilter verifies bot-generated notes are ignored.
func TestIntegration_BotNoteSelfFilter(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["gd-task-1"] = &beadsapi.BeadDetail{
		ID:     "gd-task-1",
		Type:   "task",
		Fields: map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/10"},
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		BotUsername:   "gardenio-bot",
		Logger:        slog.Default(),
	})

	// Bot's own note should be ignored.
	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "gardenio-bot"},
		"object_attributes": map[string]any{
			"note":          "Agent response posted",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{
			"iid": 10,
			"url": "https://gitlab.com/org/repo/-/merge_requests/10",
		},
	}
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// No comment should be stored — the bot's own note is filtered.
	comments := daemon.getComments()
	if len(comments) != 0 {
		t.Errorf("expected no comments (bot note should be filtered), got %d", len(comments))
	}
}

// makeEmojiWebhookPayload builds a GitLab emoji webhook payload for testing.
func makeEmojiWebhookPayload(emojiName, eventType, awardableType, noteBody, username, mrURL, sourceBranch string, mrIID, noteID int) []byte {
	event := map[string]any{
		"object_kind": "emoji",
		"event_type":  eventType,
		"user": map[string]any{
			"username": username,
			"name":     "Test User",
		},
		"project_id": 12345,
		"object_attributes": map[string]any{
			"awardable_id":   noteID,
			"awardable_type": awardableType,
			"name":           emojiName,
			"action":         eventType,
		},
		"merge_request": map[string]any{
			"iid":           mrIID,
			"url":           mrURL,
			"title":         "Test MR",
			"source_branch": sourceBranch,
		},
	}
	if noteBody != "" {
		event["note"] = map[string]any{
			"id":   noteID,
			"note": noteBody,
		}
	}
	body, _ := json.Marshal(event)
	return body
}

func TestGitLabWebhookHandler_EmojiEvent_TriggersE2ERerun(t *testing.T) {
	// Mock GitLab API server that accepts pipeline creation and note posting.
	var createdPipeline bool
	var postedNote bool
	gitlabSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pipelines") && r.Method == http.MethodPost:
			createdPipeline = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":999,"status":"created","web_url":"https://gitlab.com/test/-/pipelines/999"}`)
		case strings.Contains(r.URL.Path, "/notes") && r.Method == http.MethodPost:
			postedNote = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":1,"body":"ack"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gitlabSrv.Close()

	gitlab := NewGitLabClient(GitLabClientConfig{
		BaseURL: gitlabSrv.URL,
		Token:   "test-token",
		Logger:  slog.Default(),
	})

	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		GitLab:        gitlab,
		Daemon:        daemon,
		WebhookSecret: "secret",
		Logger:        slog.Default(),
	})

	noteBody := "<!-- e2e-test-summary -->\n### E2E Test Results ✅\n**50/50** tests passed"
	body := makeEmojiWebhookPayload(
		"runner", "award", "Note", noteBody,
		"dev-user", "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/500",
		"agent/test/fix-bug", 500, 42,
	)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"kind":"emoji"`) {
		t.Errorf("expected emoji kind in response, got %s", w.Body.String())
	}
	if !createdPipeline {
		t.Error("expected pipeline to be created")
	}
	if !postedNote {
		t.Error("expected acknowledgment note to be posted")
	}
}

func TestGitLabWebhookHandler_EmojiEvent_IgnoresNonRunnerEmoji(t *testing.T) {
	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Logger:        slog.Default(),
	})

	noteBody := "<!-- e2e-test-summary -->\n### E2E Test Results"
	body := makeEmojiWebhookPayload(
		"thumbsup", "award", "Note", noteBody,
		"dev-user", "https://gitlab.com/test/-/merge_requests/1",
		"main", 1, 10,
	)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Should still return processed (emoji handler returns early but doesn't error).
	if !strings.Contains(w.Body.String(), `"kind":"emoji"`) {
		t.Errorf("expected emoji kind in response, got %s", w.Body.String())
	}
}

func TestGitLabWebhookHandler_EmojiEvent_IgnoresRevokeAction(t *testing.T) {
	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Logger:        slog.Default(),
	})

	noteBody := "<!-- e2e-test-summary -->\n### E2E Test Results"
	body := makeEmojiWebhookPayload(
		"runner", "revoke", "Note", noteBody,
		"dev-user", "https://gitlab.com/test/-/merge_requests/1",
		"main", 1, 10,
	)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGitLabWebhookHandler_EmojiEvent_IgnoresNonNoteAwardable(t *testing.T) {
	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Logger:        slog.Default(),
	})

	// Emoji on the MR itself (not on a note/comment).
	body := makeEmojiWebhookPayload(
		"runner", "award", "MergeRequest", "",
		"dev-user", "https://gitlab.com/test/-/merge_requests/1",
		"main", 1, 0,
	)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGitLabWebhookHandler_EmojiEvent_IgnoresNonE2ESummaryNote(t *testing.T) {
	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Logger:        slog.Default(),
	})

	// A regular MR comment without the e2e summary marker.
	noteBody := "Looks good to me! Ship it."
	body := makeEmojiWebhookPayload(
		"runner", "award", "Note", noteBody,
		"dev-user", "https://gitlab.com/test/-/merge_requests/1",
		"main", 1, 10,
	)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGitLabWebhookHandler_EmojiEvent_FetchesNoteViaAPI(t *testing.T) {
	// Test that when the note body is NOT in the webhook payload,
	// the handler fetches it via the GitLab API.
	var fetchedNote bool
	var createdPipeline bool
	gitlabSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/notes/42") && r.Method == http.MethodGet:
			fetchedNote = true
			w.Header().Set("Content-Type", "application/json")
			body := `{"id":42,"body":"<!-- e2e-test-summary -->\n### E2E Test Results","system":false}`
			fmt.Fprint(w, body)
		case strings.HasSuffix(r.URL.Path, "/pipelines") && r.Method == http.MethodPost:
			createdPipeline = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":888,"status":"created","web_url":"https://gitlab.com/test/-/pipelines/888"}`)
		case strings.Contains(r.URL.Path, "/notes") && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":1,"body":"ack"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gitlabSrv.Close()

	gitlab := NewGitLabClient(GitLabClientConfig{
		BaseURL: gitlabSrv.URL,
		Token:   "test-token",
		Logger:  slog.Default(),
	})

	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		GitLab:        gitlab,
		Daemon:        daemon,
		WebhookSecret: "secret",
		Logger:        slog.Default(),
	})

	// Payload WITHOUT note body — handler should fetch via API.
	body := makeEmojiWebhookPayload(
		"runner", "award", "Note", "",
		"dev-user", "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/100",
		"fix/branch", 100, 42,
	)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !fetchedNote {
		t.Error("expected note to be fetched via API")
	}
	if !createdPipeline {
		t.Error("expected pipeline to be created after API note fetch")
	}
}

// --- Push webhook tests ---

func makePushPayload(branch, username string, commits []pushCommit) []byte {
	event := pushWebhookEvent{
		ObjectKind: "push",
		Ref:        "refs/heads/" + branch,
		Before:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		After:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		UserName:   username,
	}
	event.User.Username = username
	event.Project.ID = 1
	event.Project.Name = "test-repo"
	event.Project.PathNS = "org/test-repo"
	event.Commits = commits
	body, _ := json.Marshal(event)
	return body
}

func TestGitLabWebhookHandler_PushEvent_NudgesAgentViaTaskBead(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["task-1"] = &beadsapi.BeadDetail{
		ID:       "task-1",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "my-agent",
		Fields: map[string]string{
			"workspace": `{"branch":"fix/pe-1234","worktree_path":".beads/worktrees/task-1","base_branch":"origin/main"}`,
		},
	}

	var nudgedAgent, nudgedMsg string
	nudge := func(_ context.Context, agent, msg string) error {
		nudgedAgent = agent
		nudgedMsg = msg
		return nil
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge:         nudge,
		Logger:        slog.Default(),
	})

	body := makePushPayload("fix/pe-1234", "some-human", []pushCommit{
		{ID: "abc12345deadbeef", Title: "Fix the nil pointer bug", Author: struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}{Name: "Human", Email: "human@example.com"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if nudgedAgent != "my-agent" {
		t.Errorf("expected nudge to my-agent, got %q", nudgedAgent)
	}
	if !strings.Contains(nudgedMsg, "fix/pe-1234") {
		t.Errorf("nudge message should contain branch name, got %q", nudgedMsg)
	}
	if !strings.Contains(nudgedMsg, "abc12345") {
		t.Errorf("nudge message should contain commit SHA prefix, got %q", nudgedMsg)
	}
	if !strings.Contains(nudgedMsg, "Fix the nil pointer bug") {
		t.Errorf("nudge message should contain commit title, got %q", nudgedMsg)
	}
}

func TestGitLabWebhookHandler_PushEvent_NudgesAgentViaAgentBead(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.agentBeads = []*beadsapi.BeadDetail{
		{
			ID:   "agent-1",
			Type: "agent",
			Fields: map[string]string{
				"agent":                   "my-agent",
				"gitlab_mr_source_branch": "feature/new-thing",
			},
		},
	}

	var nudgedAgent string
	nudge := func(_ context.Context, agent, _ string) error {
		nudgedAgent = agent
		return nil
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge:         nudge,
		Logger:        slog.Default(),
	})

	body := makePushPayload("feature/new-thing", "human-dev", []pushCommit{
		{ID: "1234567890abcdef", Title: "Add feature X"},
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if nudgedAgent != "my-agent" {
		t.Errorf("expected nudge to my-agent, got %q", nudgedAgent)
	}
}

func TestGitLabWebhookHandler_PushEvent_SkipsMainBranch(t *testing.T) {
	daemon := newMockGitLabDaemon()
	nudged := false
	nudge := func(_ context.Context, _, _ string) error {
		nudged = true
		return nil
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge:         nudge,
		Logger:        slog.Default(),
	})

	body := makePushPayload("main", "someone", []pushCommit{
		{ID: "deadbeefdeadbeef", Title: "Merge something"},
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if nudged {
		t.Error("should not nudge on push to main branch")
	}
}

func TestGitLabWebhookHandler_PushEvent_SkipsBotUser(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["task-1"] = &beadsapi.BeadDetail{
		ID:       "task-1",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "my-agent",
		Fields: map[string]string{
			"workspace": `{"branch":"fix/pe-999"}`,
		},
	}

	nudged := false
	nudge := func(_ context.Context, _, _ string) error {
		nudged = true
		return nil
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		BotUsername:   "gardenio-bot",
		Nudge:         nudge,
		Logger:        slog.Default(),
	})

	body := makePushPayload("fix/pe-999", "gardenio-bot", []pushCommit{
		{ID: "abcdabcdabcdabcd", Title: "Auto-fix something"},
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if nudged {
		t.Error("should not nudge when push is from bot user")
	}
}

func TestGitLabWebhookHandler_PushEvent_SkipsSelfPush(t *testing.T) {
	daemon := newMockGitLabDaemon()
	daemon.beads["task-1"] = &beadsapi.BeadDetail{
		ID:       "task-1",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "agent-alice",
		Fields: map[string]string{
			"workspace": `{"branch":"fix/pe-777"}`,
		},
	}

	nudged := false
	nudge := func(_ context.Context, _, _ string) error {
		nudged = true
		return nil
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge:         nudge,
		Logger:        slog.Default(),
	})

	// Push username matches the agent's assignee — agent pushed its own code.
	body := makePushPayload("fix/pe-777", "agent-alice", []pushCommit{
		{ID: "1111111122222222", Title: "Agent's own commit"},
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if nudged {
		t.Error("should not nudge agent for its own push")
	}
}

func TestGitLabWebhookHandler_PushEvent_NoBranch(t *testing.T) {
	daemon := newMockGitLabDaemon()
	nudged := false
	nudge := func(_ context.Context, _, _ string) error {
		nudged = true
		return nil
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge:         nudge,
		Logger:        slog.Default(),
	})

	// Tag push, not a branch.
	event := pushWebhookEvent{
		ObjectKind: "push",
		Ref:        "refs/tags/v1.0.0",
		After:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if nudged {
		t.Error("should not nudge on tag push")
	}
}

func TestGitLabWebhookHandler_PushEvent_DeduplicatesNudges(t *testing.T) {
	daemon := newMockGitLabDaemon()
	// Two task beads on the same branch assigned to the same agent.
	daemon.beads["task-1"] = &beadsapi.BeadDetail{
		ID:       "task-1",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "my-agent",
		Fields: map[string]string{
			"workspace": `{"branch":"fix/pe-555"}`,
		},
	}
	daemon.beads["task-2"] = &beadsapi.BeadDetail{
		ID:       "task-2",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "my-agent",
		Fields: map[string]string{
			"workspace": `{"branch":"fix/pe-555"}`,
		},
	}

	nudgeCount := 0
	nudge := func(_ context.Context, _, _ string) error {
		nudgeCount++
		return nil
	}

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Nudge:         nudge,
		Logger:        slog.Default(),
	})

	body := makePushPayload("fix/pe-555", "human", []pushCommit{
		{ID: "aaaa1111bbbb2222", Title: "One commit"},
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if nudgeCount != 1 {
		t.Errorf("expected 1 nudge (deduplicated), got %d", nudgeCount)
	}
}

func TestBuildPushNudgeMessage(t *testing.T) {
	event := pushWebhookEvent{
		UserName: "human-user",
		Commits: []pushCommit{
			{ID: "abc12345deadbeef", Title: "Fix the bug"},
			{ID: "def67890cafebabe", Title: "Add tests"},
		},
	}
	event.User.Username = "human-user"

	msg := buildPushNudgeMessage(event, "fix/pe-100", "bead-42")
	if !strings.Contains(msg, "fix/pe-100") {
		t.Errorf("message should contain branch, got: %s", msg)
	}
	if !strings.Contains(msg, "@human-user") {
		t.Errorf("message should contain pusher, got: %s", msg)
	}
	if !strings.Contains(msg, "abc12345") {
		t.Errorf("message should contain commit SHA prefix, got: %s", msg)
	}
	if !strings.Contains(msg, "Fix the bug") {
		t.Errorf("message should contain commit title, got: %s", msg)
	}
	if !strings.Contains(msg, "bead-42") {
		t.Errorf("message should contain bead ID, got: %s", msg)
	}
	if !strings.Contains(msg, "git pull") {
		t.Errorf("message should contain pull instruction, got: %s", msg)
	}
}

// TestGitLabWebhookHandler_RejectsOversizeBody is the J2 regression test from
// AUDIT-2026-04-19 §J2 (ka-0000005263). A webhook POST whose body exceeds
// gitlabMaxBodyBytes must be rejected before full JSON decode allocates
// unbounded memory. We assert a non-2xx status and no bead mutations.
func TestGitLabWebhookHandler_RejectsOversizeBody(t *testing.T) {
	daemon := newMockGitLabDaemon()
	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	oversize := bytes.Repeat([]byte("A"), gitlabMaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(oversize))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code/100 == 2 {
		t.Errorf("expected non-2xx for oversize body, got %d", w.Code)
	}
	if got := len(daemon.getComments()); got != 0 {
		t.Errorf("daemon AddComment fired from oversize request: got %d comments", got)
	}
}

// --- type=merge-request bead dual-write tests (epic ka-0000001613, task ka-0000007548) ---

// postWebhook is a helper that sends a JSON-encoded webhook body to handler.
func postWebhook(t *testing.T, handler http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal webhook body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(raw))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// TestMRWebhook_OpenCreatesMRBeadAndProducesDep verifies that an MR "open"
// webhook creates a type=merge-request bead with status=open, populates its
// fields, and adds a `produces` dep from the producing task bead.
func TestMRWebhook_OpenCreatesMRBeadAndProducesDep(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/42"
	daemon := newMockGitLabDaemon()
	daemon.beads["task-1"] = &beadsapi.BeadDetail{
		ID:     "task-1",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())
	w := postWebhook(t, handler, map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "alice"},
		"project":     map[string]any{"path_with_namespace": "org/repo"},
		"object_attributes": map[string]any{
			"iid":               42,
			"state":             "opened",
			"action":            "open",
			"url":               mrURL,
			"target_project_id": 99,
			"title":             "Fix auth",
			"source_branch":     "fix/auth",
			"target_branch":     "main",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(upserts))
	}
	got := upserts[0]
	if got.Closed {
		t.Errorf("closed=true, want false for action=open")
	}
	if got.Fields.MRURL != mrURL || got.Fields.MRIID != "42" {
		t.Errorf("MR fields: url=%q iid=%q", got.Fields.MRURL, got.Fields.MRIID)
	}
	if got.Fields.SourceBranch != "fix/auth" || got.Fields.TargetBranch != "main" {
		t.Errorf("branches: src=%q tgt=%q", got.Fields.SourceBranch, got.Fields.TargetBranch)
	}
	if got.Fields.ProjectPath != "org/repo" {
		t.Errorf("project_path=%q, want org/repo", got.Fields.ProjectPath)
	}
	if got.Fields.MRState != "opened" || got.Fields.Title != "Fix auth" {
		t.Errorf("state=%q title=%q", got.Fields.MRState, got.Fields.Title)
	}
	if got.Fields.Author != "alice" {
		t.Errorf("author=%q, want alice", got.Fields.Author)
	}
	// With no UserDirectory wired, operated_by defaults to the gitlab:<creator>
	// sentinel so the MR bead still carries a concrete operator (ka-0000004473).
	if got.Fields.OperatedBy != "gitlab:alice" {
		t.Errorf("operated_by=%q, want gitlab:alice", got.Fields.OperatedBy)
	}

	// MR bead exists with status=open.
	mrBead := daemon.getBead(got.BeadID)
	if mrBead == nil {
		t.Fatalf("MR bead %s not found", got.BeadID)
	}
	if mrBead.Status != "open" {
		t.Errorf("MR bead status=%q, want open", mrBead.Status)
	}
	if mrBead.Type != "merge-request" {
		t.Errorf("MR bead type=%q, want merge-request", mrBead.Type)
	}

	// produces dep: task-1 → MR bead, plus the M2b triggered-by reverse edge
	// MR bead → task-1.
	deps := daemon.getDeps()
	var produces, triggeredBy *depRecord
	for i := range deps {
		switch deps[i].DepType {
		case "produces":
			produces = &deps[i]
		case "triggered-by":
			triggeredBy = &deps[i]
		}
	}
	if produces == nil {
		t.Fatalf("missing produces dep in %+v", deps)
	}
	if produces.BeadID != "task-1" || produces.DependsOnID != got.BeadID {
		t.Errorf("produces=%+v, want task-1 → %s", produces, got.BeadID)
	}
	if triggeredBy == nil {
		t.Fatalf("missing triggered-by dep in %+v", deps)
	}
	if triggeredBy.BeadID != got.BeadID || triggeredBy.DependsOnID != "task-1" {
		t.Errorf("triggered-by=%+v, want %s → task-1", triggeredBy, got.BeadID)
	}
}

// TestMRWebhook_OpenResolvesCreatorToHumanOperator pins ka-0000004473: when a
// UserDirectory is wired, the MR bead's operated_by resolves the GitLab
// creator to the human (slack:<id>) rather than the gitlab:<username>
// sentinel.
func TestMRWebhook_OpenResolvesCreatorToHumanOperator(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/77"
	daemon := newMockGitLabDaemon()
	users := &fakeUserLookup{
		users:    map[string][]string{"alice": {"mention"}},
		slackIDs: map[string]string{"alice": "U0ALICE"},
	}
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Users:         users,
		Logger:        slog.Default(),
	})
	w := postWebhook(t, handler, map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "alice"},
		"project":     map[string]any{"path_with_namespace": "org/repo"},
		"object_attributes": map[string]any{
			"iid":               77,
			"state":             "opened",
			"action":            "open",
			"url":               mrURL,
			"target_project_id": 99,
			"title":             "Fix auth",
			"source_branch":     "fix/auth",
			"target_branch":     "main",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(upserts))
	}
	if got := upserts[0].Fields.OperatedBy; got != "slack:U0ALICE" {
		t.Errorf("operated_by=%q, want slack:U0ALICE (resolved human creator)", got)
	}
}

// TestUpsertMRBead_OperatedBySetOnce pins ka-0000004473: operated_by is
// stamped at create time (the MR creator) and a later upsert whose actor is a
// different user (e.g. a merger) must NOT clobber it.
func TestUpsertMRBead_OperatedBySetOnce(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/88"
	daemon := newMockGitLabDaemon()

	// First upsert (creator alice) creates the bead with operated_by stamped.
	id, err := daemon.UpsertMergeRequestBead(context.Background(), beadsapi.MergeRequestBeadFields{
		MRURL:      mrURL,
		MRIID:      "88",
		Author:     "alice",
		OperatedBy: "gitlab:alice",
		MRState:    "opened",
	}, false)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if got := daemon.getBead(id); got == nil || got.Fields["operated_by"] != "gitlab:alice" {
		t.Fatalf("after create, operated_by=%q, want gitlab:alice", got.Fields["operated_by"])
	}

	// Second upsert (merger bob) must not overwrite the creator's operated_by.
	if _, err := daemon.UpsertMergeRequestBead(context.Background(), beadsapi.MergeRequestBeadFields{
		MRURL:      mrURL,
		MRIID:      "88",
		Author:     "bob",
		OperatedBy: "gitlab:bob",
		MRState:    "merged",
		MROutcome:  "merged",
	}, true); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got := daemon.getBead(id); got.Fields["operated_by"] != "gitlab:alice" {
		t.Errorf("after merge upsert, operated_by=%q, want gitlab:alice (set-once)", got.Fields["operated_by"])
	}
}

// TestMRWebhook_MergeClosesMRBeadAsMerged verifies that the merge action
// closes the MR bead with mr_outcome=merged. The legacy task-bead
// mr_merged/mr_state dual-writes were dropped (epic ka-0000001613 /
// task ka-0000008134); the MR bead is now the sole source of truth.
func TestMRWebhook_MergeClosesMRBeadAsMerged(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/42"
	daemon := newMockGitLabDaemon()
	daemon.beads["task-1"] = &beadsapi.BeadDetail{
		ID:     "task-1",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())
	w := postWebhook(t, handler, map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "bob"},
		"project":     map[string]any{"path_with_namespace": "org/repo"},
		"object_attributes": map[string]any{
			"iid":               42,
			"state":             "merged",
			"action":            "merge",
			"url":               mrURL,
			"target_project_id": 99,
			"title":             "Fix auth",
			"source_branch":     "fix/auth",
			"target_branch":     "main",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// Task bead must NOT carry the legacy mr_* writes (epic ka-0000001613).
	task := daemon.getBead("task-1")
	if got := task.Fields["mr_merged"]; got != "" {
		t.Errorf("task mr_merged should no longer be written, got %q", got)
	}
	if got := task.Fields["mr_state"]; got != "" {
		t.Errorf("task mr_state should no longer be written, got %q", got)
	}
	if got := task.Fields["gitlab_mr_iid"]; got != "" {
		t.Errorf("task gitlab_mr_iid should no longer be written, got %q", got)
	}

	// MR bead created, closed, mr_outcome=merged.
	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(upserts))
	}
	if !upserts[0].Closed {
		t.Errorf("closed=false, want true for action=merge")
	}
	if upserts[0].Fields.MROutcome != "merged" {
		t.Errorf("mr_outcome=%q, want merged", upserts[0].Fields.MROutcome)
	}
	mrBead := daemon.getBead(upserts[0].BeadID)
	if mrBead.Status != "closed" {
		t.Errorf("MR bead status=%q, want closed", mrBead.Status)
	}
	if mrBead.Fields["mr_outcome"] != "merged" {
		t.Errorf("MR bead mr_outcome=%q, want merged", mrBead.Fields["mr_outcome"])
	}

	// produces dep linked.
	deps := daemon.getDeps()
	if !depsContain(deps, "produces") {
		t.Errorf("deps=%+v, want a produces dep", deps)
	}
	if !depsContain(deps, "triggered-by") {
		t.Errorf("deps=%+v, want a triggered-by dep (M2b epic ka-0000000933)", deps)
	}
}

// TestMRWebhook_CloseWithoutMergeClosesMRBeadAsClosed verifies that a close
// action with state=closed (not merged) closes the MR bead with
// mr_outcome=closed.
func TestMRWebhook_CloseWithoutMergeClosesMRBeadAsClosed(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/43"
	daemon := newMockGitLabDaemon()
	daemon.beads["task-1"] = &beadsapi.BeadDetail{
		ID:     "task-1",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())
	w := postWebhook(t, handler, map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "carol"},
		"project":     map[string]any{"path_with_namespace": "org/repo"},
		"object_attributes": map[string]any{
			"iid":               43,
			"state":             "closed",
			"action":            "close",
			"url":               mrURL,
			"target_project_id": 99,
			"title":             "Abandoned change",
			"source_branch":     "feature/abandoned",
			"target_branch":     "main",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(upserts))
	}
	if !upserts[0].Closed {
		t.Errorf("closed=false, want true for action=close+state=closed")
	}
	if upserts[0].Fields.MROutcome != "closed" {
		t.Errorf("mr_outcome=%q, want closed", upserts[0].Fields.MROutcome)
	}
	mrBead := daemon.getBead(upserts[0].BeadID)
	if mrBead.Status != "closed" {
		t.Errorf("MR bead status=%q, want closed", mrBead.Status)
	}
	if mrBead.Fields["mr_outcome"] != "closed" {
		t.Errorf("MR bead mr_outcome=%q, want closed", mrBead.Fields["mr_outcome"])
	}

	deps := daemon.getDeps()
	if !depsContain(deps, "produces") {
		t.Errorf("deps=%+v, want a produces dep", deps)
	}
	if !depsContain(deps, "triggered-by") {
		t.Errorf("deps=%+v, want a triggered-by dep (M2b epic ka-0000000933)", deps)
	}
}

// TestMRWebhook_UpdateOnExistingMRBeadDoesNotRecreate verifies that a
// second webhook for the same MR finds the existing bead and updates it
// in place — does not create a duplicate.
func TestMRWebhook_UpdateOnExistingMRBeadDoesNotRecreate(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/44"
	daemon := newMockGitLabDaemon()

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())

	openEvent := map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"path_with_namespace": "org/repo"},
		"object_attributes": map[string]any{
			"iid":               44,
			"state":             "opened",
			"action":            "open",
			"url":               mrURL,
			"target_project_id": 99,
			"title":             "WIP",
			"source_branch":     "wip",
			"target_branch":     "main",
		},
	}
	if w := postWebhook(t, handler, openEvent); w.Code != http.StatusOK {
		t.Fatalf("open: status=%d", w.Code)
	}

	// Second event: update the title.
	updateEvent := map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"path_with_namespace": "org/repo"},
		"object_attributes": map[string]any{
			"iid":               44,
			"state":             "opened",
			"action":            "update",
			"url":               mrURL,
			"target_project_id": 99,
			"title":             "Ready for review",
			"source_branch":     "wip",
			"target_branch":     "main",
		},
	}
	if w := postWebhook(t, handler, updateEvent); w.Code != http.StatusOK {
		t.Fatalf("update: status=%d", w.Code)
	}

	upserts := daemon.getMRUpserts()
	if len(upserts) != 2 {
		t.Fatalf("got %d upserts, want 2", len(upserts))
	}
	if upserts[0].BeadID != upserts[1].BeadID {
		t.Errorf("second upsert created new bead %s (first was %s)",
			upserts[1].BeadID, upserts[0].BeadID)
	}

	// Count merge-request beads in the daemon — should be exactly 1.
	mrCount := 0
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == "merge-request" {
			mrCount++
		}
	}
	daemon.mu.Unlock()
	if mrCount != 1 {
		t.Errorf("got %d merge-request beads, want 1", mrCount)
	}
}

// TestHandleDescriptionSync_PrefersMRBeadState verifies that the description
// sync handler reads mr_state from the linked type=merge-request bead when
// present, in preference to the legacy task-bead mr_state field.
// See epic ka-0000001613.
func TestHandleDescriptionSync_PrefersMRBeadState(t *testing.T) {
	// GitLab stub: capture the PUT body so we can assert the rendered section.
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(GitLabMR{IID: 42, Description: "human"})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	daemon := newMockGitLabDaemon()
	// Task bead has stale mr_state=opened.
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:    "bead-1",
		Type:  "task",
		Title: "Fix auth",
		Fields: map[string]string{
			"mr_url":   "https://gitlab.com/org/repo/-/merge_requests/42",
			"mr_state": "opened",
		},
	}
	// Canonical MR bead has the current state (merged).
	daemon.beads["mr-bead-1"] = &beadsapi.BeadDetail{
		ID:     "mr-bead-1",
		Type:   "merge-request",
		Status: "closed",
		Fields: map[string]string{
			"mr_url":   "https://gitlab.com/org/repo/-/merge_requests/42",
			"mr_state": "merged",
		},
	}
	daemon.deps = append(daemon.deps, depRecord{
		BeadID: "bead-1", DependsOnID: "mr-bead-1", DepType: "produces",
	})

	s := NewGitLabSync(GitLabSyncConfig{
		GitLab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"}),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// Build an SSE bead.updated payload with a changed mr_state field.
	payload := map[string]any{
		"bead": map[string]any{
			"id":     "bead-1",
			"type":   "task",
			"title":  "Fix auth",
			"fields": daemon.beads["bead-1"].Fields,
		},
		"changes": map[string]any{
			"mr_state": "merged",
		},
	}
	data, _ := json.Marshal(payload)
	s.handleDescriptionSync(context.Background(), data)

	if gotBody == nil {
		t.Fatal("expected GitLab PUT to be called")
	}
	desc := gotBody["description"]
	if !strings.Contains(desc, "merged") {
		t.Errorf("rendered description should reference MR-bead mr_state=merged, got: %q", desc)
	}
}

// TestHandleDescriptionSync_NoMRBeadOmitsMRState verifies that when no MR
// bead is linked via `produces`, the rendered description omits the MR-state
// line — the legacy task-bead `mr_state` fallback was dropped under epic
// ka-0000001613 / task ka-0000008134.
func TestHandleDescriptionSync_NoMRBeadOmitsMRState(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(GitLabMR{IID: 42, Description: "human"})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	daemon := newMockGitLabDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:    "bead-1",
		Type:  "task",
		Title: "Fix auth",
		Fields: map[string]string{
			// task-bead mr_state set to a value that MUST NOT bleed through
			// (the fallback was dropped).
			"mr_url":   "https://gitlab.com/org/repo/-/merge_requests/42",
			"mr_state": "opened",
		},
	}
	// No MR bead linked.

	s := NewGitLabSync(GitLabSyncConfig{
		GitLab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"}),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	payload := map[string]any{
		"bead": map[string]any{
			"id":     "bead-1",
			"type":   "task",
			"title":  "Fix auth",
			"fields": daemon.beads["bead-1"].Fields,
		},
		"changes": map[string]any{
			"mr_state": "opened",
		},
	}
	data, _ := json.Marshal(payload)
	s.handleDescriptionSync(context.Background(), data)

	if gotBody == nil {
		t.Fatal("expected GitLab PUT to be called")
	}
	desc := gotBody["description"]
	// Description should not advertise the stale task-bead mr_state value.
	if strings.Contains(desc, "opened") {
		t.Errorf("rendered description should NOT carry task-bead mr_state, got: %q", desc)
	}
}

// TestHandleDescriptionSync_BugBeadOnMRURLLink is the live-path Gap A guard
// (task ka-0000004869): a type=bug bead whose mr_url is just set gets the
// managed "Agent Context" section on the SSE path. Two things this pins that
// used to fail: (1) non-task bead types are now covered (beadLinkTypes), and
// (2) an mr_url change alone triggers the sync (mrDescTriggerFields) — before,
// a freshly-linked MR with no subsequent display-field change never got the
// section live.
func TestHandleDescriptionSync_BugBeadOnMRURLLink(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(GitLabMR{IID: 1234, Description: "human, no marker"})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	daemon := newMockGitLabDaemon()
	fields := map[string]string{
		"mr_url": "https://gitlab.com/org/site-deployment/-/merge_requests/1234",
	}
	daemon.beads["bug-1"] = &beadsapi.BeadDetail{ID: "bug-1", Type: "bug", Title: "Broken thing", Fields: fields}

	s := NewGitLabSync(GitLabSyncConfig{
		GitLab:        NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"}),
		Daemon:        daemon,
		Logger:        slog.Default(),
		ViewerBaseURL: "https://viewer.example/beads",
	})

	// SSE event: mr_url just changed (the only change), on a type=bug bead.
	payload := map[string]any{
		"bead": map[string]any{
			"id":     "bug-1",
			"type":   "bug",
			"title":  "Broken thing",
			"fields": fields,
		},
		"changes": map[string]any{
			"mr_url": fields["mr_url"],
		},
	}
	data, _ := json.Marshal(payload)
	s.handleDescriptionSync(context.Background(), data)

	if gotBody == nil {
		t.Fatal("expected GitLab PUT for a bug bead whose mr_url was just linked")
	}
	desc := gotBody["description"]
	if !strings.Contains(desc, gardenioMarkerStart) {
		t.Errorf("description should contain the gardenio marker, got: %q", desc)
	}
	if !strings.Contains(desc, "bug-1") {
		t.Errorf("description should deep-link bead bug-1, got: %q", desc)
	}
}

// depsContain returns true if any recorded dep has the given DepType.
func depsContain(deps []depRecord, depType string) bool {
	for _, d := range deps {
		if d.DepType == depType {
			return true
		}
	}
	return false
}

// findDep returns the first recorded dep with the given (BeadID, DependsOnID,
// DepType) triple, or nil if none matches.
func findDep(deps []depRecord, beadID, dependsOnID, depType string) *depRecord {
	for i := range deps {
		d := deps[i]
		if d.BeadID == beadID && d.DependsOnID == dependsOnID && d.DepType == depType {
			return &d
		}
	}
	return nil
}

// TestMRBeadSync_BackfillsReverseDep pins M2b (epic ka-0000000933): when
// gitlab_sync upserts an MR bead and links it to a producing task bead, it
// writes BOTH `task → produces → MR` AND the reverse `MR → triggered-by →
// task` edge. Mirrors unit plan ka-0000004136 test 5.7.
func TestMRBeadSync_BackfillsReverseDep(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/100"
	daemon := newMockGitLabDaemon()
	daemon.beads["task-producer"] = &beadsapi.BeadDetail{
		ID:     "task-producer",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())
	w := postWebhook(t, handler, map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "alice"},
		"project":     map[string]any{"path_with_namespace": "org/repo"},
		"object_attributes": map[string]any{
			"iid":               100,
			"state":             "opened",
			"action":            "open",
			"url":               mrURL,
			"target_project_id": 77,
			"title":             "Add feature",
			"source_branch":     "feature/x",
			"target_branch":     "main",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(upserts))
	}
	mrBeadID := upserts[0].BeadID

	deps := daemon.getDeps()
	if findDep(deps, "task-producer", mrBeadID, "produces") == nil {
		t.Errorf("missing forward produces dep task-producer → %s in %+v", mrBeadID, deps)
	}
	if findDep(deps, mrBeadID, "task-producer", "triggered-by") == nil {
		t.Errorf("missing reverse triggered-by dep %s → task-producer in %+v", mrBeadID, deps)
	}
}

// TestMRBeadSync_MultiTaskMR_AllProducersLinked pins the multi-task fix:
// when several task beads point at the same MR, every one gets both edges
// — the historical first-match break is gone. Unit plan ka-0000004136
// test 5.9, M2b on epic ka-0000000933.
func TestMRBeadSync_MultiTaskMR_AllProducersLinked(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/200"
	daemon := newMockGitLabDaemon()
	daemon.beads["task-A"] = &beadsapi.BeadDetail{
		ID:     "task-A",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}
	daemon.beads["task-B"] = &beadsapi.BeadDetail{
		ID:     "task-B",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	handler := GitLabWebhookHandler(nil, daemon, "secret", slog.Default())
	w := postWebhook(t, handler, map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "bob"},
		"project":     map[string]any{"path_with_namespace": "org/repo"},
		"object_attributes": map[string]any{
			"iid":               200,
			"state":             "opened",
			"action":            "open",
			"url":               mrURL,
			"target_project_id": 77,
			"title":             "Release",
			"source_branch":     "release/x",
			"target_branch":     "main",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(upserts))
	}
	mrBeadID := upserts[0].BeadID

	deps := daemon.getDeps()
	for _, taskID := range []string{"task-A", "task-B"} {
		if findDep(deps, taskID, mrBeadID, "produces") == nil {
			t.Errorf("missing produces dep %s → %s in %+v", taskID, mrBeadID, deps)
		}
		if findDep(deps, mrBeadID, taskID, "triggered-by") == nil {
			t.Errorf("missing triggered-by dep %s → %s in %+v", mrBeadID, taskID, deps)
		}
	}
}

// --- handleUpdated guard: MR beads must not drive the MR-upsert path ---
// Regression tests for ka-0000004161 / root-cause ka-0000003609.

// mrBeadUpdateEvent builds a beads.bead.updated SSE payload for a bead of the
// given type carrying an mr_url field.
func beadUpdateEventWithMRURL(t *testing.T, id, btype, mrURL string) []byte {
	t.Helper()
	inner := map[string]any{
		"id":     id,
		"type":   btype,
		"status": "in_progress",
		"fields": map[string]string{"mr_url": mrURL},
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal bead: %v", err)
	}
	out, err := json.Marshal(map[string]json.RawMessage{"bead": innerJSON})
	if err != nil {
		t.Fatalf("marshal wrapper: %v", err)
	}
	return out
}

// TestHandleUpdated_MRBeadDoesNotSelfLoop verifies that a bead.updated event
// for a type=merge-request bead (which carries its own mr_url) is ignored by
// handleUpdated. Without the guard, handleUpdated re-entered with
// taskBeadID=the-MR-bead and wrote a bogus produces(MR→MR) self-loop.
func TestHandleUpdated_MRBeadDoesNotSelfLoop(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/42"
	daemon := newMockGitLabDaemon()
	daemon.beads["mr-bead-1"] = &beadsapi.BeadDetail{
		ID:     "mr-bead-1",
		Type:   "merge-request",
		Status: "open",
		Fields: map[string]string{"mr_url": mrURL},
	}

	// gitlab is intentionally nil: the guard must return before any GitLab
	// call, so a nil client would panic if the guard were missing.
	sync := NewGitLabSync(GitLabSyncConfig{GitLab: nil, Daemon: daemon, Logger: slog.Default()})
	sync.handleUpdated(context.Background(), beadUpdateEventWithMRURL(t, "mr-bead-1", "merge-request", mrURL))

	if ups := daemon.getMRUpserts(); len(ups) != 0 {
		t.Errorf("MR bead update triggered %d upserts, want 0: %+v", len(ups), ups)
	}
	if deps := daemon.getDeps(); len(deps) != 0 {
		t.Errorf("MR bead update created %d deps (self-loop), want 0: %+v", len(deps), deps)
	}
}

// TestHandleUpdated_TaskBeadCreatesProducesEdge verifies the genuine path: a
// bead.updated event for a task bead carrying mr_url upserts the MR bead and
// writes both produces(task→MR) and triggered-by(MR→task).
func TestHandleUpdated_TaskBeadCreatesProducesEdge(t *testing.T) {
	mrURL := "https://gitlab.com/org/repo/-/merge_requests/42"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitLabMR{
			IID:          42,
			State:        "opened",
			Title:        "Fix auth",
			WebURL:       mrURL,
			SourceBranch: "fix/gd-Task1",
			TargetBranch: "main",
			ProjectID:    99,
			Author:       &GitLabUser{Username: "alice"},
		})
	}))
	defer srv.Close()

	daemon := newMockGitLabDaemon()
	daemon.beads["gd-Task1"] = &beadsapi.BeadDetail{
		ID:     "gd-Task1",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	gitlab := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t", Logger: slog.Default()})
	sync := NewGitLabSync(GitLabSyncConfig{GitLab: gitlab, Daemon: daemon, Logger: slog.Default()})
	sync.handleUpdated(context.Background(), beadUpdateEventWithMRURL(t, "gd-Task1", "task", mrURL))

	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(upserts))
	}
	mrBeadID := upserts[0].BeadID

	deps := daemon.getDeps()
	produces := findDep(deps, "gd-Task1", mrBeadID, "produces")
	if produces == nil {
		t.Errorf("missing produces dep gd-Task1 → %s in %+v", mrBeadID, deps)
	}
	triggeredBy := findDep(deps, mrBeadID, "gd-Task1", "triggered-by")
	if triggeredBy == nil {
		t.Errorf("missing triggered-by dep %s → gd-Task1 in %+v", mrBeadID, deps)
	}
}

// TestGitLabPoller_poll_ErrorRetainsCursor pins the cursor-advance-on-error fix
// (ka-0000007753). GitLabPoller.poll used to advance its monotonic cursor to
// now() BEFORE the GitLab list call, so a transient API error permanently
// skipped every MR merged during the failed window — and that error is
// correlated with the same instability that drops the merge webhooks this
// poller is the documented fallback for.
//
// The httptest server errors on the FIRST ListMergedMRs call and returns a
// merged MR (updated inside the ORIGINAL window) on the SECOND. With the fix,
// poll leaves p.lastPoll unchanged on error, so the second tick re-scans the
// window, discovers the MR, and upserts+links the MR bead. With the bug, the
// second tick's `since` has already skipped past the merge and the MR is lost.
func TestGitLabPoller_poll_ErrorRetainsCursor(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/42"

	var mu sync.Mutex
	var calls int
	var lastUpdatedAfter string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		lastUpdatedAfter = r.URL.Query().Get("updated_after")
		mu.Unlock()

		if n == 1 {
			// Transient GitLab failure on the first poll.
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitLabMR{{
			IID:          42,
			State:        "merged",
			Title:        "Fix auth",
			WebURL:       mrURL,
			SourceBranch: "fix/gd-Task1",
			TargetBranch: "main",
			ProjectID:    99,
			Author:       &GitLabUser{Username: "alice"},
		}})
	}))
	defer srv.Close()

	daemon := newMockGitLabDaemon()
	daemon.beads["gd-Task1"] = &beadsapi.BeadDetail{
		ID:     "gd-Task1",
		Type:   "task",
		Status: "in_progress",
		Fields: map[string]string{"mr_url": mrURL},
	}

	gitlab := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t", Logger: slog.Default()})
	poller := NewGitLabPoller(GitLabPollerConfig{
		GitLab:       gitlab,
		Daemon:       daemon,
		Logger:       slog.Default(),
		GroupID:      7,
		PollInterval: time.Minute,
	})

	// Freeze the window start so both polls share the same `since` unless the
	// cursor is (incorrectly) advanced by the failed first poll.
	windowStart := poller.lastPoll
	firstWindow := windowStart.UTC().Format(time.RFC3339)

	// First poll: the API errors. The cursor MUST remain unchanged.
	poller.poll(context.Background())
	if !poller.lastPoll.Equal(windowStart) {
		t.Fatalf("poll advanced lastPoll on error: got %v, want unchanged %v", poller.lastPoll, windowStart)
	}
	if got := daemon.getMRUpserts(); len(got) != 0 {
		t.Fatalf("first (failed) poll upserted %d MR beads, want 0", len(got))
	}

	// Second poll: succeeds. It must re-scan the ORIGINAL window (same
	// updated_after) and discover the MR that merged during the failed window.
	poller.poll(context.Background())

	mu.Lock()
	gotCalls := calls
	gotWindow := lastUpdatedAfter
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("expected 2 ListMergedMRs calls, got %d", gotCalls)
	}
	if gotWindow != firstWindow {
		t.Errorf("second poll used updated_after=%q, want the retried original window %q "+
			"(cursor must not advance on error)", gotWindow, firstWindow)
	}

	upserts := daemon.getMRUpserts()
	if len(upserts) != 1 {
		t.Fatalf("got %d MR-bead upserts after retry, want 1 — the merge in the failed window was lost", len(upserts))
	}
	if upserts[0].Fields.MRState != "merged" {
		t.Errorf("upserted MR MRState=%q, want merged", upserts[0].Fields.MRState)
	}
	mrBeadID := upserts[0].BeadID

	deps := daemon.getDeps()
	if findDep(deps, "gd-Task1", mrBeadID, "produces") == nil {
		t.Errorf("missing produces dep gd-Task1 → %s in %+v", mrBeadID, deps)
	}
	if findDep(deps, mrBeadID, "gd-Task1", "triggered-by") == nil {
		t.Errorf("missing triggered-by dep %s → gd-Task1 in %+v", mrBeadID, deps)
	}

	// After a successful poll the cursor advances past the original window.
	if !poller.lastPoll.After(windowStart) {
		t.Errorf("successful poll did not advance lastPoll: got %v, want after %v", poller.lastPoll, windowStart)
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (m *mockGitLabDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (m *mockGitLabDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (m *mockGitLabDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}

// A GitHub-forge task bead's review flag is github-bridge's to act on (it mails
// the agent); the GitLab nudge must not fire a second time.
func TestHandleReviewNudge_SkipsGitHubForge(t *testing.T) {
	var nudged []string
	s := &GitLabSync{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		nudge: func(_ context.Context, agent, _ string) error {
			nudged = append(nudged, agent)
			return nil
		},
		seen: map[string]time.Time{},
	}
	data, _ := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id": "gd-task-gh", "type": "task", "assignee": "crew-x", "status": "in_progress",
			"fields": map[string]string{
				"mr_has_review_comments": "true", "forge": "github",
				"mr_url": "https://github.com/o/r/pull/3",
			},
		},
		"changes": map[string]any{"mr_has_review_comments": "true"},
	})
	s.handleReviewNudge(context.Background(), data)
	if len(nudged) != 0 {
		t.Errorf("nudged %v for a GitHub PR", nudged)
	}
}
