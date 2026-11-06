// Capture the canonical beads-viewer pages for MR before/after review.
//
// The seed-data contract (owned by ka-0000007138 "local dev run path"):
//   - SCREENSHOT_PROJECT  slug of a seeded project (has >= 1 open bead)
//   - SCREENSHOT_BEAD_ID  id of a seeded bead that has comments AND deps
//   - SCREENSHOT_QUERY    a search query that returns at least one result
//
// Defaults are placeholders chosen to match reasonable seed data; override
// via env once the local-dev seed fixtures are finalized. Local-dev boot
// docs live in gardenio/docs/beads-viewer-dev.md.

import { test, expect } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';

const PROJECT = process.env.SCREENSHOT_PROJECT || 'gardenio';
const BEAD_ID = process.env.SCREENSHOT_BEAD_ID || 'ka-0000001180';
const QUERY = process.env.SCREENSHOT_QUERY || 'screenshot';
// SCREENSHOT_USER_SLUG selects the per-user homepage (/u/{slug}) captured for
// the Users-Page CDQR gardener (ka-0000000737). The CDQR audit formula exports a
// real prod operator slug here; when unset the user-page capture is skipped so
// the default canonical-pages run (which has no seeded user) stays green.
const USER_SLUG = process.env.SCREENSHOT_USER_SLUG || '';
// SCREENSHOT_USER_SELF_EMAIL is the OWNER's OIDC email for the /u/{slug} page.
// When set alongside SCREENSHOT_USER_SLUG, the harness captures a SECOND detail
// render as page 'user-home-self' with the owner's email stamped on the
// X-Auth-Request-Email header — which the dev-auth-bypass path promotes into the
// verified-identity context (dev_auth.go forwardAuthTrustedEmailToContext), so
// resolver.OperatorID matches the page operator and IsViewerSelf renders TRUE.
// This grades the owner variant of /u/{slug} (the "(you)" marker + self-only
// status-message editor) that the non-self 'user-home' capture never exercises,
// closing the coverage gap in bug gd-zFNvLriG4D WITHOUT a real Keycloak session:
// the audit ran every iteration under dev-bypass with IsViewerSelf=false, so the
// owner rendering of the pin/edit/inbox-write affordances went ungraded. When
// unset the self capture is skipped (default canonical-pages run stays green).
// The header is trusted ONLY on the dev-bypass path; on the AVA/Keycloak edge it
// is client-supplied and ignored, so injecting it there is a harmless no-op.
const USER_SELF_EMAIL = process.env.SCREENSHOT_USER_SELF_EMAIL || '';
// SCREENSHOT_MAPS_GARDENER selects the per-gardener map (/maps/{gardenerID})
// captured for the Maps-Beautification gardener (ka-0000001933). The maps
// beauty audit formula exports a real prod gardener bead ID here so the rubric
// scores a /maps/{id} radial constellation rendered with live data. When unset
// the per-gardener map capture is skipped (the global /maps fleet page always
// captures). The captured page names are 'maps-fleet' (/maps) and 'maps-gardener'
// (/maps/{id}).
const MAPS_GARDENER = process.env.SCREENSHOT_MAPS_GARDENER || '';
// SCREENSHOT_GARDENERS_DETAIL selects the per-gardener life-thread page
// (/gardener/{gardenerID}) captured for the Gardeners-Page CAMP gardener
// (epic ka-0000004574 follow-on). The gardeners-page audit formula exports a
// real prod gardener bead ID here so the rubric scores a /gardener/{id} detail
// page rendered with live data. The global /gardeners index always captures
// (page name 'gardeners-index'); the detail page (page name 'gardener-detail')
// is skipped unless this is set. NOTE: this is the management-board surface,
// NOT the /maps constellation — no graph viz is captured here.
const GARDENERS_DETAIL = process.env.SCREENSHOT_GARDENERS_DETAIL || '';
// SCREENSHOT_FEED selects the activity-feed front page (/feed) captured for the
// Feed Signal-Quality (S-L-R) audit gardener (epic ka-0000004907). The feed
// audit formula sets this to '1' so the rubric scores the live /feed render —
// the chronological card stream the team is promoting to '/'. The feed mounts a
// long-lived SSE channel (/feed/events, bead_feed_hub.go), so it is captured in
// SSE mode (waitUntil 'load' + a longer settle so the first-paint seed + a few
// live cards land). Captured page names are 'feed' (/feed default chronological)
// and 'feed-lanes' (/feed?view=lanes). Skipped unless SCREENSHOT_FEED is set so
// the default canonical-pages run stays scoped + fast.
const FEED = process.env.SCREENSHOT_FEED || '';

