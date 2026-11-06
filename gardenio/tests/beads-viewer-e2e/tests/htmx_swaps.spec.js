// Phase T8 (gd-jkUTKyNIpS) — htmx partial swaps. Plan of record:
// `gardenio/plans/beads-viewer-playwright-tests.md`.
//
// What this guards against:
//
//   - hx-target / hx-swap typos that turn a partial swap into a full
//     page navigation (the regression that lets a "comment posted"
//     flash render as a re-rendered chrome-and-all bead page).
//   - partial endpoints (`GET /b/{id}/comments`, `GET /b/{id}/deps`)
//     accidentally emitting the layout — fragments only, no <html>,
//     no top-<nav>, no <head>.
//   - the htmx:afterSwap lifecycle event not firing on the target the
//     attribute pointed at — i.e. the swap landed but the script-side
//     listeners (drafts.js, future hooks) never see it.
//   - top-nav / chrome state being clobbered by an in-page swap.
//
// Strategy: load a seeded bead detail page, trigger the swap explicitly
// via `htmx.ajax(...)` (same call path the form post and the 30s poll
// take), and assert four things every time:
//
//   1. The Performance API navigation entry has not changed —
//      `performance.timing.navigationStart` is stable across the swap,
//      proving the browser did not reload the document.
//   2. A page-scope sentinel set before the swap is still in place
//      after — a full-page nav would wipe `window.__navSentinel`.
//   3. `htmx:afterSwap` fired with the swap target matching the id we
//      asked for.
//   4. The swap target id is still present and still inside <main>; the
//      top-<nav> chrome is still rendered (active-tab / nav state
//      persists across swaps).
//
// Plus two pure-HTTP checks via `page.request.get(...)` that the partial
// endpoints emit fragments — explicitly NO `<html>`, `<head>`, or top
// `<nav>` markup — only the inner section the bead-show layout splices
// in. These are the request-shape contract that browser-side hx-target
// assertions rely on.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

// Pull `BasePath` from the htmx attributes the server actually rendered,
// so tests work regardless of `BEADS_VIEWER_BASE_PATH` / `--base-path`.
async function readHxGet(page, selector) {
  return page.locator(selector).first().getAttribute('hx-get');
}

// Install a navigation-detector and an htmx:afterSwap collector before
// triggering a swap. Records:
//   - window.__navSentinel — stays "preserved" iff no full reload.
//   - window.__navStartBefore — initial navigationStart, used to prove
//     that no document load fires.
//   - window.__afterSwapTargets — array of id|tagName per fired event.
//   - window.__beforeSwapTargets — same shape for htmx:beforeSwap.
async function instrumentSwap(page) {
  await page.evaluate(() => {
    window.__navSentinel = 'preserved';
    window.__navStartBefore = performance.timing
      ? performance.timing.navigationStart
      : performance.getEntriesByType('navigation')[0]?.startTime;
    window.__afterSwapTargets = [];
    window.__beforeSwapTargets = [];
    document.body.addEventListener('htmx:afterSwap', (ev) => {
      const t = ev.target;
      window.__afterSwapTargets.push(t?.id || t?.tagName || '');
    });
    document.body.addEventListener('htmx:beforeSwap', (ev) => {
      const t = ev.target;
      window.__beforeSwapTargets.push(t?.id || t?.tagName || '');
    });
  });
}

async function readSwapState(page) {
  return page.evaluate(() => {
    const after = window.__afterSwapTargets || [];
    const before = window.__beforeSwapTargets || [];
    return {
      sentinel: window.__navSentinel,
      navStartBefore: window.__navStartBefore,
      navStartAfter: performance.timing
        ? performance.timing.navigationStart
        : performance.getEntriesByType('navigation')[0]?.startTime,
      after,
      before,
    };
  });
}

