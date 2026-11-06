package postgres

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// TestReadableSlugFromEmail pins the kind-agnostic email → readable-slug
// derivation the op-<id> backfill uses, including the sentinel-domain
// exclusion that keeps machine / un-resynced rows on op-<id>.
func TestReadableSlugFromEmail(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  string
		ok    bool
	}{
		{"oidc-basic", "matthew.baker@acme.example", "matthew.baker", true},
		{"uppercased", "al.kent@acme.example", "al.kent", true},
		{"external-domain", "srivers@contractor-a.example", "srivers", true},
		{"sentinel-slack", "slack-U0OPERATOR1@noreply.kbeads.local", "", false},
		{"sentinel-system", "system-machine@noreply.kbeads.local", "", false},
		{"reserved-localpart", "admin@acme.example", "", false},
		{"reserved-op", "op@acme.example", "", false},
		{"invalid-plus", "alice+work@acme.example", "", false},
		{"no-at", "notanemail", "", false},
		{"leading-at", "@acme.example", "", false},
		{"trailing-at", "alice@", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := readableSlugFromEmail(tc.email)
			if got != tc.want || ok != tc.ok {
				t.Errorf("readableSlugFromEmail(%q) = (%q, %v), want (%q, %v)",
					tc.email, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestBackfillOperatorSlugs_BadBatchSize(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryBackfillOperatorSlugs(context.Background(), db, store.BackfillOperatorSlugsOptions{
		BatchSize: -1,
	})
	if err == nil {
		t.Fatal("expected error for negative batch size")
	}
}

func TestBackfillOperatorSlugs_EmptyCandidates(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(`SELECT id, slug, email\s+FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "email"}))
	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillOperatorSlugs(context.Background(), db, store.BackfillOperatorSlugsOptions{
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeforeOpaque != 0 || stats.AfterOpaque != 0 || stats.Renamed != 0 {
		t.Errorf("counts = before %d after %d renamed %d, want 0/0/0",
			stats.BeforeOpaque, stats.AfterOpaque, stats.Renamed)
	}
}

// TestBackfillOperatorSlugs_DryRun exercises the read-only scan over a mixed
// batch: one human (renamable), one sentinel machine row (left on op-<id>),
// and one whose readable slug is already taken (collision). DryRun performs
// NO writes — only the collision-probe SELECT for the human/collision rows.
func TestBackfillOperatorSlugs_DryRun(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))

	mock.ExpectQuery(`SELECT id, slug, email\s+FROM operators`).
		WithArgs(int64(0), 200).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "email"}).
			AddRow(int64(10), "op-10", "matthew.baker@acme.example").
			AddRow(int64(11), "op-11", "system-machine@noreply.kbeads.local").
			AddRow(int64(12), "op-12", "al.kent@acme.example"))

	// id 10 → readable "matthew.baker", collision probe finds it free.
	mock.ExpectQuery(`SELECT count\(\*\) FROM operator_slug_aliases\s+WHERE slug = \$1 AND operator_id <> \$2`).
		WithArgs("matthew.baker", int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	// id 11 → sentinel email, no probe (machine), skipped in Go.
	// id 12 → readable "al.kent", collision probe finds it TAKEN.
	mock.ExpectQuery(`SELECT count\(\*\) FROM operator_slug_aliases\s+WHERE slug = \$1 AND operator_id <> \$2`).
		WithArgs("al.kent", int64(12)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	// after count
	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))

	stats, err := queryBackfillOperatorSlugs(context.Background(), db, store.BackfillOperatorSlugsOptions{
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Renamed != 1 {
		t.Errorf("Renamed = %d, want 1", stats.Renamed)
	}
	if stats.Machine != 1 {
		t.Errorf("Machine = %d, want 1", stats.Machine)
	}
	if stats.SkippedCollision != 1 {
		t.Errorf("SkippedCollision = %d, want 1", stats.SkippedCollision)
	}
	if !stats.DryRun {
		t.Error("DryRun stat should be true")
	}
}

// TestBackfillOperatorSlugs_RealRename exercises the write path for one human
// survivor: rename operators.slug, seed the new canonical alias, retire the
// old op-<id> alias — all in one transaction — and confirm the old slug is
// preserved as a retired alias (the /u/op-NNN link-preservation invariant).
func TestBackfillOperatorSlugs_RealRename(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	mock.ExpectQuery(`SELECT id, slug, email\s+FROM operators`).
		WithArgs(int64(0), 200).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "email"}).
			AddRow(int64(20), "op-20", "matthew.baker@acme.example"))

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE operators\s+SET slug = \$2, updated_at = NOW\(\)\s+WHERE id = \$1\s+AND slug = \$3`).
		WithArgs(int64(20), "matthew.baker", "op-20").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO operator_slug_aliases \(slug, operator_id, retired_at\)\s+VALUES \(\$1, \$2, NULL\)\s+ON CONFLICT \(slug\) DO NOTHING`).
		WithArgs("matthew.baker", int64(20)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE operator_slug_aliases\s+SET retired_at = COALESCE\(retired_at, NOW\(\)\)\s+WHERE slug = \$1 AND operator_id = \$2`).
		WithArgs("op-20", int64(20)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillOperatorSlugs(context.Background(), db, store.BackfillOperatorSlugsOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Renamed != 1 {
		t.Errorf("Renamed = %d, want 1", stats.Renamed)
	}
	if stats.BeforeOpaque != 1 || stats.AfterOpaque != 0 {
		t.Errorf("opaque counts = before %d after %d, want 1/0", stats.BeforeOpaque, stats.AfterOpaque)
	}
}

// TestBackfillOperatorSlugs_RenameCollisionRollback confirms that when the
// operators.slug UPDATE fires a unique-violation (the readable slug was
// claimed after the free-probe), the rename is a benign skip counted under
// SkippedCollision, not an error.
func TestBackfillOperatorSlugs_RenameCollisionRollback(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	mock.ExpectQuery(`SELECT id, slug, email\s+FROM operators`).
		WithArgs(int64(0), 200).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "email"}).
			AddRow(int64(30), "op-30", "matthew.baker@acme.example"))

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE operators\s+SET slug = \$2, updated_at = NOW\(\)\s+WHERE id = \$1\s+AND slug = \$3`).
		WithArgs(int64(30), "matthew.baker", "op-30").
		WillReturnError(&pq.Error{Code: "23505"})
	mock.ExpectRollback()

	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	stats, err := queryBackfillOperatorSlugs(context.Background(), db, store.BackfillOperatorSlugsOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.SkippedCollision != 1 {
		t.Errorf("SkippedCollision = %d, want 1", stats.SkippedCollision)
	}
	if stats.Renamed != 0 {
		t.Errorf("Renamed = %d, want 0", stats.Renamed)
	}
}

// TestBackfillOperatorSlugs_AlreadyRenamed confirms the idempotent re-run:
// the operators UPDATE affects 0 rows (slug no longer op-<id>), so the
// candidate is a no-op skip with no alias writes.
func TestBackfillOperatorSlugs_AlreadyRenamed(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	mock.ExpectQuery(`SELECT id, slug, email\s+FROM operators`).
		WithArgs(int64(0), 200).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "email"}).
			AddRow(int64(40), "op-40", "matthew.baker@acme.example"))

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE operators\s+SET slug = \$2, updated_at = NOW\(\)\s+WHERE id = \$1\s+AND slug = \$3`).
		WithArgs(int64(40), "matthew.baker", "op-40").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	mock.ExpectQuery(`SELECT count\(\*\) FROM operators`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	stats, err := queryBackfillOperatorSlugs(context.Background(), db, store.BackfillOperatorSlugsOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Renamed != 0 || stats.SkippedCollision != 1 {
		t.Errorf("renamed %d skipped %d, want 0/1", stats.Renamed, stats.SkippedCollision)
	}
}

// TestBackfillOperatorSlugs_Validate pins the defaulting behaviour.
func TestBackfillOperatorSlugs_Validate(t *testing.T) {
	o := store.BackfillOperatorSlugsOptions{}
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if o.BatchSize != 200 {
		t.Errorf("BatchSize default = %d, want 200", o.BatchSize)
	}
	o2 := store.BackfillOperatorSlugsOptions{BatchSize: -5}
	if err := o2.Validate(); err == nil {
		t.Fatal("expected error for negative batch size")
	}
}
