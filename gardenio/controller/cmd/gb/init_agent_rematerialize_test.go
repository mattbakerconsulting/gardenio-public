package main

import (
	"context"
	"net/http"
	"os"
	"testing"
)

// beadFieldsHandler serves one agent bead whose fields carry the given
// project/role, so hydrateAssignmentEnv has something to adopt.
func beadFieldsHandler(project, role string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"gd-pool-1","title":"pool member","type":"agent",` +
			`"status":"open","labels":[],"fields":{"project":"` + project + `","role":"` + role + `"}}`))
	})
}

// A generic warm-pool pod runs runSetup (init_agent.go:79), which writes
// ~/.claude/settings.json, BEFORE standbyWait -> hydrateAssignmentEnv
// (init_agent_loop.go:651) sets BOAT_PROJECT/BOAT_ROLE from the assignment.
// Nothing re-runs setup, so a pooled pod's Claude config is permanently
// project-less while gb prime (which reads env at call time) sees the
// specialized project. The files and the prompts disagree.
func TestHydrateAssignmentEnv_ReportsSpecialization(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_ROLE", "")
	t.Setenv("ROLE", "")
	t.Setenv("PROJECT", "")

	_, cleanup := installFakeDaemon(t, beadFieldsHandler("gardenio", "crew"))
	defer cleanup()

	if !hydrateAssignmentEnv(context.Background(), "gd-pool-1") {
		t.Fatal("adopting a concrete project+role must report specialization")
	}
	if got := os.Getenv("BOAT_PROJECT"); got != "gardenio" {
		t.Errorf("BOAT_PROJECT = %q, want gardenio", got)
	}
	if got := os.Getenv("BOAT_ROLE"); got != "crew" {
		t.Errorf("BOAT_ROLE = %q, want crew", got)
	}
}

func TestHydrateAssignmentEnv_NoChangeReportsFalse(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("ROLE", "crew")

	_, cleanup := installFakeDaemon(t, beadFieldsHandler("gardenio", "crew"))
	defer cleanup()

	if hydrateAssignmentEnv(context.Background(), "gd-pool-1") {
		t.Error("an ordinary pod whose env already matches must NOT trigger re-materialization")
	}
}

// The generic sentinel is not a project. Adopting "*" would write
// project:* subscriptions that match nothing.
func TestHydrateAssignmentEnv_SentinelIsNotSpecialization(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_ROLE", "")

	_, cleanup := installFakeDaemon(t, beadFieldsHandler(genericPoolSentinel, genericPoolSentinel))
	defer cleanup()

	if hydrateAssignmentEnv(context.Background(), "gd-pool-1") {
		t.Error("the generic sentinel must not count as specialization")
	}
	if got := os.Getenv("BOAT_PROJECT"); got != "" {
		t.Errorf("BOAT_PROJECT must stay empty, got %q", got)
	}
}

// The role can change at hydration too, so a re-run must use the HYDRATED role.
func TestHydrateAssignmentEnv_RoleChangeAloneSpecializes(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "generic")
	t.Setenv("ROLE", "generic")

	_, cleanup := installFakeDaemon(t, beadFieldsHandler("gardenio", "reviewer"))
	defer cleanup()

	if !hydrateAssignmentEnv(context.Background(), "gd-pool-1") {
		t.Fatal("a role-only change must still report specialization")
	}
	if got := os.Getenv("BOAT_ROLE"); got != "reviewer" {
		t.Errorf("BOAT_ROLE = %q, want reviewer", got)
	}
}

func TestRematerializeAfterSpecialization(t *testing.T) {
	var gotWorkdir, gotRole string
	var calls int
	orig := setupClaudeSettingsFn
	setupClaudeSettingsFn = func(coopWorkdir, role string, mockMode bool) {
		calls++
		gotWorkdir, gotRole = coopWorkdir, role
	}
	defer func() { setupClaudeSettingsFn = orig }()

	// Inject a repo root that is guaranteed to have no <botHome>/gardenio/work
	// checkout under it, rather than relying on the reviewing pod's own
	// filesystem happening to lack one — a pod (or dev machine) whose
	// checkout genuinely lives at /home/agent/bot/gardenio/work would
	// otherwise make resolveCoopWorkdir find a REAL repo and this assertion
	// fail spuriously (gd-5W5zqD7HdR).
	origBotHome := resolveCoopWorkdirBotHome
	resolveCoopWorkdirBotHome = t.TempDir()
	defer func() { resolveCoopWorkdirBotHome = origBotHome }()

	// No repo exists under <botHome>/<project>/work, so resolveCoopWorkdir
	// falls back to the scaffold workspace; give it one.
	cfg := initConfig{role: "generic", project: "", workspace: t.TempDir(), mockMode: true}

	// Not specialized → no rewrite. An ordinary pod must not pay for this.
	rematerializeAfterSpecialization(cfg, false)
	if calls != 0 {
		t.Fatalf("a non-specialized pod must not re-materialize, got %d calls", calls)
	}

	// Specialized → rewrite with the HYDRATED role, not the stale cfg.role.
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "reviewer")
	rematerializeAfterSpecialization(cfg, true)
	if calls != 1 {
		t.Fatalf("a specialized pod must re-materialize exactly once, got %d calls", calls)
	}
	if gotRole != "reviewer" {
		t.Errorf("must use the hydrated BOAT_ROLE, got %q (stale cfg.role was %q)", gotRole, cfg.role)
	}
	if gotWorkdir != cfg.workspace {
		t.Errorf("the workdir must be recomputed via resolveCoopWorkdir (scaffold %q with no project repo), got %q", cfg.workspace, gotWorkdir)
	}
}

func TestRematerializeAfterSpecialization_FallsBackToCfgRole(t *testing.T) {
	var gotRole string
	orig := setupClaudeSettingsFn
	setupClaudeSettingsFn = func(coopWorkdir, role string, mockMode bool) { gotRole = role }
	defer func() { setupClaudeSettingsFn = orig }()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "")

	rematerializeAfterSpecialization(initConfig{role: "crew", mockMode: true}, true)
	if gotRole != "crew" {
		t.Errorf("with no hydrated role, fall back to cfg.role, got %q", gotRole)
	}
}
