package server

import (
	"encoding/json"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// countByType returns the number of beads of the given type in the mock store.
func countBeadsOfType(ms *mockStore, typ string) int {
	n := 0
	for _, b := range ms.beads {
		if string(b.Type) == typ {
			n++
		}
	}
	return n
}

// TestCreateBead_IdempotencyKey_DedupsWithinProject verifies the core
// lookup-or-return contract: a second create carrying the same key in the same
// project returns the FIRST bead, does not insert a duplicate, and records no
// second create event. (ka-0000002856)
func TestCreateBead_IdempotencyKey_DedupsWithinProject(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	first, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:          "follow-up for run X",
		Type:           "task",
		Labels:         []string{"project:gardenio"},
		IdempotencyKey: "run-X-followup",
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if n := countBeadsOfType(ms, "task"); n != 1 {
		t.Fatalf("after first create: got %d task beads, want 1", n)
	}
	eventsAfterFirst := len(ms.events)
	if eventsAfterFirst == 0 {
		t.Fatalf("first create recorded no events; expected a create event")
	}

	// Second create with the same key + project: must return the existing bead.
	second, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:          "follow-up for run X (retry after crash)",
		Type:           "task",
		Labels:         []string{"project:gardenio"},
		IdempotencyKey: "run-X-followup",
	})
	if err != nil {
		t.Fatalf("second create (dedup): %v", err)
	}
	if second.Bead.Id != first.Bead.Id {
		t.Errorf("dedup returned a new bead: first=%q second=%q", first.Bead.Id, second.Bead.Id)
	}
	if n := countBeadsOfType(ms, "task"); n != 1 {
		t.Errorf("after dedup: got %d task beads, want 1 (no duplicate)", n)
	}
	// Event semantics: the deduped create must NOT emit a second create event.
	if len(ms.events) != eventsAfterFirst {
		t.Errorf("dedup recorded %d new event(s), want 0 (events: %d→%d)",
			len(ms.events)-eventsAfterFirst, eventsAfterFirst, len(ms.events))
	}
	// The returned bead must reflect the ORIGINAL title (the existing bead),
	// not the retried request's title.
	if second.Bead.Title != "follow-up for run X" {
		t.Errorf("dedup returned title=%q, want the original bead's title", second.Bead.Title)
	}
}

// TestCreateBead_IdempotencyKey_PersistedInFields verifies the key is stored in
// Fields so the partial unique index and the lookup query can find it.
func TestCreateBead_IdempotencyKey_PersistedInFields(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:          "keyed",
		Type:           "task",
		Labels:         []string{"project:gardenio"},
		IdempotencyKey: "abc-123",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored := ms.beads[resp.Bead.Id]
	var fm map[string]any
	if err := json.Unmarshal(stored.Fields, &fm); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if got := fm[idempotencyKeyField]; got != "abc-123" {
		t.Errorf("fields[%s]=%v, want abc-123", idempotencyKeyField, got)
	}
}

// TestCreateBead_IdempotencyKey_DistinctKeysCreateDistinctBeads — two different
// keys in the same project are independent creates.
func TestCreateBead_IdempotencyKey_DistinctKeysCreateDistinctBeads(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	a, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "A", Type: "task", Labels: []string{"project:gardenio"}, IdempotencyKey: "key-a",
	})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "B", Type: "task", Labels: []string{"project:gardenio"}, IdempotencyKey: "key-b",
	})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	if a.Bead.Id == b.Bead.Id {
		t.Errorf("distinct keys collapsed to one bead: %q", a.Bead.Id)
	}
	if n := countBeadsOfType(ms, "task"); n != 2 {
		t.Errorf("got %d task beads, want 2", n)
	}
}

