//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"gardenio/kbeads/internal/model"
)

// TestMigration058_HeraldryColour verifies migration 058 (heraldry spec R13,
// gd-MTOrMbn7kL): rows in the five-token form gain the colour purple, a re-run
// leaves them alone, and down drops the colour except where two live beads of
// one project bear the same places on different colours.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration058
func TestMigration058_HeraldryColour(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}
	st, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m := newTestMigrator(t, db)
	t.Cleanup(func() {
		// Leave the schema at the latest version for the other tests.
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Errorf("restore schema: %v", err)
		}
		if _, err := m.Close(); err != nil {
			t.Logf("migrator close: %v", err)
		}
	})

	ctx := context.Background()
	operatorID := anyOperatorID(t, dsn)
	alpha := "her58-alpha-" + idMust(t)
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.Exec(`DELETE FROM labels WHERE bead_id = $1`, id)
			_, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, id)
		}
	})
	newBead := func() string {
		t.Helper()
		now := time.Now().UTC()
		b := &model.Bead{
			ID: "gd-her58-" + idMust(t), Kind: model.KindIssue, Type: model.TypeEpic,
			Title: "arms", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
			Fleet: "fleet-" + idMust(t), OperatedByID: &operatorID,
		}
		if err := st.CreateBead(ctx, b); err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, b.ID)
		if err := st.AddLabel(ctx, b.ID, "project:"+alpha); err != nil {
			t.Fatalf("add project label: %v", err)
		}
		return b.ID
	}
	set := func(id, arms string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE beads SET heraldry = $2 WHERE id = $1`, id, arms); err != nil {
			t.Fatalf("set heraldry: %v", err)
		}
	}
	get := func(id string) string {
		t.Helper()
		var h sql.NullString
		if err := db.QueryRow(`SELECT heraldry FROM beads WHERE id = $1`, id).Scan(&h); err != nil {
			t.Fatalf("read heraldry: %v", err)
		}
		return h.String
	}
	migrateTo := func(v uint) {
		t.Helper()
		if err := m.Migrate(v); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Fatalf("migrate to %03d: %v", v, err)
		}
	}

	a, b, c := newBead(), newBead(), newBead()

	// Up: five-token rows gain purple; a row with no arms stays without.
	migrateTo(57)
	set(a, "🔥 . 🧭 . 📦")
	set(b, ". . 🎈 . .")
	migrateTo(58)
	if got := get(a); got != "purple 🔥 . 🧭 . 📦" {
		t.Errorf("a after up = %q", got)
	}
	if got := get(b); got != "purple . . 🎈 . ." {
		t.Errorf("b after up = %q", got)
	}
	if got := get(c); got != "" {
		t.Errorf("c (no arms) after up = %q", got)
	}

	// A re-run (a dirty retry) leaves six-token rows alone.
	if err := m.Force(57); err != nil {
		t.Fatalf("force 057: %v", err)
	}
	migrateTo(58)
	if got := get(a); got != "purple 🔥 . 🧭 . 📦" {
		t.Errorf("a after a re-run = %q; want one colour", got)
	}

	// Down: the colour goes, except for the same places on two colours.
	set(a, "blue 🔥 . 🧭 . 📦")
	set(c, "red 🔥 . 🧭 . 📦")
	migrateTo(57)
	if got := get(b); got != ". . 🎈 . ." {
		t.Errorf("b after down = %q", got)
	}
	if got, gotC := get(a), get(c); got != "blue 🔥 . 🧭 . 📦" || gotC != "red 🔥 . 🧭 . 📦" {
		t.Errorf("a, c after down = %q, %q; want their colours kept", got, gotC)
	}
}
