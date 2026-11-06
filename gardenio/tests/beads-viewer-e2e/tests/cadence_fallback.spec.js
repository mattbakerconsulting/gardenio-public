// Behavioral coverage for the viewer cadence fallback (task ka-0000007965,
// backfilling the e2e gap for ka-0000007640).
//
// schedule_cron_fallback.go (cronFromTriggers / scheduleCronTimezone) makes the
// viewer's cadence fall back to the FIRST kind:cron entry of a bead's
// `triggers:` JSON array when the legacy top-level `cron`/`timezone` fields are
// EMPTY. The legacy fields always win when present (no regression for
// un-migrated beads). It is wired into the /triggers rows (triggers.go:487) and
// the gardener read sites (gardeners.go:955 index next-fire, gardener.go:366 detail
// cadence strip).
//
// WHY THIS IS LOAD-BEARING: sibling task ka-0000004962 ("Step B") will CLEAR the
// legacy cron/timezone on 12 live gardeners. The moment it runs, this fallback is
// the ONLY source of cadence on the prod /triggers and /gardeners pages. Its
// acceptance criterion is "cron + next-fire still render" — and until this spec
// nothing in e2e proved it. Coverage was unit-only (schedule_cron_fallback_test.go).
//
// ANTI-VACUITY: every assertion checks the RENDERED VALUE (the specific cron
// string), not merely that a chip element exists — a presence-only check passes
// trivially when the fixture never produces the state. The fixtures seed
// distinctive cron expressions (17,47 / 3 3 / 23 * ) so a match is unambiguous.
//
// SAFETY: the fixtures (seed/fixtures.mjs, keys cadence_*) all carry the
// fixture:e2e:<key> label, so all three firing engines skip them (isE2EFixture);
// an enabled cron fixture never spawns a real agent. The gardener fixtures render
// on /gardeners + /gardener/{id} only because the harness sets
// BEADS_VIEWER_RENDER_FIXTURE_GARDENERS=1 (playwright.config.js webServer env).
//
// SCOPE NOTE — the task's scenario 3 (INDEPENDENT timezone fallback: legacy cron
// present, legacy tz cleared, tz resolved from triggers) is only reachable
// through buildScheduleView / the type_schedule card, which requires a
// type=schedule bead. That primitive is dead (Phase 3b/D10 rejects
// `kd create --type=schedule`), so it cannot be seeded here and stays unit-only
// in schedule_cron_fallback_test.go::TestScheduleCronTimezone. It is deliberately
// NOT stubbed with a vacuous e2e assertion. Scenario 6 (source scoping) is
// covered instead — it IS reachable and guards the same helper's blast radius.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

function rowSelector(page, id) {
  return page.locator(`li.trig-row[data-bead-id="${id}"]`);
}

