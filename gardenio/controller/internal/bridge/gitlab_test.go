package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestGitLabClient creates a GitLabClient pointing at a test server.
func newTestGitLabClient(url string) *GitLabClient {
	return NewGitLabClient(GitLabClientConfig{
		BaseURL: url, Token: "test-token", Logger: slog.Default(),
	})
}

func TestGitLabClient_GetMergeRequestByPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// httptest receives the raw (un-decoded) request; Go's net/http
		// decodes %2F to / in r.URL.Path. Use RawPath to check encoding.
		wantRaw := "/api/v4/projects/Acme%2FPlatform%2Ffics-helm-chart/merge_requests/211"
		if r.URL.RawPath != wantRaw && r.URL.Path != "/api/v4/projects/Acme/Platform/acme-helm-chart/merge_requests/211" {
			t.Errorf("unexpected path: raw=%s decoded=%s", r.URL.RawPath, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		resp := GitLabMR{IID: 211, State: "opened", ProjectID: 99}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	mr, err := client.GetMergeRequestByPath(context.Background(), "Acme/Platform/acme-helm-chart", 211)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mr.State != "opened" {
		t.Errorf("State = %q, want opened", mr.State)
	}
}

func TestGitLabClient_ListMergedMRs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/groups/10/merge_requests" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("state") != "merged" {
			t.Error("expected state=merged query param")
		}
		if r.URL.Query().Get("updated_after") == "" {
			t.Error("expected updated_after query param")
		}
		resp := []GitLabMR{
			{IID: 210, State: "merged", ProjectID: 42, Title: "MR 210"},
			{IID: 211, State: "merged", ProjectID: 42, Title: "MR 211"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	after, _ := time.Parse(time.RFC3339, "2026-03-01T00:00:00Z")
	mrs, err := client.ListMergedMRs(context.Background(), 10, after)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mrs) != 2 {
		t.Fatalf("expected 2 MRs, got %d", len(mrs))
	}
	if mrs[0].IID != 210 {
		t.Errorf("first MR IID = %d, want 210", mrs[0].IID)
	}
}

func TestGitLabClient_ErrorHandling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	_, err := client.GetMergeRequestByPath(context.Background(), "group/repo", 999)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestGitLabClient_PostMRNote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Go's net/http decodes %2F, so check both forms.
		wantPath := "/api/v4/projects/Acme/Platform/monorepo/merge_requests/42/notes"
		if r.URL.Path != wantPath {
			t.Errorf("unexpected path: %s, want %s", r.URL.Path, wantPath)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s, want POST", r.Method)
		}
		if r.Header.Get("PRIVATE-TOKEN") != "test-token" {
			t.Error("missing or wrong PRIVATE-TOKEN header")
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["body"] != "LGTM with one nit" {
			t.Errorf("unexpected note body: %q", body["body"])
		}
		resp := GitLabNote{
			ID:        1001,
			Body:      body["body"],
			Author:    GitLabUser{ID: 1, Username: "gardenio-bot"},
			CreatedAt: "2026-03-13T18:00:00Z",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	note, err := client.PostMRNote(context.Background(), "Acme/Platform/monorepo", 42, "LGTM with one nit")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if note.ID != 1001 {
		t.Errorf("note ID = %d, want 1001", note.ID)
	}
	if note.Body != "LGTM with one nit" {
		t.Errorf("note Body = %q, want %q", note.Body, "LGTM with one nit")
	}
}

func TestGitLabClient_PostMRDiscussionReply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/v4/projects/org/repo/merge_requests/7/discussions/abc123def/notes"
		if r.URL.Path != wantPath {
			t.Errorf("unexpected path: %s, want %s", r.URL.Path, wantPath)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s, want POST", r.Method)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		resp := GitLabNote{
			ID:        2002,
			Body:      body["body"],
			Author:    GitLabUser{ID: 1, Username: "gardenio-bot"},
			CreatedAt: "2026-03-13T18:05:00Z",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	note, err := client.PostMRDiscussionReply(context.Background(), "org/repo", 7, "abc123def", "Fixed in latest push")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if note.ID != 2002 {
		t.Errorf("note ID = %d, want 2002", note.ID)
	}
	if note.Body != "Fixed in latest push" {
		t.Errorf("note Body = %q, want %q", note.Body, "Fixed in latest push")
	}
}

func TestGitLabClient_PostMRNote_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"403 Forbidden"}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	_, err := client.PostMRNote(context.Background(), "org/repo", 42, "test")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestGitLabClient_ListMRNotes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/v4/projects/org/repo/merge_requests/10/notes"
		if r.URL.Path != wantPath {
			t.Errorf("unexpected path: %s, want %s", r.URL.Path, wantPath)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s, want GET", r.Method)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Error("expected per_page=100 query param")
		}
		if r.URL.Query().Get("sort") != "desc" {
			t.Error("expected sort=desc query param")
		}
		resp := []GitLabNote{
			{ID: 101, Body: "Please fix the null check", Author: GitLabUser{ID: 5, Username: "reviewer"}, System: false, CreatedAt: "2026-03-13T18:00:00Z"},
			{ID: 100, Body: "added 1 commit", Author: GitLabUser{ID: 1, Username: "gardenio-bot"}, System: true, CreatedAt: "2026-03-13T17:55:00Z"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	notes, err := client.ListMRNotes(context.Background(), "org/repo", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(notes))
	}
	if notes[0].ID != 101 {
		t.Errorf("first note ID = %d, want 101", notes[0].ID)
	}
	if notes[0].Body != "Please fix the null check" {
		t.Errorf("first note Body = %q", notes[0].Body)
	}
	if notes[1].System != true {
		t.Error("second note should be a system note")
	}
}

