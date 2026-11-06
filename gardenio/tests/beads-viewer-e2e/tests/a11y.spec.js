// Phase T16 (ka-0000005762) — accessibility baseline. Plan of record:
// `gardenio/plans/beads-viewer-playwright-tests.md`.
//
// Runs `@axe-core/playwright` against every canonical page in the viewer
// and fails the test only on `serious` + `critical` impact violations.
// `moderate` and `minor` are reported (visible in the test stdout) but
// do not gate CI — that threshold is the recommendation in the plan and
// keeps the baseline narrow enough to actually ship green while still
// catching the high-impact regressions an a11y suite is supposed to catch.
//
// Why this shape:
//   - Every canonical route gets one test. A regression that breaks
//     accessibility on (say) `/decisions` only points at one spec, not
//     a generic "axe failed somewhere" message.
//   - Per-type bead-show pages (one per fixture key) are covered too.
//     Each `templates/partials/type_*.html` partial has its own a11y
//     story (badges with no text alternative, color-only state cues,
//     unlabeled form controls in the comment box) and a single
//     fixture page is the cheapest way to cover them.
//   - `serious` + `critical` only. The recommendation in the plan;
//     also the floor that the axe team itself ships as the "must fix"
//     bar. Once the baseline is stable and clean we can ratchet
//     `moderate` in (one rule at a time) per the doc at
//     `gardenio/docs/beads-viewer-a11y-baseline.md`.
//   - Each violation that crosses the threshold is logged with rule
//     id, impact, help URL, and the affected nodes' selectors so the
//     failure message is actionable without re-running locally.
//
// Pure baseline (no exclusions, no rule disables) is intentional. If
// a specific rule needs to be suppressed, do it per-page via
// `disableRules()` here AND record the reason in the baseline doc —
// suppressions without a written rationale rot.

import { execFileSync } from 'node:child_process';

import { expect, test } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

import { fixtureId } from './_helpers.js';
import { mintAgentToken, requireOIDCStub } from './_oidc.js';

const FAILING_IMPACTS = new Set(['serious', 'critical']);

// Pretty-print a single axe violation so the test report is enough to
// fix without re-running the suite locally.
function formatViolation(v) {
  const nodes = v.nodes
    .slice(0, 5)
    .map((n) => `      - ${n.target.join(' ')}`)
    .join('\n');
  const moreNodes = v.nodes.length > 5 ? `\n      … and ${v.nodes.length - 5} more` : '';
  return [
    `  [${v.impact}] ${v.id}: ${v.help}`,
    `    ${v.helpUrl}`,
    `    nodes (${v.nodes.length}):`,
    nodes + moreNodes,
  ].join('\n');
}

// Run axe against the current page and assert no serious/critical
// violations. Logs a one-line summary every time so the baseline
// counts are visible in passing runs too — useful for ratcheting.
async function expectAccessible(page, label) {
  const results = await new AxeBuilder({ page }).analyze();

  const blocking = results.violations.filter((v) => FAILING_IMPACTS.has(v.impact));
  const advisory = results.violations.filter((v) => !FAILING_IMPACTS.has(v.impact));

  // Per-page summary line is intentional — `playwright test` prints
  // these even on green runs, so the baseline numbers are visible.
  // eslint-disable-next-line no-console
  console.log(
    `[a11y] ${label}: ${blocking.length} blocking (serious+critical), ` +
      `${advisory.length} advisory (moderate+minor)`,
  );

  if (blocking.length > 0) {
    const detail = blocking.map(formatViolation).join('\n');
    expect(blocking, `serious/critical a11y violations on ${label}:\n${detail}`).toEqual([]);
  }
}

