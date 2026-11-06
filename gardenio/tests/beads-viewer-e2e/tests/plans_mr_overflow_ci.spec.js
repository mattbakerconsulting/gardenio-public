// /plans "+N MR" overflow-pill hidden-CI-failure signal (ka-0000004346,
// backfilling the render leg of feature ka-0000000387 / MR !6656).
//
// THE CLAIM under test (a PACES-P render truth): the /plans index card collapses
// a plan's open MRs past planMRMaxRender(3) into ONE "+N MR" overflow pill. That
// pill used to be a calm-gray count that dropped all CI signal — so a plan whose
// HIDDEN (elided) MRs are FAILING CI read IDENTICALLY to one whose hidden MRs are
// all green. ka-0000000387 folds the elided tail's CI health back onto the pill:
// when any hidden MR failed CI (HasFailedHidden) the pill gains
// .plan-card-mr-more-failed + the shared loud-red .plan-card-mr-ci-failed chrome
// and a trailing "· ✕ K" facet naming the failing count.
//
// The feature shipped with Go coverage only (plans_mrs.go / plans_mrs_test.go /
// plan_card.html). A projection unit test proves FailedHiddenCount computes; it
// does NOT prove the pill actually RENDERS loud-red in the real page in BOTH
// skins. This spec closes that gap end-to-end:
//   1. positive — an elided failed-CI MR tints the overflow pill loud-red and
//      surfaces the failing count on its face + tooltip + a11y label.
//   2. negative — an all-green overflow renders the calm dashed pill WITHOUT the
//      failed class or the ✕ facet (so the test can distinguish "tint worked"
//      from "tint always on").
//   3. dark skin — the loud-red override actually WINS the cascade on the dark
//      skin (the feature shipped its dark override in the same MR; the Go/PACES
//      harness only ever renders the light skin, so no unit test catches a
//      dark-skin regression).
//
// LOCAL-RUN NOTE: this pod has no Postgres server binary / pgvector and no docker
// daemon, and localhost:8080 is the PROD daemon (which must never be seeded), so
// the running-spec leg is verified in CI's hermetic `test-beads-viewer-e2e` task
// (apt postgresql-16 + pgvector → kd serve → beads-viewer). See the suite README.
//
// ISOLATION: every plan here is EPHEMERAL (unique suffix per test, its own beads
// labelled fixture:e2e:plan_mr_ovf_*), created in the dedicated project:plansux —
// never the shared plan_epic seed fixture — so under fullyParallel:true these
// tests never race the index/cockpit read tests in plans.spec.js. AUTH: creating
// beads via `kd` uses the daemon's X-Operated-By attribution (BEADS_OPERATED_BY),
// and the index reads with `?operated_by=` (the empty-key "show all" form) so the
// plan renders regardless of which identity the viewer authenticates as — the
// same scoping plans.spec.js documents.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

// "Show all plans" — the empty operated_by key (see plans.spec.js PLANS_ALL). The
// ephemeral plan is authored as BEADS_OPERATED_BY, not the viewer identity, so the
// default filter-by-me would hide it; the explicit empty key opts into "show all".
const PLANS_ALL = '/plans?operated_by=';

// planMRMaxRender in plans_mrs.go — the inline render cap. A plan with MORE open
// MRs than this collapses the tail into the "+N MR" overflow pill under test.
// Mirrored here (not imported — the Go const is not reachable from JS) so the
// fixtures seed exactly one past the cap. If the Go const changes, bump this and
// the MR counts below in lock-step.
const PLAN_MR_MAX_RENDER = 3;

// Computed loud-red the failed overflow pill resolves to, per skin. Light:
// .plan-card-mr-ci-failed { color:#c62828 } (plans.html). Dark:
// html[data-skin^="dark"] .plan-card-mr-ci-failed { color:#ff8a8a } — the
// AA-legible dark-palette red that must WIN the cascade over the calm-blue
// html[data-skin^="dark"] .plan-card-mr rule. getComputedStyle serializes both to
// rgb() form.
const FAILED_RED_LIGHT = 'rgb(198, 40, 40)'; // #c62828
const FAILED_RED_DARK = 'rgb(255, 138, 138)'; // #ff8a8a

