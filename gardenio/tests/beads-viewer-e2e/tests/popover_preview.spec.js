// gd-vzV5yoVcAx — Playwright e2e suite for the bead-link hover preview popover.
//
// Plan of record: docs/design/beads-viewer/popover-preview.md.
// Client JS: gardenio/controller/cmd/beads-viewer/templates/static/preview_popover.js.
// Server endpoint: gardenio/controller/cmd/beads-viewer/bead_preview.go (`GET /b/{id}/preview`).
//
// Decision: harness lives here (Approach (a) from the bead) — the existing
// beads-viewer-e2e suite already boots the Go binary against a real beads
// daemon and seeds canonical fixtures. The hover popover is browser-level
// behavior (mouseover/focus/keyboard, viewport-aware positioning, in-page
// fetch caching) that is best exercised through a real Chromium against a
// real viewer, not through a Go-level chromedp driver.
//
// Strategy: every test injects a controlled <a> anchor into a loaded
// viewer page via `page.evaluate`. The popover script (loaded via
// layout.html on every page) delegates to `document`, so any anchor whose
// pathname matches `gd-XXX` triggers the preview path — regardless of how
// it ended up in the DOM. This keeps the suite independent of the
// description-rendering path (covered by markdown_linkify.spec.js) and
// lets us pin the link's viewport position for the clamp test.
//
// All assertions wait on the popover element transitioning state — no
// fixed sleeps. The preview script's HOVER_MS / HIDE_MS are 150 ms each,
// so Playwright's default expect timeout (5 s) is comfortable headroom
// for non-flake across 5+ consecutive runs.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

const POPOVER = '#bead-preview-popover';
const NOT_A_REAL_BEAD = 'gd-doesnotexist';

// installAnchor injects a single <a href="/<id>"> into the body at a
// pinned viewport position. Returns a Locator targeting the anchor so
// callers can hover/focus it.
async function installAnchor(page, { id, label, top = '50vh', left = '50vw' }) {
  await page.evaluate(
    ({ id, label, top, left }) => {
      // Remove a previous anchor from a sibling test in this spec so each
      // test starts from a clean DOM. Tests are also isolated by
      // Playwright's per-test browser context, but `reuseExistingServer`
      // means we are extra cautious.
      document.querySelectorAll('a[data-popover-test="1"]').forEach((el) => el.remove());
      const a = document.createElement('a');
      a.setAttribute('data-popover-test', '1');
      a.id = 'popover-test-link';
      a.href = '/' + id;
      a.textContent = label || id;
      a.style.position = 'fixed';
      a.style.top = top;
      a.style.left = left;
      a.style.zIndex = '999';
      a.style.background = '#fff';
      a.style.padding = '4px 8px';
      a.style.border = '1px solid #888';
      // tabindex defaults to 0 for anchors with href; explicit just in case.
      document.body.appendChild(a);
    },
    { id, label, top, left },
  );
  return page.locator('#popover-test-link');
}

async function expectPopoverVisible(page) {
  // The popover is positioned with fixed coordinates and starts as
  // `hidden`; once the fetch lands the script clears the attribute.
  await expect(page.locator(POPOVER)).toBeVisible();
}

async function expectPopoverHidden(page) {
  // `hidden` attribute is what the script toggles; use that as the
  // canonical signal so layout-side opacity transitions cannot mask a
  // logic regression.
  await expect(page.locator(POPOVER)).toHaveJSProperty('hidden', true);
}

