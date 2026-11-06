-- Audit columns for the operator right-to-be-forgotten soft-delete
-- (ka-0000005150). Migration 030 added `forgotten_at` as the redaction marker
-- but the WHO and WHY of a forget were left to raw-SQL tribal knowledge — the
-- 2026-06-16 operator cleanup (ka-0000006421) soft-deleted four rows via
-- `UPDATE operators SET forgotten_at=now(), slug=NULL` with no record of who
-- ran it or why. The `kd admin forget-operator` primitive replaces that raw
-- SQL and records the action as an auditable event on the row itself: these
-- two nullable columns capture the acting principal (server-derived, never
-- client-supplied) and the operator's stated reason.
--
--   - forgotten_by:     the AuthIdentity subject that performed the forget
--                       (an OIDC email, a fleet name, or "static"). NULL for
--                       rows soft-deleted before this migration (migration 030
--                       / the raw-SQL cleanup) — absence means "pre-audit".
--   - forgotten_reason: optional free-text justification supplied to the
--                       command (e.g. a ticket id or GDPR request ref). NULL
--                       when none was given.
--
-- Both are pure metadata columns consumed only on the redaction-audit path;
-- the hot resolver (ResolveOperators) does not read them.
--
-- Migration policy (kbeads/CLAUDE.md): idempotent DDL only, no table backfill.
-- operators is a small table (hundreds of rows in prod); two nullable column
-- adds are metadata-only ALTERs. Runs via `kd migrate` with no daemon-startup
-- deadline.

ALTER TABLE operators
    ADD COLUMN IF NOT EXISTS forgotten_by TEXT;

ALTER TABLE operators
    ADD COLUMN IF NOT EXISTS forgotten_reason TEXT;
