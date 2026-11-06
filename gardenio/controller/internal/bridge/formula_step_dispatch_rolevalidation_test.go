package bridge

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeRoleLister is a recording stand-in for the type:role catalog the
// dispatcher soft-validates against. roles is the set of backed role names;
// err (if set) makes ListRoleBeads fail so the fail-open path can be exercised.
type fakeRoleLister struct {
	mu    sync.Mutex
	roles map[string]beadsapi.RoleInfo
	err   error
	calls int
}

func newFakeRoleLister(names ...string) *fakeRoleLister {
	roles := make(map[string]beadsapi.RoleInfo, len(names))
	for _, n := range names {
		roles[n] = beadsapi.RoleInfo{Name: n}
	}
	return &fakeRoleLister{roles: roles}
}

func (l *fakeRoleLister) ListRoleBeads(_ context.Context) (map[string]beadsapi.RoleInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return l.roles, nil
}

func (l *fakeRoleLister) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestStepDispatchRoleValidation_UnbackedRoleWarnsButStillSpawns is the core
// contract: a role with no backing type:role bead bumps the role_unbacked facet
// AND still spawns (roles are free strings — soft-warned, never blocked).
func TestStepDispatchRoleValidation_UnbackedRoleWarnsButStillSpawns(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	step := readyStep("gd-fix")
	// Typo the role on both the field (dispatch reads this first) and the label.
	step.Fields["step_role"] = "monrepo-fix"
	step.Labels = []string{"project:monorepo", "role:monrepo-fix"}
	f.beads["gd-fix"] = step
	f.addBlocks("gd-fix", "gd-emit")

	// Catalog backs the correctly-spelled role only.
	lister := newFakeRoleLister("monorepo-fix", "monorepo-publish")

	before := declineCounterValue(t, declineRoleUnbacked)
	NewFormulaStepDispatcher(f, nil, quietLogger()).
		WithRoleValidation(lister).
		OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("unbacked role must STILL spawn (soft-warn only): got %d spawns, want 1", got)
	}
	if got := f.spawnedRoles[0]; got != "monrepo-fix" {
		t.Errorf("spawned role = %q, want the (typo'd) role verbatim — validation must not rewrite it", got)
	}
	if after := declineCounterValue(t, declineRoleUnbacked); after != before+1 {
		t.Errorf("role_unbacked counter = %v, want %v (an unbacked role must record the facet)", after, before+1)
	}
}

// TestStepDispatchRoleValidation_BackedRoleNoWarning proves a role WITH a
// backing role bead does not warn — the live-catalog dispatch roles
// (monorepo-fix / monorepo-publish) must stay quiet.
func TestStepDispatchRoleValidation_BackedRoleNoWarning(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix") // role monorepo-fix
	f.addBlocks("gd-fix", "gd-emit")

	lister := newFakeRoleLister("monorepo-fix", "monorepo-publish")

	before := declineCounterValue(t, declineRoleUnbacked)
	NewFormulaStepDispatcher(f, nil, quietLogger()).
		WithRoleValidation(lister).
		OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("backed role must spawn: got %d spawns, want 1", got)
	}
	if after := declineCounterValue(t, declineRoleUnbacked); after != before {
		t.Errorf("role_unbacked counter moved (%v -> %v) for a BACKED role — false alarm", before, after)
	}
}

// TestStepDispatchRoleValidation_BuiltinRoleNoWarning proves a builtin role
// (crew) never warns even with an empty role catalog — builtins are first-class
// in pod-spawn code and may have no type:role bead.
func TestStepDispatchRoleValidation_BuiltinRoleNoWarning(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	step := readyStep("gd-fix")
	step.Fields["step_role"] = "crew"
	step.Labels = []string{"project:monorepo", "role:crew"}
	f.beads["gd-fix"] = step
	f.addBlocks("gd-fix", "gd-emit")

	lister := newFakeRoleLister() // empty catalog — only builtins keep it quiet

	before := declineCounterValue(t, declineRoleUnbacked)
	NewFormulaStepDispatcher(f, nil, quietLogger()).
		WithRoleValidation(lister).
		OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("builtin role must spawn: got %d spawns, want 1", got)
	}
	if after := declineCounterValue(t, declineRoleUnbacked); after != before {
		t.Errorf("role_unbacked counter moved (%v -> %v) for a BUILTIN role — false alarm", before, after)
	}
	// A builtin short-circuits before ever listing the catalog.
	if lister.callCount() != 0 {
		t.Errorf("builtin role should not list the catalog: got %d ListRoleBeads calls, want 0", lister.callCount())
	}
}

// TestStepDispatchRoleValidation_ListErrorFailsOpen proves a role-catalog
// listing error degrades to no-warning (fail-open) and still spawns — a listing
// hiccup must never make a dispatch louder than a real gap, nor gate a spawn.
func TestStepDispatchRoleValidation_ListErrorFailsOpen(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	step := readyStep("gd-fix")
	step.Fields["step_role"] = "monrepo-fix" // typo'd — would warn if we could list
	step.Labels = []string{"project:monorepo", "role:monrepo-fix"}
	f.beads["gd-fix"] = step
	f.addBlocks("gd-fix", "gd-emit")

	lister := newFakeRoleLister()
	lister.err = errors.New("daemon unreachable")

	before := declineCounterValue(t, declineRoleUnbacked)
	NewFormulaStepDispatcher(f, nil, quietLogger()).
		WithRoleValidation(lister).
		OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("a role-listing error must NOT block the spawn: got %d spawns, want 1", got)
	}
	if after := declineCounterValue(t, declineRoleUnbacked); after != before {
		t.Errorf("role_unbacked counter moved (%v -> %v) on a listing error — must fail open (silent)", before, after)
	}
}