test.describe('bead-link hover preview popover', () => {
  test('popover_appears_on_hover renders type, title, excerpt within 500 ms', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');
    const link = await installAnchor(page, { id, label: id });
    await link.hover();
    await expectPopoverVisible(page);

    const popover = page.locator(POPOVER);
    // Server-rendered fragment carries .bead-preview with the bead id.
    await expect(popover.locator('.bead-preview')).toHaveAttribute('data-bead-id', id);
    // type, title, and the bead id-tag are guaranteed slots in
    // bead_preview.html. The excerpt is conditional on a non-empty
    // description; the schedule fixture always has one (see seed/fixtures.mjs).
    // gd-vK3RKOkRPW: fixture migrated from type=schedule to type=trigger
    // after Phase 3b/D10 hard-removed `kd create --type=schedule`.
    await expect(popover.locator('.tag-type')).toHaveText('trigger');
    await expect(popover.locator('.bead-preview-title')).not.toBeEmpty();
    await expect(popover.locator('.bead-preview-excerpt')).not.toBeEmpty();
    await expect(popover.locator('.bead-preview-id')).toHaveText(id);
  });

  test('popover_dismisses_on_mouseout hides within 500 ms', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');
    const link = await installAnchor(page, { id });
    await link.hover();
    await expectPopoverVisible(page);

    // Move the cursor far from both the link and the popover. Hovering
    // the page corner is sufficient — the popover script's mouseout
    // handler fires off the link, schedules hide, and 150 ms later the
    // popover becomes hidden.
    await page.mouse.move(0, 0);
    await expectPopoverHidden(page);
  });

  test('popover_persists_when_moved_onto_popover stays open', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');
    const link = await installAnchor(page, { id });
    await link.hover();
    await expectPopoverVisible(page);

    // Move from the link to the popover. The popover element installs
    // its own mouseenter handler which cancels the pending hide. Hover
    // the popover and assert it remains visible after the HIDE_MS
    // window has elapsed.
    await page.locator(POPOVER).hover();
    // Wait long enough that any pending hide debounce would have fired.
    // 300 ms = 2× HIDE_MS — safe margin without padding test wall time.
    await page.waitForTimeout(300);
    await expect(page.locator(POPOVER)).toBeVisible();
  });

  test('popover_dismisses_on_escape hides on Escape', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');
    const link = await installAnchor(page, { id });
    await link.hover();
    await expectPopoverVisible(page);

    await page.keyboard.press('Escape');
    await expectPopoverHidden(page);
  });

  test('popover_keyboard_focus_shows_preview shows when link gets focus', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');
    const link = await installAnchor(page, { id });

    // focus() drives the focusin path in preview_popover.js. Equivalent
    // to a Tab landing on the link from a real keyboard nav, but
    // independent of the surrounding page's tab order — which keeps the
    // test deterministic regardless of how many focusable elements the
    // index page renders today.
    await link.focus();
    await expectPopoverVisible(page);
    await expect(page.locator(`${POPOVER} .bead-preview`)).toHaveAttribute('data-bead-id', id);
  });

  test('popover_404_renders_notfound_state for an unknown id', async ({ page }) => {
    await page.goto('/feed');
    const link = await installAnchor(page, { id: NOT_A_REAL_BEAD, label: NOT_A_REAL_BEAD });
    await link.hover();
    await expectPopoverVisible(page);

    const popover = page.locator(POPOVER);
    // bead_preview.html `bead_preview_notfound` template renders the
    // missing-state article with a "not found" sentence. Match
    // case-insensitively because the template wording could be tuned
    // (e.g. capitalisation, wording) without changing the contract.
    await expect(popover.locator('.bead-preview-missing')).toBeVisible();
    await expect(popover).toContainText(/not found/i);
  });

  test('popover_caches_within_page issues exactly one preview request per id', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');

    // page.on('request') fires for every network request the page makes.
    // The popover script caches resolved Promises per id, so the second
    // hover must NOT trigger a second `/b/<id>/preview` fetch. We count
    // requests scoped to the preview path for this exact id only — other
    // fixtures or htmx polling cannot inflate the count.
    const previewURL = `/b/${id}/preview`;
    let count = 0;
    page.on('request', (req) => {
      const u = new URL(req.url());
      if (u.pathname === previewURL) count++;
    });

    const link = await installAnchor(page, { id });
    // First hover triggers fetch + render.
    await link.hover();
    await expectPopoverVisible(page);
    expect(count, 'one preview request after first hover').toBe(1);

    // Move away to dismiss the popover, then hover again. The script
    // re-runs `fetchPreview` which short-circuits via the cache Map and
    // resolves with the cached HTML — no second network request.
    await page.mouse.move(0, 0);
    await expectPopoverHidden(page);
    await link.hover();
    await expectPopoverVisible(page);
    // 200 ms slack so a (regressed) second request would have time to
    // fire before we assert.
    await page.waitForTimeout(200);
    expect(count, 'still one preview request after second hover (cache hit)').toBe(1);
  });

  test('popover_position_clamps_to_viewport stays inside the window near the right edge', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');

    // Pin the link at the viewport's right edge. The popover's
    // intrinsic max-width is 380 px (set by preview_popover.js) — when
    // anchored at right: 0 the naive left = rect.left would put the
    // popover off-screen. The script's position() clamps left to
    // `vw - pw - MARGIN` (MARGIN = 8 px) so the popover stays inside.
    const link = await installAnchor(page, {
      id,
      label: id,
      top: '50vh',
      // Anchor at the viewport's right edge (8 px gutter just to keep
      // the link itself inside, otherwise hover targeting becomes
      // browser-dependent).
      left: 'calc(100vw - 60px)',
    });
    await link.hover();
    await expectPopoverVisible(page);

    const { popoverRect, vw, vh } = await page.evaluate((sel) => {
      const el = document.querySelector(sel);
      const r = el.getBoundingClientRect();
      return {
        popoverRect: { left: r.left, top: r.top, right: r.right, bottom: r.bottom, width: r.width, height: r.height },
        vw: window.innerWidth,
        vh: window.innerHeight,
      };
    }, POPOVER);

    // All four edges must lie within the viewport. The MARGIN constant
    // in the script is 8 px — assert the looser invariant (>= 0,
    // <= vw/vh) so a future MARGIN tweak does not invalidate the test.
    expect(popoverRect.left, 'popover left within viewport').toBeGreaterThanOrEqual(0);
    expect(popoverRect.top, 'popover top within viewport').toBeGreaterThanOrEqual(0);
    expect(popoverRect.right, 'popover right within viewport').toBeLessThanOrEqual(vw);
    expect(popoverRect.bottom, 'popover bottom within viewport').toBeLessThanOrEqual(vh);
    // Sanity — the popover actually rendered with non-zero size.
    expect(popoverRect.width).toBeGreaterThan(0);
    expect(popoverRect.height).toBeGreaterThan(0);
  });

  // ka-0000006720 regression sweep + coordination. Each test below pins a
  // behavior that another popover or page-level handler could otherwise
  // silently break.

  test('popover_yields_to_bead_popover_open_event hides on coordination event', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');
    const link = await installAnchor(page, { id });
    await link.hover();
    await expectPopoverVisible(page);

    // Another popover (priority/assignee inline edit, future overlay)
    // declares it is opening. Preview must yield immediately so two
    // popovers never overlap.
    await page.evaluate(() => {
      document.dispatchEvent(
        new CustomEvent('bead-popover-open', { detail: { source: 'priority-edit' } }),
      );
    });
    await expectPopoverHidden(page);
  });

  test('popover_ignores_self_dispatched_coordination keeps preview visible', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');
    const link = await installAnchor(page, { id });
    await link.hover();
    await expectPopoverVisible(page);

    // detail.source === 'preview' is the contract for self-dispatch —
    // the preview should ignore it so a future emit-on-show does not
    // hide its own popover.
    await page.evaluate(() => {
      document.dispatchEvent(
        new CustomEvent('bead-popover-open', { detail: { source: 'preview' } }),
      );
    });
    // Wait one tick so the (rejected) hide has time to settle.
    await page.waitForTimeout(50);
    await expect(page.locator(POPOVER)).toBeVisible();
  });

  test('popover_escape_does_not_steal_focus leaves the link focused', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');
    const link = await installAnchor(page, { id });
    await link.focus();
    await expectPopoverVisible(page);

    await page.keyboard.press('Escape');
    await expectPopoverHidden(page);

    // Esc must hide the preview without yanking focus off the link.
    // A regression that called link.blur() inside hide() would fail
    // this assertion.
    const focusedId = await page.evaluate(() => document.activeElement && document.activeElement.id);
    expect(focusedId).toBe('popover-test-link');
  });

  test('popover_does_not_block_tab_navigation lets focus advance through links', async ({ page }) => {
    const id = fixtureId('schedule');
    await page.goto('/feed');

    // Inject two adjacent test links so we can Tab between them.
    await page.evaluate((id) => {
      document.querySelectorAll('a[data-popover-test="1"]').forEach((el) => el.remove());
      const wrap = document.createElement('div');
      wrap.style.position = 'fixed';
      wrap.style.top = '50vh';
      wrap.style.left = '50vw';
      wrap.style.zIndex = '999';
      wrap.style.background = '#fff';
      wrap.style.padding = '4px 8px';
      ['popover-test-link-a', 'popover-test-link-b'].forEach((linkId) => {
        const a = document.createElement('a');
        a.setAttribute('data-popover-test', '1');
        a.id = linkId;
        a.href = '/' + id;
        a.textContent = id;
        a.style.marginRight = '8px';
        wrap.appendChild(a);
      });
      document.body.appendChild(wrap);
    }, id);

    await page.locator('#popover-test-link-a').focus();
    await expectPopoverVisible(page);

    // Tab must move focus forward despite the popover floating over the
    // page. role=tooltip element is not focusable and has no tabindex,
    // so the natural tab order should hop link-a → link-b.
    await page.keyboard.press('Tab');
    const focusedId = await page.evaluate(() => document.activeElement && document.activeElement.id);
    expect(focusedId).toBe('popover-test-link-b');
  });
});
