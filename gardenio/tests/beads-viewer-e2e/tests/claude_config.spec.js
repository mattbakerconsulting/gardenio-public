// Behavioral E2E for the /claude-config role×knob matrix + /claude-config/{role}
// detail (task ka-0000000682; backfills the coverage gap left by ka-0000006351,
// merged 3c9021bbc). Parent epic ka-0000000958 (E2E coverage-gap sprint).
//
// WHY the Playwright suite (not the shell suite): /claude-config is a
// beads-viewer HTML route. This suite auto-boots the real viewer against a
// real, seeded daemon and runs in e2e.yml's `viewer` job. The shell
// suite has no viewer.
//
// WHAT the unit tests already cover, and what they CANNOT:
//   cmd/beads-viewer/claude_config_test.go + internal/claudeconfig/*_test.go
//   drive the handler/resolver in-process with a hand-fed subscription list and
//   a fake bead lister. They do NOT exercise: route registration on the live
//   mux, template execution through the real layout, or — critically — the
//   REAL resolution path where the viewer derives subscriptions from its
//   configured project (viewerProject) and reads `claude-settings` config beads
//   off the live daemon. This spec covers exactly that wiring.
//
// The fixtures (seed/fixtures.mjs, keys cc_settings_*) mirror the real fleet
// shape verified against prod: per-role `claude-settings` beads carry BOTH
// `project:gardenio` and `role:X` labels, plus one bare `global` effort pin.
// They resolve ONLY because the harness sets BOAT_PROJECT=gardenio on the viewer
// (playwright.config.js) — so `role` rows appearing here is itself proof the
// project→role subscription chain works end-to-end, not a fixture-only shape.
//
// POSITIVE CONTROL (task requirement #4 — the anti-vacuity guard): the alpha and
// beta roles differ on knobs that MUST render differently across their two
// detail pages, so a hardcoded/empty grid could not pass:
//   - ultracode : alpha=true, beta=unset (em-dash)
//   - model     : alpha=unset (em-dash), beta=claude-sonnet-5
//   - effort trap: alpha ON (global `high` pin blocks ultracode's xhigh),
//                  beta absent
// If the two detail pages ever rendered identically (a broken/stubbed matrix),
// the cross-page difference assertions below go red.

import { expect, test } from '@playwright/test';

// The two seeded roles (seed/fixtures.mjs cc_settings_alpha / cc_settings_beta).
// Their `role:` labels are the row vocabulary DiscoverRoles harvests.
const ROLE_ALPHA = 'e2e-cc-alpha'; // ultracode:true, no model → effort TRAP
const ROLE_BETA = 'e2e-cc-beta'; //  model:claude-sonnet-5, ultracode off → no trap
const BETA_MODEL = 'claude-sonnet-5';

// The /claude-config nav link is VibeMode-gated (layout.html, gb-ux #3). Sending
// the seeded vibe operator's email as X-Auth-Request-Email flips VibeMode on so
// the link renders. Keep in sync with the `vibe_user` fixture (gd-1Ct0P1Q4dA).
const VIBE_EMAIL = 'e2e-vibe@example.test';

// The layout also renders `.banner.banner-warn.admin-session-banner` on any page
// served to a roleless session (19705d06 / 0e74c950) — and the e2e session has
// no OIDC roles, so it is always present. It is not the effort-trap banner, so
// exclude it rather than let `.banner-warn` match two elements (strict mode).
const trapBannerLocator = (page) =>
  page.locator('.banner-warn:not(.admin-session-banner)');

