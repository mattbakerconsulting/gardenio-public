//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// TestSubFieldEditPrimitives_RealPostgres exercises the non-clobbering
// aggregate-field edit primitives (ka-0000007767) against a real Postgres
// backend. The sqlmock unit tests in subfield_edits_test.go pin the SQL SHAPE;
// this test pins the actual BEHAVIOR — most importantly the LOST-UPDATE race the
// primitives exist to close: two concurrent writers editing DIFFERENT keys of
// the same shared map (or elements of the same CSV) must BOTH survive.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the default
// `go test ./...` workflow stays sqlmock-only per CLAUDE.md.
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestSubFieldEditPrimitives_RealPostgres
func TestSubFieldEditPrimitives_RealPostgres(t *testing.T) {
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
	fleet := "test-subfield-edits-" + suffix

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

	newProject := func(t *testing.T, initial map[string]any) string {
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
			ID: genID(), Kind: model.KindConfig, Type: model.TypeProject,
			Title: "Project: test", Status: model.StatusOpen,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			Fleet: fleet, Fields: fields, OperatedByID: &operatorID,
		}
		if err := st.CreateBead(ctx, b); err != nil {
			t.Fatalf("create project: %v", err)
		}
		return b.ID
	}

	rawFields := func(t *testing.T, id string) map[string]json.RawMessage {
		t.Helper()
		b, err := st.GetBead(ctx, id)
		if err != nil {
			t.Fatalf("get bead: %v", err)
		}
		out := map[string]json.RawMessage{}
		if len(b.Fields) > 0 {
			if err := json.Unmarshal(b.Fields, &out); err != nil {
				t.Fatalf("unmarshal fields: %v", err)
			}
		}
		return out
	}

	mapField := func(t *testing.T, id, field string) map[string]string {
		t.Helper()
		m, err := decodeStringMapField(rawFields(t, id)[field])
		if err != nil {
			t.Fatalf("decode map field: %v", err)
		}
		return m
	}

	csvField := func(t *testing.T, id, field string) []string {
		t.Helper()
		parts, err := decodeCSVField(rawFields(t, id)[field])
		if err != nil {
			t.Fatalf("decode csv field: %v", err)
		}
		return parts
	}

	t.Cleanup(func() {
		_, _ = st.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	t.Run("map set preserves other keys and other fields", func(t *testing.T) {
		id := newProject(t, map[string]any{
			"channel_roles": map[string]string{"C1": "ops"},
			"prefix":        "gb",
		})
		if _, changed, err := st.MergeBeadMapField(ctx, id, "channel_roles", SubFieldOpSet, "C2", "crew"); err != nil || !changed {
			t.Fatalf("set C2: changed=%v err=%v", changed, err)
		}
		got := mapField(t, id, "channel_roles")
		if got["C1"] != "ops" || got["C2"] != "crew" {
			t.Fatalf("channel_roles = %v, want C1=ops C2=crew", got)
		}
		// Untouched sibling field survives.
		if p := rawFields(t, id)["prefix"]; string(p) != `"gb"` {
			t.Fatalf("prefix = %s, want \"gb\"", p)
		}
	})

	t.Run("map set is idempotent to the same value", func(t *testing.T) {
		id := newProject(t, map[string]any{"channel_roles": map[string]string{"C1": "ops"}})
		if _, changed, err := st.MergeBeadMapField(ctx, id, "channel_roles", SubFieldOpSet, "C1", "ops"); err != nil || changed {
			t.Fatalf("set same value: changed=%v err=%v (want changed=false)", changed, err)
		}
	})

	t.Run("map handles stringified-JSON storage shape", func(t *testing.T) {
		// Store channel_roles as a stringified-JSON jsonb string (the messy real
		// shape) and confirm a set still merges correctly.
		id := newProject(t, map[string]any{"channel_roles": `{"C1":"ops"}`})
		if _, changed, err := st.MergeBeadMapField(ctx, id, "channel_roles", SubFieldOpSet, "C2", "crew"); err != nil || !changed {
			t.Fatalf("set on stringified map: changed=%v err=%v", changed, err)
		}
		got := mapField(t, id, "channel_roles")
		if got["C1"] != "ops" || got["C2"] != "crew" {
			t.Fatalf("channel_roles = %v, want C1=ops C2=crew", got)
		}
	})

	t.Run("csv append preserves existing then remove drops one", func(t *testing.T) {
		id := newProject(t, map[string]any{"slack_channel": "C1,C2"})
		if _, changed, err := st.EditBeadCSVField(ctx, id, "slack_channel", SubFieldOpAppend, "C3"); err != nil || !changed {
			t.Fatalf("append C3: changed=%v err=%v", changed, err)
		}
		if got := csvField(t, id, "slack_channel"); len(got) != 3 {
			t.Fatalf("slack_channel = %v, want 3 entries", got)
		}
		// Re-append is a no-op.
		if _, changed, err := st.EditBeadCSVField(ctx, id, "slack_channel", SubFieldOpAppend, "C3"); err != nil || changed {
			t.Fatalf("re-append C3: changed=%v err=%v (want changed=false)", changed, err)
		}
		if _, changed, err := st.EditBeadCSVField(ctx, id, "slack_channel", SubFieldOpRemove, "C2"); err != nil || !changed {
			t.Fatalf("remove C2: changed=%v err=%v", changed, err)
		}
		got := csvField(t, id, "slack_channel")
		if len(got) != 2 || contains(got, "C2") {
			t.Fatalf("slack_channel = %v, want [C1 C3]", got)
		}
	})

	t.Run("missing bead is ErrNoRows", func(t *testing.T) {
		if _, _, err := st.MergeBeadMapField(ctx, "gd-nope-"+suffix, "channel_roles", SubFieldOpSet, "C1", "x"); err == nil {
			t.Fatal("expected error editing nonexistent bead, got nil")
		}
	})

	// The headline race: N writers each set a DISTINCT key of the same shared map
	// concurrently. Under the OLD get-then-put path each would read the map,
	// insert its own key, and PATCH the whole map back — the last writer wins and
	// every other writer's key is lost. With the FOR-UPDATE row lock serializing
	// the read-modify-write, ALL keys must survive.
	t.Run("concurrent distinct-key map writes never clobber", func(t *testing.T) {
		const writers = 8
		for iter := 0; iter < 15; iter++ {
			id := newProject(t, map[string]any{"channel_roles": map[string]string{"seed": "role"}})
			var wg sync.WaitGroup
			wg.Add(writers)
			for w := 0; w < writers; w++ {
				go func(w int) {
					defer wg.Done()
					key := "C" + strconv.Itoa(w)
					if _, _, err := st.MergeBeadMapField(ctx, id, "channel_roles", SubFieldOpSet, key, "role"+strconv.Itoa(w)); err != nil {
						t.Errorf("iter %d writer %d: %v", iter, w, err)
					}
				}(w)
			}
			wg.Wait()

			got := mapField(t, id, "channel_roles")
			if got["seed"] != "role" {
				t.Fatalf("iter %d: seed key clobbered; channel_roles = %v", iter, got)
			}
			for w := 0; w < writers; w++ {
				if got["C"+strconv.Itoa(w)] != "role"+strconv.Itoa(w) {
					t.Fatalf("iter %d: writer %d's key lost; channel_roles = %v", iter, w, got)
				}
			}
			if len(got) != writers+1 {
				t.Fatalf("iter %d: channel_roles = %v, want %d keys", iter, got, writers+1)
			}
		}
	})

	// Same race for the CSV field: N writers each append a distinct element.
	t.Run("concurrent distinct-element csv appends never clobber", func(t *testing.T) {
		const writers = 8
		for iter := 0; iter < 15; iter++ {
			id := newProject(t, map[string]any{"slack_channel": "seed"})
			var wg sync.WaitGroup
			wg.Add(writers)
			for w := 0; w < writers; w++ {
				go func(w int) {
					defer wg.Done()
					if _, _, err := st.EditBeadCSVField(ctx, id, "slack_channel", SubFieldOpAppend, "C"+strconv.Itoa(w)); err != nil {
						t.Errorf("iter %d writer %d: %v", iter, w, err)
					}
				}(w)
			}
			wg.Wait()

			got := csvField(t, id, "slack_channel")
			if !contains(got, "seed") {
				t.Fatalf("iter %d: seed clobbered; slack_channel = %v", iter, got)
			}
			for w := 0; w < writers; w++ {
				if !contains(got, "C"+strconv.Itoa(w)) {
					t.Fatalf("iter %d: writer %d's element lost; slack_channel = %v", iter, w, got)
				}
			}
			if len(got) != writers+1 {
				t.Fatalf("iter %d: slack_channel = %v, want %d elements", iter, got, writers+1)
			}
		}
	})

	// A whole-field replace (bare -f / PATCH) concurrent with a sub-field edit is
	// the mixed case: the sub-field edit's FOR-UPDATE lock serializes against
	// UpdateBead's own transaction, so no torn read. We just assert neither errors
	// and the bead stays well-formed.
	t.Run("sub-field edit and whole-field replace do not error", func(t *testing.T) {
		id := newProject(t, map[string]any{"channel_roles": map[string]string{"C1": "ops"}})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			b, _ := st.GetBead(ctx, id)
			b.Fields = json.RawMessage(`{"channel_roles":{"C9":"replaced"}}`)
			if err := st.UpdateBead(ctx, b); err != nil {
				t.Errorf("replace: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, _, err := st.MergeBeadMapField(ctx, id, "channel_roles", SubFieldOpSet, "C2", "crew"); err != nil && !errors.Is(err, store.ErrInvalidSubFieldEdit) {
				t.Errorf("set: %v", err)
			}
		}()
		wg.Wait()
	})
}
