package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// roleHubFake routes ListBeadsFiltered by query SHAPE so a buildRoleHub test can
// seed the role-label agent query, the role-field agent query, the role-scoped
// config query, and the role-scoped advice query independently. The shared
// fakeDaemon collapses every active-agent query onto liveAgentsResp (it routes
// by Types=[agent] + sort, not by label/field), which would make the two
// agent-union queries indistinguishable — so the role hub needs its own router.
// Embeds *fakeDaemon for the rest of the beadAPI surface (notably ListRoleBeads,
// served from rolesResp/rolesErr).
type roleHubFake struct {
	*fakeDaemon

	agentsByLabel *beadsapi.ListBeadsResult // Types=[agent] + Labels=[role:X]
	agentsByField *beadsapi.ListBeadsResult // Types=[agent] + Fields{role:X}
	configResp    *beadsapi.ListBeadsResult // Kinds=[config] + Labels=[role:X]
	adviceResp    *beadsapi.ListBeadsResult // Types=[advice] + Labels=[role:X]

	agentsByLabelErr error
	agentsByFieldErr error
	configErr        error
	adviceErr        error
}

func (f *roleHubFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	switch {
	case slices.Contains(q.Types, "advice"):
		return orEmpty(f.adviceResp), f.adviceErr
	case slices.Contains(q.Kinds, "config"):
		return orEmpty(f.configResp), f.configErr
	case slices.Contains(q.Types, "agent") && len(q.Fields) > 0:
		return orEmpty(f.agentsByField), f.agentsByFieldErr
	case slices.Contains(q.Types, "agent"):
		return orEmpty(f.agentsByLabel), f.agentsByLabelErr
	}
	return &beadsapi.ListBeadsResult{}, nil
}

func orEmpty(r *beadsapi.ListBeadsResult) *beadsapi.ListBeadsResult {
	if r == nil {
		return &beadsapi.ListBeadsResult{}
	}
	return r
}

func roleHubServer(f *roleHubFake) *Server {
	return &Server{daemon: f, logger: slog.Default(), basePath: ""}
}

func agentBead(id, name, role, project, state string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "agent",
		Status: "in_progress",
		Labels: []string{"project:" + project, "role:" + role},
		Fields: map[string]string{
			"agent":       name,
			"role":        role,
			"agent_state": state,
			"task_id":     "gd-task-" + id,
			"task_title":  "task for " + name,
		},
	}
}

