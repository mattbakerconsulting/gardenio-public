package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// statAggCols is the full ordered column set returned by the §4.1 superset
// GROUP BY operated_by_id aggregate (ka-0000002003) — open count, the
// in_progress slice, 7d created/closed throughput, MR open/merged-7d, pending
// decisions, active agents/gardeners, and last-active.
var statAggCols = []string{
	"operated_by_id", "open_beads", "in_progress", "created_7d", "closed_7d",
	"mrs_open", "mrs_merged_7d", "decisions_pending", "agents_active",
	"gardeners_active", "last_active",
}

// TestOperatorDirectoryStats pins the two-query shape: a real GROUP BY
// operated_by_id aggregate (the §4.1 superset of COUNT(*) FILTER per-row
// counts + last-active) and a windowed per-operator recent-chips query. Both
// are parsed into the typed result — NO 5000-bead sample (ka-0000001390).
func TestOperatorDirectoryStats(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT operated_by_id,\s+COUNT\(\*\) FILTER \(WHERE status IN \('open','in_progress'\)\)::int AS open_beads,\s+COUNT\(\*\) FILTER \(WHERE status = 'in_progress'\)::int AS in_progress,\s+COUNT\(\*\) FILTER \(WHERE created_at > now\(\) - interval '7 days'\)::int AS created_7d,\s+COUNT\(\*\) FILTER \(WHERE status = 'closed' AND closed_at > now\(\) - interval '7 days'\)::int AS closed_7d,\s+COUNT\(\*\) FILTER \(WHERE type = 'merge-request' AND status IN \('open','in_progress'\)\)::int AS mrs_open,\s+COUNT\(\*\) FILTER \(WHERE type = 'merge-request' AND status = 'closed' AND closed_at > now\(\) - interval '7 days'\)::int AS mrs_merged_7d,\s+COUNT\(\*\) FILTER \(WHERE type = 'decision' AND status = 'open'\)::int AS decisions_pending,\s+COUNT\(\*\) FILTER \(WHERE type = 'agent' AND status IN \('open','in_progress'\)\)::int AS agents_active,\s+COUNT\(\*\) FILTER \(WHERE type = 'trigger' AND status IN \('open','in_progress'\)\)::int AS gardeners_active,\s+MAX\(updated_at\) FILTER \(WHERE type NOT IN \('agent','session','trigger'\)\) AS last_active\s+FROM beads\s+WHERE operated_by_id IS NOT NULL\s+AND deleted_at IS NULL\s+GROUP BY operated_by_id`).
		WillReturnRows(sqlmock.NewRows(statAggCols).
			AddRow(int64(42), 7, 3, 9, 4, 2, 1, 5, 6, 8, t0).
			AddRow(int64(99), 0, 0, 0, 0, 0, 0, 0, 0, 0, t0.Add(-time.Hour)))

	mock.ExpectQuery(`ROW_NUMBER\(\) OVER`).
		WithArgs(5, 5000, 168).
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id", "id", "title", "created_at"}).
			AddRow(int64(42), "gd-a1", "Alice newest", t0).
			AddRow(int64(42), "gd-a2", "Alice older", t0.Add(-time.Minute)).
			AddRow(int64(99), "gd-b1", "Bob only", t0.Add(-time.Hour)))

	got, err := st.OperatorDirectoryStats(context.Background(), 5, 5000, 168)
	if err != nil {
		t.Fatalf("OperatorDirectoryStats: %v", err)
	}
	if len(got.Stats) != 2 {
		t.Fatalf("Stats len = %d, want 2", len(got.Stats))
	}
	s0 := got.Stats[0]
	if s0.OperatorID != 42 || s0.OpenBeads != 7 || !s0.LastActive.Equal(t0) {
		t.Errorf("Stats[0] = %+v, want {42 7 ... %v}", s0, t0)
	}
	if s0.InProgress != 3 || s0.Created7d != 9 || s0.Closed7d != 4 ||
		s0.MRsOpen != 2 || s0.MRsMerged7d != 1 || s0.DecisionsPending != 5 ||
		s0.AgentsActive != 6 || s0.GardenersActive != 8 {
		t.Errorf("Stats[0] counts = %+v, want in_progress=3 created7d=9 closed7d=4 mrs_open=2 mrs_merged7d=1 dec=5 agents=6 gardeners=8", s0)
	}
	if got.Stats[1].OperatorID != 99 || got.Stats[1].OpenBeads != 0 ||
		got.Stats[1].InProgress != 0 || got.Stats[1].GardenersActive != 0 {
		t.Errorf("Stats[1] = %+v, want all-zero counts for 99", got.Stats[1])
	}
	if len(got.Recent) != 3 {
		t.Fatalf("Recent len = %d, want 3", len(got.Recent))
	}
	if got.Recent[0].BeadID != "gd-a1" || got.Recent[0].OperatorID != 42 {
		t.Errorf("Recent[0] = %+v, want gd-a1 / 42", got.Recent[0])
	}
	if got.Recent[2].BeadID != "gd-b1" || got.Recent[2].OperatorID != 99 {
		t.Errorf("Recent[2] = %+v, want gd-b1 / 99", got.Recent[2])
	}
}

// TestOperatorDirectoryStats_DefaultsCapArgs confirms <=0 args fall back to
// the daemon defaults (5 per operator, 5000 ceiling) on the recent-chips
// query.
func TestOperatorDirectoryStats_DefaultsCapArgs(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`GROUP BY operated_by_id`).
		WillReturnRows(sqlmock.NewRows(statAggCols))
	mock.ExpectQuery(`ROW_NUMBER\(\) OVER`).
		WithArgs(defaultOperatorStatsRecentPerOperator, defaultOperatorStatsRecentCeiling, defaultOperatorStatsRecentWindowHours).
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id", "id", "title", "created_at"}))

	if _, err := st.OperatorDirectoryStats(context.Background(), 0, 0, 0); err != nil {
		t.Fatalf("OperatorDirectoryStats: %v", err)
	}
}

// TestOperatorDirectoryStats_AggregateError surfaces a failure on the first
// (aggregate) query without running the second.
func TestOperatorDirectoryStats_AggregateError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	boom := errors.New("boom")
	mock.ExpectQuery(`GROUP BY operated_by_id`).WillReturnError(boom)

	if _, err := st.OperatorDirectoryStats(context.Background(), 5, 5000, 168); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

// TestOperatorDirectoryStats_RecencyExcludesMachineTypes pins the fix for
// /people reporting "active 1m ago" for a human who had done nothing
// (ka-0000001991). The last_active aggregate must screen out the machine bead
// types that heartbeat on their own — type='agent' pods stamp
// live_turns/context_tokens every turn, 'session' rows stamp themselves, and
// 'trigger' gardener loops bump last_fired_at each cycle. Measured on prod
// before the fix, 7 of 15 active humans had a machine bead as their recency
// driver; the worst case rendered ~1.5h for someone whose last real action was
// 5 days earlier.
//
// Two properties matter and are asserted separately:
//
//  1. last_active is a FILTERED max (human-authored types only), and
//  2. it is a FILTER, not a WHERE — agents_active / gardeners_active must still
//     COUNT those very rows. Collapsing this into a WHERE would zero the fleet
//     chips, trading one wrong number for another.
func TestOperatorDirectoryStats_RecencyExcludesMachineTypes(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	// The aggregate must scope ONLY last_active by the human-type screen...
	mock.ExpectQuery(`MAX\(updated_at\) FILTER \(WHERE type NOT IN \('agent','session','trigger'\)\) AS last_active`).
		WillReturnRows(sqlmock.NewRows(statAggCols).
			AddRow(int64(42), 3, 1, 2, 1, 0, 0, 0, 4, 2, time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)))
	mock.ExpectQuery(`ROW_NUMBER\(\) OVER`).
		WithArgs(5, 5000, 24).
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id", "id", "title", "created_at"}))

	got, err := st.OperatorDirectoryStats(context.Background(), 5, 5000, 24)
	if err != nil {
		t.Fatalf("OperatorDirectoryStats: %v", err)
	}
	if len(got.Stats) != 1 {
		t.Fatalf("Stats len = %d, want 1", len(got.Stats))
	}
	// ...while the fleet-footprint counts still see the machine rows. The
	// recency is about the human; the 🤖 chips are about the machines.
	if got.Stats[0].AgentsActive != 4 || got.Stats[0].GardenersActive != 2 {
		t.Errorf("fleet counts = agents %d / gardeners %d, want 4 / 2 — the machine-type screen must be a FILTER on last_active only, never a WHERE on the whole aggregate",
			got.Stats[0].AgentsActive, got.Stats[0].GardenersActive)
	}
}

// TestOperatorDirectoryStats_NullRecencyScansAsZero covers the row shape the
// machine-type FILTER newly makes reachable: an operator whose every bead is
// machine scaffolding matches no row, so Postgres returns NULL for
// last_active. Scanning that into a bare time.Time errors and would fail the
// WHOLE /people aggregate — one such operator would blank the entire
// directory — so it must scan through a NullTime and report the zero time.
func TestOperatorDirectoryStats_NullRecencyScansAsZero(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`GROUP BY operated_by_id`).
		WillReturnRows(sqlmock.NewRows(statAggCols).
			AddRow(int64(42), 0, 0, 0, 0, 0, 0, 0, 1, 0, nil))
	mock.ExpectQuery(`ROW_NUMBER\(\) OVER`).
		WithArgs(5, 5000, 24).
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id", "id", "title", "created_at"}))

	got, err := st.OperatorDirectoryStats(context.Background(), 5, 5000, 24)
	if err != nil {
		t.Fatalf("a NULL last_active must not fail the aggregate: %v", err)
	}
	if len(got.Stats) != 1 {
		t.Fatalf("Stats len = %d, want 1", len(got.Stats))
	}
	if !got.Stats[0].LastActive.IsZero() {
		t.Errorf("LastActive = %v, want zero time for a machine-only operator", got.Stats[0].LastActive)
	}
}

// TestOperatorDirectoryStats_RecentChipsKeyOnOperatedBy pins the fix for the
// recent-activity chip strip being dead fleet-wide (ka-0000001991). The chip
// query MUST key on operated_by_id, not created_by_operator_id: nothing on the
// normal write path populates the latter (only user provisioning does), so
// against prod the old query returned ONE row in total and every other card
// rendered an empty strip. It must also carry the age bound so a months-old
// bead cannot render as current work.
func TestOperatorDirectoryStats_RecentChipsKeyOnOperatedBy(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`GROUP BY operated_by_id`).
		WillReturnRows(sqlmock.NewRows(statAggCols))
	// Partition + filter on operated_by_id, and bound the age via the window
	// arg. created_by_operator_id must not appear anywhere in the chip query.
	mock.ExpectQuery(`SELECT operated_by_id, id, title, created_at\s+FROM \(\s+SELECT operated_by_id, id, title, created_at,\s+ROW_NUMBER\(\) OVER \(\s+PARTITION BY operated_by_id\s+ORDER BY created_at DESC, id DESC\s+\) AS rn\s+FROM beads\s+WHERE operated_by_id IS NOT NULL\s+AND deleted_at IS NULL\s+AND type NOT IN \('agent','session','trigger'\)\s+AND created_at >= now\(\) - \(\$3 \|\| ' hours'\)::interval`).
		WithArgs(5, 5000, 24).
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id", "id", "title", "created_at"}).
			AddRow(int64(42), "gd-a1", "Alice newest", t0))

	got, err := st.OperatorDirectoryStats(context.Background(), 5, 5000, 24)
	if err != nil {
		t.Fatalf("OperatorDirectoryStats: %v", err)
	}
	if len(got.Recent) != 1 || got.Recent[0].OperatorID != 42 || got.Recent[0].BeadID != "gd-a1" {
		t.Errorf("Recent = %+v, want one gd-a1 chip keyed to operator 42", got.Recent)
	}
}
