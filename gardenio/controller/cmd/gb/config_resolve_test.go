package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"gardenio/clicreds/avareauth"
)

// --- Mock for config bead resolution ---

type mockConfigBeadLister struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (m *mockConfigBeadLister) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

// --- ResolveConfigBeads tests ---

func TestResolveConfigBeads_GlobalOnly(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"sonnet","permissions":{"allow":["Bash(*)"]}}`,
			},
		},
	}

	subs := []string{"global", "role:gardener"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)

	if count != 1 {
		t.Fatalf("expected 1 layer, got %d", count)
	}
	if merged["model"] != "sonnet" {
		t.Errorf("expected model=sonnet, got %v", merged["model"])
	}
}

func TestResolveConfigBeads_RoleOverridesGlobal(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"sonnet","alwaysThinkingEnabled":true}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"role:gardener"},
				Description: `{"model":"opus"}`,
			},
		},
	}

	subs := []string{"global", "role:gardener"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)

	if count != 2 {
		t.Fatalf("expected 2 layers, got %d", count)
	}
	if merged["model"] != "opus" {
		t.Errorf("expected model=opus (role override), got %v", merged["model"])
	}
	if merged["alwaysThinkingEnabled"] != true {
		t.Error("expected alwaysThinkingEnabled from global layer")
	}
}

func TestResolveConfigBeads_HooksConcat(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-hooks",
				Labels:      []string{"global"},
				Description: `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook prime"}]}]}}`,
			},
			{
				Title:       "claude-hooks",
				Labels:      []string{"role:gardener"},
				Description: `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook extra"}]}]}}`,
			},
		},
	}

	subs := []string{"global", "role:gardener"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-hooks", subs)

	if count != 2 {
		t.Fatalf("expected 2 layers, got %d", count)
	}
	hooks, ok := merged["hooks"].(map[string]any)
	if !ok {
		t.Fatal("expected hooks key")
	}
	sessionStart, ok := hooks["SessionStart"].([]any)
	if !ok {
		t.Fatal("expected SessionStart array")
	}
	if len(sessionStart) != 2 {
		t.Errorf("expected 2 SessionStart hooks (concatenated), got %d", len(sessionStart))
	}
}

func TestResolveConfigBeads_FiltersNonMatching(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"sonnet"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"role:engineer"},
				Description: `{"model":"haiku"}`,
			},
		},
	}

	// Agent is a gardener, should NOT get engineer settings.
	subs := []string{"global", "role:gardener"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)

	if count != 1 {
		t.Fatalf("expected 1 matching layer (global only), got %d", count)
	}
	if merged["model"] != "sonnet" {
		t.Errorf("expected model=sonnet (global only), got %v", merged["model"])
	}
}

