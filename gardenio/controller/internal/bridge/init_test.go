package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

type mockConfigSetter struct {
	beads []*beadsapi.BeadDetail
}

func (m *mockConfigSetter) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	id := "gd-mock-" + req.Title
	m.beads = append(m.beads, &beadsapi.BeadDetail{
		ID:          id,
		Title:       req.Title,
		Type:        req.Type,
		Description: req.Description,
		Labels:      req.Labels,
		Status:      "open",
	})
	return id, nil
}

func (m *mockConfigSetter) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

func (m *mockConfigSetter) UpdateBeadDescription(_ context.Context, beadID, description string) error {
	for _, b := range m.beads {
		if b.ID == beadID {
			b.Description = description
			break
		}
	}
	return nil
}

// findBead returns the first config bead matching the given title and labels.
func findBead(beads []*beadsapi.BeadDetail, title string, labels []string) *beadsapi.BeadDetail {
	for _, b := range beads {
		if b.Title == title && labelsMatch(b.Labels, labels) {
			return b
		}
	}
	return nil
}

// findConfigBeadDescription finds a config bead by title and labels and returns
// its description. Calls t.Fatal if not found.
func findConfigBeadDescription(t *testing.T, beads []*beadsapi.BeadDetail, title string, labels []string) string {
	t.Helper()
	for _, b := range beads {
		if b.Title == title && labelsMatch(b.Labels, labels) {
			return b.Description
		}
	}
	t.Fatalf("expected config bead with title=%q labels=%v", title, labels)
	return ""
}

func TestEnsureConfigs_SeedsNudgePrompts(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	// config:* entries are written as config beads, not to KV.
	raw := findConfigBeadDescription(t, setter.beads, "nudge-prompts", []string{"global"})

	var prompts map[string]string
	if err := json.Unmarshal([]byte(raw), &prompts); err != nil {
		t.Fatalf("unmarshal nudge-prompts: %v", err)
	}

	for _, key := range []string{"thread", "adhoc", "default", "prewarmed"} {
		if prompts[key] == "" {
			t.Errorf("nudge-prompts missing key %q", key)
		}
	}

	// Adhoc prompt should use template variable, not hardcoded "Create a bead".
	if val := prompts["adhoc"]; val != "" {
		if strings.Contains(val, "Create a bead") {
			t.Error("adhoc prompt should not say 'Create a bead' — should use {{.TaskHint}}")
		}
		if !strings.Contains(val, "{{.TaskHint}}") {
			t.Error("adhoc prompt should contain {{.TaskHint}} template variable")
		}
		if !strings.Contains(val, "{{.BoatPrompt}}") {
			t.Error("adhoc prompt should contain {{.BoatPrompt}} template variable")
		}
	}
}

func TestEnsureConfigs_SeedsClaudeInstructions(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	// config:* entries are written as config beads, not to KV.
	raw := findConfigBeadDescription(t, setter.beads, "claude-instructions", []string{"global"})

	var sections map[string]string
	if err := json.Unmarshal([]byte(raw), &sections); err != nil {
		t.Fatalf("unmarshal claude-instructions: %v", err)
	}

	expectedKeys := []string{
		"prime_header", "session_close", "core_rules", "commands",
		"workflows", "decisions", "session_resumption", "lifecycle",
		"stop_gate", "stop_gate_blocked",
	}
	for _, key := range expectedKeys {
		if sections[key] == "" {
			t.Errorf("claude-instructions missing key %q", key)
		}
	}

	// stop_gate_blocked should contain system-reminder tags.
	if val := sections["stop_gate_blocked"]; !strings.Contains(val, "<system-reminder>") {
		t.Error("stop_gate_blocked should contain <system-reminder> tags")
	}
}

