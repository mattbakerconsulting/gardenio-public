package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// --- Mock implementations ---

type mockBeadLister struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (m *mockBeadLister) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

// --- Advice dump tests ---

func TestDumpAdvice_CollectsAndSorts(t *testing.T) {
	lister := &mockBeadLister{
		beads: []*beadsapi.BeadDetail{
			{Title: "Zebra rule", Description: "z desc", Labels: []string{"global"}, Priority: 1},
			{Title: "Alpha rule", Description: "a desc", Labels: []string{"role:gardener"}, Priority: 2},
		},
	}

	entries, err := dumpAdvice(context.Background(), lister)
	if err != nil {
		t.Fatalf("dumpAdvice: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2, got %d", len(entries))
	}
	if entries[0].Title != "Alpha rule" {
		t.Errorf("expected sorted, first=%s", entries[0].Title)
	}
	if entries[1].Title != "Zebra rule" {
		t.Errorf("expected sorted, second=%s", entries[1].Title)
	}
	if entries[0].Description != "a desc" {
		t.Errorf("wrong description: %s", entries[0].Description)
	}
}

func TestDumpAdvice_Empty(t *testing.T) {
	lister := &mockBeadLister{beads: []*beadsapi.BeadDetail{}}
	entries, err := dumpAdvice(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0, got %d", len(entries))
	}
}

func TestDumpAdvice_Error(t *testing.T) {
	lister := &mockBeadLister{err: fmt.Errorf("server down")}
	_, err := dumpAdvice(context.Background(), lister)
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- Config bead dump/load tests ---

func TestDumpConfigBeads_CollectsAndSorts(t *testing.T) {
	lister := &mockBeadLister{
		beads: []*beadsapi.BeadDetail{
			{Title: "claude-mcp", Labels: []string{"global"}, Description: `{"mcpServers":{}}`},
			{Title: "claude-settings", Labels: []string{"role:gardener"}, Description: `{"model":"opus"}`},
			{Title: "claude-settings", Labels: []string{"global"}, Description: `{"model":"sonnet"}`},
		},
	}

	entries, err := dumpConfigBeads(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3, got %d", len(entries))
	}
	// Sorted by title then labels.
	if entries[0].Title != "claude-mcp" {
		t.Errorf("expected first=claude-mcp, got %s", entries[0].Title)
	}
	if entries[1].Title != "claude-settings" || entries[1].Labels[0] != "global" {
		t.Errorf("expected second=claude-settings [global], got %s %v", entries[1].Title, entries[1].Labels)
	}
	if entries[2].Title != "claude-settings" || entries[2].Labels[0] != "role:gardener" {
		t.Errorf("expected third=claude-settings [role:gardener], got %s %v", entries[2].Title, entries[2].Labels)
	}
}

func TestDumpConfigBeads_Empty(t *testing.T) {
	lister := &mockBeadLister{beads: []*beadsapi.BeadDetail{}}
	entries, err := dumpConfigBeads(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0, got %d", len(entries))
	}
}

func TestDumpConfigBeads_EmptyDescription(t *testing.T) {
	// Beads with empty descriptions should produce valid JSON (null value)
	// instead of causing marshal failures.
	lister := &mockBeadLister{
		beads: []*beadsapi.BeadDetail{
			{Title: "empty-desc", Labels: []string{"global"}, Description: ""},
			{Title: "valid-desc", Labels: []string{"global"}, Description: `{"key":"value"}`},
		},
	}

	entries, err := dumpConfigBeads(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2, got %d", len(entries))
	}

	// Verify the dump can be marshaled to valid JSON
	dump := configDump{ConfigBeads: entries}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed (empty description should produce null): %v", err)
	}
	if !json.Valid(data) {
		t.Errorf("output is not valid JSON: %s", string(data))
	}

	// The empty-description entry should have null value
	if string(entries[0].Value) != "null" {
		t.Errorf("expected null for empty description, got %s", string(entries[0].Value))
	}
}

