package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"gardenio/kbeads/internal/model"
)

const (
	defaultUITelemetryLimit = 100
	maxUITelemetryLimit     = 1000
)

// telemetryEventBody is the on-the-wire shape posted by the
// incident-intel JS module (see incident_intel_telemetry.js). The
// beads-viewer telemetry handler forwards the same shape here.
type telemetryEventBody struct {
	Event      string          `json:"event"`
	Target     string          `json:"target,omitempty"`
	From       string          `json:"from,omitempty"`
	Path       string          `json:"path,omitempty"`
	OperatorID string          `json:"operator_id,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// handleRecordUITelemetry handles POST /v1/ui/telemetry. RoleOperator.
// Persists one telemetry event row. Returns 204 on success.
func (s *BeadsServer) handleRecordUITelemetry(w http.ResponseWriter, r *http.Request) {
	var body telemetryEventBody
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.Event == "" {
		writeError(w, http.StatusBadRequest, "event: required")
		return
	}
	row := &model.UITelemetryRow{
		Event:      body.Event,
		Target:     body.Target,
		From:       body.From,
		Path:       body.Path,
		OperatorID: body.OperatorID,
		Metadata:   body.Metadata,
	}
	if err := s.store.RecordUITelemetry(r.Context(), row); err != nil {
		writeServerError(r.Context(), w, "handleRecordUITelemetry", err, "failed to record telemetry")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListUITelemetry handles GET /v1/ui/telemetry. RoleViewer.
// Query params: event (filter), since (RFC3339), limit (default 100, max 1000).
// Response: { "rows": [...] }, newest first.
func (s *BeadsServer) handleListUITelemetry(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := model.UITelemetryFilter{
		Event: q.Get("event"),
		Limit: defaultUITelemetryLimit,
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since: must be an RFC3339 timestamp")
			return
		}
		filter.Since = t
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit: must be a positive integer")
			return
		}
		if n > maxUITelemetryLimit {
			n = maxUITelemetryLimit
		}
		filter.Limit = n
	}
	rows, err := s.store.ListUITelemetry(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleListUITelemetry", err, "failed to list telemetry")
		return
	}
	if rows == nil {
		rows = []*model.UITelemetryRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}
