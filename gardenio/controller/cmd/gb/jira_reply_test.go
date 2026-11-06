package main

// Tests for `gb jira reply` (feature F2, plan ka-0000007401) — the Jira analog
// of `gb slack reply`. Mirrors the slack reply CLI tests: httptest bridge +
// beads-daemon stubs, no real network.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// clearJiraReplyLineageEnv isolates the lineage stamper from the ambient
// environment, mirroring TestSlackReply_CreatesMessageBead's setup.
func clearJiraReplyLineageEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

func TestJiraReply_MissingEnv(t *testing.T) {
	t.Setenv("JIRA_BRIDGE_URL", "")
	t.Setenv("JIRA_KEY", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	jiraReplyCmd.SetContext(context.Background())
	err := runJiraReply(jiraReplyCmd, []string{"hello"})
	if err == nil || !strings.Contains(err.Error(), "JIRA_BRIDGE_URL") {
		t.Fatalf("expected JIRA_BRIDGE_URL error, got %v", err)
	}

	t.Setenv("JIRA_BRIDGE_URL", "http://bridge.example")
	err = runJiraReply(jiraReplyCmd, []string{"hello"})
	if err == nil || !strings.Contains(err.Error(), "JIRA_KEY") {
		t.Fatalf("expected JIRA_KEY error, got %v", err)
	}
}

func TestJiraReply_PostsToBridge(t *testing.T) {
	clearJiraReplyLineageEnv(t)

	var got jiraReplyRequest
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/jira/reply" {
			t.Errorf("unexpected bridge request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jiraReplyResult{OK: true, JiraKey: "PE-42"})
	}))
	defer bridge.Close()

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_KEY", "PE-42")

	oldActor := actor
	actor = "test-agent"
	t.Cleanup(func() { actor = oldActor })

	// daemon is nil in this test → recordJiraReplyBead skips silently.
	jiraReplyCmd.SetContext(context.Background())
	if err := jiraReplyCmd.Flags().Set("public", "false"); err != nil {
		t.Fatal(err)
	}
	if err := runJiraReply(jiraReplyCmd, []string{"Deployed the fix."}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Agent != "test-agent" {
		t.Errorf("agent = %q, want test-agent", got.Agent)
	}
	if got.JiraKey != "PE-42" {
		t.Errorf("jira_key = %q, want PE-42", got.JiraKey)
	}
	if got.Text != "Deployed the fix." {
		t.Errorf("text = %q", got.Text)
	}
	if got.Visibility != "internal" {
		t.Errorf("visibility = %q, want internal by default", got.Visibility)
	}
}

func TestJiraReply_PublicFlag(t *testing.T) {
	clearJiraReplyLineageEnv(t)

	var got jiraReplyRequest
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jiraReplyResult{OK: true, JiraKey: "PE-42"})
	}))
	defer bridge.Close()

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_KEY", "PE-42")

	jiraReplyCmd.SetContext(context.Background())
	if err := jiraReplyCmd.Flags().Set("public", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jiraReplyCmd.Flags().Set("public", "false") })

	if err := runJiraReply(jiraReplyCmd, []string{"Customer-facing update"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Visibility != "public" {
		t.Errorf("visibility = %q, want public with --public", got.Visibility)
	}
}

func TestJiraReply_BridgeRejectionSurfacesError(t *testing.T) {
	clearJiraReplyLineageEnv(t)

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(jiraReplyResult{OK: false, Error: "project is not writable"})
	}))
	defer bridge.Close()

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_KEY", "FSM-100")

	jiraReplyCmd.SetContext(context.Background())
	err := runJiraReply(jiraReplyCmd, []string{"hello"})
	if err == nil {
		t.Fatal("expected an error when the bridge rejects the reply")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "project is not writable") {
		t.Errorf("error should carry status + bridge reason, got: %v", err)
	}
}

