package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestJiraSync_MRLink(t *testing.T) {
	var (
		mu           sync.Mutex
		commentAdded bool
		linkAdded    bool
		commentBody  string
		linkURL      string
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-7001/comment":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if doc, ok := body["body"].(map[string]any); ok {
				if content, ok := doc["content"].([]any); ok && len(content) > 0 {
					if para, ok := content[0].(map[string]any); ok {
						if pc, ok := para["content"].([]any); ok && len(pc) > 0 {
							if tn, ok := pc[0].(map[string]any); ok {
								commentBody, _ = tn["text"].(string)
							}
						}
					}
				}
			}
			commentAdded = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-7001/remotelink":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if obj, ok := body["object"].(map[string]any); ok {
				linkURL, _ = obj["url"].(string)
			}
			linkAdded = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-1", Type: "task", Title: "[PE-7001] Fix upload error",
		Labels: []string{"source:jira", "jira:PE-7001"},
		Fields: map[string]string{"jira_key": "PE-7001", "mr_url": "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/123"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if !commentAdded {
		t.Fatal("expected JIRA comment")
	}
	if commentBody != "Automated MR created: https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/123" {
		t.Errorf("comment body: %s", commentBody)
	}
	if !linkAdded {
		t.Fatal("expected JIRA remote link")
	}
	if linkURL != "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/123" {
		t.Errorf("link URL: %s", linkURL)
	}
}

func TestJiraSync_SkipNonJira(t *testing.T) {
	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient("https://example.com"), Logger: slog.Default()})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-2", Type: "task", Title: "Regular task",
		Labels: []string{"source:manual"},
		Fields: map[string]string{"mr_url": "https://gitlab.com/example/mr/1"},
	})
	s.handleUpdated(context.Background(), event)
}