// Canonical top-level routes — same set as T3 route smoke. Listed
// explicitly (not derived from a route registry) because the point of
// the suite is to catch a regression where a route disappears or its
// shell changes; deriving from the same source as the server would
// hide that.
const ROUTES = [
  { path: '/', label: 'GET /' },
  { path: '/index', label: 'GET /index' },
  { path: '/feed', label: 'GET /feed' },
  { path: '/search', label: 'GET /search' },
  { path: '/active', label: 'GET /active' },
  { path: '/decisions', label: 'GET /decisions' },
  { path: '/agents', label: 'GET /agents' },
  { path: '/advice', label: 'GET /advice' },
  { path: '/triggers', label: 'GET /triggers' },
  { path: '/costs', label: 'GET /costs' },
  // /gardeners and /plans (ka-0000002181): both formerly carried PRE-EXISTING
  // serious color-contrast debt and were held out of this array. That debt is
  // now fixed at the source (templates/gardeners.html, templates/layout.html
  // shared cap-*/verdict/pause chips, and templates/plans.html deck-CTA +
  // header-chip), so both routes join the baseline scan. Notably the /gardeners
  // offenders only surface against the CI-SEEDED daemon — the empty-daemon
  // chrome renders no gardener rows, so a from-pod scan read clean while the
  // seeded CI run tripped 24 findings (the empty-daemon trap that hid this).
  { path: '/gardeners', label: 'GET /gardeners' },
  { path: '/plans', label: 'GET /plans' },
  { path: '/p/gardenio', label: 'GET /p/{project}' },
];

test.describe('a11y baseline — top-level routes', () => {
  for (const { path, label } of ROUTES) {
    test(`${label} has no serious/critical violations`, async ({ page }) => {
      const res = await page.goto(path);
      expect(res, `navigation response for ${path}`).not.toBeNull();
      expect(res.status(), `${path} HTTP status`).toBeLessThan(400);
      await expectAccessible(page, label);
    });
  }
});

// ── Dark-skin (dark-terminal) a11y scan — /gardeners + /plans (ka-0000007487) ──
//
// The ROUTES describe above runs every scan in a FRESH Playwright context, which
// defaults to data-skin="baseline" (layout.html:2 + the pre-paint localStorage
// script) — so it only ever scans the LIGHT skin. That left the whole dark-skin
// (data-skin="dark-terminal") surface UNGATED, and an axe color-contrast sweep
// under the dark skin surfaced a set of PRE-EXISTING serious violations the
// baseline scan can never see:
//
//   - /gardeners: the muted-text chrome (thead th, .cap-id, .cap-goal-label,
//     .cap-ago, .cap-lastrun-meta, .gardeners-intro, .cap-expand caret,
//     .cap-drawer-count, .cap-inv-label, .cap-runs-derived, the "click to see
//     why" note) inked a bare literal #595959 / near-grey tuned only for light —
//     ~2.3–2.7:1 on the #0b0f14 canvas.
//   - /plans: the "+ New plan" compose card (a <details> operators expand)
//     rendered the dark skin's --text (#d6e0ec) on a WHITE card, because its
//     `background: var(--card-bg, #fff)` had no dark --card-bg → ~1.2–1.9:1; plus
//     two opacity-veil offenders (.plan-people-chip-count under .chip's 0.6 veil,
//     .plan-taskgrid-empty's 0.4 veil).
//
// The fix (templates/gardeners.html + templates/plans.html) lifts each to the
// dark skin's own muted palette. This test locks it in: it re-runs the SAME axe
// serious+critical gate the baseline uses, but stamps the dark skin the way the
// viewer does (layout.html's pre-paint script reads localStorage['beads-viewer:
// skin'] — the plans_mr_overflow_ci.spec.js pattern), so a dark-skin contrast
// regression on either route reds HERE, pointing at the skin — not at the
// baseline ROUTES entry, which is skin-blind to it.
//
// Scoped to the two routes this bead covers (/gardeners + /plans). Both are
// already green on the baseline scan against the SAME seeded fixtures, so this
// adds no new fixture dependency and is only as flaky as that green baseline.
const DARK_ROUTES = [
  { path: '/gardeners', label: 'dark /gardeners' },
  { path: '/plans', label: 'dark /plans' },
];

