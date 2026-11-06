// Behavioral coverage for /gardener/{id} — the Gardener Page (epic
// ka-0000001579; handler gardenio/controller/cmd/beads-viewer/gardener_page.go,
// template templates/gardener_detail.html). Task gd-DkG9VPzBgX.
//
// The gardener page is the operator's "is this gardener winning?" view. It
// renders, in order:
//
//   1. a header (title + enabled/closed badge + a one-line health verdict),
//   2. header counters (Iterations / Runs spawned / Runs completed / Max runs /
//      Run budget) from the embedded GardenerSpec,
//   3. budget + exit config chips (exit / on-exit / retry / timeout / …),
//   4. a Trajectory strip — a per-run sparkline + aggregate outcome signals —
//      so direction reads at a glance instead of being inferred from the list,
//   5. the life-thread: one iteration card per spawned run, OUTCOME-FIRST
//      (the run's wrap-up tldr/derived outcome is the headline, not the bare
//      agent name — ka-0000001187).
//
// Auth: same as every other spec in this dir — the suite boots the viewer with
// BEADS_VIEWER_DEV_AUTH_BYPASS=1 (playwright.config.js webServer env), so a
// plain page.goto() is an authenticated read. No per-test bearer/fixture is
// needed for read-scoped pages; specs that exercise write surfaces add an
// X-Auth-Request-Email header (see comment_write.spec.js / responsive.spec.js).
// The gardener page in v1 is read-only (control actions stay on /triggers), so
// no header is needed here.
//
// Fixtures (seed/fixtures.mjs): `gardener` is a type=trigger + trigger:schedule +
// role:gardener bead carrying a `gardener` JSON spec (counters, budget, exit
// condition, and a progress_state with 3 runs + 3 credited events so the
// trajectory parses non-empty). `gardener_run_1..3` are three CLOSED iteration
// agents chained via schedule_prior_agent_id and stamped schedule_id == the
// gardener id, each with a distinct wrap-up so the cards render different
// outcome headlines and exercise both the clean-exit and blocked-exit chips.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

