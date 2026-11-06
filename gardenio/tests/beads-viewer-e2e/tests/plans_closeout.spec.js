// /plans close-out E2E — the exit ramp (tasks ka-0000004467 + ka-0000005361;
// Matt's ask, Slack thread ka-0000001785: "we need a better way to close out
// plans so we dont have to look at every plan we've every made"; operator
// decision ka-0000001273 chose build-now).
//
// WHAT THIS LAYER ADDS over the Go unit tests (plans_closeout_test.go, which
// cover the projections, the legality table, the lane derivation and the bulk
// handler): the real BROWSER wiring, against a live daemon, for the two things
// unit tests structurally cannot prove —
//
//   1. The daemon's `autoclose-ready` label, written by kbeads' own auto-close
//      cascade, actually reaches a rendered card. The Go tests assert on a fake
//      daemon's labels; only an end-to-end run proves the label the REAL daemon
//      writes is the one the viewer reads.
//   2. The one-tap Mark complete round-trips: the POST fires, the plan is
//      genuinely closed + stamped plan_state=complete ON THE DAEMON, and the
//      card swaps in place.
//
// It also pins the LEGALITY GATE in a browser (a drained draft/ready plan gets
// the badge but NO lever — the daemon admits only building→complete plus any
// edge from unset, so a lever there would post a guaranteed 400).
//
// AUTH + SCOPING + EPHEMERAL-BEAD discipline mirror plan_gardener_control.spec.js
// verbatim: dev-bypass means "operator iff X-Auth-Request-Email"; every test that
// WRITES builds its own throwaway plan so it cannot race a shared fixture under
// fullyParallel. The read-only-viewer gate is pinned by the Go tests (dev-bypass
// cannot inject a viewer role) exactly as that suite documents.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { waitForHtmxIdle } from './_helpers.js';

const OPERATOR_EMAIL = 'e2e-closeout@acme.example';
const PLANS_ALL = '/plans?operated_by=&status=all';

function kd(args) {
  return execFileSync('kd', args, { encoding: 'utf8' });
}

function kdCreateJSON(args) {
  return JSON.parse(kd([...args, '--json'])).id;
}

