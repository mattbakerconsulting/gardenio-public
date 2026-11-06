package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

type globalEventsResponse struct {
	Events     []*model.Event `json:"events"`
	HasMore    bool           `json:"has_more"`
	NextCursor string         `json:"next_cursor"`
}

func seedEvents(ms *mockStore, n int, base time.Time) {
	for i := 1; i <= n; i++ {
		ms.events = append(ms.events, &model.Event{
			ID:        int64(i),
			Topic:     "bead.created",
			BeadID:    "gd-x",
			Actor:     "alice",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
}

func TestHandleListGlobalEvents_HappyPath(t *testing.T) {
	_, ms, h := newTestServer()
	base := time.Now().Add(-1 * time.Hour)
	seedEvents(ms, 3, base)

	rec := doJSON(t, h, http.MethodGet, "/v1/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp globalEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 3 {
		t.Fatalf("events: got %d, want 3", len(resp.Events))
	}
	if resp.HasMore {
		t.Errorf("has_more: got true, want false")
	}
	if resp.NextCursor != "" {
		t.Errorf("next_cursor: got %q, want empty", resp.NextCursor)
	}
	// Chronological order.
	if resp.Events[0].ID != 1 || resp.Events[2].ID != 3 {
		t.Errorf("order: got %d,%d,%d, want 1,2,3",
			resp.Events[0].ID, resp.Events[1].ID, resp.Events[2].ID)
	}
}

func TestHandleListGlobalEvents_CursorPagination(t *testing.T) {
	_, ms, h := newTestServer()
	base := time.Now().Add(-2 * time.Hour)
	seedEvents(ms, 5, base)

	rec := doJSON(t, h, http.MethodGet, "/v1/events?limit=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var page1 globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page1)
	if len(page1.Events) != 2 {
		t.Fatalf("page1 events: got %d, want 2", len(page1.Events))
	}
	if !page1.HasMore {
		t.Errorf("page1 has_more: got false, want true")
	}
	if page1.NextCursor != "2" {
		t.Errorf("page1 next_cursor: got %q, want %q", page1.NextCursor, "2")
	}

	rec = doJSON(t, h, http.MethodGet, "/v1/events?limit=2&cursor="+page1.NextCursor, nil)
	var page2 globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page2)
	if len(page2.Events) != 2 {
		t.Fatalf("page2 events: got %d, want 2", len(page2.Events))
	}
	if page2.Events[0].ID != 3 || page2.Events[1].ID != 4 {
		t.Errorf("page2 ids: got %d,%d, want 3,4", page2.Events[0].ID, page2.Events[1].ID)
	}
	if !page2.HasMore || page2.NextCursor != "4" {
		t.Errorf("page2 has_more/cursor: got %v/%q", page2.HasMore, page2.NextCursor)
	}

	rec = doJSON(t, h, http.MethodGet, "/v1/events?limit=2&cursor="+page2.NextCursor, nil)
	var page3 globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page3)
	if len(page3.Events) != 1 || page3.Events[0].ID != 5 {
		t.Fatalf("page3 events: got %+v, want [5]", page3.Events)
	}
	if page3.HasMore {
		t.Errorf("page3 has_more: got true, want false")
	}
	if page3.NextCursor != "" {
		t.Errorf("page3 next_cursor: got %q, want empty", page3.NextCursor)
	}
}

func TestHandleListGlobalEvents_DescOrder(t *testing.T) {
	_, ms, h := newTestServer()
	base := time.Now().Add(-2 * time.Hour)
	seedEvents(ms, 5, base) // ids 1..5, ascending CreatedAt

	// First desc page: newest two (5, 4), next_cursor=4 (last id on page).
	rec := doJSON(t, h, http.MethodGet, "/v1/events?order=desc&limit=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var page1 globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page1)
	if len(page1.Events) != 2 || page1.Events[0].ID != 5 || page1.Events[1].ID != 4 {
		t.Fatalf("desc page1: got %+v, want ids 5,4", page1.Events)
	}
	if !page1.HasMore || page1.NextCursor != "4" {
		t.Errorf("desc page1 has_more/cursor: got %v/%q, want true/4", page1.HasMore, page1.NextCursor)
	}

	// Paging backward: cursor=4 returns ids strictly < 4, newest-first (3, 2).
	rec = doJSON(t, h, http.MethodGet, "/v1/events?order=desc&limit=2&cursor="+page1.NextCursor, nil)
	var page2 globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page2)
	if len(page2.Events) != 2 || page2.Events[0].ID != 3 || page2.Events[1].ID != 2 {
		t.Fatalf("desc page2: got %+v, want ids 3,2", page2.Events)
	}
	if !page2.HasMore || page2.NextCursor != "2" {
		t.Errorf("desc page2 has_more/cursor: got %v/%q, want true/2", page2.HasMore, page2.NextCursor)
	}

	// Last page: id 1 only, no more.
	rec = doJSON(t, h, http.MethodGet, "/v1/events?order=desc&limit=2&cursor="+page2.NextCursor, nil)
	var page3 globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page3)
	if len(page3.Events) != 1 || page3.Events[0].ID != 1 {
		t.Fatalf("desc page3: got %+v, want [1]", page3.Events)
	}
	if page3.HasMore || page3.NextCursor != "" {
		t.Errorf("desc page3 has_more/cursor: got %v/%q, want false/empty", page3.HasMore, page3.NextCursor)
	}
}

func TestHandleListGlobalEvents_BadOrder(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/events?order=sideways", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("order=sideways: got %d, want 400", rec.Code)
	}
}

