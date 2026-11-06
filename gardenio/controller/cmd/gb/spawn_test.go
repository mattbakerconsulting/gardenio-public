package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/clicreds/credstore"
	"gardenio/controller/internal/beadsapi"
	"github.com/spf13/cobra"
)

func TestSpawnIsValidAgentName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid lowercase", "my-agent", true},
		{"valid with numbers", "agent-42", true},
		{"valid single word", "worker", true},
		{"empty", "", false},
		{"uppercase", "MyAgent", false},
		{"spaces", "my agent", false},
		{"underscores", "my_agent", false},
		{"special chars", "agent@1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spawnIsValidAgentName(tt.input); got != tt.want {
				t.Errorf("spawnIsValidAgentName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSpawnGenerateAgentName(t *testing.T) {
	tests := []struct {
		name       string
		project    string
		title      string
		wantPrefix string
	}{
		{"with project", "gardenio", "Fix authentication bug", "gardenio-fix-authentication-bug-"},
		{"no project", "", "Fix authentication bug", "fix-authentication-bug-"},
		{"long title", "gardenio", "Implement the new feature with many words in the title", "gardenio-implement-the-new-"},
		{"special chars", "gardenio", "Update API: handle /users endpoint", "gardenio-update-api-handle-"},
		{"empty title with project", "gardenio", "", "gardenio-agent-"},
		{"empty title no project", "", "", "agent-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := spawnGenerateAgentName(tt.project, tt.title)
			if name == "" {
				t.Error("expected non-empty name")
			}
			if !strings.HasPrefix(name, tt.wantPrefix) {
				t.Errorf("spawnGenerateAgentName(%q, %q) = %q, want prefix %q", tt.project, tt.title, name, tt.wantPrefix)
			}
			// Should be a valid agent name.
			if !spawnIsValidAgentName(name) {
				t.Errorf("generated name %q is not a valid agent name", name)
			}
		})
	}
}

// --- Spawn identity (spawned_by) tests ---

func TestSpawnIdentity_FromCredstoreEmail(t *testing.T) {
	// Create a temporary credstore with user email.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	credPath := filepath.Join(dir, "gardenio", "credentials.json")
	os.MkdirAll(filepath.Dir(credPath), 0o700)
	data := map[string]any{
		"current_context": "default",
		"contexts": map[string]any{
			"default": map[string]any{
				"server":       "https://beads.example.com",
				"access_token": "tok",
				"user":         map[string]string{"email": "alice@example.com", "username": "alice"},
			},
		},
	}
	raw, _ := json.Marshal(data)
	os.WriteFile(credPath, raw, 0o600)

	// Load and check — this mirrors the logic in spawnAndPrint.
	store, err := credstore.Load("")
	if err != nil {
		t.Fatalf("credstore.Load: %v", err)
	}
	cur := store.Current()
	if cur == nil {
		t.Fatal("expected current context")
	}
	if cur.User == nil || cur.User.Email != "alice@example.com" {
		t.Fatalf("expected user email alice@example.com, got %+v", cur.User)
	}

	// Simulate the identity extraction from spawnAndPrint.
	spawnedBy := ""
	if cur.User != nil && cur.User.Email != "" {
		spawnedBy = cur.User.Email
	}
	if spawnedBy != "alice@example.com" {
		t.Errorf("spawned_by=%q, want alice@example.com", spawnedBy)
	}
}

func TestSpawnIdentity_FallsBackToActor(t *testing.T) {
	// No credstore available — falls back to actor variable.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// No credentials.json written → credstore.Load returns empty store.

	store, err := credstore.Load("")
	if err != nil {
		t.Fatalf("credstore.Load: %v", err)
	}

	// No user in store — extract should return empty.
	spawnedBy := ""
	if cur := store.Current(); cur != nil && cur.User != nil && cur.User.Email != "" {
		spawnedBy = cur.User.Email
	}
	if spawnedBy != "" {
		t.Fatalf("expected empty spawned_by from empty credstore, got %q", spawnedBy)
	}

	// Actor fallback (mirrors spawnAndPrint logic).
	testActor := "operator@example.com"
	if spawnedBy == "" && testActor != "" && testActor != "unknown" {
		spawnedBy = testActor
	}
	if spawnedBy != "operator@example.com" {
		t.Errorf("spawned_by=%q, want operator@example.com", spawnedBy)
	}
}

func TestSpawnIdentity_NeitherPresent(t *testing.T) {
	// No credstore, no actor → spawned_by should be empty.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	store, err := credstore.Load("")
	if err != nil {
		t.Fatalf("credstore.Load: %v", err)
	}

	spawnedBy := ""
	if cur := store.Current(); cur != nil && cur.User != nil && cur.User.Email != "" {
		spawnedBy = cur.User.Email
	}
	// Actor is empty or "unknown".
	testActor := ""
	if spawnedBy == "" && testActor != "" && testActor != "unknown" {
		spawnedBy = testActor
	}
	if spawnedBy != "" {
		t.Errorf("expected empty spawned_by, got %q", spawnedBy)
	}

	// Also test with actor="unknown".
	testActor = "unknown"
	if spawnedBy == "" && testActor != "" && testActor != "unknown" {
		spawnedBy = testActor
	}
	if spawnedBy != "" {
		t.Errorf("expected empty spawned_by with actor=unknown, got %q", spawnedBy)
	}
}

