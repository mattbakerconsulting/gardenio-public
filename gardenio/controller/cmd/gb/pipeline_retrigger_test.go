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

// TestPipelineRetrigger_Refuses proves `gb pipeline retrigger` is retired: it
// errors with the owner rule and never reaches GitLab, so no PR pipeline is
// created (owner rule 2026-10-31, gd-lNeFr44JkM).
func TestPipelineRetrigger_Refuses(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	origURL, origToken := mrGitLabURL, mrGitLabToken
	t.Cleanup(func() { mrGitLabURL, mrGitLabToken = origURL, origToken })
	mrGitLabURL, mrGitLabToken = srv.URL, "t"

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := pipelineRetriggerCmd.RunE(cmd, []string{"https://gitlab.example.com/org/repo/-/merge_requests/42"})
	if err == nil {
		t.Fatal("gb pipeline retrigger returned nil; want a refusal")
	}
	for _, want := range []string{"never retriggered", "CI on pull requests", "retry-lost-runners.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err.Error(), want)
		}
	}
	if hits != 0 {
		t.Errorf("refusal made %d GitLab request(s); want none", hits)
	}
}

// TestCreateMergeRequestPipeline_PostsMRPipelinesEndpoint pins the client
// method to POST /projects/:id/merge_requests/:iid/pipelines with the project
// path URL-escaped and no request body (GitLab derives ref + source from the MR).
func TestCreateMergeRequestPipeline_PostsMRPipelinesEndpoint(t *testing.T) {
	var gotMethod, gotPath, gotRawURI, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotRawURI = r.Method, r.URL.Path, r.RequestURI
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bridge.GitLabPipeline{ID: 555, Status: "created", Source: "merge_request_event"})
	}))
	defer srv.Close()

	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "t"})

	pip, err := client.CreateMergeRequestPipeline(context.Background(), "Acme/Platform/monorepo", 1945)
	if err != nil {
		t.Fatalf("CreateMergeRequestPipeline: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	// The project path must reach GitLab percent-encoded — an unescaped
	// "group/sub/repo" would be read as extra path segments. net/http decodes
	// r.URL.Path, so assert the escaping on the raw request URI.
	wantRaw := "/api/v4/projects/Acme%2FPlatform%2Fmonorepo/merge_requests/1945/pipelines"
	if gotRawURI != wantRaw {
		t.Errorf("raw request URI = %q, want %q", gotRawURI, wantRaw)
	}
	wantPath := "/api/v4/projects/Acme/Platform/monorepo/merge_requests/1945/pipelines"
	if gotPath != wantPath {
		t.Errorf("decoded path = %q, want %q", gotPath, wantPath)
	}
	if gotBody != "" {
		t.Errorf("body = %q, want empty (GitLab derives ref/source from the MR)", gotBody)
	}
	if pip.ID != 555 || pip.Source != "merge_request_event" {
		t.Errorf("pipeline = %+v, want ID 555 source merge_request_event", pip)
	}
}