// TestEnsureConfigs_DecisionsSectionHasEscalationAffordance guards the
// `gb escalation create` affordance in the global claude-instructions
// `decisions` section (gd-oG5b5YqU2a, fixing finding gd-eljbyBRmJe). The
// auto-worker-haiku role has no override, so it inherits this global section —
// the affordance steers Haiku workers toward `gb escalation create` instead of
// filing an inert `type=task` titled "escalation: ..." that never reaches the
// operator queue. References advice ka-0000001089.
func TestEnsureConfigs_DecisionsSectionHasEscalationAffordance(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	raw := findConfigBeadDescription(t, setter.beads, "claude-instructions", []string{"global"})
	var sections map[string]string
	if err := json.Unmarshal([]byte(raw), &sections); err != nil {
		t.Fatalf("unmarshal claude-instructions: %v", err)
	}

	decisions := sections["decisions"]
	if decisions == "" {
		t.Fatal("claude-instructions missing decisions section")
	}
	if !strings.Contains(decisions, "gb escalation create") {
		t.Error("decisions section should steer agents to `gb escalation create` for privileged actions they cannot perform")
	}
	// The affordance must warn against the inert bare-task anti-pattern.
	if !strings.Contains(decisions, "type=task") {
		t.Error("decisions section should warn that a bare type=task escalation sits inert")
	}
}

func TestEnsureConfigs_SeedsRoleOverrides(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	// config:* entries are written as config beads, not to KV.
	for _, role := range []string{"thread", "polecat", "gardener"} {
		raw := findConfigBeadDescription(t, setter.beads, "claude-instructions", []string{"role:" + role})

		var sections map[string]string
		if err := json.Unmarshal([]byte(raw), &sections); err != nil {
			t.Errorf("unmarshal role:%s: %v", role, err)
			continue
		}

		if sections["commands"] == "" {
			t.Errorf("role:%s missing commands section", role)
		}
		if sections["lifecycle"] == "" {
			t.Errorf("role:%s missing lifecycle section", role)
		}
	}

	// Thread lifecycle should instruct respond-then-stop, not stay-alive.
	raw := findConfigBeadDescription(t, setter.beads, "claude-instructions", []string{"role:thread"})
	var threadSections map[string]string
	_ = json.Unmarshal([]byte(raw), &threadSections)
	if !strings.Contains(threadSections["lifecycle"], "gb stop") {
		t.Error("thread lifecycle should instruct calling gb stop after responding")
	}
	if strings.Contains(threadSections["lifecycle"], "stay alive") {
		t.Error("thread lifecycle should NOT tell agents to stay alive (stop-by-default)")
	}

	// Polecat lifecycle should mention single-task.
	raw = findConfigBeadDescription(t, setter.beads, "claude-instructions", []string{"role:polecat"})
	var polecatSections map[string]string
	_ = json.Unmarshal([]byte(raw), &polecatSections)
	if !strings.Contains(polecatSections["lifecycle"], "single-task") {
		t.Error("polecat lifecycle should mention single-task")
	}

	// Gardener lifecycle should mention orchestrator.
	raw = findConfigBeadDescription(t, setter.beads, "claude-instructions", []string{"role:gardener"})
	var gardenerSections map[string]string
	_ = json.Unmarshal([]byte(raw), &gardenerSections)
	if !strings.Contains(gardenerSections["lifecycle"], "orchestrator") {
		t.Error("gardener lifecycle should mention orchestrator")
	}

	// Crew workflows should surface discovery commands (ka-0000006883: crew agent
	// executed in isolation without searching beads — root cause was empty workflows).
	raw = findConfigBeadDescription(t, setter.beads, "claude-instructions", []string{"role:crew"})
	var crewSections map[string]string
	_ = json.Unmarshal([]byte(raw), &crewSections)
	if !strings.Contains(crewSections["workflows"], "kd search") {
		t.Error("crew workflows should surface kd search for discovery")
	}
	if !strings.Contains(crewSections["workflows"], "gb advice show") {
		t.Error("crew workflows should surface gb advice show for discovery")
	}
}

