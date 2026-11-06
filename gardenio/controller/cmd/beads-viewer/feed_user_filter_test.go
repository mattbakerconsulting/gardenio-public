package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// feed_user_filter_test.go — covers the user-filter dropdown (gd-vBlxwRfgYd):
// the feed page offers a dropdown of operators seen in the recent window,
// sorted by activity recency, and the selected user pins the feed to that
// operator's activity.

// feedEventDataOp builds the SSE wrapper payload like feedEventData but stamps
// the operator identity (operated_by / operated_by_email) used by the user
// dropdown + client-side scoping.
func feedEventDataOp(t *testing.T, id, typ, title, status, operatedBy, operatedByEmail string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id":                id,
			"type":              typ,
			"title":             title,
			"status":            status,
			"labels":            []string{"project:gardenio"},
			"operated_by":       operatedBy,
			"operated_by_email": operatedByEmail,
		},
	})
	if err != nil {
		t.Fatalf("marshal feed event: %v", err)
	}
	return data
}

// TestFeedSeedCardsAndUsers_SortedByRecency confirms the seed scan harvests the
// distinct operators and returns them most-recently-active first, preferring
// the resolved email as the key/label and falling back to the raw operator id.
func TestFeedSeedCardsAndUsers_SortedByRecency(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		// Durable log is oldest-first.
		Events: []beadsapi.Event{
			// alice acts first, then again later — her later time wins.
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventDataOp(t, "gd-a", "task", "a", "open", "oidc:alice", "alice@x.io")},
			{ID: 2, Topic: "beads.bead.updated", CreatedAt: t0.Add(1 * time.Minute), Payload: feedEventDataOp(t, "gd-b", "task", "b", "in_progress", "slack:UBOB", "")},
			{ID: 3, Topic: "beads.bead.updated", CreatedAt: t0.Add(2 * time.Minute), Payload: feedEventDataOp(t, "gd-a", "task", "a", "in_progress", "oidc:alice", "alice@x.io")},
			// An event with no operator identity must not produce a dropdown entry.
			{ID: 4, Topic: "beads.bead.created", CreatedAt: t0.Add(3 * time.Minute), Payload: feedEventData(t, "gd-c", "task", "c", "open")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	_, users := srv.feedSeedCardsAndUsers(context.Background(), 30)
	if len(users) != 2 {
		t.Fatalf("expected 2 distinct operators (anonymous event excluded), got %d: %+v", len(users), users)
	}
	// alice's most-recent activity (05:02) is after bob's (05:01), so alice leads.
	if users[0].Key != "alice@x.io" {
		t.Errorf("expected alice first (most recent), got %q", users[0].Key)
	}
	if !users[0].LastActive.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("alice LastActive = %v, want the later (05:02) event time", users[0].LastActive)
	}
	// bob has no email, so the raw operator id is the key + label, lower-cased.
	if users[1].Key != "slack:ubob" || users[1].Label != "slack:UBOB" {
		t.Errorf("bob key/label = %q/%q, want raw operator id key lower-cased", users[1].Key, users[1].Label)
	}
}

// TestHandleFeed_RendersUserDropdown drives GET /feed and asserts the user
// dropdown and its options render, with the selected user reflected.
func TestHandleFeed_RendersUserDropdown(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventDataOp(t, "gd-a", "task", "a", "open", "oidc:alice", "alice@x.io")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/feed?user=alice@x.io", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="feed-user"`,           // the dropdown
		`<option value="">`,           // "all users" sentinel
		`value="alice@x.io" selected`, // selected user
		`data-user="alice@x.io"`,      // client seed attribute
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/feed page missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleFeed_SelectedUserNotInWindow confirms an explicit ?user= that isn't
// in the recent seed window is still offered as a (selected) option so a shared
// or reloaded link keeps its selection.
func TestHandleFeed_SelectedUserNotInWindow(t *testing.T) {
	fake := newFake() // no events -> empty harvested user set
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/feed?user=ghost@x.io", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="ghost@x.io" selected`) {
		t.Errorf("an out-of-window selected user should still render as a selected option\nbody=%s", body)
	}
}
