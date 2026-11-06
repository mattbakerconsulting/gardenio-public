// Backfill (ka-0000003378): behavioral coverage for the /people operator
// directory (M2 §4, ka-0000000340) + its alias-aware per-row stat strip
// (June re-audit gd-DXIgdu0NeF: real GROUP BY stats via the daemon
// /v1/operators/stats endpoint). This large viewer surface shipped with no
// dedicated e2e spec — nav.spec.js only touches /settings.
//
// GROUND TRUTH THIS SPEC DEPENDS ON (verified on the live harness):
//   The seed's global identity oidc:e2e-fixture@example.com is SCREENED OUT
//   of /people by the daemon's humans-only filter (botOperatorMarkerRe matches
//   the RFC-2606 fixture domain @example.com), so with only that identity the
//   directory renders "No operators have a public profile" (total=0). The
//   people_dir_open_task fixture is therefore attributed (via `operatedBy`) to
//   a dedicated operator on a NON-fixture domain — oidc:e2e-people@example.test
//   → slug 'e2e-people' — which the daemon lazily provisions on that first
//   attributed write. That operator is this spec's deterministic anchor row.
//   Keep PEOPLE_SLUG / PEOPLE_DISPLAY in sync with seed/fixtures.mjs
//   (people_dir_open_task + people_dir_user).
//
// What this spec covers:
//   1. /people renders the layout shell + <h1>Users + the directory list.
//   2. The seeded operator row is present, with its display name resolved
//      through the user-bead join and a name link to /u/{slug}.
//   3. The row carries the reusable hover-card trigger (hx-get to the
//      operator-hover partial) — the M2 acceptance criterion.
//   4. The per-row stat strip renders real counts: the row's "open" chip
//      reflects the open task attributed to the operator (guards the
//      sampling→GROUP BY replacement, ka-0000002003), and the header total
//      is the sum over rows.
//   5. The operator-hover partial resolves to a 200 for the seeded slug.

import { expect, test } from '@playwright/test';

// The dedicated directory operator seeded by people_dir_open_task /
// people_dir_user. Slug is derived by the daemon from the email local-part.
const PEOPLE_SLUG = 'e2e-people';
const PEOPLE_DISPLAY = 'E2E People Directory';

test.describe('GET /people — operator directory', () => {
  test('renders the directory shell with the Users heading', async ({ page }) => {
    const res = await page.goto('/people');
    expect(res, 'navigation response').not.toBeNull();
    expect(res.status(), '/people HTTP status').toBeLessThan(400);
    // Layout shell — a break in layout.html regresses every route at once.
    await expect(page.locator('nav').first()).toBeVisible();
    // Page landmark: "Users (<count>)". The count span carries the
    // server-rendered total the header-vs-rows invariant is built on.
    await expect(page.locator('h1').first()).toContainText(/^Users/);
    await expect(page.locator('ul.people-list').first()).toBeVisible();
  });

  test('lists the seeded operator with a name link to /u/{slug}', async ({ page }) => {
    await page.goto('/people');

    // Scope to the seeded operator's row so the assertions never drift onto
    // another directory entry. data-operator-id is stable per row; we locate
    // by the name link's href which is the row's canonical /u/{slug} target.
    const row = page.locator('li.people-row', {
      has: page.locator(`a.people-row-name[href="/u/${PEOPLE_SLUG}"]`),
    });
    await expect(row, 'seeded operator row present').toHaveCount(1);

    // The name resolves through the user-bead join (people.go displayName),
    // so the link text is the display_name, not the raw email/slug.
    const nameLink = row.locator('a.people-row-name');
    await expect(nameLink).toHaveText(PEOPLE_DISPLAY);
    await expect(nameLink).toHaveAttribute('href', `/u/${PEOPLE_SLUG}`);
  });

  test('the row name carries the reusable hover-card trigger', async ({ page }) => {
    await page.goto('/people');
    const nameLink = page.locator(`a.people-row-name[href="/u/${PEOPLE_SLUG}"]`);
    // M2 §4 acceptance criterion: a reusable hover-card preview. The trigger
    // is an htmx hx-get to the operator-hover partial for this slug.
    await expect(nameLink).toHaveAttribute(
      'hx-get',
      `/htmx/operator-hover/${PEOPLE_SLUG}`,
    );
  });

  test('the row stat strip renders a real open-beads count', async ({ page }) => {
    await page.goto('/people');
    const row = page.locator('li.people-row', {
      has: page.locator(`a.people-row-name[href="/u/${PEOPLE_SLUG}"]`),
    });
    // The stat strip is the alias-aware GROUP BY output (ka-0000002003). The
    // open chip must render a concrete count, not the em-dash empty state —
    // the operator owns the seeded people_dir_open_task, so "open" is ≥ 1.
    const openChip = row.locator('.people-stat-open');
    await expect(openChip).toBeVisible();
    await expect(openChip).toContainText(/\bopen\b/);
    await expect(openChip, 'open chip is a real count, not the em-dash empty state')
      .not.toContainText('—');
  });

  test('the header total is ≥ 1 and matches the rendered row count', async ({ page }) => {
    await page.goto('/people');
    // Server-rendered total on the <h1> count span.
    const total = await page
      .locator('[data-people-header-count]')
      .getAttribute('data-people-total');
    const totalNum = Number(total);
    expect(Number.isFinite(totalNum), 'header total is numeric').toBe(true);
    expect(totalNum, 'directory has at least the seeded operator').toBeGreaterThanOrEqual(1);

    // The header count is "people" (deduped humans). Every rendered row (active
    // grid + dormant strip) is one such person, so total == rows on the page.
    // This is the header-vs-rows invariant the directory audit pins.
    const rows = await page.locator('li.people-row').count();
    expect(rows, 'header total equals rendered people rows').toBe(totalNum);
  });
});

test.describe('GET /htmx/operator-hover/{slug} — hover card partial', () => {
  test('resolves the seeded slug to a 200 card fragment', async ({ page }) => {
    const res = await page.goto(`/htmx/operator-hover/${PEOPLE_SLUG}`);
    expect(res, 'navigation response').not.toBeNull();
    expect(res.status(), 'hover-card HTTP status').toBeLessThan(400);
    // The card previews the same operator — it links to /u/{slug} and names
    // the operator's canonical email/slug in the hover-card head.
    await expect(
      page.locator(`a.operator-hover-card-name[href="/u/${PEOPLE_SLUG}"]`),
    ).toBeVisible();
  });

  test('404s an unknown slug', async ({ request }) => {
    const res = await request.get('/htmx/operator-hover/no-such-operator-xyz', {
      // A hover card for a non-existent slug is a 404, not a soft empty card.
      maxRedirects: 0,
    });
    expect(res.status(), 'unknown slug hover-card status').toBe(404);
  });
});