func TestEnsureConfigs_SeedsAccountType(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	b := findBead(setter.beads, "type:account", []string{"global"})
	if b == nil {
		t.Fatal("expected config bead type:account")
	}

	var tc TypeConfig
	if err := json.Unmarshal([]byte(b.Description), &tc); err != nil {
		t.Fatalf("unmarshal type:account: %v", err)
	}

	if tc.Kind != "config" {
		t.Errorf("kind: got %q, want %q", tc.Kind, "config")
	}

	wantFields := map[string]bool{
		"provider":            true,
		"base_url":            true,
		"credential":          true,
		"model":               true,
		"probe_model":         true,
		"max_budget":          true,
		"fallback":            true,
		"litellm_key":         true,
		"total_input_tokens":  true,
		"total_output_tokens": true,
		"total_cost_usd":      true,
		"session_count":       true,
		"spend_at_last_reset": true,
		"last_reset_at":       true,
		// Subscription-window bills (ka-0000003343): spend_sync writes these in the
		// SAME atomic PATCH as total/daily/weekly cost, so an unregistered field
		// 400s the whole batch and freezes the /costs page — the ka-0000007493
		// trap. Both schema copies (here + kbeads config.go) MUST carry them.
		"spend_5h_window_usd": true,
		"spend_7d_window_usd": true,
		// The tenant land purse stamp (Lands Track B §3.4), written by
		// spend_sync on lands-<land> accounts; kbeads config.go carries it too.
		"spend_synced_at":    true,
		"live_cost_baseline": true,
		// The purse watcher's state on a land's account (Track B step 8.4).
		"purse_notice_level": true,
		"purse_granted_seen": true,
		// The steward's park (step 9.7), beside the watcher's state.
		"steward_park_reason": true,
		// The freeze's key block (gd-nHh7M0k3ps), beside the watcher's state.
		"land_key_blocked": true,
		// billing_type gates the out_of_credits recovery-hold (ka-0000003350);
		// its absence from this authoritative schema 400s any operator write of
		// billing_type=metered and re-strands subscription primaries — the exact
		// divergence this guard exists to catch (config.go builtinConfigs must
		// carry it too; see builtin_types_test.go).
		"billing_type": true,
	}
	gotFields := make(map[string]bool, len(tc.Fields))
	for _, f := range tc.Fields {
		gotFields[f.Name] = true
	}
	for name := range wantFields {
		if !gotFields[name] {
			t.Errorf("missing field %q in type:account", name)
		}
	}

	// provider and credential must be required.
	for _, f := range tc.Fields {
		switch f.Name {
		case "provider", "credential":
			if !f.Required {
				t.Errorf("field %q should be required", f.Name)
			}
		}
	}
}

// TestEnsureConfigs_SeedsUserProfilePrefs pins a json-typed profile_prefs field
// on the type:user config. The beads-viewer Pin/Unpin button writes inbox-pin
// state into profile_prefs via UpdateBeadFieldsTyped on the type=user
// kind=config bead; without this field the daemon rejects the PATCH with
// HTTP 400 "profile_prefs: unknown field" and the viewer returns a 502
// "Failed to toggle pin." (bug gd-NlwSPbsrUB). This must stay in sync with the
// kbeads builtin in kbeads/internal/server/config.go.
func TestEnsureConfigs_SeedsUserProfilePrefs(t *testing.T) {
	cfg, ok := configs()["type:user"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:user entry")
	}

	var fd *FieldDef
	for i := range cfg.Fields {
		if cfg.Fields[i].Name == "profile_prefs" {
			fd = &cfg.Fields[i]
			break
		}
	}
	if fd == nil {
		t.Fatal("type:user missing field profile_prefs")
	}
	if fd.Type != "json" {
		t.Errorf("type:user profile_prefs type=%q, want json", fd.Type)
	}
}

