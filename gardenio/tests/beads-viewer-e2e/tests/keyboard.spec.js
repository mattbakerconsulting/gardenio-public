// Phase T12 (gd-O9XsBujIfl) of epic ka-0000004611 — keyboard shortcuts
// behavioral coverage. Plan of record:
// `gardenio/plans/beads-viewer-playwright-tests.md`.
//
// Source under test: `gardenio/controller/cmd/beads-viewer/templates/static/keys.js`.
// The script is included as a deferred script in `layout.html` and binds
// a single `keydown` listener on `document`. There are no Go unit tests
// for this surface — it is pure client-side — so this spec is the
// canary for regressions in the shortcut wiring.
//
// Shortcuts under test (per keys.js header):
//   ?              - toggle the help overlay
//   Esc            - close the overlay / blur input
//   /              - focus the search input (.nav-search input[name="q"])
//   j / k          - move focus down / up the visible bead-row list
//   Enter          - open the focused bead (when a row is focused)
//   c              - on bead detail, focus #comment-text
//   g a / s / d / c / v / i / h - navigate to /agents /triggers /decisions
//                                /costs /advice /active /
//   (any of the above while focus is in an editable element) - no-op
//
// Strategy notes:
//   - The agents, triggers, decisions, costs, advice, active, and
//     project pages all render `.bead-row` lists, so j/k/Enter is
//     exercised on `/agents` (cheap fixture set, deterministic order).
//   - g-prefix navigation needs a 1s timeout to clear in keys.js; we
//     dispatch the second key immediately, well within that window.
//   - The help overlay uses a <dialog> element. It is appended to <body>
//     lazily on first ?-press, so the locator must wait for it.
//   - The Enter-in-input guard is exercised on the search input — the
//     spec asserts both that the j/k/g shortcuts do NOT fire while the
//     search input has focus AND that pressing Enter inside the search
//     input does not invoke `openFocused()` (the form submit is the
//     expected behavior, but we only assert the shortcut path didn't run
//     — submitting the form mid-test would navigate away).

import { test, expect } from '@playwright/test';

import { waitForHtmxIdle } from './_helpers.js';

test.describe('keyboard shortcuts — help overlay', () => {
  test('? opens the help overlay; Esc closes it', async ({ page }) => {
    await page.goto('/feed');
    await waitForHtmxIdle(page);

    // Overlay is created lazily — not in the DOM until ? is pressed.
    await expect(page.locator('dialog.kbd-help')).toHaveCount(0);

    // Press ? — Shift+/ on a US keyboard. Playwright's `keyboard.press`
    // accepts the literal character.
    await page.keyboard.press('Shift+Slash');

    const dialog = page.locator('dialog.kbd-help');
    await expect(dialog).toBeVisible();
    // <dialog>.open === true when shown via showModal() / setAttribute.
    await expect(dialog).toHaveJSProperty('open', true);

    // Body should reflect the canonical bindings — the overlay is the
    // user-facing "what shortcuts exist" surface and a stale table is
    // its own regression class.
    await expect(dialog.getByRole('heading', { name: 'Keyboard shortcuts' })).toBeVisible();

    await page.keyboard.press('Escape');
    await expect(dialog).toHaveJSProperty('open', false);
  });

  test('? toggles open/closed', async ({ page }) => {
    await page.goto('/feed');
    await waitForHtmxIdle(page);

    await page.keyboard.press('Shift+Slash');
    const dialog = page.locator('dialog.kbd-help');
    await expect(dialog).toHaveJSProperty('open', true);

    // Pressing ? again should close it (toggleHelp() in keys.js).
    await page.keyboard.press('Shift+Slash');
    await expect(dialog).toHaveJSProperty('open', false);
  });
});

test.describe('keyboard shortcuts — focus search', () => {
  test('/ focuses the nav search input', async ({ page }) => {
    await page.goto('/feed');
    await waitForHtmxIdle(page);

    const search = page.locator('nav .nav-search input[name="q"]');
    await expect(search).toBeVisible();

    // Sanity: search isn't focused until we ask for it.
    await expect(search).not.toBeFocused();

    await page.keyboard.press('Slash');
    await expect(search).toBeFocused();
  });

  test('Esc on a non-empty search clears the value first, second Esc blurs', async ({ page }) => {
    // Matches the "/" → type → Esc → Esc unwind from GitHub/Linear keys.js
    // ships (search input with content: first Esc clears, second Esc blurs).
    await page.goto('/feed');
    await waitForHtmxIdle(page);

    const search = page.locator('nav .nav-search input[name="q"]');
    await search.focus();
    await search.fill('hello');
    await expect(search).toBeFocused();

    // First Esc clears the value but keeps the input focused.
    await page.keyboard.press('Escape');
    await expect(search).toBeFocused();
    await expect(search).toHaveValue('');

    // Second Esc blurs the now-empty input.
    await page.keyboard.press('Escape');
    await expect(search).not.toBeFocused();
    await expect(search).toHaveValue('');
  });
});