function uniqueSuffix() {
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 7)}`;
}

// planFields reads a bead's fields off the daemon so a test can prove a lever
// actually mutated persisted state rather than only re-rendering.
function planFields(id) {
  const b = JSON.parse(kd(['show', id, '--json']));
  return { status: b.status, planState: (b.fields || {}).plan_state || '' };
}

// createDrainedPlan builds a plan-labeled epic whose ONE child step is already
// closed — the "drained" shape the viewer derives (StepsAllDone) without needing
// the daemon's cascade to have fired. planState seeds fields.plan_state, which is
// what the legality gate reads. Lives in its own unique project so a test can
// filter the index down to exactly its own rows.
function createDrainedPlan(suffix, planState, project) {
  const epicArgs = [
    'create',
    `[E2E ephemeral] closeout plan ${suffix}`,
    '--type', 'epic',
    '--label', 'plan',
    '--label', `project:${project}`,
    '--label', `fixture:e2e:closeout_plan_${suffix}`,
  ];
  if (planState) epicArgs.push('--field', `plan_state=${planState}`);
  const epicId = kdCreateJSON(epicArgs);

  // One child step, then closed → every rendered step is closed while the epic
  // itself stays open, which is exactly planCloseoutDrained's condition.
  const stepId = kdCreateJSON([
    'create',
    `[E2E ephemeral] closeout step ${suffix}`,
    '--type', 'task',
    '--label', `project:${project}`,
    '--label', `fixture:e2e:closeout_step_${suffix}`,
    '--parent', epicId,
  ]);
  kd(['close', stepId]);
  return { epicId, stepId };
}

// -----------------------------------------------------------------------------
// 1. The badge + the one-tap lever (task ka-0000004467)
// -----------------------------------------------------------------------------
test.describe('plans index — close-out surfacing', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('a drained plan renders the close-out badge and lanes into "Done?"', async ({ page }) => {
    const suffix = uniqueSuffix();
    const project = `closeoutlane-${suffix}`;
    const { epicId, stepId } = createDrainedPlan(suffix, 'building', project);

    // Scope to this test's own project so the board shows only its row.
    await page.goto(`/plans?operated_by=&project=${project}`);

    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card, 'the drained plan renders').toBeVisible();

    // The badge — the honest "this looks finished" signal.
    const badge = card.locator('.plan-closeout-chip');
    await expect(badge, 'a drained plan carries the close-out badge').toBeVisible();

    // And the card sits in the derived "Done?" lane rather than reading as live
    // work in Open / In progress. This is the lane that turns close-out into a
    // routine instead of an archaeology dig through every card.
    const lane = page.locator('.plans-lane-closeout');
    await expect(lane, 'the board grows a Done? lane').toBeVisible();
    await expect(lane.locator('.plans-lane-head')).toContainText('Done?');
    await expect(
      lane.locator(`li.plan-card[data-plan-id="${epicId}"]`),
      'the drained plan is IN the Done? lane',
    ).toBeVisible();

    kd(['close', epicId]);
  });

  test('one-tap Mark complete closes the plan and stamps plan_state=complete on the daemon', async ({ page }) => {
    const suffix = uniqueSuffix();
    const project = `closeouttap-${suffix}`;
    const { epicId } = createDrainedPlan(suffix, 'building', project);

    await page.goto(`/plans?operated_by=&project=${project}`);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    const lever = card.locator('button.plan-closeout-btn');
    await expect(lever, 'a legally-completable drained plan offers the lever').toBeVisible();

    // The lever carries an hx-confirm; auto-accept it so the POST goes out.
    page.on('dialog', (d) => d.accept());

    const postPromise = page.waitForResponse(
      (r) => r.url().includes(`/plans/${epicId}/complete`) && r.request().method() === 'POST',
    );
    await lever.click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /plans/{id}/complete status').toBe(200);
    await waitForHtmxIdle(page);

    // THE ACCEPTANCE: the plan is genuinely retired on the daemon — not merely
    // re-rendered. Both halves matter (the board keys on the close; the lifecycle
    // chip and the Reopen affordance key on plan_state).
    const after = planFields(epicId);
    expect(after.status, 'the plan epic is closed on the daemon').toBe('closed');
    expect(after.planState, 'plan_state is stamped complete').toBe('complete');
  });

  test('a drained plan whose lifecycle cannot reach complete shows the badge but NO lever', async ({ page }) => {
    // plan_state=ready is drained-but-never-approved. The daemon admits only
    // building→complete (plus any edge from unset), so ready→complete is
    // REJECTED — a one-tap lever here would post a guaranteed 400. This is not
    // hypothetical: it is the live shape of ka-0000005352 and gd-Xs3QgeUHCR, and
    // it is what the deferred `abandoned` state (ka-0000001377) exists to fix.
    const suffix = uniqueSuffix();
    const project = `closeoutgate-${suffix}`;
    const { epicId } = createDrainedPlan(suffix, 'ready', project);

    await page.goto(`/plans?operated_by=&project=${project}`);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card).toBeVisible();

    // The badge still renders — the operator must be able to SEE that the plan
    // looks finished, even when it cannot be one-tap retired.
    await expect(
      card.locator('.plan-closeout-chip'),
      'the badge renders even when completion is blocked',
    ).toBeVisible();
    // But no lever, and the tooltip names the blocking lifecycle instead.
    await expect(
      card.locator('button.plan-closeout-btn'),
      'a ready plan cannot reach complete — no lever may render',
    ).toHaveCount(0);
    await expect(card.locator('.plan-closeout-chip')).toHaveAttribute('title', /ready/);

    kd(['close', epicId]);
  });
});

// -----------------------------------------------------------------------------
// 2. Bulk Mark complete (task ka-0000005361)
// -----------------------------------------------------------------------------
test.describe('plans index — bulk Mark complete', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('Select all → Mark complete retires every selected plan in one POST', async ({ page }) => {
    // The headline: "retire 20 plans in one POST instead of 20 clicks". Two
    // ephemeral drained plans sharing a unique project so Select-all over the
    // filtered DOM selects exactly them.
    const s1 = uniqueSuffix();
    const project = `closeoutbulk-${s1}`;
    const a = createDrainedPlan(`${s1}a`, 'building', project);
    const b = createDrainedPlan(`${s1}b`, 'building', project);

    await page.goto(`/plans?operated_by=&project=${project}`);
    await expect(page.locator('li.plan-card')).toHaveCount(2);

    const bar = page.locator('#plans-bulk-bar');
    // NOTE the checkbox value is the PLAN-EPIC id (re-keyed by this task): a
    // gardener-less plan — which both of these are, and which 48 of 56 live open
    // plans are — must still be selectable, or bulk close-out reaches almost
    // nothing.
    const cardA = page.locator(`li.plan-card[data-plan-id="${a.epicId}"]`);
    await expect(cardA.locator('input.plans-row-select')).toHaveAttribute('value', a.epicId);

    await cardA.locator('input.plans-row-select').check();
    await expect(bar).toBeVisible();
    await bar.locator('[data-bulk-select-all]').click();
    await expect(bar.locator('[data-bulk-count]')).toHaveText('2 selected');

    // Mark complete carries htmx's native hx-confirm at EVERY selection size
    // (completing closes plans, so unlike a reversible pause every invocation
    // prompts) — accept it.
    page.on('dialog', (d) => d.accept());

    const postPromise = page.waitForResponse(
      (r) => r.url().includes('/plans/bulk/complete') && r.request().method() === 'POST',
    );
    await bar.locator('form.plans-bulk-complete button[type="submit"]').click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /plans/bulk/complete status').toBe(200);
    await waitForHtmxIdle(page);

    // BOTH plans retired on the daemon in the single POST.
    for (const [name, p] of [['A', a], ['B', b]]) {
      const after = planFields(p.epicId);
      expect(after.status, `plan ${name} closed on the daemon`).toBe('closed');
      expect(after.planState, `plan ${name} stamped complete`).toBe('complete');
    }
  });

  test('a mixed selection retires what it legally can and names what it cannot', async ({ page }) => {
    // The reason the legality gate is enforced in the HANDLER and not only in the
    // card render: the card hides the lever on a draft/ready plan, but "Select
    // all" can absolutely put one in the form. The batch must not abort — it
    // retires the legal plan and reports the blocked one by name.
    const s1 = uniqueSuffix();
    const project = `closeoutmixed-${s1}`;
    const ok = createDrainedPlan(`${s1}ok`, 'building', project);
    const blocked = createDrainedPlan(`${s1}no`, 'ready', project);

    await page.goto(`/plans?operated_by=&project=${project}`);
    await expect(page.locator('li.plan-card')).toHaveCount(2);

    const bar = page.locator('#plans-bulk-bar');
    await page.locator(`li.plan-card[data-plan-id="${ok.epicId}"] input.plans-row-select`).check();
    await expect(bar).toBeVisible();
    await bar.locator('[data-bulk-select-all]').click();
    await expect(bar.locator('[data-bulk-count]')).toHaveText('2 selected');

    page.on('dialog', (d) => d.accept());
    const postPromise = page.waitForResponse(
      (r) => r.url().includes('/plans/bulk/complete') && r.request().method() === 'POST',
    );
    await bar.locator('form.plans-bulk-complete button[type="submit"]').click();
    const resp = await postPromise;
    // Partial success is a 200, not an error — some work committed.
    expect(resp.status(), 'a partially-refused bulk is still 200').toBe(200);
    await waitForHtmxIdle(page);

    // The legal plan retired …
    const okAfter = planFields(ok.epicId);
    expect(okAfter.status, 'the building plan retired').toBe('closed');
    expect(okAfter.planState, 'the building plan stamped complete').toBe('complete');

    // … the blocked plan was left completely alone (NOT closed, plan_state
    // untouched — the daemon would have rejected the state write anyway).
    const blockedAfter = planFields(blocked.epicId);
    expect(blockedAfter.status, 'the ready plan is untouched').not.toBe('closed');
    expect(blockedAfter.planState, 'the ready plan keeps its lifecycle').toBe('ready');

    // … and the toolbar names it, so the operator learns WHY rather than
    // wondering which rows silently did nothing.
    await expect(bar).toContainText(blocked.epicId);

    kd(['close', blocked.epicId]);
  });
});