test.describe('htmx partial swaps — comments', () => {
  test('GET /b/{id}/comments returns a fragment, not a full page', async ({ page, request }) => {
    const id = fixtureId('generic');
    // Load the page once so we can read the server-rendered hx-get URL
    // (it carries the configured BasePath prefix).
    await page.goto(`/${id}`);
    await waitForHtmxIdle(page);

    const url = await readHxGet(page, '#comments-section');
    expect(url, 'comments-section advertises an hx-get URL').toBeTruthy();
    expect(url).toMatch(/\/b\/[^/]+\/comments$/);

    const res = await request.get(url);
    expect(res.status(), 'comments fragment HTTP status').toBeLessThan(400);
    expect(res.headers()['content-type'] || '').toMatch(/text\/html/);
    const body = await res.text();

    // Must contain the partial's outer wrapper.
    expect(body).toContain('id="comments-section"');
    expect(body).toMatch(/<h2>Comments/);

    // Must NOT contain layout markers — those would mean the partial
    // accidentally inherited the full-page template.
    expect(body, 'fragment must not include <html>').not.toMatch(/<html[\s>]/i);
    expect(body, 'fragment must not include <head>').not.toMatch(/<head[\s>]/i);
    expect(body, 'fragment must not include <body>').not.toMatch(/<body[\s>]/i);
    expect(body, 'fragment must not include the top-nav chrome').not.toMatch(/<nav[\s>][^]*Decisions[^]*<\/nav>/i);
  });

  test('htmx swap of #comments-section replaces target, fires afterSwap, no full reload', async ({ page }) => {
    const id = fixtureId('generic');
    await page.goto(`/${id}`);
    await waitForHtmxIdle(page);

    // Sanity: the swap target the partial advertises is what we expect.
    const target = page.locator('#comments-section');
    await expect(target).toBeVisible();
    await expect(target).toHaveAttribute('hx-swap', 'outerHTML');

    // Capture the original DOM-node identity of #comments-section.
    // `hx-swap=outerHTML` replaces the element, so the post-swap node
    // should NOT === the pre-swap node.
    const tagBefore = await page.evaluate(() => {
      const el = document.getElementById('comments-section');
      el.setAttribute('data-pre-swap-marker', 'mark-1');
      return el.tagName;
    });
    expect(tagBefore).toBe('SECTION');

    // Sibling node we expect the swap to leave alone (chrome-state
    // persistence sub-assertion #4).
    const depsTagBefore = await page.evaluate(() => {
      const el = document.getElementById('deps-section');
      if (el) el.setAttribute('data-pre-swap-marker', 'deps-mark');
      return !!el;
    });

    await instrumentSwap(page);

    // Trigger the swap explicitly via htmx.ajax — identical wire shape
    // to the form-post and the 30s poll.
    const url = await readHxGet(page, '#comments-section');
    await page.evaluate(({ url }) => {
      return window.htmx.ajax('GET', url, {
        target: '#comments-section',
        swap: 'outerHTML',
      });
    }, { url });
    await waitForHtmxIdle(page);

    const state = await readSwapState(page);

    // (1) Performance entry didn't change → no document reload.
    expect(state.navStartAfter, 'navigationStart unchanged across swap')
      .toBe(state.navStartBefore);
    // (2) Page-scope JS state survived → no full nav.
    expect(state.sentinel).toBe('preserved');
    // (3) htmx:afterSwap fired and named our element.
    expect(state.after.length, 'htmx:afterSwap fired at least once').toBeGreaterThan(0);
    expect(state.after).toContain('comments-section');
    expect(state.before).toContain('comments-section');

    // (4a) Element with the same id is still present after outerHTML
    // swap, but it is a fresh node — the pre-swap marker we stamped
    // should NOT be on the replacement.
    await expect(page.locator('#comments-section')).toBeVisible();
    const stillMarked = await page.locator('#comments-section').getAttribute('data-pre-swap-marker');
    expect(stillMarked, 'outerHTML swap replaced the element (no marker)').toBeNull();

    // (4b) Siblings outside the swap target are untouched. If
    // #deps-section existed pre-swap, its marker should survive.
    if (depsTagBefore) {
      const depsStillMarked = await page.locator('#deps-section').getAttribute('data-pre-swap-marker');
      expect(depsStillMarked, 'sibling outside swap target preserved').toBe('deps-mark');
    }

    // (4c) Top-<nav> chrome still rendered — active-tab/nav state
    // persists across swaps because the layout was never re-rendered.
    // Assert CORE links only: this page loads in simple mode (no vibe
    // email), so the vibe-only links (Ratings, Advice, …) are gated out
    // by {{if .VibeMode}} (ka-0000003340). Agents + Decisions are core
    // (as is Active since ka-0000002647).
    const nav = page.locator('nav').first();
    await expect(nav).toBeVisible();
    await expect(nav.getByRole('link', { name: 'Agents', exact: true })).toBeVisible();
    await expect(nav.getByRole('link', { name: 'Decisions', exact: true })).toBeVisible();

    // (4d) After-swap target lives back inside <main> (fragment was
    // spliced in, not appended past the end of the document).
    const inMain = await page.evaluate(() => {
      const el = document.getElementById('comments-section');
      const main = document.querySelector('main') || document.body;
      return main.contains(el);
    });
    expect(inMain, 'comments-section still inside <main> after swap').toBe(true);
  });
});