// TestResolveConfigBeads_CanonicalProdScopingDoesNotLeak pins the
// ka-0000005600 regression: the live config bead gd-Xur4qDwePi carries
// title="claude-instructions" with ungrouped labels
// ["project:gardenio", "role:canonical-prod"] and seeds a 10-rule
// CRITICAL RULES preamble (umbrella epic ka-0000005972, no-implementation
// gate, batteries-included, beads DB migration, etc.). Before MR !2249
// (ka-0000001253) tightened MatchesSubscriptions, the bare project label
// formed its own OR group that any project:gardenio agent satisfied —
// leaking the canonical-prod preamble to every gardenio gardener, crew, and
// thread agent. After the fix the role: label is required across the
// advice as a whole.
//
// This is a complementary integration-layer regression test for the
// config-resolve path that surfaces the bug. !2249's tests cover
// MatchesSubscriptions; this one covers ResolveConfigBeads end-to-end
// with the exact shape of the live bead.
func TestResolveConfigBeads_CanonicalProdScopingDoesNotLeak(t *testing.T) {
	canonicalProdBead := &beadsapi.BeadDetail{
		Title:       "claude-instructions",
		Labels:      []string{"project:gardenio", "role:canonical-prod"},
		Description: `{"core_rules":"## Core Rules\n1. No implementation until human approval...","identity":"## canonical-prod"}`,
	}
	globalInstructions := &beadsapi.BeadDetail{
		Title:       "claude-instructions",
		Labels:      []string{"global"},
		Description: `{"prime_header":"# Beads Workflow Context"}`,
	}
	lister := &mockConfigBeadLister{beads: []*beadsapi.BeadDetail{globalInstructions, canonicalProdBead}}

	// Roles that MUST NOT see the canonical-prod preamble. Each subs list
	// mirrors what buildSubscriptions produces for that role on
	// project:gardenio.
	leakRoles := []struct {
		name string
		subs []string
	}{
		{"crew", []string{"global", "role:crew", "project:gardenio", "agent:gardenio-x"}},
		{"gardener", []string{"global", "role:gardener", "project:gardenio", "agent:gardenio-y"}},
		{"thread", []string{"global", "role:thread", "project:gardenio", "agent:thread-1"}},
		{"gardener-delivery", []string{"global", "role:gardener-delivery", "project:gardenio", "agent:gardenio-z"}},
	}
	for _, tc := range leakRoles {
		merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-instructions", tc.subs)
		if count != 1 {
			t.Errorf("[%s] expected 1 layer (global only), got %d — canonical-prod preamble leaked", tc.name, count)
		}
		if _, ok := merged["core_rules"]; ok {
			t.Errorf("[%s] canonical-prod core_rules leaked into merged config", tc.name)
		}
		if _, ok := merged["identity"]; ok {
			t.Errorf("[%s] canonical-prod identity leaked into merged config", tc.name)
		}
	}

	// canonical-prod role MUST still see the preamble.
	cpSubs := []string{"global", "role:canonical-prod", "project:gardenio", "agent:gardenio-cp"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-instructions", cpSubs)
	if count != 2 {
		t.Fatalf("canonical-prod agent: expected 2 layers (global + canonical-prod), got %d", count)
	}
	if _, ok := merged["core_rules"]; !ok {
		t.Error("canonical-prod agent: core_rules missing — preamble was over-filtered")
	}
	if _, ok := merged["identity"]; !ok {
		t.Error("canonical-prod agent: identity missing — preamble was over-filtered")
	}
}

func TestResolveConfigBeads_FiltersWrongCategory(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-mcp",
				Labels:      []string{"global"},
				Description: `{"mcpServers":{}}`,
			},
		},
	}

	subs := []string{"global"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)

	if count != 0 || merged != nil {
		t.Errorf("expected no match for wrong category, got %d layers", count)
	}
}

func TestResolveConfigBeads_NoBeads(t *testing.T) {
	lister := &mockConfigBeadLister{beads: []*beadsapi.BeadDetail{}}

	subs := []string{"global"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)

	if count != 0 || merged != nil {
		t.Errorf("expected 0 layers, got %d", count)
	}
}

func TestResolveConfigBeads_InvalidJSON(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `not valid json`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"role:gardener"},
				Description: `{"model":"opus"}`,
			},
		},
	}

	subs := []string{"global", "role:gardener"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)

	if count != 1 {
		t.Fatalf("expected 1 valid layer (skipping invalid), got %d", count)
	}
	if merged["model"] != "opus" {
		t.Errorf("expected model=opus, got %v", merged["model"])
	}
}

func TestResolveConfigBeads_UnknownCategory(t *testing.T) {
	lister := &mockConfigBeadLister{beads: []*beadsapi.BeadDetail{}}

	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "nonexistent", []string{"global"})
	if count != 0 || merged != nil {
		t.Error("expected nil for unknown category")
	}
}

