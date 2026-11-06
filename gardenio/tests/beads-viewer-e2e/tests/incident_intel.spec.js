// ka-0000006319 — Incident Intelligence behavioral coverage.
//
// The Incident Intelligence epic (ka-0000006762) shipped Phases 0–9
// across ~140 Go unit tests in cmd/beads-viewer/. Those unit tests
// pin the renderer behavior with stub daemons; this spec is the
// thin end-to-end layer that proves the wire goes daemon → viewer →
// browser for the four entry points the user actually touches:
//
//   1. type=rich-report bead renders the rich-report page (header
//      meta, headline, back-link to the typed `parent(...)` mention).
//   2. /dev/incident-intel/gallery serves the dev gallery — we use
//      this as a single-URL smoke test that every primitive + widget
//      template at least loads.
//   3. /active inbox surfaces the rich-report row chip + "View report
//      →" CTA; clicking the title routes to the rich-report page;
//      clicking the back-link from the rich-report routes to the
//      intake bead. End-to-end click-through.
//   4. POST /dev/incident-intel/telemetry — known event → 204,
//      unknown event → 400. The endpoint is wired to the static
//      incident_intel_telemetry.js click handler the layout loads.
//
// Sibling Go tests (incident_intel_phase9_test.go, widgets_phase4..7,
// etc.) cover finer-grain behavior; this file deliberately stays
// shallow so a daemon/wire regression points at one spec.

import { expect, test } from '@playwright/test';

import { fixtureId } from './_helpers.js';

test.describe('rich-report render path', () => {
  test('GET /<rich-report-id> renders header, headline, meta, back-link', async ({ page }) => {
    const id = fixtureId('rich_report');
    const intakeID = fixtureId('incident_intake');

    const res = await page.goto(`/${id}`);
    expect(res, 'navigation response for rich-report').not.toBeNull();
    expect(res.status(), 'rich-report HTTP status').toBeLessThan(400);

    // The rich-report container is the entry point for every Phase 9
    // assertion below — if this isn't visible the renderer dispatch
    // never picked the type=rich-report partial.
    const report = page.locator('section.rich-report');
    await expect(report).toBeVisible();

    // Header meta: headline + report_kind + severity all came from the
    // fixture's Fields. A regression in the renderer's field plumbing
    // (e.g. dropping the Fields map before passing to buildRichReportView)
    // would empty all three.
    await expect(report.locator('h2.rich-report-headline')).toHaveText(
      /study-web returning 502s in america-prod/,
    );
    const meta = report.locator('dl.rich-report-meta');
    await expect(meta).toContainText('active-incident');
    await expect(meta).toContainText('P1');
    await expect(meta).toContainText('FSM-9000');

    // Back-link: typed parent(...) mention resolved at seed time. The
    // link must point at the intake bead and carry the Phase 9
    // telemetry attrs so the static JS handler picks it up.
    const back = report.locator('a.rich-report-back');
    await expect(back).toBeVisible();
    await expect(back).toHaveAttribute('href', new RegExp(`/${intakeID}$`));
    await expect(back).toHaveAttribute('data-ii-telemetry', 'rich-report-backlink-click');
    await expect(back).toHaveAttribute('data-ii-telemetry-target', intakeID);
    // The rendered title should be the intake bead's title, not its id —
    // the id falls back only when the title is empty (covered by the Go
    // test TestRichReportView_BackLinkFallsBackToIDWhenTitleEmpty).
    await expect(back.locator('span.rich-report-back-title')).toContainText(
      'FSM-9000 sponsor portal slow',
    );

    // ka-0000003679: rich-report fixture now seeds one widget-instance
    // child per registered widget kind (Phase 4 plain-HTML, Phase 5
    // code-highlighted, Phase 6 diff, Phase 7 time-series). Each child
    // is rendered as <article class="widget-card" data-widget-type=…>.
    // Assert one card per kind is visible — a regression in
    // buildRichReportView's typed-mention/parent-child filter, in the
    // dispatcher (rich_report.go), or in any single registered renderer
    // collapses one of these.
    const widgetTypes = [
      'header-incident-summary',
      'ai-recommended-actions',
      'service-health',
      'affected-files',
      'suggested-test-cases',
      'user-reported-issue-header',
      'reporter-info',
      'reproduction-steps',
      'workaround',
      'related-tickets',
      'activity-log',
      'rca-evidence-cards',
      'source-code-analysis',
      'fix-available-pending-branch',
      'vm-sparklines',
      'event-timeline',
    ];
    for (const t of widgetTypes) {
      await expect(
        report.locator(`article.widget-card[data-widget-type="${t}"]`),
        `widget card for ${t}`,
      ).toBeVisible();
    }
    // No cards should fall through the placeholder branch — that fires
    // when a registered widget renderer returns empty HTML, which would
    // mean the fixture didn't carry the fields the renderer needs.
    await expect(report.locator('article.widget-card-placeholder')).toHaveCount(0);
  });
});

