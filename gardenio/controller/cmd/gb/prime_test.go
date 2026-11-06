package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mockBead is a lightweight bead JSON for test responses.
type mockBead struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	Type   string            `json:"type"`
	Kind   string            `json:"kind"`
	Status string            `json:"status"`
	Fields map[string]string `json:"fields,omitempty"`
}

// mockListResponse mirrors the daemon list response.
type mockListResponse struct {
	Beads []mockBead `json:"beads"`
	Total int        `json:"total"`
}

// setupTestDaemon creates an httptest server that responds to GetBead and
// ListBeadsFiltered requests with the provided data, and sets the package-level
// daemon variable. Returns a cleanup function.
func setupTestDaemon(t *testing.T, agentBead *mockBead, projectBeads []mockBead) func() {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// GetBead: GET /v1/beads/<id>
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.URL.RawQuery == "" {
			if agentBead != nil {
				_ = json.NewEncoder(w).Encode(agentBead)
			} else {
				http.Error(w, "not found", http.StatusNotFound)
			}
			return
		}

		// ListBeadsFiltered: GET /v1/beads?...
		if r.Method == "GET" && r.URL.Path == "/v1/beads" {
			typeFilter := r.URL.Query().Get("type")
			if typeFilter == "project" {
				resp := mockListResponse{Beads: projectBeads, Total: len(projectBeads)}
				_ = json.NewEncoder(w).Encode(resp)
			} else {
				// Return empty for other queries (e.g., in_progress check).
				resp := mockListResponse{Beads: nil, Total: 0}
				_ = json.NewEncoder(w).Encode(resp)
			}
			return
		}

		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))

	oldDaemon := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}
	daemon = c

	return func() {
		daemon = oldDaemon
		srv.Close()
	}
}

// --- buildSystemPromptBody tests ---

func TestBuildSystemPromptBody_IncludesIdentityAndWorkflow(t *testing.T) {
	cleanup := setupTestDaemon(t, nil, nil)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_AGENT_ID", "")

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, "test-agent", "gardener")
	out := buf.String()

	// Identity must be present so the agent knows who it is from the
	// system prompt alone (no SessionStart hook required).
	if !strings.Contains(out, "## Your Identity") {
		t.Error("system prompt should include identity")
	}
	if !strings.Contains(out, "test-agent") {
		t.Error("system prompt should include agent ID")
	}
}

// TestBuildSystemPromptBody_IncludesSlackThreadContext verifies the fix for
// ka-0000005208: thread context must be baked into the system-prompt body for
// thread-spawned agents, bypassing the SessionStart hook's 2 KB output cap.
func TestBuildSystemPromptBody_IncludesSlackThreadContext(t *testing.T) {
	cleanup := setupTestDaemon(t, nil, nil)
	defer cleanup()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msgs := []slackThreadMessage{
			{Author: "alice", Text: "first message in thread", Timestamp: "111.222"},
			{Author: "bob", Text: "second reply", Timestamp: "111.223"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	}))
	defer srv.Close()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, "thread-agent", "thread")
	out := buf.String()

	if !strings.Contains(out, "## Slack Thread Context") {
		t.Error("system prompt should include thread context header for thread-spawned agents")
	}
	if !strings.Contains(out, "first message in thread") {
		t.Error("system prompt should include earliest thread message")
	}
	if !strings.Contains(out, "second reply") {
		t.Error("system prompt should include subsequent thread messages")
	}
}

func TestBuildSystemPromptBody_OmitsLiveState(t *testing.T) {
	cleanup := setupTestDaemon(t, nil, nil)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_AGENT_ID", "")

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, "test-agent", "crew")
	out := buf.String()

	// Live-state sections are emitted by buildPrimeBody but must be
	// excluded from the system prompt — they change every cycle and
	// would invalidate the prompt cache without delivering durable rules.
	for _, forbidden := range []string{
		"## Active Jacks",
		"## Active Agents",
		"## Auto-Assign",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("system prompt should NOT include %q (live state)", forbidden)
		}
	}
}

// --- outputIdentity tests ---

func TestOutputIdentity_AllFields(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")

	var buf bytes.Buffer
	outputIdentity(&buf, "test-agent", "gardener")
	out := buf.String()

	if !strings.Contains(out, "## Your Identity") {
		t.Error("output should contain identity header")
	}
	if !strings.Contains(out, "**Agent**: test-agent") {
		t.Error("output should contain agent name")
	}
	if !strings.Contains(out, "**Role**: gardener") {
		t.Error("output should contain role")
	}
	if !strings.Contains(out, "**Project**: gardenio") {
		t.Error("output should contain project")
	}
	if !strings.Contains(out, "Announce your role") {
		t.Error("output should contain announcement instruction")
	}
}

