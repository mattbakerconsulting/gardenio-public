package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetUnreleasedData(t *testing.T) {
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo1/tags":
			writeJSON(w, []ghTag{{Name: "2026.59.1"}})
		case "/repos/org/repo1/compare/2026.59.1...main":
			writeJSON(w, ghCompare{
				AheadBy: 2,
				Commits: []struct {
					SHA    string `json:"sha"`
					Commit struct {
						Message string `json:"message"`
						Author  struct {
							Name string `json:"name"`
						} `json:"author"`
					} `json:"commit"`
				}{
					{SHA: "aaa1111222233334444", Commit: struct {
						Message string `json:"message"`
						Author  struct {
							Name string `json:"name"`
						} `json:"author"`
					}{Message: "feat: new thing\n\ndetails", Author: struct {
						Name string `json:"name"`
					}{Name: "Alice"}}},
					{SHA: "bbb5555666677778888", Commit: struct {
						Message string `json:"message"`
						Author  struct {
							Name string `json:"name"`
						} `json:"author"`
					}{Message: "fix: old thing", Author: struct {
						Name string `json:"name"`
					}{Name: "Bob"}}},
				},
			})
		case "/repos/org/repo2/tags":
			writeJSON(w, []ghTag{{Name: "2026.60.1"}})
		case "/repos/org/repo2/compare/2026.60.1...main":
			writeJSON(w, ghCompare{AheadBy: 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()

	client := newTestGitHubClient(ghSrv.URL, "")

	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		GitHub: client,
		Repos: []RepoRef{
			{Owner: "org", Repo: "repo1"},
			{Owner: "org", Repo: "repo2", External: true},
		},
		Version: "test-v1",
	})

	if resp.Bridge != "test-v1" {
		t.Errorf("got bridge=%q, want test-v1", resp.Bridge)
	}
	if len(resp.Repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(resp.Repos))
	}

	// repo1: 2 unreleased commits, not external.
	r1 := resp.Repos[0]
	if r1.Repo != "org/repo1" {
		t.Errorf("repo1: got repo=%q, want org/repo1", r1.Repo)
	}
	if r1.AheadBy != 2 {
		t.Errorf("repo1: got aheadBy=%d, want 2", r1.AheadBy)
	}
	if r1.LatestTag != "2026.59.1" {
		t.Errorf("repo1: got latestTag=%q, want 2026.59.1", r1.LatestTag)
	}
	if r1.External {
		t.Errorf("repo1: expected External=false")
	}
	if len(r1.Commits) != 2 {
		t.Fatalf("repo1: got %d commits, want 2", len(r1.Commits))
	}
	// firstLine should strip multi-line commit messages.
	if r1.Commits[0].Message != "feat: new thing" {
		t.Errorf("repo1: commit[0] message=%q, want 'feat: new thing'", r1.Commits[0].Message)
	}

	// repo2: up to date, external dep.
	r2 := resp.Repos[1]
	if r2.AheadBy != 0 {
		t.Errorf("repo2: got aheadBy=%d, want 0", r2.AheadBy)
	}
	if !r2.External {
		t.Errorf("repo2: expected External=true")
	}
}

func TestGetUnreleasedData_NoGitHub(t *testing.T) {
	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		Repos:   []RepoRef{{Owner: "org", Repo: "r"}},
		Version: "v0",
	})
	if len(resp.Repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(resp.Repos))
	}
	// With nil GitHub client, repos slice is allocated but empty.
	if resp.Repos[0].AheadBy != 0 {
		t.Errorf("expected zero-value repo entry")
	}
}

func TestGetUnreleasedData_WithControllerVersion(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			writeJSON(w, controllerVersionInfo{
				Version:    "v3.0.0",
				Commit:     "deadbeef12345678",
				AgentImage: "agent:latest",
				Namespace:  "gardenio",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ctrlSrv.Close()

	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		ControllerURL: ctrlSrv.URL,
		Version:       "v1",
	})

	if resp.Cluster == nil {
		t.Fatal("expected cluster info")
	}
	if resp.Cluster.Version != "v3.0.0" {
		t.Errorf("cluster version=%q, want v3.0.0", resp.Cluster.Version)
	}
	if resp.Cluster.Namespace != "gardenio" {
		t.Errorf("cluster namespace=%q, want gardenio", resp.Cluster.Namespace)
	}
}

