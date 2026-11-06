-- Seeds the single, narrowly-scoped `system:machine` operator + canonical
-- alias — the ONE allowlisted machine-origin sink the "real operator for
-- every bead" directive (epic ka-0000004835, task ka-0000006012) retains
-- after collapsing the old per-source system:<source> / fleet:<name> sentinel
-- family.
--
-- system:machine is the FK target for TRUE machine-origin writes that have no
-- human behind them (audit-log/retention bookkeeping, escalation replay,
-- reconciler housekeeping, cron sweeps, CI-pipeline-status with no event
-- user). It is reachable ONLY via an explicit, code-reviewed
-- WithMachineOperator call at an allowlisted infra call site — never a
-- resolver fallback. Seeding it here makes it a frozen, FK-stable target with
-- a known synthetic email so the cold-path UpsertOperator never has to invent
-- one on first machine write.
--
-- The pre-existing system:* / fleet:* sentinel rows (seeded by migrations
-- 015/016) are intentionally LEFT IN PLACE: they are immutable FK targets of
-- hundreds of thousands of historical beads (beads.operated_by_id is a NOT
-- NULL FK to operators(id)), so "eliminate system operators" means stop
-- EMITTING them for new writes (resolver collapse in audit.go), not delete
-- the rows.
--
-- The synthetic email uses RFC 6762's reserved `.local` TLD so it can never
-- collide with real mail, matching migration 027's sentinel-email scheme and
-- satisfying the operators_email_not_empty CHECK added there.
--
-- Idempotent on re-apply via ON CONFLICT DO NOTHING.

INSERT INTO operators (kind, subject, email) VALUES
    ('system', 'machine', 'system-machine@noreply.kbeads.local')
ON CONFLICT (kind, subject) DO NOTHING;

INSERT INTO operator_aliases (operator_id, kind, subject, source)
SELECT id, kind, subject, 'migration'
  FROM operators
 WHERE kind = 'system' AND subject = 'machine'
ON CONFLICT (kind, subject) DO NOTHING;

-- Seed the operator slug (migration 028 scheme: system-kind rows get the
-- opaque `op-<id>` form) + canonical slug alias so /u/:slug resolution has a
-- single index probe and the cold-path ensureOperatorSlug never has to fill
-- it on first machine write. Predicate-gated on slug IS NULL, mirroring 028.
UPDATE operators
   SET slug = 'op-' || id
 WHERE kind = 'system' AND subject = 'machine' AND slug IS NULL;

INSERT INTO operator_slug_aliases (slug, operator_id)
SELECT slug, id
  FROM operators
 WHERE kind = 'system' AND subject = 'machine' AND slug IS NOT NULL
ON CONFLICT (slug) DO NOTHING;
