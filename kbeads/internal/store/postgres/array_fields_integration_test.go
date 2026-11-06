//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// TestArrayFieldPrimitives_RealPostgres exercises the phase_order/step_order
// correctness rail (ka-0000000988) against a real Postgres backend. The sqlmock
// unit tests in array_fields_test.go pin the SQL SHAPE; this test pins the
// actual BEHAVIOR — most importantly the RACE the primitives exist to close:
// a concurrent append + reorder must never clobber one another.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the default
// `go test ./...` workflow stays sqlmock-only per CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestArrayFieldPrimitives_RealPostgres
func TestArrayFieldPrimitives_RealPostgres(t *testing.T) {
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
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-array-fields-" + suffix

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

	newPlan := func(t *testing.T, initial map[string]any) string {
		t.Helper()
		var fields json.RawMessage
		if initial != nil {
			b, err := json.Marshal(initial)
			if err != nil {
				t.Fatalf("marshal initial fields: %v", err)
			}
			fields = b
		}
		b := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: model.TypeEpic,
			Title: "Plan", Status: model.StatusOpen,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			Fleet: fleet, Fields: fields, OperatedByID: &operatorID,
		}
		if err := st.CreateBead(ctx, b); err != nil {
			t.Fatalf("create plan: %v", err)
		}
		return b.ID
	}

	// arrayField reads the current string-array value of a plan's field.
	arrayField := func(t *testing.T, id, field string) []string {
		t.Helper()
		b, err := st.GetBead(ctx, id)
		if err != nil {
			t.Fatalf("get bead: %v", err)
		}
		existing := map[string]any{}
		if len(b.Fields) > 0 {
			if err := json.Unmarshal(b.Fields, &existing); err != nil {
				t.Fatalf("unmarshal fields: %v", err)
			}
		}
		raw, ok := existing[field].([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			out = append(out, v.(string))
		}
		return out
	}

	t.Cleanup(func() {
		_, _ = st.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	t.Run("append grows and is idempotent", func(t *testing.T) {
		id := newPlan(t, nil)

		// First append onto an ABSENT field seeds it from the []-COALESCE default.
		if _, changed, err := st.AppendBeadArrayField(ctx, id, "phase_order", "gd-a"); err != nil || !changed {
			t.Fatalf("append gd-a: changed=%v err=%v", changed, err)
		}
		if _, changed, err := st.AppendBeadArrayField(ctx, id, "phase_order", "gd-b"); err != nil || !changed {
			t.Fatalf("append gd-b: changed=%v err=%v", changed, err)
		}
		// Re-appending an existing element is an idempotent no-op.
		if _, changed, err := st.AppendBeadArrayField(ctx, id, "phase_order", "gd-a"); err != nil || changed {
			t.Fatalf("re-append gd-a: changed=%v err=%v (want changed=false)", changed, err)
		}
		if got := arrayField(t, id, "phase_order"); len(got) != 2 || got[0] != "gd-a" || got[1] != "gd-b" {
			t.Fatalf("phase_order = %v, want [gd-a gd-b]", got)
		}
	})

	t.Run("append missing bead is ErrNoRows", func(t *testing.T) {
		if _, _, err := st.AppendBeadArrayField(ctx, "gd-nonexistent-"+suffix, "phase_order", "gd-x"); err == nil {
			t.Fatal("expected error appending to nonexistent bead, got nil")
		}
	})

	// InsertBeadArrayField is the positional sibling (ka-0000001001): emplace at a
	// chosen slot rather than always the tail. Prove the ordering across head/mid/
	// tail slots, the absent-field seed, and the append-if-absent no-op.
	t.Run("insert emplaces at slot and is append-if-absent", func(t *testing.T) {
		id := newPlan(t, nil)

		// First insert onto an ABSENT field seeds it (slot 0 into []).
		if _, changed, err := st.InsertBeadArrayField(ctx, id, "phase_order", "gd-b", 0); err != nil || !changed {
			t.Fatalf("insert gd-b@0: changed=%v err=%v", changed, err)
		}
		// Head insert: gd-a before gd-b.
		if _, changed, err := st.InsertBeadArrayField(ctx, id, "phase_order", "gd-a", 0); err != nil || !changed {
			t.Fatalf("insert gd-a@0: changed=%v err=%v", changed, err)
		}
		// Mid insert: gd-mid between them.
		if _, changed, err := st.InsertBeadArrayField(ctx, id, "phase_order", "gd-mid", 1); err != nil || !changed {
			t.Fatalf("insert gd-mid@1: changed=%v err=%v", changed, err)
		}
		// index==len is a tail append.
		if _, changed, err := st.InsertBeadArrayField(ctx, id, "phase_order", "gd-tail", 3); err != nil || !changed {
			t.Fatalf("insert gd-tail@3: changed=%v err=%v", changed, err)
		}
		want := []string{"gd-a", "gd-mid", "gd-b", "gd-tail"}
		if got := arrayField(t, id, "phase_order"); !equalSlice(got, want) {
			t.Fatalf("phase_order = %v, want %v", got, want)
		}
		// Re-inserting an existing element is an idempotent no-op — never relocated.
		if _, changed, err := st.InsertBeadArrayField(ctx, id, "phase_order", "gd-b", 0); err != nil || changed {
			t.Fatalf("re-insert gd-b@0: changed=%v err=%v (want changed=false)", changed, err)
		}
		if got := arrayField(t, id, "phase_order"); !equalSlice(got, want) {
			t.Fatalf("no-op insert must not reorder; phase_order = %v, want %v", got, want)
		}
	})

	// The insert must be race-safe under FOR UPDATE just like the append: two
	// concurrent inserts at slot 0 both land (distinct elements), neither clobbers
	// the other, and the final array holds all originals + both newcomers.
	t.Run("concurrent inserts never clobber", func(t *testing.T) {
		for iter := 0; iter < 25; iter++ {
			id := newPlan(t, map[string]any{"phase_order": []string{"gd-1", "gd-2"}})

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				if _, _, err := st.InsertBeadArrayField(ctx, id, "phase_order", "gd-x", 0); err != nil {
					t.Errorf("iter %d insert gd-x: %v", iter, err)
				}
			}()
			go func() {
				defer wg.Done()
				if _, _, err := st.InsertBeadArrayField(ctx, id, "phase_order", "gd-y", 0); err != nil {
					t.Errorf("iter %d insert gd-y: %v", iter, err)
				}
			}()
			wg.Wait()

			got := arrayField(t, id, "phase_order")
			for _, want := range []string{"gd-1", "gd-2", "gd-x", "gd-y"} {
				if !contains(got, want) {
					t.Fatalf("iter %d: concurrent insert dropped %s; phase_order = %v", iter, want, got)
				}
			}
			if len(got) != 4 {
				t.Fatalf("iter %d: phase_order = %v, want 4 distinct elements", iter, got)
			}
		}
	})

	t.Run("CAS swaps on match, conflicts on stale", func(t *testing.T) {
		id := newPlan(t, map[string]any{"phase_order": []string{"gd-a", "gd-b"}})

		// Matching expected → permutation lands.
		if _, err := st.CASBeadArrayField(ctx, id, "phase_order",
			[]string{"gd-a", "gd-b"}, []string{"gd-b", "gd-a"}); err != nil {
			t.Fatalf("cas swap: %v", err)
		}
		if got := arrayField(t, id, "phase_order"); got[0] != "gd-b" || got[1] != "gd-a" {
			t.Fatalf("after swap phase_order = %v, want [gd-b gd-a]", got)
		}

		// Stale expected (still the pre-swap order) → conflict, no write.
		_, err := st.CASBeadArrayField(ctx, id, "phase_order",
			[]string{"gd-a", "gd-b"}, []string{"gd-a", "gd-b"})
		if !errors.Is(err, store.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict on stale expected, got %v", err)
		}
		if got := arrayField(t, id, "phase_order"); got[0] != "gd-b" || got[1] != "gd-a" {
			t.Fatalf("conflict must not write; phase_order = %v, want [gd-b gd-a]", got)
		}
	})

	// The headline race: an append and a reorder run concurrently against the
	// same plan. Under the OLD read-modify-write path the reorder would GET the
	// pre-append array, permute it, and PATCH it back — dropping the appended
	// element. With the CAS rail, the reorder either (a) lands before the append
	// (then the append still succeeds — its element is preserved), or (b) races
	// the append and gets ErrCASConflict, after which the caller re-reads and
	// retries against the fresh array. Either way the appended element must
	// SURVIVE. We loop the retry to model the real caller.
	t.Run("concurrent append + reorder never clobbers", func(t *testing.T) {
		for iter := 0; iter < 25; iter++ {
			id := newPlan(t, map[string]any{"phase_order": []string{"gd-1", "gd-2", "gd-3"}})

			var wg sync.WaitGroup
			wg.Add(2)

			// Writer A: append gd-new.
			go func() {
				defer wg.Done()
				if _, _, err := st.AppendBeadArrayField(ctx, id, "phase_order", "gd-new"); err != nil {
					t.Errorf("iter %d append: %v", iter, err)
				}
			}()

			// Writer B: reverse the first three, retrying on CAS conflict against
			// the fresh array (the real reorder caller's loop).
			go func() {
				defer wg.Done()
				for attempt := 0; attempt < 50; attempt++ {
					cur := arrayField(t, id, "phase_order")
					next := reversedHead3(cur)
					_, err := st.CASBeadArrayField(ctx, id, "phase_order", cur, next)
					if err == nil {
						return
					}
					if errors.Is(err, store.ErrCASConflict) {
						continue // re-read + retry
					}
					t.Errorf("iter %d reorder: %v", iter, err)
					return
				}
				t.Errorf("iter %d reorder: exhausted retries", iter)
			}()

			wg.Wait()

			got := arrayField(t, id, "phase_order")
			if !contains(got, "gd-new") {
				t.Fatalf("iter %d: appended gd-new was clobbered; phase_order = %v", iter, got)
			}
			// The three originals must all still be present (a reorder permutes,
			// never drops).
			for _, want := range []string{"gd-1", "gd-2", "gd-3"} {
				if !contains(got, want) {
					t.Fatalf("iter %d: reorder dropped %s; phase_order = %v", iter, want, got)
				}
			}
			if len(got) != 4 {
				t.Fatalf("iter %d: phase_order = %v, want 4 distinct elements", iter, got)
			}
		}
	})
}

// reversedHead3 reverses the first up-to-3 elements of a, appending the tail
// unchanged. It is a deterministic permutation used by the race test's reorder
// writer.
func reversedHead3(a []string) []string {
	out := append([]string(nil), a...)
	n := 3
	if len(out) < n {
		n = len(out)
	}
	for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func contains(a []string, x string) bool {
	for _, v := range a {
		if v == x {
			return true
		}
	}
	return false
}

// equalSlice reports whether a and b hold the same elements in the same order —
// an ORDERED equality (unlike contains), so the insert test can assert the exact
// emplacement sequence, not just membership.
func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
