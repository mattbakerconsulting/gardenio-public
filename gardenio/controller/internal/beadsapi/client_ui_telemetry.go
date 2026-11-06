package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// UITelemetryRow mirrors model.UITelemetryRow on the daemon side — one row in
// ui_telemetry, the append-only sink for incident-intel click telemetry. The
// wire shape matches what beads-viewer's incident_intel_telemetry.js posts
// (event/target/from/path) plus daemon-assigned id/created_at and the
// optional operator_id/metadata escape-hatches.
type UITelemetryRow struct {
	ID         int64           `json:"id"`
	Event      string          `json:"event"`
	Target     string          `json:"target,omitempty"`
	From       string          `json:"from,omitempty"`
	Path       string          `json:"path,omitempty"`
	OperatorID string          `json:"operator_id,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// UITelemetryFilter narrows ListUITelemetry. Empty Event matches all events;
// zero Since means no lower bound; Limit <= 0 lets the daemon apply its
// default page size.
type UITelemetryFilter struct {
	Event string
	Since time.Time
	Limit int
}

// RecordUITelemetry POSTs one telemetry event to the daemon
// (POST /v1/ui/telemetry, RoleOperator). The daemon assigns id and created_at.
func (c *Client) RecordUITelemetry(ctx context.Context, row *UITelemetryRow) error {
	if row == nil {
		return fmt.Errorf("recording ui telemetry: nil row")
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/ui/telemetry", row, nil); err != nil {
		return fmt.Errorf("recording ui telemetry: %w", err)
	}
	return nil
}

// ListUITelemetry fetches telemetry rows from the daemon
// (GET /v1/ui/telemetry, RoleViewer). Rows are returned newest-first.
func (c *Client) ListUITelemetry(ctx context.Context, filter UITelemetryFilter) ([]*UITelemetryRow, error) {
	q := url.Values{}
	if filter.Event != "" {
		q.Set("event", filter.Event)
	}
	if !filter.Since.IsZero() {
		q.Set("since", filter.Since.UTC().Format(time.RFC3339))
	}
	if filter.Limit > 0 {
		q.Set("limit", strconv.Itoa(filter.Limit))
	}
	path := "/v1/ui/telemetry"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp struct {
		Rows []*UITelemetryRow `json:"rows"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing ui telemetry: %w", err)
	}
	return resp.Rows, nil
}
