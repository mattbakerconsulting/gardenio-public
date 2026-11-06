package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// labelEventDaemon is a stub daemon for the sweeps' label-event scans. It
// records each events request's query and answers with eventsByID[id] — in
// full, ignoring ?topic=, the way a daemon that predates the filter does — so
// a test covers both the request shape and the client-side Topic check.
func labelEventDaemon(t *testing.T, eventsByID map[string][]map[string]any) (*beadsapi.Client, func() []string) {
	t.Helper()
	var (
		mu      sync.Mutex
		queries []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			mu.Lock()
			queries = append(queries, r.URL.RawQuery)
			mu.Unlock()
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/events")
			_ = json.NewEncoder(w).Encode(map[string]any{"events": eventsByID[id]})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), queries...)
	}
}

// mixedLabelHistory is one bead's history as an old daemon would return it
// for any topic: the intake label added then removed, interleaved with a
// newer bead.updated row and a newer add/remove of an unrelated label.
func mixedLabelHistory(added, removed, newer time.Time) []map[string]any {
	return []map[string]any{
		{"id": 1, "topic": "beads.label.added", "created_at": added.Format(time.RFC3339), "payload": json.RawMessage(`{"label":"needs-intake"}`)},
		{"id": 2, "topic": "beads.label.removed", "created_at": removed.Format(time.RFC3339), "payload": json.RawMessage(`{"label":"needs-intake"}`)},
		{"id": 3, "topic": "beads.bead.updated", "created_at": newer.Format(time.RFC3339), "payload": json.RawMessage(`{"label":"needs-intake"}`)},
		{"id": 4, "topic": "beads.label.added", "created_at": newer.Format(time.RFC3339), "payload": json.RawMessage(`{"label":"blocked"}`)},
		{"id": 5, "topic": "beads.label.removed", "created_at": newer.Format(time.RFC3339), "payload": json.RawMessage(`{"label":"blocked"}`)},
	}
}

// TestLastDeLane_FetchesLabelRemovedTopicOnly pins gd-cP9tODid8j: the orphan
// sweep's de-lane scan asks the daemon for beads.label.removed events only,
// and still picks the intake-label removal when the daemon ignores ?topic=.
func TestLastDeLane_FetchesLabelRemovedTopicOnly(t *testing.T) {
	added := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	removed := added.Add(time.Hour)
	newer := removed.Add(time.Hour)
	client, queries := labelEventDaemon(t, map[string][]map[string]any{
		"gd-a": mixedLabelHistory(added, removed, newer),
	})

	got, err := orphanStore{daemon: client, intakeLabel: "needs-intake"}.LastDeLane(context.Background(), []string{"gd-a"})
	if err != nil {
		t.Fatalf("LastDeLane: %v", err)
	}
	if !got["gd-a"].Equal(removed) {
		t.Errorf("de-lane = %s, want the intake-label removal %s", got["gd-a"], removed)
	}
	q := queries()
	if len(q) != 1 || q[0] != "limit=200&topic=beads.label.removed" {
		t.Errorf("events queries = %q, want one limit=200&topic=beads.label.removed", q)
	}
}

// TestLabelAddedAt_FetchesLabelAddedTopicOnly is the auto-intake twin: only
// beads.label.added events are requested, and the intake-label add wins.
func TestLabelAddedAt_FetchesLabelAddedTopicOnly(t *testing.T) {
	added := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	removed := added.Add(time.Hour)
	newer := removed.Add(time.Hour)
	client, queries := labelEventDaemon(t, map[string][]map[string]any{
		"gd-a": mixedLabelHistory(added, removed, newer),
	})

	got, err := autoIntakeStore{daemon: client, intakeLabel: "needs-intake"}.LabelAddedAt(context.Background(), []string{"gd-a", "gd-none"})
	if err != nil {
		t.Fatalf("LabelAddedAt: %v", err)
	}
	if !got["gd-a"].Equal(added) {
		t.Errorf("label added at = %s, want the intake-label add %s", got["gd-a"], added)
	}
	if _, ok := got["gd-none"]; ok {
		t.Errorf("gd-none has no events; must be absent from the map")
	}
	q := queries()
	if len(q) != 2 || q[0] != "limit=200&topic=beads.label.added" || q[1] != q[0] {
		t.Errorf("events queries = %q, want two limit=200&topic=beads.label.added", q)
	}
}