test.describe('htmx partial swaps — deps', () => {
  test('GET /b/{id}/deps returns a fragment, not a full page', async ({ page, request }) => {
    // Pick any fixture that's seeded — `generic` is fine; deps may be
    // empty, in which case the partial still renders the section
    // wrapper and the empty-state em-dash.
    const id = fixtureId('generic');
    const url = `/b/${id}/deps`;

    const res = await request.get(url);
    expect(res.status(), 'deps fragment HTTP status').toBeLessThan(400);
    const body = await res.text();

    expect(body).toContain('id="deps-section"');
    expect(body).toMatch(/<h2>Dependencies<\/h2>/);

    expect(body, 'fragment must not include <html>').not.toMatch(/<html[\s>]/i);
    expect(body, 'fragment must not include <head>').not.toMatch(/<head[\s>]/i);
    expect(body, 'fragment must not include <body>').not.toMatch(/<body[\s>]/i);
    expect(body, 'fragment must not include the top-nav chrome').not.toMatch(/<nav[\s>][^]*Decisions[^]*<\/nav>/i);
  });

  test('manual htmx swap into #deps-section uses the deps endpoint and fires afterSwap', async ({ page }) => {
    // Deps don't carry an hx-trigger on the section itself, but the
    // server still has to honour an explicit htmx.ajax targeting it
    // (this is the call path drafts/keys/future hooks would use).
    // This test catches a regression where a route rename or a partial
    // wrapper rename breaks programmatic swap targeting.
    const id = fixtureId('generic');
    await page.goto(`/${id}`);
    await waitForHtmxIdle(page);

    const deps = page.locator('#deps-section');
    await expect(deps).toBeVisible();

    // Derive the deps URL from the comments section's hx-get so the
    // basePath prefix is honoured automatically.
    const commentsUrl = await readHxGet(page, '#comments-section');
    const depsUrl = commentsUrl.replace(/\/comments$/, '/deps');

    await instrumentSwap(page);

    await page.evaluate((url) => {
      return window.htmx.ajax('GET', url, {
        target: '#deps-section',
        swap: 'outerHTML',
      });
    }, depsUrl);
    await waitForHtmxIdle(page);

    const state = await readSwapState(page);
    expect(state.navStartAfter).toBe(state.navStartBefore);
    expect(state.sentinel).toBe('preserved');
    expect(state.after, 'htmx:afterSwap targeted deps-section').toContain('deps-section');
    await expect(page.locator('#deps-section')).toBeVisible();
    await expect(page.locator('#deps-section h2')).toHaveText('Dependencies');

    // Top-nav chrome still rendered → no full reload.
    await expect(page.locator('nav').first()).toBeVisible();
  });
});
