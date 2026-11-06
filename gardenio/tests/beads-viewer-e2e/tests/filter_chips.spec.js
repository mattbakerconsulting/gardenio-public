// Phase T11 (gd-5m9i70WiTJ) — type / status / label filter chips on
// `/p/{project}`. Plan of record: `gardenio/plans/beads-viewer-playwright-tests.md`.
//
// What this guards against:
//
//   - Type chip click failing to update the URL or list contents:
//     `hx-boost` + `hx-push-url` regression on the chip strip.
//   - `resolveProjectTypeFilter` / `projectFilterTypes` regressing so a
//     `?type=X` URL no longer narrows the list to that single type.
//   - The `Issues` macro filter losing one of its constituent types
//     (`issueMacroTypes` in server.go: task / feature / bug / epic / chore).
//   - `buildProjectChipHref` dropping `status` / `label` / `q` query
//     params when a type chip is clicked — the multi-dimensional filter
//     would silently collapse to a single dimension.
//   - `parseCommaSeparated` + daemon `Statuses[]` wiring regressing so a
//     `?status=open,closed` URL collapses to a single status (the
//     "multi-select status" surface today is URL-driven; T11 asserts
//     the wire contract a future status-chip UI would build on).
//   - The default-state contract regressing: visiting `/p/{project}`
//     with no query renders the `Issues` chip active, lists open beads
//     of issue types, and effectively constitutes the "cleared" state
//     for the project page.
//
// Strategy: follow `nav.spec.js` and resolve a project by clicking the
// first `/p/...` link off the index, rather than hard-coding a project
// name. The seeded fixture daemon may not have a `project:gardenio`
// label on its fixtures; whatever project the index advertises is the
// one we test against. All filter assertions are robust to whatever
// data the daemon actually exposes — counts and `data-type` attributes
// drive the assertions, never specific bead ids.

import { test, expect } from '@playwright/test';

import { waitForHtmxIdle } from './_helpers.js';

// `issueMacroTypes` mirror server.go. When the `Issues` chip is active
// (default), every rendered row's `data-type` MUST be one of these.
// gd-u7I2iWKgMA: keep this list in sync with `issueMacroTypes` in
// gardenio/controller/cmd/beads-viewer/server.go — `rich-report` was
// added to the macro in ka-0000005611 so the Phase 9 "View report →"
// CTA surfaces on the default /p/<project> view.
const ISSUE_MACRO_TYPES = ['task', 'feature', 'bug', 'epic', 'chore', 'rich-report'];

// Resolve the first project advertised on the projects index. Same
// pattern as `nav.spec.js` — pick whatever the index has, don't
// hard-code. The index moved off `/` (now the feed) to `/index`, so
// resolve project links there.
//
// Scope to the PROJECTS LIST (`ul.index-project-list a.index-project-name`),
// NOT any `a[href*="/p/"]` on the page. `/index` now renders the fleet-wide
// Live-agents rail ABOVE the projects list, and each rail card carries its own
// `/p/{project}` link (`.index-rail-project`) for whatever agent is newest by
// -updated_at. A bare `a[href*="/p/"].first()` grabs THAT rail link, so any
// spec that seeds a fresh alive agent in an issue-less ephemeral project (this
// suite's index_home_rail + a11y fixtures) makes gotoFirstProject land on a
// project with no issue rows → the default-state assertions read a null
// data-type/data-status and fail (the long-standing filter_chips flake,
// gd-HnYhgj2xnO). The projects-list link is the one this helper always MEANT
// to click — a real, issue-bearing project (alphabetically first, e.g.
// `gardenio`) — and is immune to whatever populates the rail.
async function gotoFirstProject(page) {
  await page.goto('/index');
  const projectLink = page.locator('ul.index-project-list a.index-project-name').first();
  await expect(projectLink, 'index lists at least one project').toBeVisible();
  const href = await projectLink.getAttribute('href');
  await projectLink.click();
  await page.waitForURL(`**${href}`);
  return href;
}

// Read each row's `data-type` so the test can verify type-filter
// behaviour without needing to know the seeded fixtures by id. Returns
// an empty array when the project has no rows under the active filter
// — that itself is a valid filter state (the `<p class="empty">` path).
async function rowTypes(page) {
  const rows = page.locator('li.bead-row');
  const count = await rows.count();
  const types = [];
  for (let i = 0; i < count; i += 1) {
    types.push(await rows.nth(i).getAttribute('data-type'));
  }
  return types;
}