// Canonical output folder: test-results/beads-viewer-screenshots/<viewport>/<page>.png
// The MR formula consumes this directory layout for before/after diffing.
const OUTPUT_ROOT = path.resolve(
  process.cwd(),
  'test-results/beads-viewer-screenshots',
);

function outPath(viewport, page, ext = 'png') {
  return path.join(OUTPUT_ROOT, viewport, `${page}.${ext}`);
}

// Disable animations, transitions, and caret blink so screenshots diff meaningfully.
async function freezeRendering(page) {
  await page.addStyleTag({
    content: `
      *, *::before, *::after {
        animation-duration: 0s !important;
        animation-delay: 0s !important;
        transition-duration: 0s !important;
        transition-delay: 0s !important;
        caret-color: transparent !important;
        scroll-behavior: auto !important;
      }
      html { scroll-behavior: auto !important; }
    `,
  });
}

async function capture(page, viewport, name, url, opts = {}) {
  // Pages that mount a long-lived SSE channel (e.g. /agents — gd-Ahapvqc1ND)
  // never reach networkidle, so the spec would time out at 60s. For those
  // pages use 'load' + an explicit settle delay; non-SSE pages keep the
  // stricter 'networkidle' wait so the screenshot reflects all htmx partials.
  // Tracked in gd-1u7F5GTTvD.
  const { sse = false, settleMs = 3000, headers = null } = opts;
  const target = outPath(viewport, name);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  // Per-capture extra request headers (merged with the config-level bearer).
  // Used by the /u/{slug} self-variant capture to stamp X-Auth-Request-Email so
  // the dev-bypass path renders IsViewerSelf=true (bug gd-zFNvLriG4D).
  if (headers) {
    await page.setExtraHTTPHeaders(headers);
  }
  const response = await page.goto(url, {
    waitUntil: sse ? 'load' : 'networkidle',
  });
  expect(response, `navigation to ${url}`).not.toBeNull();
  expect(response.status(), `HTTP status for ${url}`).toBeLessThan(400);
  if (sse) {
    await page.waitForTimeout(settleMs);
  }
  await freezeRendering(page);
  // One rAF tick to settle layout after the style injection.
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));
  await page.screenshot({ path: target, fullPage: true });
  // Also persist post-render DOM so the info-density measure step has real
  // bytes to weigh (data-fields, dom-element count, text/byte ratio). Saving
  // page.content() — not the raw HTTP body — picks up htmx-loaded partials.
  const html = await page.content();
  fs.writeFileSync(outPath(viewport, name, 'html'), html);
}

