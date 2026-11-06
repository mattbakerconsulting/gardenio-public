// gd-98ZHVK6FgI — Playwright e2e suite for the constellation hover popover.
//
// Plan of record: docs/design/beads-viewer/constellation-popover.md.
// Sibling spec: popover_preview.spec.js (HTML-link popover). This one
// pins the same behavior contract for SVG <a> nodes inside
// figure.constellation on the bead-show page.
//
// Test bead: the seed creates `constellation_center` with one forward
// (parent) and one reverse (child) dep so the constellation always
// renders at least two non-center nodes — see seed/fixtures.mjs. We do
// not assume "similar" nodes exist: those come from embedding similarity
// and are non-deterministic without seeded vectors. Tests that only need
// "any node" use a class-agnostic locator that matches forward/reverse/
// similar interchangeably.
//
// All assertions wait on the popover element transitioning state — no
// fixed sleeps for behavior. The preview script's HOVER_MS / HIDE_MS
// are 150 ms each, so Playwright's default expect timeout (5 s) is
// comfortable headroom.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

const POPOVER = '#bead-preview-popover';
const CONSTELLATION = 'figure.constellation';
const CENTER_ANCHOR = `${CONSTELLATION} .constellation-center > a`;
// Class-agnostic non-center node anchor. Forward/reverse are
// deterministic via the seeded deps; similar is opportunistic. `nth(0)`
// pins to the first painted non-center node so timing is stable.
const NODE_ANCHOR = `${CONSTELLATION} .constellation-node > a`;

async function expectPopoverVisible(page) {
  await expect(page.locator(POPOVER)).toBeVisible();
}

async function expectPopoverHidden(page) {
  await expect(page.locator(POPOVER)).toHaveJSProperty('hidden', true);
}

// Open the bead-show page for the constellation_center fixture and wait
// for the constellation SVG to be present. The viewer hydrates dep
// targets server-side, so by the time the page is loaded the
// constellation has all its nodes — no htmx idle dance required for the
// SVG itself, but we wait for any auto-fired htmx prefetches to settle
// so they don't race the popover's own fetch counter.
async function openConstellationPage(page) {
  const id = fixtureId('constellation_center');
  // ?constellation=expanded opens the <details class="constellation-reveal">
  // wrapper on first render (bead_show.html sets `open` when
  // ConstellationDefaultState=="expanded"). Without this, the SVG is
  // collapsed by default and every hover assertion below fails because
  // the figure is `display:none` inside the closed <details>.
  await page.goto('/' + id + '?constellation=expanded');
  // Sanity: the figure must render. If buildConstellation returned an
  // empty view we'd silently lose every assertion below.
  await expect(page.locator(CONSTELLATION)).toBeVisible();
  await expect(page.locator(NODE_ANCHOR).first()).toBeVisible();
  await waitForHtmxIdle(page);
  return id;
}

// hrefBeadId mirrors the JS `beadIdOf` extraction: SVG <a>.href is an
// SVGAnimatedString, so we read the href attribute directly. Returns
// the bare `kd-...` segment (no base path).
async function hrefBeadId(locator) {
  const raw = await locator.getAttribute('href');
  if (!raw) throw new Error('anchor has no href');
  // Strip leading slash + optional base path. The viewer under test
  // boots with BasePath="" (see playwright.config.js webServer env), so
  // we just trim the leading slash.
  return raw.replace(/^\/+/, '').replace(/\/+$/, '');
}

