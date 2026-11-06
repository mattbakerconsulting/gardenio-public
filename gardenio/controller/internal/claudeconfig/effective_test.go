package claudeconfig

import (
	"context"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mockLister is a BeadLister backed by a fixed bead slice.
type mockLister struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (m *mockLister) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

func settingsBead(labels []string, desc string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:          "gd-" + labels[len(labels)-1],
		Title:       "claude-settings",
		Labels:      labels,
		Description: desc,
	}
}

// TestResolveEffectiveConfig_DefaultsOnly: no config beads → the fleet defaults
// govern. ultracode OFF (rendered as unset), effort=high from the default pin.
func TestResolveEffectiveConfig_DefaultsOnly(t *testing.T) {
	lister := &mockLister{}
	ec, err := ResolveEffectiveConfig(context.Background(), lister, []string{"global", "role:crew"})
	if err != nil {
		t.Fatal(err)
	}
	if ec.LayerCount != 0 {
		t.Errorf("expected 0 layers, got %d", ec.LayerCount)
	}
	if uc := ec.Knobs[KnobUltracode]; uc.Set {
		t.Errorf("expected ultracode unset (OFF default), got %+v", uc)
	}
	effort := ec.Knobs[KnobEffort]
	if !effort.Set || effort.Value != "high" {
		t.Errorf("expected effort=high (default), got %+v", effort)
	}
	if effort.Provenance != "default" {
		t.Errorf("expected effort provenance=default, got %q", effort.Provenance)
	}
	if ec.EffortTrap {
		t.Error("expected no effort trap with defaults only")
	}
}

// TestResolveEffectiveConfig_RoleUltracodeXhigh: a role bead that sets only
// ultracode:true (with the workflows env knob) → effort resolves to xhigh
// attributed to the role, NO trap, because clearEffortForUltracode dropped the
// inherited high pin.
func TestResolveEffectiveConfig_RoleUltracodeXhigh(t *testing.T) {
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		settingsBead([]string{"role:thread"}, `{"ultracode":true,"env":{"CLAUDE_CODE_WORKFLOWS":"1"}}`),
	}}
	ec, err := ResolveEffectiveConfig(context.Background(), lister, []string{"global", "role:thread"})
	if err != nil {
		t.Fatal(err)
	}
	uc := ec.Knobs[KnobUltracode]
	if !uc.Set || uc.Value != "true" || uc.Provenance != "role" {
		t.Errorf("expected ultracode true from role, got %+v", uc)
	}
	effort := ec.Knobs[KnobEffort]
	if !effort.Set || effort.Value != "xhigh" {
		t.Errorf("expected effective effort xhigh, got %+v", effort)
	}
	if effort.Provenance != "role" {
		t.Errorf("expected effort provenance=role (ultracode layer), got %q", effort.Provenance)
	}
	if ec.EffortTrap {
		t.Error("expected NO trap: role ultracode cleanly reached xhigh")
	}
	wf := ec.Knobs[KnobWorkflows]
	if !wf.Set || wf.Value != "on" {
		t.Errorf("expected workflows on, got %+v", wf)
	}
}

// TestResolveEffectiveConfig_GlobalEffortTrap: THE trap the matrix exists to
// surface. A global bead pins effort=high; a role bead flips ultracode:true but
// does NOT re-pin effort. Because global effort merges key-by-key and survives,
// ultracode's xhigh is BLOCKED — effort stays high, provenance=global, trap=true.
func TestResolveEffectiveConfig_GlobalEffortTrap(t *testing.T) {
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		settingsBead([]string{"global"}, `{"env":{"CLAUDE_CODE_EFFORT_LEVEL":"high"}}`),
		settingsBead([]string{"role:crew"}, `{"ultracode":true}`),
	}}
	ec, err := ResolveEffectiveConfig(context.Background(), lister, []string{"global", "role:crew"})
	if err != nil {
		t.Fatal(err)
	}
	uc := ec.Knobs[KnobUltracode]
	if !uc.Set || uc.Provenance != "role" {
		t.Errorf("expected ultracode true from role, got %+v", uc)
	}
	effort := ec.Knobs[KnobEffort]
	if !effort.Set || effort.Value != "high" {
		t.Errorf("expected effort pinned to high (trap), got %+v", effort)
	}
	if effort.Provenance != "global" {
		t.Errorf("expected effort provenance=global (blocking pin), got %+v", effort)
	}
	if !ec.EffortTrap {
		t.Fatal("expected EffortTrap=true: global effort pin blocks ultracode xhigh")
	}
}

// TestResolveEffectiveConfig_RolePinsOwnEffort: a role that sets ultracode AND
// its own effort pin is a deliberate operator choice, NOT the trap.
func TestResolveEffectiveConfig_RolePinsOwnEffort(t *testing.T) {
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		settingsBead([]string{"global"}, `{"env":{"CLAUDE_CODE_EFFORT_LEVEL":"high"}}`),
		settingsBead([]string{"role:gardener"}, `{"ultracode":true,"env":{"CLAUDE_CODE_EFFORT_LEVEL":"medium"}}`),
	}}
	ec, err := ResolveEffectiveConfig(context.Background(), lister, []string{"global", "role:gardener"})
	if err != nil {
		t.Fatal(err)
	}
	effort := ec.Knobs[KnobEffort]
	if effort.Value != "medium" || effort.Provenance != "role" {
		t.Errorf("expected effort=medium from role, got %+v", effort)
	}
	if ec.EffortTrap {
		t.Error("expected NO trap: role pinned its own effort deliberately")
	}
}

