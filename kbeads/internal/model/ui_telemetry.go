package model

import (
	"encoding/json"
	"time"
)

// UITelemetryRow is one row in ui_telemetry — append-only sink for
// incident-intel click telemetry posted by the static
// incident_intel_telemetry.js module to POST /v1/ui/telemetry. Phase 9 /
// ka-0000006089. The on-the-wire shape mirrors telemetryEvent in beads-viewer
// (event/target/from/path); Metadata is an opaque escape-hatch jsonb.
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

// UITelemetryFilter narrows ListUITelemetry — Event filters by event name
// (empty = all), Since restricts to created_at >= Since (zero = no lower
// bound). Limit caps the page size; the implementation may apply its own
// hard ceiling on top.
type UITelemetryFilter struct {
	Event string
	Since time.Time
	Limit int
}
