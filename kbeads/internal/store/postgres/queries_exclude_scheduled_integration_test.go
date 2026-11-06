//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
)

// TestExcludeScheduled_RealPostgres exercises the ExcludeScheduled predicate
// (ka-0000006652, extended by ka-0000007340 and ka-0000007651) against a real
// Postgres backend. The unit tests in postgres_test.go pin the SQL shape via
// sqlmock; this test pins the behavior end-to-end so the three OR predicates
// (schedule_id, multi_fire_key, schedule_fire_key) can never silently regress.
//
// Regression context: per-run multi/schedule *firing* beads are plain
// kind=issue tasks that carry NO schedule_id, so the original
// schedule_id-only predicate let them flood `gb ready` (194/200 rows at
// peak). They are distinguished by:
//   - multi firings   (label source:multi):       fields.multi_fire_key present
//   - schedule firings (label source:schedule):   fields.schedule_fire_key present
//
// ka-0000007651: the predicate was originally keyed off multi_entry_kind='cron',
// which let the event-driven firing variant (multi_entry_kind='bead_event'/
// 'event', still carrying multi_fire_key) leak — 122 flooded `gb ready`. The
// predicate now keys off multi_fire_key presence (the kind-agnostic firing
// marker), so every Multi[...] firing shape is dropped uniformly.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the
// default `go test ./...` workflow stays sqlmock-only per CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestExcludeScheduled_RealPostgres
func TestExcludeScheduled_RealPostgres(t *testing.T) {
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
	fleet := "test-exclude-scheduled-" + suffix

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

	// createBead inserts an open kind=issue/type=task bead with the given
	// JSONB fields, scoped to this test's fleet.
	createBead := func(t *testing.T, title string, fields map[string]string) string {
		t.Helper()
		now := time.Now().UTC()
		var raw json.RawMessage
		if fields != nil {
			b, err := json.Marshal(fields)
			if err != nil {
				t.Fatalf("marshal fields: %v", err)
			}
			raw = b
		}
		b := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: model.TypeTask,
			Title: title, Status: model.StatusOpen,
			CreatedAt: now, UpdatedAt: now, Fleet: fleet,
			Fields: raw, OperatedByID: &operatorID,
		}
		if err := store.CreateBead(ctx, b); err != nil {
			t.Fatalf("create bead %q: %v", title, err)
		}
		return b.ID
	}

	// Genuine claimable work — no scheduler fields. MUST survive.
	genuine := createBead(t, "Genuine claimable task", nil)

	// Legacy scheduled-task bead carrying schedule_id. MUST be dropped.
	scheduleID := createBead(t, "Legacy scheduled task", map[string]string{
		"schedule_id": "gd-some-schedule",
	})

	// Per-run cron firing bead (source:multi). MUST be dropped.
	cronFiring := createBead(t, "[2:2:2026-06-22T23:43] Multi[cron]: gardener X", map[string]string{
		"multi_entry_kind": "cron",
		"multi_fire_key":   "2:2:2026-06-22T23:43",
		"trigger_id":       "gd-some-trigger",
	})

	// Per-run schedule firing bead (source:schedule). MUST be dropped.
	scheduleFiring := createBead(t, "[2026-06-23T00:43] Schedule: gardener Y", map[string]string{
		"schedule_fire_key":  "2026-06-23T00:43",
		"schedule_fire_time": "2026-06-23T00:43:00Z",
		"schedule_cron":      "13,43 * * * *",
		"trigger_id":         "gd-some-trigger",
	})

	// Per-run event-driven firing bead (source:multi, multi_entry_kind=
	// 'bead_event'). ka-0000007651: this is the variant that leaked past the
	// old multi_entry_kind='cron' predicate (122 flooded `gb ready`). It is
	// just as much operational exhaust as the cron firing — carries
	// multi_fire_key, re-spawns on every matched bead event, never claimed —
	// so it MUST now be dropped.
	beadEventFiring := createBead(t, "[0:0:closed:gd-some-bead] Multi[bead_event]: gardener Z", map[string]string{
		"multi_entry_kind": "bead_event",
		"multi_fire_key":   "0:0:closed:gd-some-bead",
		"trigger_id":       "gd-some-trigger",
	})

	// A genuine multi fan-out child carries source:multi context but NO
	// multi_fire_key (it is real work spawned by the molecule, not a firing
	// record). It MUST survive — multi_fire_key, not source:multi membership,
	// is the firing-record marker.
	multiChild := createBead(t, "Multi fan-out child task", map[string]string{
		"multi_entry_kind": "event",
	})

	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	listIDs := func(t *testing.T, exclude bool) map[string]bool {
		t.Helper()
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{
			Fleet:            fleet,
			ExcludeScheduled: exclude,
			Limit:            100,
		})
		if err != nil {
			t.Fatalf("ListBeads: %v", err)
		}
		got := make(map[string]bool, len(beads))
		for _, b := range beads {
			got[b.ID] = true
		}
		return got
	}

	t.Run("exclude_false_returns_all", func(t *testing.T) {
		got := listIDs(t, false)
		mustContain(t, got,
			genuine, scheduleID, cronFiring, scheduleFiring, beadEventFiring, multiChild)
	})

	t.Run("exclude_true_drops_firing_and_scheduled_keeps_genuine", func(t *testing.T) {
		got := listIDs(t, true)
		mustContain(t, got, genuine, multiChild)
		mustNotContain(t, got, scheduleID, cronFiring, scheduleFiring, beadEventFiring)
	})
}