// TestEnsureConfigs_SeedsAdviceType pins the type:advice FieldDef list so that
// any aux-field written by `gb advice` / `kd advice` stays declared. If a new
// field is added at a write site (advice.go, advice_add.go, advice_confirm.go,
// advice_flag.go) without also being added here, this test fails. Keep in sync
// with kbeads/internal/server/config.go type:advice.
func TestEnsureConfigs_SeedsAdviceType(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	b := findBead(setter.beads, "type:advice", []string{"global"})
	if b == nil {
		t.Fatal("expected config bead type:advice")
	}

	var tc TypeConfig
	if err := json.Unmarshal([]byte(b.Description), &tc); err != nil {
		t.Fatalf("unmarshal type:advice: %v", err)
	}

	if tc.Kind != "data" {
		t.Errorf("kind: got %q, want %q", tc.Kind, "data")
	}

	wantFields := []string{
		// Hook execution (written by gb advice / kd advice add).
		"hook_command",
		"hook_trigger",
		"hook_timeout",
		"hook_on_failure",
		// Subscription targeting (written by kd advice add).
		"subscriptions",
		"subscriptions_exclude",
		// Legacy confidence scoring — no writers since the confidence/survey
		// subsystem was removed (ka-0000001115); kept for existing bead data.
		"confidence",
		"confirmations",
		"last_confirmed",
		// Provenance / memory promotion metadata.
		"source",
		"author_agent",
		"content_hash",
		"memory_category",
		"tags",
		"supersedes",
		// Expiration (written by gb advice add --ttl / --expires-at).
		"expires_at",
	}
	got := make(map[string]bool, len(tc.Fields))
	for _, f := range tc.Fields {
		got[f.Name] = true
	}
	for _, name := range wantFields {
		if !got[name] {
			t.Errorf("missing field %q in type:advice", name)
		}
	}
}

func TestEnsureConfigs_SeedsAllExpectedTypes(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	// type:* and view:* entries become global config beads with the full key as title.
	// context:* entries become config beads with title="context" and role labels.
	expected := []struct {
		title  string
		labels []string
	}{
		{"type:agent", []string{"global"}},
		{"type:account", []string{"global"}},
		{"type:project", []string{"global"}},
		{"type:task", []string{"global"}},
		{"view:agents:active", []string{"global"}},
		{"view:decisions:pending", []string{"global"}},
		{"context", []string{"role:crew"}},
	}

	for _, e := range expected {
		if findBead(setter.beads, e.title, e.labels) == nil {
			t.Errorf("expected config bead with title=%q labels=%v", e.title, e.labels)
		}
	}
}

func TestEnsureConfigs_CreatesConfigBeads(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	// Should have created config beads for all entries.
	expectedBeads := []struct {
		title  string
		labels []string
	}{
		{"nudge-prompts", []string{"global"}},
		{"claude-instructions", []string{"global"}},
		{"claude-instructions", []string{"role:thread"}},
		{"claude-instructions", []string{"role:polecat"}},
		{"type:agent", []string{"global"}},
		{"type:task", []string{"global"}},
		{"view:agents:active", []string{"global"}},
		{"context", []string{"role:gardener"}},
	}

	for _, e := range expectedBeads {
		b := findBead(setter.beads, e.title, e.labels)
		if b == nil {
			t.Errorf("expected config bead with title=%q labels=%v", e.title, e.labels)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(b.Description), &m); err != nil {
			t.Errorf("config bead %s %v has invalid JSON description: %v", e.title, e.labels, err)
		}
	}
}