func TestSpawnIdentity_CredstoreOverridesActor(t *testing.T) {
	// When credstore has user info AND actor is set, credstore wins.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	credPath := filepath.Join(dir, "gardenio", "credentials.json")
	os.MkdirAll(filepath.Dir(credPath), 0o700)
	data := map[string]any{
		"current_context": "default",
		"contexts": map[string]any{
			"default": map[string]any{
				"server":       "https://beads.example.com",
				"access_token": "tok",
				"user":         map[string]string{"email": "alice@example.com"},
			},
		},
	}
	raw, _ := json.Marshal(data)
	os.WriteFile(credPath, raw, 0o600)

	store, err := credstore.Load("")
	if err != nil {
		t.Fatalf("credstore.Load: %v", err)
	}

	// Simulate the full spawnAndPrint identity resolution.
	spawnedBy := ""
	if cur := store.Current(); cur != nil && cur.User != nil && cur.User.Email != "" {
		spawnedBy = cur.User.Email
	}
	testActor := "fallback-actor"
	if spawnedBy == "" && testActor != "" && testActor != "unknown" {
		spawnedBy = testActor
	}

	// Credstore email should win over actor.
	if spawnedBy != "alice@example.com" {
		t.Errorf("spawned_by=%q, want alice@example.com (credstore should override actor)", spawnedBy)
	}
}

// --- Slack thread field inheritance (ka-0000007355) ---

func TestTaskHasSlackBinding(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		fields map[string]string
		want   bool
	}{
		{
			name:   "source:slack label",
			labels: []string{"project:gardenio", "source:slack"},
			fields: nil,
			want:   true,
		},
		{
			name:   "slack_thread_channel field only",
			labels: []string{"project:gardenio"},
			fields: map[string]string{"slack_thread_channel": "C123"},
			want:   true,
		},
		{
			name:   "both label and field",
			labels: []string{"source:slack"},
			fields: map[string]string{"slack_thread_channel": "C123"},
			want:   true,
		},
		{
			name:   "neither",
			labels: []string{"project:gardenio", "role:crew"},
			fields: map[string]string{"task_id": "gd-abc"},
			want:   false,
		},
		{
			name:   "empty",
			labels: nil,
			fields: nil,
			want:   false,
		},
		{
			name:   "empty slack_thread_channel value",
			labels: []string{"project:gardenio"},
			fields: map[string]string{"slack_thread_channel": ""},
			want:   false,
		},
		{
			name:   "thread_id field only (non-slack wish)",
			labels: []string{"project:gardenio"},
			fields: map[string]string{"thread_id": "gd-W1sh"},
			want:   true,
		},
		{
			name:   "empty thread_id value",
			labels: []string{"project:gardenio"},
			fields: map[string]string{"thread_id": ""},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := taskHasSlackBinding(tt.labels, tt.fields); got != tt.want {
				t.Errorf("taskHasSlackBinding(%v, %v) = %v, want %v", tt.labels, tt.fields, got, tt.want)
			}
		})
	}
}

