// Plan-gardener control-room E2E coverage (step 5, gd-cconyepcBc; epic
// ka-0000004792 — "activate/deactivate plan gardeners from the EXISTING /plans
// index"). The plans index IS the control room: each row shows its bound
// gardener's state chip and (for operators) a per-row enable/disable toggle +
// bulk multi-select bar; the page carries a master switch + global-pause banner.
//
// The feature's HANDLER logic is thoroughly covered by Go unit tests
// (plans_gardener_test.go, plans_index_test.go, plans_master_switch_test.go,
// plans_bulk_test.go, plans_gardener_control_test.go). This suite is the missing
// BEHAVIORAL layer: it drives the real htmx wiring end-to-end in a browser —
// chips render, a click flips the daemon field AND swaps the row/switch in
// place, the bulk bar's select-all → disable round-trips, and the confirm-over-5
// gate actually fires — against a live seeded daemon.
//
// AUTH + SCOPING mirror plans.spec.js verbatim: the viewer runs in dev-bypass
// (BEADS_VIEWER_DEV_AUTH_BYPASS=1), so a request is "operator" iff it carries
// X-Auth-Request-Email. Every navigation uses `/plans?operated_by=` (the empty-
// key "show all" form) so the seeded plans render regardless of viewer identity.
//
// NEGATIVE OPERATOR GATE (read-only viewer sees no levers): NOT exercised here.
// Dev-bypass is a pure passthrough (cmd/beads-viewer/dev_auth.go) with no way to
// inject a gardenio-viewer OIDC role, so every request renders AS an operator.
// That gate is pinned by the Go unit tests instead (plans_gardener_test.go's
// RBAC render test, plans_bulk_test.go's TestPlansIndex_NoBulkBarForViewer,
// plans_master_switch_test.go's TestPlansMasterSwitch_ViewerHidesLever), exactly
// as plans_authoring.spec.js documents for the Approve&build lever.
//
// MUTATIONS USE EPHEMERAL BEADS. The per-row toggle, bulk disable, and master
// switch all WRITE. Under `fullyParallel: true` a mutation on a shared seed
// fixture would race the read tests, so every write test builds its own
// throwaway plan(s) + gardener(s) via `kd` (mirroring plans.spec.js's
// createEphemeralPlan) and asserts only on its own beads. The master switch
// toggles a fleet-wide SINGLETON gate, so its mutation test is serialized and
// cleans the gate up afterward.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

// --- kd shell-out helpers (mirror plans.spec.js) -----------------------------

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