func TestGitLabClient_ListMRNotes_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	_, err := client.ListMRNotes(context.Background(), "org/nonexistent", 999)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestGitLabClient_GetLatestTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Go's net/http decodes %2F, so check decoded path.
		wantPath := "/api/v4/projects/Acme/Platform/gardenio/repository/tags"
		if r.URL.Path != wantPath {
			t.Errorf("unexpected path: %s, want %s", r.URL.Path, wantPath)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Error("expected per_page=100 query param")
		}
		tags := []GitLabTag{
			{Name: "2026.100.1"},
			{Name: "not-a-calver"},
			{Name: "2026.106.3"},
			{Name: "2026.106.2"},
			{Name: "2025.365.1"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tags)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	tag, err := client.GetLatestTag(context.Background(), "Acme/Platform/gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "2026.106.3" {
		t.Errorf("got tag=%q, want 2026.106.3", tag)
	}
}

func TestGitLabClient_GetLatestTag_VPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tags := []GitLabTag{
			{Name: "v2026.100.1"},
			{Name: "v2026.106.5"},
			{Name: "v2026.106.4"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tags)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	tag, err := client.GetLatestTag(context.Background(), "org/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "v2026.106.5" {
		t.Errorf("got tag=%q, want v2026.106.5", tag)
	}
}

func TestGitLabClient_GetLatestTag_NoCalver(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tags := []GitLabTag{
			{Name: "latest"},
			{Name: "release-candidate"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tags)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	_, err := client.GetLatestTag(context.Background(), "org/repo")
	if err == nil {
		t.Fatal("expected error for no calver tags")
	}
}

func TestGitLabClient_GetLatestTag_NoTags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitLabTag{})
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	_, err := client.GetLatestTag(context.Background(), "org/repo")
	if err == nil {
		t.Fatal("expected error for no tags")
	}
}

func TestGitLabClient_CompareRefs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/v4/projects/Acme/Platform/gardenio/repository/compare"
		if r.URL.Path != wantPath {
			t.Errorf("unexpected path: %s, want %s", r.URL.Path, wantPath)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("from") != "2026.106.3" {
			t.Errorf("from=%q, want 2026.106.3", r.URL.Query().Get("from"))
		}
		if r.URL.Query().Get("to") != "main" {
			t.Errorf("to=%q, want main", r.URL.Query().Get("to"))
		}
		// Raw body mirroring a real GitLab /repository/compare response:
		// top-level keys are commit, commits, diffs, compare_timeout,
		// compare_same_ref, web_url — note NO commit_count. Written as a
		// literal rather than a marshalled GitLabCompare so the mock cannot
		// invent response fields the API does not send (ka-0000003891).
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"commit": {"id": "ccc333", "title": "chore: bump"},
			"commits": [
				{"id": "aaa111", "title": "fix: thing", "message": "fix: thing\n\ndetails", "author_name": "Alice", "committed_date": "2026-03-13T18:00:00Z", "created_at": "2026-03-13T18:00:00Z"},
				{"id": "bbb222", "title": "feat: new", "message": "feat: new", "author_name": "Bob"},
				{"id": "ccc333", "title": "chore: bump", "message": "chore: bump", "author_name": "Carol"}
			],
			"diffs": [
				{"old_path": "gardenio/controller/main.go", "new_path": "gardenio/controller/main.go"},
				{"old_path": "", "new_path": "gardenio/images/agent/new.sh", "new_file": true}
			],
			"compare_timeout": false,
			"compare_same_ref": false,
			"web_url": "https://gitlab.com/Acme/Platform/gardenio/-/compare/2026.106.3...main"
		}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	cmp, err := client.CompareRefs(context.Background(), "Acme/Platform/gardenio", "2026.106.3", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The count must be derived from the commits array, since the real API
	// never sends a commit_count field.
	if cmp.CommitCount() != 3 {
		t.Errorf("CommitCount()=%d, want 3 (len of commits array)", cmp.CommitCount())
	}
	if len(cmp.Commits) != 3 {
		t.Fatalf("got %d commits, want 3", len(cmp.Commits))
	}
	if cmp.Commits[0].ID != "aaa111" {
		t.Errorf("first commit ID=%q, want aaa111", cmp.Commits[0].ID)
	}
	if cmp.Commits[0].Author != "Alice" {
		t.Errorf("first commit Author=%q, want Alice", cmp.Commits[0].Author)
	}
	// committed_date feeds the delivery-freshness idle window (ka-0000003628).
	if cmp.Commits[0].CommittedDate != "2026-03-13T18:00:00Z" {
		t.Errorf("first commit CommittedDate=%q, want 2026-03-13T18:00:00Z", cmp.Commits[0].CommittedDate)
	}
	if len(cmp.Diffs) != 2 {
		t.Fatalf("got %d diffs, want 2", len(cmp.Diffs))
	}
	if !cmp.Diffs[1].NewFile {
		t.Error("second diff should be a new file")
	}
}