func TestResolveConfigBeads_MultiRolePrecedence(t *testing.T) {
	// When an agent has multiple roles (e.g. "thread,crew"), the first role
	// should have higher precedence (its values override later roles).
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"sonnet","base":"yes"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"role:crew"},
				Description: `{"model":"haiku","crew_setting":"yes"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"role:thread"},
				Description: `{"model":"opus","thread_setting":"yes"}`,
			},
		},
	}

	// thread,crew -> thread is first (higher precedence)
	subs := []string{"global", "role:thread", "role:crew"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)

	if count != 3 {
		t.Fatalf("expected 3 layers, got %d", count)
	}
	// role:thread should win for model (first role = highest precedence)
	if merged["model"] != "opus" {
		t.Errorf("expected model=opus (first role thread wins), got %v", merged["model"])
	}
	// Both role settings should be present
	if merged["crew_setting"] != "yes" {
		t.Error("expected crew_setting=yes from role:crew layer")
	}
	if merged["thread_setting"] != "yes" {
		t.Error("expected thread_setting=yes from role:thread layer")
	}
	if merged["base"] != "yes" {
		t.Error("expected base=yes from global layer")
	}
}

func TestResolveConfigBeads_SpecificityOrder(t *testing.T) {
	// Verify that global < rig < role in merge order.
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"role:gardener"},
				Description: `{"model":"opus"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"sonnet","extra":"yes"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"project:gardenio"},
				Description: `{"model":"haiku","project":"gardenio"}`,
			},
		},
	}

	subs := []string{"global", "project:gardenio", "role:gardener"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)

	if count != 3 {
		t.Fatalf("expected 3 layers, got %d", count)
	}
	// role:gardener is most specific, should win for model.
	if merged["model"] != "opus" {
		t.Errorf("expected model=opus (role wins), got %v", merged["model"])
	}
	// global's "extra" should survive.
	if merged["extra"] != "yes" {
		t.Error("expected extra=yes from global layer")
	}
	// project's "project" should survive (not overridden by role).
	if merged["project"] != "gardenio" {
		t.Error("expected project=gardenio from project layer")
	}
}

func TestResolveConfigBeads_ProjectInlineOverridesRole(t *testing.T) {
	// Project bead inline config (extra layer at "2~:" specificity) should
	// override role config beads but be overridden by agent config beads.
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"sonnet","base":"yes"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"role:crew"},
				Description: `{"model":"haiku","role_key":"yes"}`,
			},
		},
	}

	// Inject project inline config as an extra layer.
	extras := []resolvedConfig{{
		value:       []byte(`{"model":"opus","project_inline":"yes"}`),
		specificity: projectInlineSpecificity,
	}}

	subs := []string{"global", "role:crew"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs, extras...)

	if count != 3 {
		t.Fatalf("expected 3 layers (global + role + inline), got %d", count)
	}
	// Project inline should override role for "model".
	if merged["model"] != "opus" {
		t.Errorf("expected model=opus (project inline wins over role), got %v", merged["model"])
	}
	// Role key should survive.
	if merged["role_key"] != "yes" {
		t.Error("expected role_key=yes from role layer")
	}
	// Project inline key should be present.
	if merged["project_inline"] != "yes" {
		t.Error("expected project_inline=yes from inline layer")
	}
	// Global base should survive.
	if merged["base"] != "yes" {
		t.Error("expected base=yes from global layer")
	}
}

func TestResolveConfigBeads_PerRoleModelSelection(t *testing.T) {
	// Simulates the crew-lite/crew-ultralite pattern: project sets opus,
	// role overrides to a cheaper model.
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"permissions":{"allow":["Bash(*)"]},"alwaysThinkingEnabled":true}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"project:monorepo"},
				Description: `{"model":"claude-opus-4-6"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"role:crew-lite"},
				Description: `{"model":"claude-sonnet-4-6"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"role:crew-ultralite"},
				Description: `{"model":"claude-haiku-4-5"}`,
			},
		},
	}

	tests := []struct {
		name      string
		role      string
		wantModel string
	}{
		{"crew-lite gets sonnet", "crew-lite", "claude-sonnet-4-6"},
		{"crew-ultralite gets haiku", "crew-ultralite", "claude-haiku-4-5"},
		{"crew (no role override) gets opus from project", "crew", "claude-opus-4-6"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subs := []string{"global", "role:" + tc.role, "project:monorepo"}
			merged, _, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)
			if merged == nil {
				t.Fatal("expected non-nil merged settings")
			}
			if merged["model"] != tc.wantModel {
				t.Errorf("model: got %v, want %s", merged["model"], tc.wantModel)
			}
			// Global settings should survive role/project overrides.
			if merged["alwaysThinkingEnabled"] != true {
				t.Error("expected alwaysThinkingEnabled from global layer")
			}
		})
	}
}

func TestResolveConfigBeads_RoleInlineModelOverridesProject(t *testing.T) {
	// Role bead claude_config (inline) overrides project-level config bead.
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"alwaysThinkingEnabled":true}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"project:monorepo"},
				Description: `{"model":"claude-opus-4-6"}`,
			},
		},
	}

	// Simulate role bead inline config (from fetchRoleClaudeConfig).
	roleInline := []resolvedConfig{{
		value:       []byte(`{"model":"claude-sonnet-4-6"}`),
		specificity: roleInlineSpecificity("crew-lite"),
	}}

	subs := []string{"global", "role:crew-lite", "project:monorepo"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs, roleInline...)

	if count != 3 {
		t.Fatalf("expected 3 layers (global + project + role-inline), got %d", count)
	}
	if merged["model"] != "claude-sonnet-4-6" {
		t.Errorf("expected model=claude-sonnet-4-6 (role inline wins over project), got %v", merged["model"])
	}
}

