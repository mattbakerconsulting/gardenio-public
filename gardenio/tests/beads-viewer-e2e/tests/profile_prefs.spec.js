// Backfill (ka-0000003378): the /settings profile-preferences surface
// (ProfilePref toggle + reordering, ka-0000004773). The Homepage-sections
// editor — visibility toggles + arrow reordering that drive /u/{slug}'s
// section order — shipped with no behavioral spec; nav.spec.js only checks the
// /settings h1 renders.
//
// WIRE (settings.go / profile_prefs.go): /settings resolves the viewer's
// type=user bead by the X-Auth-Request-Email header (findUserBeadByEmail; NO
// operator row required) and renders settings_block.html. Anonymous viewers
// get a 401. The inline editor (settings_edit.html) toggles section visibility
// via `section_visible_<slug>` checkboxes and reorders rows via up/down buttons
// whose JS keeps the hidden `#settings-section-order` input in sync; Save POSTs
// to /settings and htmx-swaps the read-only block back in.
//
// This spec drives the SETTINGS operator (e2e-settings@example.test, seeded by
// people_settings_user) — deliberately SEPARATE from the /people directory
// operator so a save here can never clobber the directory row's display name
// under parallel execution. The stateful editor flow is `.serial` so its
// mutations don't race between workers, and every assertion is re-derived from
// the LIVE editor state, so the file stays green across repeated runs even
// though a save persists. Keep SETTINGS_EMAIL in sync with seed/fixtures.mjs.

import { expect, test } from '@playwright/test';

import { waitForHtmxIdle } from './_helpers.js';

const SETTINGS_EMAIL = 'e2e-settings@example.test';

test.describe('GET /settings — auth gate', () => {
  test('an anonymous viewer is refused (401)', async ({ request }) => {
    const res = await request.get('/settings', { maxRedirects: 0 });
    expect(res.status(), 'anonymous /settings status').toBe(401);
  });
});

test.describe('GET /settings — Homepage sections (read-only)', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': SETTINGS_EMAIL } });

  test('renders the settings shell with the Homepage sections list', async ({
    page,
  }) => {
    const res = await page.goto('/settings');
    expect(res.status(), '/settings status').toBeLessThan(400);
    await expect(page.locator('nav').first()).toBeVisible();
    await expect(page.locator('h1').first()).toContainText(/^Settings/);

    // The Homepage-sections list is the ProfilePref surface. Every known
    // section renders one row carrying its slug; each row is marked visible or
    // hidden. (buildProfileSectionView always emits the full catalog.)
    const rows = page.locator('ol.profile-sections-list li.profile-section-row');
    const count = await rows.count();
    expect(count, 'every homepage section renders a row').toBeGreaterThanOrEqual(9);

    // Each row exposes a non-empty data-slug and a visible/hidden marker.
    for (let i = 0; i < count; i += 1) {
      const slug = await rows.nth(i).getAttribute('data-slug');
      expect(slug, `row ${i} has a slug`).toBeTruthy();
      await expect(rows.nth(i), `row ${i} states visibility`).toContainText(
        /visible|hidden/,
      );
    }
  });

  test('the read-only block offers the Edit affordance', async ({ page }) => {
    await page.goto('/settings');
    await expect(
      page.getByRole('button', { name: 'Edit preferences' }),
    ).toBeVisible();
  });
});

// The editor round-trip mutates the settings user's profile_prefs, so serialize
// it to keep parallel workers from racing on the shared user bead.
test.describe.serial('the Homepage-sections editor', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': SETTINGS_EMAIL } });

  test('opening the editor renders toggles, reorder controls, and the order input', async ({
    page,
  }) => {
    await page.goto('/settings');
    await page.getByRole('button', { name: 'Edit preferences' }).click();
    await waitForHtmxIdle(page);

    // The editor swapped in: the ordered list of editable rows, the hidden
    // order input the JS keeps in sync, and per-row visibility + reorder
    // controls.
    await expect(page.locator('#settings-section-list')).toBeVisible();
    await expect(page.locator('#settings-section-order')).toBeAttached();
    const editRows = page.locator('#settings-section-list li.profile-section-row');
    const n = await editRows.count();
    expect(n, 'editor lists every section').toBeGreaterThanOrEqual(9);
    // Each row carries a visibility checkbox + up/down reorder buttons.
    await expect(
      editRows.first().locator('input[type="checkbox"]'),
    ).toBeAttached();
    await expect(editRows.first().locator('.profile-section-up')).toBeVisible();
    await expect(editRows.first().locator('.profile-section-down')).toBeVisible();
  });

  test('reordering a row via the arrow updates the hidden order input', async ({
    page,
  }) => {
    await page.goto('/settings');
    await page.getByRole('button', { name: 'Edit preferences' }).click();
    await waitForHtmxIdle(page);

    const orderInput = page.locator('#settings-section-order');
    const before = await orderInput.inputValue();
    const slugs = before.split(',');
    expect(slugs.length, 'order input lists sections').toBeGreaterThanOrEqual(9);

    // Move the SECOND row up: the JS syncOrder swaps the first two slugs.
    const secondRow = page
      .locator('#settings-section-list li.profile-section-row')
      .nth(1);
    await secondRow.locator('.profile-section-up').click();

    const after = await orderInput.inputValue();
    const afterSlugs = after.split(',');
    // The move is a swap of the first two entries — the hidden input reflects
    // the new DOM order (proves the reorder JS is wired to the input the POST
    // reads).
    expect(after, 'order input changed after reorder').not.toBe(before);
    expect(afterSlugs[0], 'row 2 moved to the front').toBe(slugs[1]);
    expect(afterSlugs[1], 'row 1 moved to second').toBe(slugs[0]);
  });

  test('toggling visibility off and saving reflects the change in the read-only block', async ({
    page,
  }) => {
    await page.goto('/settings');
    await page.getByRole('button', { name: 'Edit preferences' }).click();
    await waitForHtmxIdle(page);

    // Pick the first CURRENTLY-VISIBLE section (checked box) and hide it. Doing
    // it relative to live state keeps the test idempotent across repeated runs.
    const checkedBoxes = page.locator(
      '#settings-section-list li.profile-section-row input[type="checkbox"]:checked',
    );
    const visibleCount = await checkedBoxes.count();
    expect(visibleCount, 'at least one visible section to hide').toBeGreaterThanOrEqual(1);

    const targetRow = page
      .locator('#settings-section-list li.profile-section-row')
      .filter({ has: page.locator('input[type="checkbox"]:checked') })
      .first();
    const targetSlug = await targetRow.getAttribute('data-slug');
    await targetRow.locator('input[type="checkbox"]').uncheck();

    // Save — htmx swaps the read-only block back in.
    await page.getByRole('button', { name: 'Save', exact: true }).click();
    await waitForHtmxIdle(page);

    // Back on the read-only block: the toggled section now reads "hidden".
    const readonlyRow = page.locator(
      `ol.profile-sections-list li.profile-section-row[data-slug="${targetSlug}"]`,
    );
    await expect(readonlyRow, 'read-only block re-rendered').toBeVisible();
    await expect(readonlyRow, 'toggled section now hidden').toContainText('hidden');

    // Re-fetching /settings shows the persisted state (a durable write, the
    // same round-trip guarantee user_status_message_auth.spec.js now asserts
    // for status_message since it joined the type:user schema in bb8645d16).
    await page.goto('/settings');
    const persistedRow = page.locator(
      `ol.profile-sections-list li.profile-section-row[data-slug="${targetSlug}"]`,
    );
    await expect(persistedRow, 'hidden state persisted').toContainText('hidden');
  });
});