test.describe('/gardener/{id} gardener detail page', () => {
  test('renders the page for a known gardener id (header + verdict)', async ({ page }) => {
    const id = fixtureId('gardener');
    const res = await page.goto(`/gardener/${id}`);
    expect(res.status(), 'gardener page HTTP status').toBeLessThan(400);

    // Title carries the gardener's spec Name and an enabled badge (the fixture
    // seeds enabled=true). The id is surfaced in the header meta.
    const title = page.locator('h1.gardener-title');
    await expect(title).toBeVisible();
    await expect(title).toContainText('E2E demo-drive gardener');
    await expect(title.locator('.gardener-badge-enabled')).toBeVisible();
    await expect(page.locator('.gardener-header-meta .bead-row-id')).toHaveText(id);

    // The one-line health verdict is the operator's at-a-glance signal. The
    // fixture's progress_state credits an mr_merged + two child_closed events,
    // so the verdict is the "making progress" kind, not stalled/stuck.
    const verdict = title.locator('.gardener-verdict').first();
    await expect(verdict).toBeVisible();
    await expect(verdict).toHaveClass(/gardener-verdict-progress/);
    await expect(verdict).toContainText('making progress');
  });

  test('header counters render from the gardener spec', async ({ page }) => {
    const id = fixtureId('gardener');
    await page.goto(`/gardener/${id}`);

    const stats = page.locator('dl.gardener-stats');
    await expect(stats).toBeVisible();

    // Iterations is the AUTHORITATIVE live count == the number of rendered
    // iteration cards (3). Runs spawned / completed are the engine-snapshot
    // secondaries; the fixture pins all three to 3 so no counter-drift
    // annotation appears.
    const iterations = stats.locator('div', { has: page.locator('dt', { hasText: /^Iterations$/ }) });
    await expect(iterations.locator('dd')).toContainText('3');
    await expect(stats.locator('.gardener-counter-drift')).toHaveCount(0);

    // Max runs (10) is the budget snapshot; presence pins the spec→header path.
    await expect(stats.getByText('Max runs', { exact: true })).toBeVisible();
    const maxRuns = stats.locator('div', { has: page.locator('dt', { hasText: /^Max runs$/ }) });
    await expect(maxRuns.locator('dd')).toHaveText('10');
  });

  test('budget and exit config chips are visible', async ({ page }) => {
    const id = fixtureId('gardener');
    await page.goto(`/gardener/${id}`);

    // The Run budget counter (run_budget=500000) is the spend ceiling the
    // operator reads off the header stats.
    const stats = page.locator('dl.gardener-stats');
    await expect(stats.getByText('Run budget', { exact: true })).toBeVisible();
    const budget = stats.locator('div', { has: page.locator('dt', { hasText: /^Run budget$/ }) });
    await expect(budget.locator('dd')).toHaveText('500000');

    // The config chip strip carries the exit condition + on-exit + retry +
    // timeout chips parsed from the spec. The exit chip is the headline one.
    const config = page.locator('.gardener-config');
    await expect(config).toBeVisible();
    const exitChip = config.locator('.chip-type', { hasText: /^exit:/ });
    await expect(exitChip).toBeVisible();
    await expect(exitChip).toContainText('epic gd-demoEpic closed');
    // on-exit + retry chips also derive from the spec (on_exit=notify,
    // retry_on_failure + max_retries=2).
    await expect(config.locator('.chip-type', { hasText: /^on exit:/ })).toBeVisible();
    await expect(config.locator('.chip-type', { hasText: /^retry/ })).toBeVisible();
  });

  test('trajectory strip is present with sparkline bars', async ({ page }) => {
    const id = fixtureId('gardener');
    await page.goto(`/gardener/${id}`);

    const strip = page.locator('section.gardener-trajectory');
    await expect(strip).toBeVisible();
    await expect(strip.locator('.gardener-trajectory-heading')).toHaveText('Trajectory');

    // The sparkline draws one bar per recorded run. The fixture seeds three
    // runs (two scored "ok", one "failed"), so there are exactly three bars and
    // they carry the per-outcome class hook the CSS colors by.
    const spark = strip.locator('.gardener-spark');
    await expect(spark).toBeVisible();
    const bars = spark.locator('.gardener-spark-bar');
    await expect(bars).toHaveCount(3);
    // At least one winning bar and the one failing bar must be classed so a
    // CSS-classname regression (the bars all rendering the same color) is
    // caught. Each bar also carries a non-zero inline height.
    await expect(spark.locator('.gardener-spark-ok')).toHaveCount(2);
    await expect(spark.locator('.gardener-spark-fail')).toHaveCount(1);
    const firstHeight = await bars.first().evaluate((el) => el.style.height);
    expect(firstHeight, 'spark bar has an inline height').toMatch(/\d+%/);

    // Aggregate signals: the trajectory stats dl surfaces the recorded-run
    // count and the credited outcome events (1 MR merged, 2 beads closed).
    const tstats = strip.locator('dl.gardener-trajectory-stats');
    await expect(tstats.getByText('Recorded runs', { exact: true })).toBeVisible();
    await expect(tstats.getByText('MRs merged', { exact: true })).toBeVisible();
    await expect(tstats.getByText('Beads closed', { exact: true })).toBeVisible();
  });

  test('every iteration card shows an outcome (not a bare title)', async ({ page }) => {
    const id = fixtureId('gardener');
    await page.goto(`/gardener/${id}`);

    // The life-thread renders one card per closed iteration (3). They are
    // newest-first; each carries the run-number ribbon and an outcome headline.
    const cards = page.locator('ul.gardener-thread li.gardener-iter');
    await expect(cards).toHaveCount(3);

    // CORE assertion of this spec: NO card falls back to the bare-agent-name
    // headline. The template tags an outcome-less card's title link with
    // .gardener-iter-noutcome (ka-0000001187); every fixture run posted a
    // wrap-up tldr, so that class must never appear.
    await expect(page.locator('.bead-row-title.gardener-iter-noutcome')).toHaveCount(0);

    // Each card's headline link is its wrap-up tldr — not the agent name. The
    // newest run (top) is the blocked one; assert its outcome headline and the
    // blocked-exit chip, then the two clean runs below.
    const headlines = cards.locator('a.bead-row-title');
    await expect(headlines.nth(0)).toContainText('Blocked on a flaky deploy pipeline');
    await expect(headlines.nth(1)).toContainText('Merged MR !101');
    await expect(headlines.nth(2)).toContainText('Opened the scaffolding MR');

    // Outcome-classified exit chips: the blocked run renders a blocked-exit
    // chip, the two clean runs render clean-exit chips. Both branches of the
    // exit-chip switch are exercised.
    await expect(page.locator('.gardener-iter-exit-fail')).toHaveCount(1);
    await expect(page.locator('.gardener-iter-exit-ok')).toHaveCount(2);
    await expect(cards.nth(0).locator('.gardener-iter-exit-fail')).toContainText('blocked exit');

    // Run-number ribbon is chain-ordered 1..N (the schedule_prior_agent_id
    // chain is intact, so no "(by time)" heuristic suffix). Newest (#3) on top.
    await expect(cards.nth(0).locator('.gardener-run-label')).toContainText('Iteration 3 of 10');
    await expect(page.locator('.gardener-run-bytime')).toHaveCount(0);
  });

  test('layout stays intact at a 430px narrow viewport', async ({ page }) => {
    const id = fixtureId('gardener');
    // iPhone 14 Pro Max logical width — the widest "phone" the layout must
    // survive. The gardener page opts into the wide-page skin (the hidden
    // .wide-page marker in gardener_detail.html); the narrow-viewport rules
    // must still keep the page within the viewport with no horizontal scroll.
    await page.setViewportSize({ width: 430, height: 932 });
    await page.goto(`/gardener/${id}`);

    // Header + thread still render at this width.
    await expect(page.locator('h1.gardener-title')).toBeVisible();
    await expect(page.locator('section.gardener-trajectory')).toBeVisible();
    await expect(page.locator('ul.gardener-thread li.gardener-iter')).toHaveCount(3);

    // No horizontal overflow: documentElement.scrollWidth must not exceed the
    // viewport (1px tolerance for sub-pixel rounding). This is the catch-all
    // detector for a fixed-width element / unwrapped flex row at 430px.
    const m = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      innerWidth: window.innerWidth,
    }));
    expect(
      m.scrollWidth,
      `gardener page scrollWidth=${m.scrollWidth} > innerWidth=${m.innerWidth} @ 430px`,
    ).toBeLessThanOrEqual(m.innerWidth + 1);

    // The sparkline bars stay within the content box too (a common overflow
    // source for a horizontal bar strip).
    const spark = page.locator('.gardener-spark');
    await expect(spark).toBeVisible();
    const box = await spark.boundingBox();
    expect(box, 'spark has a layout box').not.toBeNull();
    expect(box.x + box.width, 'spark right edge within viewport').toBeLessThanOrEqual(430 + 1);
  });
});
