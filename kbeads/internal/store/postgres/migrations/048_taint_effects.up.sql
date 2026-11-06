-- Reserve the taints.effects column: a taint carries a SET of effects, not
-- just the single implicit "gate" it has today. Schema reservation only, per
-- ADR docs/adr/taints.md §15.2/§15.4 (gd-9YQiNJVW6u) — this migration
-- enforces nothing new. The default backfills every existing row to mean
-- exactly what it meant before ('gate'), so no data migration is needed.
ALTER TABLE taints ADD COLUMN IF NOT EXISTS effects TEXT[] NOT NULL DEFAULT '{gate}';
