-- Reverse of 028_operator_slugs.up.sql. Drops the alias table, the unique
-- index, and the slug column. Idempotent.

DROP INDEX IF EXISTS operators_slug_uniq;

DROP TABLE IF EXISTS operator_slug_aliases;

ALTER TABLE operators
    DROP COLUMN IF EXISTS slug;