func TestHandleUnreleased(t *testing.T) {
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo/tags":
			writeJSON(w, []ghTag{{Name: "2026.59.1"}})
		case "/repos/org/repo/compare/2026.59.1...main":
			writeJSON(w, ghCompare{AheadBy: 1, Commits: []struct {
				SHA    string `json:"sha"`
				Commit struct {
					Message string `json:"message"`
					Author  struct {
						Name string `json:"name"`
					} `json:"author"`
				} `json:"commit"`
			}{{SHA: "abc1234", Commit: struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
				} `json:"author"`
			}{Message: "fix: thing", Author: struct {
				Name string `json:"name"`
			}{Name: "Dev"}}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()

	handler := HandleUnreleased(UnreleasedConfig{
		GitHub:  newTestGitHubClient(ghSrv.URL, ""),
		Repos:   []RepoRef{{Owner: "org", Repo: "repo"}},
		Version: "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/unreleased", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}

	var resp UnreleasedResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Bridge != "test" {
		t.Errorf("bridge=%q, want test", resp.Bridge)
	}
	if len(resp.Repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(resp.Repos))
	}
	if resp.Repos[0].AheadBy != 1 {
		t.Errorf("aheadBy=%d, want 1", resp.Repos[0].AheadBy)
	}
}

func TestGetUnreleasedData_WithImages(t *testing.T) {
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo/compare/2026.60.1...main":
			writeJSON(w, map[string]any{
				"ahead_by": 3,
				"commits": []map[string]any{
					{"sha": "aaa111", "commit": map[string]any{
						"message": "fix(agent): update entrypoint",
						"author":  map[string]any{"name": "Alice"},
					}},
					{"sha": "bbb222", "commit": map[string]any{
						"message": "feat(bridge): new endpoint",
						"author":  map[string]any{"name": "Bob"},
					}},
					{"sha": "ccc333", "commit": map[string]any{
						"message": "chore: bump versions",
						"author":  map[string]any{"name": "Carol"},
					}},
				},
				"files": []map[string]any{
					{"filename": "images/agent/entrypoint.sh", "status": "modified"},
					{"filename": "controller/internal/bridge/new.go", "status": "added"},
					{"filename": "gardenio/images/agent/install/versions.env", "status": "modified"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()

	client := newTestGitHubClient(ghSrv.URL, "")

	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		GitHub:  client,
		Version: "test-v1",
		Images: []ImageTrackConfig{
			{
				Name:  "agent",
				Repo:  RepoRef{Owner: "org", Repo: "repo"},
				Tag:   "2026.60.1",
				Paths: []string{"images/agent/", "gardenio/images/agent/"},
			},
		},
	})

	if len(resp.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(resp.Images))
	}

	img := resp.Images[0]
	if img.Name != "agent" {
		t.Errorf("image name=%q, want agent", img.Name)
	}
	if img.DeployedTag != "2026.60.1" {
		t.Errorf("deployedTag=%q, want 2026.60.1", img.DeployedTag)
	}
	if img.AheadBy != 3 {
		t.Errorf("aheadBy=%d, want 3", img.AheadBy)
	}
	if img.ImageAheadBy != 2 {
		t.Errorf("imageAheadBy=%d, want 2", img.ImageAheadBy)
	}
	if len(img.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(img.Files))
	}
	if img.Error != "" {
		t.Errorf("unexpected error: %s", img.Error)
	}
}

func TestGetUnreleasedData_ImagesNoChanges(t *testing.T) {
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ahead_by": 0,
			"commits":  []map[string]any{},
			"files":    []map[string]any{},
		})
	}))
	defer ghSrv.Close()

	client := newTestGitHubClient(ghSrv.URL, "")

	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		GitHub:  client,
		Version: "v1",
		Images: []ImageTrackConfig{
			{
				Name:  "agent",
				Repo:  RepoRef{Owner: "org", Repo: "repo"},
				Tag:   "2026.63.1",
				Paths: []string{"images/agent/"},
			},
		},
	})

	if len(resp.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(resp.Images))
	}
	if resp.Images[0].ImageAheadBy != 0 {
		t.Errorf("imageAheadBy=%d, want 0", resp.Images[0].ImageAheadBy)
	}
	if resp.Images[0].AheadBy != 0 {
		t.Errorf("aheadBy=%d, want 0", resp.Images[0].AheadBy)
	}
}