func TestOutputIdentity_NoInfo(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")

	var buf bytes.Buffer
	outputIdentity(&buf, "", "")
	out := buf.String()

	if out != "" {
		t.Errorf("expected empty output when no identity info, got %q", out)
	}
}

func TestOutputIdentity_RoleOnly(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")

	var buf bytes.Buffer
	outputIdentity(&buf, "", "worker")
	out := buf.String()

	if !strings.Contains(out, "**Role**: worker") {
		t.Error("output should contain role")
	}
	if strings.Contains(out, "**Agent**") {
		t.Error("output should not contain agent when empty")
	}
}

// --- outputConfigSections tests (replaced outputWorkflowContextHardcoded) ---

func TestOutputConfigSections_RendersAllSections(t *testing.T) {
	config := map[string]any{
		"prime_header":       "# Header\n",
		"session_close":      "# Close\n",
		"core_rules":         "## Rules\n",
		"commands":           "## Commands\n",
		"workflows":          "## Workflows\n",
		"decisions":          "## Decisions\n",
		"session_resumption": "## Resumption\n",
		"lifecycle":          "## Lifecycle\n",
		"stop_gate":          "## Stop Gate\n",
	}

	var buf bytes.Buffer
	outputConfigSections(&buf, config)
	out := buf.String()

	for _, expected := range []string{"# Header", "# Close", "## Rules", "## Commands", "## Workflows", "## Decisions", "## Resumption", "## Lifecycle", "## Stop Gate"} {
		if !strings.Contains(out, expected) {
			t.Errorf("output should contain %q", expected)
		}
	}
}

func TestOutputConfigSections_SkipsEmptySections(t *testing.T) {
	config := map[string]any{
		"prime_header": "# Header\n",
		"workflows":    "", // empty — should be skipped
		"lifecycle":    "## Lifecycle\n",
	}

	var buf bytes.Buffer
	outputConfigSections(&buf, config)
	out := buf.String()

	if !strings.Contains(out, "# Header") {
		t.Error("output should contain header")
	}
	if !strings.Contains(out, "## Lifecycle") {
		t.Error("output should contain lifecycle")
	}
}

func TestIsAutoAssignEnabled_DefaultDisabled(t *testing.T) {
	// No agent bead, no project bead — default is disabled (agents pull work).
	cleanup := setupTestDaemon(t, nil, nil)
	defer cleanup()

	t.Setenv("KD_AGENT_ID", "")

	got := isAutoAssignEnabled(context.Background(), "testproj")
	if got {
		t.Error("expected auto_assign to be disabled by default")
	}
}

func TestIsAutoAssignEnabled_AgentExplicitFalse(t *testing.T) {
	// Agent bead has auto_assign="false" — disabled.
	cleanup := setupTestDaemon(t, &mockBead{
		ID:     "gd-agent1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"auto_assign": "false"},
	}, nil)
	defer cleanup()

	t.Setenv("KD_AGENT_ID", "gd-agent1")

	got := isAutoAssignEnabled(context.Background(), "testproj")
	if got {
		t.Error("expected auto_assign to be disabled when agent bead has auto_assign=false")
	}
}

func TestIsAutoAssignEnabled_AgentExplicitTrue(t *testing.T) {
	// Agent bead has auto_assign="true" — enabled.
	cleanup := setupTestDaemon(t, &mockBead{
		ID:     "gd-agent1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"auto_assign": "true"},
	}, nil)
	defer cleanup()

	t.Setenv("KD_AGENT_ID", "gd-agent1")

	got := isAutoAssignEnabled(context.Background(), "testproj")
	if !got {
		t.Error("expected auto_assign to be enabled when agent bead has auto_assign=true")
	}
}

func TestIsAutoAssignEnabled_AgentFieldAbsent_ProjectDisabled(t *testing.T) {
	// Agent bead exists but has no auto_assign field.
	// Project bead has auto_assign="false" — should inherit project setting.
	cleanup := setupTestDaemon(t, &mockBead{
		ID:     "gd-agent1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"project": "testproj"},
	}, []mockBead{
		{
			ID:     "gd-proj1",
			Title:  "testproj",
			Type:   "project",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{"auto_assign": "false"},
		},
	})
	defer cleanup()

	t.Setenv("KD_AGENT_ID", "gd-agent1")

	got := isAutoAssignEnabled(context.Background(), "testproj")
	if got {
		t.Error("expected auto_assign to be disabled when agent has no override and project is disabled")
	}
}

