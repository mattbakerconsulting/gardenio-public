// Behavioral coverage for /gardeners — the gardeners INDEX page and its
// per-row expandable "runs" drawer (handler gardeners.go + gardener_invocations.go,
// templates gardeners.html + partials/gardener_invocations.html).
//
// The drawer is the "runs dropdown": each gardener row has a `runs` expand
// button that HTMX-loads the gardener's invocations (its spawned agent runs)
// into an inline drawer, with a TLDR ↔ TLDR+accomplishments toggle.
//
// REGRESSION FOCUS (gd-9t0i0PjcPF, "the runs dropdown doesn't work"): every
// invocation row inside the drawer must link to ITS OWN run/agent bead
// (/{runID}) — NOT back to the gardener page (/gardener/{id}). The bug was that
// all rows pointed at /gardener/{id}, so clicking a run merely reloaded the same
// gardener page and the dropdown felt dead. The only /gardener/{id} link the
// drawer should carry is the "open full page" link in its header.
//
// Auth: same dev-auth bypass as the rest of the suite — a plain page.goto() is
// an authenticated read (see gardener_detail.spec.js header for the full note).
//
// Fixtures: the `gardener` trigger bead + its three CLOSED `gardener_run_1..3`
// iteration agents (seed/fixtures.mjs). The same fixtures the detail spec uses;
// here we reach them through the index drawer instead of the detail page.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

test.describe('/gardeners index — runs drawer', () => {
  test('gardener row exposes a runs expand button', async ({ page }) => {
    const id = fixtureId('gardener');
    const res = await page.goto('/gardeners');
    expect(res.status(), '/gardeners HTTP status').toBeLessThan(400);

    // The fixture gardener has a row, and that row carries the runs expand button
    // wired to HTMX-load its invocations drawer.
    const row = page.locator(`#cap-row-${id}`);
    await expect(row).toBeVisible();
    const expand = row.locator('button.cap-expand');
    await expect(expand).toBeVisible();
    // The button is a compact caret (densified index, ka-0000003582); its
    // "runs" affordance is carried by the accessible name, not visible text.
    await expect(expand).toHaveAccessibleName('runs');
    await expect(expand).toHaveAttribute('aria-expanded', 'false');
    // It targets this row's drawer and lazy-loads from the invocations endpoint.
    await expect(expand).toHaveAttribute('hx-get', new RegExp(`/gardener/${id}/invocations$`));
    await expect(expand).toHaveAttribute('aria-controls', `cap-drawer-${id}`);
  });

  test('clicking runs opens the drawer and HTMX-loads the invocations', async ({ page }) => {
    const id = fixtureId('gardener');
    await page.goto('/gardeners');

    const row = page.locator(`#cap-row-${id}`);
    const drawer = page.locator(`#cap-drawer-${id}`);
    // Drawer starts hidden (empty host) — content loads on first open.
    await expect(drawer).toBeHidden();

    await row.locator('button.cap-expand').click();
    await waitForHtmxIdle(page);

    // Drawer is now visible, aria flips, and the invocations partial swapped in.
    await expect(drawer).toBeVisible();
    await expect(row.locator('button.cap-expand')).toHaveAttribute('aria-expanded', 'true');
    // Three closed fixture runs → three invocation rows + a count chip.
    const invRows = drawer.locator('.cap-inv-row');
    await expect(invRows).toHaveCount(3);
    await expect(drawer.locator('.cap-drawer-count')).toContainText('3 invocations');
  });

  test('REGRESSION: each run row links to its run bead, not back to the gardener', async ({ page }) => {
    const id = fixtureId('gardener');
    const run1 = fixtureId('gardener_run_1');
    const run2 = fixtureId('gardener_run_2');
    const run3 = fixtureId('gardener_run_3');

    await page.goto('/gardeners');
    const row = page.locator(`#cap-row-${id}`);
    const drawer = page.locator(`#cap-drawer-${id}`);
    await row.locator('button.cap-expand').click();
    await waitForHtmxIdle(page);

    // CORE assertion of this spec (gd-9t0i0PjcPF): every run headline links to
    // its own /{runID} bead page. If any row reverted to /gardener/{id}, the
    // dropdown is "dead" — clicking a run would reload the gardener page.
    const headlines = drawer.locator('a.cap-inv-headline');
    await expect(headlines).toHaveCount(3);
    for (const runId of [run1, run2, run3]) {
      await expect(
        drawer.locator(`a.cap-inv-headline[href="/${runId}"]`),
        `run ${runId} must link to its own bead page`,
      ).toHaveCount(1);
    }
    // No run row may point at the gardener page. The ONLY /gardener/{id} link in
    // the drawer is the header "open full page" link.
    await expect(drawer.locator(`a.cap-inv-headline[href="/gardener/${id}"]`)).toHaveCount(0);
    await expect(drawer.locator(`a.cap-drawer-fullpage[href="/gardener/${id}"]`)).toHaveCount(1);

    // And it actually navigates: clicking the newest run lands on that run's
    // bead page, not the gardener.
    await drawer.locator(`a.cap-inv-headline[href="/${run3}"]`).click();
    await expect(page).toHaveURL(new RegExp(`/${run3}$`));
  });

  test('TLDR ↔ accomplishments toggle swaps the drawer detail level', async ({ page }) => {
    const id = fixtureId('gardener');
    await page.goto('/gardeners');

    const row = page.locator(`#cap-row-${id}`);
    const drawer = page.locator(`#cap-drawer-${id}`);
    await row.locator('button.cap-expand').click();
    await waitForHtmxIdle(page);

    // Default view is TLDR-only: the accomplishments prose blocks are absent.
    await expect(drawer.locator('.cap-inv-accomplishments')).toHaveCount(0);
    const tldrTab = drawer.locator('.cap-drawer-tab', { hasText: 'TLDR' }).first();
    await expect(tldrTab).toHaveClass(/active/);

    // Switch to "TLDR + accomplishments": the prose blocks appear for each
    // closed run (all three fixture runs carry accomplishments).
    await drawer.locator('.cap-drawer-tab', { hasText: 'accomplishments' }).click();
    await waitForHtmxIdle(page);
    await expect(drawer.locator('.cap-inv-accomplishments')).toHaveCount(3);
    await expect(
      drawer.locator('.cap-drawer-tab', { hasText: 'accomplishments' }),
    ).toHaveClass(/active/);
  });
});
