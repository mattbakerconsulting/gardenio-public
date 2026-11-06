// I2.1 — the RICH create-gardener form (epic ka-0000001611 "Gardener
// Configuration Surface"; task ka-0000007082). The create form no longer spawns
// a one-shot type=agent blob — it AUTHORS a real type=trigger / role:gardener
// LOOP with full knob coverage (preset, schedule/cron, terminal goal, run
// budget, wall-clock, stop spec) and a cron pre-flight on the epic's open
// children. This spec drives that form through a real browser against a live
// daemon and asserts the DAEMON-SIDE fact: a bounded gardener trigger persisted
// with the knobs the form submitted.
//
// WHY THIS SUITE: like gardener_lifecycle_author.spec.js (V3.1), the target is a
// beads-viewer HTTP surface (open the modal, POST the form) that must
// round-trip through type:trigger field validation on a REAL daemon. A Go unit
// test cannot drive the preset-seeding JS or prove the create button opens the
// modal in a browser; this behavioral suite is the right home.
//
// LOAD-BEARING ASSERTIONS (anti-vacuity, advice ka-0000001521):
//   - the form POSTs 200 and the result partial names a real kd-… trigger bead;
//   - the persisted bead is type=trigger with role:gardener + trigger:schedule +
//     gardener-epic:<epic> labels (a gardener LOOP, not a one-shot agent);
//   - its `gardener` blob is BOUNDED: run_budget>0, max_runs>0, a stop spec, and
//     the terminal goal the form defaulted from the chosen preset;
//   - the cron the preset seeded is persisted on the trigger.
// Breaking the trigger authoring (wrong type), the bounded-blob overlay (drop
// run_budget/stop), or the label set turns the matching assertion RED.
//
// AUTH: dev-bypass is on; set X-Auth-Request-Email at suite level so the
// operator-gated create route accepts the POST (mirrors
// gardener_lifecycle_author.spec.js).

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { waitForHtmxIdle } from './_helpers.js';

const OPERATOR_EMAIL = 'e2e-i21-createform@example.test';

test.use({
  extraHTTPHeaders: {
    'X-Auth-Request-Email': OPERATOR_EMAIL,
  },
});

// --- kd shell-out helpers (mirror gardener_lifecycle_author.spec.js) ----------