func TestJiraSync_Closed(t *testing.T) {
	var (
		mu              sync.Mutex
		commentAdded    bool
		transitionCalls int
	)
	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-42/comment":
			commentAdded = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-42/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "31", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
				{"id": "41", "name": "Review", "to": map[string]string{"name": "Review"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-42/transitions":
			transitionCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-3", Type: "task", Title: "[PE-42] Fix CI pipeline",
		Labels: []string{"source:jira", "jira:PE-42"},
		Fields: map[string]string{"jira_key": "PE-42"},
	})
	s.handleClosed(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if !commentAdded {
		t.Fatal("expected JIRA closing comment")
	}
	// Transitions are no longer triggered on close — they fire on mr_merged=true.
	if transitionCalls != 0 {
		t.Errorf("expected 0 transition calls on close, got %d", transitionCalls)
	}
}

func TestJiraSync_MRMerged(t *testing.T) {
	var (
		mu              sync.Mutex
		commentAdded    bool
		commentBody     string
		transitionCalls int
	)
	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-100/comment":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if doc, ok := body["body"].(map[string]any); ok {
				if content, ok := doc["content"].([]any); ok && len(content) > 0 {
					if para, ok := content[0].(map[string]any); ok {
						if pc, ok := para["content"].([]any); ok && len(pc) > 0 {
							if tn, ok := pc[0].(map[string]any); ok {
								commentBody, _ = tn["text"].(string)
							}
						}
					}
				}
			}
			commentAdded = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-100/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "31", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
				{"id": "41", "name": "Review", "to": map[string]string{"name": "Review"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-100/transitions":
			transitionCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-5", Type: "task", Title: "[PE-100] Fix login bug",
		Labels: []string{"source:jira", "jira:PE-100"},
		Fields: map[string]string{"jira_key": "PE-100", "mr_merged": "true"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if !commentAdded {
		t.Fatal("expected JIRA MR merged comment")
	}
	if commentBody != "MR merged — transitioning to Review." {
		t.Errorf("comment body: %s", commentBody)
	}
	if transitionCalls != 1 {
		t.Errorf("expected 1 transition call, got %d", transitionCalls)
	}
}

func TestJiraSync_MRMerged_TransitionsDisabled(t *testing.T) {
	var (
		mu              sync.Mutex
		commentAdded    bool
		transitionCalls int
	)
	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-200/comment":
			commentAdded = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-200/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "41", "name": "Review", "to": map[string]string{"name": "Review"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-200/transitions":
			transitionCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{
		Jira:               newTestJiraClient(jiraServer.URL),
		Logger:             slog.Default(),
		DisableTransitions: true,
	})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-6", Type: "task", Title: "[PE-200] Fix upload",
		Labels: []string{"source:jira", "jira:PE-200"},
		Fields: map[string]string{"jira_key": "PE-200", "mr_merged": "true"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if !commentAdded {
		t.Fatal("expected JIRA MR merged comment even with transitions disabled")
	}
	if transitionCalls != 0 {
		t.Errorf("expected 0 transition calls with DisableTransitions, got %d", transitionCalls)
	}
}

func TestJiraSync_Claimed(t *testing.T) {
	var (
		mu              sync.Mutex
		commentAdded    bool
		commentBody     string
		labelCalled     bool
		transitionCalls int
		transitionName  string
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-500/comment":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if doc, ok := body["body"].(map[string]any); ok {
				if content, ok := doc["content"].([]any); ok && len(content) > 0 {
					if para, ok := content[0].(map[string]any); ok {
						if pc, ok := para["content"].([]any); ok && len(pc) > 0 {
							if tn, ok := pc[0].(map[string]any); ok {
								commentBody, _ = tn["text"].(string)
							}
						}
					}
				}
			}
			commentAdded = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "PUT" && r.URL.Path == "/rest/api/3/issue/PE-500":
			labelCalled = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-500/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "21", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
				{"id": "41", "name": "Review", "to": map[string]string{"name": "Review"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-500/transitions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if t, ok := body["transition"].(map[string]any); ok {
				transitionName, _ = t["id"].(string)
			}
			transitionCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-10", Type: "task", Title: "[PE-500] Fix rendering bug",
		Status:   "in_progress",
		Assignee: "agent-worker-1",
		Labels:   []string{"source:jira", "jira:PE-500"},
		Fields:   map[string]string{"jira_key": "PE-500"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if !commentAdded {
		t.Fatal("expected JIRA claim comment")
	}
	if commentBody != "Gardenio agent agent-worker-1 is working on this issue." {
		t.Errorf("comment body: %s", commentBody)
	}
	if labelCalled {
		t.Fatal("did not expect any label edits on claim")
	}
	if transitionCalls != 1 {
		t.Errorf("expected 1 transition call (In Progress), got %d", transitionCalls)
	}
	if transitionName != "21" {
		t.Errorf("expected transition ID 21 (In Progress), got %s", transitionName)
	}
}

// adfBodyText flattens every text node of a posted ADF comment body, joining
// runs with "\n" so a test can assert content that spans hardBreak boundaries
// (the attribution line lands on its own line — jira_attribution.go).
func adfBodyText(body map[string]any) string {
	doc, ok := body["body"].(map[string]any)
	if !ok {
		return ""
	}
	content, ok := doc["content"].([]any)
	if !ok || len(content) == 0 {
		return ""
	}
	para, ok := content[0].(map[string]any)
	if !ok {
		return ""
	}
	nodes, ok := para["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, n := range nodes {
		if node, ok := n.(map[string]any); ok {
			if txt, ok := node["text"].(string); ok {
				parts = append(parts, txt)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// TestJiraSync_Claimed_StampsOperatorAttribution covers epic ka-0000004904
// decision #4 / ka-0000006003: the claim comment carries an "Acted by
// <operator>" line derived from the bead's operated_by. The SSE event has no
// daemon-resolved display, so it is the raw-fallback form — and the raw
// operated_by MUST appear verbatim (the E2E harness greps for it).
func TestJiraSync_Claimed_StampsOperatorAttribution(t *testing.T) {
	var (
		mu          sync.Mutex
		commentBody string
	)
	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-501/comment":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			commentBody = adfBodyText(body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-501/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "21", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-501/transitions":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-attr", Type: "task", Title: "[PE-501] Fix thing",
		Status:   "in_progress",
		Assignee: "agent-worker-1",
		Labels:   []string{"source:jira", "jira:PE-501"},
		Fields:   map[string]string{"jira_key": "PE-501", "operated_by": "slack:U0OPERATOR1"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(commentBody, "Gardenio agent agent-worker-1 is working on this issue.") {
		t.Errorf("expected original claim text retained, got %q", commentBody)
	}
	if !strings.Contains(commentBody, "Acted by slack:U0OPERATOR1") {
		t.Errorf("expected operator attribution line, got %q", commentBody)
	}
}

// TestJiraSync_Claimed_NoAttributionWhenNoOperator covers the omit case: a bead
// with no operated_by must produce the bare claim comment with no fabricated
// attribution (and stay a single ADF text node).
func TestJiraSync_Claimed_NoAttributionWhenNoOperator(t *testing.T) {
	var (
		mu          sync.Mutex
		commentBody string
	)
	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-502/comment":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			commentBody = adfBodyText(body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-502/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "21", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-502/transitions":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-noattr", Type: "task", Title: "[PE-502] Fix thing",
		Status:   "in_progress",
		Assignee: "agent-worker-1",
		Labels:   []string{"source:jira", "jira:PE-502"},
		Fields:   map[string]string{"jira_key": "PE-502"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if commentBody != "Gardenio agent agent-worker-1 is working on this issue." {
		t.Errorf("expected bare claim comment with no attribution, got %q", commentBody)
	}
}

func TestJiraSync_Claimed_WithBotAssignment(t *testing.T) {
	var (
		mu         sync.Mutex
		assigned   bool
		assigneeID string
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-600/comment":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "PUT" && r.URL.Path == "/rest/api/3/issue/PE-600":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-600/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "21", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-600/transitions":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "PUT" && r.URL.Path == "/rest/api/3/issue/PE-600/assignee":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			assigneeID, _ = body["accountId"].(string)
			assigned = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{
		Jira:         newTestJiraClient(jiraServer.URL),
		Logger:       slog.Default(),
		BotAccountID: "5f1234567890abcdef012345",
	})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-11", Type: "task", Title: "[PE-600] Fix API timeout",
		Status:   "in_progress",
		Assignee: "agent-worker-2",
		Labels:   []string{"source:jira", "jira:PE-600"},
		Fields:   map[string]string{"jira_key": "PE-600"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if !assigned {
		t.Fatal("expected JIRA issue to be assigned to bot account")
	}
	if assigneeID != "5f1234567890abcdef012345" {
		t.Errorf("assignee account ID: %s", assigneeID)
	}
}

func TestJiraSync_Claimed_ClaimTransitionOverride(t *testing.T) {
	var (
		mu              sync.Mutex
		transitionCalls int
		transitionName  string
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-800/comment":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "PUT" && r.URL.Path == "/rest/api/3/issue/PE-800":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-800/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "21", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
				{"id": "41", "name": "Review", "to": map[string]string{"name": "Review"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-800/transitions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if t, ok := body["transition"].(map[string]any); ok {
				transitionName, _ = t["id"].(string)
			}
			transitionCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	// DisableTransitions=true but EnableClaimTransition=true → claim transition should fire.
	s := NewJiraSync(JiraSyncConfig{
		Jira:                  newTestJiraClient(jiraServer.URL),
		Logger:                slog.Default(),
		DisableTransitions:    true,
		EnableClaimTransition: true,
	})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-13", Type: "task", Title: "[PE-800] Fix dashboard",
		Status:   "in_progress",
		Assignee: "agent-worker-4",
		Labels:   []string{"source:jira", "jira:PE-800"},
		Fields:   map[string]string{"jira_key": "PE-800"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	if transitionCalls != 1 {
		t.Errorf("expected 1 transition call (In Progress) with EnableClaimTransition override, got %d", transitionCalls)
	}
	if transitionName != "21" {
		t.Errorf("expected transition ID 21 (In Progress), got %s", transitionName)
	}
	mu.Unlock()

	// Verify Review transition is still blocked — send mr_merged event.
	// Must release lock before calling handleUpdated since the mock handler needs it.
	s.handleUpdated(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-14", Type: "task", Title: "[PE-800] Fix dashboard",
		Labels: []string{"source:jira", "jira:PE-800"},
		Fields: map[string]string{"jira_key": "PE-800", "mr_merged": "true"},
	}))

	mu.Lock()
	defer mu.Unlock()
	// transitionCalls should still be 1 (Review blocked by DisableTransitions).
	if transitionCalls != 1 {
		t.Errorf("expected Review transition to be blocked (DisableTransitions=true), got %d total transition calls", transitionCalls)
	}
}

func TestJiraSync_Claimed_Dedup(t *testing.T) {
	var (
		mu           sync.Mutex
		commentCount int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-700/comment":
			commentCount++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "PUT" && r.URL.Path == "/rest/api/3/issue/PE-700":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-700/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "21", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-700/transitions":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-12", Type: "task", Title: "[PE-700] Fix search",
		Status:   "in_progress",
		Assignee: "agent-worker-3",
		Labels:   []string{"source:jira", "jira:PE-700"},
		Fields:   map[string]string{"jira_key": "PE-700"},
	})

	// First call should post comment.
	s.handleUpdated(context.Background(), event)
	// Second call (same assignee) should be deduped.
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if commentCount != 1 {
		t.Errorf("expected 1 comment (dedup), got %d", commentCount)
	}
}

func TestAdfToMarkdown(t *testing.T) {
	tests := []struct {
		name, input, expected string
	}{
		{"empty", "", ""},
		{"paragraph", `{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Hello world"}]}]}`, "Hello world"},
		{"heading", `{"version":1,"type":"doc","content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Title"}]}]}`, "## Title"},
		{"bullet list", `{"version":1,"type":"doc","content":[{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"Item A"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"Item B"}]}]}]}]}`, "- Item A\n- Item B"},
		{"code block", `{"version":1,"type":"doc","content":[{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"fmt.Println()"}]}]}`, "```go\nfmt.Println()\n```"},
		{"bold+italic", `{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"bold","marks":[{"type":"strong"}]},{"type":"text","text":" and "},{"type":"text","text":"italic","marks":[{"type":"em"}]}]}]}`, "**bold** and _italic_"},
		{"inline code + link", `{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"run "},{"type":"text","text":"go test","marks":[{"type":"code"}]},{"type":"text","text":" see "},{"type":"text","text":"docs","marks":[{"type":"link","attrs":{"href":"https://x.dev"}}]}]}]}`, "run `go test` see [docs](https://x.dev)"},
		{"mention node kept", `{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"mention","attrs":{"id":"a","text":"@gardenio"}},{"type":"text","text":" go"}]}]}`, "@gardenio go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := adfToMarkdown(json.RawMessage(tt.input)); got != tt.expected {
				t.Errorf("adfToMarkdown():\n  got:  %q\n  want: %q", got, tt.expected)
			}
		})
	}
}

// mockCatchUpClient implements JiraSyncCatchUpClient for testing.
type mockCatchUpClient struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (m *mockCatchUpClient) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

func TestJiraSync_CatchUp_PreventsReplay(t *testing.T) {
	var (
		mu           sync.Mutex
		commentCount int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-900/comment":
			commentCount++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "PUT" && r.URL.Path == "/rest/api/3/issue/PE-900":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-900/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "21", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-900/transitions":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	// Simulate restart catch-up: bead is already in_progress with an assignee.
	client := &mockCatchUpClient{
		beads: []*beadsapi.BeadDetail{
			{
				ID:       "bd-task-20",
				Status:   "in_progress",
				Assignee: "agent-old",
				Labels:   []string{"source:jira", "jira:PE-900"},
				Fields:   map[string]string{"jira_key": "PE-900", "mr_url": "https://gitlab.com/mr/1"},
			},
		},
	}
	s.CatchUp(context.Background(), client)

	// SSE replays the same event after restart — should be deduped.
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-20", Type: "task", Title: "[PE-900] Fix auth",
		Status:   "in_progress",
		Assignee: "agent-old",
		Labels:   []string{"source:jira", "jira:PE-900"},
		Fields:   map[string]string{"jira_key": "PE-900", "mr_url": "https://gitlab.com/mr/1"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if commentCount != 0 {
		t.Errorf("expected 0 comments after catch-up (deduped), got %d", commentCount)
	}
}

func TestJiraSync_CatchUp_NilClient(t *testing.T) {
	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient("https://example.com"), Logger: slog.Default()})
	// Should not panic.
	s.CatchUp(context.Background(), nil)
}

// TestJiraSync_CatchUp_ClosedBeadPreventsReplay verifies that CatchUp marks
// closed beads so that SSE replay of historical "updated" events for
// long-gone agents doesn't re-post "working on this issue" comments.
// This is the fix for the bug where the jira bridge repeated itself on restart
// because closed beads were not included in the CatchUp query.
func TestJiraSync_CatchUp_ClosedBeadPreventsReplay(t *testing.T) {
	var (
		mu           sync.Mutex
		commentCount int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-950/comment":
			commentCount++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		case r.Method == "PUT" && r.URL.Path == "/rest/api/3/issue/PE-950":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/issue/PE-950/transitions":
			resp := map[string]any{"transitions": []map[string]any{
				{"id": "21", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-950/transitions":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	// Simulate restart catch-up: bead was claimed by an agent that is now
	// long gone — the bead is closed.
	client := &mockCatchUpClient{
		beads: []*beadsapi.BeadDetail{
			{
				ID:       "bd-task-closed",
				Status:   "closed",
				Assignee: "jira-fixer99",
				Labels:   []string{"source:jira"},
				Fields:   map[string]string{"jira_key": "PE-950"},
			},
		},
	}
	s.CatchUp(context.Background(), client)

	// SSE replays the historical "updated" event (the claim from before
	// the bead was closed). Without the fix, this would re-post the
	// "working on this issue" comment to Jira.
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-closed", Type: "task", Title: "[PE-950] Fix something",
		Status:   "in_progress",
		Assignee: "jira-fixer99",
		Labels:   []string{"source:jira"},
		Fields:   map[string]string{"jira_key": "PE-950"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if commentCount != 0 {
		t.Errorf("expected 0 comments for closed bead (deduped), got %d", commentCount)
	}
}

// TestJiraSync_CatchUp_ClosedBeadPreventsCloseReplay verifies that CatchUp
// also marks close events for closed beads.
func TestJiraSync_CatchUp_ClosedBeadPreventsCloseReplay(t *testing.T) {
	var (
		mu           sync.Mutex
		commentCount int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PE-960/comment" {
			commentCount++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	client := &mockCatchUpClient{
		beads: []*beadsapi.BeadDetail{
			{
				ID:       "bd-task-closed2",
				Status:   "closed",
				Assignee: "agent-old",
				Labels:   []string{"source:jira"},
				Fields:   map[string]string{"jira_key": "PE-960"},
			},
		},
	}
	s.CatchUp(context.Background(), client)

	// Replay close event — should be deduped.
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-task-closed2", Type: "task", Title: "[PE-960] Old task",
		Status: "closed",
		Labels: []string{"source:jira"},
		Fields: map[string]string{"jira_key": "PE-960"},
	})
	s.handleClosed(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if commentCount != 0 {
		t.Errorf("expected 0 comments for replayed close event (deduped), got %d", commentCount)
	}
}

// TestJiraSync_BlockedProject_Updated verifies that handleUpdated blocks
// all write-back operations (comments, labels, transitions, remote links)
// for beads from blocked JIRA projects like FSM.
func TestJiraSync_BlockedProject_Updated(t *testing.T) {
	var (
		mu       sync.Mutex
		apiCalls int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		apiCalls++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"1"}`)
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	// Simulate an FSM bead being claimed — should NOT post to JIRA.
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-fsm-1", Type: "task", Title: "[FSM-100] Customer request",
		Status:   "in_progress",
		Assignee: "agent-triage-1",
		Labels:   []string{"source:jira", "jira:FSM-100"},
		Fields:   map[string]string{"jira_key": "FSM-100", "mr_url": "https://gitlab.com/mr/99"},
	})
	s.handleUpdated(context.Background(), event)

	// Also try MR merged event.
	mergedEvent := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-fsm-2", Type: "task", Title: "[FSM-200] Another request",
		Labels: []string{"source:jira", "jira:FSM-200"},
		Fields: map[string]string{"jira_key": "FSM-200", "mr_merged": "true"},
	})
	s.handleUpdated(context.Background(), mergedEvent)

	mu.Lock()
	defer mu.Unlock()
	if apiCalls != 0 {
		t.Errorf("expected 0 JIRA API calls for blocked project FSM, got %d", apiCalls)
	}
}

// TestJiraSync_NonAllowlistedProject_Updated verifies the allow-list flip:
// a project that is neither blocked nor allowlisted (e.g. an arbitrary
// agent-chosen key) is denied by default — no outbound Jira writes.
func TestJiraSync_NonAllowlistedProject_Updated(t *testing.T) {
	var (
		mu       sync.Mutex
		apiCalls int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		apiCalls++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"1"}`)
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	// ACME is not on the allow-list and not blocked — deny by default.
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-acme-1", Type: "task", Title: "[ACME-42] Fix CI",
		Status:   "in_progress",
		Assignee: "agent-worker-x",
		Labels:   []string{"source:jira", "jira:ACME-42"},
		Fields:   map[string]string{"jira_key": "ACME-42", "mr_url": "https://gitlab.com/mr/1"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if apiCalls != 0 {
		t.Errorf("expected 0 JIRA API calls for non-allowlisted project ACME, got %d", apiCalls)
	}
}

// TestJiraSync_NonAllowlistedProject_Closed verifies handleClosed denies
// write-back for a non-allowlisted project.
func TestJiraSync_NonAllowlistedProject_Closed(t *testing.T) {
	var (
		mu       sync.Mutex
		apiCalls int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		apiCalls++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"1"}`)
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-acme-2", Type: "task", Title: "[ACME-99] Old task",
		Status: "closed",
		Labels: []string{"source:jira", "jira:ACME-99"},
		Fields: map[string]string{"jira_key": "ACME-99"},
	})
	s.handleClosed(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if apiCalls != 0 {
		t.Errorf("expected 0 JIRA API calls for non-allowlisted project ACME, got %d", apiCalls)
	}
}

// TestJiraSync_DarkMarker_Updated verifies that a source:jira-dark bead
// early-returns in handleUpdated — even when it carries an allowlisted (PE)
// jira_key and a claim/MR — so dark-universe work never flows upstream.
func TestJiraSync_DarkMarker_Updated(t *testing.T) {
	var (
		mu       sync.Mutex
		apiCalls int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		apiCalls++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"1"}`)
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	// PE key is allowlisted, but the source:jira-dark marker must win.
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-dark-1", Type: "task", Title: "[PE-1234] Dark mirror work",
		Status:   "in_progress",
		Assignee: "agent-dark-1",
		Labels:   []string{"source:jira-dark", "jira:PE-1234"},
		Fields:   map[string]string{"jira_key": "PE-1234", "mr_url": "https://gitlab.com/mr/7", "mr_merged": "true"},
	})
	s.handleUpdated(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if apiCalls != 0 {
		t.Errorf("expected 0 JIRA API calls for dark-marked bead, got %d", apiCalls)
	}
}

// TestJiraSync_DarkMarker_Closed verifies handleClosed early-returns for a
// source:jira-dark bead.
func TestJiraSync_DarkMarker_Closed(t *testing.T) {
	var (
		mu       sync.Mutex
		apiCalls int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		apiCalls++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"1"}`)
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-dark-2", Type: "task", Title: "[PE-5678] Dark mirror close",
		Status: "closed",
		Labels: []string{"source:jira-dark", "jira:PE-5678"},
		Fields: map[string]string{"jira_key": "PE-5678"},
	})
	s.handleClosed(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if apiCalls != 0 {
		t.Errorf("expected 0 JIRA API calls for dark-marked bead on close, got %d", apiCalls)
	}
}

// TestJiraSync_BlockedProject_Closed verifies that handleClosed blocks
// write-back for beads from blocked JIRA projects.
func TestJiraSync_BlockedProject_Closed(t *testing.T) {
	var (
		mu       sync.Mutex
		apiCalls int
	)

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		apiCalls++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"1"}`)
	}))
	defer jiraServer.Close()

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(jiraServer.URL), Logger: slog.Default()})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-fsm-3", Type: "task", Title: "[FSM-300] Service request",
		Status: "closed",
		Labels: []string{"source:jira", "jira:FSM-300"},
		Fields: map[string]string{"jira_key": "FSM-300"},
	})
	s.handleClosed(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	if apiCalls != 0 {
		t.Errorf("expected 0 JIRA API calls for blocked project FSM, got %d", apiCalls)
	}
}

// --- M9: CatchUp must page to completion, never ride a single silent cap ---
//
// These tests pin the fix for ka-0000006235 / M9. CatchUp exists SPECIFICALLY
// to pre-mark ALL historical source:jira sync operations so SSE replay after a
// restart cannot re-post a duplicate Jira comment. The old shape fetched a
// single Limit:1000 page; once the source:jira population (open + in_progress +
// CLOSED, no window) crosses the daemon's default cap it would silently
// truncate (HTTP 200, no error) and stop pre-marking the tail — reintroducing
// the exact duplicate this code prevents. The tests assert on the REQUESTS the
// walk makes (page size, advancing offsets, stable created_at sort) AND on the
// correctness outcome (a bead PAST the old 1000 cap is pre-marked), because a
// correct-looking dedup map can still come from a truncated fetch when the
// corpus is small. Compare TestFleetArchetypesPaged_NeverFetchesUnbounded.

// recordingCatchUpLister serves a fixed corpus paginated by Limit/Offset and
// records every query, so tests can assert the WALK shape, not just the result.
// Total is always the full corpus size — the daemon's real behavior — so a
// caller that fails to page sees Total > len(page) exactly as in production.
type recordingCatchUpLister struct {
	corpus []*beadsapi.BeadDetail
	calls  []beadsapi.ListBeadsQuery
	err    error
	errOn  int // if >0, return err starting on the (1-based) call number
}

func (r *recordingCatchUpLister) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	r.calls = append(r.calls, q)
	if r.err != nil && r.errOn == 0 {
		return nil, r.err
	}
	if r.errOn > 0 && len(r.calls) >= r.errOn {
		return nil, r.err
	}
	start := min(q.Offset, len(r.corpus))
	end := len(r.corpus)
	if q.Limit > 0 && start+q.Limit < end {
		end = start + q.Limit
	}
	page := append([]*beadsapi.BeadDetail(nil), r.corpus[start:end]...)
	return &beadsapi.ListBeadsResult{Beads: page, Total: len(r.corpus)}, nil
}

// TestJiraSync_CatchUp_PagesEntirePopulation is the regression guard for M9. A
// corpus larger than the old single Limit:1000 fetch must be pre-marked in
// FULL: every page requested with an explicit page-size Limit, advancing
// offsets, a stable created_at sort, and — the correctness payoff — a closed
// bead sitting PAST row 1000 must have its close dedup key pre-marked so a
// replayed close event is deduped rather than re-posted to Jira.
func TestJiraSync_CatchUp_PagesEntirePopulation(t *testing.T) {
	const n = 1300 // > 1000 old cap and spans multiple pages
	corpus := make([]*beadsapi.BeadDetail, 0, n)
	for i := 0; i < n; i++ {
		corpus = append(corpus, &beadsapi.BeadDetail{
			ID:     fmt.Sprintf("gd-jira-%04d", i),
			Status: "closed",
			Labels: []string{"source:jira", fmt.Sprintf("jira:PE-%04d", i)},
			Fields: map[string]string{"jira_key": fmt.Sprintf("PE-%04d", i)},
		})
	}
	lister := &recordingCatchUpLister{corpus: corpus}

	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient("https://example.com"), Logger: slog.Default()})
	s.CatchUp(context.Background(), lister)

	// --- assert on the REQUESTS ---
	if len(lister.calls) < 2 {
		t.Fatalf("made %d list calls for a %d-bead corpus — a single call means it rode a silent daemon cap and did not page (M9)", len(lister.calls), n)
	}
	for i, c := range lister.calls {
		if c.Limit <= 0 {
			t.Errorf("call %d: Limit=%d — an unbounded request lets the daemon silently cap the result", i, c.Limit)
		}
		if c.Limit > jiraCatchUpPageSize {
			t.Errorf("call %d: Limit=%d exceeds page size %d", i, c.Limit, jiraCatchUpPageSize)
		}
		if want := i * jiraCatchUpPageSize; c.Offset != want {
			t.Errorf("call %d: Offset=%d, want %d (offsets must advance or the walk repeats/skips)", i, c.Offset, want)
		}
		if c.Sort != "created_at" {
			t.Errorf("call %d: Sort=%q, want \"created_at\" for a stable paged walk", i, c.Sort)
		}
	}
	wantPages := (n + jiraCatchUpPageSize - 1) / jiraCatchUpPageSize
	if len(lister.calls) != wantPages {
		t.Errorf("made %d calls, want %d (%d beads / %d per page)", len(lister.calls), wantPages, n, jiraCatchUpPageSize)
	}

	// --- assert on CORRECTNESS: the tail bead is pre-marked ---
	tailID := corpus[n-1].ID
	if !s.isDuplicate("close:" + tailID) {
		t.Errorf("closed bead %s (past the old 1000-row cap) was NOT pre-marked — a replayed close event would re-post a duplicate Jira comment (M9)", tailID)
	}
	// A bead inside the first page must also be marked (baseline).
	if !s.isDuplicate("close:" + corpus[0].ID) {
		t.Errorf("first-page bead %s was not pre-marked", corpus[0].ID)
	}
}

// TestJiraSync_CatchUp_StopsOnShortPage pins the termination condition: a corpus
// smaller than one page costs exactly one request, not a walk to the max-pages
// backstop.
func TestJiraSync_CatchUp_StopsOnShortPage(t *testing.T) {
	lister := &recordingCatchUpLister{corpus: []*beadsapi.BeadDetail{{
		ID: "gd-only", Status: "closed",
		Labels: []string{"source:jira", "jira:PE-1"},
		Fields: map[string]string{"jira_key": "PE-1"},
	}}}
	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient("https://example.com"), Logger: slog.Default()})
	s.CatchUp(context.Background(), lister)
	if len(lister.calls) != 1 {
		t.Errorf("made %d calls for a 1-bead corpus, want 1 (a short page means done)", len(lister.calls))
	}
	if !s.isDuplicate("close:gd-only") {
		t.Error("the single bead was not pre-marked")
	}
}

// TestJiraSync_CatchUp_MidWalkErrorStopsWalk pins that a page error aborts the
// walk (the partial-pre-mark condition is surfaced, not silently accepted): the
// walk must not keep requesting pages after a failure.
func TestJiraSync_CatchUp_MidWalkErrorStopsWalk(t *testing.T) {
	const n = 1300
	corpus := make([]*beadsapi.BeadDetail, 0, n)
	for i := 0; i < n; i++ {
		corpus = append(corpus, &beadsapi.BeadDetail{
			ID: fmt.Sprintf("gd-e-%04d", i), Status: "closed",
			Labels: []string{"source:jira", fmt.Sprintf("jira:PE-%04d", i)},
			Fields: map[string]string{"jira_key": fmt.Sprintf("PE-%04d", i)},
		})
	}
	lister := &recordingCatchUpLister{corpus: corpus, err: errors.New("daemon down"), errOn: 2}
	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient("https://example.com"), Logger: slog.Default()})
	s.CatchUp(context.Background(), lister)
	if len(lister.calls) != 2 {
		t.Errorf("made %d calls, want 2 (page 0 ok, page 1 errors and aborts the walk)", len(lister.calls))
	}
}