func TestHandleListGlobalEvents_TimeRangeFilter(t *testing.T) {
	_, ms, h := newTestServer()
	// Truncate to second precision so RFC3339 round-tripping doesn't drop
	// sub-second nanos and exclude boundary events.
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	seedEvents(ms, 5, base) // events at base+1m..base+5m

	since := base.Add(2 * time.Minute).Format(time.RFC3339)
	until := base.Add(4 * time.Minute).Format(time.RFC3339)
	rec := doJSON(t, h, http.MethodGet, "/v1/events?since="+since+"&until="+until, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 3 {
		t.Fatalf("events: got %d, want 3 (ids 2,3,4)", len(resp.Events))
	}
	if resp.Events[0].ID != 2 || resp.Events[2].ID != 4 {
		t.Errorf("range filter: got ids %d..%d, want 2..4",
			resp.Events[0].ID, resp.Events[len(resp.Events)-1].ID)
	}
}

func TestHandleListGlobalEvents_DefaultSince24h(t *testing.T) {
	_, ms, h := newTestServer()
	now := time.Now()
	// One event 48h ago — outside default window. One event 1h ago — inside.
	ms.events = []*model.Event{
		{ID: 1, Topic: "old", BeadID: "gd-a", CreatedAt: now.Add(-48 * time.Hour)},
		{ID: 2, Topic: "new", BeadID: "gd-b", CreatedAt: now.Add(-1 * time.Hour)},
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 1 || resp.Events[0].ID != 2 {
		t.Fatalf("default since=24h: got %+v, want only id=2", resp.Events)
	}
}

func TestHandleListGlobalEvents_LimitClamp(t *testing.T) {
	_, ms, h := newTestServer()
	seedEvents(ms, 3, time.Now().Add(-1*time.Hour))

	rec := doJSON(t, h, http.MethodGet, "/v1/events?limit=99999", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	// Just verify the request was accepted; clamping is internal.
	var resp globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 3 {
		t.Errorf("events: got %d, want 3", len(resp.Events))
	}
}

func TestHandleListGlobalEvents_InvalidParams(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"invalid since", "/v1/events?since=not-a-time"},
		{"invalid until", "/v1/events?until=tomorrow"},
		{"since after until", "/v1/events?since=2026-04-25T10:00:00Z&until=2026-04-25T09:00:00Z"},
		{"zero limit", "/v1/events?limit=0"},
		{"negative limit", "/v1/events?limit=-1"},
		{"non-numeric limit", "/v1/events?limit=abc"},
		{"non-numeric cursor", "/v1/events?cursor=abc"},
		{"negative cursor", "/v1/events?cursor=-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, h := newTestServer()
			rec := doJSON(t, h, http.MethodGet, tc.path, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleListGlobalEvents_EmptyArrayNotNull(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	// Body should contain "events":[] not "events":null so JS clients can
	// always iterate without a null check.
	body := rec.Body.String()
	if !strings.Contains(body, `"events":[]`) {
		t.Errorf("expected empty events array, got body=%s", body)
	}
}
