-- Pre-cursor migration for the operators backfill (P3).
-- Adds the `system:legacy-pre-attribution` sentinel operator + canonical alias
-- so the backfill admin command can rebind every empty-string operated_by row
-- on beads / comments / deps to a known FK target. Per ADR
-- docs/adr/human-attribution-identity-v2.md §4.5: live-state tables (beads,
-- comments, deps) get the sentinel; history-only tables (events,
-- mutation_history) keep operated_by_id NULL when the legacy TEXT was empty.
--
-- Idempotent on re-apply via ON CONFLICT DO NOTHING. Lands ahead of P3
-- (`kd admin backfill-operators`) and ahead of the P4 cutover migration that
-- enforces NOT NULL on operated_by_id.
--
-- Bead: ka-0000003618 (P3 backfill). Umbrella: ka-0000003625.

INSERT INTO operators (kind, subject) VALUES
    ('system', 'legacy-pre-attribution')
ON CONFLICT (kind, subject) DO NOTHING;

INSERT INTO operator_aliases (operator_id, kind, subject, source)
SELECT id, kind, subject, 'migration'
  FROM operators
 WHERE kind = 'system' AND subject = 'legacy-pre-attribution'
ON CONFLICT (kind, subject) DO NOTHING;
