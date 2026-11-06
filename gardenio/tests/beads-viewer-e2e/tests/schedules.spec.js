// Behavioral coverage for /triggers (schedule source) — the page Matt
// reported as broken ("sort by next fire isn't working") in
// https://beads-viewer.gardenio.app.e2e.dev.acme.example/ka-0000006359.
//
// The legacy /schedules route now 301-redirects to /triggers and the
// schedule fixtures themselves are seeded as type=trigger label=trigger:schedule
// (gd-ASAfYmze5C). The behavior pinned here is unchanged from the original
// schedule view — only the page URL and row selectors moved.
//
// Two independent bugs converged on the same operator-visible symptom:
//
//   1. The cron parser used by buildTriggerRow rejected descriptor forms
//      (@hourly, @daily, …) so any schedule using one parsed as an error
//      and sank to the bottom of the next-fire sort.
//   2. The absolute next-fire string was rendered in each schedule's own
//      timezone, so a correctly-sorted list ("13:00 UTC", "11:03 PDT")
//      LOOKED unsorted to a human reader.
//
// This file pins both behaviors:
//
//   - sort=next-fire orders rows by absolute instant ascending; the
//     hourly fixture (which used to error out) is sorted into its true
//     position rather than dragged to the bottom.
//   - each row's match-summary carries the cron + timezone, and the
//     relative "next Xh" suffix is the operator-visible signal that
//     mixed-timezone rows are still in true chronological order.
//   - sort=cron orders rows lexicographically by cron string and is
//     reachable via the visible "cron" sort link.
//   - filter chips (All / Enabled / Disabled) gate which trigger rows
//     render; disabled fixtures should leave the visible list under the
//     Enabled chip.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// Resolves to a regex matching any string the row identified by `id`
// renders containing. Falling back to the bead id is more robust than
// matching the title because the listing strips trailing whitespace and
// HTMX swap state can briefly mutate the title text.
function rowSelector(page, id) {
  return page.locator(`li.trig-row[data-bead-id="${id}"]`);
}

