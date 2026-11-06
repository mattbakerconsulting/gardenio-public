package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestHandleRecordUITelemetry_PersistsRow(t *testing.T) {
	_, ms, h := newTestServer()
	body := map[string]any{
		"event":       "inbox-rich-report-click",
		"target":      "gd-rep01",
		"from":        "/active",
		"path":        "/gd-rep01",
		"operator_id": "matt@example.com",
	}
	rec := doJSON(t, h, http.MethodPost, "/v1/ui/telemetry", body)
	requireStatus(t, rec, http.StatusNoContent)

	if got, want := len(ms.uiTelemetry), 1; got != want {
		t.Fatalf("uiTelemetry rows = %d, want %d", got, want)
	}
	row := ms.uiTelemetry[0]
	if row.Event != "inbox-rich-report-click" {
		t.Errorf("Event=%q, want inbox-rich-report-click", row.Event)
	}
	if row.Target != "gd-rep01" {
		t.Errorf("Target=%q, want gd-rep01", row.Target)
	}
	if row.OperatorID != "matt@example.com" {
		t.Errorf("OperatorID=%q, want matt@example.com", row.OperatorID)
	}
	if row.ID == 0 {
		t.Error("expected ID assigned by store")
	}
}

func TestHandleRecordUITelemetry_RejectsMissingEvent(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodPost, "/v1/ui/telemetry", map[string]any{
		"target": "gd-x",
	})
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleRecordUITelemetry_RejectsBadJSON(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodPost, "/v1/ui/telemetry", "not-an-object")
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleListUITelemetry_NewestFirst(t *testing.T) {
	_, ms, h := newTestServer()
	for _, ev := range []string{"a", "b", "c"} {
		rec := doJSON(t, h, http.MethodPost, "/v1/ui/telemetry", map[string]any{"event": ev})
		requireStatus(t, rec, http.StatusNoContent)
	}
	if len(ms.uiTelemetry) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(ms.uiTelemetry))
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/ui/telemetry", nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		Rows []*struct {
			Event string `json:"event"`
		} `json:"rows"`
	}
	decodeJSON(t, rec, &resp)
	if got := len(resp.Rows); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
	want := []string{"c", "b", "a"}
	for i, r := range resp.Rows {
		if r.Event != want[i] {
			t.Errorf("rows[%d].Event = %q, want %q", i, r.Event, want[i])
		}
	}
}

func TestHandleListUITelemetry_FiltersEvent(t *testing.T) {
	_, _, h := newTestServer()
	for _, ev := range []string{"alpha", "beta", "alpha"} {
		rec := doJSON(t, h, http.MethodPost, "/v1/ui/telemetry", map[string]any{"event": ev})
		requireStatus(t, rec, http.StatusNoContent)
	}
	rec := doJSON(t, h, http.MethodGet, "/v1/ui/telemetry?event=alpha", nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		Rows []*struct {
			Event string `json:"event"`
		} `json:"rows"`
	}
	decodeJSON(t, rec, &resp)
	if got := len(resp.Rows); got != 2 {
		t.Fatalf("rows=%d want 2", got)
	}
	for _, r := range resp.Rows {
		if r.Event != "alpha" {
			t.Errorf("filter leaked: got %q", r.Event)
		}
	}
}

func TestHandleListUITelemetry_LimitParam(t *testing.T) {
	_, _, h := newTestServer()
	for range 5 {
		rec := doJSON(t, h, http.MethodPost, "/v1/ui/telemetry", map[string]any{"event": "e"})
		requireStatus(t, rec, http.StatusNoContent)
	}
	rec := doJSON(t, h, http.MethodGet, "/v1/ui/telemetry?limit=2", nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		Rows []json.RawMessage `json:"rows"`
	}
	decodeJSON(t, rec, &resp)
	if got := len(resp.Rows); got != 2 {
		t.Errorf("rows=%d want 2", got)
	}
}

func TestHandleListUITelemetry_BadLimit(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/ui/telemetry?limit=oops", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleListUITelemetry_BadSince(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/ui/telemetry?since=not-a-time", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleListUITelemetry_SinceFilter(t *testing.T) {
	_, ms, h := newTestServer()
	now := time.Now()
	// Manually seed two rows with controlled timestamps.
	ms.uiTelemetry = []*model.UITelemetryRow{
		{ID: 1, Event: "old", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: 2, Event: "new", CreatedAt: now.Add(-5 * time.Minute)},
	}
	rec := doJSON(t, h, http.MethodGet, "/v1/ui/telemetry?since="+now.Add(-1*time.Hour).UTC().Format(time.RFC3339), nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		Rows []*struct {
			Event string `json:"event"`
		} `json:"rows"`
	}
	decodeJSON(t, rec, &resp)
	if got := len(resp.Rows); got != 1 {
		t.Fatalf("rows=%d want 1", got)
	}
	if resp.Rows[0].Event != "new" {
		t.Errorf("Event=%q want new", resp.Rows[0].Event)
	}
}