func TestExtractSlackInheritFields(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		fields map[string]string
		want   map[string]string
	}{
		{
			name:   "non-slack task returns nil",
			labels: []string{"project:gardenio"},
			fields: map[string]string{"task_id": "gd-abc"},
			want:   nil,
		},
		{
			name:   "source:slack with full field set",
			labels: []string{"source:slack", "project:gardenio"},
			fields: map[string]string{
				"slack_thread_channel": "C123",
				"slack_thread_ts":      "1700000000.000100",
				"slack_spawn_channel":  "C456",
				"slack_opener_user_id": "U789",
				"slack_permalink":      "https://slack.example.com/archives/C123/p1700000000000100",
				"unrelated":            "ignored",
			},
			want: map[string]string{
				"slack_thread_channel": "C123",
				"slack_thread_ts":      "1700000000.000100",
				"slack_spawn_channel":  "C456",
				"slack_opener_user_id": "U789",
				"slack_permalink":      "https://slack.example.com/archives/C123/p1700000000000100",
			},
		},
		{
			name:   "slack_thread_channel field triggers inheritance even without label",
			labels: []string{"project:gardenio"},
			fields: map[string]string{
				"slack_thread_channel": "C123",
				"slack_thread_ts":      "1700000000.000100",
			},
			want: map[string]string{
				"slack_thread_channel": "C123",
				"slack_thread_ts":      "1700000000.000100",
			},
		},
		{
			name:   "permalink omitted when empty",
			labels: []string{"source:slack"},
			fields: map[string]string{
				"slack_thread_channel": "C123",
				"slack_thread_ts":      "1700000000.000100",
				"slack_permalink":      "",
			},
			want: map[string]string{
				"slack_thread_channel": "C123",
				"slack_thread_ts":      "1700000000.000100",
			},
		},
		{
			name:   "source:slack label but no slack fields populated returns nil",
			labels: []string{"source:slack"},
			fields: map[string]string{"other": "value"},
			want:   nil,
		},
		{
			name:   "non-slack wish carrying only thread_id inherits thread_id",
			labels: []string{"project:gardenio"},
			fields: map[string]string{"thread_id": "gd-W1sh"},
			want:   map[string]string{"thread_id": "gd-W1sh"},
		},
		{
			name:   "thread_id with legacy slack_thread_bead_id alias both carried",
			labels: []string{"project:gardenio"},
			fields: map[string]string{
				"thread_id":            "gd-W1sh",
				"slack_thread_bead_id": "gd-W1sh",
			},
			want: map[string]string{
				"thread_id":            "gd-W1sh",
				"slack_thread_bead_id": "gd-W1sh",
			},
		},
		{
			name:   "slack fields plus thread_id all carried together",
			labels: []string{"source:slack"},
			fields: map[string]string{
				"slack_thread_channel": "C123",
				"slack_thread_ts":      "1700000000.000100",
				"thread_id":            "gd-W1sh",
			},
			want: map[string]string{
				"slack_thread_channel": "C123",
				"slack_thread_ts":      "1700000000.000100",
				"thread_id":            "gd-W1sh",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSlackInheritFields(tt.labels, tt.fields)
			if len(got) != len(tt.want) {
				t.Fatalf("extractSlackInheritFields len=%d want=%d (got=%v want=%v)",
					len(got), len(tt.want), got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("field %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestInheritSlackFieldsFromTask_SourceSlackFixture exercises the full path
// `gb spawn --task <id>` would take when the task bead is a source:slack
// thread fixture: stub the daemon GetBead endpoint, invoke the helper, assert
// the returned map carries all five canonical slack fields.
func TestInheritSlackFieldsFromTask_SourceSlackFixture(t *testing.T) {
	taskID := "gd-SLACK001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/beads/"+taskID {
			http.NotFound(w, r)
			return
		}
		body := map[string]any{
			"id":     taskID,
			"title":  "Slack thread: please fix the broken pipeline",
			"type":   "task",
			"kind":   "issue",
			"status": "open",
			"labels": []string{"source:slack", "project:gardenio"},
			"fields": map[string]any{
				"slack_thread_channel": "C-THREAD",
				"slack_thread_ts":      "1700000000.000100",
				"slack_spawn_channel":  "C-SPAWN",
				"slack_opener_user_id": "U-OPENER",
				"slack_permalink":      "https://slack.example.com/archives/C-THREAD/p1700000000000100",
				"spawn_source":         "slack-thread",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	got := inheritSlackFieldsFromTask(context.Background(), taskID)
	want := map[string]string{
		"slack_thread_channel": "C-THREAD",
		"slack_thread_ts":      "1700000000.000100",
		"slack_spawn_channel":  "C-SPAWN",
		"slack_opener_user_id": "U-OPENER",
		"slack_permalink":      "https://slack.example.com/archives/C-THREAD/p1700000000000100",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d fields, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q = %q, want %q", k, got[k], v)
		}
	}
}

// TestInheritSlackFieldsFromTask_NonSlackTaskReturnsNil verifies that an
// ordinary task bead (no source:slack label, no slack fields) produces no
// inherited fields — keeps the normal `gb spawn --task` flow unchanged.
func TestInheritSlackFieldsFromTask_NonSlackTaskReturnsNil(t *testing.T) {
	taskID := "gd-PLAIN001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/beads/"+taskID {
			http.NotFound(w, r)
			return
		}
		body := map[string]any{
			"id":     taskID,
			"title":  "Fix the thing",
			"type":   "task",
			"kind":   "issue",
			"status": "open",
			"labels": []string{"project:gardenio"},
			"fields": map[string]any{"priority": "2"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	if got := inheritSlackFieldsFromTask(context.Background(), taskID); got != nil {
		t.Errorf("expected nil for non-slack task, got %v", got)
	}
}

// TestInheritSlackFieldsFromTask_FieldOnlyFixture covers the legacy case where
// a task carries `slack_thread_channel` but lacks the `source:slack` label
// (older thread beads predating the label convention).
func TestInheritSlackFieldsFromTask_FieldOnlyFixture(t *testing.T) {
	taskID := "gd-LEGACY01"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/beads/"+taskID {
			http.NotFound(w, r)
			return
		}
		body := map[string]any{
			"id":     taskID,
			"title":  "Legacy slack-derived task",
			"type":   "task",
			"kind":   "issue",
			"status": "open",
			"labels": []string{"project:gardenio"},
			"fields": map[string]any{
				"slack_thread_channel": "C-OLD",
				"slack_thread_ts":      "1690000000.000100",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	got := inheritSlackFieldsFromTask(context.Background(), taskID)
	if got["slack_thread_channel"] != "C-OLD" || got["slack_thread_ts"] != "1690000000.000100" {
		t.Errorf("expected legacy slack fields inherited, got %v", got)
	}
	if _, ok := got["slack_permalink"]; ok {
		t.Errorf("expected slack_permalink not to be set when missing on task, got %v", got)
	}
}

// TestResolveSlackFields covers the precedence rules between task-bead
// inheritance and explicit --slack-thread-* flag overrides:
//   - explicit flags win over inherited values
//   - explicit-only (no task) still produces fields
//   - non-slack task + no flags returns nil
func TestResolveSlackFields(t *testing.T) {
	slackTaskID := "gd-RSF-SLACK"
	plainTaskID := "gd-RSF-PLAIN"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/beads/" + slackTaskID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": slackTaskID, "type": "task", "kind": "issue", "status": "open",
				"labels": []string{"source:slack"},
				"fields": map[string]any{
					"slack_thread_channel": "C-INHERIT",
					"slack_thread_ts":      "1700000000.000100",
					"slack_permalink":      "https://example/p1",
				},
			})
		case "/v1/beads/" + plainTaskID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": plainTaskID, "type": "task", "kind": "issue", "status": "open",
				"labels": []string{"project:gardenio"},
				"fields": map[string]any{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	tests := []struct {
		name            string
		taskID          string
		explicitChannel string
		explicitTs      string
		wantChannel     string
		wantTs          string
		wantPermalink   string // "" means must not be present
		wantNil         bool
	}{
		{
			name:          "inheritance only (slack task, no flags)",
			taskID:        slackTaskID,
			wantChannel:   "C-INHERIT",
			wantTs:        "1700000000.000100",
			wantPermalink: "https://example/p1",
		},
		{
			name:            "explicit flags override inherited values",
			taskID:          slackTaskID,
			explicitChannel: "C-OVERRIDE",
			explicitTs:      "1800000000.000200",
			wantChannel:     "C-OVERRIDE",
			wantTs:          "1800000000.000200",
			wantPermalink:   "https://example/p1", // other inherited fields untouched
		},
		{
			name:            "explicit channel only overrides; ts inherited",
			taskID:          slackTaskID,
			explicitChannel: "C-OVERRIDE",
			wantChannel:     "C-OVERRIDE",
			wantTs:          "1700000000.000100",
			wantPermalink:   "https://example/p1",
		},
		{
			name:            "explicit flags with no task",
			explicitChannel: "C-EXPLICIT",
			explicitTs:      "1900000000.000300",
			wantChannel:     "C-EXPLICIT",
			wantTs:          "1900000000.000300",
		},
		{
			name:    "non-slack task and no flags returns nil",
			taskID:  plainTaskID,
			wantNil: true,
		},
		{
			name:    "no task and no flags returns nil",
			wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveSlackFields(context.Background(), tt.taskID, tt.explicitChannel, tt.explicitTs)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if got["slack_thread_channel"] != tt.wantChannel {
				t.Errorf("slack_thread_channel = %q, want %q", got["slack_thread_channel"], tt.wantChannel)
			}
			if got["slack_thread_ts"] != tt.wantTs {
				t.Errorf("slack_thread_ts = %q, want %q", got["slack_thread_ts"], tt.wantTs)
			}
			if tt.wantPermalink != "" && got["slack_permalink"] != tt.wantPermalink {
				t.Errorf("slack_permalink = %q, want %q", got["slack_permalink"], tt.wantPermalink)
			}
		})
	}
}

// --- Gardener→crew channel inheritance (ka-0000004174) ---

// TestSpawnerSlackChannel_EnvWins verifies SLACK_SPAWN_CHANNEL short-circuits
// before any daemon lookup.
func TestSpawnerSlackChannel_EnvWins(t *testing.T) {
	t.Setenv("SLACK_SPAWN_CHANNEL", "C-ENV")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	if got := spawnerSlackChannel(context.Background()); got != "C-ENV" {
		t.Errorf("spawnerSlackChannel = %q, want C-ENV", got)
	}
}

// TestSpawnerSlackChannel_FromOwnBead exercises the gardener-pod path: no env
// channel, but the agent's own bead (KD_AGENT_ID) carries a channel field.
// Also asserts field precedence: slack_spawn_channel > schedule_slack_channel.
func TestSpawnerSlackChannel_FromOwnBead(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{
			name:   "slack_spawn_channel preferred",
			fields: map[string]any{"slack_spawn_channel": "C-SPAWN", "schedule_slack_channel": "C-SCHED"},
			want:   "C-SPAWN",
		},
		{
			name:   "schedule_slack_channel when no spawn channel",
			fields: map[string]any{"schedule_slack_channel": "C-SCHED"},
			want:   "C-SCHED",
		},
		{
			name:   "slack_thread_channel last resort",
			fields: map[string]any{"slack_thread_channel": "C-THREAD"},
			want:   "C-THREAD",
		},
		{
			name:   "no channel fields yields empty",
			fields: map[string]any{"role": "gardener"},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selfID := "gd-SELF01"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/beads/"+selfID {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": selfID, "type": "agent", "kind": "config", "status": "open",
					"fields": tt.fields,
				})
			}))
			t.Cleanup(srv.Close)

			c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			oldDaemon := daemon
			daemon = c
			t.Cleanup(func() { daemon = oldDaemon })

			t.Setenv("SLACK_SPAWN_CHANNEL", "")
			t.Setenv("KD_AGENT_ID", selfID)
			t.Setenv("BOAT_AGENT_BEAD_ID", "")

			if got := spawnerSlackChannel(context.Background()); got != tt.want {
				t.Errorf("spawnerSlackChannel = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSpawnerSlackChannel_NoBindingReturnsEmpty verifies that outside a pod
// (no env, no self-bead id) the helper returns "" so routing falls through to
// the existing channel_roles / project-primary behavior.
func TestSpawnerSlackChannel_NoBindingReturnsEmpty(t *testing.T) {
	t.Setenv("SLACK_SPAWN_CHANNEL", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	if got := spawnerSlackChannel(context.Background()); got != "" {
		t.Errorf("spawnerSlackChannel = %q, want empty", got)
	}
}

// TestRunSpawn_SlackSpawnChannelFlag is the regression for ka-0000004297: an
// explicit --slack-spawn-channel must land on the created agent bead as
// slack_spawn_channel (a channel ID passes through verbatim — no bridge
// resolution needed), so resolveChannel pins the agent's reporting channel
// FIRST, ahead of channel_roles[role]. This is what lets a manually fired
// gardener (no schedule_slack_channel) report to its own channel instead of
// falling through to channel_roles[role:gardener].
func TestRunSpawn_SlackSpawnChannelFlag(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_PROJECT", "")
	// No spawner bead / env channel — isolate the flag as the only source.
	t.Setenv("SLACK_SPAWN_CHANNEL", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	// Unset the bridge URL so resolveBridgeURL yields "" and the flag value
	// (already an ID) is stored verbatim without a network round-trip.
	t.Setenv("SLACK_BRIDGE_URL", "")

	var sawCreate bool
	var createdSpawnChannel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			sawCreate = true
			var body struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var fields map[string]string
			_ = json.Unmarshal(body.Fields, &fields)
			createdSpawnChannel = fields["slack_spawn_channel"]
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SPAWNCH01"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	_, out, runErr := newSpawnTestCmd(t, []string{"--role", "gardener", "--slack-spawn-channel", "C0B5S61RVNU"})
	if runErr != nil {
		t.Fatalf("spawn with --slack-spawn-channel must not error, got: %v\nout: %s", runErr, out.String())
	}
	if !sawCreate {
		t.Fatal("spawn should have reached the daemon create POST")
	}
	if createdSpawnChannel != "C0B5S61RVNU" {
		t.Errorf("agent bead fields.slack_spawn_channel = %q, want %q (from --slack-spawn-channel)", createdSpawnChannel, "C0B5S61RVNU")
	}
}

func TestSpawnProjectFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"with project", []string{"role:crew", "project:gardenio"}, "gardenio"},
		{"no project", []string{"role:crew"}, ""},
		{"empty", nil, ""},
		{"multiple labels", []string{"project:monorepo", "role:engineer"}, "monorepo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spawnProjectFromLabels(tt.labels); got != tt.want {
				t.Errorf("spawnProjectFromLabels(%v) = %q, want %q", tt.labels, got, tt.want)
			}
		})
	}
}

// TestResolveTaskProject_KdBeadLabel is the regression test for ka-0000007269:
// `gb spawn --task <gd-bead>` must infer the project from the bead's project:
// label, not hard-error with "project is required". Previously only ticket
// references (PE-1234) read labels, so a plain gd- bead fell through to the
// env-only path.
func TestResolveTaskProject_KdBeadLabel(t *testing.T) {
	taskID := "gd-PROJ0001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/beads/"+taskID {
			http.NotFound(w, r)
			return
		}
		body := map[string]any{
			"id":     taskID,
			"title":  "test task",
			"type":   "task",
			"kind":   "issue",
			"status": "open",
			"labels": []string{"project:gardenio"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	gotID, gotProject, bead, err := resolveTaskProject(context.Background(), taskID)
	if err != nil {
		t.Fatalf("resolveTaskProject returned error: %v", err)
	}
	if gotID != taskID {
		t.Errorf("taskID = %q, want %q", gotID, taskID)
	}
	if gotProject != "gardenio" {
		t.Errorf("project = %q, want %q", gotProject, "gardenio")
	}
	if bead == nil || bead.Title != "test task" {
		t.Errorf("bead = %+v, want title %q", bead, "test task")
	}
}

// TestResolveTaskProject_EmptyTaskRef ensures the no-task path stays a no-op:
// no daemon call, empty project, nil bead.
func TestResolveTaskProject_EmptyTaskRef(t *testing.T) {
	gotID, gotProject, bead, err := resolveTaskProject(context.Background(), "")
	if err != nil {
		t.Fatalf("resolveTaskProject(\"\") returned error: %v", err)
	}
	if gotID != "" || gotProject != "" || bead != nil {
		t.Errorf("resolveTaskProject(\"\") = (%q, %q, %v), want empty/nil", gotID, gotProject, bead)
	}
}

// --- Bare `gb spawn` guard (ka-0000003691 / bug ka-0000003560) ---

// newSpawnTestCmd builds a fresh spawn command wired to runSpawn with the same
// flag set as the real command, parses argv, and captures stdout/stderr. It is
// isolated per-test so cobra global state never leaks between runs. Any
// SpawnAgent call fans out to the daemon HTTP client, so tests that must assert
// "no spawn happened" point `daemon` at a server that fails the test if hit.
func newSpawnTestCmd(t *testing.T, argv []string) (*cobra.Command, *bytes.Buffer, error) {
	t.Helper()
	cmd := &cobra.Command{Use: "spawn", Args: spawnNoPositionalArgs, RunE: runSpawn, SilenceUsage: true, SilenceErrors: true}
	registerSpawnFlags(cmd)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(argv)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	return cmd, &out, err
}

// failOnSpawnDaemon installs a daemon whose HTTP handler fails the test if the
// spawn flow reaches the daemon at all (dedup GET, create POST, etc.). Used to
// prove the guard short-circuits before any mutation.
func failOnSpawnDaemon(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("daemon must not be called on a guarded bare spawn, got %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected daemon call", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

// TestRunSpawn_BareNoFlagsIsBlocked is the core regression: `gb spawn` with
// zero flags must return an error, print usage, and NOT touch the daemon.
func TestRunSpawn_BareNoFlagsIsBlocked(t *testing.T) {
	// Even with a project available in the env, a bare spawn is blocked before
	// project resolution — the guard fires on missing intent, not missing project.
	t.Setenv("BOAT_PROJECT", "gardenio")
	failOnSpawnDaemon(t)

	_, out, err := newSpawnTestCmd(t, nil)
	if err == nil {
		t.Fatal("expected an error from bare `gb spawn`, got nil")
	}
	if !strings.Contains(err.Error(), "unnamed idle agent") {
		t.Errorf("error should explain the bare-spawn guard, got: %v", err)
	}
	// cmd.Help() output should have been printed (usage), routed to our buffer.
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage/help text on bare spawn, got: %q", out.String())
	}
}

// TestRunSpawn_StrayPositionalGivesQuotingHint is the argv-guard regression
// (bug ka-0000002137): a mis-quoted `--prompt` body lands as a stray positional,
// and `gb spawn` must reject it with a targeted shell-quoting hint (pointing at
// --prompt + the heredoc recipe) instead of cobra's generic "unknown command",
// which misdirects the caller toward a bad-subcommand theory. It must also NOT
// touch the daemon (the guard fires at argv validation, before RunE).
func TestRunSpawn_StrayPositionalGivesQuotingHint(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	failOnSpawnDaemon(t)

	// A mis-quoted `gb spawn --prompt 'REVIEW-AND-MERGE ... gb stop'` closes the
	// quote early on a nested `'`, so the body arrives as a stray positional.
	stray := "REVIEW-AND-MERGE MR Acme/Platform/fluentbit!7 then gb stop"
	_, _, err := newSpawnTestCmd(t, []string{stray})
	if err == nil {
		t.Fatal("expected an error from a stray positional arg, got nil")
	}
	msg := err.Error()
	// Must NOT be cobra's generic misdirecting error.
	if strings.Contains(msg, "unknown command") {
		t.Errorf("argv-guard should replace the generic cobra error, got: %v", err)
	}
	// Must name the real cause and the fix.
	for _, want := range []string{"takes no positional arguments", "--prompt", "heredoc", "1 unexpected arg"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
	// Must echo the offending arg so the caller can spot it.
	if !strings.Contains(msg, "REVIEW-AND-MERGE") {
		t.Errorf("error should echo the stray arg, got: %v", err)
	}
}

// TestSpawnNoPositionalArgs_TruncatesLongArg verifies the echoed stray arg is
// capped so a kilobyte-long mis-quoted prompt body does not flood the terminal.
func TestSpawnNoPositionalArgs_TruncatesLongArg(t *testing.T) {
	long := strings.Repeat("x", maxEchoedStrayArg*3)
	err := spawnNoPositionalArgs(nil, []string{long})
	if err == nil {
		t.Fatal("expected an error for a positional arg, got nil")
	}
	if strings.Contains(err.Error(), strings.Repeat("x", maxEchoedStrayArg+1)) {
		t.Errorf("stray arg should be truncated to %d chars, got full-length echo: %v", maxEchoedStrayArg, err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Errorf("truncated arg should carry an ellipsis marker, got: %v", err)
	}
	// A zero-arg call is a valid invocation and must pass.
	if err := spawnNoPositionalArgs(nil, nil); err != nil {
		t.Errorf("no positional args should be valid, got: %v", err)
	}
}

// TestRunSpawn_ConfirmBypassesGuard verifies --confirm is the escape hatch:
// with it set, a bare spawn proceeds to the daemon and creates the agent bead.
func TestRunSpawn_ConfirmBypassesGuard(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_PROJECT", "")

	var sawCreate bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			sawCreate = true
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-CONFIRM01"})
		default:
			// label / dependency best-effort calls
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	_, _, runErr := newSpawnTestCmd(t, []string{"--confirm"})
	if runErr != nil {
		t.Fatalf("--confirm should spawn without error, got: %v", runErr)
	}
	if !sawCreate {
		t.Error("--confirm should have reached the daemon create POST")
	}
}

// TestRunSpawn_IntentFlagsBypassGuard confirms each intent flag independently
// satisfies the guard — a flagged invocation is never blocked (gardeners depend
// on `gb spawn --task/--prompt/--role` staying non-interactive). Each subtest
// asserts the create POST fires.
func TestRunSpawn_IntentFlagsBypassGuard(t *testing.T) {
	// A task-ref subtest needs a project-labeled task bead to resolve; the others
	// take project from the env.
	taskID := "gd-INTENT01"
	cases := []struct {
		name string
		argv []string
	}{
		{"prompt flag", []string{"--prompt", "Review PR #42"}},
		{"role flag (even default value)", []string{"--role", "crew"}},
		{"role flag non-default", []string{"--role", "gardener"}},
		{"task flag", []string{"--task", taskID}},
		{"slack-thread-channel flag", []string{"--slack-thread-channel", "C123"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOAT_PROJECT", "gardenio")
			t.Setenv("KD_PROJECT", "")

			var sawCreate bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/v1/beads/"+taskID && r.Method == http.MethodGet:
					// Task bead lookup (resolveTaskProject + slack inheritance).
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": taskID, "title": "intent task", "type": "task",
						"kind": "issue", "status": "open",
						"labels": []string{"project:gardenio"},
					})
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
					http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
					// Single-claim guard list query (only for --task path).
					_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
				case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
					sawCreate = true
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SPAWNED01"})
				default:
					_ = json.NewEncoder(w).Encode(map[string]string{})
				}
			}))
			t.Cleanup(srv.Close)
			c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			oldDaemon := daemon
			daemon = c
			t.Cleanup(func() { daemon = oldDaemon })

			if _, _, runErr := newSpawnTestCmd(t, tc.argv); runErr != nil {
				t.Fatalf("intent flag %v should spawn without error, got: %v", tc.argv, runErr)
			}
			if !sawCreate {
				t.Errorf("intent flag %v should have reached the daemon create POST", tc.argv)
			}
		})
	}
}

// TestRunSpawn_TaskLabelInferenceNoEnv is the end-to-end regression for
// gd-EB7cYH9fvH: `gb spawn --task <bead>` with NEITHER BOAT_PROJECT nor
// KD_PROJECT set must infer the project from the task bead's project: label and
// spawn successfully — it must NOT hard-error with "project is required".
//
// This closes a real coverage gap. TestResolveTaskProject_KdBeadLabel exercises
// resolveTaskProject in isolation, and TestRunSpawn_IntentFlagsBypassGuard's
// --task case runs the full command but with BOAT_PROJECT=gardenio in the env —
// so the env fallback would mask a broken label-inference path. Only this test
// drives the whole runSpawn flow with an empty env, proving the label is what
// resolves the project (and lands on the created agent bead's fields.project +
// project: label).
func TestRunSpawn_TaskLabelInferenceNoEnv(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("KD_PROJECT", "")

	taskID := "gd-NOENV001"
	var sawCreate bool
	var createdProject string
	var sawProjectLabel bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/beads/"+taskID && r.Method == http.MethodGet:
			// Task bead carries ONLY a project: label — no env project available.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": taskID, "title": "Configure GLM reasoning", "type": "task",
				"kind": "issue", "status": "open",
				"labels": []string{"project:glm-lab"},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			sawCreate = true
			var body struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var fields map[string]string
			_ = json.Unmarshal(body.Fields, &fields)
			createdProject = fields["project"]
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-LNV4YGXEUZ"})
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == http.MethodPost:
			var body struct {
				Label string `json:"label"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Label == "project:glm-lab" {
				sawProjectLabel = true
			}
			_ = json.NewEncoder(w).Encode(map[string]string{})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	_, out, runErr := newSpawnTestCmd(t, []string{"--task", taskID, "--role", "glm-lab"})
	if runErr != nil {
		t.Fatalf("spawn --task with project-labeled bead and empty env must not error, got: %v\nout: %s", runErr, out.String())
	}
	if !sawCreate {
		t.Fatal("spawn should have reached the daemon create POST")
	}
	if createdProject != "glm-lab" {
		t.Errorf("agent bead fields.project = %q, want %q (inferred from task label, not env)", createdProject, "glm-lab")
	}
	if !sawProjectLabel {
		t.Error("agent bead should have been labeled project:glm-lab")
	}
}

// TestSpawnHasIntent unit-tests the pure guard predicate across the flag set,
// isolating it from the daemon path.
func TestSpawnHasIntent(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"no flags", nil, false},
		{"confirm only is not intent", []string{"--confirm"}, false},
		{"task", []string{"--task", "gd-x"}, true},
		{"prompt", []string{"--prompt", "hi"}, true},
		{"role even at default", []string{"--role", "crew"}, true},
		{"slack channel", []string{"--slack-thread-channel", "C1"}, true},
		{"slack ts", []string{"--slack-thread-ts", "1.2"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "spawn", RunE: func(*cobra.Command, []string) error { return nil }}
			registerSpawnFlags(cmd)
			cmd.SetArgs(tc.argv)
			if err := cmd.ParseFlags(tc.argv); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			if got := spawnHasIntent(cmd); got != tc.want {
				t.Errorf("spawnHasIntent(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// captureSpawnStdout runs fn with os.Stdout redirected to a pipe and returns
// what it printed. The spawn success line goes to os.Stdout via fmt.Printf (not
// the cobra out buffer), so asserting on it requires a real stdout capture.
func captureSpawnStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// incumbentAgentName is the name of the live agent the single-claim stub
// reports as already holding the task — deliberately different from the name
// the spawn under test generates.
const incumbentAgentName = "gardenio-incumbent-crew-aaa"

// spawnOutcomeDaemon installs a stub daemon for the spawn success-signal tests.
// existingByName / existingByTask drive the two idempotence guards: non-empty
// values make the corresponding guard return an incumbent instead of creating.
func spawnOutcomeDaemon(t *testing.T, taskID, existingByName, existingByTask string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/beads/"+taskID && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": taskID, "title": "outcome task", "type": "task",
				"kind": "issue", "status": "open",
				"labels": []string{"project:gardenio"},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			// Name-dedup guard.
			if existingByName == "" {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": existingByName, "title": "incumbent", "type": "agent",
				"kind": "config", "status": "in_progress",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// Single-claim guard list query.
			if existingByTask == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{
				map[string]any{
					"id": existingByTask, "title": "live-crew", "type": "agent",
					"kind": "config", "status": "in_progress",
					"fields": map[string]string{
						"task_id": taskID, "agent_state": "running",
						"agent": incumbentAgentName,
					},
				},
			}})
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-FRESH001"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

// TestRunSpawn_PrintsMachineGreppableSuccessLine is the core regression for bug
// ka-0000001243: a successful `gb spawn` must print a stable success token as
// the FIRST line so a scripted gardener can confirm the dispatch with one grep.
// Before this, the only success output was the present-participle prose
// "Spawning agent <name>", so a gardener grepping for a success token matched
// NOTHING on a successful spawn, concluded it had failed, and re-dispatched.
func TestRunSpawn_PrintsMachineGreppableSuccessLine(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_PROJECT", "")
	taskID := "gd-OUTCOME1"
	spawnOutcomeDaemon(t, taskID, "", "")

	var runErr error
	out := captureSpawnStdout(t, func() {
		_, _, runErr = newSpawnTestCmd(t, []string{"--task", taskID, "--role", "crew"})
	})
	if runErr != nil {
		t.Fatalf("spawn should succeed, got: %v", runErr)
	}

	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(first, "SPAWNED ") {
		t.Errorf("first stdout line must be the SPAWNED success token, got: %q", first)
	}
	for _, want := range []string{"bead=gd-FRESH001", "status=created", "task=" + taskID} {
		if !strings.Contains(first, want) {
			t.Errorf("success line missing %q, got: %q", want, first)
		}
	}
	// A fresh spawn must NOT be labelled as a deduped no-op.
	if strings.Contains(first, "status=existing") {
		t.Errorf("fresh spawn must report status=created, got: %q", first)
	}
}

// TestRunSpawn_SuccessLineDistinguishesDedupedNoOp pins the half of the contract
// that makes the token trustworthy: when an idempotence guard returns a LIVE
// incumbent instead of creating an agent, the success line must say so
// (status=existing + which guard fired). A caller that cannot tell a real
// dispatch from a no-op will over-report work it never dispatched.
func TestRunSpawn_SuccessLineDistinguishesDedupedNoOp(t *testing.T) {
	cases := []struct {
		name           string
		existingByName string
		existingByTask string
		wantBead       string
		wantReason     string
		wantAgent      string
	}{
		// name_dedup matches BY name, so the incumbent's name IS the requested one.
		{"name dedup", "gd-INCUMB01", "", "gd-INCUMB01", "reason=name_dedup", ""},
		// single_claim matches by task_id, so the incumbent has a DIFFERENT name.
		{"single claim", "", "gd-LIVECRW1", "gd-LIVECRW1", "reason=single_claim", incumbentAgentName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOAT_PROJECT", "gardenio")
			t.Setenv("KD_PROJECT", "")
			taskID := "gd-OUTCOME2"
			spawnOutcomeDaemon(t, taskID, tc.existingByName, tc.existingByTask)

			var runErr error
			out := captureSpawnStdout(t, func() {
				_, _, runErr = newSpawnTestCmd(t, []string{"--task", taskID, "--role", "crew"})
			})
			if runErr != nil {
				t.Fatalf("spawn should succeed, got: %v", runErr)
			}

			first := strings.SplitN(out, "\n", 2)[0]
			if !strings.HasPrefix(first, "SPAWNED ") {
				t.Fatalf("expected a SPAWNED line even for a deduped spawn, got: %q", first)
			}
			if !strings.Contains(first, "status=existing") {
				t.Errorf("deduped spawn must report status=existing, got: %q", first)
			}
			if !strings.Contains(first, "bead="+tc.wantBead) {
				t.Errorf("deduped spawn must return the incumbent bead %s, got: %q", tc.wantBead, first)
			}
			if !strings.Contains(first, tc.wantReason) {
				t.Errorf("deduped spawn must name the guard (%s), got: %q", tc.wantReason, first)
			}
			// The reported agent name must be the INCUMBENT's, never the
			// freshly-generated name for the agent that was not created. The
			// single-claim guard matches on task_id, so those differ — reporting
			// the requested name would name an agent that does not exist.
			if tc.wantAgent != "" && !strings.Contains(first, "agent="+tc.wantAgent) {
				t.Errorf("deduped spawn must report the incumbent agent %q, got: %q", tc.wantAgent, first)
			}
			// The prose must not claim a pod is coming — none is.
			if strings.Contains(out, "The reconciler will schedule a pod shortly") {
				t.Errorf("deduped spawn must not promise a new pod, got: %q", out)
			}
		})
	}
}

// TestRunSpawn_JSONCarriesStatus pins the scripting path: `gb spawn --json`
// must always carry a "status" key so `--jq '.status'` never yields null on
// success, and must name the guard on a deduped spawn.
func TestRunSpawn_JSONCarriesStatus(t *testing.T) {
	cases := []struct {
		name           string
		existingByTask string
		wantStatus     string
		wantReason     string
	}{
		{"created", "", "created", ""},
		{"existing", "gd-LIVECRW2", "existing", "single_claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOAT_PROJECT", "gardenio")
			t.Setenv("KD_PROJECT", "")
			taskID := "gd-OUTCOME3"
			spawnOutcomeDaemon(t, taskID, "", tc.existingByTask)

			oldJSON := jsonOutput
			jsonOutput = true
			t.Cleanup(func() { jsonOutput = oldJSON })

			var runErr error
			out := captureSpawnStdout(t, func() {
				_, _, runErr = newSpawnTestCmd(t, []string{"--task", taskID, "--role", "crew"})
			})
			if runErr != nil {
				t.Fatalf("spawn should succeed, got: %v", runErr)
			}

			var got map[string]string
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("--json output must be parseable, got %q: %v", out, err)
			}
			if got["status"] != tc.wantStatus {
				t.Errorf("status = %q, want %q (full: %v)", got["status"], tc.wantStatus, got)
			}
			if got["reason"] != tc.wantReason {
				t.Errorf("reason = %q, want %q (full: %v)", got["reason"], tc.wantReason, got)
			}
			if got["name"] == "" || got["id"] == "" {
				t.Errorf("--json must carry name and id, got: %v", got)
			}
			// The canonical gardener dispatch recipe does `gb spawn --json | jq -r
			// '.name'` and stamps the result. On a single-claim hit that name must
			// be the live incumbent, not the never-created generated name.
			if tc.wantStatus == "existing" && got["name"] != incumbentAgentName {
				t.Errorf("--json name on a deduped spawn must be the incumbent %q, got %q",
					incumbentAgentName, got["name"])
			}
		})
	}
}

// A gardener's crew is spawned with the human operator it inherited; that
// human owns the crew. Sentinels own nothing.
func TestSpawnOwnerFields(t *testing.T) {
	cases := map[string]string{
		"oidc:matt@example.com": "oidc:matt@example.com",
		"system:gardener":       "",
		"fleet:slack-bridge":    "",
		"":                      "",
	}
	for op, want := range cases {
		got := spawnOwnerFields(op)["owner"]
		if got != want {
			t.Errorf("spawnOwnerFields(%q) owner = %q, want %q", op, got, want)
		}
	}
}
