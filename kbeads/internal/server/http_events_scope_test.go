package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// Cross-project read-authz regression tests for the GLOBAL event surface
// GET /v1/events (handleListGlobalEvents), closed in ka-0000006863. It was gated
// ONLY by requireRole(RoleViewer) and returned event rows spanning every bead in
// every project. Event payloads embed the full bead for create/update/close
// topics, so a project-scoped OIDC viewer or project-bound fleet token could read
// title/description/fields for projects it is not authorized to see.
//
// Sibling suite: read_authz_scope_test.go (the by-id / batch / aggregation
// surfaces closed in ka-0000008132). Setup helpers (oidcProjARequest,
// tokenProjARequest, makeUserBead) are shared from there.

// seedProjectEvents seeds one event on a projA bead and one on a projB bead,
// both inside the default 24h window, and returns their event IDs.
func seedProjectEvents(ms *mockStore) (aEventID, bEventID int64) {
	base := time.Now().Add(-1 * time.Hour)
	ms.beads["gd-ev-a"] = &model.Bead{ID: "gd-ev-a", Title: "A", Type: "task", Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
	ms.beads["gd-ev-b"] = &model.Bead{ID: "gd-ev-b", Title: "B", Type: "task", Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
	ms.labels["gd-ev-a"] = []string{"project:projA"}
	ms.labels["gd-ev-b"] = []string{"project:projB"}
	ms.events = []*model.Event{
		{ID: 1, Topic: "beads.bead.created", BeadID: "gd-ev-a", Actor: "alice", CreatedAt: base.Add(1 * time.Minute)},
		{ID: 2, Topic: "beads.bead.created", BeadID: "gd-ev-b", Actor: "bob", CreatedAt: base.Add(2 * time.Minute)},
	}
	return 1, 2
}

// --- GET /v1/events (handleListGlobalEvents) ---

func TestScopeAuthz_GlobalEvents_FiltersCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	_, bEventID := seedProjectEvents(ms)

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/events")
	rec := httptest.NewRecorder()
	srv.handleListGlobalEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp globalEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range resp.Events {
		if e.ID == bEventID || e.BeadID == "gd-ev-b" {
			t.Fatalf("cross-project event %d (bead %s) leaked into /v1/events for projA viewer: %+v", bEventID, e.BeadID, resp.Events)
		}
	}
	if len(resp.Events) != 1 || resp.Events[0].BeadID != "gd-ev-a" {
		t.Fatalf("expected only the in-scope projA event, got %+v", resp.Events)
	}
}

func TestScopeAuthz_GlobalEvents_FiltersCrossProjectToken(t *testing.T) {
	srv, ms, _ := newTestServer()
	_, bEventID := seedProjectEvents(ms)

	req := tokenProjARequest(http.MethodGet, "/v1/events")
	rec := httptest.NewRecorder()
	srv.handleListGlobalEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, e := range resp.Events {
		if e.ID == bEventID || e.BeadID == "gd-ev-b" {
			t.Fatalf("cross-project event %d leaked into /v1/events for projA token: %+v", bEventID, resp.Events)
		}
	}
	if len(resp.Events) != 1 || resp.Events[0].BeadID != "gd-ev-a" {
		t.Fatalf("expected only the in-scope projA event, got %+v", resp.Events)
	}
}

// Desc order takes the sibling store path (GetEventsByTimeRangeDesc); make sure
// the gate applies there too.
func TestScopeAuthz_GlobalEvents_FiltersCrossProjectOIDC_DescOrder(t *testing.T) {
	srv, ms, _ := newTestServer()
	_, bEventID := seedProjectEvents(ms)

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/events?order=desc")
	rec := httptest.NewRecorder()
	srv.handleListGlobalEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, e := range resp.Events {
		if e.ID == bEventID || e.BeadID == "gd-ev-b" {
			t.Fatalf("cross-project event %d leaked into desc /v1/events: %+v", bEventID, resp.Events)
		}
	}
	if len(resp.Events) != 1 || resp.Events[0].BeadID != "gd-ev-a" {
		t.Fatalf("expected only the in-scope projA event, got %+v", resp.Events)
	}
}

// A static/primary token (no project scope) must still see every project.
func TestScopeAuthz_GlobalEvents_StaticTokenUnrestricted(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedProjectEvents(ms)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req = req.WithContext(withAuthMethod(req.Context(), authMethodStatic))
	rec := httptest.NewRecorder()
	srv.handleListGlobalEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp globalEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 2 {
		t.Fatalf("static token should see both projects' events, got %d: %+v", len(resp.Events), resp.Events)
	}
}
