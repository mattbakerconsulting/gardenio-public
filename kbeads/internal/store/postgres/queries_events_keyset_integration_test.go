//go:build integration

package postgres

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
)

// TestEventsKeysetPagination_NoDropOrDup_RealPostgres pins, end-to-end against
// a real Postgres, that the global /v1/events keyset pagination returns every
// event in the window exactly once even when events.id order disagrees with
// events.created_at order.
//
// The bug: GetEventsByTimeRange[Desc] paged with a cursor on events.id but
// ORDER BY (created_at, id). events.id is a strictly-monotonic BIGSERIAL;
// events.created_at DEFAULTs to NOW() (transaction-start), so under concurrent
// inserts a row can commit with a later created_at yet a lower id. Sorting by
// created_at while cursoring on id then (a) DROPS a row that sorts after the
// page's last id but has id <= cursor, and (b) DUPLICATES a row that has
// id > cursor but sorts before the page boundary. This test manufactures that
// exact inversion by inserting events with explicit ids and deliberately
// shuffled created_at values, then walks every page and asserts the multiset
// of returned ids equals the inserted set with no repeats.
//
// Gated by //go:build integration and BEADS_TEST_DATABASE_URL so the default
// sqlmock-only `go test ./...` workflow is unaffected (see CLAUDE.md). Run:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestEventsKeysetPagination
func TestEventsKeysetPagination_NoDropOrDup_RealPostgres(t *testing.T) {
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
	fleet := "test-events-keyset-" + suffix

	// One host bead the events hang off (bead_id is a NOT NULL text column).
	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)
	now := time.Now().UTC()
	bead := &model.Bead{
		ID: "gd-" + suffix, Kind: model.KindIssue, Type: model.TypeTask,
		Title: "events-keyset host", Status: model.StatusOpen,
		CreatedAt: now, UpdatedAt: now, Fleet: fleet, OperatedByID: &operatorID,
	}
	if err := store.CreateBead(ctx, bead); err != nil {
		t.Fatalf("create host bead: %v", err)
	}

	// Insert N events with monotonically increasing ids (natural BIGSERIAL
	// order) but created_at values shuffled so that id order and created_at
	// order disagree. We let the sequence assign ids and capture them, then
	// stamp each row's created_at from a permuted schedule inside a shared
	// [base, base+N*step] window. A composite (created_at, id) sort would
	// interleave these ids very differently from a pure id sort — that
	// divergence is exactly what breaks an id-only cursor.
	const n = 40
	base := now.Add(-time.Hour).Truncate(time.Second)
	step := time.Second
	// permuted[k] chooses the created_at slot for the k-th inserted row so the
	// created_at ordering is a non-trivial permutation of the id ordering.
	permuted := func(k int) time.Time {
		// interleave halves: 0,20,1,21,2,22,... — id order != created_at order.
		var slot int
		if k%2 == 0 {
			slot = k / 2
		} else {
			slot = n/2 + k/2
		}
		return base.Add(time.Duration(slot) * step)
	}

	insertedIDs := make([]int64, 0, n)
	for k := 0; k < n; k++ {
		var id int64
		var createdAt time.Time
		// Insert letting BIGSERIAL assign id, then force created_at to the
		// permuted slot so (id, created_at) are deliberately de-correlated.
		err := store.db.QueryRowContext(ctx, `
			INSERT INTO events (topic, bead_id, actor, payload, fleet, created_at, operated_by_id)
			VALUES ('beads.test.keyset', $1, 'tester', '{}'::jsonb, $2, $3, $4)
			RETURNING id, created_at`,
			bead.ID, fleet, permuted(k), operatorID,
		).Scan(&id, &createdAt)
		if err != nil {
			t.Fatalf("insert event %d: %v", k, err)
		}
		insertedIDs = append(insertedIDs, id)
	}
	sort.Slice(insertedIDs, func(i, j int) bool { return insertedIDs[i] < insertedIDs[j] })

	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`DELETE FROM events WHERE fleet = $1`, fleet)
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	// Window spans every inserted created_at (base .. base + N*step) with slack.
	since := base.Add(-time.Minute)
	until := base.Add(time.Duration(n)*step + time.Minute)
	scope := model.EventReadScope{} // unscoped: read every event in the window

	// pageAll walks the feed one small page at a time following next_cursor,
	// exactly as the HTTP handler does (limit+1 to detect has_more), and
	// returns the ordered (created_at, id) sequence it observed.
	pageAll := func(desc bool, pageSize int) []*model.Event {
		var seq []*model.Event
		var cursor int64
		for {
			var evts []*model.Event
			var err error
			if desc {
				evts, err = store.GetEventsByTimeRangeDesc(ctx, since, until, cursor, pageSize+1, scope)
			} else {
				evts, err = store.GetEventsByTimeRange(ctx, since, until, cursor, pageSize+1, scope)
			}
			if err != nil {
				t.Fatalf("page fetch (desc=%v cursor=%d): %v", desc, cursor, err)
			}
			hasMore := len(evts) > pageSize
			if hasMore {
				evts = evts[:pageSize]
			}
			seq = append(seq, evts...)
			if !hasMore || len(evts) == 0 {
				break
			}
			cursor = evts[len(evts)-1].ID
		}
		return seq
	}

	assertExactlyOnce := func(t *testing.T, seq []*model.Event, wantDesc bool) {
		t.Helper()
		if len(seq) != len(insertedIDs) {
			t.Fatalf("paged %d events, want %d (drop or duplicate across page boundaries)",
				len(seq), len(insertedIDs))
		}
		// No duplicates, and the multiset matches the inserted ids exactly.
		seen := make(map[int64]bool, len(seq))
		for _, e := range seq {
			if seen[e.ID] {
				t.Fatalf("event id %d returned more than once (keyset duplicate)", e.ID)
			}
			seen[e.ID] = true
		}
		for _, id := range insertedIDs {
			if !seen[id] {
				t.Fatalf("event id %d never returned on any page (keyset drop)", id)
			}
		}
		// Ordering must be strictly monotonic on the actual sort key,
		// (created_at, id) -- not on id alone. The test deliberately
		// decorrelates id order from created_at order (see permuted above),
		// so an id-only check would fail on a correct implementation too.
		less := func(a, b *model.Event) bool {
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.ID < b.ID
		}
		for i := 1; i < len(seq); i++ {
			prev, cur := seq[i-1], seq[i]
			if wantDesc && !less(cur, prev) {
				t.Fatalf("desc page not strictly descending on (created_at, id) at %d: %v/%d then %v/%d",
					i, prev.CreatedAt, prev.ID, cur.CreatedAt, cur.ID)
			}
			if !wantDesc && !less(prev, cur) {
				t.Fatalf("asc page not strictly ascending on (created_at, id) at %d: %v/%d then %v/%d",
					i, prev.CreatedAt, prev.ID, cur.CreatedAt, cur.ID)
			}
		}
	}

	// Exercise several page sizes so a boundary lands between an inverted
	// (id, created_at) pair regardless of the exact permutation.
	for _, pageSize := range []int{1, 3, 7, n - 1} {
		t.Run("asc", func(t *testing.T) {
			assertExactlyOnce(t, pageAll(false, pageSize), false)
		})
		t.Run("desc", func(t *testing.T) {
			assertExactlyOnce(t, pageAll(true, pageSize), true)
		})
	}
}
