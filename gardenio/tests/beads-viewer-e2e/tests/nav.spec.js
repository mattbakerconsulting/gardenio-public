// Phase T4 (gd-TZ6hqNPFVA) — top-level navigation, active-tab indicator
// state, and breadcrumb back-links. Plan of record:
// `gardenio/plans/beads-viewer-playwright-tests.md`.
//
// Three concerns, three describe blocks:
//
//   1. top-nav transitions — click each tab in `<nav>` from the projects
//      index (`/index`; `/` redirects to /garden, which has no layout nav),
//      assert URL change and the destination's page-level landmark
//      (`<h1>`) renders.
//   2. active-tab indicator — the chip-strip on the project page is the
//      only active-tab UI in the viewer today (top-nav links carry no
//      active marker; see layout.html). Assert: default lands with the
//      "All" chip active; clicking another chip pushes URL state and
//      moves the active marker to the clicked chip.
//   3. breadcrumbs / back navigation — project page renders a
//      "← all projects" back-link that returns to `/` (the /garden front
//      door). Browser back
//      button restores prior page from a chain (index → project → bead).
//
// Notes for future-you:
//   - Top-nav has NO active-tab styling today. If/when one is added
//     (e.g. `aria-current="page"` on the matching `<a>` in layout.html),
//     extend the first describe to assert it. For now we only assert
//     URL + landmark.
//   - The chip-strip uses `hx-boost` + `hx-push-url`, so clicking a
//     chip is an htmx swap, not a full nav. We wait for htmx-idle, not
//     for `page.waitForURL` alone, before asserting markup.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

// The nav bar is gated on VibeMode (gb-ux #3, ka-0000003340; decision
// ka-0000007562). SIMPLE mode (the default, VibeMode=false) shows only the
// CORE links; the full ("vibe") experience shows every link. So the top-nav
// transitions split into two describes: the core links are asserted from a
// default page load, and the nineteen vibe-only links are asserted only after
// enabling VibeMode via the X-Auth-Request-Email header (see VIBE_EMAIL).
//
// Source of truth for the split: templates/layout.html (the <nav> block) and
// the Go render-level pin in cmd/beads-viewer/nav_vibe_mode_test.go, which
// asserts the exact core / vibe-only href sets.

// VIBE_EMAIL is the email of the seeded `vibe_user` fixture (type=user,
// preferences.vibe_mode=true — see seed/fixtures.mjs, gd-1Ct0P1Q4dA). Sending
// it as X-Auth-Request-Email makes viewerVibeMode resolve to true, so the full
// nav renders. Keep in sync with the fixture's `email` field.
const VIBE_EMAIL = 'e2e-vibe@example.test';

// SIMPLE_EMAIL is an authenticated operator with NO seeded user bead, so
// viewerVibeMode resolves to false (findUserBeadByEmail → nil) — i.e. simple
// mode, but logged in. The simple-mode transition loop needs this because
// /settings is auth-gated (handleSettings 401s an anonymous viewer, so no h1
// and no nav render); every other core destination renders anonymously too,
// and an authenticated non-vibe viewer still sees exactly the simple nav. The
// truly-anonymous default is pinned at render level by the Go test
// TestNav_LoggedOut_SimpleByDefault, and by the anonymous "vibe-only links are
// hidden" test below.
const SIMPLE_EMAIL = 'e2e-simple@example.test';

