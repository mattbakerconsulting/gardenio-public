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

func TestBuildGardenioSection_Full(t *testing.T) {
	s := MRDescriptionSection{
		BeadID:         "gd-abc123",
		JIRAKey:        "PE-6967",
		JIRAStatus:     "In Progress",
		PipelineStatus: "success",
		PipelineURL:    "https://gitlab.com/org/repo/-/pipelines/123",
		Approved:       "true",
		Approvers:      "alice,bob",
		MRState:        "opened",
	}
	result := buildGardenioSection(s)

	checks := []string{
		"**Bead:** gd-abc123",
		"**JIRA:** PE-6967 (In Progress)",
		"**MR State:** opened",
		"[✅ success]",
		"**Approved:** true (by alice,bob)",
	}
	for _, c := range checks {
		if !strings.Contains(result, c) {
			t.Errorf("expected section to contain %q, got:\n%s", c, result)
		}
	}
}

func TestBuildGardenioSection_DeepLinks(t *testing.T) {
	s := MRDescriptionSection{
		BeadID:        "gd-abc123",
		JIRAKey:       "PE-6967",
		JIRAStatus:    "In Progress",
		ViewerBaseURL: "https://gardenio.app.e2e.dev.acme.example/beads/", // trailing slash trimmed
		JIRABaseURL:   "https://acme.atlassian.net",
	}
	result := buildGardenioSection(s)

	checks := []string{
		"- **Bead:** [gd-abc123](https://gardenio.app.e2e.dev.acme.example/beads/gd-abc123)",
		"- **JIRA:** [PE-6967](https://acme.atlassian.net/browse/PE-6967) (In Progress)",
	}
	for _, c := range checks {
		if !strings.Contains(result, c) {
			t.Errorf("expected section to contain %q, got:\n%s", c, result)
		}
	}
}

func TestBuildGardenioSection_NoViewerURLFallsBackToPlainText(t *testing.T) {
	s := MRDescriptionSection{BeadID: "gd-abc123", JIRAKey: "PE-6967"}
	result := buildGardenioSection(s)

	if !strings.Contains(result, "- **Bead:** gd-abc123\n") {
		t.Errorf("expected plain-text bead ref without viewer URL, got:\n%s", result)
	}
	if strings.Contains(result, "](") {
		t.Errorf("expected no Markdown links when base URLs are empty, got:\n%s", result)
	}
}

func TestBuildGardenioSection_Minimal(t *testing.T) {
	s := MRDescriptionSection{BeadID: "gd-123"}
	result := buildGardenioSection(s)

	if !strings.Contains(result, "gd-123") {
		t.Errorf("expected bead ID in section, got:\n%s", result)
	}
	if strings.Contains(result, "JIRA") {
		t.Errorf("expected no JIRA line for empty key, got:\n%s", result)
	}
	if strings.Contains(result, "Pipeline") {
		t.Errorf("expected no Pipeline line for empty status, got:\n%s", result)
	}
}

func TestSpliceGardenioSection_AppendNew(t *testing.T) {
	desc := "Human-written description here."
	section := "### Agent Context\n- **Bead:** gd-abc\n"

	result := spliceGardenioSection(desc, section)

	if !strings.HasPrefix(result, "Human-written description here.") {
		t.Errorf("human content should be preserved at start")
	}
	if !strings.Contains(result, gardenioMarkerStart) {
		t.Errorf("expected start marker in result")
	}
	if !strings.Contains(result, gardenioMarkerEnd) {
		t.Errorf("expected end marker in result")
	}
	if !strings.Contains(result, "gd-abc") {
		t.Errorf("expected bead ID in result")
	}
}

func TestSpliceGardenioSection_ReplaceExisting(t *testing.T) {
	desc := "Human content\n\n" + gardenioMarkerStart + "\nold content\n" + gardenioMarkerEnd + "\nFooter"
	section := "### Agent Context\n- **Bead:** gd-new\n"

	result := spliceGardenioSection(desc, section)

	if !strings.HasPrefix(result, "Human content") {
		t.Errorf("human content should be preserved")
	}
	if strings.Contains(result, "old content") {
		t.Errorf("old content should be replaced")
	}
	if !strings.Contains(result, "gd-new") {
		t.Errorf("new content should be present")
	}
	if !strings.HasSuffix(result, "\nFooter") {
		t.Errorf("footer should be preserved, got: %q", result)
	}
}

func TestSpliceGardenioSection_EmptyDescription(t *testing.T) {
	result := spliceGardenioSection("", "content\n")

	if !strings.HasPrefix(result, gardenioMarkerStart) {
		t.Errorf("expected marker at start for empty description, got: %q", result)
	}
}

func TestExtractGardenioSection(t *testing.T) {
	desc := "Human\n\n" + gardenioMarkerStart + "\nfoo bar\n" + gardenioMarkerEnd
	got := extractGardenioSection(desc)
	if got != "foo bar\n" {
		t.Errorf("extractGardenioSection = %q, want %q", got, "foo bar\n")
	}
}

func TestExtractGardenioSection_NoMarkers(t *testing.T) {
	got := extractGardenioSection("just a normal description")
	if got != "" {
		t.Errorf("expected empty for no markers, got %q", got)
	}
}

func TestPipelineIcon(t *testing.T) {
	tests := map[string]string{
		"success":  "✅",
		"failed":   "❌",
		"running":  "🔄",
		"pending":  "⏳",
		"canceled": "🚫",
		"unknown":  "⚙️",
	}
	for status, want := range tests {
		got := pipelineIcon(status)
		if got != want {
			t.Errorf("pipelineIcon(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestSyncMRDescription_UpdatesDescription(t *testing.T) {
	var gotBody map[string]string
	var gotMethod, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mr := GitLabMR{
				IID:         42,
				Description: "Existing MR description",
			}
			_ = json.NewEncoder(w).Encode(mr)
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	client := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "test"})

	section := MRDescriptionSection{
		BeadID:         "gd-abc",
		PipelineStatus: "success",
	}

	err := syncMRDescription(context.Background(), client, "org/repo", 42, section, slog.Default())
	if err != nil {
		t.Fatalf("syncMRDescription failed: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("expected PUT, got %s", gotMethod)
	}
	if !strings.Contains(gotPath, "merge_requests/42") {
		t.Errorf("expected path to contain merge_requests/42, got %s", gotPath)
	}
	if gotBody == nil || gotBody["description"] == "" {
		t.Fatal("expected description in PUT body")
	}
	if !strings.Contains(gotBody["description"], "Existing MR description") {
		t.Errorf("human content should be preserved in description")
	}
	if !strings.Contains(gotBody["description"], "gd-abc") {
		t.Errorf("bead ID should be in description")
	}
	if !strings.Contains(gotBody["description"], gardenioMarkerStart) {
		t.Errorf("gardenio markers should be in description")
	}
}

func TestSyncMRDescription_SkipsWhenUnchanged(t *testing.T) {
	section := MRDescriptionSection{BeadID: "gd-abc"}
	content := buildGardenioSection(section)
	existingDesc := "Human text\n\n" + gardenioMarkerStart + "\n" + content + gardenioMarkerEnd

	putCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mr := GitLabMR{IID: 42, Description: existingDesc}
			_ = json.NewEncoder(w).Encode(mr)
			return
		}
		putCalled = true
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	client := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "test"})

	err := syncMRDescription(context.Background(), client, "org/repo", 42, section, slog.Default())
	if err != nil {
		t.Fatalf("syncMRDescription failed: %v", err)
	}
	if putCalled {
		t.Error("PUT should not be called when section is unchanged")
	}
}
