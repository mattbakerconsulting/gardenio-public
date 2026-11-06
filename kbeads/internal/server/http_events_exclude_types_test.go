package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// GET /v1/events?exclude_types= drops events whose bead has a listed type, in
// both orders (gd-Mfr8MRXbYG: trigger-bead mutation events crowded the /feed
// seed's bounded scan).
func TestGlobalEvents_ExcludeTypes(t *testing.T) {
	srv, ms, _ := newTestServer()
	base := time.Now().Add(-time.Hour)
	for id, typ := range map[string]model.BeadType{"gd-task": "task", "gd-trig": "trigger", "gd-agent": "agent"} {
		ms.beads[id] = &model.Bead{ID: id, Title: id, Type: typ, Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
	}
	ms.events = []*model.Event{
		{ID: 1, Topic: "beads.bead.updated", BeadID: "gd-trig", CreatedAt: base.Add(1 * time.Minute)},
		{ID: 2, Topic: "beads.bead.updated", BeadID: "gd-task", CreatedAt: base.Add(2 * time.Minute)},
		{ID: 3, Topic: "beads.bead.updated", BeadID: "gd-agent", CreatedAt: base.Add(3 * time.Minute)},
	}

	for _, tc := range []struct {
		query   string
		wantIDs []int64
	}{
		{"/v1/events", []int64{1, 2, 3}},
		{"/v1/events?exclude_types=trigger", []int64{2, 3}},
		{"/v1/events?exclude_types=trigger,%20agent,", []int64{2}},
		{"/v1/events?order=desc&exclude_types=trigger,agent", []int64{2}},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.query, nil)
		req = req.WithContext(withAuthMethod(req.Context(), authMethodStatic))
		rec := httptest.NewRecorder()
		srv.handleListGlobalEvents(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.query, rec.Code, rec.Body.String())
		}
		var resp globalEventsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, e := range resp.Events {
			ids = append(ids, e.ID)
		}
		if len(ids) != len(tc.wantIDs) {
			t.Fatalf("%s: ids = %v, want %v", tc.query, ids, tc.wantIDs)
		}
		seen := map[int64]bool{}
		for _, id := range ids {
			seen[id] = true
		}
		for _, id := range tc.wantIDs {
			if !seen[id] {
				t.Fatalf("%s: ids = %v, want %v", tc.query, ids, tc.wantIDs)
			}
		}
	}
}