test.describe('a11y — dark-terminal skin (ka-0000007487)', () => {
  test.beforeEach(async ({ page }) => {
    // Stamp the dark skin before the pre-paint script runs on the next
    // navigation (addInitScript persists across page.goto within this context).
    await page.addInitScript(() => {
      try {
        window.localStorage.setItem('beads-viewer:skin', 'dark-terminal');
      } catch (e) {
        /* localStorage always available in the harness */
      }
    });
  });

  for (const { path, label } of DARK_ROUTES) {
    test(`${label} has no serious/critical violations`, async ({ page }) => {
      const res = await page.goto(path);
      expect(res, `navigation response for ${path}`).not.toBeNull();
      expect(res.status(), `${path} HTTP status`).toBeLessThan(400);

      // Confirm the dark skin actually applied — else the scan would silently
      // measure the baseline skin and this gate would be vacuous. The CSS gate
      // is [data-skin^="dark"]; the switcher value is "dark-terminal".
      await expect(page.locator('html')).toHaveAttribute('data-skin', /^dark/);

      // On /plans, expand the "+ New plan" <details> so the compose card (the
      // form that carried cluster-2's white-card-under-dark violation) is
      // actually rendered — axe skips display:none subtrees, so without this the
      // form's contrast would never be measured and the guard would pass over
      // nothing. The disclosure is operator-gated; when present, open it.
      if (path === '/plans') {
        const newPlan = page.locator('details.plan-new');
        if (await newPlan.count()) {
          await newPlan.evaluate((el) => {
            el.open = true;
          });
          await expect(page.locator('.plan-new-form')).toBeVisible();
        }
      }

      await expectAccessible(page, label);
    });
  }
});

