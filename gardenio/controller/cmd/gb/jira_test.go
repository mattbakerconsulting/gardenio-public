package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJiraADFToText(t *testing.T) {
	tests := []struct {
		name string
		adf  string
		want string
	}{
		{
			name: "simple paragraph",
			adf:  `{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Hello world"}]}]}`,
			want: "Hello world",
		},
		{
			name: "bold text",
			adf:  `{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"bold","marks":[{"type":"strong"}]}]}]}`,
			want: "**bold**",
		},
		{
			name: "heading",
			adf:  `{"version":1,"type":"doc","content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Title"}]}]}`,
			want: "## Title",
		},
		{
			name: "bullet list",
			adf:  `{"version":1,"type":"doc","content":[{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item one"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item two"}]}]}]}]}`,
			want: "- item one\n- item two",
		},
		{
			name: "empty",
			adf:  ``,
			want: "",
		},
		{
			name: "invalid json",
			adf:  `{not valid}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jiraADFToText(json.RawMessage(tt.adf))
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatJiraTime(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"2026-03-13T02:33:54.000+0000", "2026-03-13 02:33"},
		{"invalid", "invalid"},
		{"", ""},
	}
	for _, tt := range tests {
		got := formatJiraTime(tt.input)
		if got != tt.want {
			t.Errorf("formatJiraTime(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNameOrEmpty(t *testing.T) {
	if got := nameOrEmpty(nil); got != "(none)" {
		t.Errorf("got %q, want (none)", got)
	}
	ref := &jiraNamedRef{Name: "Bug"}
	if got := nameOrEmpty(ref); got != "Bug" {
		t.Errorf("got %q, want Bug", got)
	}
}

func TestUserOrEmpty(t *testing.T) {
	if got := userOrEmpty(nil); got != "(unassigned)" {
		t.Errorf("got %q, want (unassigned)", got)
	}
	ref := &jiraUserRef{DisplayName: "Alice"}
	if got := userOrEmpty(ref); got != "Alice" {
		t.Errorf("got %q, want Alice", got)
	}
}

func TestRunJiraShow_MissingEnvVars(t *testing.T) {
	// With NEITHER the direct creds NOR the bridge URL, gb jira show has no
	// access path and errors, naming both options.
	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	t.Setenv("JIRA_BRIDGE_URL", "")

	err := runJiraShow(jiraShowCmd, []string{"PE-1234"})
	if err == nil {
		t.Fatal("expected error when no access path is configured")
	}
	if !strings.Contains(err.Error(), "JIRA_BASE_URL") || !strings.Contains(err.Error(), "JIRA_BRIDGE_URL") {
		t.Errorf("error should mention both access paths, got: %v", err)
	}
}

// TestRunJiraShow_BridgeFallback pins that with no direct JIRA_* creds but
// JIRA_BRIDGE_URL set (the agent-pod case), gb jira show reads the bound ticket
// through the bridge's GET /api/jira/issue — feature G6.
func TestRunJiraShow_BridgeFallback(t *testing.T) {
	var gotPath, gotAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAgent = r.URL.Query().Get("agent")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jiraBridgeIssueResult{
			OK:          true,
			JiraKey:     "PE-1234",
			Summary:     "Bridge-proxied read",
			Description: "current body",
			Status:      "In Progress",
		})
	}))
	defer srv.Close()

	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	t.Setenv("JIRA_BRIDGE_URL", srv.URL)
	oldActor := actor
	actor = "test-agent"
	t.Cleanup(func() { actor = oldActor })

	if err := runJiraShow(jiraShowCmd, []string{"PE-1234"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/jira/issue" {
		t.Errorf("path = %q, want /api/jira/issue", gotPath)
	}
	if gotAgent != "test-agent" {
		t.Errorf("agent = %q, want test-agent", gotAgent)
	}
}

// TestRunJiraShow_BridgeRejectsOtherTicket pins the bound-ticket guard: the
// bridge only returns the agent's own ticket, so asking for a different key is
// an explicit error rather than a silent swap.
func TestRunJiraShow_BridgeRejectsOtherTicket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jiraBridgeIssueResult{OK: true, JiraKey: "PE-1234", Summary: "bound"})
	}))
	defer srv.Close()

	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	t.Setenv("JIRA_BRIDGE_URL", srv.URL)

	err := runJiraShow(jiraShowCmd, []string{"DEVOPS-99"})
	if err == nil {
		t.Fatal("expected error when requesting a ticket other than the bound one")
	}
	if !strings.Contains(err.Error(), "PE-1234") {
		t.Errorf("error should name the bound ticket, got: %v", err)
	}
}

// TestRunJiraShow_BridgeNoBindingIsActionable pins that the bridge's cryptic
// 403 "no jira_key binding" is expanded into an actionable message: this pod
// was not spawned from a ticket and carries no direct JIRA creds, so the agent
// should report a verification gap rather than retry or guess (bug
// ka-0000000090). Without this, an agent that trusts the "JIRA is injected"
// advice burns turns on a route that cannot work.
func TestRunJiraShow_BridgeNoBindingIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(jiraBridgeIssueResult{Error: "agent has no jira_key binding"})
	}))
	defer srv.Close()

	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	t.Setenv("JIRA_BRIDGE_URL", srv.URL)

	err := runJiraShow(jiraShowCmd, []string{"PE-10116"})
	if err == nil {
		t.Fatal("expected error when the pod has no bound ticket and no direct creds")
	}
	// The message must name the requested ticket and steer toward a verification
	// gap — not leave the raw "jira_key binding" jargon as the whole story.
	if !strings.Contains(err.Error(), "PE-10116") {
		t.Errorf("error should name the requested ticket, got: %v", err)
	}
	if !strings.Contains(err.Error(), "verification gap") {
		t.Errorf("error should steer toward a verification gap, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not spawned from a JIRA ticket") {
		t.Errorf("error should explain why the bridge cannot serve the ticket, got: %v", err)
	}
}

// TestRunJiraShow_PrefersDirectCreds pins that when BOTH the direct creds and
// the bridge URL are present, the direct REST path wins (operator laptop).
func TestRunJiraShow_PrefersDirectCreds(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/comment") {
			json.NewEncoder(w).Encode(map[string]any{"comments": []any{}, "total": 0})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"key": "PE-1234", "fields": map[string]any{"summary": "direct"}})
	}))
	defer direct.Close()

	bridgeHit := false
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bridgeHit = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bridge.Close()

	t.Setenv("JIRA_BASE_URL", direct.URL)
	t.Setenv("JIRA_EMAIL", "test@example.com")
	t.Setenv("JIRA_API_TOKEN", "fake-token")
	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)

	if err := runJiraShow(jiraShowCmd, []string{"PE-1234"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bridgeHit {
		t.Error("bridge was hit even though direct creds were present")
	}
}

func TestRunJiraShow_Integration(t *testing.T) {
	// Mock JIRA API server.
	issueResp := map[string]any{
		"key": "PE-1234",
		"id":  "12345",
		"fields": map[string]any{
			"summary":   "Test bug",
			"status":    map[string]string{"name": "To Do"},
			"issuetype": map[string]string{"name": "Bug"},
			"priority":  map[string]string{"name": "High"},
			"labels":    []string{"frontend"},
			"created":   "2026-03-13T02:33:54.000+0000",
			"updated":   "2026-03-13T10:00:00.000+0000",
			"description": map[string]any{
				"version": 1,
				"type":    "doc",
				"content": []any{
					map[string]any{
						"type": "paragraph",
						"content": []any{
							map[string]any{"type": "text", "text": "This is the description"},
						},
					},
				},
			},
		},
	}

	commentsResp := map[string]any{
		"comments": []any{
			map[string]any{
				"author":  map[string]string{"displayName": "Alice"},
				"created": "2026-03-13T03:00:00.000+0000",
				"body": map[string]any{
					"version": 1,
					"type":    "doc",
					"content": []any{
						map[string]any{
							"type": "paragraph",
							"content": []any{
								map[string]any{"type": "text", "text": "A comment"},
							},
						},
					},
				},
			},
		},
		"total": 1,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/comment") {
			json.NewEncoder(w).Encode(commentsResp)
		} else {
			json.NewEncoder(w).Encode(issueResp)
		}
	}))
	defer srv.Close()

	t.Setenv("JIRA_BASE_URL", srv.URL)
	t.Setenv("JIRA_EMAIL", "test@example.com")
	t.Setenv("JIRA_API_TOKEN", "fake-token")

	err := runJiraShow(jiraShowCmd, []string{"PE-1234"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestJiraHTTPClient_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errorMessages":["Issue does not exist"]}`))
	}))
	defer srv.Close()

	client := &jiraHTTPClient{
		baseURL:    srv.URL,
		authHeader: "Basic dGVzdDp0ZXN0",
		http:       &http.Client{},
	}

	_, err := client.getIssue(t.Context(), "INVALID-999")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404, got: %v", err)
	}
}