test.describe('keyboard shortcuts — j/k cursor navigation', () => {
  test('j moves focus down the bead-row list; k moves it up', async ({ page }) => {
    // /agents renders a list of `.bead-row` <li> elements with at least
    // the seeded fixture agent + whatever is in the daemon. Two rows is
    // enough to assert j-then-k arithmetic without depending on a
    // specific count.
    await page.goto('/agents');
    await waitForHtmxIdle(page);

    const rows = page.locator('.bead-row');
    const rowCount = await rows.count();
    test.skip(rowCount < 2, `need at least two .bead-row entries on /agents (found ${rowCount})`);

    // No row is focused yet — keys.js only stamps `data-kbd-focus` once
    // the user presses j/k.
    await expect(page.locator('[data-kbd-focus]')).toHaveCount(0);

    // First j: cursor lands on the first row.
    await page.keyboard.press('j');
    await expect(page.locator('[data-kbd-focus]')).toHaveCount(1);
    let focusedHref = await page.locator('[data-kbd-focus] a[href]').first().getAttribute('href');
    const firstHref = focusedHref;

    // Second j: cursor advances to the second row (different href).
    await page.keyboard.press('j');
    focusedHref = await page.locator('[data-kbd-focus] a[href]').first().getAttribute('href');
    expect(focusedHref, 'j moved to a different row').not.toBe(firstHref);
    const secondHref = focusedHref;

    // k: cursor returns to the first row.
    await page.keyboard.press('k');
    focusedHref = await page.locator('[data-kbd-focus] a[href]').first().getAttribute('href');
    expect(focusedHref, 'k moved focus back up').toBe(firstHref);
    expect(focusedHref).not.toBe(secondHref);
  });

  test('Enter on a focused row navigates to the bead', async ({ page }) => {
    await page.goto('/agents');
    await waitForHtmxIdle(page);

    const rows = page.locator('.bead-row');
    const rowCount = await rows.count();
    test.skip(rowCount < 1, `need at least one .bead-row on /agents (found ${rowCount})`);

    await page.keyboard.press('j');
    const targetHref = await page.locator('[data-kbd-focus] a[href]').first().getAttribute('href');
    expect(targetHref).toBeTruthy();

    await page.keyboard.press('Enter');
    // Enter calls `link.click()` which is a same-origin navigation —
    // wait for URL to settle on the row's href.
    await page.waitForURL((url) => url.pathname + url.search === targetHref || url.pathname.endsWith(targetHref));
  });
});

test.describe('keyboard shortcuts — c focuses comment textarea', () => {
  test('c focuses #comment-text on a bead detail page', async ({ page }) => {
    // Pick the first bead-row on /agents — landing on its detail page
    // gives us the comment form (visible under dev-bypass + the auth
    // header below).
    await page.context().setExtraHTTPHeaders({
      'X-Auth-Request-Email': 'e2e-t12@example.test',
    });
    // Force list density: `defaultFeedDensity` is "compact" (ka-0000004132),
    // and the compact view renders `<table class="agents-table">` instead
    // of the `.bead-row` list this test walks. `?density=list` is the
    // documented opt-out, persisted via cookie; the test doesn't care
    // which density the operator prefers.
    await page.goto('/agents?density=list');
    await waitForHtmxIdle(page);

    const firstLink = page.locator('.bead-row a[href]').first();
    const href = await firstLink.getAttribute('href');
    expect(href, 'fixture agents page must have at least one bead-row link').toBeTruthy();
    await firstLink.click();
    await waitForHtmxIdle(page);

    const ta = page.locator('#comment-text');
    await expect(ta, 'comment-text textarea renders under dev-bypass + X-Auth-Request-Email').toBeVisible();
    await expect(ta).not.toBeFocused();

    await page.keyboard.press('c');
    await expect(ta).toBeFocused();
  });
});