func TestDumpConfigBeads_InvalidJSON(t *testing.T) {
	// Beads with non-JSON descriptions should be skipped to prevent
	// json.MarshalIndent failures on invalid json.RawMessage values.
	lister := &mockBeadLister{
		beads: []*beadsapi.BeadDetail{
			{Title: "valid", Labels: []string{"global"}, Description: `{"model":"opus"}`},
			{Title: "bad-json", Labels: []string{"global"}, Description: "this-is-not-json!!!"},
			{Title: "also-valid", Labels: []string{"role:crew"}, Description: `{"thinking":true}`},
		},
	}

	entries, err := dumpConfigBeads(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 (invalid skipped), got %d", len(entries))
	}

	// Verify the dump can be marshaled without error
	dump := configDump{ConfigBeads: entries}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !json.Valid(data) {
		t.Errorf("output is not valid JSON: %s", string(data))
	}
}

type mockConfigBeadCreator struct {
	beads   []*beadsapi.BeadDetail
	created []beadsapi.CreateBeadRequest
	err     error
}

func (m *mockConfigBeadCreator) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

func (m *mockConfigBeadCreator) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	m.created = append(m.created, req)
	return fmt.Sprintf("gd-load-%d", len(m.created)), nil
}

func TestLoadConfigBeads_CreatesNew(t *testing.T) {
	client := &mockConfigBeadCreator{beads: []*beadsapi.BeadDetail{}}
	entries := []configBeadEntry{
		{Title: "claude-settings", Labels: []string{"global"}, Value: json.RawMessage(`{"model":"opus"}`)},
		{Title: "claude-mcp", Labels: []string{"global"}, Value: json.RawMessage(`{"mcpServers":{}}`)},
	}

	created, skipped, errors := loadConfigBeads(context.Background(), client, entries)
	if created != 2 || skipped != 0 || errors != 0 {
		t.Errorf("expected 2/0/0, got %d/%d/%d", created, skipped, errors)
	}
	if len(client.created) != 2 {
		t.Fatalf("expected 2 creates, got %d", len(client.created))
	}
	if client.created[0].Type != "config" {
		t.Errorf("wrong type: %s", client.created[0].Type)
	}
}

func TestLoadConfigBeads_SkipsDuplicates(t *testing.T) {
	client := &mockConfigBeadCreator{
		beads: []*beadsapi.BeadDetail{
			{Title: "claude-settings", Labels: []string{"global"}, Description: `{"model":"opus"}`},
		},
	}
	entries := []configBeadEntry{
		{Title: "claude-settings", Labels: []string{"global"}, Value: json.RawMessage(`{"model":"sonnet"}`)},
		{Title: "claude-mcp", Labels: []string{"global"}, Value: json.RawMessage(`{}`)},
	}

	created, skipped, errors := loadConfigBeads(context.Background(), client, entries)
	if created != 1 || skipped != 1 || errors != 0 {
		t.Errorf("expected 1/1/0, got %d/%d/%d", created, skipped, errors)
	}
}

func TestConfigDump_IncludesConfigBeads(t *testing.T) {
	dump := configDump{
		Configs: []configEntry{
			{Key: "claude-settings:global", Value: json.RawMessage(`{"model":"opus"}`)},
		},
		ConfigBeads: []configBeadEntry{
			{Title: "claude-settings", Labels: []string{"global"}, Value: json.RawMessage(`{"model":"opus"}`)},
		},
		Advice: []adviceEntry{
			{Title: "Rule", Description: "desc", Labels: []string{"global"}, Priority: 1},
		},
	}

	data, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}

	var loaded configDump
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	if len(loaded.Configs) != 1 {
		t.Errorf("expected 1 config, got %d", len(loaded.Configs))
	}
	if len(loaded.ConfigBeads) != 1 {
		t.Errorf("expected 1 config bead, got %d", len(loaded.ConfigBeads))
	}
	if len(loaded.Advice) != 1 {
		t.Errorf("expected 1 advice, got %d", len(loaded.Advice))
	}
}

func TestConfigDump_OmitsEmptyConfigBeads(t *testing.T) {
	dump := configDump{
		Configs: []configEntry{{Key: "a", Value: json.RawMessage(`{}`)}},
	}

	data, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}

	// config_beads should be omitted when nil.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["config_beads"]; ok {
		t.Error("expected config_beads to be omitted when empty")
	}
}

// --- KV key parsing tests ---

func TestParseKVKeyToLabels_Global(t *testing.T) {
	cat, labels := parseKVKeyToLabels("claude-settings:global")
	if cat != "claude-settings" {
		t.Errorf("expected category=claude-settings, got %s", cat)
	}
	if len(labels) != 1 || labels[0] != "global" {
		t.Errorf("expected [global], got %v", labels)
	}
}

