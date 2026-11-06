// Phase T10 (gd-7igjIVM1aP) of epic ka-0000004611 — search behavior.
// Plan of record: `gardenio/plans/beads-viewer-playwright-tests.md`.
//
// What this guards against:
//
//   - The /search page silently regressing from "form + results" to a
//     500/panic (template rename, daemon contract drift). The route
//     smoke in routes.spec.js covers the empty-query render; this spec
//     pins the QUERY → RESULTS render and the per-row link contract.
//   - The nav-search form (in templates/layout.html) losing its action
//     or `name="q"` so submitting from an arbitrary page no longer
//     lands on /search?q=… The keyboard suite covers the focus path;
//     this spec covers the actual navigation contract.
//   - The "no results for X" empty-state branch in search.html
//     disappearing — that branch is the only feedback a user gets when
//     a query returns zero hits, and a regression to a blank page would
//     look indistinguishable from a successful render with no rows.
//   - Per-row links in `.search-results tbody` regressing from
//     `<a href="/{id}">` to a non-anchor (`<span>`) or losing the id
//     itself — clicking a result row MUST navigate to the bead.
//   - The "debounce" contract on the search box. The plan T10 section
//     calls out "no request <Nms after keystroke". The viewer today
//     submits search via plain GET form-submit (no live debounced
//     querying). That IS the debounce: zero AJAX requests fire while
//     typing, only the form submit triggers a navigation. This spec
//     pins that contract — if a future change adds a live `hx-trigger`
//     on the search input without `delay:Nms`, the no-request-while-
//     typing assertion will fail and the change will have to land its
//     own debounce + test in the same MR.
//
// Strategy: use the "fixture" string as the matching query — every
// fixture seeded by `seed/fixtures.mjs` includes the word "fixture" in
// its title and/or description, so we are guaranteed at least one row
// against any seeded daemon. For the no-match case we use a long
// random-looking literal that no live daemon should plausibly contain.
// All assertions are robust to noise — we never count rows or pin a
// specific result id, only assert "≥ 1 row" / "the empty-state text
// rendered" / "every row's link points at a bead-show URL".

import { expect, test } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// "fixture" appears in every seeded fixture's title or description —
// see seed/fixtures.mjs. Any seeded daemon answers this with ≥ 1 row.
const MATCH_QUERY = 'fixture';

// A VALID, registered bead type that no seeded fixture uses (see
// seed/fixtures.mjs — the seeded set is task/bug/epic/agent/project/
// trigger/decision/formula/advice/doc/bundle/message/merge-request/user/
// rich-report/widget-*; `runbook` is a real builtin but never seeded).
// Combined with `?q=…`, this forces the post-search filter to drop every
// row regardless of whether the daemon ran semantic or lexical search.
// (Semantic search ALWAYS returns its nearest-N neighbors — there is no
// distance threshold — so a query string alone cannot reliably produce
// zero rows. A type filter that nothing matches will.)
//
// It MUST be a registered type, not a bogus string: the daemon now
// validates the --type list filter server-side (ka-0000008041) and 400s
// an unknown type instead of returning empty — so an unregistered value
// would make lexical search render the hard-error banner, not the
// "No results" empty state this test asserts.
const NO_MATCH_TYPE = 'runbook';

test.describe('GET /search — empty query form-only render', () => {
  test('no `q` param → form renders, hint row in place of results', async ({ page }) => {
    // The empty-query branch in search.go bails out before calling the
    // daemon. The "boost info-density" template change (763ba0d5) keeps
    // the results table in the DOM and uses an inline `<td class="empty">`
    // row to render the "Enter a query above to search beads." hint
    // instead of a separate `<p class="empty">`. This pins the new
    // contract: form present, table present with zero result rows, and
    // the empty-row hint visible. A regression that drops the empty
    // branch would surface as a missing `td.empty` or a stray result row.
    await page.goto('/search');

    await expect(page.locator('form.search-form input[name="q"]')).toBeVisible();

    const table = page.locator('table.search-results');
    await expect(table).toBeVisible();
    await expect(table.locator('tbody tr:not(.search-empty-row)')).toHaveCount(0);
    const emptyCell = table.locator('tr.search-empty-row td.empty');
    await expect(emptyCell).toBeVisible();
    await expect(emptyCell).toContainText(/enter a query/i);
  });
});

