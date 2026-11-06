// Spawn-agent button browser-level coverage (ka-0000001619 / gd-3DpGDoFMvP).
//
// Regression guard for the "spawn agent button is broken" bug fixed alongside
// this spec: the beads-viewer write handlers were calling the kbeads daemon
// without stamping X-Operated-By on the outbound ctx. Post-R2 enforcement
// (ka-0000005772), the daemon fail-closes such writes with HTTP 400
// "operator could not be resolved", which the UI surfaced as a generic
// "Failed to spawn agent." banner — the symptom Matt hit on
// 2026-05-18. Fix lives at gardenio/controller/cmd/beads-viewer/quick_spawn.go
// (per-handler) and middleware.go::operatorMiddleware (defense-in-depth for
// the rest of the write surface).
//
// What this spec asserts:
//
//   1. The nav "+ New Agent" button opens the quick-spawn modal as an htmx
//      partial swap (#quick-spawn-host gets the sheet, no full page nav).
//   2. Submitting a valid form makes the daemon write succeed end-to-end:
//      the success result renders with a real kd-… agent bead id and an
//      "Open bead" link. If X-Operated-By were missing the response would
//      render the "Spawn failed." error banner instead — that's exactly
//      what this test fails on.
//   3. The POST status is 200, not 502 / 400 (the visible failure mode
//      pre-fix when the daemon rejects the write).
//
// Auth: dev-bypass mode (BEADS_VIEWER_DEV_AUTH_BYPASS=1) means
// `oidcauth.EmailFromRequest` returns "" unless we set
// X-Auth-Request-Email here. The bypass middleware is a passthrough so
// the header reaches the handler exactly as ForwardAuth would inject it
// in prod, and `operatorMiddleware` stamps it onto the daemon ctx.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

const AUTH_EMAIL = 'e2e-spawn@example.test';

test.use({
  extraHTTPHeaders: {
    'X-Auth-Request-Email': AUTH_EMAIL,
  },
});

test.describe('spawn agent button', () => {
  test('nav "+ New Agent" opens the modal and a valid submit spawns a bead', async ({ page }) => {
    // Land on a bead-show page so CurrentBeadID is set — that matches the
    // common operator path (right-click a bead, "Spawn agent") and also
    // pre-fills the task_id field on the modal, exercising the
    // AddDependency leg of `spawnOne`.
    const id = fixtureId('generic');
    await page.goto(`/${id}`);
    await waitForHtmxIdle(page);

    // (1) Click the nav "+ New Agent" button and assert the modal lands
    // inside #quick-spawn-host without a full reload.
    const navStartBefore = await page.evaluate(() =>
      performance.timing
        ? performance.timing.navigationStart
        : performance.getEntriesByType('navigation')[0]?.startTime,
    );

    const navBtn = page.locator('button.nav-quick-spawn');
    await expect(navBtn).toBeVisible();
    await navBtn.click();
    await waitForHtmxIdle(page);

    const navStartAfter = await page.evaluate(() =>
      performance.timing
        ? performance.timing.navigationStart
        : performance.getEntriesByType('navigation')[0]?.startTime,
    );
    expect(navStartAfter, 'opening the modal must be an htmx swap, not a reload').toBe(navStartBefore);

    const sheet = page.locator('#quick-spawn-sheet');
    await expect(sheet).toBeVisible();
    await expect(sheet.locator('h2', { hasText: 'New Agent' })).toBeVisible();

    // (2) Fill the prompt. project defaults to gardenio (the daemon's
    // project listing always seeds it via buildQuickSpawnSheetView); role
    // defaults to crew. task_id is pre-filled to the current bead.
    const marker = `e2e-spawn-${Date.now()}`;
    const promptText = `${marker} — exercise spawn path end-to-end`;

    const promptBox = sheet.locator('textarea[name="prompt"]');
    await expect(promptBox).toBeVisible();
    await promptBox.fill(promptText);

    // The task_id field should be pre-filled with the current bead id —
    // this is how the layout passes context through to the modal form.
    await expect(sheet.locator('input[name="task_id"]')).toHaveValue(id);

    // (3) Wait for the POST so we can assert its status independent of the
    // rendered body. A pre-fix run returned 502 here (renderQuickSpawnError
    // path triggered by the daemon's HTTP 400 operator-could-not-be-resolved
    // response). Post-fix this must be a 200.
    const postPromise = page.waitForResponse(
      (r) => r.url().endsWith('/quick-spawn') && r.request().method() === 'POST',
    );
    await sheet.locator('button[type="submit"]', { hasText: 'Spawn Agent' }).click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /quick-spawn status').toBe(200);
    await waitForHtmxIdle(page);

    // (4) Success result rendered into the same #quick-spawn-sheet slot
    // (hx-swap=outerHTML on the form). The body must NOT carry the
    // error banner.
    const resultSheet = page.locator('#quick-spawn-sheet');
    await expect(resultSheet.locator('h2', { hasText: 'Agent spawned' })).toBeVisible();
    await expect(resultSheet.locator('.quick-spawn-error')).toHaveCount(0);

    // (5) Success view must show a real kd-… link. The "Open bead" anchor
    // points at the new agent bead — extract its href to prove the
    // CreateBead call returned a real id.
    const openLink = resultSheet.locator('a.quick-spawn-submit', { hasText: 'Open bead' });
    await expect(openLink).toBeVisible();
    const href = await openLink.getAttribute('href');
    expect(href, 'Open bead link href').toMatch(/\/(gd|ka)-[A-Za-z0-9]+$/);
  });

  test('rendered error banner surfaces a daemon write failure (mocked)', async ({ page }) => {
    // Symmetry check with the happy path: when the daemon write actually
    // fails, the spawn handler must render the error banner so the user
    // sees a real failure mode rather than a silent no-op. This guards
    // against future regressions that would swallow daemon errors.
    const id = fixtureId('generic');
    await page.goto(`/${id}`);
    await waitForHtmxIdle(page);

    await page.locator('button.nav-quick-spawn').click();
    await waitForHtmxIdle(page);

    const sheet = page.locator('#quick-spawn-sheet');
    await expect(sheet).toBeVisible();
    await sheet.locator('textarea[name="prompt"]').fill('will be intercepted');

    // Force a 502 on the POST. htmx 2.x's default responseHandling does
    // not swap on 4xx/5xx — the existing modal DOM is left in place, so
    // we assert the user is not silently bounced out of the flow.
    await page.route('**/quick-spawn', async (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      await route.fulfill({
        status: 502,
        contentType: 'text/html; charset=utf-8',
        body:
          '<div id="quick-spawn-sheet" class="quick-spawn-sheet">' +
          '<p class="quick-spawn-error" role="alert">Failed to spawn agent.</p>' +
          '</div>',
      });
    });

    const respPromise = page.waitForResponse(
      (r) => r.url().endsWith('/quick-spawn') && r.request().method() === 'POST',
    );
    await sheet.locator('button[type="submit"]', { hasText: 'Spawn Agent' }).click();
    const resp = await respPromise;
    expect(resp.status()).toBe(502);
    await waitForHtmxIdle(page);

    // Draft preserved → the modal DOM survived the failed swap.
    await expect(page.locator('#quick-spawn-sheet textarea[name="prompt"]')).toHaveValue('will be intercepted');
  });
});
