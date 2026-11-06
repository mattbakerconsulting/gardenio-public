-- ui_telemetry: append-only sink for incident-intel click telemetry posted by
-- POST /v1/ui/telemetry. Phase 9 / ka-0000006089. Distinct from bead_events to
-- keep the bead history feed clean (operator-facing) while keeping a queryable
-- click stream for design / RCA. v1 is intentionally narrow — four columns
-- mirror the wire shape (event/target/from/path), an opaque metadata jsonb
-- escape-hatch covers anything we want to add without another migration, and
-- a created_at index supports `gb telemetry tail`.
-- Idempotent DDL so a partial-apply (table landed but index failed) can be
-- retried without tripping on `relation "ui_telemetry" already exists`.
CREATE TABLE IF NOT EXISTS ui_telemetry (
    id          BIGSERIAL PRIMARY KEY,
    event       TEXT      NOT NULL,
    target      TEXT      NOT NULL DEFAULT '',
    from_path   TEXT      NOT NULL DEFAULT '',
    path        TEXT      NOT NULL DEFAULT '',
    operator_id TEXT      NULL,
    metadata    JSONB     NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ui_telemetry_created_at ON ui_telemetry (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ui_telemetry_event_created_at ON ui_telemetry (event, created_at DESC);
