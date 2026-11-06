package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetPipelineByPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/v4/projects/Acme%2FPlatform%2Fgardenio/pipelines/2472545231"
		if r.URL.EscapedPath() != wantPath {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      2472545231,
			"status":  "failed",
			"ref":     "main",
			"sha":     "8f3fb5cedeadbeefcafefeedfacebeefdeadfeed",
			"web_url": "https://gitlab.com/Acme/Platform/gardenio/-/pipelines/2472545231",
		})
	}))
	defer srv.Close()

	c := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t", Logger: slog.Default()})
	pip, err := c.GetPipelineByPath(context.Background(), "Acme/Platform/gardenio", 2472545231)
	if err != nil {
		t.Fatalf("GetPipelineByPath: %v", err)
	}
	if pip.Status != "failed" || pip.SHA == "" {
		t.Errorf("got %+v", pip)
	}
}

func TestListCommitStatusesByPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repository/commits/8f3fb5ce/statuses") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "name": "changes", "status": "success", "allow_failure": false},
			{"id": 2, "name": "ext: Deploy: gardenio-e2e", "status": "failed", "allow_failure": false, "target_url": "https://ci.example/run/abc"},
			{"id": 3, "name": "ext: docker.yml (push)", "status": "failed", "allow_failure": false},
		})
	}))
	defer srv.Close()

	c := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t", Logger: slog.Default()})
	got, err := c.ListCommitStatusesByPath(context.Background(), "Acme/Platform/gardenio", "8f3fb5ce")
	if err != nil {
		t.Fatalf("ListCommitStatusesByPath: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 statuses, got %d", len(got))
	}
	var failed int
	for _, s := range got {
		if s.Status == "failed" {
			failed++
		}
	}
	if failed != 2 {
		t.Errorf("want 2 failed, got %d", failed)
	}
	if got[1].TargetURL == "" {
		t.Errorf("TargetURL lost: %+v", got[1])
	}
}
