-- Operator right-to-be-forgotten soft-delete marker (ka-0000004824).
--
-- Customer-1 telemetry plan §4 "Right-to-be-forgotten": handled via
-- soft-delete — the opaque operator_id PERSISTS in historical metric series
-- (those are immutable counters; rewriting them is neither possible nor
-- desired), while the human-identifying fields (display / email) resolve to a
-- redacted placeholder server-side. No metric rewrite is required: the chart
-- backend's server-side operator_id -> display join is the single resolution
-- point, so flipping one column here redacts every dashboard at once.
--
-- We add a nullable forgotten_at timestamp rather than physically clearing
-- display/email so the action is auditable and reversible-by-record (the row
-- is never deleted; the FK from beads/comments/events/... stays intact). The
-- resolver treats `forgotten_at IS NOT NULL` as "redact": email -> the
-- sentinel string "deleted operator", display -> "" so the UI falls back to
-- the same placeholder. The operator_id continues to identify the (now
-- anonymous) actor across historical series, satisfying both the audit trail
-- and the erasure request.
--
-- Migration policy (kbeads/CLAUDE.md): idempotent DDL only, no table backfill.
-- operators is a small table (hundreds of rows in prod); the column add is a
-- metadata-only ALTER. Runs via `kd migrate` with no daemon-startup deadline.

ALTER TABLE operators
    ADD COLUMN IF NOT EXISTS forgotten_at TIMESTAMPTZ;

-- Partial index so the chart backend's "list forgotten operators" / redaction
-- audit path probes only the small forgotten subset, never a full scan.
CREATE INDEX IF NOT EXISTS idx_operators_forgotten_at
    ON operators(forgotten_at)
    WHERE forgotten_at IS NOT NULL;