func TestHandleUnreleased_WithImages(t *testing.T) {
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo/tags":
			writeJSON(w, []ghTag{{Name: "2026.60.1"}})
		case "/repos/org/repo/compare/2026.60.1...main":
			writeJSON(w, map[string]any{
				"ahead_by": 1,
				"commits": []map[string]any{
					{"sha": "abc1234", "commit": map[string]any{
						"message": "fix: agent Dockerfile",
						"author":  map[string]any{"name": "Dev"},
					}},
				},
				"files": []map[string]any{
					{"filename": "images/agent/Dockerfile", "status": "modified"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()

	handler := HandleUnreleased(UnreleasedConfig{
		GitHub:  newTestGitHubClient(ghSrv.URL, ""),
		Repos:   []RepoRef{{Owner: "org", Repo: "repo"}},
		Version: "test",
		Images: []ImageTrackConfig{
			{
				Name:  "agent",
				Repo:  RepoRef{Owner: "org", Repo: "repo"},
				Tag:   "2026.60.1",
				Paths: []string{"images/agent/"},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/unreleased", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	var resp UnreleasedResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(resp.Images))
	}
	if resp.Images[0].Name != "agent" {
		t.Errorf("image name=%q, want agent", resp.Images[0].Name)
	}
	if resp.Images[0].ImageAheadBy != 1 {
		t.Errorf("imageAheadBy=%d, want 1", resp.Images[0].ImageAheadBy)
	}
	if len(resp.Images[0].Files) != 1 {
		t.Errorf("got %d files, want 1", len(resp.Images[0].Files))
	}
}

func TestNewGitHubClientIfConfigured(t *testing.T) {
	logger := slog.Default()

	if c := NewGitHubClientIfConfigured("", nil, logger); c != nil {
		t.Error("expected nil with no token and no repos")
	}
	if c := NewGitHubClientIfConfigured("tok", nil, logger); c == nil {
		t.Error("expected non-nil with token")
	}
	if c := NewGitHubClientIfConfigured("", []RepoRef{{Owner: "o", Repo: "r"}}, logger); c == nil {
		t.Error("expected non-nil with repos")
	}
}

// --- GitLab-backed unreleased tests ---

// gitlabTagsAndCompareHandler returns an httptest handler that serves
// GitLab tags and compare API responses for unreleased testing.
func gitlabTagsAndCompareHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Go's net/http decodes %2F in paths, so match decoded paths.
		switch r.URL.Path {
		case "/api/v4/projects/Acme/Platform/gardenio/repository/tags":
			writeJSON(w, []GitLabTag{
				{Name: "2026.106.7"},
				{Name: "2026.106.6"},
				{Name: "not-a-tag"},
			})
		case "/api/v4/projects/Acme/Platform/gardenio/repository/compare":
			from := r.URL.Query().Get("from")
			to := r.URL.Query().Get("to")
			if from == "2026.106.7" && to == "main" {
				writeJSON(w, GitLabCompare{
					Commits: []GitLabCompareCommit{
						{ID: "aaa111222233334444", Title: "feat: new thing", Message: "feat: new thing\n\ndetails", Author: "Alice"},
						{ID: "bbb555666677778888", Title: "fix: old thing", Message: "fix: old thing", Author: "Bob"},
					},
					Diffs: []GitLabDiff{
						{NewPath: "gardenio/controller/main.go"},
						{NewPath: "gardenio/images/agent/entrypoint.sh"},
					},
				})
			} else {
				writeJSON(w, GitLabCompare{})
			}
		case "/api/v4/projects/org/other-repo/repository/tags":
			writeJSON(w, []GitLabTag{{Name: "2026.100.1"}})
		case "/api/v4/projects/org/other-repo/repository/compare":
			writeJSON(w, GitLabCompare{})
		default:
			http.NotFound(w, r)
		}
	})
}

func TestGetUnreleasedData_GitLab(t *testing.T) {
	glSrv := httptest.NewServer(gitlabTagsAndCompareHandler(t))
	defer glSrv.Close()

	client := newTestGitLabClient(glSrv.URL)

	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		GitLab: client,
		Projects: []ProjectRef{
			{Path: "Acme/Platform/gardenio"},
			{Path: "org/other-repo", External: true},
		},
		Version: "test-v1",
	})

	if resp.Bridge != "test-v1" {
		t.Errorf("got bridge=%q, want test-v1", resp.Bridge)
	}
	if len(resp.Repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(resp.Repos))
	}

	// First project: 2 unreleased commits.
	r1 := resp.Repos[0]
	if r1.Repo != "Acme/Platform/gardenio" {
		t.Errorf("repo1: got repo=%q, want Acme/Platform/gardenio", r1.Repo)
	}
	if r1.AheadBy != 2 {
		t.Errorf("repo1: got aheadBy=%d, want 2", r1.AheadBy)
	}
	if r1.LatestTag != "2026.106.7" {
		t.Errorf("repo1: got latestTag=%q, want 2026.106.7", r1.LatestTag)
	}
	if r1.External {
		t.Error("repo1: expected External=false")
	}
	if len(r1.Commits) != 2 {
		t.Fatalf("repo1: got %d commits, want 2", len(r1.Commits))
	}
	// firstLine should strip multi-line commit messages.
	if r1.Commits[0].Message != "feat: new thing" {
		t.Errorf("repo1: commit[0] message=%q, want 'feat: new thing'", r1.Commits[0].Message)
	}
	if r1.Error != "" {
		t.Errorf("repo1: unexpected error: %s", r1.Error)
	}

	// Second project: up to date, external dep.
	r2 := resp.Repos[1]
	if r2.AheadBy != 0 {
		t.Errorf("repo2: got aheadBy=%d, want 0", r2.AheadBy)
	}
	if !r2.External {
		t.Error("repo2: expected External=true")
	}
}

func TestGetUnreleasedData_GitLab_NoClient(t *testing.T) {
	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		Projects: []ProjectRef{{Path: "org/repo"}},
		Version:  "v0",
	})
	if len(resp.Repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(resp.Repos))
	}
	// With nil GitLab and nil GitHub, repos slice is allocated but zero-valued.
	if resp.Repos[0].AheadBy != 0 {
		t.Errorf("expected zero-value repo entry")
	}
}