func TestParseKVKeyToLabels_Role(t *testing.T) {
	cat, labels := parseKVKeyToLabels("claude-hooks:gardener")
	if cat != "claude-hooks" {
		t.Errorf("expected category=claude-hooks, got %s", cat)
	}
	if len(labels) != 1 || labels[0] != "role:gardener" {
		t.Errorf("expected [role:gardener], got %v", labels)
	}
}

func TestParseKVKeyToLabels_NoColon(t *testing.T) {
	cat, labels := parseKVKeyToLabels("orphan")
	if cat != "orphan" {
		t.Errorf("expected category=orphan, got %s", cat)
	}
	if len(labels) != 1 || labels[0] != "global" {
		t.Errorf("expected [global], got %v", labels)
	}
}

func TestParseKVKeyToLabels_TypeDefinition(t *testing.T) {
	title, labels := parseKVKeyToLabels("type:task")
	if title != "type:task" {
		t.Errorf("expected title=type:task, got %s", title)
	}
	if len(labels) != 1 || labels[0] != "global" {
		t.Errorf("expected [global] for type definitions, got %v", labels)
	}
}

func TestParseKVKeyToLabels_ViewDefinition(t *testing.T) {
	title, labels := parseKVKeyToLabels("view:agents:active")
	if title != "view:agents:active" {
		t.Errorf("expected title=view:agents:active, got %s", title)
	}
	if len(labels) != 1 || labels[0] != "global" {
		t.Errorf("expected [global] for view definitions, got %v", labels)
	}
}

func TestParseKVKeyToLabels_ContextRole(t *testing.T) {
	title, labels := parseKVKeyToLabels("context:gardener")
	if title != "context" {
		t.Errorf("expected title=context, got %s", title)
	}
	if len(labels) != 1 || labels[0] != "role:gardener" {
		t.Errorf("expected [role:gardener], got %v", labels)
	}
}

// --- Mock for migration ---

type mockMigrator struct {
	configs map[string][]beadsapi.ConfigEntry
	beads   []*beadsapi.BeadDetail
	created []beadsapi.CreateBeadRequest
	err     error
}

func (m *mockMigrator) ListConfigs(_ context.Context, namespace string) ([]beadsapi.ConfigEntry, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.configs[namespace], nil
}

func (m *mockMigrator) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

func (m *mockMigrator) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	m.created = append(m.created, req)
	return fmt.Sprintf("gd-migrate-%d", len(m.created)), nil
}

// --- Migration tests ---

func TestPlanMigration_BasicEntries(t *testing.T) {
	client := &mockMigrator{
		configs: map[string][]beadsapi.ConfigEntry{
			"claude-settings": {
				{Key: "claude-settings:global", Value: json.RawMessage(`{"model":"opus"}`)},
				{Key: "claude-settings:gardener", Value: json.RawMessage(`{"model":"sonnet"}`)},
			},
		},
		beads: []*beadsapi.BeadDetail{},
	}

	actions, err := planMigration(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}

	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}

	// Should be sorted: global before role:gardener.
	if actions[0].Category != "claude-settings" || actions[0].Labels[0] != "global" {
		t.Errorf("expected first action to be claude-settings [global], got %s %v", actions[0].Category, actions[0].Labels)
	}
	if actions[1].Labels[0] != "role:gardener" {
		t.Errorf("expected second action labels [role:gardener], got %v", actions[1].Labels)
	}
	if actions[0].Skipped || actions[1].Skipped {
		t.Error("expected no actions to be skipped")
	}
}