// TestStepDispatchRoleValidation_NilListerDisablesValidation proves the feature
// is opt-in: without WithRoleValidation the dispatcher behaves exactly as before
// and never touches the role_unbacked facet, even for a typo'd role.
func TestStepDispatchRoleValidation_NilListerDisablesValidation(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	step := readyStep("gd-fix")
	step.Fields["step_role"] = "monrepo-fix"
	step.Labels = []string{"project:monorepo", "role:monrepo-fix"}
	f.beads["gd-fix"] = step
	f.addBlocks("gd-fix", "gd-emit")

	before := declineCounterValue(t, declineRoleUnbacked)
	NewFormulaStepDispatcher(f, nil, quietLogger()).
		OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("validation off: must spawn: got %d spawns, want 1", got)
	}
	if after := declineCounterValue(t, declineRoleUnbacked); after != before {
		t.Errorf("role_unbacked counter moved (%v -> %v) with validation OFF", before, after)
	}
}

// TestStepDispatchRoleValidation_CatalogCachedWithinTTL proves a backfill burst
// of many dispatches lists the role catalog at most once within the TTL, rather
// than round-tripping per dispatch.
func TestStepDispatchRoleValidation_CatalogCachedWithinTTL(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	// Three ready steps, all unblocked by the same emit close, all typo'd so each
	// reaches the (post-cache) warn path.
	for _, id := range []string{"gd-a", "gd-b", "gd-c"} {
		s := readyStep(id)
		s.Fields["step_role"] = "monrepo-fix"
		s.Labels = []string{"project:monorepo", "role:monrepo-fix"}
		f.beads[id] = s
		f.addBlocks(id, "gd-emit")
	}

	lister := newFakeRoleLister("monorepo-fix")
	NewFormulaStepDispatcher(f, nil, quietLogger()).
		WithRoleValidation(lister).
		OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 3 {
		t.Fatalf("want 3 spawns across the burst, got %d", got)
	}
	if got := lister.callCount(); got != 1 {
		t.Errorf("role catalog listed %d times across a 3-dispatch burst, want 1 (TTL cache)", got)
	}
}

// TestStepDispatchRoleValidation_StaleCacheReusedOnListError proves a transient
// list error after a good listing reuses the cached catalog rather than failing
// open — validation keeps working across a daemon blip.
func TestStepDispatchRoleValidation_StaleCacheReusedOnListError(t *testing.T) {
	lister := newFakeRoleLister("monorepo-fix")
	d := NewFormulaStepDispatcher(newFakeStepDaemon(), nil, quietLogger()).
		WithRoleValidation(lister)
	// Freeze time so the first catalog stays within TTL for the second lookup.
	frozen := time.Unix(1_700_000_000, 0)
	d.now = func() time.Time { return frozen }

	// Prime the cache with a good listing.
	if roles, ok := d.roleCatalog(context.Background()); !ok || roles == nil {
		t.Fatalf("priming listing should succeed")
	}
	// Now break listing AND advance past the TTL so the cache is stale.
	lister.mu.Lock()
	lister.err = errors.New("blip")
	lister.mu.Unlock()
	d.now = func() time.Time { return frozen.Add(2 * stepRoleCacheTTL) }

	roles, ok := d.roleCatalog(context.Background())
	if !ok {
		t.Fatalf("a stale-but-valid cache must be reused across a list error (ok=false)")
	}
	if _, backed := roles["monorepo-fix"]; !backed {
		t.Errorf("reused catalog lost its entry: monorepo-fix not backed")
	}
}

// TestStepDispatchBuiltinRolesParity is the drift guard: the dispatcher's copy
// of builtin roles MUST equal the controller's authoritative map
// (cmd/controller/project_role_cache.go). The controller's map lives in its own package main
// and cannot be imported, so we parse the keys straight out of its source and
// compare — mirroring gb's TestBuiltinRolesParity. If someone adds a builtin
// role to the controller without updating the dispatcher, an unbacked-role
// warning would fire on a legitimate builtin — this test fails first
// (ka-0000001425).
func TestStepDispatchBuiltinRolesParity(t *testing.T) {
	src, err := os.ReadFile("../../cmd/controller/project_role_cache.go")
	if err != nil {
		t.Fatalf("reading controller project_role_cache.go: %v", err)
	}
	block := regexp.MustCompile(`(?s)var builtinRoles = map\[string\]bool\{(.*?)\}`).FindSubmatch(src)
	if block == nil {
		t.Fatal("could not find builtinRoles map in controller project_role_cache.go")
	}
	keyRe := regexp.MustCompile(`"([^"]+)"\s*:\s*true`)
	want := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(string(block[1]), -1) {
		want[m[1]] = true
	}
	if len(want) == 0 {
		t.Fatal("parsed zero builtin roles from controller source — regex drift?")
	}
	if len(want) != len(stepDispatchBuiltinRoles) {
		t.Fatalf("builtin role count drift: controller=%d dispatcher=%d", len(want), len(stepDispatchBuiltinRoles))
	}
	for r := range want {
		if !stepDispatchBuiltinRoles[r] {
			t.Errorf("controller builtin role %q missing from stepDispatchBuiltinRoles", r)
		}
	}
}