func TestGetUnreleasedData_GitLab_WithImages(t *testing.T) {
	glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/projects/Acme/Platform/gardenio/repository/compare" {
			writeJSON(w, GitLabCompare{
				Commits: []GitLabCompareCommit{
					{ID: "aaa111", Message: "fix(agent): update entrypoint", Author: "Alice"},
					{ID: "bbb222", Message: "feat(bridge): new endpoint", Author: "Bob"},
					{ID: "ccc333", Message: "chore: bump versions", Author: "Carol"},
				},
				Diffs: []GitLabDiff{
					{NewPath: "gardenio/images/agent/entrypoint.sh"},
					{NewPath: "gardenio/controller/internal/bridge/new.go"},
					{NewPath: "gardenio/images/agent/install/versions.env"},
				},
			})
		} else {
			http.NotFound(w, r)
		}
	}))
	defer glSrv.Close()

	client := newTestGitLabClient(glSrv.URL)

	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		GitLab:  client,
		Version: "test-v1",
		Images: []ImageTrackConfig{
			{
				Name:        "agent",
				ProjectPath: "Acme/Platform/gardenio",
				Tag:         "2026.106.3",
				Paths:       []string{"gardenio/images/agent/", "images/agent/"},
			},
		},
	})

	if len(resp.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(resp.Images))
	}

	img := resp.Images[0]
	if img.Name != "agent" {
		t.Errorf("image name=%q, want agent", img.Name)
	}
	if img.DeployedTag != "2026.106.3" {
		t.Errorf("deployedTag=%q, want 2026.106.3", img.DeployedTag)
	}
	if img.AheadBy != 3 {
		t.Errorf("aheadBy=%d, want 3", img.AheadBy)
	}
	if img.ImageAheadBy != 2 {
		t.Errorf("imageAheadBy=%d, want 2 (agent entrypoint + agent-node.lock)", img.ImageAheadBy)
	}
	if len(img.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(img.Files))
	}
	if img.Error != "" {
		t.Errorf("unexpected error: %s", img.Error)
	}
}