test.describe('/triggers (schedule source) sort + timezone display', () => {
  test('sort=next-fire orders rows by next-fire ascending, including @hourly descriptor', async ({ page }) => {
    const hourlyId = fixtureId('schedule_hourly');
    const utc9Id = fixtureId('schedule');     // cron 0 9 * * *  UTC
    const pdtId  = fixtureId('schedule_pdt'); // cron 0 21 * * * America/Los_Angeles

    // Default trigger sort is source+title — pin sort=next-fire explicitly
    // since this test is specifically about next-fire ordering. Also filter
    // to source=schedule to keep other trigger sources out of the row index.
    await page.goto('/triggers?sort=next-fire&source=schedule');
    await expect(page.locator('h1').first()).toHaveText(/^Triggers/);

    // All three enabled fixtures must render.
    await expect(rowSelector(page, hourlyId)).toBeVisible();
    await expect(rowSelector(page, utc9Id)).toBeVisible();
    await expect(rowSelector(page, pdtId)).toBeVisible();

    // None should carry a parse error — descriptor cron must be accepted.
    // The row template suppresses a separate error span on /triggers; the
    // match-summary text would contain "next: <error>" if parsing failed.
    const errMatches = page.locator('li.trig-row .trig-match', { hasText: /next:\s/ });
    await expect(errMatches).toHaveCount(0);

    // Snapshot the rendered order of our three fixtures relative to one
    // another by reading positions in the DOM. We don't assert against
    // ALL rows because other seeds (or pre-existing schedules in the
    // shared daemon) may interleave; we only require *our* three to be
    // in the expected order.
    const positions = await page.evaluate((ids) => {
      const rows = Array.from(document.querySelectorAll('li.trig-row'));
      return ids.map((id) => rows.findIndex((row) => row.getAttribute('data-bead-id') === id));
    }, [hourlyId, utc9Id, pdtId]);
    expect(positions.every((p) => p >= 0), `all three fixtures rendered: ${JSON.stringify(positions)}`).toBe(true);

    // Hourly fires soonest (within the next ~60m), then 0 9 UTC, then
    // 0 21 PDT (= 04:00 UTC the next day). The hourly fixture is the
    // critical case — pre-fix it would sink to the bottom.
    const [pHourly, pUtc9, pPdt] = positions;
    // Compute the actual next-fire ordering: hourly is always within 60m,
    // so it's strictly earlier than any daily fixture. utc9 vs pdt depends
    // on the current wall clock; the assertion below tolerates either
    // order for the daily pair, but pins hourly to first.
    expect(pHourly).toBeLessThan(pUtc9);
    expect(pHourly).toBeLessThan(pPdt);
  });

  test('row renders cron + timezone in match summary and timezone meta chip', async ({ page }) => {
    const pdtId = fixtureId('schedule_pdt');
    await page.goto('/triggers?source=schedule');

    const row = rowSelector(page, pdtId);
    await expect(row).toBeVisible();

    // The /triggers row template folds the next-fire signal into the
    // match-summary span as `cron: <expr> (<tz>) · next <relative>`.
    // The presence of the timezone in the match summary is the
    // operator-visible signal that the row is rendered in its own tz.
    const match = row.locator('.trig-match').first();
    await expect(match).toHaveText(/cron:\s*0 21 \* \* \* \(America\/Los_Angeles\)/);

    // Relative-time suffix — the operator-visible signal that the sort is
    // honest across timezones. Format: " · next 42s" / " · next 12m" /
    // " · next 3h" / " · next 2d".
    await expect(match).toHaveText(/·\s*next\s+\d+(s|m|h|d)/);

    // Timezone meta chip on the row's line-4. Source field is the seeded
    // `timezone` value, not a derived abbreviation, so we assert on the
    // IANA name.
    await expect(row.getByText('America/Los_Angeles', { exact: true })).toBeVisible();
  });

  test('sort=cron orders rows lexicographically by cron expression', async ({ page }) => {
    const utc9Id    = fixtureId('schedule');     // cron 0 9 * * *
    const pdtId     = fixtureId('schedule_pdt'); // cron 0 21 * * *
    const hourlyId  = fixtureId('schedule_hourly');

    await page.goto('/triggers?source=schedule');
    await page.getByRole('link', { name: 'cron', exact: true }).click();
    await page.waitForURL(/\/triggers\?.*sort=cron/);

    // Lexicographic order on the cron field: "0 21 * * *" < "0 9 * * *" <
    // "@hourly" (digits sort before '@' in ASCII). All three are enabled
    // so they're in the same active group and the cron sort wins.
    const positions = await page.evaluate((ids) => {
      const rows = Array.from(document.querySelectorAll('li.trig-row'));
      return ids.map((id) => rows.findIndex((row) => row.getAttribute('data-bead-id') === id));
    }, [pdtId, utc9Id, hourlyId]);
    expect(positions.every((p) => p >= 0), `all three fixtures rendered: ${JSON.stringify(positions)}`).toBe(true);
    const [pPdt, pUtc9, pHourly] = positions;
    expect(pPdt).toBeLessThan(pUtc9);
    expect(pUtc9).toBeLessThan(pHourly);
  });

  test('filter=enabled hides disabled fixtures', async ({ page }) => {
    const disabledId = fixtureId('schedule_disabled');
    const enabledId  = fixtureId('schedule');

    await page.goto('/triggers?filter=enabled&source=schedule');

    await expect(rowSelector(page, enabledId)).toBeVisible();
    await expect(rowSelector(page, disabledId)).toHaveCount(0);

    // The Enabled chip is the active one. The first .chip-strip on
    // /triggers is the All/Enabled/Disabled filter strip.
    const activeChip = page.locator('.chip-strip').first().locator('.chip-active');
    await expect(activeChip).toContainText(/^Enabled\b/);
  });

  test('filter=disabled shows the disabled fixture and hides enabled ones', async ({ page }) => {
    const disabledId = fixtureId('schedule_disabled');
    const enabledId  = fixtureId('schedule');

    await page.goto('/triggers?filter=disabled&source=schedule');

    await expect(rowSelector(page, disabledId)).toBeVisible();
    await expect(rowSelector(page, enabledId)).toHaveCount(0);

    const activeChip = page.locator('.chip-strip').first().locator('.chip-active');
    await expect(activeChip).toContainText(/^Disabled\b/);
  });
});