// CORE_NAV: links that render in BOTH modes (each entry: visible link text,
// destination route, expected destination <h1>). Simple mode has no
// wordmark/logo affordance (operator-intentional); Feed is the way back to the
// activity feed, which moved to /feed when `/` became the /garden front door. Plans is a
// core, always-on link too (epic ka-0000001409). "beads" (the vibe-only
// home/logo link) is exercised in the VibeMode describe below. Active joined
// the core set once it became a real work surface — a "Needs attention" pull
// plus per-project recency list (ka-0000002647); operator sign-off on decision
// ka-0000007562 ("I would like for Active to be in there but it needs a lot of
// work to be worthy").
const CORE_NAV = [
  { label: 'Active',      path: '/active',      h1: /^Active/ },
  { label: 'Feed',        path: '/feed',        h1: /Activity feed/i },
  { label: 'Agents',      path: '/agents',      h1: /^Agents/ },
  { label: 'Threads',     path: '/threads',     h1: /^Threads/ },
  { label: 'Plans',       path: '/plans',       h1: /^Plans/ },
  { label: 'Decisions',   path: '/decisions',   h1: /^Decisions/ },
  { label: 'Escalations', path: '/escalations', h1: /^Escalations/ },
  { label: 'Settings',    path: '/settings',    h1: /^Settings/ },
];

// VIBE_NAV: the vibe-only links this suite exercises as full navigations.
// These are a representative subset of the {{if .VibeMode}} links
// (nav_vibe_mode_test.go pins the full set at render level); each needs a
// page-level <h1> to assert the destination rendered. Active is NOT here — it
// is now a CORE link (ka-0000002647); only its Me/Home derivatives
// (/active?operated_by=me, /inbox) remain vibe-gated.
const VIBE_NAV = [
  { label: 'Ratings',  path: '/ratings',  h1: /^Ratings/ },
  { label: 'Advice',   path: '/advice',   h1: /^Advice/ },
  { label: 'Triggers', path: '/triggers', h1: /^Triggers/ },
  { label: 'Costs',    path: '/costs',    h1: /^Costs/i },
  { label: 'My Views', path: '/views',    h1: /(My )?[Vv]iews/ },
];

test.describe('top-nav transitions — simple mode (default)', () => {
  // Authenticate as a non-vibe operator so /settings (auth-gated) renders,
  // while VibeMode stays false → the simple nav. The vibe-only absence check
  // below runs anonymously (no header) to pin the true logged-out default.
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': SIMPLE_EMAIL } });

  for (const { label, path, h1 } of CORE_NAV) {
    test(`${label} link navigates to ${path}`, async ({ page }) => {
      await page.goto('/index');
      // Scope to the layout <nav> to avoid matching same-text links in
      // page bodies (e.g. an "Agents" h1 on a body list).
      const nav = page.locator('nav').first();
      const link = nav.getByRole('link', { name: label, exact: true });
      await expect(link, `${label} present in top nav`).toBeVisible();

      await Promise.all([
        page.waitForURL(`**${path}`),
        link.click(),
      ]);

      // Page-level landmark proves the destination template rendered.
      await expect(page.locator('h1').first()).toHaveText(h1);
      // Top-<nav> remains rendered after the transition (layout intact).
      await expect(page.locator('nav').first()).toBeVisible();
    });
  }
});

test.describe('top-nav — vibe-only links hidden by default', () => {
  // No auth header → anonymous viewer → viewerVibeMode short-circuits to false
  // (no email to resolve). This is the true logged-out default, so the
  // vibe-only links must NOT render. Catches a regression that ungates a link
  // or silently flips the default to vibe.
  test('vibe-only links are absent from the anonymous nav', async ({ page }) => {
    await page.goto('/index');
    const nav = page.locator('nav').first();
    await expect(nav).toBeVisible();
    // A core link still renders anonymously — proves the nav itself is present
    // (so the absences below are the gate's doing, not a missing nav).
    await expect(nav.getByRole('link', { name: 'Feed', exact: true })).toBeVisible();
    for (const { label } of VIBE_NAV) {
      await expect(
        nav.getByRole('link', { name: label, exact: true }),
        `${label} must be vibe-gated (hidden by default)`,
      ).toHaveCount(0);
    }
    // The vibe-only "beads" home/logo link is also gated.
    await expect(
      nav.getByRole('link', { name: 'beads', exact: true }),
      'beads home link is vibe-gated',
    ).toHaveCount(0);
  });
});