// ── /feed kind-badge contrast regression guard (ka-0000004966) ─────────────
//
// Commit 724ee0f9c (ka-0000005146) raised the `/feed` MAIL and SQUAWK kind-badge
// colors to meet WCAG AA (`#0d9488 → #0f766e`, 3.74:1 → 5.47:1; `#0891b2 →
// #0e7490`, 3.68:1 → 5.36:1 on white) but shipped as a templates-only change
// with NO test, so nothing stops the next palette tweak from silently
// re-regressing it. The `/feed` ROUTES entry above runs axe's `color-contrast`
// rule (serious impact → a hard failure) over the whole page, but that guard is
// only meaningful if the rendered feed actually contains BOTH badges for axe to
// measure — and axe skips `display:none` nodes, so a badge hidden by the feed's
// type-filter mutes contributes nothing.
//
// This test makes the guard non-vacuous and pins the invariant directly:
//   1. seed/fixtures.mjs seeds a project:gardenio type=mail (feed_mail) and a
//      type=message+squawk (feed_squawk) bead; each renders one first-paint
//      backfill card (.feed-card-mail / .feed-card-squawk) via handleFeed.
//   2. The MAIL chip is shown by default but the SQUAWK chip starts MUTED
//      (feed.html aria-pressed="false" → feed.js sets display:none), so we click
//      it on and assert BOTH badges are actually VISIBLE — otherwise axe's
//      color-contrast rule would skip the hidden squawk badge and pass vacuously.
//   3. We run axe SCOPED to just the two badges (color-contrast only), so this
//      guard fails iff the mail/squawk kind-badge pair regresses — never on
//      unrelated feed chrome (that is the broad `/feed` ROUTES entry's job).
//   4. Belt-and-suspenders: an explicit ≥4.5:1 (WCAG AA normal text) assertion
//      on each badge's rendered color-vs-background, computed the same way axe
//      does. axe covers the rendered pair; this names the ratio ka-0000005146
//      bought so a failure reads as "the contrast fix regressed", not "axe found
//      something on /feed".
test.describe('a11y — /feed kind-badge contrast (ka-0000005146 regression guard)', () => {
  // WCAG 2.x relative-luminance + contrast-ratio, matching axe-core's own
  // formula, so the explicit assertion and the scoped axe run agree.
  const CONTRAST_FN = `
    (function () {
      function parse(c) {
        var m = c.match(/rgba?\\(([^)]+)\\)/);
        if (!m) return null;
        var p = m[1].split(',').map(function (x) { return parseFloat(x.trim()); });
        return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
      }
      function lin(v) {
        v /= 255;
        return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
      }
      function lum(c) { return 0.2126 * lin(c.r) + 0.7152 * lin(c.g) + 0.0722 * lin(c.b); }
      // Walk up to the first ancestor with a non-transparent background so the
      // effective backdrop matches what axe composites the text over.
      function bgOf(el) {
        var node = el;
        while (node) {
          var c = parse(getComputedStyle(node).backgroundColor);
          if (c && c.a === 1) return c;
          node = node.parentElement;
        }
        return { r: 255, g: 255, b: 255, a: 1 };
      }
      window.__badgeContrast = function (selector) {
        var el = document.querySelector(selector);
        if (!el) return null;
        var fg = parse(getComputedStyle(el).color);
        var bg = bgOf(el);
        var l1 = lum(fg), l2 = lum(bg);
        var hi = Math.max(l1, l2), lo = Math.min(l1, l2);
        return (hi + 0.05) / (lo + 0.05);
      };
    })();
  `;

  test('mail + squawk kind badges meet WCAG AA and pass axe color-contrast', async ({ page }) => {
    const res = await page.goto('/feed');
    expect(res, 'navigation response for /feed').not.toBeNull();
    expect(res.status(), '/feed HTTP status').toBeLessThan(400);

    // Scope to the default-visible Concept-A stream (#feed-stream). handleFeed
    // SSRs each backfill card into BOTH the stream AND its status lane
    // (.feed-lanes, display:none in the default stream view), so an unscoped
    // selector would match two copies per card; the stream copy is the one the
    // default view actually shows and that axe measures.
    const stream = page.locator('#feed-stream');
    const mailBadges = stream.locator('.feed-card-mail .feed-card-kind');
    const squawkBadges = stream.locator('.feed-card-squawk .feed-card-kind');

    // Non-vacuity gate 1: at least one mail card and one squawk card must be in
    // the stream. The seed guarantees a project:gardenio feed_mail + feed_squawk
    // pair (there are also project:inboxdir inbox_mail/inbox_squawk fixtures, so
    // the count is ≥1, not exactly 1). If the classifier ever stopped surfacing
    // mail/squawk — or the seed stopped producing them — this fails LOUD rather
    // than letting the contrast checks below pass over nothing.
    expect(await mailBadges.count(), 'at least one .feed-card-mail badge in stream').toBeGreaterThanOrEqual(1);
    expect(
      await squawkBadges.count(),
      'at least one .feed-card-squawk badge in stream',
    ).toBeGreaterThanOrEqual(1);
    const mailBadge = mailBadges.first();
    const squawkBadge = squawkBadges.first();

    // The mail chip is shown by default; the squawk chip starts muted
    // (feed.html aria-pressed="false"), so its card is display:none and axe
    // would skip it. Click the squawk chip on so the badge is actually rendered
    // and measured — without this, the color-contrast coverage is vacuous.
    const squawkChip = page.locator('.feed-typefilters button[data-type="squawk"]');
    await expect(squawkChip, 'squawk mute chip present').toHaveCount(1);
    if ((await squawkChip.getAttribute('aria-pressed')) !== 'true') {
      await squawkChip.click();
    }

    // Non-vacuity gate 2: both badges must be VISIBLE (not display:none) before
    // axe runs — a hidden node is skipped by the color-contrast rule.
    await expect(mailBadge, 'mail badge visible for axe to measure').toBeVisible();
    await expect(squawkBadge, 'squawk badge visible for axe to measure').toBeVisible();

    // Explicit ratio assertion — the invariant ka-0000005146 bought, named
    // directly. Computed in-page with the WCAG formula axe uses. Pre-fix the
    // mail badge was 3.68:1 and the squawk badge 3.74:1; both must now clear the
    // 4.5:1 AA floor for normal text.
    await page.addScriptTag({ content: CONTRAST_FN });
    const mailRatio = await page.evaluate(() =>
      window.__badgeContrast('#feed-stream .feed-card-mail .feed-card-kind'),
    );
    const squawkRatio = await page.evaluate(() =>
      window.__badgeContrast('#feed-stream .feed-card-squawk .feed-card-kind'),
    );
    expect(mailRatio, 'mail kind-badge contrast ratio').not.toBeNull();
    expect(squawkRatio, 'squawk kind-badge contrast ratio').not.toBeNull();
    expect(
      mailRatio,
      `.feed-card-mail kind badge contrast ${mailRatio?.toFixed(2)}:1 must be ≥ 4.5:1 (WCAG AA)`,
    ).toBeGreaterThanOrEqual(4.5);
    expect(
      squawkRatio,
      `.feed-card-squawk kind badge contrast ${squawkRatio?.toFixed(2)}:1 must be ≥ 4.5:1 (WCAG AA)`,
    ).toBeGreaterThanOrEqual(4.5);

    // Scoped axe run: color-contrast only, limited to the two badges. Fails iff
    // THIS pair regresses — never on unrelated feed chrome. Belt-and-suspenders
    // over the explicit ratio math above (axe composites real rendered pixels).
    const axe = new AxeBuilder({ page })
      .include('#feed-stream .feed-card-mail .feed-card-kind')
      .include('#feed-stream .feed-card-squawk .feed-card-kind')
      .withRules(['color-contrast']);
    const results = await axe.analyze();
    const blocking = results.violations.filter((v) => FAILING_IMPACTS.has(v.impact));
    if (blocking.length > 0) {
      const detail = blocking.map(formatViolation).join('\n');
      expect(blocking, `color-contrast violations on /feed kind badges:\n${detail}`).toEqual([]);
    }
  });
});