func TestPlanMigration_SkipsExisting(t *testing.T) {
	client := &mockMigrator{
		configs: map[string][]beadsapi.ConfigEntry{
			"claude-settings": {
				{Key: "claude-settings:global", Value: json.RawMessage(`{"model":"opus"}`)},
			},
		},
		beads: []*beadsapi.BeadDetail{
			{Title: "claude-settings", Labels: []string{"global"}, Description: `{"model":"opus"}`},
		},
	}

	actions, err := planMigration(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !actions[0].Skipped {
		t.Error("expected action to be skipped (existing bead)")
	}
}

func TestPlanMigration_Empty(t *testing.T) {
	client := &mockMigrator{
		configs: map[string][]beadsapi.ConfigEntry{},
		beads:   []*beadsapi.BeadDetail{},
	}

	actions, err := planMigration(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Errorf("expected 0 actions, got %d", len(actions))
	}
}

func TestExecuteMigration_CreatesBeads(t *testing.T) {
	client := &mockMigrator{created: nil}
	actions := []migrateAction{
		{Category: "claude-settings", Labels: []string{"global"}, Value: `{"model":"opus"}`},
		{Category: "claude-hooks", Labels: []string{"role:gardener"}, Value: `{"hooks":{}}`},
	}

	created, skipped, errors := executeMigration(context.Background(), client, actions)
	if created != 2 || skipped != 0 || errors != 0 {
		t.Errorf("expected 2/0/0, got %d/%d/%d", created, skipped, errors)
	}

	if len(client.created) != 2 {
		t.Fatalf("expected 2 create calls, got %d", len(client.created))
	}
	if client.created[0].Title != "claude-settings" {
		t.Errorf("wrong title: %s", client.created[0].Title)
	}
	if client.created[0].Type != "config" {
		t.Errorf("wrong type: %s", client.created[0].Type)
	}
	if client.created[0].Description != `{"model":"opus"}` {
		t.Errorf("wrong description: %s", client.created[0].Description)
	}
}

func TestExecuteMigration_SkipsExisting(t *testing.T) {
	client := &mockMigrator{created: nil}
	actions := []migrateAction{
		{Category: "claude-settings", Labels: []string{"global"}, Value: `{}`, Skipped: true},
		{Category: "claude-hooks", Labels: []string{"global"}, Value: `{}`},
	}

	created, skipped, errors := executeMigration(context.Background(), client, actions)
	if created != 1 || skipped != 1 || errors != 0 {
		t.Errorf("expected 1/1/0, got %d/%d/%d", created, skipped, errors)
	}
}

// --- Role dump/load tests ---

func TestDumpRoles_CollectsAndSorts(t *testing.T) {
	lister := &mockBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "r2", Title: "ops", Type: "role", Status: "open",
				Fields: map[string]string{"image": "ops-image", "docker": "true"},
			},
			{
				ID: "r1", Title: "crew", Type: "role", Status: "open",
				Fields: map[string]string{
					"image":         "crew-image",
					"cpu_request":   "500m",
					"secrets":       `[{"env":"TOKEN","secret":"crew-gh","key":"token"}]`,
					"claude_config": `{"claude-settings":{"model":"opus"}}`,
				},
			},
		},
	}

	entries, err := dumpRoles(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2, got %d", len(entries))
	}
	// Sorted alphabetically.
	if entries[0].Name != "crew" {
		t.Errorf("expected first=crew, got %s", entries[0].Name)
	}
	if entries[1].Name != "ops" {
		t.Errorf("expected second=ops, got %s", entries[1].Name)
	}
	// Fields captured.
	if entries[0].Fields["image"] != "crew-image" {
		t.Errorf("crew image: got %q", entries[0].Fields["image"])
	}
	if entries[0].Fields["claude_config"] == "" {
		t.Error("crew claude_config should be captured")
	}
}

func TestDumpRoles_SkipsEmptyTitle(t *testing.T) {
	lister := &mockBeadLister{
		beads: []*beadsapi.BeadDetail{
			{ID: "r1", Title: "", Type: "role", Status: "open", Fields: map[string]string{"image": "x"}},
			{ID: "r2", Title: "crew", Type: "role", Status: "open", Fields: map[string]string{"image": "y"}},
		},
	}

	entries, err := dumpRoles(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 (empty title skipped), got %d", len(entries))
	}
}

func TestDumpRoles_SkipsEmptyFields(t *testing.T) {
	lister := &mockBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "r1", Title: "crew", Type: "role", Status: "open",
				Fields: map[string]string{"image": "crew-img", "cpu_request": "", "storage_class": ""},
			},
		},
	}

	entries, err := dumpRoles(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("expected 1 entry")
	}
	// Only non-empty fields should be captured.
	if len(entries[0].Fields) != 1 {
		t.Errorf("expected 1 field (image only), got %d: %v", len(entries[0].Fields), entries[0].Fields)
	}
}

type mockRoleLoader struct {
	beads   []*beadsapi.BeadDetail
	created []beadsapi.CreateBeadRequest
	updated map[string]map[string]string // beadID -> fields
	err     error
}