test.describe('dev gallery smoke', () => {
  test('GET /dev/incident-intel/gallery renders all phase sections', async ({ page }) => {
    const res = await page.goto('/dev/incident-intel/gallery');
    expect(res, 'navigation response for gallery').not.toBeNull();
    expect(res.status(), 'gallery HTTP status').toBeLessThan(400);

    // The gallery page is the single visual smoke test that every
    // incident-intel primitive + widget template renders without
    // template parse / data-binding errors. We assert on a small set
    // of section anchors that have been stable across phases — adding
    // a new phase should add a heading here, not silently leave the
    // section unverified.
    // Phase 1 has primitive headings (color tokens, source-kind, etc.)
    // not a "Phase 1" heading; we sample the named-phase headings that
    // do exist so the smoke fails when the dispatch into one of them
    // breaks. See incident_intel_gallery.html for the full list.
    const phases = [/Phase 3 — action descriptors/i, /Phase 4 — plain-HTML widgets/i, /Phase 5 — code-highlighted widgets/i];
    for (const heading of phases) {
      await expect(page.locator('h2', { hasText: heading })).toBeVisible();
    }

    // The Phase-9 telemetry static JS module must be linked from the
    // layout; an isolated regression in the layout (telemetry script
    // tag dropped) would silently break click-through tracking
    // everywhere. Pin it here so the gallery smoke catches it.
    const scripts = page.locator('script[src*="incident_intel_telemetry.js"]');
    await expect(scripts).toHaveCount(1);
  });
});

test.describe('inbox link-out → rich-report → back', () => {
  test('rich-report row carries CTA, click-through routes to rich-report and back', async ({ page }) => {
    const id = fixtureId('rich_report');
    const intakeID = fixtureId('incident_intake');

    // Use the project Work list rather than /active so the assertion is
    // stable regardless of cluster activity. The project-hub redesign
    // (ka-0000001620) made a bare /p/gardenio render the hub Overview;
    // the bead list — the only view that emits per-bead `data-bead-id`
    // rows — is now the "Work" tab reached via ?view=list. The list
    // shows every open bead labelled project:gardenio (default status
    // filter "open", default type filter "issues" which includes
    // rich-report), so the fixture is guaranteed to appear.
    const res = await page.goto('/p/gardenio?view=list');
    expect(res.status(), '/p/gardenio?view=list HTTP status').toBeLessThan(400);

    // Locate the row by id — the row partial sets data-bead-id so
    // tests can address a specific row without depending on its
    // ordering on the page.
    const row = page.locator(`[data-bead-id="${id}"]`).first();
    await expect(row).toBeVisible();

    // The Phase 9 chip class differentiates rich-report rows visually
    // and is the hook that the inbox-rich-report-click telemetry binds
    // to. A regression in projectViewFromBead would drop both.
    const title = row.locator('a.bead-row-title-rich-report');
    await expect(title).toHaveAttribute('data-ii-telemetry', 'inbox-rich-report-click');
    await expect(title).toHaveAttribute('data-ii-telemetry-target', id);

    // The "View report →" CTA is a separate <a> — explicit because
    // the title is also clickable, and operators wanted a discoverable
    // affordance distinct from the title link.
    const cta = row.locator('a.bead-row-rich-report-cta');
    await expect(cta).toHaveText(/View report/);
    await expect(cta).toHaveAttribute('data-ii-telemetry', 'inbox-rich-report-cta-click');
    await expect(cta).toHaveAttribute('data-ii-telemetry-target', id);

    // Click the CTA. We expect a full navigation to the rich-report
    // page, not an htmx swap — the CTA is a plain <a href>.
    await Promise.all([
      page.waitForURL(new RegExp(`/${id}$`)),
      cta.click(),
    ]);
    await expect(page.locator('section.rich-report')).toBeVisible();

    // Back-link round-trip: clicking ← Back lands on the intake bead.
    await Promise.all([
      page.waitForURL(new RegExp(`/${intakeID}$`)),
      page.locator('a.rich-report-back').click(),
    ]);
    // Intake bead is a plain task — assert the page title is present
    // so we know the bead-show route actually rendered, not just that
    // the URL changed.
    await expect(page.locator('h1, h2').filter({ hasText: /FSM-9000 sponsor portal slow/ }).first())
      .toBeVisible();
  });
});

