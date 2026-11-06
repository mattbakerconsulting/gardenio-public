//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
)

// TestCreateBead_DanglingTypedMention_RealPostgres pins the SAVEPOINT fix
// for ka-0000000195 against a REAL Postgres backend — the only place the bug
// is reachable. The sqlmock unit tests (queries_references_test.go) return
// canned results and do NOT model Postgres transaction-abort semantics
// (SQLSTATE 25P02), so before the fix they showed the FK-`continue` path
// "working" while a real database rolled the entire bead write back.
//
// The bug: bulkInsertTypedDeps inserts each promotion-verb typed mention as
// a deps row on the SHARED bead-write tx. deps.depends_on_id / deps.bead_id
// are NOT-DEFERRABLE `REFERENCES beads(id)` (migration 001), so a dangling
// target raises a 23503 foreign_key_violation that ABORTS the whole tx in
// Postgres — every later statement (and the final COMMIT) errors until
// ROLLBACK. So a bare `continue` on FK error did NOT skip the dangling dep;
// it silently rolled back the bead INSERT and all its bead_references. Any
// `kd create --description "blocks(gd-typo)"` (a forward reference or typo)
// 500'd instead of the documented "skip the dangling dep, keep the bead".
//
// The fix wraps each dep INSERT in a per-statement SAVEPOINT so an FK
// violation rolls back only that INSERT (recovering the tx) and the bead +
// its bead_references + its valid deps all survive.
//
// Gated by `//go:build integration` + BEADS_TEST_DATABASE_URL so the default
// sqlmock-only `go test ./...` workflow is unaffected (per CLAUDE.md). Run:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestCreateBead_DanglingTypedMention_RealPostgres
func TestCreateBead_DanglingTypedMention_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-dangling-dep-" + suffix
	now := time.Now().UTC()

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}

	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	// A real target that exists — its promotion should land a dep row.
	realTarget := genID()
	if err := store.CreateBead(ctx, &model.Bead{
		ID: realTarget, Kind: model.KindIssue, Type: model.TypeEpic,
		Title: "real parent", Status: model.StatusOpen,
		CreatedAt: now, UpdatedAt: now, Fleet: fleet, OperatedByID: &operatorID,
	}); err != nil {
		t.Fatalf("create real target: %v", err)
	}

	// The dangling target id is never created — a forward reference / typo /
	// cross-fleet id. `parent(<dangling>)` would raise the FK violation that
	// used to poison the whole tx.
	danglingTarget := genID()

	// The bead under test carries BOTH a dangling promotion mention and a
	// valid one. Before the fix, the dangling FK violation aborted the tx and
	// the whole CreateBead rolled back (bead + bead_references gone).
	source := genID()
	src := &model.Bead{
		ID: source, Kind: model.KindIssue, Type: model.TypeTask,
		Title:       "task with dangling and valid typed mentions",
		Status:      model.StatusOpen,
		Description: "parent(" + danglingTarget + ") and blocked-by(" + realTarget + ")",
		CreatedAt:   now, UpdatedAt: now, Fleet: fleet, OperatedByID: &operatorID,
	}

	// --- Assertion 1: the create SUCCEEDS despite the dangling FK.
	if err := store.CreateBead(ctx, src); err != nil {
		t.Fatalf("CreateBead with a dangling typed-mention target must succeed "+
			"(dangling dep skipped, bead kept) — got: %v", err)
	}

	// --- Assertion 2: the bead row actually persisted (not rolled back).
	got, err := store.GetBead(ctx, source)
	if err != nil {
		t.Fatalf("GetBead(source): %v", err)
	}
	if got == nil || got.ID != source {
		t.Fatalf("source bead did not persist: %+v", got)
	}

	// --- Assertion 3: bead_references rows survived (both targets, dangling
	// or not — the references table intentionally has NO FK to beads(id)).
	refs, total, err := store.ListBeadReferencesFrom(ctx, source, 100, 0, nil)
	if err != nil {
		t.Fatalf("ListBeadReferencesFrom(source): %v", err)
	}
	if total == 0 {
		t.Fatalf("expected bead_references rows for source, got none")
	}
	sawDangling, sawReal := false, false
	for _, r := range refs {
		switch r.ID {
		case danglingTarget:
			sawDangling = true
		case realTarget:
			sawReal = true
		}
	}
	if !sawDangling {
		t.Errorf("bead_references lost the dangling target %s — the whole write rolled back?", danglingTarget)
	}
	if !sawReal {
		t.Errorf("bead_references lost the real target %s", realTarget)
	}

	// --- Assertion 4: the VALID promotion landed a deps row; the dangling one
	// was skipped. `blocked-by(realTarget)` writes source-as-bead_id depending
	// on realTarget (DepBlocks); `parent(danglingTarget)` would write
	// source-depends-on-dangling but is dropped by the FK skip.
	deps, err := store.GetDependencies(ctx, source)
	if err != nil {
		t.Fatalf("GetDependencies(source): %v", err)
	}
	var sawRealDep bool
	for _, d := range deps {
		if d.DependsOnID == danglingTarget {
			t.Errorf("dangling dep %s should have been skipped, but a deps row exists: %+v", danglingTarget, d)
		}
		if d.DependsOnID == realTarget && d.Type == model.DepBlocks {
			sawRealDep = true
		}
	}
	if !sawRealDep {
		t.Errorf("valid promotion blocked-by(%s) did not land a deps row; got deps=%+v", realTarget, deps)
	}
}