test.describe('GET /claude-config — role×knob matrix', () => {
  test('renders the matrix with the seeded roles resolved off the live daemon', async ({
    page,
  }) => {
    const res = await page.goto('/claude-config');
    expect(res, 'navigation response').not.toBeNull();
    expect(res.status(), '/claude-config HTTP status').toBeLessThan(400);

    // Layout shell + page landmark rendered (template executed, not a panic).
    await expect(page.locator('nav').first()).toBeVisible();
    await expect(page.locator('h1').first()).toContainText(
      /Claude Config — per-role effective settings/i,
    );

    // Both seeded roles appear as rows — proof the project:gardenio + role:X
    // beads resolved through the live viewerProject→subscription chain. Each
    // row links to its detail page (the cc-role-cell anchor).
    const alphaRow = page.locator(`a[href$="/claude-config/${ROLE_ALPHA}"]`);
    const betaRow = page.locator(`a[href$="/claude-config/${ROLE_BETA}"]`);
    await expect(alphaRow, `${ROLE_ALPHA} row link`).toBeVisible();
    await expect(betaRow, `${ROLE_BETA} row link`).toBeVisible();
    await expect(alphaRow).toHaveText(ROLE_ALPHA);

    // The effort-trap surfacing is the headline feature: alpha flips ultracode
    // but inherits the global `high` effort pin, so xhigh is blocked. The
    // matrix flags it with the "⚠ blocks xhigh" chip and a warn banner.
    await expect(
      page.getByText(/blocks xhigh/i).first(),
      'effort-trap marker rendered for the trap role',
    ).toBeVisible();
    await expect(trapBannerLocator(page)).toBeVisible();
  });

  test('nav "Claude Config" link (vibe mode) navigates to the matrix', async ({
    page,
  }) => {
    // The link only renders in the full (vibe) nav — send the vibe operator email.
    await page.setExtraHTTPHeaders({ 'X-Auth-Request-Email': VIBE_EMAIL });
    await page.goto('/feed');

    const nav = page.locator('nav').first();
    const link = nav.getByRole('link', { name: 'Claude Config', exact: true });
    await expect(link, 'Claude Config present in the vibe-mode top nav').toBeVisible();

    await Promise.all([page.waitForURL('**/claude-config'), link.click()]);
    await expect(page.locator('h1').first()).toContainText(
      /Claude Config — per-role effective settings/i,
    );
  });
});

test.describe('GET /claude-config/{role} — role detail', () => {
  test(`${ROLE_ALPHA} detail resolves the role and shows its effective knobs + trap`, async ({
    page,
  }) => {
    const res = await page.goto(`/claude-config/${ROLE_ALPHA}`);
    expect(res.status(), `detail HTTP status`).toBeLessThan(400);

    // Page landmark names the resolved role (template executed with real data).
    await expect(page.locator('h1').first()).toContainText(ROLE_ALPHA);
    // The effective-knobs table rendered.
    await expect(page.getByRole('heading', { name: 'Effective knobs' })).toBeVisible();
    // Resolved settings.json pane rendered (the exact document gb setup writes).
    await expect(page.getByRole('heading', { name: /Resolved/ })).toBeVisible();

    // ultracode ON for alpha, and the effort-trap banner is present (global pin
    // blocks xhigh). Both are load-bearing for the positive control below. The
    // detail template's only page-body `.banner-warn` is the effort-trap banner (error /
    // not-found states use `.banner-error`), so the class is an unambiguous
    // handle for it.
    await expect(page.locator('table.ccd-knobs')).toContainText('ultracode');
    const trapBanner = trapBannerLocator(page);
    await expect(trapBanner, 'alpha detail shows the effort-trap banner').toBeVisible();
    await expect(trapBanner).toContainText(/Effort trap/i);
  });

  test(`${ROLE_BETA} detail resolves the role and shows its model override`, async ({
    page,
  }) => {
    const res = await page.goto(`/claude-config/${ROLE_BETA}`);
    expect(res.status(), `detail HTTP status`).toBeLessThan(400);

    await expect(page.locator('h1').first()).toContainText(ROLE_BETA);
    // beta pins model=claude-sonnet-5 — it appears in the knob table AND in the
    // resolved settings.json pane.
    await expect(page.locator('table.ccd-knobs')).toContainText(BETA_MODEL);
    await expect(page.locator('pre.ccd-settings')).toContainText(BETA_MODEL);

    // beta has ultracode OFF → NO effort trap. Absence here is what makes the
    // alpha-has-trap assertion above meaningful.
    await expect(
      trapBannerLocator(page),
      'beta detail must NOT show the effort-trap banner',
    ).toHaveCount(0);
  });

  test('positive control: a knob differs between the two role detail pages', async ({
    page,
  }) => {
    // This is the anti-vacuity guard. A hardcoded/empty/stubbed matrix would
    // render the two detail pages identically; these cross-page differences
    // prove the per-role resolution is real.

    // model: present on beta, absent on alpha.
    await page.goto(`/claude-config/${ROLE_BETA}`);
    await expect(page.locator('pre.ccd-settings')).toContainText(BETA_MODEL);

    await page.goto(`/claude-config/${ROLE_ALPHA}`);
    await expect(
      page.locator('pre.ccd-settings'),
      'alpha carries no model override',
    ).not.toContainText(BETA_MODEL);

    // effort trap: alpha yes, beta no (already asserted per-page above; pinned
    // here as the paired cross-role contrast). The detail template renders the
    // trap as its sole page-body `.banner-warn`.
    await expect(trapBannerLocator(page), 'alpha is a trap').toHaveCount(1);
    await page.goto(`/claude-config/${ROLE_BETA}`);
    await expect(trapBannerLocator(page), 'beta is not a trap').toHaveCount(0);
  });
});