func TestEnsureConfigs_UpdatesExistingConfigBead(t *testing.T) {
	setter := &mockConfigSetter{}

	// Pre-seed a config bead that EnsureConfigs should update.
	setter.beads = append(setter.beads, &beadsapi.BeadDetail{
		ID:          "gd-existing",
		Title:       "nudge-prompts",
		Type:        "config",
		Description: `{"old":"value"}`,
		Labels:      []string{"global"},
		Status:      "open",
	})

	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	// Should have updated the existing bead, not created a new one.
	nudgeCount := 0
	for _, b := range setter.beads {
		if b.Title == "nudge-prompts" && labelsMatch(b.Labels, []string{"global"}) {
			nudgeCount++
		}
	}
	if nudgeCount != 1 {
		t.Errorf("expected 1 nudge-prompts bead (upsert), got %d", nudgeCount)
	}

	// The description should be updated to the current value.
	for _, b := range setter.beads {
		if b.ID == "gd-existing" {
			var prompts map[string]string
			if err := json.Unmarshal([]byte(b.Description), &prompts); err != nil {
				t.Fatalf("unmarshal updated description: %v", err)
			}
			if prompts["thread"] == "" {
				t.Error("expected thread prompt in updated description")
			}
			break
		}
	}
}

// TestEnsureConfigs_SeedsNeedsK8sAPI pins the `needs_k8s_api` boolean field on
// both type:project and type:role. The field is the opt-in for mounting a
// projected k8s ServiceAccount token into agent pods (AUDIT.md §C3,
// ka-0000006344). Keep in sync with kbeads/internal/server/config.go.
func TestEnsureConfigs_SeedsNeedsK8sAPI(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	for _, typeKey := range []string{"type:project", "type:role"} {
		b := findBead(setter.beads, typeKey, []string{"global"})
		if b == nil {
			t.Errorf("expected config bead %s", typeKey)
			continue
		}
		var tc TypeConfig
		if err := json.Unmarshal([]byte(b.Description), &tc); err != nil {
			t.Errorf("unmarshal %s: %v", typeKey, err)
			continue
		}
		var found bool
		for _, f := range tc.Fields {
			if f.Name == "needs_k8s_api" {
				found = true
				if f.Type != "boolean" {
					t.Errorf("%s needs_k8s_api type=%q, want boolean", typeKey, f.Type)
				}
				break
			}
		}
		if !found {
			t.Errorf("%s missing field needs_k8s_api", typeKey)
		}
	}
}

func TestEnsureConfigs_SeedsFleetType(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	b := findBead(setter.beads, "type:fleet", []string{"global"})
	if b == nil {
		t.Fatal("expected type:fleet config bead")
	}

	var tc TypeConfig
	if err := json.Unmarshal([]byte(b.Description), &tc); err != nil {
		t.Fatalf("unmarshal type:fleet: %v", err)
	}
	if tc.Kind != "config" {
		t.Errorf("type:fleet kind = %q, want config", tc.Kind)
	}

	wantFields := []string{"namespace", "description", "token_secret", "created_by_fleet", "parent_fleet", "max_agents", "status"}
	gotFields := make(map[string]bool, len(tc.Fields))
	for _, f := range tc.Fields {
		gotFields[f.Name] = true
	}
	for _, name := range wantFields {
		if !gotFields[name] {
			t.Errorf("type:fleet missing field %q", name)
		}
	}

	// namespace should be required.
	for _, f := range tc.Fields {
		if f.Name == "namespace" && !f.Required {
			t.Error("type:fleet field 'namespace' should be required")
		}
	}

	// status should be an enum.
	for _, f := range tc.Fields {
		if f.Name == "status" {
			if f.Type != "enum" {
				t.Errorf("type:fleet field 'status' type = %q, want enum", f.Type)
			}
			if len(f.Values) != 2 {
				t.Errorf("type:fleet field 'status' values = %v, want [active archived]", f.Values)
			}
		}
	}
}

func TestEnsureConfigs_SeedsFleetView(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	b := findBead(setter.beads, "view:fleets:active", []string{"global"})
	if b == nil {
		t.Fatal("expected view:fleets:active config bead")
	}

	var vc ViewConfig
	if err := json.Unmarshal([]byte(b.Description), &vc); err != nil {
		t.Fatalf("unmarshal view:fleets:active: %v", err)
	}
	if len(vc.Filter.Type) != 1 || vc.Filter.Type[0] != "fleet" {
		t.Errorf("view:fleets:active filter type = %v, want [fleet]", vc.Filter.Type)
	}
}