// TestResolveEffectiveConfig_ModelProvenance: model set at project scope, role
// leaves it alone → effective model from project.
func TestResolveEffectiveConfig_ModelProvenance(t *testing.T) {
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		settingsBead([]string{"project:gardenio"}, `{"model":"claude-sonnet-5"}`),
		settingsBead([]string{"role:crew-lite"}, `{"model":"claude-haiku-4-5"}`),
	}}
	// crew (no crew-lite override) → project model.
	ec, err := ResolveEffectiveConfig(context.Background(), lister, []string{"global", "role:crew", "project:gardenio"})
	if err != nil {
		t.Fatal(err)
	}
	model := ec.Knobs[KnobModel]
	if model.Value != "claude-sonnet-5" || model.Provenance != "project" {
		t.Errorf("expected model from project, got %+v", model)
	}
	// crew-lite → role model wins.
	ec2, _ := ResolveEffectiveConfig(context.Background(), lister, []string{"global", "role:crew-lite", "project:gardenio"})
	m2 := ec2.Knobs[KnobModel]
	if m2.Value != "claude-haiku-4-5" || m2.Provenance != "role" {
		t.Errorf("expected model from role crew-lite, got %+v", m2)
	}
}

// TestResolveEffectiveConfig_DaemonError propagates listing failures.
func TestResolveEffectiveConfig_DaemonError(t *testing.T) {
	lister := &mockLister{err: context.DeadlineExceeded}
	_, err := ResolveEffectiveConfig(context.Background(), lister, []string{"global"})
	if err == nil {
		t.Fatal("expected error on daemon failure")
	}
}

func TestDiscoverRoleScopes(t *testing.T) {
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		{ID: "gd-g", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"global"}},
		{ID: "gd-a", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"project:gardenio", "role:crew"}},
		{ID: "gd-b", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"project:gascity", "role:crew"}},
		{ID: "gd-c", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"role:reviewer"}},
	}}

	all, err := DiscoverRoleScopes(context.Background(), lister, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []RoleScope{{"", "reviewer"}, {"gardenio", "crew"}, {"gascity", "crew"}}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("project-agnostic discovery: want %v, got %v", want, all)
	}

	scoped, err := DiscoverRoleScopes(context.Background(), lister, "gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantScoped := []RoleScope{{"", "reviewer"}, {"gardenio", "crew"}}
	if !reflect.DeepEqual(scoped, wantScoped) {
		t.Errorf("gardenio-scoped discovery: want %v, got %v", wantScoped, scoped)
	}
}

// TestResolveEffectiveConfig_SettingsMatchWritePath asserts the merged settings
// document equals what the write path (DefaultUserSettings + MergeSettingsOnto)
// produces, so the matrix and the pod's settings.json can never disagree.
func TestResolveEffectiveConfig_SettingsMatchWritePath(t *testing.T) {
	desc := `{"ultracode":true,"env":{"CLAUDE_CODE_WORKFLOWS":"1"}}`
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		settingsBead([]string{"role:thread"}, desc),
	}}
	ec, err := ResolveEffectiveConfig(context.Background(), lister, []string{"global", "role:thread"})
	if err != nil {
		t.Fatal(err)
	}
	// Independently reproduce the write path.
	want := MergeSettingsOntoDefaults(DefaultUserSettings(), map[string]any{
		"ultracode": true,
		"env":       map[string]any{"CLAUDE_CODE_WORKFLOWS": "1"},
	})
	gotEnv, _ := ec.Settings["env"].(map[string]any)
	wantEnv, _ := want["env"].(map[string]any)
	if _, present := gotEnv["CLAUDE_CODE_EFFORT_LEVEL"]; present {
		t.Error("effort key should be cleared under ultracode in merged settings")
	}
	if (gotEnv["CLAUDE_CODE_WORKFLOWS"] != wantEnv["CLAUDE_CODE_WORKFLOWS"]) ||
		ec.Settings["ultracode"] != want["ultracode"] {
		t.Errorf("merged settings diverge from write path:\n got=%v\nwant=%v", ec.Settings, want)
	}
}

func TestCountProjectScopedSettings(t *testing.T) {
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		{ID: "gd-1", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"global"}},
		{ID: "gd-2", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"project:gardenio", "role:crew"}},
		{ID: "gd-3", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"g1:project:gascity", "role:crew"}},
		{ID: "gd-4", Title: "claude-mcp", Type: "config", Status: "open", Labels: []string{"project:gardenio"}},
	}}
	got, err := CountProjectScopedSettings(context.Background(), lister)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("want 2 project-scoped claude-settings beads (group prefix stripped, other categories ignored), got %d", got)
	}
}