async function rowStatuses(page) {
  const rows = page.locator('li.bead-row');
  const count = await rows.count();
  const statuses = [];
  for (let i = 0; i < count; i += 1) {
    statuses.push(await rows.nth(i).getAttribute('data-status'));
  }
  return statuses;
}

test.describe('type chip → URL state + list filters', () => {
  test('default state: Issues chip active and rows are issue types', async ({ page }) => {
    await gotoFirstProject(page);

    // No `?type=` in the URL → resolveProjectTypeFilter("") returns
    // "issues", so the Issues chip is the active default.
    const active = page.locator('nav.chip-strip .chip-active');
    await expect(active, 'exactly one chip is active').toHaveCount(1);
    await expect(active).toHaveText(/^Issues\b/i);
    await expect(active).toHaveAttribute('aria-current', 'true');

    // All rows under the default filter must be one of the issue macro
    // types. Empty list is acceptable — we only assert that nothing
    // sneaks in that ISN'T an issue type.
    const types = await rowTypes(page);
    for (const t of types) {
      expect(ISSUE_MACRO_TYPES, `row type ${t} is in the issues macro`).toContain(t);
    }
  });

  test('clicking a chip updates ?type= and re-filters the list', async ({ page }) => {
    const projectHref = await gotoFirstProject(page);
    const chipStrip = page.locator('nav.chip-strip');

    // Pick the first non-active, non-"All" chip — clicking "All" clears
    // type entirely (Types: nil), which weakens the "list is filtered"
    // assertion. A single-type chip narrows to one type, which is the
    // strongest signal that filtering is wired up.
    // `\b` (not `$`) so the regex still matches when count badges are
    // rendered into the chip ("All 42"). Mirrors the active-chip text
    // assertions elsewhere in this file.
    const targetChip = chipStrip.locator('a.chip:not(.chip-active)')
      .filter({ hasNotText: /^All\b/i })
      .first();
    await expect(targetChip, 'project has a non-default single-type chip').toBeVisible();
    const targetHref = await targetChip.getAttribute('href');
    expect(targetHref, 'chip href stays inside this project').toContain(projectHref);

    // Pull the type value the chip will push to the URL — drives the
    // post-click `data-type` assertion below.
    const url = new URL(targetHref, 'http://x');
    const typeParam = url.searchParams.get('type');
    expect(typeParam, 'chip URL carries ?type=').toBeTruthy();
    expect(typeParam, 'chip URL is not the macro / All chip').not.toBe('issues');
    expect(typeParam).not.toBe('all');

    await targetChip.click();
    await waitForHtmxIdle(page);
    await page.waitForURL(`**${targetHref}`);

    // Active marker tracked the click.
    const newActive = page.locator('nav.chip-strip .chip-active');
    await expect(newActive).toHaveCount(1);
    await expect(newActive).toHaveAttribute('aria-current', 'true');

    // Every row's `data-type` matches the ?type= we navigated to.
    // (Single-type chips map 1:1 in projectFilterTypes.)
    const types = await rowTypes(page);
    for (const t of types) {
      expect(t, `row type ${t} matches ?type=${typeParam}`).toBe(typeParam);
    }
  });

  test('?type=all removes the type restriction', async ({ page }) => {
    // `?type=all` → projectFilterTypes returns nil → daemon Types
    // filter is empty → list is unrestricted by type. Drive the URL
    // directly so the test does not depend on the chip strip's text.
    const projectHref = await gotoFirstProject(page);
    await page.goto(`${projectHref}?type=all`);

    const active = page.locator('nav.chip-strip .chip-active');
    await expect(active).toHaveText(/^All\b/i);

    // We can't strictly prove "no type restriction" from the rendered
    // list alone (a project may legitimately have only one type even
    // with no filter). Instead, assert that the All-state row count is
    // never SMALLER than the Issues-state row count — All is a
    // superset of Issues by construction.
    const allCount = await page.locator('li.bead-row').count();
    await page.goto(projectHref);
    const issuesCount = await page.locator('li.bead-row').count();
    expect(allCount, 'all-filter rows ≥ issues-filter rows').toBeGreaterThanOrEqual(issuesCount);
  });
});