test.describe('telemetry endpoint', () => {
  test('POST with allowlisted event returns 204', async ({ request }) => {
    const res = await request.post('/dev/incident-intel/telemetry', {
      data: {
        event: 'inbox-rich-report-click',
        target: 'gd-fakeid',
        from: '/p/gardenio',
        path: 'a',
      },
    });
    expect(res.status(), 'valid telemetry POST status').toBe(204);
  });

  test('POST with unknown event returns 400', async ({ request }) => {
    const res = await request.post('/dev/incident-intel/telemetry', {
      data: { event: 'definitely-not-allowlisted', target: 'gd-x' },
    });
    expect(res.status(), 'unknown-event telemetry POST status').toBe(400);
  });

  test('POST with non-JSON body returns 400', async ({ request }) => {
    const res = await request.post('/dev/incident-intel/telemetry', {
      headers: { 'Content-Type': 'application/json' },
      data: 'not json',
    });
    expect(res.status(), 'bad-JSON telemetry POST status').toBe(400);
  });

  test('static telemetry asset is served and references sendBeacon', async ({ request }) => {
    const res = await request.get('/assets/static/incident_intel_telemetry.js');
    expect(res.status(), 'telemetry asset HTTP status').toBe(200);
    const body = await res.text();
    expect(body).toContain('data-ii-telemetry');
    expect(body).toContain('sendBeacon');
    expect(body).toContain('/dev/incident-intel/telemetry');
  });

  // ka-0000006089 — Phase 9 sink wiring. The viewer's telemetry handler
  // forwards the click to the daemon's ui_telemetry table; this test posts
  // a uniquely-tagged event through the viewer and then queries the daemon
  // directly to prove the row landed. A regression in either the
  // beads-viewer forward path or the daemon /v1/ui/telemetry endpoint
  // breaks this assertion.
  test('POSTed telemetry event lands in the daemon ui_telemetry sink', async ({ playwright, request }) => {
    const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
    const target = `gd-e2e-sink-${Date.now()}`;
    const post = await request.post('/dev/incident-intel/telemetry', {
      data: {
        event: 'inbox-rich-report-click',
        target,
        from: '/p/gardenio',
        path: '/active',
      },
    });
    expect(post.status(), 'forwarded telemetry POST status').toBe(204);

    // Hit the daemon directly. Authenticate as the primary static token when
    // the harness runs the daemon with auth on (CI sets BEADS_AUTH_TOKEN and
    // exports the SAME value as BEADS_DAEMON_TOKEN — see e2e.yml's
    // `viewer` job, ka-0000006262). GET /v1/ui/telemetry is
    // requireRole(RoleViewer); the primary-static identity bypasses role
    // enforcement but still needs a valid bearer to clear AuthMiddleware.
    // When the token is unset (local auth-disabled dev) we send no header and
    // the daemon's auth-disabled passthrough serves the request as before.
    const daemonToken = process.env.BEADS_DAEMON_TOKEN || process.env.BEADS_AUTH_TOKEN || '';
    const daemon = await playwright.request.newContext({
      baseURL: beadsHTTPAddr,
      extraHTTPHeaders: daemonToken ? { Authorization: `Bearer ${daemonToken}` } : {},
    });
    try {
      const list = await daemon.get('/v1/ui/telemetry?event=inbox-rich-report-click&limit=50');
      expect(list.status(), 'daemon /v1/ui/telemetry status').toBe(200);
      const body = await list.json();
      const rows = body.rows ?? [];
      const hit = rows.find((r) => r.target === target);
      expect(hit, `expected sink to contain row with target=${target}; saw ${rows.length} rows`).toBeTruthy();
      expect(hit.event).toBe('inbox-rich-report-click');
      expect(hit.from).toBe('/p/gardenio');
      expect(hit.path).toBe('/active');
    } finally {
      await daemon.dispose();
    }
  });
});

// ka-0000003679: per-widget-family interaction smoke. One distinctive
// click/render assertion per family — Go unit tests cover renderer
// internals, these prove the seeded payload + static JS wire end-to-end.

