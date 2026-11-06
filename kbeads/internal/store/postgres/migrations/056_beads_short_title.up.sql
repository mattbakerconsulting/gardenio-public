-- short_title: the <=40-char human display name (plan 2026-10-14-short-titles, gd-V5nGseRS5f).
-- Nullable; no backfill here — the groomer agent fills it. Length is enforced in Go (runes).
-- The plan named this 052; 052-055 landed first, so it is 056.
ALTER TABLE beads ADD COLUMN IF NOT EXISTS short_title TEXT;