function kdCreateJSON(args) {
  const raw = kd([...args, '--json']);
  const i = raw.search(/[{[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) throw new Error(`kd create returned no id: ${raw}`);
  return bead.id;
}

// createEphemeralPlanWithGardener builds a fresh, isolated plan-labeled epic and
// binds a plan gardener to it via the gardener-epic:<epicId> label (the SAME
// binding planGardener / planGardenersByEpic resolve). `enabled` sets the gardener's
// starting state ("true" → running, "false" → paused). The gardener is a
// type=trigger + trigger:schedule + role:gardener + plan-gardener bead — the shape
// isGardenerBead + planGardenerViewFromBead read. Lives in project:plansux (like
// the plan fixtures) so it never becomes project:gardenio's first plan bead.
// Returns { epicId, gardenerId }.
function createEphemeralPlanWithGardener(suffix, enabled) {
  const epicId = kdCreateJSON([
    'create',
    `[E2E ephemeral] gardener-control plan ${suffix}`,
    '--type', 'epic',
    '--label', 'plan',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plancap_plan_${suffix}`,
  ]);
  const gardenerId = kdCreateJSON([
    'create',
    `[E2E ephemeral] gardener ${suffix}`,
    '--type', 'trigger',
    '--label', 'trigger:schedule',
    '--label', 'role:gardener',
    '--label', 'plan-gardener',
    '--label', 'project:plansux',
    '--label', `gardener-epic:${epicId}`,
    '--label', `fixture:e2e:plancap_cap_${suffix}`,
    '--field', 'source=schedule',
    '--field', 'action=spawn_agent',
    // project FIELD (not just the label) is what the controller reads to
    // derive BOAT_PROJECT / the OAuth secret / the pod git clone; the daemon
    // rejects a project-less agent-spawning trigger (ka-0000000254).
    '--field', 'project=plansux',
    '--field', 'cron=19 * * * *',
    '--field', 'role=gardener',
    '--field', `enabled=${enabled}`,
    '--field', 'timezone=UTC',
  ]);
  return { epicId, gardenerId };
}

// gardenerEnabledField reads the gardener bead's `enabled` field off the daemon so
// a test can prove a toggle actually flipped the persisted state, not just the
// rendered chip. Normalized to a string: `kd create --field enabled=true` stores
// the STRING "true"/"false", but a typed toggle (UpdateBeadFieldsTyped writes a
// JSON bool) surfaces via `kd show --json` as the BOOL false — so coerce both to
// a string ("false"/"true") for a stable assertion. Returns undefined if unset
// (which the loader treats as running for a trigger:schedule bead).
function gardenerEnabledField(gardenerId) {
  const raw = kd(['show', gardenerId, '--json']);
  const bead = JSON.parse(raw.slice(raw.search(/[{[]/)));
  const v = bead.fields?.enabled;
  return v === undefined ? undefined : String(v);
}

// Operator identity for the write tests. Any non-empty email flips dev-bypass
// into operator mode; the value only has to be stable within a run.
const OPERATOR_EMAIL = 'e2e-plancap@example.test';

// "Show all plans" — the empty operated_by key (see plans.spec.js).
const PLANS_ALL = '/plans?operated_by=';

const uniqueSuffix = () => `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;

// -----------------------------------------------------------------------------
// 1. Row chip — a plan with a bound gardener renders its gardener-state chip on
//    the /plans index. Uses the read-only seed fixture (plan_ready_gardener,
//    enabled=false → the paused chip), so it never races the mutation tests.
// -----------------------------------------------------------------------------
test.describe('plans index — gardener state chip', () => {
  test('a plan row with a bound (paused) gardener renders the paused state chip', async ({ page }) => {
    const planId = fixtureId('plan_ready_epic');
    await page.goto(PLANS_ALL);

    const card = page.locator(`li.plan-card[data-plan-id="${planId}"]`);
    await expect(card, 'seeded ready plan card present on /plans').toBeVisible();

    // plan_ready_gardener is enabled=false → planGardenerLoopState=paused →
    // planAgentChip renders the paused-tone chip. This is the SAME plan_agent_chip
    // the cockpit renders, so the index and detail can never disagree.
    const chip = card.locator('.plan-agent-chip');
    await expect(chip, 'plan row carries a gardener state chip').toBeVisible();
    await expect(chip).toHaveClass(/plan-agent-chip-paused/);
  });
});

// -----------------------------------------------------------------------------
// 2. Per-row toggle — an operator flips a gardener from the /plans row; the
//    trigger bead's enabled field flips AND the whole row re-renders (the
//    unified state chip updates live).
// -----------------------------------------------------------------------------
test.describe('plans index — per-row gardener toggle', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('flipping a running gardener to paused writes enabled=false and re-renders the row chip', async ({ page }) => {
    const suffix = uniqueSuffix();
    // Start running (enabled=true) so the toggle is a real pause flip. A running
    // gardener with no live iteration renders the calm "idle" chip on the index.
    const { epicId, gardenerId } = createEphemeralPlanWithGardener(suffix, 'true');

    await page.goto(PLANS_ALL);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card).toBeVisible();

    // Pre-flip: the per-row pause/resume lever is present for an operator and
    // reads "running" (a running gardener, click to pause).
    const toggleBtn = card.locator('form.cap-pause button.cap-pause-btn');
    await expect(toggleBtn, 'operator sees the per-row gardener toggle').toBeVisible();
    await expect(toggleBtn).toHaveClass(/cap-pause-running/);

    // The flip POSTs to the plans-scoped row handler and swaps the whole card.
    const postPromise = page.waitForResponse(
      (r) =>
        r.url().includes(`/plans/${epicId}/gardener/pause-toggle`) &&
        r.request().method() === 'POST',
    );
    await toggleBtn.click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /plans/{id}/gardener/pause-toggle status').toBe(200);
    await waitForHtmxIdle(page);

    // The persisted daemon field flipped to false (the toggle writes a JSON
    // bool; gardenerEnabledField coerces it to the string "false").
    expect(gardenerEnabledField(gardenerId), 'gardener enabled field on the daemon').toBe('false');

    // The re-rendered row reads paused: the switch skin flips AND the unified
    // state chip flips (the whole-row swap is what makes the chip update live).
    const cardAfter = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(cardAfter.locator('form.cap-pause button.cap-pause-btn')).toHaveClass(/cap-pause-paused/);
    await expect(cardAfter.locator('.plan-agent-chip')).toHaveClass(/plan-agent-chip-paused/);

    kd(['close', gardenerId]);
    kd(['close', epicId]);
  });
});

// -----------------------------------------------------------------------------
// 3. Master switch — an operator pauses ALL plan gardeners from the page-level
//    switch; the gardeners-pause gate opens and the global-pause banner appears.
//    Resume closes the gate and the banner disappears.
//
//    SERIALIZED: the master switch operates a fleet-wide SINGLETON gate
//    (gardeners-pause), so two workers toggling it at once would clobber each
//    other. Run these serially and clean the gate up afterward so a failed
//    resume never leaves the fleet paused for the rest of the run.
// -----------------------------------------------------------------------------
test.describe.serial('plans index — master switch + global-pause banner', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  // Belt-and-braces: ensure no gardeners-pause gate lingers before/after these
  // tests, regardless of assertion outcome, so we never strand the fleet paused.
  function closeAnyGardenersPauseGate() {
    let raw;
    try {
      raw = kd(['list', '--type', 'gate', '--status', 'open', '--all-types', '--all-projects', '--json']);
    } catch {
      return; // best-effort cleanup
    }
    const arr = JSON.parse(raw.slice(raw.search(/[{[]/)));
    for (const g of Array.isArray(arr) ? arr : []) {
      if (g.title === 'gardeners-pause') {
        try {
          kd(['close', g.id]);
        } catch {
          /* best-effort */
        }
      }
    }
  }

  test.beforeAll(() => closeAnyGardenersPauseGate());
  test.afterAll(() => closeAnyGardenersPauseGate());

  test('the master switch renders in the running state with no banner', async ({ page }) => {
    await page.goto(PLANS_ALL);
    const master = page.locator('#plans-master-switch');
    await expect(master, 'master switch region present at the top of /plans').toBeVisible();
    // With no gate engaged, the switch reads "all running" and NO banner shows.
    await expect(master.locator('.cap-pause-label')).toHaveText('all running');
    await expect(page.locator('.plans-master-banner')).toHaveCount(0);
  });

  test('pausing all gardeners opens the gate + shows the banner; resuming closes it', async ({ page }) => {
    await page.goto(PLANS_ALL);
    const master = page.locator('#plans-master-switch');
    await expect(master).toBeVisible();

    const pauseBtn = master.locator('form.plans-master-form button.plans-master-btn');
    await expect(pauseBtn, 'operator sees the master switch lever').toBeVisible();
    await expect(pauseBtn).toHaveClass(/cap-pause-running/);

    // PAUSE: the toggle POSTs and swaps #plans-master-switch in place.
    const pausePromise = page.waitForResponse(
      (r) => r.url().includes('/plans/master-switch/toggle') && r.request().method() === 'POST',
    );
    await pauseBtn.click();
    const pauseResp = await pausePromise;
    expect(pauseResp.status(), 'POST /plans/master-switch/toggle status').toBe(200);
    await waitForHtmxIdle(page);

    // The re-rendered switch reads "all paused" and the global-pause banner is
    // now present, naming the gardeners-pause storm-guard and warning it
    // supersedes the per-row toggles.
    const masterPaused = page.locator('#plans-master-switch');
    await expect(masterPaused.locator('.cap-pause-label')).toHaveText('all paused');
    const banner = masterPaused.locator('.plans-master-banner');
    await expect(banner, 'global-pause banner appears when gardeners-pause is engaged').toBeVisible();
    await expect(banner).toContainText('All plan gardeners are paused');
    await expect(banner).toContainText(/supersede/i);

    // The gate really opened on the daemon (not just the render).
    const openGates = kd(['list', '--type', 'gate', '--status', 'open', '--all-types', '--all-projects', '--json']);
    const gates = JSON.parse(openGates.slice(openGates.search(/[{[]/)));
    expect(
      gates.some((g) => g.title === 'gardeners-pause'),
      'a gardeners-pause gate bead is open on the daemon',
    ).toBe(true);

    // RESUME: flip it back. The button now reads "paused" (click to resume).
    const resumeBtn = masterPaused.locator('form.plans-master-form button.plans-master-btn');
    await expect(resumeBtn).toHaveClass(/cap-pause-paused/);
    const resumePromise = page.waitForResponse(
      (r) => r.url().includes('/plans/master-switch/toggle') && r.request().method() === 'POST',
    );
    await resumeBtn.click();
    const resumeResp = await resumePromise;
    expect(resumeResp.status(), 'resume POST status').toBe(200);
    await waitForHtmxIdle(page);

    // Back to running, banner gone, gate closed on the daemon.
    const masterResumed = page.locator('#plans-master-switch');
    await expect(masterResumed.locator('.cap-pause-label')).toHaveText('all running');
    await expect(masterResumed.locator('.plans-master-banner')).toHaveCount(0);

    const afterGates = kd(['list', '--type', 'gate', '--status', 'open', '--all-types', '--all-projects', '--json']);
    const gates2 = JSON.parse(afterGates.slice(afterGates.search(/[{[]/)));
    expect(
      gates2.some((g) => g.title === 'gardeners-pause'),
      'the gardeners-pause gate is closed again after resume',
    ).toBe(false);
  });
});

// -----------------------------------------------------------------------------
// 4. Bulk multi-select — filter (implicit: only the ephemeral plans this test
//    creates carry its unique project), Select all, Disable, and assert every
//    selected gardener flips enabled=false with its row chip OOB-updated.
// -----------------------------------------------------------------------------
test.describe('plans index — bulk disable', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('the bulk bar + row checkboxes render for an operator, wired to the plans-scoped endpoints', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { epicId, gardenerId } = createEphemeralPlanWithGardener(suffix, 'true');

    await page.goto(PLANS_ALL);
    // The page-level bulk bar (starts hidden until a row is checked).
    const bar = page.locator('#plans-bulk-bar[data-bulk-scope="plans"]');
    await expect(bar, 'operator sees the bulk bar container').toHaveCount(1);
    await expect(bar.locator('form.plans-bulk-disable')).toHaveAttribute('hx-post', /\/plans\/bulk\/disable$/);
    await expect(bar.locator('form.plans-bulk-enable')).toHaveAttribute('hx-post', /\/plans\/bulk\/enable$/);
    // Confirm-over-5 gate declared on the forms.
    await expect(bar.locator('form.plans-bulk-disable')).toHaveAttribute('data-bulk-confirm-over', '5');
    await expect(bar.locator('form.plans-bulk-disable')).toHaveAttribute('data-bulk-confirm-noun', 'plan gardeners');

    // The close-out verb shares this same selection (task ka-0000005361).
    await expect(bar.locator('form.plans-bulk-complete')).toHaveAttribute('hx-post', /\/plans\/bulk\/complete$/);

    // The row carries a selection checkbox whose value is the PLAN-EPIC id.
    // RE-KEYED from the bound gardener id by the close-out work (ka-0000005361):
    // the old key meant the checkbox only rendered when a non-retired gardener was
    // bound, and 48 of 56 live open plans have none — so the selection model was
    // invisible on 86% of the board, and "Mark complete" is a verb on the PLAN
    // anyway. The gardener verbs now resolve epic -> gardener server-side.
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    const checkbox = card.locator('input.plans-row-select');
    await expect(checkbox, 'row carries a bulk checkbox').toBeVisible();
    await expect(checkbox).toHaveAttribute('value', epicId);
    // And explicitly NOT the gardener id — that key is what hid the selection model.
    await expect(checkbox).not.toHaveAttribute('value', gardenerId);

    kd(['close', gardenerId]);
    kd(['close', epicId]);
  });

  test('Select all → Disable flips every selected gardener and OOB-updates each row chip', async ({ page }) => {
    // Two ephemeral running gardeners — "Select all" over the filtered DOM must
    // flip BOTH in one click (the "disable all of Matt's monorepo gardeners"
    // acceptance), and each row's chip must OOB-swap to paused.
    const s1 = uniqueSuffix();
    const s2 = uniqueSuffix();
    const a = createEphemeralPlanWithGardener(s1, 'true');
    const b = createEphemeralPlanWithGardener(s2, 'true');

    // Scope the list to just these two ephemeral plans by project so "Select all"
    // over the filtered DOM selects exactly them (project:plansux would also pull
    // in the shared plan fixtures). Both ephemeral plans share the same unique
    // project so the filtered index shows only this test's rows.
    const proj = `plancapbulk-${s1}`;
    kd(['label', 'add', a.epicId, `project:${proj}`]);
    kd(['label', 'add', b.epicId, `project:${proj}`]);

    await page.goto(`/plans?operated_by=&project=${proj}`);

    const cardA = page.locator(`li.plan-card[data-plan-id="${a.epicId}"]`);
    const cardB = page.locator(`li.plan-card[data-plan-id="${b.epicId}"]`);
    await expect(cardA).toBeVisible();
    await expect(cardB).toBeVisible();
    // Only this test's two rows are in the filtered view.
    await expect(page.locator('li.plan-card')).toHaveCount(2);

    // The bulk bar (which hosts Select all) starts hidden and only reveals once
    // at least one row checkbox is checked (project_bulk.js). Check one row to
    // reveal it, then Select all to check the rest — both boxes end checked and
    // the count reads 2.
    const bar = page.locator('#plans-bulk-bar');
    await cardA.locator('input.plans-row-select').check();
    await expect(bar).toBeVisible();
    await bar.locator('[data-bulk-select-all]').click();
    await expect(bar.locator('[data-bulk-count]')).toHaveText('2 selected');
    await expect(cardA.locator('input.plans-row-select')).toBeChecked();
    await expect(cardB.locator('input.plans-row-select')).toBeChecked();

    // Disable → POST /plans/bulk/disable. 2 selected ≤ 5, so no confirm fires.
    const postPromise = page.waitForResponse(
      (r) => r.url().includes('/plans/bulk/disable') && r.request().method() === 'POST',
    );
    await bar.locator('form.plans-bulk-disable button[type="submit"]').click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /plans/bulk/disable status').toBe(200);
    await waitForHtmxIdle(page);

    // Both gardeners flipped on the daemon.
    expect(gardenerEnabledField(a.gardenerId), 'gardener A enabled after bulk disable').toBe('false');
    expect(gardenerEnabledField(b.gardenerId), 'gardener B enabled after bulk disable').toBe('false');

    // Each row's chip OOB-swapped to paused (the whole point of the OOB rows).
    await expect(
      page.locator(`li.plan-card[data-plan-id="${a.epicId}"] .plan-agent-chip`),
    ).toHaveClass(/plan-agent-chip-paused/);
    await expect(
      page.locator(`li.plan-card[data-plan-id="${b.epicId}"] .plan-agent-chip`),
    ).toHaveClass(/plan-agent-chip-paused/);

    kd(['close', a.gardenerId]);
    kd(['close', a.epicId]);
    kd(['close', b.gardenerId]);
    kd(['close', b.epicId]);
  });

  test('a >5 selection fires the confirm-over-5 gate; dismissing it issues no request', async ({ page }) => {
    // Six ephemeral running gardeners so a Select-all → Disable exceeds the
    // confirm-over-5 threshold and project_bulk.js's htmx:confirm gate fires.
    const proj = `plancapconfirm-${uniqueSuffix()}`;
    const made = [];
    for (let i = 0; i < 6; i++) {
      const c = createEphemeralPlanWithGardener(`${proj}-${i}`, 'true');
      kd(['label', 'add', c.epicId, `project:${proj}`]);
      made.push(c);
    }

    await page.goto(`/plans?operated_by=&project=${proj}`);
    await expect(page.locator('li.plan-card')).toHaveCount(6);

    // Check one row to reveal the (initially hidden) bulk bar, then Select all
    // so all 6 are selected — over the confirm-over-5 threshold.
    const bar = page.locator('#plans-bulk-bar');
    await page.locator('li.plan-card input.plans-row-select').first().check();
    await expect(bar).toBeVisible();
    await bar.locator('[data-bulk-select-all]').click();
    await expect(bar.locator('[data-bulk-count]')).toHaveText('6 selected');

    // DISMISS the confirm dialog → the request must NOT go out, so no gardener
    // flips. Register a one-shot dialog handler that dismisses and records it.
    let confirmMsg = null;
    page.once('dialog', (d) => {
      confirmMsg = d.message();
      d.dismiss();
    });
    // Race a short window for a would-be POST against the dialog: if the gate
    // works, no /plans/bulk/disable request is issued.
    let postFired = false;
    page.on('request', (req) => {
      if (req.url().includes('/plans/bulk/disable') && req.method() === 'POST') postFired = true;
    });
    await bar.locator('form.plans-bulk-disable button[type="submit"]').click();
    // Give htmx:confirm time to raise the dialog + the dismissal to settle.
    await page.waitForTimeout(500);

    // The confirm gate fired with the plan-gardeners noun + the count.
    expect(confirmMsg, 'confirm-over-5 dialog fired').toBeTruthy();
    expect(confirmMsg).toMatch(/6 plan gardeners/i);
    // Dismissing it issued no request; the gardeners are untouched.
    expect(postFired, 'a dismissed confirm must not POST').toBe(false);
    expect(gardenerEnabledField(made[0].gardenerId), 'gardener unchanged after dismiss').toBe('true');

    // ACCEPTING the confirm lets the request through — the gate is a gate, not a
    // hard block. Re-select (the dismissed click left the boxes checked) and
    // accept this time; every one of the 6 gardeners flips to paused.
    page.once('dialog', (d) => d.accept());
    const postPromise = page.waitForResponse(
      (r) => r.url().includes('/plans/bulk/disable') && r.request().method() === 'POST',
    );
    await bar.locator('form.plans-bulk-disable button[type="submit"]').click();
    const resp = await postPromise;
    expect(resp.status(), 'accepted confirm issues the POST').toBe(200);
    await waitForHtmxIdle(page);
    for (const c of made) {
      expect(gardenerEnabledField(c.gardenerId), `gardener ${c.gardenerId} disabled after accept`).toBe('false');
    }

    for (const c of made) {
      kd(['close', c.gardenerId]);
      kd(['close', c.epicId]);
    }
  });
});