// TestBuildRoleHub_FullAssembly is the happy path: a type:role bead resolves to
// overrides, agents across two projects (deduped across the label+field union)
// group by project, and the role-scoped config + advice lists populate.
func TestBuildRoleHub_FullAssembly(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	resume := true

	f := &roleHubFake{
		fakeDaemon: newFake(),
		agentsByLabel: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			agentBead("a1", "crew-alpha", "crew", "gardenio", "working"),
			agentBead("a2", "crew-beta", "crew", "kbeads", "working"),
		}},
		// a2 reappears via the field query (no label) plus a field-only agent a3;
		// a2 must dedupe, a3 must be added.
		agentsByField: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			agentBead("a2", "crew-beta", "crew", "kbeads", "working"),
			agentBead("a3", "crew-gamma", "crew", "gardenio", "idle"),
		}},
		configResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			{ID: "gd-cfg1", Type: "config", Status: "open", Title: "crew nudge prompt", UpdatedAt: now.Add(-2 * time.Hour)},
		}},
		adviceResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			{ID: "gd-adv1", Type: "advice", Status: "open", Title: "crew advice", UpdatedAt: now.Add(-30 * time.Minute)},
		}},
	}
	f.rolesResp = map[string]beadsapi.RoleInfo{
		"crew": {
			Name:             "crew",
			Image:            "registry/agent:crew",
			RuntimeClassName: "runsc",
			Account:          "acct-crew",
			Model:            "opus",
			Harness:          "codex",
			DockerEnabled:    true,
			SessionResume:    &resume,
			Secrets:          []beadsapi.SecretEntry{{Env: "FOO", Secret: "s", Key: "k"}},
			EnvVars:          []beadsapi.EnvEntry{{Name: "BAR", Value: "baz"}},
			EnvOverrides:     map[string]string{"Z": "1", "A": "2"},
			ClaudeConfig:     map[string]json.RawMessage{"hooks": json.RawMessage(`{}`)},
			AccountWhitelist: []string{"acct-crew", "acct-spare"},
			EnabledSkills:    []string{"onboarding", "video"},
			DisabledSkills:   []string{"deploy"},
		},
	}

	view := roleHubServer(f).buildRoleHub(context.Background(), "crew", now)

	if view.Name != "crew" {
		t.Fatalf("Name = %q, want crew", view.Name)
	}
	if !view.Found {
		t.Fatal("Found = false, want true (type:role bead exists)")
	}
	if view.Info.Image != "registry/agent:crew" {
		t.Errorf("Info.Image = %q, want registry/agent:crew", view.Info.Image)
	}

	// Agents: a1+a3 in gardenio, a2 in kbeads; a2 deduped across the two queries.
	if view.AgentCount != 3 {
		t.Fatalf("AgentCount = %d, want 3 (a2 deduped across label+field)", view.AgentCount)
	}
	if len(view.AgentGroups) != 2 {
		t.Fatalf("AgentGroups = %d, want 2", len(view.AgentGroups))
	}
	// Groups sorted by project name: gardenio before kbeads.
	if view.AgentGroups[0].Project != "gardenio" || view.AgentGroups[1].Project != "kbeads" {
		t.Errorf("group order = [%s,%s], want [gardenio,kbeads]",
			view.AgentGroups[0].Project, view.AgentGroups[1].Project)
	}
	if len(view.AgentGroups[0].Agents) != 2 {
		t.Errorf("gardenio group = %d agents, want 2", len(view.AgentGroups[0].Agents))
	}
	// Within gardenio, agents sorted by name: crew-alpha before crew-gamma.
	if view.AgentGroups[0].Agents[0].Name != "crew-alpha" {
		t.Errorf("first gardenio agent = %q, want crew-alpha", view.AgentGroups[0].Agents[0].Name)
	}

	// Overrides: scalar rows populated, false-able flags only when set.
	wantOverrides := map[string]string{
		"Image":          "registry/agent:crew",
		"Runtime class":  "runsc",
		"Account":        "acct-crew",
		"Model":          "opus",
		"Harness":        "codex",
		"Docker":         "enabled",
		"Session resume": "true",
	}
	got := map[string]string{}
	for _, row := range view.Overrides {
		got[row.Label] = row.Value
	}
	for label, want := range wantOverrides {
		if got[label] != want {
			t.Errorf("override %q = %q, want %q", label, got[label], want)
		}
	}
	// Postgres/K8s-API are unset (false) → must NOT appear (a parsed false is
	// indistinguishable from unset, so a row would be a lie).
	if _, ok := got["Postgres"]; ok {
		t.Error("Postgres override present but PostgresEnabled is false")
	}

	// Secret carried by NAME only.
	if len(view.Secrets) != 1 || view.Secrets[0].Env != "FOO" || view.Secrets[0].Secret != "s" {
		t.Errorf("Secrets = %+v, want one {FOO,s,k}", view.Secrets)
	}
	// env_json + claude_config surfaced as sorted KEYS only.
	if want := []string{"A", "Z"}; !slices.Equal(view.EnvOverrideKeys, want) {
		t.Errorf("EnvOverrideKeys = %v, want %v (sorted keys only)", view.EnvOverrideKeys, want)
	}
	if want := []string{"hooks"}; !slices.Equal(view.ClaudeConfigKeys, want) {
		t.Errorf("ClaudeConfigKeys = %v, want %v", view.ClaudeConfigKeys, want)
	}
	if want := []string{"acct-crew", "acct-spare"}; !slices.Equal(view.AccountWhitelist, want) {
		t.Errorf("AccountWhitelist = %v, want %v", view.AccountWhitelist, want)
	}
	// Per-role skill toggle surfaced on the view-model (Adam feedback #1).
	if want := []string{"onboarding", "video"}; !slices.Equal(view.EnabledSkills, want) {
		t.Errorf("EnabledSkills = %v, want %v", view.EnabledSkills, want)
	}
	if want := []string{"deploy"}; !slices.Equal(view.DisabledSkills, want) {
		t.Errorf("DisabledSkills = %v, want %v", view.DisabledSkills, want)
	}

	// Scoped config + advice.
	if len(view.Config) != 1 || view.Config[0].ID != "gd-cfg1" {
		t.Errorf("Config = %+v, want one gd-cfg1", view.Config)
	}
	if len(view.Advice) != 1 || view.Advice[0].ID != "gd-adv1" {
		t.Errorf("Advice = %+v, want one gd-adv1", view.Advice)
	}
}