func (m *mockRoleLoader) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

func (m *mockRoleLoader) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	m.created = append(m.created, req)
	return fmt.Sprintf("gd-role-%d", len(m.created)), nil
}

func (m *mockRoleLoader) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	if m.err != nil {
		return m.err
	}
	if m.updated == nil {
		m.updated = make(map[string]map[string]string)
	}
	m.updated[beadID] = fields
	return nil
}

func TestLoadRoles_CreatesNew(t *testing.T) {
	client := &mockRoleLoader{beads: []*beadsapi.BeadDetail{}}
	entries := []roleEntry{
		{Name: "crew", Fields: map[string]string{"image": "crew-img", "docker": "true"}},
	}

	created, updated, errors := loadRoles(context.Background(), client, entries)
	if created != 1 || updated != 0 || errors != 0 {
		t.Errorf("expected 1/0/0, got %d/%d/%d", created, updated, errors)
	}
	if len(client.created) != 1 {
		t.Fatal("expected 1 create")
	}
	if client.created[0].Type != "role" {
		t.Errorf("wrong type: %s", client.created[0].Type)
	}
}

func TestLoadRoles_UpdatesExisting(t *testing.T) {
	client := &mockRoleLoader{
		beads: []*beadsapi.BeadDetail{
			{ID: "existing-1", Title: "crew", Type: "role", Status: "open"},
		},
	}
	entries := []roleEntry{
		{Name: "crew", Fields: map[string]string{"image": "new-img"}},
	}

	created, updated, errors := loadRoles(context.Background(), client, entries)
	if created != 0 || updated != 1 || errors != 0 {
		t.Errorf("expected 0/1/0, got %d/%d/%d", created, updated, errors)
	}
	if client.updated["existing-1"]["image"] != "new-img" {
		t.Errorf("expected updated image=new-img, got %v", client.updated)
	}
}

func TestLoadRoles_MixedCreateAndUpdate(t *testing.T) {
	client := &mockRoleLoader{
		beads: []*beadsapi.BeadDetail{
			{ID: "existing-1", Title: "crew", Type: "role", Status: "open"},
		},
	}
	entries := []roleEntry{
		{Name: "crew", Fields: map[string]string{"image": "updated-img"}},
		{Name: "ops", Fields: map[string]string{"image": "new-ops"}},
	}

	created, updated, errors := loadRoles(context.Background(), client, entries)
	if created != 1 || updated != 1 || errors != 0 {
		t.Errorf("expected 1/1/0, got %d/%d/%d", created, updated, errors)
	}
}

func TestConfigDump_IncludesRoles(t *testing.T) {
	dump := configDump{
		Roles: []roleEntry{
			{Name: "crew", Fields: map[string]string{"image": "crew-img"}},
		},
	}

	data, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}

	var loaded configDump
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	if len(loaded.Roles) != 1 {
		t.Errorf("expected 1 role, got %d", len(loaded.Roles))
	}
	if loaded.Roles[0].Name != "crew" {
		t.Errorf("expected name=crew, got %s", loaded.Roles[0].Name)
	}
}

// --- Field-keyed section dump/load tests (triggers/schedules/projects/accounts/formulas) ---