test.describe('keyboard shortcuts — g-prefix navigation', () => {
  // Source of truth for these mappings: GO_TARGETS in keys.js. Mirror
  // them verbatim — drift between the table here and that map is the
  // exact regression this describe-block is built to catch.
  const GO_TARGETS = [
    { key: 'a', path: '/agents' },
    { key: 's', path: '/triggers' },
    { key: 'd', path: '/decisions' },
    { key: 'c', path: '/costs' },
    { key: 'v', path: '/advice' },
    { key: 'i', path: '/active' },
  ];

  for (const { key, path } of GO_TARGETS) {
    test(`g ${key} navigates to ${path}`, async ({ page }) => {
      // Land on the feed (a layout page that loads keys.js) so the
      // destination is always a real navigation away. `/` itself
      // redirects to /garden, which does not load the layout.
      await page.goto('/feed');
      await waitForHtmxIdle(page);

      await page.keyboard.press('g');
      await page.keyboard.press(key);

      // g-prefix navigation uses `window.location.href = ...`, which
      // triggers a full document load; waitForURL covers that.
      await page.waitForURL((url) => url.pathname === path);
    });
  }

  test('g h navigates to /', async ({ page }) => {
    // Start somewhere other than `/` so the navigation is observable.
    await page.goto('/agents');
    await waitForHtmxIdle(page);

    // `g h` still targets `/`, the front door. `/` 302s to /garden when the
    // garden handler is wired (decision gd-e17Jl61EcP), so the browser
    // settles there; prove the hop went through `/` rather than straight to
    // /garden by watching the request.
    const homeRequest = page.waitForRequest((req) => new URL(req.url()).pathname === '/');
    await page.keyboard.press('g');
    await page.keyboard.press('h');

    await homeRequest;
    await page.waitForURL((url) => url.pathname === '/garden');
  });
});

test.describe('keyboard shortcuts — input guard', () => {
  test('shortcut keys do nothing while focus is in an input', async ({ page }) => {
    // Going to /agents gives us a bead-row list AND a search input,
    // so we can assert the j/k cursor never moves while the search
    // input is focused.
    await page.goto('/agents');
    await waitForHtmxIdle(page);

    const search = page.locator('nav .nav-search input[name="q"]');
    await search.focus();
    await expect(search).toBeFocused();

    // None of these should trigger a shortcut while the input has focus.
    // Check before each press that no row is focused (the cursor must
    // never wake up while typing).
    for (const k of ['j', 'k', 'g', 'a']) {
      await page.keyboard.press(k);
      await expect(
        page.locator('[data-kbd-focus]'),
        `pressing ${k} while search has focus must not move the bead-row cursor`,
      ).toHaveCount(0);
    }

    // Pressing `?` while focused should also NOT open the help overlay
    // (isEditable check returns true and short-circuits the handler).
    await page.keyboard.press('Shift+Slash');
    await expect(page.locator('dialog.kbd-help[open]')).toHaveCount(0);

    // The keystrokes should have ended up in the input value, exactly
    // like normal typing — `?` is Shift+/ so it shows up as `?`.
    // (We don't assert exact value because Shift+Slash on some keymaps
    // produces only `/`; we only need to confirm the input received SOMEthing
    // and the shortcut layer was dormant.)
    const value = await search.inputValue();
    expect(value.length, 'shortcut keys typed into the focused input').toBeGreaterThan(0);

    // Search must STILL be focused — none of the keys above blurred it.
    await expect(search).toBeFocused();
  });

  test('Enter inside the search input does not trigger the row-open shortcut', async ({ page }) => {
    // Two-part guarantee:
    //   1. Pre-condition: a row IS focused on /agents (j once),
    //      so if Enter-in-input wrongly fired the shortcut, it would
    //      navigate to the focused bead — not submit the form.
    //   2. We focus the search input, type, and route the search GET
    //      so the form submission is intercepted (no navigation away).
    //      After Enter, we assert the URL went to /search?q=..., which
    //      is the form's natural submit target — proving that Enter
    //      followed the form path, NOT the openFocused() path.
    await page.goto('/agents');
    await waitForHtmxIdle(page);

    const rows = page.locator('.bead-row');
    const rowCount = await rows.count();
    test.skip(rowCount < 1, `need at least one .bead-row on /agents (found ${rowCount})`);

    // Focus the cursor on the first row first — this is the trap for
    // openFocused(). If Enter-in-input wrongly fires the shortcut,
    // navigation will land on the row's href, not /search.
    await page.keyboard.press('j');
    await expect(page.locator('[data-kbd-focus]')).toHaveCount(1);
    const focusedHref = await page.locator('[data-kbd-focus] a[href]').first().getAttribute('href');
    expect(focusedHref).toBeTruthy();

    const search = page.locator('nav .nav-search input[name="q"]');
    await search.focus();
    await search.fill('e2e-t12-enter-guard');
    await expect(search).toBeFocused();

    // Submit the form by pressing Enter inside the input. The form
    // action is GET /search, so the URL should advance to /search?q=...
    // — emphatically NOT to the focused row's href.
    await Promise.all([
      page.waitForURL((url) => url.pathname.endsWith('/search') && url.searchParams.get('q') === 'e2e-t12-enter-guard'),
      page.keyboard.press('Enter'),
    ]);

    // Sanity: we did not land on the focused row's bead detail.
    expect(page.url()).not.toContain(focusedHref);
  });
});
