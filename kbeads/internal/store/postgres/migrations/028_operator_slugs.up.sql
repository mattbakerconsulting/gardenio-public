-- Per-user homepage M1 — operator slugs (ka-0000000022, parent epic
-- ka-0000006114). Design doc: gardenio/docs/research/per-user-homepage-design.md
-- §2.2.
--
-- Adds the routing primitive for `/u/:slug` — every operators row gets a
-- stable URL fragment. Slugs come from one of three places, in precedence
-- order:
--
--   1. The operator's email local-part, when it is `^[a-z0-9._-]+$`, not a
--      reserved word, and uses one of the human kinds (oidc).
--   2. Opaque `op-<id>` fallback for sentinels, slack-only operators, and
--      anything that fails the regex / reserved check.
--   3. Collision-resolved `<candidate>-<id>` suffix when two operators
--      would otherwise share a slug.
--
-- The slug history table preserves old slugs across renames so external
-- links don't 404 after a slug rewrite. v1 only seeds the canonical row
-- (slug = current slug, retired_at = NULL); future rename flows insert
-- additional rows with retired_at populated.
--
-- Operators is a small table (~hundreds of rows in prod), so the inline
-- backfill is safe — CLAUDE.md's "no eager full-table backfills" rule
-- targets multi-million-row tables. The migration runs idempotently end-
-- to-end; re-application is a no-op modulo updated_at bumps that we
-- explicitly avoid by predicate-gating writes on `slug IS NULL`.

-- ---------------------------------------------------------------------------
-- Step A — schema.
-- ---------------------------------------------------------------------------
ALTER TABLE operators
    ADD COLUMN IF NOT EXISTS slug TEXT;

CREATE TABLE IF NOT EXISTS operator_slug_aliases (
    slug         TEXT PRIMARY KEY,
    operator_id  BIGINT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    retired_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_operator_slug_aliases_operator_id
    ON operator_slug_aliases(operator_id);

-- ---------------------------------------------------------------------------
-- Step B — backfill candidate slug from (kind, email).
--
-- Reserved-word list mirrors the platform routes registered in beads-viewer
-- (`/me`, `/inbox`, `/people`, …) plus the `/u/op-…` opaque-id prefix `op`.
-- Anything that collides falls through to `op-<id>`.
-- ---------------------------------------------------------------------------
WITH candidates AS (
    SELECT id,
           CASE
               WHEN kind = 'oidc'
                AND position('@' IN email) > 1
                AND lower(split_part(email, '@', 1)) ~ '^[a-z0-9._-]+$'
                AND lower(split_part(email, '@', 1)) NOT IN (
                    'me','inbox','people','agents','fleet','new','search','admin',
                    'login','logout','auth','health','healthz','readyz','api',
                    'v1','image','htmx','dev','widgets','active','up','decisions',
                    'escalations','ratings','triggers','schedules','costs',
                    'coverage','views','settings','banner','cmdk','p','u','op'
                )
               THEN lower(split_part(email, '@', 1))
               ELSE 'op-' || id
           END AS candidate
      FROM operators
     WHERE slug IS NULL
)
UPDATE operators o
   SET slug = c.candidate
  FROM candidates c
 WHERE o.id = c.id;

-- ---------------------------------------------------------------------------
-- Step C — resolve collisions deterministically. The lowest operator id
-- keeps the unsuffixed slug; everyone else gets `<slug>-<id>`.
-- ---------------------------------------------------------------------------
WITH dups AS (
    SELECT slug, MIN(id) AS keeper
      FROM operators
     WHERE slug IS NOT NULL
     GROUP BY slug
    HAVING COUNT(*) > 1
)
UPDATE operators o
   SET slug = o.slug || '-' || o.id
  FROM dups d
 WHERE o.slug = d.slug
   AND o.id <> d.keeper;

-- ---------------------------------------------------------------------------
-- Step D — uniqueness invariant. Partial UNIQUE INDEX ignores rows where
-- slug is still NULL (post-migration only possible if upserts race the
-- backfill — the daemon-side cold path is responsible for filling that gap).
-- ---------------------------------------------------------------------------
CREATE UNIQUE INDEX IF NOT EXISTS operators_slug_uniq
    ON operators(slug)
    WHERE slug IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Step E — seed the alias table with the canonical row for each operator
-- so /u/:slug resolution can use a single index probe through
-- operator_slug_aliases without falling back to operators.slug.
-- ---------------------------------------------------------------------------
INSERT INTO operator_slug_aliases (slug, operator_id)
SELECT slug, id
  FROM operators
 WHERE slug IS NOT NULL
    ON CONFLICT (slug) DO NOTHING;
