package main

// Tests for `gb jira thread` (feature G1, epic ka-0000007401) — the Jira
// analog of `gb slack thread`. Mirrors the jira reply CLI tests: httptest
// bridge, no real network.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestJiraThread_MissingEnv(t *testing.T) {
	t.Setenv("JIRA_BRIDGE_URL", "")

	jiraThreadCmd.SetContext(context.Background())
	err := runJiraThread(jiraThreadCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "JIRA_BRIDGE_URL") {
		t.Fatalf("expected JIRA_BRIDGE_URL error, got %v", err)
	}
}

func TestJiraThread_FetchesAndPrintsMarkdown(t *testing.T) {
	var gotAgent, gotLimit string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/jira/thread" {
			t.Errorf("unexpected bridge request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAgent = r.URL.Query().Get("agent")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jiraThreadResult{
			OK:      true,
			JiraKey: "PE-42",
			Comments: []jiraThreadComment{
				{ID: "1", Author: "Matt", Created: "2026-07-18T10:00:00.000+0000", Text: "please fix the flaky test"},
				{ID: "2", Author: "gardenio", Created: "2026-07-18T11:00:00.000+0000", Text: "on it"},
			},
		})
	}))
	defer bridge.Close()

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)

	oldActor := actor
	actor = "test-agent"
	t.Cleanup(func() { actor = oldActor })

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	jiraThreadCmd.SetContext(context.Background())
	if err := jiraThreadCmd.Flags().Set("limit", "7"); err != nil {
		t.Fatal(err)
	}
	err := runJiraThread(jiraThreadCmd, nil)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotAgent != "test-agent" {
		t.Errorf("agent = %q, want test-agent", gotAgent)
	}
	if gotLimit != "7" {
		t.Errorf("limit = %q, want 7", gotLimit)
	}
	text := string(out)
	if !strings.Contains(text, "PE-42 conversation") {
		t.Errorf("output missing header: %s", text)
	}
	// Oldest first, both comments present.
	first := strings.Index(text, "please fix the flaky test")
	second := strings.Index(text, "on it")
	if first < 0 || second < 0 || first > second {
		t.Errorf("comments missing or out of order: %s", text)
	}
	if !strings.Contains(text, "**Matt**") || !strings.Contains(text, "**gardenio**") {
		t.Errorf("authors missing: %s", text)
	}
}

func TestJiraThread_BridgeError(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(jiraThreadResult{OK: false, Error: "agent has no jira_key binding"})
	}))
	defer bridge.Close()

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)

	jiraThreadCmd.SetContext(context.Background())
	if err := jiraThreadCmd.Flags().Set("limit", "0"); err != nil {
		t.Fatal(err)
	}
	err := runJiraThread(jiraThreadCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "agent has no jira_key binding") {
		t.Fatalf("expected bridge error surfaced, got %v", err)
	}
}

func TestJiraThread_BearerTokenSent(t *testing.T) {
	var gotAuth string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jiraThreadResult{OK: true, JiraKey: "PE-42"})
	}))
	defer bridge.Close()

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_BRIDGE_TOKEN", "sekret")

	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = old
	}()

	jiraThreadCmd.SetContext(context.Background())
	if err := runJiraThread(jiraThreadCmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization = %q, want Bearer sekret", gotAuth)
	}
}
