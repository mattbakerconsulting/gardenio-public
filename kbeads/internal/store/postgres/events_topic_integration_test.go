//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// TestGetEventsByTopic_RealPostgres pins queryGetEventsByTopic (?topic= on GET
// /v1/beads/{id}/events, gd-cP9tODid8j) against a real Postgres: only the
// bead's events of that topic, the newest `limit` of them, chronological, and
// limit <= 0 binding a NULL LIMIT that Postgres reads as LIMIT ALL.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestGetEventsByTopic_RealPostgres
func TestGetEventsByTopic_RealPostgres(t *testing.T) {
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
	now := time.Now().UTC()
	id, other := genTestID(t), genTestID(t)
	for _, bid := range []string{id, other} {
		if err := st.CreateBead(ctx, &model.Bead{
			ID: bid, Kind: model.KindIssue, Type: model.TypeTask,
			Title: "events by topic", Status: model.StatusOpen,
			CreatedAt: now, UpdatedAt: now, Fleet: "test-events-topic", OperatedByID: &operatorID,
		}); err != nil {
			t.Fatalf("create bead: %v", err)
		}
	}

	record := func(bid, topic, label string) int64 {
		t.Helper()
		e := &model.Event{Topic: topic, BeadID: bid, Actor: "tester", Payload: json.RawMessage(`{"label":"` + label + `"}`), OperatedByID: &operatorID}
		if err := st.RecordEvent(ctx, e); err != nil {
			t.Fatalf("record event: %v", err)
		}
		return e.ID
	}
	r1 := record(id, "beads.label.removed", "a")
	record(id, "beads.bead.updated", "")
	r2 := record(id, "beads.label.removed", "b")
	record(id, "beads.label.added", "a")
	r3 := record(id, "beads.label.removed", "c")
	record(other, "beads.label.removed", "a")

	ids := func(evts []*model.Event) []int64 {
		out := make([]int64, 0, len(evts))
		for _, e := range evts {
			if e.BeadID != id || e.Topic != "beads.label.removed" {
				t.Errorf("unexpected event %+v", e)
			}
			out = append(out, e.ID)
		}
		return out
	}
	same := func(a, b []int64) bool {
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

	all, err := st.GetEventsByTopic(ctx, id, "beads.label.removed", 0)
	if err != nil {
		t.Fatalf("GetEventsByTopic (no limit): %v", err)
	}
	if got, want := ids(all), []int64{r1, r2, r3}; !same(got, want) {
		t.Errorf("no limit: ids = %v, want %v", got, want)
	}

	two, err := st.GetEventsByTopic(ctx, id, "beads.label.removed", 2)
	if err != nil {
		t.Fatalf("GetEventsByTopic (limit 2): %v", err)
	}
	if got, want := ids(two), []int64{r2, r3}; !same(got, want) {
		t.Errorf("limit 2: ids = %v, want the newest two %v", got, want)
	}
}
