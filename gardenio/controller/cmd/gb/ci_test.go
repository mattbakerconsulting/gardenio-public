package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/bridge"
)

// TestBuildWhyFailedReport exercises the pure aggregation logic that merges
// GitLab CI jobs and external commit statuses into a single report.
func TestBuildWhyFailedReport(t *testing.T) {
	pip := &bridge.GitLabPipeline{
		ID:     2472545231,
		Status: "failed",
		SHA:    "8f3fb5ce",
		WebURL: "https://gitlab.com/Acme/Platform/gardenio/-/pipelines/2472545231",
	}

	// Mirrors the real SHA 8f3fb5ce scenario from the bead: 4 GitLab jobs all
	// green, and 2 external commit statuses red.
	jobs := []bridge.GitLabJob{
		{ID: 1, Name: "changes", Stage: "detect", Status: "success", WebURL: "https://example/jobs/1"},
		{ID: 2, Name: "e2e-dispatch", Stage: "e2e", Status: "success"},
		{ID: 3, Name: "e2e-results", Stage: "e2e", Status: "success"},
		{ID: 4, Name: "cancel-stale-statuses", Stage: "cleanup", Status: "success"},
	}

	statuses := []bridge.GitLabCommitStatus{
		// Duplicates of GitLab jobs — should be dropped by de-dup.
		{ID: 101, Name: "changes", Status: "success"},
		{ID: 102, Name: "e2e-dispatch", Status: "success"},
		// External hard failures pushed by another CI runner.
		{
			ID:           201,
			Name:         "ext: Deploy: gardenio-e2e",
			Status:       "failed",
			AllowFailure: false,
			TargetURL:    "https://ci.example/deploy/abc",
			Description:  "task exited non-zero",
		},
		{
			ID:           202,
			Name:         "ext: docker.yml (push)",
			Status:       "failed",
			AllowFailure: false,
			TargetURL:    "https://ci.example/docker/def",
		},
		// An allow_failure external status — present but NOT a hard failure.
		{ID: 203, Name: "ext: nightly-bench", Status: "failed", AllowFailure: true},
	}

	report := buildWhyFailedReport("Acme/Platform/gardenio", pip.ID, pip, jobs, statuses)

	if report.PipelineStatus != "failed" {
		t.Errorf("PipelineStatus = %q, want failed", report.PipelineStatus)
	}
	if report.SHA != "8f3fb5ce" {
		t.Errorf("SHA = %q, want 8f3fb5ce", report.SHA)
	}
	if report.jobCount != 4 {
		t.Errorf("jobCount = %d, want 4", report.jobCount)
	}

	// AllEntries = 4 jobs + 3 non-duplicate statuses = 7
	if got := len(report.AllEntries); got != 7 {
		t.Fatalf("AllEntries has %d entries, want 7: %+v", got, report.AllEntries)
	}

	// Hard failures = the 2 non-allow_failure external entries.
	if got := len(report.Failures); got != 2 {
		t.Fatalf("Failures has %d entries, want 2: %+v", got, report.Failures)
	}

	wantFailureNames := map[string]bool{
		"ext: Deploy: gardenio-e2e": true,
		"ext: docker.yml (push)":    true,
	}
	for _, f := range report.Failures {
		if !wantFailureNames[f.Name] {
			t.Errorf("unexpected failure: %q", f.Name)
		}
		if f.Source != "commit-status" {
			t.Errorf("failure %q: source = %q, want commit-status", f.Name, f.Source)
		}
		if f.AllowFailure {
			t.Errorf("failure %q has AllowFailure=true — should be excluded", f.Name)
		}
		if f.TargetURL == "" {
			t.Errorf("failure %q missing TargetURL — gardeners need the external run link", f.Name)
		}
	}

	// Sanity check: de-dup'd status "changes" should NOT be in the report as
	// a commit-status entry — only as a gitlab-job entry.
	for _, e := range report.AllEntries {
		if e.Name == "changes" && e.Source == "commit-status" {
			t.Errorf("commit-status entry for %q leaked past de-dup; GitLab job is canonical", e.Name)
		}
	}
}