test.describe('viewer cadence fallback to triggers[0] cron (ka-0000007640)', () => {
  // Scenario 1 — FALLBACK FIRES. A schedule-source row whose legacy flat cron is
  // cleared renders its cadence from the triggers[] kind:cron entry: the
  // /triggers match summary carries `cron: 17,47 * * * *` plus a live next-fire
  // relative suffix. This is the exact post-Step-B state.
  test('scenario 1: /triggers renders cron + next-fire from triggers[] when legacy cron is empty', async ({ page }) => {
    const id = fixtureId('cadence_fallback');
    await page.goto('/triggers?source=schedule');

    const row = rowSelector(page, id);
    await expect(row).toBeVisible();

    // VALUE assertion: the specific triggers[] cron string is rendered, not just
    // a chip. The /triggers row folds cadence into the match summary as
    // `cron: <expr> (<tz>) · next <relative>`.
    const match = row.locator('.trig-match').first();
    await expect(match).toHaveText(/cron:\s*17,47 \* \* \* \* \(UTC\)/);

    // Non-empty next-fire — the fallback drives computeNextFire, not just the
    // cron string. Format: " · next 42s" / "12m" / "3h" / "2d".
    await expect(match).toHaveText(/·\s*next\s+\d+(s|m|h|d)/);

    // No parse error leaked into the summary (a real cron parsed cleanly).
    await expect(match).not.toHaveText(/next:\s/);
  });

  // Scenario 2 — LEGACY PRECEDENCE (regression guard). A row carrying BOTH a
  // legacy flat cron (3 3 * * *) and a DIFFERENT triggers[] cron (59 23 * * *)
  // must render the LEGACY one. A naive fix that always prefers triggers[]
  // inverts this — the no-regression invariant for un-migrated beads.
  test('scenario 2: legacy flat cron wins over a different triggers[] cron', async ({ page }) => {
    const id = fixtureId('cadence_legacy_wins');
    await page.goto('/triggers?source=schedule');

    const row = rowSelector(page, id);
    await expect(row).toBeVisible();

    const match = row.locator('.trig-match').first();
    // The LEGACY cron renders...
    await expect(match).toHaveText(/cron:\s*3 3 \* \* \* \(UTC\)/);
    // ...and the triggers[] cron does NOT leak in.
    await expect(match).not.toHaveText(/59 23/);
    await expect(match).not.toHaveText(/America\/New_York/);
  });

  // Scenario 5 — NON-CRON ENTRY IGNORED. A schedule-source row with the flat
  // cron cleared and a triggers[] holding ONLY a bead_event entry (no cron)
  // renders the empty cadence state ("(no cron)"), NOT a bogus value.
  // cronFromTriggers returns "" for a cron-less triggers array.
  test('scenario 5: a triggers[] with no cron entry renders the empty cadence state', async ({ page }) => {
    const id = fixtureId('cadence_noncron');
    await page.goto('/triggers?source=schedule');

    const row = rowSelector(page, id);
    await expect(row).toBeVisible();

    const match = row.locator('.trig-match').first();
    // Empty cadence — the schedule-source match summary for a cron-less row.
    await expect(match).toHaveText(/\(no cron\)/);
    // And no fabricated next-fire or cron expression.
    await expect(match).not.toHaveText(/·\s*next\s/);
  });

  // Scenario 6 — SOURCE SCOPING. The /triggers fallback is scoped
  // `row.Source == "schedule"` (triggers.go:486), so a NON-schedule-source
  // (gitlab) row carrying a cron in triggers[] must NOT surface that cron as a
  // cadence — its match summary is the gitlab match, with no cron/next-fire.
  test('scenario 6: a non-schedule-source row does not surface a triggers[] cron as cadence', async ({ page }) => {
    const id = fixtureId('cadence_source_scoping');
    // ?filter=all so the disabled fixture is in the population; ?source=gitlab
    // scopes to its facet.
    await page.goto('/triggers?filter=all&source=gitlab');

    const row = rowSelector(page, id);
    await expect(row).toBeVisible();

    const match = row.locator('.trig-match').first();
    // The gitlab match summary renders (no gitlab event fields → "(any gitlab event)").
    await expect(match).toHaveText(/any gitlab event/);
    // The triggers[] cron must NOT be surfaced as a cadence on a non-schedule row.
    await expect(match).not.toHaveText(/cron:\s*41/);
    await expect(match).not.toHaveText(/·\s*next\s/);
  });

  // Scenario 1 (gardener surfaces) — the fallback also drives the two gardener
  // read sites. On /gardeners the migrated gardener's next-fire lane renders a
  // `cap-next` relative fire computed from the triggers[] cron; on /gardener/{id}
  // and /{id} the gardener_summary cadence strip renders the `gardener-cadence`
  // chip carrying `23 * * * *`.
  test('scenario 1 (gardeners): /gardeners next-fire + /gardener/{id} cadence chip fall back to triggers[] cron', async ({ page }) => {
    const id = fixtureId('cadence_gardener');

    // /gardeners index row: the next-fire lane renders a relative fire.
    await page.goto('/gardeners');
    const row = page.locator(`#cap-row-${id}`);
    await expect(row).toBeVisible();
    const next = row.locator('.cap-next').first();
    await expect(next).toBeVisible();
    await expect(next).toHaveText(/\d+(s|m|h|d)/); // e.g. "in 23m"

    // /gardener/{id} detail: the cadence strip renders the fallback cron VALUE.
    await page.goto(`/gardener/${id}`);
    const cadence = page.locator('.gardener-cadence').first();
    await expect(cadence).toBeVisible();
    await expect(cadence).toContainText('23 * * * *');
  });

  // Scenario 4 — TRIGGER:MULTI WATCHDOG EXCLUSION. An event-mode gardener
  // (trigger:multi) whose triggers[] holds a bead_event entry plus a WATCHDOG
  // cron. The /gardeners next-fire fallback is guarded off for trigger:multi
  // (gardeners.go: `&& !hasLabel(b, "trigger:multi")`), so the watchdog cron must
  // NOT drive a next-fire — the row's next-fire lane shows the em-dash, not a
  // countdown from `13 * * * *`. This guard lives only on the gardeners path.
  test('scenario 4: a trigger:multi watchdog cron is not surfaced as a clock cadence on /gardeners', async ({ page }) => {
    const id = fixtureId('cadence_multi_watchdog');
    await page.goto('/gardeners');

    const row = page.locator(`#cap-row-${id}`);
    await expect(row).toBeVisible();

    // The gardener is enabled + open, so the ONLY reason its next-fire lane
    // carries no `cap-next` clock countdown is the trigger:multi guard: the
    // watchdog cron must not masquerade as a cadence. (Positive control: the
    // scenario-1 gardener above — same shape minus the guard — DOES render a
    // cap-next, so a count of 0 here is the guard firing, not a dead row.)
    await expect(row.locator('.cap-next')).toHaveCount(0);
  });
});