func TestParseEntryKey(t *testing.T) {
	tests := []struct {
		key        string
		wantTitle  string
		wantLabels []string
	}{
		// config:* keys.
		{"config:nudge-prompts:global", "nudge-prompts", []string{"global"}},
		{"config:claude-instructions:global", "claude-instructions", []string{"global"}},
		{"config:claude-instructions:role:thread", "claude-instructions", []string{"role:thread"}},
		{"config:claude-instructions:role:polecat", "claude-instructions", []string{"role:polecat"}},
		{"config:claude-instructions:role:local", "claude-instructions", []string{"role:local"}},
		{"config:nudge-prompts:role:local", "nudge-prompts", []string{"role:local"}},
		// Role-keyed namespaces.
		{"context:gardener", "context", []string{"role:gardener"}},
		{"context:crew", "context", []string{"role:crew"}},
		{"context:global", "context", []string{"global"}},
		// Definition namespaces (full key = title, always global).
		{"type:agent", "type:agent", []string{"global"}},
		{"type:task", "type:task", []string{"global"}},
		{"view:agents:active", "view:agents:active", []string{"global"}},
		{"view:ready", "view:ready", []string{"global"}},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			title, labels := parseEntryKey(tt.key)
			if title != tt.wantTitle {
				t.Errorf("title: got %q, want %q", title, tt.wantTitle)
			}
			if !labelsMatch(labels, tt.wantLabels) {
				t.Errorf("labels: got %v, want %v", labels, tt.wantLabels)
			}
		})
	}
}

// TestAgentSchema_HasScheduleLoopContextFields asserts that every schedule_*
// field scheduler.spawnScheduledAgent stamps onto agent beads is registered in
// the type:agent schema. The kbeads daemon rejects unknown fields with
// HTTP 400, which manifests as a stuck "Run failed to start" loop on the
// schedule bead and eventually auto-disables it after 3 failures.
// Regression for: schedule beads auto-disabled when commit 89ca538e added
// run-budget fields to the scheduler without extending the agent schema.
func TestAgentSchema_HasScheduleLoopContextFields(t *testing.T) {
	cfg, ok := configs()["type:agent"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:agent entry")
	}
	registered := map[string]bool{}
	for _, f := range cfg.Fields {
		registered[f.Name] = true
	}
	// Keep this list in sync with scheduler.spawnScheduledAgent in
	// gardenio/controller/internal/scheduler/scheduler.go.
	for _, name := range []string{
		"schedule_id",
		"schedule_title",
		"schedule_cron",
		"schedule_slack_channel",
		"schedule_name",
		"schedule_max_runs",
		"schedule_runs_spawned",
		"schedule_runs_completed",
		"schedule_terminal_goal",
		"schedule_prior_agent_id",
	} {
		if !registered[name] {
			t.Errorf("type:agent schema missing field %q (stamped by scheduler.spawnScheduledAgent)", name)
		}
	}
}

// TestTaskSchema_HasGitLabMentionFields is a regression guard for gd-bi3lO4oBIh.
// gitlab-bridge stamps these fields onto the task bead it creates when an
// @gardenio MR mention spawns an agent. If type:task drops them, kbeads rejects
// CreateBead with HTTP 400 and the mention silently fails to spawn.
func TestTaskSchema_HasGitLabMentionFields(t *testing.T) {
	cfg, ok := configs()["type:task"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:task entry")
	}
	registered := map[string]bool{}
	for _, f := range cfg.Fields {
		registered[f.Name] = true
	}
	// Keep this list in sync with the fields stamped by
	// MentionHandler.spawnMentionAgent in gitlab_mentions.go.
	for _, name := range []string{
		"mention_author",
		"spawn_source",
		"gitlab_discussion_id",
		"gitlab_mr_source_branch",
		// Track A3 (plan ka-0000004007): nudge-delivery stamp fields.
		"nudged_agent",
		"nudge_delivered_at",
	} {
		if !registered[name] {
			t.Errorf("type:task schema missing field %q (stamped by MentionHandler.spawnMentionAgent)", name)
		}
	}
}