test.describe('top-nav transitions — vibe mode', () => {
  // Enabling VibeMode is a per-viewer preference resolved from a seeded user
  // bead; send its email so the full nav renders. Applies to every test in
  // this describe.
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': VIBE_EMAIL } });

  for (const { label, path, h1 } of VIBE_NAV) {
    test(`${label} link navigates to ${path}`, async ({ page }) => {
      await page.goto('/index');
      const nav = page.locator('nav').first();
      const link = nav.getByRole('link', { name: label, exact: true });
      await expect(link, `${label} present in vibe-mode top nav`).toBeVisible();

      await Promise.all([
        page.waitForURL(`**${path}`),
        link.click(),
      ]);

      await expect(page.locator('h1').first()).toHaveText(h1);
      await expect(page.locator('nav').first()).toBeVisible();
    });
  }

  test('home link returns to the /garden front door from a sub-page', async ({ page }) => {
    await page.goto('/active');
    await expect(page.locator('h1').first()).toHaveText(/^Active/);

    // The vibe-mode home link → "/": the "gardenio" wordmark sign under the
    // default meadow theme, the plain "beads" link under the plain theme
    // (templates/layout.html). `/` redirects to /garden, the front door
    // (decision gd-e17Jl61EcP); the feed lives at /feed.
    const home = page.locator('nav').first().getByRole('link', { name: /^(gardenio|beads)$/ });
    await expect(home).toHaveAttribute('href', '/');
    await Promise.all([
      page.waitForURL((u) => new URL(u).pathname === '/garden'),
      home.click(),
    ]);
    await expect(page.locator('h1').first()).toContainText(/The Garden/i);
  });
});

test.describe('active-tab indicator (project chip strip)', () => {
  // The chip-strip is the viewer's only active-tab UI today. Assertions
  // here intentionally cover the ARIA contract (`aria-current="true"`)
  // and the visual class (`chip-active`) that the Go test
  // TestHandleProject_URLStateRoundTrip already checks at the
  // request/response level — this is the browser-level counterpart.

  // Resolve a project that definitely has beads by following the first
  // project link from the projects index. The index moved off `/` (now
  // the feed) to `/index`, so resolve project links there. Hard-coding
  // "gardenio" is tempting but brittle — a fresh fixtures-only daemon may
  // use a different project label on the seeded beads.
  async function gotoFirstProject(page) {
    await page.goto('/index');
    const projectLink = page.locator('main, body').first()
      .locator('a[href*="/p/"]').first();
    await expect(projectLink, 'index lists at least one project').toBeVisible();
    const href = await projectLink.getAttribute('href');
    await projectLink.click();
    await page.waitForURL(`**${href}`);
    return href;
  }

  test('lands on project page with the default chip active', async ({ page }) => {
    await gotoFirstProject(page);
    const chipStrip = page.locator('nav.chip-strip');
    await expect(chipStrip, 'project page renders chip strip').toBeVisible();

    const active = chipStrip.locator('.chip-active');
    await expect(active, 'exactly one chip is active').toHaveCount(1);
    await expect(active).toHaveAttribute('aria-current', 'true');
    // Default chip when no `?type=` query is present is "Issues" — see
    // resolveProjectTypeFilter() in server.go (matches the iOS BeadsTab
    // default). If that default ever changes, update this assertion to
    // match the new resolveProjectTypeFilter("") return value.
    await expect(active).toHaveText(/^Issues\b/i);
  });

  test('clicking a non-default chip moves the active marker and updates URL', async ({ page }) => {
    const projectHref = await gotoFirstProject(page);
    const chipStrip = page.locator('nav.chip-strip');

    // Pick the first chip that is NOT currently active. We intentionally
    // do not hard-code "Tasks" / "Decisions" because the chip set varies
    // by what the daemon reports; whichever non-default chip is present
    // is fine.
    const otherChip = chipStrip.locator('a.chip:not(.chip-active)').first();
    await expect(otherChip, 'project has at least one non-default chip').toBeVisible();
    const otherHref = await otherChip.getAttribute('href');
    const otherText = (await otherChip.textContent())?.trim() ?? '';

    await otherChip.click();
    // Chip strip uses hx-boost + hx-push-url — wait for the htmx swap to
    // settle before asserting on the resulting markup, then verify the
    // pushed URL.
    await waitForHtmxIdle(page);
    await page.waitForURL(`**${otherHref}`);

    const newActive = chipStrip.locator('.chip-active');
    await expect(newActive, 'still exactly one active chip').toHaveCount(1);
    await expect(newActive).toHaveAttribute('aria-current', 'true');
    await expect(newActive).toContainText(otherText.split(/\s+/)[0] || otherText);
    // Pushed URL points back at the same project, with a ?type= filter.
    expect(otherHref, 'chip URL stays within project').toContain(projectHref);
    expect(otherHref, 'chip URL adds a type filter').toMatch(/[?&]type=/);
  });
});

