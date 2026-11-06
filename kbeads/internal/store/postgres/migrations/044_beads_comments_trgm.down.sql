-- Reverse of 044_beads_comments_trgm.up.sql. Drop the trgm indexes; each is
-- IF EXISTS so the down is idempotent and a partially-applied up recovers
-- cleanly. Do NOT DROP EXTENSION pg_trgm — other objects may come to depend on
-- it, and dropping an extension in use fails; mirror migration 005's "don't
-- drop the shared extension on down" note.

DROP INDEX IF EXISTS idx_comments_text_trgm;

DROP INDEX IF EXISTS idx_beads_description_trgm;

DROP INDEX IF EXISTS idx_beads_title_trgm;