// TestBuildWhyFailedReport_AllGreen verifies no failures are reported when
// every GitLab job and every external status is green.
func TestBuildWhyFailedReport_AllGreen(t *testing.T) {
	pip := &bridge.GitLabPipeline{ID: 1, Status: "success", SHA: "abc123"}
	jobs := []bridge.GitLabJob{{ID: 1, Name: "build", Status: "success"}}
	statuses := []bridge.GitLabCommitStatus{
		{ID: 2, Name: "ext: smoke", Status: "success"},
	}

	report := buildWhyFailedReport("owner/repo", 1, pip, jobs, statuses)

	if len(report.Failures) != 0 {
		t.Errorf("expected 0 failures, got %d: %+v", len(report.Failures), report.Failures)
	}
	if len(report.AllEntries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(report.AllEntries))
	}
}

// TestBuildWhyFailedReport_NilPipeline defends against a nil pipeline pointer
// (e.g. partial fetch) — the report should still surface jobs/statuses.
func TestBuildWhyFailedReport_NilPipeline(t *testing.T) {
	jobs := []bridge.GitLabJob{{ID: 1, Name: "build", Status: "failed"}}
	statuses := []bridge.GitLabCommitStatus{}

	report := buildWhyFailedReport("owner/repo", 42, nil, jobs, statuses)

	if report.PipelineID != 42 {
		t.Errorf("PipelineID = %d, want 42", report.PipelineID)
	}
	if len(report.Failures) != 1 {
		t.Errorf("expected 1 failure, got %d", len(report.Failures))
	}
	if report.SHA != "" {
		t.Errorf("SHA = %q, want empty when pipeline is nil", report.SHA)
	}
}

// TestWhyFailedEndToEnd_MockGitLab wires the aggregation behind mocked GitLab
// responses to confirm the whole pipeline (fetch pipeline → list jobs → list
// commit statuses → aggregate) returns a coherent report.
func TestWhyFailedEndToEnd_MockGitLab(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pipelines/999") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(bridge.GitLabPipeline{
				ID: 999, Status: "failed", SHA: "deadbeef",
				WebURL: "https://gitlab.example/pipelines/999",
			})
		case strings.Contains(r.URL.Path, "/pipelines/999/jobs"):
			_ = json.NewEncoder(w).Encode([]bridge.GitLabJob{
				{ID: 1, Name: "lint", Status: "success"},
			})
		case strings.Contains(r.URL.Path, "/repository/commits/deadbeef/statuses"):
			_ = json.NewEncoder(w).Encode([]bridge.GitLabCommitStatus{
				{ID: 10, Name: "lint", Status: "success"},
				{ID: 11, Name: "ext: docker.yml (push)", Status: "failed", TargetURL: "https://ci.example/r/xyz"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "token"})
	ctx := t.Context()

	pip, err := c.GetPipelineByPath(ctx, "owner/repo", 999)
	if err != nil {
		t.Fatalf("GetPipelineByPath: %v", err)
	}
	jobs, err := c.ListPipelineJobsByPath(ctx, "owner/repo", 999)
	if err != nil {
		t.Fatalf("ListPipelineJobsByPath: %v", err)
	}
	statuses, err := c.ListCommitStatusesByPath(ctx, "owner/repo", pip.SHA)
	if err != nil {
		t.Fatalf("ListCommitStatusesByPath: %v", err)
	}

	report := buildWhyFailedReport("owner/repo", 999, pip, jobs, statuses)

	if len(report.Failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(report.Failures))
	}
	fail := report.Failures[0]
	if fail.Name != "ext: docker.yml (push)" {
		t.Errorf("failure.Name = %q, want ext: docker.yml (push)", fail.Name)
	}
	if fail.TargetURL != "https://ci.example/r/xyz" {
		t.Errorf("failure.TargetURL = %q, want external run link", fail.TargetURL)
	}
	if fail.Source != "commit-status" {
		t.Errorf("failure.Source = %q, want commit-status", fail.Source)
	}
}