test.describe('breadcrumb / back navigation', () => {
  test('project page breadcrumb "Projects" returns to / (the /garden front door)', async ({ page }) => {
    // Reach a project page. Same pattern as above — first project link
    // off the projects index (now at `/index`, since `/` is the feed).
    await page.goto('/index');
    const projectLink = page.locator('a[href*="/p/"]').first();
    await expect(projectLink).toBeVisible();
    await projectLink.click();
    await page.waitForURL(/\/p\//);

    // The project-hub breadcrumb (templates/project_hub.html: <nav class="crumbs">)
    // renders `<a href="/">Projects</a> › <project>`. The "Projects" link
    // points at `/`, which redirects to the /garden front door.
    const back = page.getByRole('link', { name: /^projects$/i });
    await expect(back, 'breadcrumb-back link present').toBeVisible();
    await expect(back).toHaveAttribute('href', '/');

    await Promise.all([
      page.waitForURL((u) => new URL(u).pathname === '/garden'),
      back.click(),
    ]);
    await expect(page.locator('h1').first()).toContainText(/The Garden/i);
  });

  test('browser back restores prior page in a 3-step chain', async ({ page }) => {
    // index → project → seeded bead → back → project → back → index.
    // The projects index lives at `/index` now (`/` is the feed), so the
    // chain starts and ends there.
    await page.goto('/index');
    const indexH1 = await page.locator('h1').first().textContent();
    expect(indexH1).toContain('Beads Viewer');

    const projectLink = page.locator('a[href*="/p/"]').first();
    const projectHref = await projectLink.getAttribute('href');
    await projectLink.click();
    await page.waitForURL(`**${projectHref}`);
    // Project hub h1 = <h1 class="project-hub-title">{{.Project}} …</h1>; the
    // bare project name, not a "Project: " prefix (templates/project_hub.html).
    await expect(page.locator('h1.project-hub-title').first()).toBeVisible();

    // Step into a seeded bead by id (deterministic — does not depend on
    // the project page actually listing the fixture).
    const bid = fixtureId('generic');
    await page.goto(`/${bid}`);
    await expect(page.locator('main, body').first()).toBeVisible();
    // Bead-show page renders the bead id in a <code> within the meta line.
    await expect(page.locator('code').first()).toContainText(bid);

    await page.goBack();
    await page.waitForURL(`**${projectHref}`);
    // Project hub h1 = <h1 class="project-hub-title">{{.Project}} …</h1>; the
    // bare project name, not a "Project: " prefix (templates/project_hub.html).
    await expect(page.locator('h1.project-hub-title').first()).toBeVisible();

    await page.goBack();
    await page.waitForURL((u) => new URL(u).pathname === '/index');
    await expect(page.locator('h1').first()).toHaveText('Beads Viewer');
  });
});
