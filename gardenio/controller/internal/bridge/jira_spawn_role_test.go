package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

type stubTriggerLister struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (s *stubTriggerLister) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &beadsapi.ListBeadsResult{Beads: s.beads}, nil
}

func newSpawnRoleLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func spawnRoleTriggerBead(fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{Fields: fields}
}

func TestResolveJiraSpawnRole_MatchingTrigger(t *testing.T) {
	lister := &stubTriggerLister{beads: []*beadsapi.BeadDetail{
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "monorepo",
			"role":    "monorepo-gardener",
			"enabled": "true",
		}),
	}}
	got, err := resolveJiraSpawnRole(context.Background(), lister, "monorepo", newSpawnRoleLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "monorepo-gardener" {
		t.Fatalf("got role %q, want monorepo-gardener", got)
	}
}

func TestResolveJiraSpawnRole_SkipsDisabledAndMismatchedProject(t *testing.T) {
	lister := &stubTriggerLister{beads: []*beadsapi.BeadDetail{
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "monorepo",
			"role":    "ignored-because-disabled",
			"enabled": "false",
		}),
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "other-project",
			"role":    "ignored-because-wrong-project",
		}),
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "monorepo",
			"role":    "monorepo-gardener",
		}),
	}}
	got, err := resolveJiraSpawnRole(context.Background(), lister, "monorepo", newSpawnRoleLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "monorepo-gardener" {
		t.Fatalf("got role %q, want monorepo-gardener", got)
	}
}

func TestResolveJiraSpawnRole_NoMatchReturnsError(t *testing.T) {
	lister := &stubTriggerLister{beads: []*beadsapi.BeadDetail{
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "other-project",
			"role":    "other-role",
		}),
	}}
	got, err := resolveJiraSpawnRole(context.Background(), lister, "monorepo", newSpawnRoleLogger())
	if err == nil {
		t.Fatalf("expected error when no trigger matches, got role %q", got)
	}
	if got != "" {
		t.Fatalf("got role %q, want empty", got)
	}
}

func TestResolveJiraSpawnRole_SkipsNonSpawnAction(t *testing.T) {
	lister := &stubTriggerLister{beads: []*beadsapi.BeadDetail{
		spawnRoleTriggerBead(map[string]string{
			"action":  "notify",
			"project": "monorepo",
			"role":    "should-be-ignored",
		}),
	}}
	got, err := resolveJiraSpawnRole(context.Background(), lister, "monorepo", newSpawnRoleLogger())
	if err == nil {
		t.Fatalf("expected error when only non-spawn triggers match, got role %q", got)
	}
	if got != "" {
		t.Fatalf("got role %q, want empty", got)
	}
}

func TestResolveJiraSpawnRole_ListErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	lister := &stubTriggerLister{err: boom}
	got, err := resolveJiraSpawnRole(context.Background(), lister, "monorepo", newSpawnRoleLogger())
	if err == nil {
		t.Fatalf("expected error when lookup fails, got role %q", got)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped boom error, got %v", err)
	}
	if got != "" {
		t.Fatalf("got role %q, want empty", got)
	}
}

// twoRoleLister returns two enabled spawn_agent triggers for the same project —
// the first (default) plus an alternate operators can select via --role.
func twoRoleLister() *stubTriggerLister {
	return &stubTriggerLister{beads: []*beadsapi.BeadDetail{
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "PE",
			"role":    "pe-crew", // first enabled → default
			"enabled": "true",
		}),
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "PE",
			"role":    "pe-reviewer", // selectable alternate
			"enabled": "true",
		}),
	}}
}

func TestResolveJiraSpawnRoleHinted_EmptyHintUsesDefault(t *testing.T) {
	// preferredRole "" must reproduce the original first-enabled behavior.
	got, err := resolveJiraSpawnRoleHinted(context.Background(), twoRoleLister(), "PE", "", newSpawnRoleLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pe-crew" {
		t.Fatalf("got role %q, want pe-crew (default first-enabled)", got)
	}
}

func TestResolveJiraSpawnRoleHinted_HonorsConfiguredHint(t *testing.T) {
	got, err := resolveJiraSpawnRoleHinted(context.Background(), twoRoleLister(), "PE", "pe-reviewer", newSpawnRoleLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pe-reviewer" {
		t.Fatalf("got role %q, want pe-reviewer (honored hint)", got)
	}
}

func TestResolveJiraSpawnRoleHinted_UnconfiguredHintFallsBackToDefault(t *testing.T) {
	// Deny-by-default: a role the project is not configured to spawn is ignored,
	// falling back to the default rather than widening the surface or erroring.
	got, err := resolveJiraSpawnRoleHinted(context.Background(), twoRoleLister(), "PE", "root", newSpawnRoleLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pe-crew" {
		t.Fatalf("got role %q, want pe-crew (fallback for unconfigured hint)", got)
	}
}

func TestResolveJiraSpawnRoleHinted_HintNeverSelectsDisabledOrOtherProject(t *testing.T) {
	// A hint may only re-select among ENABLED triggers for THIS project — it can
	// never resurrect a disabled trigger or reach into another project's roles.
	lister := &stubTriggerLister{beads: []*beadsapi.BeadDetail{
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "PE",
			"role":    "pe-crew",
			"enabled": "true",
		}),
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "PE",
			"role":    "pe-disabled",
			"enabled": "false",
		}),
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "DEVOPS",
			"role":    "devops-crew",
			"enabled": "true",
		}),
	}}
	// Hint the disabled role → ignored, fall back to default.
	got, err := resolveJiraSpawnRoleHinted(context.Background(), lister, "PE", "pe-disabled", newSpawnRoleLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pe-crew" {
		t.Fatalf("disabled-role hint: got %q, want pe-crew", got)
	}
	// Hint another project's role → ignored, fall back to default.
	got, err = resolveJiraSpawnRoleHinted(context.Background(), lister, "PE", "devops-crew", newSpawnRoleLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pe-crew" {
		t.Fatalf("cross-project-role hint: got %q, want pe-crew", got)
	}
}

func TestResolveJiraSpawnRoleHinted_NoTriggersErrorsEvenWithHint(t *testing.T) {
	// A hint cannot conjure a spawn where the project has no configured trigger.
	lister := &stubTriggerLister{beads: []*beadsapi.BeadDetail{
		spawnRoleTriggerBead(map[string]string{
			"action":  "spawn_agent",
			"project": "DEVOPS",
			"role":    "devops-crew",
			"enabled": "true",
		}),
	}}
	got, err := resolveJiraSpawnRoleHinted(context.Background(), lister, "PE", "pe-reviewer", newSpawnRoleLogger())
	if err == nil {
		t.Fatalf("expected error when project has no trigger, got role %q", got)
	}
	if got != "" {
		t.Fatalf("got role %q, want empty", got)
	}
}
