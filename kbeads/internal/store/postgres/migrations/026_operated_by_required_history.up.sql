-- R2 (ka-0000005772): require operated_by_id on the audit-history tables.
--
-- The dual-write window for live-state tables (beads/comments/deps) closed in
-- migration 017 / 022 with SET NOT NULL. The history tables — events and
-- mutation_history — stayed nullable because the resolver was best-effort and
-- the columns are observability, not transactional.
--
-- R2 changed the server resolver to fail-closed (resolveAuditOperatorRequired
-- now returns ok=false on unattributable requests instead of falling back to
-- the system:legacy-pre-attribution sentinel). Every audit-bearing write path
-- either rejects with InvalidArgument (live-state) or logs+skips the history
-- insert (events / mutation_history). With those skips in place, brand-new
-- history rows ALWAYS carry a non-null operated_by_id.
--
-- We enforce that invariant for new writes via a CHECK constraint with
-- NOT VALID semantics. SET NOT NULL would scan the entire table inside the
-- migration transaction — on a 313K-row prod events table that stalls the
-- daemon for the migration window (see ka-0000006380 post-mortem of migration
-- 022). NOT VALID skips the initial full-table validation scan: existing
-- rows are grandfathered as they stand at migration time.
--
-- CORRECTION (gd-DWM1o3VZAC): "only NEW inserts must satisfy the
-- constraint" above is wrong. Postgres re-checks a NOT VALID CHECK against
-- the NEW tuple on every INSERT *and* every UPDATE of a row — NOT VALID only
-- exempts the migration's own one-time backfill scan, not later writes. An
-- UPDATE that touches a legacy NULL-operated_by_id row on events or
-- mutation_history without also setting operated_by_id still violates this
-- constraint and aborts the transaction (hit by ScrubTaint's purge/strip
-- content cascade, internal/store/postgres/scrub_taint.go, before its fix).
-- Any future writer of these rows must COALESCE(operated_by_id, <resolved
-- id>) rather than assume a bare UPDATE is exempt.
--
-- Migration policy: idempotent DDL only, no eager backfill, runs via
-- `kd migrate` (no daemon startup-probe deadline).

-- events: enforce non-null operated_by_id on new inserts.
ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_operated_by_id_not_null;
ALTER TABLE events
    ADD CONSTRAINT events_operated_by_id_not_null
    CHECK (operated_by_id IS NOT NULL) NOT VALID;

-- mutation_history: enforce non-null operated_by_id on new inserts.
ALTER TABLE mutation_history
    DROP CONSTRAINT IF EXISTS mutation_history_operated_by_id_not_null;
ALTER TABLE mutation_history
    ADD CONSTRAINT mutation_history_operated_by_id_not_null
    CHECK (operated_by_id IS NOT NULL) NOT VALID;