func TestResolveConfigBeads_AgentOverridesProjectInline(t *testing.T) {
	// Agent-level config beads (3:) should override project inline (2~:).
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"sonnet"}`,
			},
			{
				Title:       "claude-settings",
				Labels:      []string{"agent:gd-test-agent"},
				Description: `{"model":"opus"}`,
			},
		},
	}

	extras := []resolvedConfig{{
		value:       []byte(`{"model":"haiku","inline_key":"yes"}`),
		specificity: projectInlineSpecificity,
	}}

	subs := []string{"global", "agent:gd-test-agent"}
	merged, count, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs, extras...)

	if count != 3 {
		t.Fatalf("expected 3 layers, got %d", count)
	}
	// Agent should win for "model".
	if merged["model"] != "opus" {
		t.Errorf("expected model=opus (agent wins over project inline), got %v", merged["model"])
	}
	// Project inline key should survive.
	if merged["inline_key"] != "yes" {
		t.Error("expected inline_key=yes from project inline layer")
	}
}

// TestResolveConfigBeads_SameSpecificityNewerWins verifies that when two beads
// share the same specificity (e.g. two "global" claude-settings beads), the
// newer one wins. This guards against E2E-style failures where a leaked or
// stale config bead at the same scope used to flip results based on a random
// beadID alphabetical tiebreak (gd-znrju90bhg).
func TestResolveConfigBeads_SameSpecificityNewerWins(t *testing.T) {
	older := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	newer := older.Add(1 * time.Hour)

	// Order in the input list deliberately puts the newer bead FIRST and gives
	// it an alphabetically-smaller ID, so a buggy tiebreak (alphabetical or
	// input order) would pick the older bead.
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				ID:          "gd-aaaa",
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"claude-sonnet-4-6","fresh":"yes"}`,
				CreatedAt:   newer,
			},
			{
				ID:          "gd-zzzz",
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"sonnet","stale":"yes"}`,
				CreatedAt:   older,
			},
		},
	}

	subs := []string{"global"}
	merged, count, err := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 layers, got %d", count)
	}
	if merged["model"] != "claude-sonnet-4-6" {
		t.Errorf("expected newer global bead to win (model=claude-sonnet-4-6), got %v", merged["model"])
	}
	// Both layers' non-overlapping keys still survive (one-level merge).
	if merged["fresh"] != "yes" || merged["stale"] != "yes" {
		t.Errorf("expected both fresh and stale keys present, got %v", merged)
	}
}

// TestResolveConfigBeads_SameSpecificitySameTimestampStableByID verifies the
// final beadID tiebreak when two beads share both specificity and CreatedAt
// (rare but possible at sub-second resolution): higher beadID wins to match
// the newer-wins intent (alphabetical order is the only signal left).
func TestResolveConfigBeads_SameSpecificitySameTimestampStableByID(t *testing.T) {
	ts := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				ID:          "gd-aaaa",
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"first"}`,
				CreatedAt:   ts,
			},
			{
				ID:          "gd-zzzz",
				Title:       "claude-settings",
				Labels:      []string{"global"},
				Description: `{"model":"second"}`,
				CreatedAt:   ts,
			},
		},
	}

	subs := []string{"global"}
	merged, _, _ := ResolveConfigBeads(context.Background(), lister, "claude-settings", subs)
	// Higher beadID applies last → wins.
	if merged["model"] != "second" {
		t.Errorf("expected model=second (higher beadID wins on full tiebreak), got %v", merged["model"])
	}
}

// --- resolveConfigBeadsWithRetry tests ---

// flakyLister returns err for the first failFor calls to ListBeadsFiltered, then
// succeeds with beads. It models the busy shared kbeads instance that blips on a
// listing before recovering (the config-merge-override flake, ka-0000006617).
type flakyLister struct {
	beads   []*beadsapi.BeadDetail
	err     error
	failFor int
	calls   int
}