function kd(args) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  let raw;
  try {
    raw = execFileSync('kd', args, { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (err) {
    const stderr = err.stderr ? err.stderr.toString() : '';
    const firstLine = stderr.split('\n').find((l) => l.trim().length > 0) || err.message;
    throw new Error(`kd ${args[0]} failed (exit ${err.status}): ${firstLine}`);
  }
  return raw;
}

function kdJSON(args) {
  const raw = kd(args);
  const i = raw.search(/[{[]/);
  return JSON.parse(i === -1 ? raw : raw.slice(i));
}

function kdCreateID(args) {
  const bead = kdJSON([...args, '--json']);
  if (!bead.id) throw new Error(`kd create returned no id: ${JSON.stringify(bead)}`);
  return bead.id;
}

// gardenerSpec reads a gardener trigger bead's `gardener` JSON field back off the
// daemon (stored as a string; parse a second time).
function gardenerSpec(gardenerId) {
  const bead = kdJSON(['show', gardenerId, '--json']);
  const raw = bead.fields?.gardener;
  if (!raw) return {};
  return typeof raw === 'string' ? JSON.parse(raw) : raw;
}

const uniqueSuffix = () => `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;

// createEphemeralEpic authors a type=epic in project:plansux (like the other
// ephemeral fixtures, so it never becomes project:gardenio's first gardener) with
// one open child, so the create form's cron pre-flight (open-children>0) is
// meaningful and the epic label seeds the project.
function createEphemeralEpic(suffix) {
  const epicID = kdCreateID([
    'create',
    `[E2E ephemeral] I2.1 create-form epic ${suffix}`,
    '--type', 'epic',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:i21_epic_${suffix}`,
  ]);
  // A child so the epic has open children (the wake pre-flight is non-vacuous).
  const childID = kdCreateID([
    'create',
    `[E2E ephemeral] I2.1 child ${suffix}`,
    '--type', 'task',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:i21_child_${suffix}`,
  ]);
  try {
    kd(['dep', 'add', childID, epicID, '--type', 'parent-child']);
  } catch {
    /* best-effort; the epic label still seeds the form */
  }
  return { epicID, childID };
}

test.describe('/create-gardener — rich create form authors a bounded gardener loop', () => {
  test('open the modal, pick a preset, submit, and a bounded type=trigger gardener persists', async ({
    page,
  }) => {
    const suffix = uniqueSuffix();
    const { epicID, childID } = createEphemeralEpic(suffix);
    let gardenerId = '';

    try {
      // ---- open the epic and click "+ Create gardener" ----
      await page.goto(`/${epicID}`);
      await waitForHtmxIdle(page);

      const createBtn = page.locator(`[hx-get$="/create-gardener?epic=${epicID}"]`);
      await expect(createBtn, 'create-gardener button on epic page').toBeVisible();
      await createBtn.click();
      await waitForHtmxIdle(page);

      const form = page.locator('#quick-spawn-sheet form');
      await expect(form, 'create-gardener modal form revealed').toBeVisible();

      // ---- the always-visible rich knobs are present (I2.1 coverage) ----
      // preset picker, schedule cadence, terminal goal, and cron are rendered
      // at the top level; the budget/bounds knobs (max_runs, wall-clock) live
      // inside a collapsed <details> (progressive disclosure), so they exist in
      // the DOM but are not "visible" until the section is expanded.
      await expect(form.locator('[data-testid="cc-preset"]'), 'preset picker').toBeVisible();
      await expect(form.locator('[data-testid="cc-schedule"]'), 'schedule picker').toBeVisible();
      await expect(form.locator('[data-testid="cc-terminal-goal"]'), 'terminal goal').toBeVisible();
      await expect(form.locator('[data-testid="cc-cron"]'), 'cron input').toBeVisible();
      // max_runs is present in the DOM (inside the collapsed budget section).
      await expect(form.locator('[data-testid="cc-max-runs"]'), 'max runs field exists').toHaveCount(1);

      // The staggered cron default is seeded server-side (non-empty).
      const cronDefault = await form.locator('[data-testid="cc-cron"]').inputValue();
      expect(cronDefault, 'staggered cron seeded by default').not.toBe('');

      // ---- choose the ready-queue-drainer preset; the JS re-seeds the knobs ----
      await form.locator('[data-testid="cc-preset"]').selectOption('ready-queue-drainer');
      // Preset cron is */20; the JS writes it into the cron box.
      await expect(form.locator('[data-testid="cc-cron"]')).toHaveValue('*/20 * * * *');
      // max_runs re-seeds even while collapsed (toHaveValue works on hidden inputs).
      await expect(form.locator('[data-testid="cc-max-runs"]')).toHaveValue('30');
      const goalSeeded = await form.locator('[data-testid="cc-terminal-goal"]').inputValue();
      expect(goalSeeded, 'preset seeds the terminal goal (epic substituted)').toContain(epicID);

      // Expand the budget/bounds section and confirm the knob is now visible +
      // editable — exercises the progressive-disclosure <details>.
      await form.locator('.quick-spawn-details summary', { hasText: 'Budget' }).first().click();
      await expect(form.locator('[data-testid="cc-max-runs"]'), 'max runs visible when expanded').toBeVisible();

      // The prompt is required + pre-filled; keep it.
      const prompt = await form.locator('textarea[name="prompt"]').inputValue();
      expect(prompt.length, 'prompt pre-filled').toBeGreaterThan(0);

      // ---- submit → POST /create-gardener ----
      const postPromise = page.waitForResponse(
        (r) => r.url().includes('/create-gardener') && r.request().method() === 'POST',
      );
      await form.locator('button.quick-spawn-submit').click();
      const postResp = await postPromise;
      expect(postResp.status(), 'POST /create-gardener status').toBe(200);
      await waitForHtmxIdle(page);

      // The result partial names the new gardener trigger bead + links it.
      const link = page.locator('.quick-spawn-success a.quick-spawn-link').first();
      await expect(link, 'success result links the authored gardener').toBeVisible();
      const href = await link.getAttribute('href');
      const m = /\/((gd|ka)-[A-Za-z0-9]+)$/.exec(href || '');
      expect(m, `result link carries a kd-… id (href=${href})`).toBeTruthy();
      gardenerId = m[1];

      // ===================================================================
      // DAEMON-SIDE PROOF: a bounded gardener LOOP persisted.
      // ===================================================================
      const bead = kdJSON(['show', gardenerId, '--json']);
      expect(bead.type, 'authored a type=trigger gardener loop (not a one-shot agent)').toBe('trigger');
      for (const label of ['role:gardener', 'trigger:schedule', `gardener-epic:${epicID}`, 'project:plansux']) {
        expect(bead.labels, `gardener carries label ${label}`).toContain(label);
      }
      expect(String(bead.fields?.cron), 'preset cron persisted on the trigger').toBe('*/20 * * * *');
      expect(String(bead.fields?.role), 'role field').toBe('gardener');
      expect(String(bead.fields?.action), 'action field').toBe('spawn_agent');

      const spec = gardenerSpec(gardenerId);
      expect(spec.assign_bead, 'gardener assign_bead is the epic').toBe(epicID);
      expect(spec.run_budget, 'gardener is bounded: run_budget>0').toBeGreaterThan(0);
      expect(spec.max_runs, 'gardener is bounded: max_runs>0').toBeGreaterThan(0);
      expect(spec.stop, 'gardener carries a stop spec').toBeTruthy();
      expect(spec.terminal_goal, 'gardener carries the preset-seeded goal').toContain(epicID);
    } finally {
      // ---- cleanup ----
      for (const id of [gardenerId, childID, epicID]) {
        if (!id) continue;
        try {
          kd(['close', id]);
        } catch {
          /* best-effort */
        }
      }
    }
  });
});