func TestGitLabClient_CompareRefs_NoDiff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Identical refs: GitLab returns an empty commits array and sets
		// compare_same_ref. Again, no commit_count key.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"commit": {}, "commits": [], "diffs": [], "compare_timeout": false, "compare_same_ref": true}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	cmp, err := client.CompareRefs(context.Background(), "org/repo", "v1.0.0", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmp.CommitCount() != 0 {
		t.Errorf("CommitCount()=%d, want 0", cmp.CommitCount())
	}
}

// TestGitLabCompare_CommitCount_IgnoresPhantomCommitCountField is the
// regression guard for ka-0000003891: `gb release status` reported
// main_commits_ahead=0 while main was 8 commits ahead of the latest tag,
// because GitLabCompare declared a `commit_count` field that GitLab's
// /repository/compare API never returns. Decoding a realistic body (no
// commit_count) must still yield the true count, and a body that DOES carry
// a stray commit_count must not be trusted over the commits array.
func TestGitLabCompare_CommitCount_IgnoresPhantomCommitCountField(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "realistic body without commit_count",
			body: `{"commits": [{"id": "a"}, {"id": "b"}, {"id": "c"}], "diffs": []}`,
			want: 3,
		},
		{
			name: "stray commit_count is ignored in favour of the commits array",
			body: `{"commit_count": 0, "commits": [{"id": "a"}, {"id": "b"}], "diffs": []}`,
			want: 2,
		},
		{
			name: "empty range",
			body: `{"commits": [], "diffs": []}`,
			want: 0,
		},
		{
			name: "absent commits key",
			body: `{"diffs": []}`,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cmp GitLabCompare
			if err := json.Unmarshal([]byte(tt.body), &cmp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := cmp.CommitCount(); got != tt.want {
				t.Errorf("CommitCount()=%d, want %d", got, tt.want)
			}
		})
	}
}

// TestGitLabCompare_DiffsTruncated is the regression guard for ka-0000004642:
// GitLab's /repository/compare caps the diffs array at 3000 files and signals
// it via compare_timeout=true, so any diffs-derived count (ImageAheadBy) is a
// lower bound, not exact. Verified live against Acme/Platform/gardenio
// (root...main: 16,130 commits, exactly 3000 diffs, compare_timeout=true; a
// recent tag...main: 570 commits, 744 diffs, compare_timeout=false).
func TestGitLabCompare_DiffsTruncated(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantTrun bool
	}{
		{
			name:     "small diff, flag false",
			body:     `{"commits": [{"id": "a"}], "diffs": [{"new_path": "x"}], "compare_timeout": false}`,
			wantTrun: false,
		},
		{
			name:     "compare_timeout flag set — trust the API",
			body:     `{"commits": [{"id": "a"}], "diffs": [{"new_path": "x"}], "compare_timeout": true}`,
			wantTrun: true,
		},
		{
			name:     "flag absent (default false), small diff",
			body:     `{"commits": [{"id": "a"}], "diffs": [{"new_path": "x"}]}`,
			wantTrun: false,
		},
		{
			name:     "diffs at the cap with flag omitted — len heuristic catches it",
			body:     compareBodyWithNDiffs(gitlabCompareDiffCap, false),
			wantTrun: true,
		},
		{
			name:     "diffs at the cap with flag set",
			body:     compareBodyWithNDiffs(gitlabCompareDiffCap, true),
			wantTrun: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cmp GitLabCompare
			if err := json.Unmarshal([]byte(tt.body), &cmp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := cmp.DiffsTruncated(); got != tt.wantTrun {
				t.Errorf("DiffsTruncated()=%v, want %v", got, tt.wantTrun)
			}
			// The commits array is never truncated, so CommitCount stays exact
			// regardless of the diffs cap.
			if got := cmp.CommitCount(); got != len(cmp.Commits) {
				t.Errorf("CommitCount()=%d, want %d", got, len(cmp.Commits))
			}
		})
	}
}