// ── /index Live-agents recent-event-stream a11y (ka-0000007896) ────────────
//
// The broad `GET /index` ROUTES entry above axe-scans the projects landing
// page — but the shared seed (seed/fixtures.mjs) carries NO *alive* agent: its
// agent fixtures are agent_state=idle/done/failed, none in the
// {working,spawning,yielding} set computeIndexLiveAgents requires. So that
// entry only ever scans the "No live agents." empty state and NEVER exercises
// the per-agent RECENT EVENT STREAM markup that MR !6697 (e2b876ce9) added:
// templates/index.html renders <ul class="index-active-card-events"> with
// per-row index-active-card-event-{squawk|comment}, a 📣/💬 glyph, and a
// reduced-opacity, small-text -ago stamp. Nested list semantics + glyph-only
// content + tiny timestamps are exactly the shapes axe catches (list /
// color-contrast / non-text content) — and that markup lives ONLY on a
// live-agent card, so absent a seeded live agent the /index scan is vacuous
// for it (the VACUITY TRAP the origin task names).
//
// This test makes the coverage non-vacuous. It seeds an ephemeral ALIVE agent
// with a squawk AND a comment (reusing index_home_rail.spec.js's kd shell-out
// + reload-until-rows approach — squawks are visible the instant `kd create`
// returns; the comment rides the durable EVENTS table and catches up within a
// reload, so gotoHomeUntilRows retries until BOTH rows render), confirms the
// event-stream <ul> actually renders both a -event-squawk and a -event-comment
// <li>, then runs axe SCOPED to that <ul> so a serious/critical violation on
// the event-stream markup fails HERE pointing at it — not at unrelated /index
// chrome (the broad ROUTES entry's job).
//
// ISOLATION: the seeded agent/squawk/comment are EPHEMERAL — unique suffix,
// own fixture:e2e:a11y_rail_* labels, in a DEDICATED project:zza11yrail — so
// under fullyParallel they never race the shared seed or index_home_rail's
// homerail fixtures. The Home Live-agents panel is fleet-wide and sorts
// -updated_at, so a freshly-seeded agent surfaces at the top regardless of the
// rest of the fleet. The card is keyed by its foot agent-link (href ending
// /{agentId}), the one selector that uniquely joins a card to its agent bead.
//
// PROJECT NAME must sort AFTER the shared seed's real projects. /index lists
// projects alphabetically (server.go computeIndexProjects → sort.Strings), and
// filter_chips.spec.js::gotoFirstProject drives the FIRST /p/{project} link on
// /index, expecting an issue-bearing project (its rows must carry data-type /
// data-status). This project holds ONLY a type=agent + type=message bead — no
// issue rows — so if its name sorted before `gardenio` it would BECOME that
// first link and break filter_chips with a null data-type (regression observed
// when this was named `a11yrail`). The `zz` prefix parks it dead-last, after
// gardenio/inboxdir/kbeads/peopledir/plansux, so `gardenio` stays first. Sibling
// ephemeral projects (homerail, plansux) already sort after gardenio for the
// same reason.
//
// LOCAL-RUN NOTE (mirrors index_home_rail.spec.js / plans_signal_strip.spec.js):
// the running-spec leg needs Postgres+pgvector + the seeded daemon this pod
// lacks, so it is verified in CI's hermetic `test-beads-viewer-e2e` task
// (advice ka-0000000850) — which runs on EVERY pipeline, so no run_module
// wiring and no shard-count change.
test.describe('a11y — /index Live-agents recent-event stream (ka-0000007896)', () => {
  // Minting the agent token needs the harness OIDC issuer.
  requireOIDCStub(test);

  const HOME = '/index';
  const PROJECT = 'zza11yrail';

  // kd shells out to the CLI against the same daemon the viewer reads (mirrors
  // index_home_rail.spec.js). Surfaces the daemon's one-line error rather than
  // kd's usage banner on failure.
  function kd(argv) {
    const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
    const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
    try {
      return execFileSync('kd', argv, { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
    } catch (err) {
      const stderr = err.stderr ? err.stderr.toString() : '';
      const firstLine = stderr.split('\n').find((l) => l.trim().length > 0) || err.message;
      throw new Error(`kd ${argv[0]} failed (exit ${err.status}): ${firstLine}`);
    }
  }

  function kdCreateJSON(argv) {
    const raw = kd([...argv, '--json']);
    // Strip any deprecation banner kd prints before the JSON payload.
    const i = raw.search(/[{[]/);
    const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
    if (!bead.id) throw new Error(`kd create returned no id: ${raw}`);
    return bead.id;
  }

  function uniqueSuffix() {
    return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  }

  // Mint an ephemeral ALIVE agent bead (agent_state=working ∈ agentStateAlive)
  // with the schema-required [agent,role,project] fields. The agent NAME is the
  // join key: recentSquawksByAgent keys on squawk source_agent and
  // recentCommentsByAgent on comment author, both matched to fields.agent.
  function createLiveAgent(suffix) {
    const agentName = `e2e-a11y-rail-${suffix}`;
    const agentId = kdCreateJSON([
      'create',
      `[E2E ephemeral] a11y-rail agent ${suffix}`,
      '--type', 'agent',
      '--label', `project:${PROJECT}`,
      '--label', `fixture:e2e:a11y_rail_agent_${suffix}`,
      '--field', `agent=${agentName}`,
      '--field', 'role=crew',
      '--field', `project=${PROJECT}`,
      '--field', 'agent_state=working',
    ]);
    return { agentId, agentName };
  }

  // Closed type=message squawk attributed to agentName via source_agent (the
  // shape `gb squawk` writes). The `squawk` label + source_agent are what
  // recentSquawksByAgent joins on.
  function seedSquawk(agentName, suffix, text) {
    kdCreateJSON([
      'create',
      `[E2E ephemeral] a11y-rail squawk ${suffix}`,
      '--type', 'message',
      '--label', `project:${PROJECT}`,
      '--label', 'squawk',
      '--label', `fixture:e2e:a11y_rail_squawk_${suffix}`,
      '--field', `source_agent=${agentName}`,
      '--field', `text=${text}`,
    ]);
  }

  // Bead comment authored by agentName: written on the agent's own per-agent
  // token (mintAgentToken), since the daemon takes the author from the
  // credential and honours no X-Actor override (gd-ke6PC7bRto). Hung on the
  // agent's own bead; the target is irrelevant to the join (author is the only
  // key). recentCommentsByAgent joins on author.
  async function seedComment(agentId, agentName, text) {
    const token = await mintAgentToken(agentId, agentName);
    kd(['--token', token, 'comment', 'add', agentId, '--body', text]);
  }

  // The ONE Live-agents card whose foot agent-link points at the seeded agent
  // bead — the only selector that uniquely keys a card to its bead.
  function agentCard(page, agentId) {
    return page.locator('li.index-active-card', {
      has: page.locator(`a.index-active-card-agentlink[href$="/${agentId}"]`),
    });
  }

  test('the seeded event-stream <ul> (squawk + comment rows) has no serious/critical violations', async ({
    page,
  }) => {
    const suffix = uniqueSuffix();
    const { agentId, agentName } = createLiveAgent(suffix);
    // Squawk first (older), then a comment (newer, authored after the squawk
    // bead is created) so both feeds render distinct rows carrying both the
    // 📣/💬 glyphs and the -ago stamp axe measures.
    const squawkText = `a11y rail squawk ${suffix}`;
    const commentText = `a11y rail comment ${suffix}`;
    seedSquawk(agentName, suffix, squawkText);
    await seedComment(agentId, agentName, commentText);

    // Reload until BOTH feeds joined onto the card — the squawk is immediate;
    // the comment's durable event catches up within a reload (index_home_rail's
    // gotoHomeUntilRows rationale). /index is a fully static server-render, so a
    // single-shot count can't observe that catch-up; a reload can.
    const card = agentCard(page, agentId);
    await expect(async () => {
      await page.goto(HOME);
      const rows = card.locator('ul.index-active-card-events li.index-active-card-event');
      await expect(rows).toHaveCount(2);
    }).toPass({ timeout: 20000 });

    // Non-vacuity gate: the event-stream <ul> must actually render, carrying
    // BOTH a squawk row (📣) and a comment row (💬) — axe measures only what
    // renders, so an empty stream would scan clean and prove nothing.
    const stream = card.locator('ul.index-active-card-events');
    await expect(stream, 'the recent-event stream <ul> renders').toBeVisible();
    await expect(
      stream.locator('li.index-active-card-event-squawk'),
      'a squawk row (📣) renders for axe to scan',
    ).toHaveCount(1);
    await expect(
      stream.locator('li.index-active-card-event-comment'),
      'a comment row (💬) renders for axe to scan',
    ).toHaveCount(1);

    // Scope axe to exactly THIS card's event-stream <ul> (via :has() on the
    // card's agent-link) so a serious/critical finding on the event-stream
    // markup — nested list semantics, the reduced-opacity -ago timestamp's
    // color-contrast, the glyph-only content — fails HERE with a message that
    // points at it, never at unrelated /index chrome or another fleet card.
    const streamSelector =
      `li.index-active-card:has(a.index-active-card-agentlink[href$="/${agentId}"]) ` +
      'ul.index-active-card-events';
    const results = await new AxeBuilder({ page }).include(streamSelector).analyze();
    const blocking = results.violations.filter((v) => FAILING_IMPACTS.has(v.impact));
    // eslint-disable-next-line no-console
    console.log(
      `[a11y] /index event-stream (${agentId}): ${blocking.length} blocking, ` +
        `${results.violations.length - blocking.length} advisory`,
    );
    if (blocking.length > 0) {
      const detail = blocking.map(formatViolation).join('\n');
      expect(
        blocking,
        `serious/critical a11y violations on /index Live-agents event stream:\n${detail}`,
      ).toEqual([]);
    }
  });
});

// One test per fixture type. The keys are pinned to the canonical
// fixture set declared in `seed/fixtures.mjs` so failures point at a
// specific partial (regression in `type_<key>.html`). Adding a new
// fixture type? Add it here too — keep this list in lockstep with
// seed/fixtures.mjs::FIXTURES.
const FIXTURE_KEYS = [
  'schedule',
  'agent',
  'advice',
  'decision',
  'decision_resolved',
  'formula',
  'mr',
  'generic',
];

test.describe('a11y baseline — per-type bead-show pages', () => {
  for (const key of FIXTURE_KEYS) {
    test(`/{id} bead-show — ${key} fixture has no serious/critical violations`, async ({ page }) => {
      const id = fixtureId(key);
      const res = await page.goto(`/${id}`);
      expect(res, `navigation response for /${id}`).not.toBeNull();
      expect(res.status(), `/${id} HTTP status`).toBeLessThan(400);
      await expectAccessible(page, `bead-show:${key}`);
    });
  }
});