test.describe('plain-HTML family — header-incident-summary Acknowledge', () => {
  test('clicking Acknowledge opens the confirm modal', async ({ page }) => {
    const id = fixtureId('rich_report');
    await page.goto(`/${id}`);

    const ack = page
      .locator('article.widget-card[data-widget-type="header-incident-summary"]')
      .locator('button.ii-action-btn', { hasText: 'Acknowledge' });
    await expect(ack).toBeVisible();
    await ack.click();

    // confirmModal in incident_intel.js builds .ii-modal-backdrop > .ii-modal
    // with the title from the ConfirmDescriptor on AckIncidentAction.
    const modal = page.locator('.ii-modal-backdrop .ii-modal');
    await expect(modal).toBeVisible();
    await expect(modal.locator('.ii-modal-title')).toContainText(/Acknowledge incident/i);
    await expect(modal.locator('button.ii-modal-confirm')).toBeVisible();

    // Cancel — no POST should fire (we don't have the ack endpoint wired
    // here), and the backdrop should detach.
    await modal.locator('button.ii-modal-cancel').click();
    await expect(page.locator('.ii-modal-backdrop')).toHaveCount(0);
  });
});

test.describe('code-highlighted family — rca-evidence-cards drill-down drawer', () => {
  test('clicking Drill down opens the drawer with template body', async ({ page }) => {
    const id = fixtureId('rich_report');
    await page.goto(`/${id}`);

    const card = page.locator('article.widget-card[data-widget-type="rca-evidence-cards"]');
    await expect(card).toBeVisible();

    // Each card has its own <button class="ii-w-rca-drill" data-ii-drawer-key>
    // and a sibling <template data-ii-drawer-key="…"> with the body. We
    // exercise the first card — the binder is per-button, not per-card.
    const drill = card.locator('button.ii-w-rca-drill').first();
    await expect(drill).toBeVisible();
    await drill.click();

    const drawer = page.locator('.ii-drawer-backdrop .ii-drawer');
    await expect(drawer).toBeVisible();
    await expect(drawer.locator('.ii-drawer-title')).toBeVisible();
    await expect(drawer.locator('.ii-drawer-body')).not.toBeEmpty();

    await drawer.locator('.ii-drawer-close').click();
    await expect(page.locator('.ii-drawer-backdrop')).toHaveCount(0);
  });
});

test.describe('diff family — fix-available-pending-branch render', () => {
  test('renders branch name, CI pill, and Fast-track merge button', async ({ page }) => {
    const id = fixtureId('rich_report');
    await page.goto(`/${id}`);

    const card = page.locator(
      'article.widget-card[data-widget-type="fix-available-pending-branch"]',
    );
    await expect(card).toBeVisible();

    // Branch name surfaces from the unified diff payload's metadata.
    await expect(card.locator('code.ii-w-pending-branch-name')).toBeVisible();

    // CI pill — fixture seeds ci_status: passed → normaliseCIStatus keeps
    // the literal "passed", which the template binds to data-state.
    await expect(card.locator('.ii-pill[data-state="passed"]')).toBeVisible();

    // The Fast-track merge button is only emitted when the diff has at
    // least one hunk — fixture seeds a one-file/one-hunk patch, so this
    // is the regression alarm for the diff parser collapsing.
    await expect(
      card.locator('button.ii-action-btn', { hasText: 'Fast-track merge' }),
    ).toBeVisible();
  });
});

test.describe('time-series family — vm-sparklines uPlot canvas', () => {
  test('renders one sparkline canvas per series with a uPlot config', async ({ page }) => {
    const id = fixtureId('rich_report');
    await page.goto(`/${id}`);

    const card = page.locator('article.widget-card[data-widget-type="vm-sparklines"]');
    await expect(card).toBeVisible();

    // Fixture seeds two series; sparkConfig is emitted on each canvas.
    // The static JS upgrades these into uPlot charts client-side, but
    // we assert on the server-rendered shape so the test is independent
    // of whether the uPlot bundle has loaded.
    const canvases = card.locator('.ii-w-sparkline-canvas[data-uplot-config]');
    await expect(canvases).toHaveCount(2);
    // Config attribute should be a non-empty JSON object.
    const cfg = await canvases.first().getAttribute('data-uplot-config');
    expect(cfg).toMatch(/^\{.*\}$/);
  });
});