// TestCreateBead_IdempotencyKey_PerProjectScope — the SAME key in DIFFERENT
// projects yields two distinct beads. Uniqueness is per-project.
func TestCreateBead_IdempotencyKey_PerProjectScope(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	a, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "in gardenio", Type: "task", Labels: []string{"project:gardenio"}, IdempotencyKey: "shared-key",
	})
	if err != nil {
		t.Fatalf("create in gardenio: %v", err)
	}
	b, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "in other", Type: "task", Labels: []string{"project:other"}, IdempotencyKey: "shared-key",
	})
	if err != nil {
		t.Fatalf("create in other: %v", err)
	}
	if a.Bead.Id == b.Bead.Id {
		t.Errorf("same key in different projects deduped across project boundary: %q", a.Bead.Id)
	}
	if n := countBeadsOfType(ms, "task"); n != 2 {
		t.Errorf("got %d task beads, want 2 (per-project scope)", n)
	}
}

// TestCreateBead_NoIdempotencyKey_AlwaysCreates — omitting the key preserves
// the pre-existing behavior: every create inserts a new bead.
func TestCreateBead_NoIdempotencyKey_AlwaysCreates(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	for i := range 3 {
		if _, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
			Title: "dup", Type: "task", Labels: []string{"project:gardenio"},
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if n := countBeadsOfType(ms, "task"); n != 3 {
		t.Errorf("got %d task beads, want 3 (no dedup without a key)", n)
	}
}

// TestHTTPCreateBead_IdempotencyKey_BodyField — the HTTP layer accepts the key
// as a JSON body field and dedups on a second POST.
func TestHTTPCreateBead_IdempotencyKey_BodyField(t *testing.T) {
	_, ms, h := newTestServerAuth()

	body := map[string]any{
		"title":           "http keyed",
		"type":            "task",
		"labels":          []string{"project:gardenio"},
		"idempotency_key": "http-key-1",
	}
	rec1 := doJSONAs(t, h, "POST", "/v1/beads", body, "alice")
	requireStatus(t, rec1, 201)
	var first model.Bead
	if err := json.Unmarshal(rec1.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	eventsAfterFirst := len(ms.events)

	rec2 := doJSONAs(t, h, "POST", "/v1/beads", body, "alice")
	// Dedup returns the existing bead. The handler still writes 201 (the
	// create path's success status); the contract is "return existing, not
	// error", which 201 with the original bead body satisfies.
	if rec2.Code != 200 && rec2.Code != 201 {
		t.Fatalf("dedup POST status=%d, want 200 or 201 (not an error)", rec2.Code)
	}
	var second model.Bead
	if err := json.Unmarshal(rec2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("HTTP dedup returned new bead: first=%q second=%q", first.ID, second.ID)
	}
	if n := countBeadsOfType(ms, "task"); n != 1 {
		t.Errorf("after HTTP dedup: got %d task beads, want 1", n)
	}
	if len(ms.events) != eventsAfterFirst {
		t.Errorf("HTTP dedup recorded %d new event(s), want 0", len(ms.events)-eventsAfterFirst)
	}
}

// TestHTTPCreateBead_IdempotencyKey_QueryParam — the HTTP layer accepts the key
// as a ?idempotency_key= query param when the body omits it, so a caller can
// keep its body byte-stable and vary the key out-of-band.
func TestHTTPCreateBead_IdempotencyKey_QueryParam(t *testing.T) {
	_, ms, h := newTestServerAuth()

	body := map[string]any{
		"title":  "query keyed",
		"type":   "task",
		"labels": []string{"project:gardenio"},
	}
	rec1 := doJSONAs(t, h, "POST", "/v1/beads?idempotency_key=q-key-1", body, "alice")
	requireStatus(t, rec1, 201)
	var first model.Bead
	if err := json.Unmarshal(rec1.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	rec2 := doJSONAs(t, h, "POST", "/v1/beads?idempotency_key=q-key-1", body, "alice")
	var second model.Bead
	if err := json.Unmarshal(rec2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("query-param dedup returned new bead: first=%q second=%q", first.ID, second.ID)
	}
	if n := countBeadsOfType(ms, "task"); n != 1 {
		t.Errorf("after query-param dedup: got %d task beads, want 1", n)
	}
	// The key was persisted from the query param into Fields.
	stored := ms.beads[first.ID]
	var fm map[string]any
	if err := json.Unmarshal(stored.Fields, &fm); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if got := fm[idempotencyKeyField]; got != "q-key-1" {
		t.Errorf("fields[%s]=%v, want q-key-1", idempotencyKeyField, got)
	}
}