// TestTaskSchema_HasTriageClassificationField guards the fsm-triage
// bead→bridge→GBFSM path (ka-0000001301): the triage agent stamps
// recommended_affects_version on its own task bead and
// JiraSync.syncGBFSMTicket reads it back (via triageClassification)
// to write the version FIELD on the one upserted GBFSM ticket (1:1 reshape
// ka-0000001491). If the seeded type:task schema drops the field, the agent's
// `kd update` 400s "recommended_affects_version: unknown field" and the version
// never reaches GBFSM — the write half of the outbox silently breaks.
func TestTaskSchema_HasTriageClassificationField(t *testing.T) {
	cfg, ok := configs()["type:task"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:task entry")
	}
	for _, f := range cfg.Fields {
		if f.Name == "recommended_affects_version" {
			return
		}
	}
	t.Error("type:task schema missing field \"recommended_affects_version\" (fsm-triage agent stamps it; JiraSync.syncGBFSMTicket reads it)")
}

// TestDecisionSchema_HasEvidenceField: `gb decision create --evidence` stamps
// evidence on the decision bead (gd-4hDcGbtVGc). The seeded type:decision must
// declare it, matching the kbeads builtin, or the strict schema 400s the create
// "evidence: unknown field".
func TestDecisionSchema_HasEvidenceField(t *testing.T) {
	cfg, ok := configs()["type:decision"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:decision entry")
	}
	for _, f := range cfg.Fields {
		if f.Name == "evidence" {
			if f.Type != "string" {
				t.Errorf("type:decision evidence type = %q, want string", f.Type)
			}
			return
		}
	}
	t.Error("type:decision schema missing field \"evidence\" (gb decision create --evidence stamps it)")
}

func TestEnsureConfigs_SeedsDashboardType(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	b := findBead(setter.beads, "type:dashboard", []string{"global"})
	if b == nil {
		t.Fatal("expected config bead type:dashboard")
	}

	var tc TypeConfig
	if err := json.Unmarshal([]byte(b.Description), &tc); err != nil {
		t.Fatalf("unmarshal type:dashboard: %v", err)
	}
	if tc.Kind != "issue" {
		t.Errorf("kind: got %q, want %q", tc.Kind, "issue")
	}

	wantFields := []string{
		"dashboard_uid",
		"dashboard_folder",
		"dashboard_version",
		"dashboard_hash",
		"dashboard_source",
		"dashboard_configmap_key",
		"dashboard_conflict_critical",
	}
	got := make(map[string]bool, len(tc.Fields))
	for _, f := range tc.Fields {
		got[f.Name] = true
	}
	for _, name := range wantFields {
		if !got[name] {
			t.Errorf("missing field %q in type:dashboard", name)
		}
	}

	// dashboard_source is an enum over {beads, grafana}.
	for _, f := range tc.Fields {
		if f.Name == "dashboard_source" {
			if f.Type != "enum" {
				t.Errorf("dashboard_source type: got %q, want enum", f.Type)
			}
			if !labelsMatch(f.Values, []string{"beads", "grafana"}) {
				t.Errorf("dashboard_source values: got %v", f.Values)
			}
		}
	}
}

// TestTaskSchema_HasChannelIDField guards the Commons channel_id stamp on
// thread heads (gd-0moiIbZPom): getOrCreateThreadBead writes channel_id on the
// head create, and type:task rejects unknown fields, so dropping it here would
// 400 every head minted in a seeded land.
func TestTaskSchema_HasChannelIDField(t *testing.T) {
	cfg, ok := configs()["type:task"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:task entry")
	}
	for _, f := range cfg.Fields {
		if f.Name == "channel_id" {
			return
		}
	}
	t.Error("type:task schema missing field \"channel_id\" (stamped on thread heads by getOrCreateThreadBead)")
}