test.describe('multi-select status (URL-driven) + filter preservation', () => {
  test('?status= drives the row list; multi-select widens it', async ({ page }) => {
    const projectHref = await gotoFirstProject(page);

    // Use ?type=all so type filtering doesn't hide rows whose only
    // distinguishing dimension is status. Compare three status filters:
    //   open,closed   ⊇   open   and   open,closed   ⊇   closed
    // Multi-select must NEVER show fewer rows than either single-select
    // case — that's the contract this test pins.
    await page.goto(`${projectHref}?type=all&status=open`);
    const openCount = await page.locator('li.bead-row').count();
    const openOnly = await rowStatuses(page);
    for (const s of openOnly) {
      expect(s, `?status=open row has status ${s}`).toBe('open');
    }

    await page.goto(`${projectHref}?type=all&status=closed`);
    const closedCount = await page.locator('li.bead-row').count();
    const closedOnly = await rowStatuses(page);
    for (const s of closedOnly) {
      expect(s, `?status=closed row has status ${s}`).toBe('closed');
    }

    await page.goto(`${projectHref}?type=all&status=open,closed`);
    const bothCount = await page.locator('li.bead-row').count();
    const bothStatuses = await rowStatuses(page);
    for (const s of bothStatuses) {
      expect(['open', 'closed'], `?status=open,closed row has status ${s}`).toContain(s);
    }

    // Multi-select widens (or matches when one single-select is empty).
    expect(bothCount, 'open,closed ≥ open').toBeGreaterThanOrEqual(openCount);
    expect(bothCount, 'open,closed ≥ closed').toBeGreaterThanOrEqual(closedCount);

    // If there is a closed bead at all, the multi-select view must
    // include at least one closed row alongside the open ones — proves
    // the comma-separated parse landed both values, not just the first.
    if (closedCount > 0 && openCount > 0) {
      expect(bothStatuses, 'multi-select carries both statuses')
        .toEqual(expect.arrayContaining(['open', 'closed']));
    }
  });

  test('clicking a type chip preserves status / label / q params', async ({ page }) => {
    // Pin the multi-dim filter contract: the chip-strip URLs are built
    // by buildProjectChipHref, which preserves the other dimensions.
    // Regressions here silently collapse the filter — the failure
    // surface (a 0-row list) looks like correct behaviour to a casual
    // observer, so the assertion has to be on the URL itself.
    const projectHref = await gotoFirstProject(page);
    await page.goto(`${projectHref}?type=task&status=open,closed&label=foo&q=bar`);

    const chipStrip = page.locator('nav.chip-strip');
    const otherChip = chipStrip.locator('a.chip:not(.chip-active)').first();
    await expect(otherChip).toBeVisible();
    const otherHref = await otherChip.getAttribute('href');

    const url = new URL(otherHref, 'http://x');
    expect(url.pathname, 'chip URL stays in project').toContain(projectHref);
    expect(url.searchParams.get('type'), 'type updated').not.toBe('task');
    expect(url.searchParams.get('status'), 'status preserved').toBe('open,closed');
    expect(url.searchParams.get('label'), 'label preserved').toBe('foo');
    expect(url.searchParams.get('q'), 'q preserved').toBe('bar');
  });
});

test.describe('reset to defaults (the "clear" path)', () => {
  test('navigating to /p/{project} with no query restores defaults', async ({ page }) => {
    // The viewer has no dedicated "clear filters" button today; the
    // defined-by-design clear path is "land on /p/{project} with no
    // query string" — resolveProjectTypeFilter("") returns "issues",
    // statuses fall back to ["open"], and label/q are empty. This test
    // pins that contract: from a maximally-filtered URL, navigating to
    // the bare project URL must restore the Issues default + open
    // beads only.
    const projectHref = await gotoFirstProject(page);

    // Drive into a heavily-filtered state, then back out.
    await page.goto(`${projectHref}?type=decision&status=closed&label=foo&q=bar`);
    await expect(page.locator('nav.chip-strip .chip-active')).toHaveText(/^Decisions\b/i);

    await page.goto(projectHref);

    // Default chip is Issues again.
    const active = page.locator('nav.chip-strip .chip-active');
    await expect(active, 'default chip is Issues').toHaveText(/^Issues\b/i);
    await expect(active).toHaveAttribute('aria-current', 'true');

    // Default status is "open" — every row reflects that.
    const statuses = await rowStatuses(page);
    for (const s of statuses) {
      expect(s, `default-state row has open status (got ${s})`).toBe('open');
    }

    // Default rows are issue types only.
    const types = await rowTypes(page);
    for (const t of types) {
      expect(ISSUE_MACRO_TYPES).toContain(t);
    }
  });
});
