package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// TestResolvePipelineInfo_PipelineURLFastPath proves that a full pipeline URL
// resolves without any GitLab call and returns the base URL derived from the
// URL host — even with no --gitlab-url/GITLAB_BASE_URL set (bug ka-0000008229).
func TestResolvePipelineInfo_PipelineURLFastPath(t *testing.T) {
	origURL := mrGitLabURL
	t.Cleanup(func() { mrGitLabURL = origURL })
	mrGitLabURL = ""

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	proj, pid, baseURL, err := resolvePipelineInfo(cmd, "https://gitlab.example.com/org/repo/-/pipelines/12345")
	if err != nil {
		t.Fatalf("resolvePipelineInfo pipeline URL: %v", err)
	}
	if proj != "org/repo" {
		t.Errorf("projectPath = %q, want org/repo", proj)
	}
	if pid != 12345 {
		t.Errorf("pipelineID = %d, want 12345", pid)
	}
	if baseURL != "https://gitlab.example.com" {
		t.Errorf("baseURL = %q, want https://gitlab.example.com", baseURL)
	}
}

// TestResolvePipelineInfo_MRURLDerivesBaseURL proves that an MR URL is resolved
// against the GitLab instance whose host is the MR URL, with no config set: the
// derived base URL routes GetMergeRequestByPath to the httptest server and the
// MR's head pipeline id + the derived base URL are returned.
func TestResolvePipelineInfo_MRURLDerivesBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bridge.GitLabMR{
			IID:          7,
			HeadPipeline: &bridge.GitLabPipRef{ID: 999, Status: "running"},
		})
	}))
	defer srv.Close()

	origURL, origToken := mrGitLabURL, mrGitLabToken
	t.Cleanup(func() { mrGitLabURL, mrGitLabToken = origURL, origToken })
	mrGitLabURL = ""
	mrGitLabToken = "tok"

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	mrURL := srv.URL + "/owner/repo/-/merge_requests/7"
	proj, pid, baseURL, err := resolvePipelineInfo(cmd, mrURL)
	if err != nil {
		t.Fatalf("resolvePipelineInfo MR URL: %v", err)
	}
	if proj != "owner/repo" {
		t.Errorf("projectPath = %q, want owner/repo", proj)
	}
	if pid != 999 {
		t.Errorf("pipelineID = %d, want 999 (MR head pipeline)", pid)
	}
	if baseURL != srv.URL {
		t.Errorf("baseURL = %q, want %q (derived from MR URL host)", baseURL, srv.URL)
	}
	if want := "/api/v4/projects/owner/repo/merge_requests/7"; gotPath != want {
		t.Errorf("API path = %q, want %q", gotPath, want)
	}
}

// TestResolvePipelineInfo_UnparseableMRURL covers the ParseMRURL-nil branch: a
// URL arg that is not a pipeline URL and not a recognizable MR URL.
func TestResolvePipelineInfo_UnparseableMRURL(t *testing.T) {
	origURL := mrGitLabURL
	t.Cleanup(func() { mrGitLabURL = origURL })
	mrGitLabURL = ""

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	_, _, _, err := resolvePipelineInfo(cmd, "https://gitlab.example.com/org/repo")
	if err == nil || !strings.Contains(err.Error(), "could not parse MR URL") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

// TestResolvePipelineInfo_MRFetchError covers the GitLab-fetch error branch.
func TestResolvePipelineInfo_MRFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	origURL, origToken := mrGitLabURL, mrGitLabToken
	t.Cleanup(func() { mrGitLabURL, mrGitLabToken = origURL, origToken })
	mrGitLabURL = ""
	mrGitLabToken = "tok"

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	_, _, _, err := resolvePipelineInfo(cmd, srv.URL+"/owner/repo/-/merge_requests/7")
	if err == nil || !strings.Contains(err.Error(), "fetching MR") {
		t.Fatalf("expected fetch error, got %v", err)
	}
}

// TestResolvePipelineInfo_MRWithoutPipeline surfaces the no-pipeline error path.
func TestResolvePipelineInfo_MRWithoutPipeline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bridge.GitLabMR{IID: 7}) // no head pipeline
	}))
	defer srv.Close()

	origURL, origToken := mrGitLabURL, mrGitLabToken
	t.Cleanup(func() { mrGitLabURL, mrGitLabToken = origURL, origToken })
	mrGitLabURL = ""
	mrGitLabToken = "tok"

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	_, _, _, err := resolvePipelineInfo(cmd, srv.URL+"/owner/repo/-/merge_requests/7")
	if err == nil || !strings.Contains(err.Error(), "no pipeline") {
		t.Fatalf("expected 'no pipeline' error, got %v", err)
	}
}