// kd shells out to the CLI (mirrors plans.spec.js / decision_resolve.spec.js) to
// build fixtures against the same daemon the viewer reads. Surfaces the daemon's
// one-line error rather than kd's usage banner on failure.
function kd(args) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  try {
    return execFileSync('kd', args, { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (err) {
    const stderr = err.stderr ? err.stderr.toString() : '';
    const firstLine = stderr.split('\n').find((l) => l.trim().length > 0) || err.message;
    throw new Error(`kd ${args[0]} failed (exit ${err.status}): ${firstLine}`);
  }
}

function kdCreateJSON(args) {
  const raw = kd([...args, '--json']);
  // Strip any deprecation banner kd prints before the JSON payload (matches
  // seed.mjs / plans.spec.js).
  const i = raw.search(/[{[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) throw new Error(`kd create returned no id: ${raw}`);
  return bead.id;
}

// createPlanWithProducedMRs builds an ephemeral plan whose single step PRODUCES a
// list of MR beads — the live linkage planOpenMRsBulk walks (step --produces-->
// merge-request). `mrSpecs` is applied in array order. Returns the epic id keyed
// for the index card selector.
//
// WHY THIS TEST DOES NOT TRY TO CONTROL *WHICH* MR IS ELIDED. The viewer orders a
// plan's MRs "newest-first by the MR bead's update instant truncated to seconds
// (b.UpdatedAt.Unix()), URL-ascending tie-break" (plans_mrs.go). An earlier
// version of this spec tried to pin the elided MR by making it oldest + highest-
// URL — but against the real seeded daemon the observed render order was neither
// pure newest-first NOR pure URL-ascending (four sub-second creates did not tie
// the second the way the client assumed, and the last-created MR — not the
// highest-URL one — landed in the elided tail). Rather than reverse-engineer the
// daemon's exact hydration/updated_at ordering (brittle, and not the contract
// under test), the positive tests make EVERY seeded MR fail CI: then whichever MR
// the ordering elides is NECESSARILY a failed one, so FailedHiddenCount == the
// overflow's MoreCount deterministically — no ordering assumption at all. The
// negative test makes every MR green for the symmetric calm case. The head/tail
// "elided-only, not the visible head" split is already pinned by the Go unit test
// TestPlanMRInfo_FailedHiddenProjection; this spec owns the RENDER leg.
function createPlanWithProducedMRs(suffix, mrSpecs) {
  const epicId = kdCreateJSON([
    'create',
    `[E2E ephemeral] plan mr-overflow ${suffix}`,
    '--type', 'epic',
    '--label', 'plan',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_mr_ovf_${suffix}`,
  ]);
  // One OPEN task step. collectRenderedStepIDs includes every issue-type child
  // regardless of status, so an open step is a fully-rendered timeline step whose
  // forward `produces` edges planOpenMRsBulk walks — no in_progress/assignee dance
  // needed (this test asserts only the MR row, never the step's active chrome).
  const stepId = kdCreateJSON([
    'create',
    `[E2E ephemeral] plan mr-overflow step ${suffix}`,
    '--type', 'task',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_mr_ovf_step_${suffix}`,
  ]);
  // Child → parent parent-child edge (planTimeline walks the epic's reverse
  // parent-child deps), matching the seed's step shape.
  kd(['dep', 'add', stepId, epicId, '--type', 'parent-child']);

  const mrIds = [];
  for (const spec of mrSpecs) {
    const args = [
      'create',
      `[E2E ephemeral] MR ${spec.tag} ${suffix}`,
      '--type', 'merge-request', // first-class kbeads builtin type (config.go)
      '--label', 'project:plansux',
      '--label', `fixture:e2e:plan_mr_ovf_${suffix}_${spec.tag}`,
      '--field', `mr_url=${spec.url}`,
      '--field', 'mr_state=opened',
    ];
    // mr_pipeline_status is what planOpenMRsBulk copies onto the ref and
    // FailedHiddenCount buckets. Omit it to model an MR with no pipeline yet.
    if (spec.pipeline) args.push('--field', `mr_pipeline_status=${spec.pipeline}`);
    const mrId = kdCreateJSON(args);
    // step --produces--> MR: the FORWARD edge planOpenMRsBulk aggregates per plan.
    kd(['dep', 'add', stepId, mrId, '--type', 'produces']);
    mrIds.push(mrId);
  }
  return { epicId, stepId, mrIds };
}

// mrURL builds a distinct canonical GitLab-shaped MR url per fixture MR. Each MR
// needs its OWN url (a shared url would collapse to one pill via the viewer's
// dedup-by-url pass), but the exact ordering the urls induce does NOT matter here
// — see createPlanWithProducedMRs's note on why the tests are ordering-agnostic.
function mrURL(suffix, order, tag) {
  return `https://gitlab.example/mr/ovf-${suffix}-${order}-${tag}`;
}

function uniqueSuffix() {
  return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

// -----------------------------------------------------------------------------
// 1. Positive — a HIDDEN failed-CI MR tints the "+N MR" overflow pill loud red
//    and surfaces the failing count (ka-0000000387 render leg).
// -----------------------------------------------------------------------------
test.describe('plans "+N MR" overflow — hidden-CI-failure signal', () => {
  // Pin the DAY skin. With no stored skin the viewer's SKIN_CLOCK (skin_clock.go)
  // paints dark-homestead from dusk (19:00) to dawn (07:00) local time, and the
  // harness runs in UTC — so an unpinned page reads #ff8a8a after 19:00Z and the
  // light-red assertions below fail on every evening run (gd-y3cxVhNenL). A
  // stored skin wins outright. Only an EMPTY key is filled: Playwright leaves the
  // order of several init scripts undefined, so this must never clobber the
  // dark-terminal the dark-skin test stores for its second navigation.
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(() => {
      try {
        if (!window.localStorage.getItem('beads-viewer:skin')) {
          window.localStorage.setItem('beads-viewer:skin', 'homestead');
        }
      } catch (e) {
        /* ignore — localStorage always available in the harness */
      }
    });
  });

  test('an elided failed-CI MR tints the overflow pill loud-red with a "✕ K" facet', async ({ page }) => {
    const suffix = uniqueSuffix();
    // planMRMaxRender + 1 MRs, ALL failed CI. Exactly one MR overflows (MoreCount
    // = total − cap = 1); because EVERY MR failed, whichever one the viewer's
    // newest-first/URL ordering places in the elided tail is NECESSARILY a failed
    // MR — so FailedHiddenCount == MoreCount == 1 no matter how the daemon orders
    // them. This sidesteps the brittle "make exactly the elided one red" ordering
    // dance (see createPlanWithProducedMRs) while still exercising the real signal:
    // the overflow collapses a hidden FAILED MR, so the pill must go loud-red.
    const mrSpecs = [];
    for (let i = 0; i < PLAN_MR_MAX_RENDER + 1; i++) {
      mrSpecs.push({ tag: `failed${i + 1}`, url: mrURL(suffix, String(i + 1), 'failed'), pipeline: 'failed' });
    }
    const { epicId } = createPlanWithProducedMRs(suffix, mrSpecs);

    await page.goto(PLANS_ALL);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card, 'ephemeral plan card present on /plans').toBeVisible();

    // Setup sanity: exactly planMRMaxRender inline pills render + one overflow, so
    // one MR is genuinely ELIDED (the overflow collapses it rather than rendering
    // its own inline pill — the head/tail split FailedHiddenCount depends on).
    const inlinePills = card.locator('a.plan-card-mr:not(.plan-card-mr-more)');
    await expect(inlinePills).toHaveCount(PLAN_MR_MAX_RENDER);

    const overflow = card.locator('a.plan-card-mr-more');
    await expect(overflow, 'the "+N MR" overflow pill renders').toBeVisible();
    // MoreCount = total(4) − cap(3) = 1.
    await expect(overflow).toContainText('+1 MR');

    // THE SIGNAL: the overflow pill gains BOTH the overflow-failed marker class
    // AND the shared loud-red CI-failed chrome the per-pill badge + ⚠/⛔ attention
    // signals use.
    await expect(overflow).toHaveClass(/plan-card-mr-more-failed/);
    await expect(overflow).toHaveClass(/plan-card-mr-ci-failed/);

    // The "· ✕ K" facet names how many hidden MRs are red, on the pill FACE and in
    // its a11y label.
    const facet = overflow.locator('span.plan-card-mr-more-ci');
    await expect(facet, 'the "✕ K" hidden-failed facet renders').toBeVisible();
    await expect(facet).toContainText('✕'); // ✕
    await expect(facet).toContainText('1');
    await expect(facet).toHaveAttribute('aria-label', '1 hidden MR with failed CI');

    // The tooltip carries the failing count so a keyboard/hover user gets it too.
    await expect(overflow).toHaveAttribute('title', /1 with FAILED CI/);

    // The pill actually PAINTS loud-red on the pinned (homestead) light skin —
    // asserting the rendered color, not just the class, so a future CSS edit that
    // drops the color while keeping the class is caught.
    await expect(overflow).toHaveCSS('color', FAILED_RED_LIGHT);
  });

  // ---------------------------------------------------------------------------
  // 2. Negative — an all-green overflow stays calm. Without this leg the positive
  //    test cannot distinguish "the tint fired because a hidden MR is red" from
  //    "the tint is always on".
  // ---------------------------------------------------------------------------
  test('an all-green overflow renders the calm dashed pill without the failed signal', async ({ page }) => {
    const suffix = uniqueSuffix();
    // planMRMaxRender + 1 MRs, ALL green — the overflow exists (HasMore) but hides
    // nothing red.
    const mrSpecs = [];
    for (let i = 0; i < PLAN_MR_MAX_RENDER + 1; i++) {
      mrSpecs.push({ tag: `ok${i + 1}`, url: mrURL(suffix, String(i + 1), 'ok'), pipeline: 'success' });
    }
    const { epicId } = createPlanWithProducedMRs(suffix, mrSpecs);

    await page.goto(PLANS_ALL);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card).toBeVisible();

    const overflow = card.locator('a.plan-card-mr-more');
    await expect(overflow, 'the overflow pill still renders (4 > cap of 3)').toBeVisible();
    await expect(overflow).toContainText('+1 MR');

    // The calm state: NO failed class, NO shared loud-red chrome, NO ✕ facet.
    await expect(overflow).not.toHaveClass(/plan-card-mr-more-failed/);
    await expect(overflow).not.toHaveClass(/plan-card-mr-ci-failed/);
    await expect(overflow.locator('span.plan-card-mr-more-ci')).toHaveCount(0);
    // And the tooltip must not claim any failing CI.
    await expect(overflow).not.toHaveAttribute('title', /FAILED CI/);
    // The pill paints the calm (non-red) tint — proving the loud-red in the
    // positive test is data-driven, not a static style.
    await expect(overflow).not.toHaveCSS('color', FAILED_RED_LIGHT);
  });

  // ---------------------------------------------------------------------------
  // 3. Dark skin — the loud-red override must WIN the cascade on the dark skin
  //    (the feature shipped its dark override in the same MR; the light-only
  //    PACES/Go harness never renders the dark skin, so only an e2e catches a
  //    dark regression).
  // ---------------------------------------------------------------------------
  test('the failed overflow pill uses the AA-legible dark-red under the dark skin', async ({ page }) => {
    const suffix = uniqueSuffix();
    // All-failed, same as the positive test: the single elided MR is necessarily
    // failed regardless of ordering, so the overflow pill is guaranteed loud-red —
    // this test then checks WHICH red it paints per skin.
    const mrSpecs = [];
    for (let i = 0; i < PLAN_MR_MAX_RENDER + 1; i++) {
      mrSpecs.push({ tag: `failed${i + 1}`, url: mrURL(suffix, String(i + 1), 'failed'), pipeline: 'failed' });
    }
    const { epicId } = createPlanWithProducedMRs(suffix, mrSpecs);

    const cardSel = `li.plan-card[data-plan-id="${epicId}"]`;
    const overflowSel = `${cardSel} a.plan-card-mr-more`;

    // CONTROL: the pinned day (light) skin first — the pill paints the light loud-red.
    // This is the "distinguish the override worked from it-was-always-this-color"
    // discipline applied to the SKIN dimension: the same element must resolve to a
    // DIFFERENT red once the dark skin is applied.
    await page.goto(PLANS_ALL);
    await expect(page.locator('html')).toHaveAttribute('data-skin', 'homestead');
    const overflowLight = page.locator(overflowSel);
    await expect(overflowLight).toBeVisible();
    await expect(overflowLight).toHaveClass(/plan-card-mr-ci-failed/);
    await expect(overflowLight).toHaveCSS('color', FAILED_RED_LIGHT);

    // Apply the dark skin the way the viewer does: layout.html's pre-paint script
    // reads localStorage['beads-viewer:skin'] and stamps html[data-skin] before
    // first paint. Seed it via addInitScript (the copy_buttons.spec.js pattern) so
    // it is present for that inline script on the next navigation.
    await page.addInitScript(() => {
      try {
        window.localStorage.setItem('beads-viewer:skin', 'dark-terminal');
      } catch (e) {
        /* ignore — localStorage always available in the harness */
      }
    });
    await page.goto(PLANS_ALL);

    // Confirm the dark skin actually applied (else the color assertion below would
    // be meaningless). The CSS gate is [data-skin^="dark"]; the switcher value is
    // "dark-terminal".
    await expect(page.locator('html')).toHaveAttribute('data-skin', /^dark/);

    const overflowDark = page.locator(overflowSel);
    await expect(overflowDark).toBeVisible();
    await expect(overflowDark).toHaveClass(/plan-card-mr-ci-failed/);
    // The dark override wins the cascade over the calm-blue dark .plan-card-mr
    // rule: the pill paints the bright dark-palette red, NOT the light one.
    await expect(overflowDark).toHaveCSS('color', FAILED_RED_DARK);
    await expect(overflowDark).not.toHaveCSS('color', FAILED_RED_LIGHT);
  });
});