func (f *flakyLister) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.calls++
	if f.calls <= f.failFor {
		return nil, f.err
	}
	return &beadsapi.ListBeadsResult{Beads: f.beads, Total: len(f.beads)}, nil
}

// withFastBackoffs shrinks the retry schedule to near-zero for the duration of a
// test so retry paths don't add real wall-clock, and restores it afterward.
func withFastBackoffs(t *testing.T) {
	t.Helper()
	orig := resolveConfigBackoffs
	resolveConfigBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { resolveConfigBackoffs = orig })
}

func TestResolveConfigBeadsWithRetry_AbsorbsTransient(t *testing.T) {
	withFastBackoffs(t)
	lister := &flakyLister{
		beads: []*beadsapi.BeadDetail{
			{Title: "claude-settings", Labels: []string{"global"}, Description: `{"model":"sonnet"}`},
		},
		err:     errors.New("connection reset by peer"),
		failFor: 2, // fail twice, succeed on the third attempt
	}

	merged, count, err := resolveConfigBeadsWithRetry(context.Background(), lister, "claude-settings", []string{"global"})
	if err != nil {
		t.Fatalf("expected transient failure to be absorbed, got err: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 layer after recovery, got %d", count)
	}
	if merged["model"] != "sonnet" {
		t.Errorf("expected model=sonnet, got %v", merged["model"])
	}
	if lister.calls != 3 {
		t.Errorf("expected 3 attempts (2 fail + 1 success), got %d", lister.calls)
	}
}

func TestResolveConfigBeadsWithRetry_SucceedsFirstTry(t *testing.T) {
	withFastBackoffs(t)
	lister := &flakyLister{
		beads: []*beadsapi.BeadDetail{
			{Title: "claude-settings", Labels: []string{"global"}, Description: `{"model":"opus"}`},
		},
		failFor: 0,
	}

	_, _, err := resolveConfigBeadsWithRetry(context.Background(), lister, "claude-settings", []string{"global"})
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if lister.calls != 1 {
		t.Errorf("expected exactly 1 attempt on a healthy daemon, got %d", lister.calls)
	}
}

func TestResolveConfigBeadsWithRetry_PersistentFailureExhausts(t *testing.T) {
	withFastBackoffs(t)
	sentinel := errors.New("daemon down")
	lister := &flakyLister{
		err:     sentinel,
		failFor: 1000, // never recovers
	}

	_, _, err := resolveConfigBeadsWithRetry(context.Background(), lister, "claude-settings", []string{"global"})
	if err == nil {
		t.Fatal("expected a persistent failure to surface after retries are exhausted")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the underlying error to be returned, got %v", err)
	}
	// 1 initial + len(backoffs) retries.
	if want := 1 + len(resolveConfigBackoffs); lister.calls != want {
		t.Errorf("expected %d attempts, got %d", want, lister.calls)
	}
}

func TestResolveConfigBeadsWithRetry_BailsOnReauth(t *testing.T) {
	withFastBackoffs(t)
	lister := &flakyLister{
		err:     avareauth.ErrReauthRequired,
		failFor: 1000,
	}

	_, _, err := resolveConfigBeadsWithRetry(context.Background(), lister, "claude-settings", []string{"global"})
	if !errors.Is(err, avareauth.ErrReauthRequired) {
		t.Fatalf("expected ErrReauthRequired to be returned, got %v", err)
	}
	// A deterministic re-auth bounce must NOT burn the retry schedule.
	if lister.calls != 1 {
		t.Errorf("expected exactly 1 attempt on a re-auth bounce, got %d", lister.calls)
	}
}

func TestResolveConfigBeadsWithRetry_RespectsContextCancel(t *testing.T) {
	// Long backoffs so the context cancel wins the select race deterministically.
	orig := resolveConfigBackoffs
	resolveConfigBackoffs = []time.Duration{time.Hour}
	t.Cleanup(func() { resolveConfigBackoffs = orig })

	lister := &flakyLister{err: errors.New("blip"), failFor: 1000}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, _, err := resolveConfigBeadsWithRetry(ctx, lister, "claude-settings", []string{"global"})
	if err == nil {
		t.Fatal("expected an error when the context is cancelled during retry backoff")
	}
	if lister.calls != 1 {
		t.Errorf("expected 1 attempt before the cancelled context aborts the backoff, got %d", lister.calls)
	}
}
