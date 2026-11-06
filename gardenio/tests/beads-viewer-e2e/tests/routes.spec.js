// Phase T3 route smoke (gd-XRjiW082bf) of epic ka-0000004611.
//
// One `test.describe` per route. For each, assert:
//   1. HTTP status < 400 (the regression this catches: a renamed
//      template, a panic in a handler, a busted dependency on the
//      daemon — all show up as 4xx/5xx here).
//   2. The shared layout shell rendered (top-level <nav> from
//      templates/layout.html). If layout.html breaks, every route
//      regresses at once — assert it once per route to get clear
//      blame in the report.
//   3. A page-level <h1> is visible with route-specific text. The
//      h1 lives in the page template (index.html, active.html, etc.),
//      so this is the cheapest "the right page rendered" signal.
//
// Per-type detail content (cron, options list, MR pipeline status, …)
// belongs to T5/T6 and is intentionally NOT asserted here — keep T3
// boring and fast.

import { expect, test } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// Asserts the layout shell rendered: <nav> from layout.html plus a
// page-level <h1> matching `h1Pattern`. Each route's describe block
// uses this so the failure messages stay consistent.
async function assertShell(page, h1Pattern) {
  await expect(page.locator('nav').first()).toBeVisible();
  await expect(page.locator('h1').first()).toContainText(h1Pattern);
}

async function gotoOK(page, path) {
  const res = await page.goto(path);
  expect(res, `navigation response for ${path}`).not.toBeNull();
  expect(res.status(), `${path} HTTP status`).toBeLessThan(400);
  return res;
}

test.describe('GET /', () => {
  // `/` is the front door: with the garden handler wired it 302s to /garden
  // (handleHome, decision gd-e17Jl61EcP). The activity feed moved to /feed
  // and the projects index lives at `/index` (both asserted below).
  test('redirects to the /garden front door', async ({ page }) => {
    const res = await gotoOK(page, '/');
    expect(new URL(res.url()).pathname).toBe('/garden');
    const redirect = res.request().redirectedFrom();
    expect(redirect, '/ answered with a redirect').not.toBeNull();
    expect(new URL(redirect.url()).pathname).toBe('/');
    await expect(page.locator('h1').first()).toContainText(/The Garden/i);
  });
});

test.describe('GET /feed', () => {
  test('renders feed with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/feed');
    await assertShell(page, /Activity feed/i);
  });
});

test.describe('GET /index', () => {
  // The projects index moved off `/` to `/index` when the feed became
  // the home page. Same layout shell + the "Beads Viewer" h1 that used
  // to live at `/`.
  test('renders projects index with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/index');
    await assertShell(page, /Beads Viewer/i);
  });
});

test.describe('GET /search', () => {
  test('renders search with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/search');
    await assertShell(page, /Search/i);
  });
});

test.describe('GET /active', () => {
  test('renders active with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/active');
    await assertShell(page, /Active/i);
  });
});

test.describe('GET /decisions', () => {
  test('renders decisions with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/decisions');
    await assertShell(page, /Decisions/i);
  });
});

test.describe('GET /agents', () => {
  test('renders agents with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/agents');
    await assertShell(page, /Agents/i);
  });
});

test.describe('GET /advice', () => {
  test('renders advice with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/advice');
    await assertShell(page, /Advice/i);
  });
});

test.describe('GET /triggers', () => {
  test('renders triggers with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/triggers');
    await assertShell(page, /Triggers/i);
  });
});

test.describe('GET /costs', () => {
  test('renders costs with layout shell + h1', async ({ page }) => {
    await gotoOK(page, '/costs');
    await assertShell(page, /Costs/i);
  });
});

test.describe('GET /p/{project}', () => {
  test('renders project page with layout shell + h1', async ({ page }) => {
    // `gardenio` is the project the seed labels its fixtures under, so
    // there is at least one bead present and the list renders something
    // resembling real content. The fixtures themselves are seeded by T2.
    await gotoOK(page, '/p/gardenio');
    // The project page is now the "project hub" (templates/project_hub.html):
    // <h1 class="project-hub-title"> renders the bare project name (plus an
    // optional config-validity badge), no longer a "Project:" prefix.
    await assertShell(page, /gardenio/i);
  });
});

test.describe('GET /{id}', () => {
  test('renders bead-show page with layout shell + h1', async ({ page }) => {
    // Use the `generic` fixture so this test covers the simplest
    // type_generic.html path. Per-type detail assertions are T5/T6.
    const id = fixtureId('generic');
    await gotoOK(page, `/${id}`);
    await assertShell(page, /E2E fixture/i);
  });
});