func TestJiraReply_SendsBearerToken(t *testing.T) {
	clearJiraReplyLineageEnv(t)

	var auth string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jiraReplyResult{OK: true})
	}))
	defer bridge.Close()

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_KEY", "PE-42")
	t.Setenv("JIRA_BRIDGE_TOKEN", "sekrit")

	jiraReplyCmd.SetContext(context.Background())
	if err := runJiraReply(jiraReplyCmd, []string{"hi"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want Bearer sekrit", auth)
	}
}

// TestJiraReply_CreatesMessageBead verifies the lineage side: after posting,
// gb jira reply records a type=message bead labeled jira-reply (mirroring
// recordSlackReplyBead) and closes it.
func TestJiraReply_CreatesMessageBead(t *testing.T) {
	clearJiraReplyLineageEnv(t)

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/jira/reply" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jiraReplyResult{OK: true, JiraKey: "PE-42"})
			return
		}
		t.Errorf("unexpected bridge request: %s %s", r.Method, r.URL.Path)
	}))
	defer bridge.Close()

	var createdBody map[string]any
	closed := false
	beads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewDecoder(r.Body).Decode(&createdBody)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-JREPLY1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			closed = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		default:
			t.Errorf("unexpected daemon %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer beads.Close()
	swapDaemon(t, beads.URL)

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_KEY", "PE-42")

	oldActor := actor
	actor = "test-agent"
	t.Cleanup(func() { actor = oldActor })

	// Silence stdout.
	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = old
	}()

	jiraReplyCmd.SetContext(context.Background())
	if err := runJiraReply(jiraReplyCmd, []string{"On it"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if createdBody == nil {
		t.Fatal("expected a message bead to be created, got none")
	}
	if got := createdBody["type"]; got != "message" {
		t.Errorf("bead type = %v, want message", got)
	}
	labels, _ := createdBody["labels"].([]any)
	foundReplyLabel := false
	foundFromLabel := false
	for _, l := range labels {
		switch l {
		case "jira-reply":
			foundReplyLabel = true
		case "from:test-agent":
			foundFromLabel = true
		}
	}
	if !foundReplyLabel {
		t.Errorf("expected `jira-reply` label, got %v", labels)
	}
	if !foundFromLabel {
		t.Errorf("expected `from:test-agent` label, got %v", labels)
	}
	var fields map[string]string
	if raw, ok := createdBody["fields"].(string); ok {
		_ = json.Unmarshal([]byte(raw), &fields)
	} else if m, ok := createdBody["fields"].(map[string]any); ok {
		fields = make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				fields[k] = s
			}
		}
	}
	if fields["jira_key"] != "PE-42" {
		t.Errorf("fields.jira_key = %q, want PE-42 (fields: %v)", fields["jira_key"], createdBody["fields"])
	}
	if !closed {
		t.Error("expected bead to be closed after creation")
	}
}