func TestIsAutoAssignEnabled_AgentOverridesProject(t *testing.T) {
	// Agent bead has auto_assign="true", project has auto_assign="false".
	// Agent should override project.
	cleanup := setupTestDaemon(t, &mockBead{
		ID:     "gd-agent1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"auto_assign": "true"},
	}, []mockBead{
		{
			ID:     "gd-proj1",
			Title:  "testproj",
			Type:   "project",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{"auto_assign": "false"},
		},
	})
	defer cleanup()

	t.Setenv("KD_AGENT_ID", "gd-agent1")

	got := isAutoAssignEnabled(context.Background(), "testproj")
	if !got {
		t.Error("expected agent auto_assign=true to override project auto_assign=false")
	}
}

func TestIsAutoAssignEnabled_AgentDisabledOverridesProjectEnabled(t *testing.T) {
	// Agent bead has auto_assign="false", project has auto_assign="true".
	// Agent should override project.
	cleanup := setupTestDaemon(t, &mockBead{
		ID:     "gd-agent1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"auto_assign": "false"},
	}, []mockBead{
		{
			ID:     "gd-proj1",
			Title:  "testproj",
			Type:   "project",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{"auto_assign": "true"},
		},
	})
	defer cleanup()

	t.Setenv("KD_AGENT_ID", "gd-agent1")

	got := isAutoAssignEnabled(context.Background(), "testproj")
	if got {
		t.Error("expected agent auto_assign=false to override project auto_assign=true")
	}
}

func TestIsAutoAssignEnabled_NoAgentID_ProjectDisabled(t *testing.T) {
	// No KD_AGENT_ID set, project bead has auto_assign="false".
	cleanup := setupTestDaemon(t, nil, []mockBead{
		{
			ID:     "gd-proj1",
			Title:  "testproj",
			Type:   "project",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{"auto_assign": "false"},
		},
	})
	defer cleanup()

	t.Setenv("KD_AGENT_ID", "")

	got := isAutoAssignEnabled(context.Background(), "testproj")
	if got {
		t.Error("expected auto_assign to be disabled when no agent ID and project is disabled")
	}
}

// --- outputTldrDecisionHints tests ---

// startTldrDecisionDaemon stands up a minimal daemon that serves a single
// open decision (with the caller-supplied options JSON) and empty lists
// for every other query outputTldrDecisionHints and its siblings may issue.
func startTldrDecisionDaemon(t *testing.T, agentBeadID, optionsJSON string) func() {
	t.Helper()

	decBead := mockBead{
		ID:     "gd-dec001",
		Title:  "ship tldr",
		Type:   "decision",
		Kind:   "data",
		Status: "open",
		Fields: map[string]string{
			"prompt":                   "ship tldr?",
			"options":                  optionsJSON,
			"requesting_agent_bead_id": agentBeadID,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/v1/beads" {
			if r.URL.Query().Get("type") == "decision" {
				_ = json.NewEncoder(w).Encode(mockListResponse{Beads: []mockBead{decBead}, Total: 1})
				return
			}
			_ = json.NewEncoder(w).Encode(mockListResponse{Beads: nil, Total: 0})
			return
		}
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))

	old := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("new beadsapi client: %v", err)
	}
	daemon = c
	return func() {
		daemon = old
		srv.Close()
	}
}

func TestPrime_TldrDecision(t *testing.T) {
	const agentBeadID = "gd-agent42"
	opts := `[{"id":"done","short":"Summary","label":"Summary","artifact_type":"tldr","artifact_params":{"max_chars":400,"min_chars":40,"warn_at":350}}]`
	cleanup := startTldrDecisionDaemon(t, agentBeadID, opts)
	defer cleanup()

	t.Setenv("KD_AGENT_ID", agentBeadID)

	var buf bytes.Buffer
	outputTldrDecisionHints(&buf, agentBeadID)
	out := buf.String()

	// Core expectations: header names the decision, budget numbers come
	// from artifact_params, and the example shape is the constant sentence.
	for _, want := range []string{
		"Pending decision gd-dec001 has a tldr option",
		"max 400 chars",
		"warn 350",
		"min 40",
		`Example shape: "Root cause: X. Fix: Y. Verified: Z."`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tldr hint missing %q; got:\n%s", want, out)
		}
	}

	// Hint block must stay under ~250 chars so it does not pressure the
	// prime size budget (acceptance criterion on gd-wMXgNtHdqG).
	if len(out) > 260 {
		t.Errorf("tldr hint block is %d bytes; want <= 260 (~250 soft cap)", len(out))
	}
}