// fieldKeyedDumpCase drives a table test over each per-type dump function,
// asserting allowlisting and sorting behave identically to dumpRoles.
func TestDumpFieldKeyedBeads_PerType(t *testing.T) {
	cases := []struct {
		name      string
		beadType  string
		dump      func(context.Context, beadLister) ([]roleEntry, error)
		keepField string // allowlisted field that must survive
		dropField string // non-allowlisted field that must be stripped
		keepValue string
	}{
		{"triggers", "trigger", dumpTriggers, "cron", "rotation_count", "0 * * * *"},
		{"schedules", "schedule", dumpSchedules, "prompt", "session_count", "do the thing"},
		{"projects", "project", dumpProjects, "prefix", "internal_state", "kd"},
		{"accounts", "account", dumpAccounts, "provider", "total_cost_usd", "anthropic"},
		{"formulas", "formula", dumpFormulas, "steps", "last_run_at", `[{"x":1}]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lister := &mockBeadLister{
				beads: []*beadsapi.BeadDetail{
					{
						ID: "b2", Title: "zeta", Type: tc.beadType, Status: "open",
						Fields: map[string]string{tc.keepField: tc.keepValue, tc.dropField: "should-be-stripped"},
					},
					{
						ID: "b1", Title: "alpha", Type: tc.beadType, Status: "open",
						Fields: map[string]string{tc.keepField: tc.keepValue, "empty_one": ""},
					},
					// Empty title must be skipped.
					{ID: "b0", Title: "", Type: tc.beadType, Status: "open", Fields: map[string]string{tc.keepField: tc.keepValue}},
				},
			}

			entries, err := tc.dump(context.Background(), lister)
			if err != nil {
				t.Fatalf("dump: %v", err)
			}
			if len(entries) != 2 {
				t.Fatalf("expected 2 (empty title skipped), got %d", len(entries))
			}
			// Sorted by title.
			if entries[0].Name != "alpha" || entries[1].Name != "zeta" {
				t.Errorf("expected sorted [alpha zeta], got [%s %s]", entries[0].Name, entries[1].Name)
			}
			// Allowlisted field survives.
			if entries[1].Fields[tc.keepField] != tc.keepValue {
				t.Errorf("expected %s=%q, got %q", tc.keepField, tc.keepValue, entries[1].Fields[tc.keepField])
			}
			// Non-allowlisted field is stripped.
			if _, ok := entries[1].Fields[tc.dropField]; ok {
				t.Errorf("expected %s to be excluded, got %v", tc.dropField, entries[1].Fields)
			}
			// Empty-valued allowlisted field is omitted.
			if _, ok := entries[0].Fields["empty_one"]; ok {
				t.Errorf("expected empty field to be omitted, got %v", entries[0].Fields)
			}
		})
	}
}

// Account dump must exclude litellm_key and all runtime/telemetry fields, per
// the strict allowlist (provider, model, credential, fallback, base_url,
// probe_model).
func TestDumpAccounts_ExcludesSecretAndRuntimeFields(t *testing.T) {
	lister := &mockBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "a1", Title: "anthropic-prod", Type: "account", Status: "open",
				Fields: map[string]string{
					// Allowlisted config fields — must survive.
					"provider":    "anthropic",
					"model":       "claude-opus-4-8",
					"credential":  "gardenio-claude-oauth-prod",
					"fallback":    "anthropic-backup",
					"base_url":    "https://api.anthropic.com",
					"probe_model": "claude-haiku-4-5",
					// Raw secret — must be excluded.
					"litellm_key": "sk-secret-do-not-dump",
					// Runtime/telemetry — must all be excluded.
					"total_tokens":         "123456",
					"total_cost_usd":       "42.50",
					"utilization_pct":      "88",
					"throttle_until":       "2026-01-01T00:00:00Z",
					"probe_checked_at":     "2026-01-01T00:00:00Z",
					"rotation_count":       "7",
					"session_count":        "12",
					"last_rotated_at":      "2026-01-01T00:00:00Z",
					"exhausted_at":         "2026-01-01T00:00:00Z",
					"cooldown_until":       "2026-01-01T00:00:00Z",
					"soft_failover_state":  "active",
					"overage_count":        "3",
					"shadow_model":         "x",
					"representative_claim": "y",
					"status":               "healthy",
				},
			},
		},
	}

	entries, err := dumpAccounts(context.Background(), lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1, got %d", len(entries))
	}
	f := entries[0].Fields

	// Exactly the 6 allowlisted fields, nothing else.
	if len(f) != 6 {
		t.Errorf("expected exactly 6 allowlisted fields, got %d: %v", len(f), f)
	}
	for _, want := range []string{"provider", "model", "credential", "fallback", "base_url", "probe_model"} {
		if f[want] == "" {
			t.Errorf("expected allowlisted field %q to be present", want)
		}
	}
	// Explicit exclusions.
	forbidden := []string{
		"litellm_key", "total_tokens", "total_cost_usd", "utilization_pct",
		"throttle_until", "probe_checked_at", "rotation_count", "session_count",
		"last_rotated_at", "exhausted_at", "cooldown_until", "soft_failover_state",
		"overage_count", "shadow_model", "representative_claim", "status",
	}
	for _, bad := range forbidden {
		if _, ok := f[bad]; ok {
			t.Errorf("field %q must NOT be dumped (allowlist violation)", bad)
		}
	}
}

func TestLoadFieldKeyedBeads_PerType(t *testing.T) {
	cases := []struct {
		name     string
		beadType string
		beadKind string
		load     func(context.Context, roleLoader, []roleEntry) (int, int, int)
	}{
		{"triggers", "trigger", "data", loadTriggers},
		{"schedules", "schedule", "config", loadSchedules},
		{"projects", "project", "config", loadProjects},
		{"accounts", "account", "config", loadAccounts},
		{"formulas", "formula", "data", loadFormulas},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/create", func(t *testing.T) {
			client := &mockRoleLoader{beads: []*beadsapi.BeadDetail{}}
			created, updated, errors := tc.load(context.Background(), client, []roleEntry{
				{Name: "thing-a", Fields: map[string]string{"cron": "x"}},
			})
			if created != 1 || updated != 0 || errors != 0 {
				t.Errorf("expected 1/0/0, got %d/%d/%d", created, updated, errors)
			}
			if len(client.created) != 1 {
				t.Fatalf("expected 1 create, got %d", len(client.created))
			}
			if client.created[0].Type != tc.beadType {
				t.Errorf("expected type=%s, got %s", tc.beadType, client.created[0].Type)
			}
			if client.created[0].Kind != tc.beadKind {
				t.Errorf("expected kind=%s, got %s", tc.beadKind, client.created[0].Kind)
			}
		})

		t.Run(tc.name+"/update", func(t *testing.T) {
			client := &mockRoleLoader{
				beads: []*beadsapi.BeadDetail{
					{ID: "existing-1", Title: "thing-a", Type: tc.beadType, Status: "open"},
				},
			}
			created, updated, errors := tc.load(context.Background(), client, []roleEntry{
				{Name: "thing-a", Fields: map[string]string{"cron": "updated"}},
			})
			if created != 0 || updated != 1 || errors != 0 {
				t.Errorf("expected 0/1/0, got %d/%d/%d", created, updated, errors)
			}
			if client.updated["existing-1"]["cron"] != "updated" {
				t.Errorf("expected updated cron, got %v", client.updated)
			}
		})
	}
}

func TestConfigDump_IncludesAllFieldKeyedSections(t *testing.T) {
	dump := configDump{
		Triggers:  []roleEntry{{Name: "cap", Fields: map[string]string{"cron": "x"}}},
		Schedules: []roleEntry{{Name: "sched", Fields: map[string]string{"cron": "y"}}},
		Projects:  []roleEntry{{Name: "gardenio", Fields: map[string]string{"prefix": "kd"}}},
		Accounts:  []roleEntry{{Name: "acct", Fields: map[string]string{"provider": "anthropic"}}},
		Formulas:  []roleEntry{{Name: "rel", Fields: map[string]string{"steps": "[]"}}},
	}

	data, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}

	var loaded configDump
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	if len(loaded.Triggers) != 1 || loaded.Triggers[0].Name != "cap" {
		t.Errorf("triggers round-trip failed: %v", loaded.Triggers)
	}
	if len(loaded.Schedules) != 1 || len(loaded.Projects) != 1 ||
		len(loaded.Accounts) != 1 || len(loaded.Formulas) != 1 {
		t.Errorf("expected all sections present, got %+v", loaded)
	}

	// JSON tags must be the lowercase plural section names.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"triggers", "schedules", "projects", "accounts", "formulas"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("expected JSON key %q in dump", key)
		}
	}
}

func TestConfigDump_OmitsEmptyFieldKeyedSections(t *testing.T) {
	dump := configDump{
		Configs: []configEntry{{Key: "a", Value: json.RawMessage(`{}`)}},
	}
	data, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"triggers", "schedules", "projects", "accounts", "formulas"} {
		if _, ok := raw[key]; ok {
			t.Errorf("expected %q to be omitted when empty", key)
		}
	}
}

func TestExecuteMigration_ContinuesOnError(t *testing.T) {
	client := &mockMigrator{err: fmt.Errorf("fail")}
	actions := []migrateAction{
		{Category: "a", Labels: []string{"global"}, Value: `{}`},
		{Category: "b", Labels: []string{"global"}, Value: `{}`},
	}

	created, skipped, errors := executeMigration(context.Background(), client, actions)
	if created != 0 || skipped != 0 || errors != 2 {
		t.Errorf("expected 0/0/2, got %d/%d/%d", created, skipped, errors)
	}
}