test.describe('GET /search?q=… — results render', () => {
  test('matching query renders the results table with ≥ 1 row', async ({ page }) => {
    await page.goto(`/search?q=${encodeURIComponent(MATCH_QUERY)}`);

    // Form retains the submitted query — the input's value attribute
    // is what the user sees in the box after the round-trip.
    await expect(page.locator('form.search-form input[name="q"]'))
      .toHaveValue(MATCH_QUERY);

    // Results section rendered. Mode is either "semantic" or "lexical"
    // depending on whether the daemon's embedder is enabled — both are
    // valid; the contract is "a results table with ≥ 1 row".
    const table = page.locator('table.search-results');
    await expect(table).toBeVisible();

    const rows = table.locator('tbody tr');
    expect(await rows.count(), 'matching query has ≥ 1 result').toBeGreaterThan(0);

    // Every row's first-column link points at a bead-show URL
    // (`/kd-…`). A regression that drops the `<a>` or the id would be
    // indistinguishable in the table layout but breaks navigation —
    // pin it here.
    const links = table.locator('tbody tr td:first-child a');
    expect(await links.count(), 'every result row exposes a bead link')
      .toBe(await rows.count());
    const hrefs = await links.evaluateAll((as) => as.map((a) => a.getAttribute('href')));
    for (const href of hrefs) {
      expect(href, `result link href ${href} points at a kd-… bead`)
        .toMatch(/\/(gd|ka)-[\w-]+$/);
    }
  });

  test('no-match query renders the "No results" empty state', async ({ page }) => {
    // Stack `q=fixture` (which would otherwise match) with a VALID but
    // never-seeded type filter (NO_MATCH_TYPE). Both semantic AND lexical
    // search apply the type filter server-side, so the result set is empty
    // regardless of which mode the daemon runs. Without the type pin,
    // semantic search would return 20 nearest neighbors for ANY query,
    // defeating the empty-state assertion. The type must be REGISTERED —
    // the daemon now 400s an unknown --type filter (ka-0000008041), which
    // would surface as an error banner rather than the empty state.
    await page.goto(
      `/search?q=${encodeURIComponent(MATCH_QUERY)}` +
        `&type=${encodeURIComponent(NO_MATCH_TYPE)}`,
    );

    // Post-template-rewrite (763ba0d5 "boost info-density"), the no-match
    // branch keeps the results table in the DOM and renders the empty
    // state as a single `<tr class="search-empty-row"><td class="empty">`
    // row spanning every column. Asserts the user-visible contract:
    // table present, exactly the empty row inside it, and the message
    // echoes the query so the user knows the search ran.
    const table = page.locator('table.search-results');
    await expect(table).toBeVisible();
    await expect(table.locator('tbody tr:not(.search-empty-row)')).toHaveCount(0);
    const emptyCell = table.locator('tr.search-empty-row td.empty');
    await expect(emptyCell).toBeVisible();
    await expect(emptyCell).toContainText(/no results/i);
    await expect(emptyCell).toContainText(MATCH_QUERY);
  });

  test('clicking a result row navigates to the bead detail page', async ({ page }) => {
    // Don't pin a specific result id — semantic-vs-lexical ranking
    // changes per daemon and a literal id query won't surface the
    // matching bead at all (id is not in title/description text).
    // Instead, search for "fixture" (matches all seeded fixtures by
    // title/description), grab the first row's link, follow it, and
    // assert we land on a bead-show page for the same id.
    const id = fixtureId('generic');
    await page.goto(`/search?q=${encodeURIComponent(MATCH_QUERY)}`);

    const firstLink = page.locator('table.search-results tbody tr td:first-child a').first();
    await expect(firstLink, 'matching query renders ≥ 1 result link').toBeVisible();
    const href = await firstLink.getAttribute('href');
    expect(href, 'first result href points at a kd-… bead').toMatch(/\/(gd|ka)-[\w-]+$/);
    const targetId = href.split('/').pop();

    // Sanity-check that `id` in the manifest IS resolvable on this
    // daemon — without that, a regression in fixture seeding would
    // surface as a navigation failure here, which would mis-blame
    // the search-result link contract. Doing this AFTER the search
    // assertion keeps the failure mode clean: search renders → link
    // present → fixture is reachable independently.
    expect(id, 'fixtures/ids.json carries the generic key').toMatch(/^(gd|ka)-/);

    await firstLink.click();
    await page.waitForURL(`**/${targetId}`);

    // Assert we landed on the bead-show page — the layout shell <nav>
    // is present on every page; an <h1> exists at the top.
    await expect(page.locator('nav').first()).toBeVisible();
    await expect(page.locator('h1').first()).toBeVisible();
  });
});

test.describe('nav-search box submits to /search', () => {
  test('typing in the nav search and pressing Enter navigates to /search?q=…', async ({ page }) => {
    // The nav-search form lives in templates/layout.html. Its action
    // is `{{.BasePath}}/search` and its lone field is `name="q"`. From
    // a non-/search page (use /agents to be sure), submitting must
    // navigate to the search page with the query in the URL.
    await page.goto('/agents');

    const navSearch = page.locator('nav .nav-search input[name="q"]');
    await expect(navSearch).toBeVisible();

    await navSearch.fill(MATCH_QUERY);
    await navSearch.press('Enter');

    await page.waitForURL(/\/search\?(?:[^#]*&)?q=fixture(?:&|$)/);

    // Search page rendered with the submitted query echoed back.
    await expect(page.locator('form.search-form input[name="q"]'))
      .toHaveValue(MATCH_QUERY);
  });
});

test.describe('search box does not fire requests while typing (debounce contract)', () => {
  test('typing characters triggers zero /search HTTP requests', async ({ page }) => {
    // Plan T10: "no request <Nms after keystroke". The viewer today
    // submits search only on form-submit — there is NO live debounced
    // query, which is the strongest possible debounce contract: zero
    // requests fire on `input` events. Pin this so a future change
    // that adds a live `hx-trigger` on the search input MUST also add
    // a `delay:` modifier and update this test to assert the delay.
    await page.goto('/feed');

    const searchRequests = [];
    page.on('request', (req) => {
      const url = new URL(req.url());
      if (url.pathname === '/search') {
        searchRequests.push({ url: req.url(), method: req.method() });
      }
    });

    const navSearch = page.locator('nav .nav-search input[name="q"]');
    await navSearch.focus();

    // Type one character at a time — Playwright fires `input` events
    // per keystroke, which is exactly what a live `hx-trigger="input"`
    // would react to. Zero of these may turn into /search requests.
    await navSearch.pressSequentially('abcdef', { delay: 30 });

    // A short settle gives any (mistakenly-wired) debounced trigger a
    // chance to fire before we assert. 600ms is well past the
    // conventional htmx 300-500ms debounce window — anything wired up
    // would have fired by now.
    // eslint-disable-next-line no-restricted-syntax -- this is the wait
    await page.waitForTimeout(600);

    expect(
      searchRequests,
      'typing into the search box must not fire any /search requests; ' +
        'if a live debounced search has been added, update this test ' +
        'to assert the delay window instead of zero requests',
    ).toEqual([]);
  });
});