// TestGetUnreleasedData_GitLab_ImagesTruncated is the regression guard for
// ka-0000004642: when GitLab caps the compare diffs array (compare_timeout=true),
// ImageAheadBy under-reports, so the entry must carry FilesTruncated=true to let
// consumers render the count as a lower bound rather than an exact figure.
func TestGetUnreleasedData_GitLab_ImagesTruncated(t *testing.T) {
	glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/projects/Acme/Platform/gardenio/repository/compare" {
			// Mirror a real truncated compare response: commits array intact,
			// diffs capped, compare_timeout flag set.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"commits": [{"id": "aaa111", "author_name": "Alice"}, {"id": "bbb222", "author_name": "Bob"}],
				"diffs": [
					{"new_path": "gardenio/images/agent/entrypoint.sh"},
					{"new_path": "gardenio/images/agent/install/versions.env"}
				],
				"compare_timeout": true,
				"compare_same_ref": false
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer glSrv.Close()

	client := newTestGitLabClient(glSrv.URL)

	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		GitLab:  client,
		Version: "test-v1",
		Images: []ImageTrackConfig{
			{
				Name:        "agent",
				ProjectPath: "Acme/Platform/gardenio",
				Tag:         "2026.106.3",
				Paths:       []string{"gardenio/images/agent/"},
			},
		},
	})

	if len(resp.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(resp.Images))
	}
	img := resp.Images[0]
	if !img.FilesTruncated {
		t.Error("expected FilesTruncated=true when compare_timeout is set")
	}
	// Both diffs match the agent path prefix, so the (lower-bound) count is 2.
	if img.ImageAheadBy != 2 {
		t.Errorf("imageAheadBy=%d, want 2 (lower bound)", img.ImageAheadBy)
	}
	// Commits are never truncated, so the commit count stays exact.
	if img.AheadBy != 2 {
		t.Errorf("aheadBy=%d, want 2", img.AheadBy)
	}
}

func TestGetUnreleasedData_GitLab_ImagesNoChanges(t *testing.T) {
	glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, GitLabCompare{})
	}))
	defer glSrv.Close()

	client := newTestGitLabClient(glSrv.URL)

	resp := GetUnreleasedData(context.Background(), UnreleasedConfig{
		GitLab:  client,
		Version: "v1",
		Images: []ImageTrackConfig{
			{
				Name:        "agent",
				ProjectPath: "Acme/Platform/gardenio",
				Tag:         "2026.106.7",
				Paths:       []string{"gardenio/images/agent/"},
			},
		},
	})

	if len(resp.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(resp.Images))
	}
	if resp.Images[0].ImageAheadBy != 0 {
		t.Errorf("imageAheadBy=%d, want 0", resp.Images[0].ImageAheadBy)
	}
	if resp.Images[0].AheadBy != 0 {
		t.Errorf("aheadBy=%d, want 0", resp.Images[0].AheadBy)
	}
}

func TestHandleUnreleased_GitLab(t *testing.T) {
	glSrv := httptest.NewServer(gitlabTagsAndCompareHandler(t))
	defer glSrv.Close()

	client := newTestGitLabClient(glSrv.URL)

	handler := HandleUnreleased(UnreleasedConfig{
		GitLab:   client,
		Projects: []ProjectRef{{Path: "Acme/Platform/gardenio"}},
		Version:  "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/unreleased", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}

	var resp UnreleasedResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Bridge != "test" {
		t.Errorf("bridge=%q, want test", resp.Bridge)
	}
	if len(resp.Repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(resp.Repos))
	}
	if resp.Repos[0].AheadBy != 2 {
		t.Errorf("aheadBy=%d, want 2", resp.Repos[0].AheadBy)
	}
}

func TestGitLabMonorepoImageConfigs(t *testing.T) {
	t.Run("with tag", func(t *testing.T) {
		configs := GitLabMonorepoImageConfigs("Acme/Platform/gardenio", "2026.106.7")
		if len(configs) != 6 {
			t.Fatalf("got %d configs, want 6", len(configs))
		}
		if configs[0].Name != "agent" {
			t.Errorf("configs[0].Name=%q, want agent", configs[0].Name)
		}
		if configs[0].ProjectPath != "Acme/Platform/gardenio" {
			t.Errorf("configs[0].ProjectPath=%q, want Acme/Platform/gardenio", configs[0].ProjectPath)
		}
		if configs[0].Tag != "2026.106.7" {
			t.Errorf("configs[0].Tag=%q, want 2026.106.7", configs[0].Tag)
		}
	})

	t.Run("empty tag", func(t *testing.T) {
		configs := GitLabMonorepoImageConfigs("Acme/Platform/gardenio", "")
		if len(configs) != 0 {
			t.Fatalf("got %d configs, want 0", len(configs))
		}
	})
}
