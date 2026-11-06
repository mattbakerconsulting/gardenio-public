-- heraldry, round 1: the field colour joins the arms (heraldry spec
-- gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md R5, R13,
-- §4.1; step 1.1b gd-MTOrMbn7kL). The canonical form grows from five tokens
-- (the places) to six: a lower-case colour word, then the five places
-- ("purple 🔥 . 🧭 . 📦"). Equality on the column is the register's identity,
-- so idx_beads_heraldry_register (migration 057) keys on colour plus places
-- with no change to the index: the same places on another colour are other
-- arms.
--
-- This gives the rows written in the five-token form a colour. The daemon
-- chooses a colour by measured contrast (kbeads/internal/heraldry
-- DefaultTincture), which SQL cannot repeat, so every such row takes purple,
-- the colour DefaultTincture chooses for most of the library on an empty
-- register (TestMigration058ColourIsTheCommonDefault keeps this true).
-- Giving every row the same colour keeps the register unique: two rows that
-- differed in their places still differ. The set is the few beads given arms
-- between migration 057 and this one (the partial index's rows), so this is
-- not a full-table backfill. Idempotent: a six-token row is left alone.
UPDATE beads
   SET heraldry = 'purple ' || heraldry
 WHERE heraldry IS NOT NULL
   AND array_length(string_to_array(heraldry, ' '), 1) = 5;