// TestBuildRoleHub_UnknownRole verifies the label-only/builtin role path: no
// type:role bead, but agents + scoped beads still render. Found is false.
func TestBuildRoleHub_UnknownRole(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	f := &roleHubFake{
		fakeDaemon: newFake(),
		agentsByLabel: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			agentBead("a1", "thread-x", "thread", "gardenio", "working"),
		}},
	}
	f.rolesResp = map[string]beadsapi.RoleInfo{} // no "thread" role bead

	view := roleHubServer(f).buildRoleHub(context.Background(), "thread", now)

	if view.Found {
		t.Error("Found = true, want false for a role with no type:role bead")
	}
	if len(view.Overrides) != 0 {
		t.Errorf("Overrides = %d, want 0 for an unknown role", len(view.Overrides))
	}
	if view.AgentCount != 1 {
		t.Errorf("AgentCount = %d, want 1 (agents still render for unknown role)", view.AgentCount)
	}
}

// TestBuildRoleHub_BestEffortDegradation verifies that an error on every
// sub-fetch degrades to empty panels rather than failing the assembly — the
// assembler returns a non-nil view and never panics.
func TestBuildRoleHub_BestEffortDegradation(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	boom := errors.New("daemon down")
	f := &roleHubFake{
		fakeDaemon:       newFake(),
		agentsByLabelErr: boom,
		agentsByFieldErr: boom,
		configErr:        boom,
		adviceErr:        boom,
	}
	f.rolesErr = boom

	view := roleHubServer(f).buildRoleHub(context.Background(), "crew", now)

	if view == nil {
		t.Fatal("buildRoleHub returned nil on all-error path")
	}
	if view.Name != "crew" {
		t.Errorf("Name = %q, want crew (always populated)", view.Name)
	}
	if view.Found {
		t.Error("Found = true despite ListRoleBeads error")
	}
	if view.AgentCount != 0 || len(view.AgentGroups) != 0 {
		t.Errorf("agents = %d/%d, want 0/0 on error", view.AgentCount, len(view.AgentGroups))
	}
	if len(view.Config) != 0 || len(view.Advice) != 0 {
		t.Errorf("config/advice = %d/%d, want 0/0 on error", len(view.Config), len(view.Advice))
	}
}

// TestBuildRoleHub_EmptyRoleName guards the degenerate empty-path: a blank role
// name returns an empty view without issuing any daemon call.
func TestBuildRoleHub_EmptyRoleName(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	f := &roleHubFake{fakeDaemon: newFake()}

	view := roleHubServer(f).buildRoleHub(context.Background(), "  ", now)

	if view.Name != "" {
		t.Errorf("Name = %q, want empty for blank role", view.Name)
	}
	if view.Found || view.AgentCount != 0 || len(view.Config) != 0 {
		t.Error("blank role produced non-empty panels")
	}
}

// TestBuildRoleHub_SessionResumeFalse verifies the tri-state *bool: an
// explicit false surfaces (distinct from nil/inherit, which is omitted).
func TestBuildRoleHub_SessionResumeFalse(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	no := false
	f := &roleHubFake{fakeDaemon: newFake()}
	f.rolesResp = map[string]beadsapi.RoleInfo{
		"reviewer": {Name: "reviewer", SessionResume: &no},
	}

	view := roleHubServer(f).buildRoleHub(context.Background(), "reviewer", now)

	var found bool
	for _, row := range view.Overrides {
		if row.Label == "Session resume" {
			found = true
			if row.Value != "false" {
				t.Errorf("Session resume = %q, want false", row.Value)
			}
		}
	}
	if !found {
		t.Error("explicit SessionResume=false did not surface as an override row")
	}
}
