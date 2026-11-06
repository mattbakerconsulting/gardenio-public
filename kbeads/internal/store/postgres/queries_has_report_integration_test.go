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

// TestHasReportPredicate_RealPostgres exercises the has_report predicate
// (ka-0000007923) against a real Postgres backend. The unit tests in
// postgres_test.go pin the SQL shape via sqlmock; this test pins the
// behavior end-to-end so the EXISTS subquery, soft-delete filter, and
// child-type matching can never silently regress.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the
// default `go test ./...` workflow stays sqlmock-only per CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestHasReportPredicate_RealPostgres
//
// The test scopes its fixtures to a unique fleet and soft-deletes
// everything it created on cleanup so it is safe against a shared DB.
func TestHasReportPredicate_RealPostgres(t *testing.T) {
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
	fleet := "test-has-report-" + suffix

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	// genID returns a fresh gd-id; failure is fatal because every
	// fixture row needs one.
	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}

	// createParentWithChild builds a parent bead and (when childType is
	// non-empty) a child of that type wired by parent-child dep. When
	// softDeleted is true, the child is marked deleted_at = NOW() so the
	// has_report EXISTS predicate must skip it.
	createParentWithChild := func(t *testing.T, title, childType string, softDeleted bool) string {
		t.Helper()
		now := time.Now().UTC()
		parent := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: model.TypeTask,
			Title: title, Status: model.StatusOpen,
			CreatedAt: now, UpdatedAt: now, Fleet: fleet, OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, parent); err != nil {
			t.Fatalf("create parent: %v", err)
		}
		if childType == "" {
			return parent.ID
		}
		child := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: model.BeadType(childType),
			Title: "child of " + title, Status: model.StatusOpen,
			CreatedAt: now, UpdatedAt: now, Fleet: fleet, OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, child); err != nil {
			t.Fatalf("create child: %v", err)
		}
		dep := &model.Dependency{
			BeadID: child.ID, DependsOnID: parent.ID,
			Type: model.DepParentChild, CreatedAt: now, OperatedByID: &operatorID,
		}
		if err := store.AddDependency(ctx, dep); err != nil {
			t.Fatalf("add parent-child dep: %v", err)
		}
		if softDeleted {
			if _, err := store.db.ExecContext(ctx,
				`UPDATE beads SET deleted_at = NOW() WHERE id = $1`, child.ID); err != nil {
				t.Fatalf("soft-delete child: %v", err)
			}
		}
		return parent.ID
	}

	parentReport := createParentWithChild(t, "P-report", "report", false)
	parentRich := createParentWithChild(t, "P-rich", "rich-report", false)
	parentBundle := createParentWithChild(t, "P-bundle", "bundle", false)
	parentTask := createParentWithChild(t, "P-task-only", "task", false)
	parentNoChild := createParentWithChild(t, "P-no-child", "", false)
	parentDeleted := createParentWithChild(t, "P-deleted-report", "report", true)

	// Cleanup: soft-delete every bead we created so reruns stay clean
	// even on a shared DB.
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	listIDs := func(t *testing.T, hasReport *bool) map[string]bool {
		t.Helper()
		filter := model.BeadFilter{
			Fleet: fleet,
			// A non-nil HasReport implicitly enables the has_report
			// projection (see model.BeadFilter doc comment); the nil case
			// needs WithHasReport explicitly or the projection stays the
			// cheap "FALSE AS has_report" placeholder every row gets by
			// default.
			WithHasReport: true,
			HasReport:     hasReport,
			Limit:         100,
		}
		beads, _, err := store.ListBeads(ctx, filter)
		if err != nil {
			t.Fatalf("ListBeads: %v", err)
		}
		got := make(map[string]bool, len(beads))
		for _, b := range beads {
			got[b.ID] = b.HasReport
		}
		return got
	}

	t.Run("nil_filter_returns_all_with_correct_per_row", func(t *testing.T) {
		got := listIDs(t, nil)
		want := map[string]bool{
			parentReport:  true,
			parentRich:    true,
			parentBundle:  true,
			parentTask:    false,
			parentNoChild: false,
			parentDeleted: false, // child is soft-deleted → predicate false
		}
		for id, wantHR := range want {
			gotHR, ok := got[id]
			if !ok {
				t.Errorf("parent %s missing from list", id)
				continue
			}
			if gotHR != wantHR {
				t.Errorf("parent %s: has_report=%v, want %v", id, gotHR, wantHR)
			}
		}
	})

	t.Run("filter_true_includes_only_with_live_report_child", func(t *testing.T) {
		tru := true
		got := listIDs(t, &tru)
		mustContain(t, got, parentReport, parentRich, parentBundle)
		mustNotContain(t, got, parentTask, parentNoChild, parentDeleted)
	})

	t.Run("filter_false_excludes_with_live_report_child", func(t *testing.T) {
		fal := false
		got := listIDs(t, &fal)
		mustContain(t, got, parentTask, parentNoChild, parentDeleted)
		mustNotContain(t, got, parentReport, parentRich, parentBundle)
	})
}

func mustContain(t *testing.T, m map[string]bool, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, ok := m[id]; !ok {
			t.Errorf("expected id %s in result", id)
		}
	}
}

func mustNotContain(t *testing.T, m map[string]bool, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, ok := m[id]; ok {
			t.Errorf("did not expect id %s in result", id)
		}
	}
}