// TestJiraReply_LinksReplyToMirror verifies the outbound reply message bead is
// linked into the ticket graph: a `triggered-by` edge is stamped from the reply
// bead to the ticket's Jira-mirror bead (label jira:<KEY>), mirroring the
// bridge's inbound writeMirrorTriggeredBy so the reply is traversable from the
// ticket node in beads-viewer.
func TestJiraReply_LinksReplyToMirror(t *testing.T) {
	clearJiraReplyLineageEnv(t)

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/jira/reply" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jiraReplyResult{OK: true, JiraKey: "PE-42"})
			return
		}
		t.Errorf("unexpected bridge request: %s %s", r.Method, r.URL.Path)
	}))
	defer bridge.Close()

	var depTarget string    // depends_on_id posted on the reply bead
	var listedLabels string // labels the mirror lookup queried by
	beads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-JREPLY1"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			listedLabels = r.URL.Query().Get("labels")
			// Return the ticket's Jira-mirror bead.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []any{map[string]any{"id": "gd-MIRROR", "title": "PE-42 mirror"}},
				"total": 1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/gd-JREPLY1/dependencies":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["type"] == "triggered-by" {
				depTarget = body["depends_on_id"]
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected daemon %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer beads.Close()
	swapDaemon(t, beads.URL)

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_KEY", "PE-42")

	oldActor := actor
	actor = "test-agent"
	t.Cleanup(func() { actor = oldActor })

	// Silence stdout.
	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = old
	}()

	jiraReplyCmd.SetContext(context.Background())
	if err := runJiraReply(jiraReplyCmd, []string{"On it"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(listedLabels, "jira:PE-42") {
		t.Errorf("mirror lookup labels = %q, want to contain jira:PE-42", listedLabels)
	}
	if depTarget != "gd-MIRROR" {
		t.Errorf("triggered-by edge target = %q, want gd-MIRROR (the ticket mirror)", depTarget)
	}
}

// TestJiraReply_NoMirrorNoLink verifies the reply still succeeds and creates its
// message bead when no Jira-mirror bead exists yet (poller not synced / manual
// binding): the mirror lookup returns empty and no triggered-by edge to a mirror
// is attempted. The reply keeps its agent/trigger lineage regardless.
func TestJiraReply_NoMirrorNoLink(t *testing.T) {
	clearJiraReplyLineageEnv(t)

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/jira/reply" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jiraReplyResult{OK: true, JiraKey: "PE-42"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bridge.Close()

	created := false
	closed := false
	depPosted := false
	beads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			created = true
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-JREPLY1"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// No mirror synced yet.
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dependencies"):
			depPosted = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			closed = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected daemon %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer beads.Close()
	swapDaemon(t, beads.URL)

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_KEY", "PE-42")

	// Silence stdout.
	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = old
	}()

	jiraReplyCmd.SetContext(context.Background())
	if err := runJiraReply(jiraReplyCmd, []string{"On it"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !created {
		t.Error("expected the reply message bead to be created even with no mirror")
	}
	if !closed {
		t.Error("expected the reply message bead to be closed")
	}
	// A lineage triggered-by edge to an agent/trigger bead may still be posted
	// in other envs, but this test cleared those env vars, so with no mirror and
	// no agent/trigger context, no dependency should be attempted at all.
	if depPosted {
		t.Error("no mirror and no agent/trigger context: expected no triggered-by edge")
	}
}

// TestJiraReply_BeadCreateFailureDoesNotFailReply verifies a beads daemon
// outage cannot make a successful Jira reply look like a failure.
func TestJiraReply_BeadCreateFailureDoesNotFailReply(t *testing.T) {
	clearJiraReplyLineageEnv(t)

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/jira/reply" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jiraReplyResult{OK: true, JiraKey: "PE-42"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bridge.Close()

	beads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer beads.Close()
	swapDaemon(t, beads.URL)

	t.Setenv("JIRA_BRIDGE_URL", bridge.URL)
	t.Setenv("JIRA_KEY", "PE-42")

	// Silence stdout.
	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = old
	}()

	jiraReplyCmd.SetContext(context.Background())
	if err := runJiraReply(jiraReplyCmd, []string{"still fine"}); err != nil {
		t.Fatalf("bead failure must not fail the reply, got: %v", err)
	}
}

// TestResolveJiraReplyEnv_BeadFallback verifies the agent-bead jira_key
// fallback when JIRA_KEY is not injected (pre-F2 pods).
func TestResolveJiraReplyEnv_BeadFallback(t *testing.T) {
	beads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-AGENT1",
				"title":  "agent bead",
				"fields": map[string]string{"jira_key": "DEVOPS-9"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer beads.Close()
	swapDaemon(t, beads.URL)

	t.Setenv("JIRA_BRIDGE_URL", "http://bridge.example")
	t.Setenv("JIRA_KEY", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-AGENT1")

	_, jiraKey, err := resolveJiraReplyEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jiraKey != "DEVOPS-9" {
		t.Errorf("jiraKey = %q, want DEVOPS-9 (bead fallback)", jiraKey)
	}
	if os.Getenv("JIRA_KEY") != "DEVOPS-9" {
		t.Error("expected JIRA_KEY to be cached in env after fallback")
	}
}
