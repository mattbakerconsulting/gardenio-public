package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitLabClient_GetMergeRequestDiffStats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v4/projects/grp%2Frepo/merge_requests/7/changes" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 3 added (+a,+b,+c), 1 removed (-x); the +++/--- headers must be excluded.
		_, _ = w.Write([]byte(`{"changes":[
			{"diff":"--- a/foo\n+++ b/foo\n@@ -1,2 +1,4 @@\n+a\n+b\n-x\n unchanged\n","new_file":false},
			{"diff":"+++ b/bar\n+c\n","new_file":true}
		]}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	stats, err := client.GetMergeRequestDiffStats(context.Background(), "grp/repo", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Additions != 3 {
		t.Errorf("Additions = %d, want 3", stats.Additions)
	}
	if stats.Deletions != 1 {
		t.Errorf("Deletions = %d, want 1", stats.Deletions)
	}
	if stats.FilesChanged != 2 {
		t.Errorf("FilesChanged = %d, want 2", stats.FilesChanged)
	}
	if stats.LinesChanged() != 4 {
		t.Errorf("LinesChanged = %d, want 4", stats.LinesChanged())
	}
	if stats.CodeDiffChars == 0 {
		t.Error("CodeDiffChars should be > 0")
	}
}

func TestGitLabClient_ListMergeRequestApprovals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"approved":true,"approvals_required":1,"approvals_left":0,"approved_by":[{"user":{"username":"alice"}},{"user":{"username":"bob"}}]}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	ap, err := client.ListMergeRequestApprovals(context.Background(), "grp/repo", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ap.Approved {
		t.Error("Approved should be true")
	}
	if len(ap.ApprovedBy) != 2 || ap.ApprovedBy[0] != "alice" {
		t.Errorf("ApprovedBy = %v, want [alice bob]", ap.ApprovedBy)
	}
}

// On a Community-Edition dark instance the /approvals endpoint 404s; that must
// be treated as "no approval gate", returning a zero-value struct, not an error.
func TestGitLabClient_ListMergeRequestApprovals_CE404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	ap, err := client.ListMergeRequestApprovals(context.Background(), "grp/repo", 7)
	if err != nil {
		t.Fatalf("CE 404 should not error, got: %v", err)
	}
	if ap.Approved || len(ap.ApprovedBy) != 0 {
		t.Errorf("CE 404 should yield zero approvals, got %+v", ap)
	}
}

func TestCountDiffLines(t *testing.T) {
	diff := "--- a/x\n+++ b/x\n@@ -1 +1,3 @@\n+one\n+two\n-old\n ctx\n"
	add, del := countDiffLines(diff)
	if add != 2 {
		t.Errorf("added = %d, want 2", add)
	}
	if del != 1 {
		t.Errorf("removed = %d, want 1", del)
	}
}

func TestProjectPathFromWebURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://dark.example/grp/repo/-/merge_requests/7", "grp/repo"},
		{"https://dark.example/a/b/c/-/merge_requests/12", "a/b/c"},
		{"not-a-url", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := projectPathFromWebURL(tc.url); got != tc.want {
			t.Errorf("projectPathFromWebURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