func TestPrime_TldrDecision_Defaults(t *testing.T) {
	// No artifact_params on the option — Phase 1 defaults (500/0/350) apply.
	const agentBeadID = "gd-agent42"
	opts := `[{"id":"done","short":"Summary","label":"Summary","artifact_type":"tldr"}]`
	cleanup := startTldrDecisionDaemon(t, agentBeadID, opts)
	defer cleanup()

	var buf bytes.Buffer
	outputTldrDecisionHints(&buf, agentBeadID)
	out := buf.String()

	for _, want := range []string{"max 500 chars", "warn 350", "min 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("default tldr hint missing %q; got:\n%s", want, out)
		}
	}
}

func TestPrime_TldrDecision_NoMatch(t *testing.T) {
	// Decision belongs to a different agent — no hint should render.
	const agentBeadID = "gd-agent42"
	opts := `[{"id":"done","short":"Summary","label":"Summary","artifact_type":"tldr"}]`
	cleanup := startTldrDecisionDaemon(t, "gd-someone-else", opts)
	defer cleanup()

	var buf bytes.Buffer
	outputTldrDecisionHints(&buf, agentBeadID)
	if got := buf.String(); got != "" {
		t.Errorf("expected no hint when decision is not ours; got %q", got)
	}
}

func TestPrime_TldrDecision_NonTldrOption(t *testing.T) {
	// Decision is ours but option is artifact_type=report — no hint.
	const agentBeadID = "gd-agent42"
	opts := `[{"id":"go","short":"Go","label":"Go","artifact_type":"report"}]`
	cleanup := startTldrDecisionDaemon(t, agentBeadID, opts)
	defer cleanup()

	var buf bytes.Buffer
	outputTldrDecisionHints(&buf, agentBeadID)
	if got := buf.String(); got != "" {
		t.Errorf("expected no hint for non-tldr option; got %q", got)
	}
}

func TestIsAutoAssignEnabled_ProjectWithLegacyPrefix(t *testing.T) {
	// Project bead has "Project: " prefix in title.
	cleanup := setupTestDaemon(t, nil, []mockBead{
		{
			ID:     "gd-proj1",
			Title:  "Project: testproj",
			Type:   "project",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{"auto_assign": "false"},
		},
	})
	defer cleanup()

	t.Setenv("KD_AGENT_ID", "")

	got := isAutoAssignEnabled(context.Background(), "testproj")
	if got {
		t.Error("expected project with legacy 'Project: ' prefix to still be matched")
	}
}

// TestPrimeProjectFlagRegistered locks the gd-nqKwUv41GO regression: the
// advice-supersession E2E invokes `gb prime --project <proj>`, but the flag
// was never registered, so cobra rejected it with "unknown flag: --project",
// exited non-zero, and produced empty stdout. The test's `|| echo ""` then
// swallowed the error, so every "advice renders" assertion failed while the
// "advice absent after supersession" assertions passed trivially (4/10 fail).
// This guard fails loudly if the flag is ever dropped again.
func TestPrimeProjectFlagRegistered(t *testing.T) {
	f := primeCmd.Flags().Lookup("project")
	if f == nil {
		t.Fatal("gb prime is missing the --project flag; the advice-supersession E2E invokes `gb prime --project <proj>` and will break (gd-nqKwUv41GO)")
	}
	if f.Value.Type() != "string" {
		t.Errorf("--project flag should be a string, got %q", f.Value.Type())
	}
}

// TestPrimeProjectFlagOverridesEnv asserts the --project flag wins over the
// KD_PROJECT / BOAT_PROJECT env vars in defaultGBProject, so callers can scope
// prime output to another project without mutating pod env.
func TestPrimeProjectFlagOverridesEnv(t *testing.T) {
	oldProject := primeProject
	defer func() { primeProject = oldProject }()

	t.Setenv("KD_PROJECT", "envkd")
	t.Setenv("BOAT_PROJECT", "envboat")

	primeProject = ""
	if got := defaultGBProject(); got != "envkd" {
		t.Errorf("with empty flag, expected env precedence envkd, got %q", got)
	}

	primeProject = "flagwins"
	if got := defaultGBProject(); got != "flagwins" {
		t.Errorf("--project flag should override env, got %q", got)
	}
}