for (const viewport of ['desktop', 'narrow']) {
  test.describe(`beads-viewer @ ${viewport}`, () => {
    test.beforeEach(({}, testInfo) => {
      test.skip(testInfo.project.name !== viewport, 'viewport-scoped');
    });

    test('index page /', async ({ page }) => {
      await capture(page, viewport, 'index', '/');
    });

    test('project page /p/<project>', async ({ page }) => {
      await capture(page, viewport, 'project', `/p/${encodeURIComponent(PROJECT)}`);
    });

    test('bead-show page /<bead-id>', async ({ page }) => {
      // NOTE: The viewer exposes bead-show at `/{id}` (not `/b/{id}`); the
      // `/b/{id}/{comments,deps}` routes are htmx partials loaded into this
      // page. Captured URL matches the human-facing canonical URL.
      await capture(page, viewport, 'bead-show', `/${encodeURIComponent(BEAD_ID)}`);
    });

    test('search results /search?q=…', async ({ page }) => {
      await capture(
        page,
        viewport,
        'search',
        `/search?q=${encodeURIComponent(QUERY)}`,
      );
    });

    test('agents page /agents', async ({ page }) => {
      await capture(page, viewport, 'agents', '/agents', { sse: true });
    });

    test('costs page /costs', async ({ page }) => {
      await capture(page, viewport, 'costs', '/costs');
    });

    test('agents page filtered /agents?project=…', async ({ page }) => {
      await capture(
        page,
        viewport,
        'agents-filtered',
        `/agents?project=${encodeURIComponent(PROJECT)}`,
        { sse: true },
      );
    });

    test('advice page /advice', async ({ page }) => {
      await capture(page, viewport, 'advice', '/advice');
    });

    test('health page /health', async ({ page }) => {
      // T4 (ka-0000000085): /health is a 7-tile grid with htmx pollers.
      // Capture for MR before/after diffing of CSS polish + tile layout.
      await capture(page, viewport, 'health', '/health');
    });

    test('inbox page /inbox', async ({ page }) => {
      // gd-gQg7OGSgzA: /inbox is the gardener's primary surface — a unified
      // Gmail-density activity stream (handler: handleInbox in inbox.go).
      // Captured so the info-density audit formula (ka-0000002694) produces
      // metrics + verdict for /inbox alongside the other canonical pages.
      await capture(page, viewport, 'inbox', '/inbox');
    });

    test('user homepage /u/<slug>', async ({ page }) => {
      // ka-0000000737: per-user homepage (/u/{slug}, handler: handleUserHome
      // in user_home.go). Captured under the page name 'user-home' so the
      // Users-Page CDQR gardener scores Correctness/Density/Quality/Relevance
      // on a real prod operator. Skipped unless SCREENSHOT_USER_SLUG is set so
      // the default canonical-pages run (no seeded user) stays green.
      test.skip(!USER_SLUG, 'SCREENSHOT_USER_SLUG not set');
      await capture(
        page,
        viewport,
        'user-home',
        `/u/${encodeURIComponent(USER_SLUG)}`,
      );
    });

    test('user homepage self /u/<slug> (owner)', async ({ page }) => {
      // gd-zFNvLriG4D: the OWNER (self) variant of /u/{slug}. The plain
      // 'user-home' capture above runs under dev-auth-bypass with no viewer
      // identity, so IsViewerSelf=false and the owner-only affordances (the
      // "(you)" marker, the self status-message editor, and every write control
      // gated on IsViewerSelf) never render — leaving that variant ungraded
      // every iteration. Here we stamp the owner's OIDC email on
      // X-Auth-Request-Email; the dev-bypass middleware promotes it into the
      // verified-identity context (dev_auth.go forwardAuthTrustedEmailToContext),
      // so resolver.OperatorID matches the page operator and IsViewerSelf=true.
      // Captured as page 'user-home-self'. Skipped unless BOTH the slug and the
      // owner email are set, so the default canonical run is unaffected.
      test.skip(
        !USER_SLUG || !USER_SELF_EMAIL,
        'SCREENSHOT_USER_SLUG or SCREENSHOT_USER_SELF_EMAIL not set',
      );
      await capture(
        page,
        viewport,
        'user-home-self',
        `/u/${encodeURIComponent(USER_SLUG)}`,
        { headers: { 'X-Auth-Request-Email': USER_SELF_EMAIL } },
      );
    });

    test('maps fleet /maps', async ({ page }) => {
      // ka-0000001933: the global all-gardeners fleet map + picker (handler:
      // handleMapsIndex in maps.go). Captured under 'maps-fleet' so the
      // Maps-Beautification gardener scores Correctness/Density/Quality/Relevance
      // on the live fleet constellation. Skipped unless SCREENSHOT_MAPS_GARDENER
      // is set, so the default canonical-pages run stays scoped + fast.
      test.skip(!MAPS_GARDENER, 'SCREENSHOT_MAPS_GARDENER not set');
      await capture(page, viewport, 'maps-fleet', '/maps');
    });

    test('maps gardener /maps/<gardenerID>', async ({ page }) => {
      // ka-0000001933: the per-gardener radial constellation map (handler:
      // handleMap in maps.go). Captured under 'maps-gardener' for the same
      // rubric. Skipped unless SCREENSHOT_MAPS_GARDENER names a real gardener.
      test.skip(!MAPS_GARDENER, 'SCREENSHOT_MAPS_GARDENER not set');
      await capture(
        page,
        viewport,
        'maps-gardener',
        `/maps/${encodeURIComponent(MAPS_GARDENER)}`,
      );
    });

    test('feed front page /feed', async ({ page }) => {
      // ka-0000004907: the activity-feed front page (handler: handleFeed in
      // feed.go) — the chronological card stream slated to become '/'. Captured
      // under 'feed' so the Feed Signal-Quality (S-L-R) audit scores Signal /
      // Legibility / Rhythm on the live feed render. The page mounts a
      // long-lived SSE channel (/feed/events), so capture in SSE mode with a
      // longer settle to let the seed + a few live cards paint. Skipped unless
      // SCREENSHOT_FEED is set so the default canonical run stays fast.
      test.skip(!FEED, 'SCREENSHOT_FEED not set');
      await capture(page, viewport, 'feed', '/feed', { sse: true, settleMs: 6000 });
    });

    test('feed lanes /feed?view=lanes', async ({ page }) => {
      // ka-0000004907: the lane-grouped variant of the feed (now /
      // recently-changed / just-done). Captured under 'feed-lanes' for the same
      // S-L-R rubric so the audit can score whether the lane grouping improves
      // legibility over the flat chronological stream. SSE-mode like /feed.
      test.skip(!FEED, 'SCREENSHOT_FEED not set');
      await capture(page, viewport, 'feed-lanes', '/feed?view=lanes', {
        sse: true,
        settleMs: 6000,
      });
    });

    test('gardeners index /gardeners', async ({ page }) => {
      // Gardeners-Page CAMP gardener: the global gardeners management board
      // (handler: handleGardeners in gardeners.go) — every gardener as a row with
      // verdict, last-run TLDR, counters, next-fire, and filter chips. Captured
      // under 'gardeners-index' so the rubric scores Coverage/Accuracy/
      // Management/Polish on the live fleet of gardeners. This is the management
      // table surface, NOT the /maps constellation. Always captures.
      await capture(page, viewport, 'gardeners-index', '/gardeners');
    });

    test('gardener detail /gardener/<gardenerID>', async ({ page }) => {
      // Gardeners-Page CAMP gardener: the per-gardener life-thread detail page
      // (handler: handleGardenerDetail in gardener_page.go) — gardener header +
      // iteration cards. Captured under 'gardener-detail' for the same rubric.
      // Skipped unless SCREENSHOT_GARDENERS_DETAIL names a real gardener.
      test.skip(!GARDENERS_DETAIL, 'SCREENSHOT_GARDENERS_DETAIL not set');
      await capture(
        page,
        viewport,
        'gardener-detail',
        `/gardener/${encodeURIComponent(GARDENERS_DETAIL)}`,
      );
    });

    test('people directory /people', async ({ page }) => {
      // People-Directory CDQR gardener (epic ka-0000005027): the public operator
      // directory (handler: handlePeople in people.go) — the fleet's who's-who,
      // rendering canonical humans after collapsing duplicate identities, each
      // row with per-operator stat chips (open/created/closed/MRs/decisions/
      // fleet/last-active) and linked-identity chips, above a page-level totals
      // header (people · identities merged · active now · open beads · …).
      // Captured under 'people' so the CDQR rubric scores Correctness/Density/
      // Quality/Relevance/Dedup on the live directory. Not an SSE page — the
      // htmx hover-cards load on mouseover, so the default 'networkidle' wait
      // captures the fully-rendered directory. Always captures (mirrors how the
      // agents/gardeners directory pages are captured).
      await capture(page, viewport, 'people', '/people');
    });
  });
}