test.describe('constellation-node hover preview popover', () => {
  test('popover_appears_on_constellation_node_hover renders for the hovered bead', async ({
    page,
  }) => {
    await openConstellationPage(page);
    const node = page.locator(NODE_ANCHOR).first();
    const expectedID = await hrefBeadId(node);

    await node.hover();
    await expectPopoverVisible(page);

    const popover = page.locator(POPOVER);
    await expect(popover.locator('.bead-preview')).toHaveAttribute(
      'data-bead-id',
      expectedID,
    );
    // bead_preview.html guarantees a type tag, a title, and the bead
    // id-tag are populated for any real bead. Excerpt is conditional
    // on a non-empty description; the seeded parent/child both have
    // descriptions, so this is safe.
    await expect(popover.locator('.tag-type')).not.toBeEmpty();
    await expect(popover.locator('.bead-preview-title')).not.toBeEmpty();
    await expect(popover.locator('.bead-preview-id')).toHaveText(expectedID);
  });

  test('popover_appears_on_constellation_center renders the current bead', async ({
    page,
  }) => {
    const centerID = await openConstellationPage(page);
    const center = page.locator(CENTER_ANCHOR);

    await center.hover();
    await expectPopoverVisible(page);

    const popover = page.locator(POPOVER);
    // The center anchor links to `/{id}` of the bead we are already on —
    // hovering it produces the *same* bead's preview. That is feature,
    // not bug: it confirms parity with HTML-link popover (gd-98ZHVK6FgI
    // acceptance A2).
    await expect(popover.locator('.bead-preview')).toHaveAttribute(
      'data-bead-id',
      centerID,
    );
    await expect(popover.locator('.bead-preview-id')).toHaveText(centerID);
  });

  test('popover_dismisses_when_leaving_constellation_node hides within 500 ms', async ({
    page,
  }) => {
    await openConstellationPage(page);
    const node = page.locator(NODE_ANCHOR).first();
    await node.hover();
    await expectPopoverVisible(page);

    // Move the mouse far from both the node and the popover. (0,0) is
    // outside the viewer chrome and outside any constellation geometry.
    await page.mouse.move(0, 0);
    await expectPopoverHidden(page);
  });

  test('popover_persists_when_moved_onto_popover_from_constellation', async ({
    page,
  }) => {
    await openConstellationPage(page);
    const node = page.locator(NODE_ANCHOR).first();
    await node.hover();
    await expectPopoverVisible(page);

    // Drift onto the popover element. Its mouseenter handler cancels the
    // pending hide — same contract as the HTML-link case.
    await page.locator(POPOVER).hover();
    // 2× HIDE_MS: any pending hide would have fired by now if the
    // mouseenter handler regressed.
    await page.waitForTimeout(300);
    await expect(page.locator(POPOVER)).toBeVisible();
  });

  test('popover_constellation_dismisses_on_escape hides on Escape', async ({
    page,
  }) => {
    await openConstellationPage(page);
    const node = page.locator(NODE_ANCHOR).first();
    await node.hover();
    await expectPopoverVisible(page);

    await page.keyboard.press('Escape');
    await expectPopoverHidden(page);
  });

  test('popover_constellation_caches_within_page issues exactly one preview request per id', async ({
    page,
  }) => {
    await openConstellationPage(page);
    const node = page.locator(NODE_ANCHOR).first();
    const id = await hrefBeadId(node);

    // Scope the request counter to /b/<id>/preview so unrelated htmx
    // prefetches (the constellation node also carries an
    // hx-trigger="mouseover once" hx-get to warm the navigation, which
    // hits a different URL — the bead-show page itself, not the preview
    // fragment) cannot inflate the count.
    const previewURL = `/b/${id}/preview`;
    let count = 0;
    page.on('request', (req) => {
      const u = new URL(req.url());
      if (u.pathname === previewURL) count++;
    });

    await node.hover();
    await expectPopoverVisible(page);
    expect(count, 'one preview request after first hover').toBe(1);

    // Dismiss, re-hover. Cache hit must not fire a second fetch.
    await page.mouse.move(0, 0);
    await expectPopoverHidden(page);
    await node.hover();
    await expectPopoverVisible(page);
    // 200 ms slack so a (regressed) second request would have time to
    // fire before we assert.
    await page.waitForTimeout(200);
    expect(count, 'still one preview request after second hover (cache hit)').toBe(1);
  });

  test('popover_constellation_keyboard_focus_shows_preview when an SVG <a> gains focus', async ({
    page,
  }) => {
    await openConstellationPage(page);
    const node = page.locator(NODE_ANCHOR).first();
    const expectedID = await hrefBeadId(node);

    // SVGAElement is focusable and `.focus()` drives the same focusin
    // path that a real Tab keypress would. Equivalent to keyboard
    // navigation but independent of how many focusable elements precede
    // the constellation in the page tab order — keeps the test
    // deterministic across viewer chrome changes.
    await node.focus();
    await expectPopoverVisible(page);
    await expect(page.locator(`${POPOVER} .bead-preview`)).toHaveAttribute(
      'data-bead-id',
      expectedID,
    );
  });

  test('popover_constellation_no_native_tooltip_race shows the popover at the hover-intent threshold', async ({
    page,
  }) => {
    await openConstellationPage(page);
    const node = page.locator(NODE_ANCHOR).first();

    // The popover script's HOVER_MS is 150 ms. Hovering and asserting
    // visibility right after the script's own setTimeout fires is the
    // tightest race window. If a regression made the popover flash and
    // hide (e.g. the SVG <title> handler stealing focus, or a duplicate
    // hide-on-mouseout firing), this test would fail because the
    // popover would not be visible at the 150 ms mark.
    //
    // We cannot directly observe the browser's native <title> tooltip
    // from Playwright — there is no DOM API for it — so the practical
    // assertion is "the custom popover wins": it is visible, and it is
    // the topmost role=tooltip element on the page. That covers the
    // observable user-facing failure mode for a tooltip race.
    await node.hover();
    // Wait *just* past HOVER_MS so the show is committed but no later
    // than HIDE_MS would fire from a stray mouseout. expect(...).toBeVisible
    // already polls, but this explicit window pins the timing intent.
    await page.waitForTimeout(180);
    await expectPopoverVisible(page);

    const popoverInfo = await page.evaluate((sel) => {
      const el = document.querySelector(sel);
      if (!el) return null;
      const rect = el.getBoundingClientRect();
      return {
        role: el.getAttribute('role'),
        hidden: el.hidden,
        width: rect.width,
        height: rect.height,
      };
    }, POPOVER);
    expect(popoverInfo).not.toBeNull();
    expect(popoverInfo.role, 'popover preserves role=tooltip').toBe('tooltip');
    expect(popoverInfo.hidden, 'popover stays unhidden through threshold').toBe(false);
    expect(popoverInfo.width, 'popover has rendered geometry').toBeGreaterThan(0);
    expect(popoverInfo.height, 'popover has rendered geometry').toBeGreaterThan(0);

    // Belt-and-braces: 200 ms later (well past any stray hide
    // debounce), popover must still be visible. A flash regression
    // — appear-then-vanish at the 150 ms mark — would fail here even
    // if the snapshot above happened to land in the visible window.
    await page.waitForTimeout(200);
    await expect(page.locator(POPOVER)).toBeVisible();
  });
});