// compareBodyWithNDiffs builds a compare response JSON body with n diff
// entries and the given compare_timeout flag, for exercising the len==cap
// truncation heuristic.
func compareBodyWithNDiffs(n int, timeout bool) string {
	diffs := make([]string, n)
	for i := range diffs {
		diffs[i] = `{"new_path": "f"}`
	}
	return fmt.Sprintf(`{"commits": [{"id": "a"}], "diffs": [%s], "compare_timeout": %t}`,
		strings.Join(diffs, ","), timeout)
}

func TestGitLabClient_CompareRefs_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	_, err := client.CompareRefs(context.Background(), "org/nonexistent", "v1", "main")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestParseMRURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantPath string
		wantIID  int
		wantNil  bool
	}{
		{
			name:     "standard MR URL",
			url:      "https://gitlab.com/Acme/Platform/acme-helm-chart/-/merge_requests/211",
			wantPath: "Acme/Platform/acme-helm-chart",
			wantIID:  211,
		},
		{
			name:     "simple project",
			url:      "https://gitlab.com/org/repo/-/merge_requests/42",
			wantPath: "org/repo",
			wantIID:  42,
		},
		{
			name:     "deeply nested project",
			url:      "https://gitlab.example.com/a/b/c/d/-/merge_requests/1",
			wantPath: "a/b/c/d",
			wantIID:  1,
		},
		{
			name:    "not a MR URL",
			url:     "https://gitlab.com/org/repo/-/issues/42",
			wantNil: true,
		},
		{
			name:    "empty string",
			url:     "",
			wantNil: true,
		},
		{
			name:    "github URL",
			url:     "https://github.com/org/repo/pull/42",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := ParseMRURL(tt.url)
			if tt.wantNil {
				if ref != nil {
					t.Errorf("expected nil, got %+v", ref)
				}
				return
			}
			if ref == nil {
				t.Fatal("expected non-nil MRRef")
			}
			if ref.ProjectPath != tt.wantPath {
				t.Errorf("ProjectPath = %q, want %q", ref.ProjectPath, tt.wantPath)
			}
			if ref.IID != tt.wantIID {
				t.Errorf("IID = %d, want %d", ref.IID, tt.wantIID)
			}
		})
	}
}

// TestGitLabClient_BranchExists is the post-merge cleanup verification check
// for bug ka-0000004927: `--remove-source-branch` reports "✓ Merged!" even when
// the delete is silently refused, so BranchExists asks GitLab directly. A 200
// means the branch survived; a 404 is the positive "cleaned up" signal; any
// other status must surface as an error so an unreachable API is never mistaken
// for a deleted branch.
func TestGitLabClient_BranchExists(t *testing.T) {
	const proj = "acme/platform/gardenio"
	const branch = "feat/gd-abc-thing"
	// url.PathEscape escapes the project path's slashes to %2F and the branch's
	// slash likewise, so the raw request path carries both encoded.
	wantPath := "/api/v4/projects/" + url.PathEscape(proj) + "/repository/branches/" + url.PathEscape(branch)

	tests := []struct {
		name       string
		status     int
		body       string
		wantExists bool
		wantErr    bool
	}{
		{name: "branch present → exists", status: 200, body: `{"name":"feat/gd-abc-thing"}`, wantExists: true},
		{name: "branch gone → 404 is cleaned-up, not an error", status: 404, body: `{"message":"404 Branch Not Found"}`, wantExists: false},
		{name: "auth failure → error, never a false clean", status: 401, body: `{"message":"401 Unauthorized"}`, wantErr: true},
		{name: "server error → error, never a false clean", status: 500, body: `{"message":"boom"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.RequestURI // raw, before %2F decoding
				if r.Header.Get("PRIVATE-TOKEN") != "test-token" {
					t.Error("missing or wrong PRIVATE-TOKEN header")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := newTestGitLabClient(server.URL)
			exists, err := client.BranchExists(context.Background(), proj, branch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for status %d, got exists=%v", tt.status, exists)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if exists != tt.wantExists {
				t.Errorf("exists = %v, want %v", exists, tt.wantExists)
			}
			if gotPath != wantPath {
				t.Errorf("request path = %q, want %q", gotPath, wantPath)
			}
		})
	}
}
