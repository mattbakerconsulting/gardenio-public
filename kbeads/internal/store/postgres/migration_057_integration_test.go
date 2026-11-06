//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/lib/pq"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// TestMigration057_BeadsHeraldry verifies migration 057 (heraldry spec §3.1,
// gd-pTtoEKamiI): beads.heraldry and beads.project are nullable TEXT columns,
// the register index exists, and down drops all three and up re-adds them.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration057
func TestMigration057_BeadsHeraldry(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	m := newTestMigrator(t, db)
	t.Cleanup(func() {
		if _, err := m.Close(); err != nil {
			t.Logf("migrator close: %v", err)
		}
	})
	if err := m.Migrate(57); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 057: %v", err)
	}
	present := func() bool {
		h, p, i := columnExists(t, db, "beads", "heraldry"), columnExists(t, db, "beads", "project"), indexExists(t, db, "idx_beads_heraldry_register")
		if h != p || p != i {
			t.Fatalf("partial state: heraldry=%v project=%v index=%v", h, p, i)
		}
		return h
	}
	if !present() {
		t.Fatal("057 columns and index missing after up")
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 057: %v", err)
	}
	if present() {
		t.Fatal("057 columns and index still present after down")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-up 057: %v", err)
	}
	if !present() {
		t.Fatal("057 columns and index missing after re-up")
	}
}

// TestHeraldryRegister_RealPostgres exercises the store side of the register:
// arms round-trip through the scanners, beads.project follows the project:
// label on every path that moves it, and the unique index refuses the same
// arms twice in one project (but not across projects, or after a soft delete).
func TestHeraldryRegister_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}
	st, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	operatorID := anyOperatorID(t, dsn)
	suffix := idMust(t)
	alpha, beta := "her-alpha-"+suffix, "her-beta-"+suffix
	const arms = "🔥 . 🧭 . 📦"

	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = st.db.Exec(`DELETE FROM labels WHERE bead_id = $1`, id)
			_, _ = st.db.Exec(`DELETE FROM beads WHERE id = $1`, id)
		}
	})
	newBead := func(heraldry string) *model.Bead {
		t.Helper()
		now := time.Now().UTC()
		b := &model.Bead{
			ID: "gd-her-" + idMust(t), Kind: model.KindIssue, Type: model.TypeTask,
			Title: "arms", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
			Fleet: "fleet-" + idMust(t), OperatedByID: &operatorID, Heraldry: heraldry,
		}
		if err := st.CreateBead(ctx, b); err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, b.ID)
		return b
	}
	project := func(id string) sql.NullString {
		t.Helper()
		var p sql.NullString
		if err := st.db.QueryRow(`SELECT project FROM beads WHERE id = $1`, id).Scan(&p); err != nil {
			t.Fatalf("read project: %v", err)
		}
		return p
	}
	isRegisterViolation := func(err error) bool {
		var pqe *pq.Error
		return errors.As(err, &pqe) && pqe.Code == "23505" && strings.Contains(err.Error(), "idx_beads_heraldry_register")
	}

	// Arms round-trip, and a project label sets beads.project.
	a := newBead(arms)
	if err := st.AddLabel(ctx, a.ID, "project:"+alpha); err != nil {
		t.Fatalf("add project label: %v", err)
	}
	if p := project(a.ID); p.String != alpha {
		t.Fatalf("project = %v; want %s", p, alpha)
	}
	got, err := st.GetBead(ctx, a.ID)
	if err != nil || got.Heraldry != arms {
		t.Fatalf("GetBead heraldry = %q, %v; want %q", got.Heraldry, err, arms)
	}

	// The same arms in the same project are refused; the label never lands.
	// The two beads carry different fleets: the register is per project only.
	b := newBead(arms)
	if err := st.AddLabel(ctx, b.ID, "project:"+alpha); !isRegisterViolation(err) {
		t.Fatalf("same arms in one project: err = %v; want the register 23505", err)
	}
	if labels, _ := st.GetLabels(ctx, b.ID); len(labels) != 0 {
		t.Fatalf("the refused label landed: %v", labels)
	}

	// Another project is another register.
	if err := st.AddLabel(ctx, b.ID, "project:"+beta); err != nil {
		t.Fatalf("same arms in another project: %v", err)
	}

	// Two project labels: no single project. Back to one: the project again.
	if err := st.AddLabel(ctx, b.ID, "project:other-"+suffix); err != nil {
		t.Fatalf("second project label: %v", err)
	}
	if p := project(b.ID); p.Valid {
		t.Fatalf("project with two project labels = %v; want NULL", p)
	}
	if _, err := st.RemoveLabel(ctx, b.ID, "project:other-"+suffix); err != nil {
		t.Fatalf("remove second project label: %v", err)
	}
	if p := project(b.ID); p.String != beta {
		t.Fatalf("project = %v; want %s", p, beta)
	}

	// A move inside a transaction (the reconcileLabels shape) into alpha's
	// register collides, and the whole move rolls back.
	err = st.RunInTransaction(ctx, func(tx store.Store) error {
		if _, err := tx.RemoveLabel(ctx, b.ID, "project:"+beta); err != nil {
			return err
		}
		return tx.AddLabel(ctx, b.ID, "project:"+alpha)
	})
	if !isRegisterViolation(err) {
		t.Fatalf("move into a register holding the arms: err = %v; want the register 23505", err)
	}
	if p := project(b.ID); p.String != beta {
		t.Fatalf("project after the refused move = %v; want %s", p, beta)
	}

	// UpdateBead recomputes project, so a legacy row (NULL) gains it on its
	// next write, and arms set by an update are in the register.
	c := newBead("")
	if err := st.AddLabel(ctx, c.ID, "project:"+alpha); err != nil {
		t.Fatalf("label c: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE beads SET project = NULL WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("simulate legacy row: %v", err)
	}
	c, _ = st.GetBead(ctx, c.ID)
	c.Heraldry = arms
	if err := st.UpdateBead(ctx, c); !isRegisterViolation(err) {
		t.Fatalf("update to arms taken in the project: err = %v; want the register 23505", err)
	}
	c.Heraldry = ". . 🎈 . ."
	if err := st.UpdateBead(ctx, c); err != nil {
		t.Fatalf("update to free arms: %v", err)
	}
	if p := project(c.ID); p.String != alpha {
		t.Fatalf("project after update = %v; want %s", p, alpha)
	}

	// The list filters.
	reg, _, err := st.ListBeads(ctx, model.BeadFilter{HeraldryRegister: alpha, Fleet: model.FleetAll})
	if err != nil {
		t.Fatalf("list register: %v", err)
	}
	if len(reg) != 2 {
		t.Fatalf("register for %s holds %d beads; want 2 (a, c)", alpha, len(reg))
	}
	same, _, err := st.ListBeads(ctx, model.BeadFilter{Heraldry: arms, IDs: []string{a.ID, b.ID, c.ID}, Fleet: model.FleetAll})
	if err != nil {
		t.Fatalf("list by arms: %v", err)
	}
	if len(same) != 2 {
		t.Fatalf("heraldry filter matched %d beads; want 2 (a, b)", len(same))
	}

	// A soft-deleted bead frees its arms.
	if err := st.DeleteBead(ctx, a.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	c.Heraldry = arms
	if err := st.UpdateBead(ctx, c); err != nil {
		t.Fatalf("take the deleted bead's arms: %v", err)
	}
}
