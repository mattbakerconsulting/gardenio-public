// V3.1 — author a real opportunistic gardener end-to-end through the UI, then
// confirm it WAKES and STOPS as configured (epic ka-0000001611 "Gardener
// Configuration Surface"; task gd-mV3sU4mkdE). This is the Phase-3 BEHAVIORAL
// verification that complements V3.2's Go golden round-trip test
// (gardener_config_roundtrip_test.go, task gd-6fyrAk0bWH): V3.2 proves the
// config-save handler preserves the full spec at the handler layer against a
// fakeDaemon; THIS spec drives the same surface through a real browser against
// a live daemon, so the htmx wiring + the daemon field-validation path are
// exercised end to end.
//
// WHY A NEW SPEC (suite choice): V3.1's target is a beads-viewer HTTP surface —
// open the operator config form, POST it, then POST the lifecycle levers, and
// assert BOTH the persisted `gardener` JSON on the daemon AND the re-rendered
// DOM. That is exactly what this Playwright suite is for (it already owns the
// /gardener/{id} coverage in gardener_detail.spec.js), and it runs the viewer
// against a REAL daemon so the write actually round-trips through type:trigger
// field validation. A Go unit test cannot drive the htmx config form or prove
// the run-now spawn renders in the life-thread in a browser; a shell module in
// tests/e2e/scripts/ would have to stand up the viewer + daemon itself. So the
// behavioral suite is the right home. No run-shard.sh wiring is needed — this
// file is picked up by the test-beads-viewer-e2e CI task automatically.
//
// WHAT "opportunistic gardener" means here + WHAT IS SHIPPED: an opportunistic
// gardener is a type=trigger bead carrying role:gardener + a `gardener` JSON spec
// (GardenerSpec) governing wake/budget/stop. The shipped operator surface for
// AUTHORING that power is the /gardener/{id} config editor (handleGardenerEditForm
// / handleGardenerConfigSave in gardener_ops.go) — it persists terminal_goal,
// run_budget, max_runs, retry, timeouts, and the raw stop spec. The rich
// CREATE form + the wake-conditions[] builder (epic tasks I2.1 / I2.3) are not
// yet shipped, so this spec authors the trigger bead's shell via `kd` (as every
// mutation spec here seeds its own ephemeral beads) and then does the real
// authoring — goal + budget + STOP SPEC — through the browser config form. The
// "wakes" leg is the run-now lever (spawns one iteration immediately, the same
// engine-mirroring spawn path a scheduled fire takes); the "stops as
// configured" leg is Disable (halts spawning) then Retire (closes the loop).
//
// LOAD-BEARING ASSERTIONS (anti-vacuity, advice ka-0000001521): every leg
// asserts a DAEMON-SIDE fact, not just a 200 / a re-render:
//   - configure → the persisted `gardener` spec on the trigger bead carries the
//     exact goal / run_budget / max_runs / stop blob the form submitted, AND
//     re-opening the form shows them (survives the encode→persist→decode trip).
//   - wake       → a NEW type=agent iteration bead exists stamped schedule_id ==
//     the gardener id (the FK gardenerIterations filters on), it renders as a card
//     in the life-thread, and runs_spawned bumped 0→1 on the spec.
//   - stop       → Disable writes enabled=false on the daemon; Retire closes the
//     trigger bead (status != open) and the control bar reads "retired".
// Breaking the config-overlay (drop a knob), the run-now spawn (no schedule_id),
// or the enable/close writes turns the matching leg RED while the read-only
// gardener_detail.spec.js stays GREEN — the mutation proof for the PROOF block.
//
// AUTH (gd-ZTu68qjfEC): every leg needs a gardenio-admin session. kbeads
// writes run_budget / runs_spawned only for an OIDC gardenio-admin or the
// trigger engine (autonomous_field_authz.go, gd-is1H5NmI05), and run-now
// refuses a roleless session (viewerCanAdmin, gd-uzCT6oXur9). So this suite
// runs against the OIDC-mode viewer with a gardenio-admin bearer from the
// harness issuer (tests/_oidc.js), which the viewer verifies and forwards to
// the daemon as the write credential, the same path a Keycloak admin takes.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { waitForHtmxIdle } from './_helpers.js';
import { oidcSession, requireOIDCStub } from './_oidc.js';

const OPERATOR_EMAIL = 'e2e-v31-gardener@example.test';

// --- kd shell-out helpers (mirror plan_gardener_control.spec.js) --------------

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

// kdJSON parses the first JSON value out of a kd --json invocation, tolerating a
// leading deprecation banner (seed.mjs uses the same slice-from-first-brace).
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

// gardenerSpec reads the gardener bead's `gardener` JSON field back off the daemon
// and returns the decoded GardenerSpec object. The daemon stores it as a JSON
// string under fields.gardener; `kd show --json` surfaces it as a string, so
// parse it a second time. Returns {} when unset.
function gardenerSpec(gardenerId) {
  const bead = kdJSON(['show', gardenerId, '--json']);
  const raw = bead.fields?.gardener;
  if (!raw) return {};
  return typeof raw === 'string' ? JSON.parse(raw) : raw;
}

// gardenerEnabledField reads the `enabled` field, normalized to a string so the
// STRING "true"/"false" a `kd create --field` writes and the JSON BOOL a typed
// toggle writes both compare stably (mirror plan_gardener_control.spec.js).
function gardenerEnabledField(gardenerId) {
  const bead = kdJSON(['show', gardenerId, '--json']);
  const v = bead.fields?.enabled;
  return v === undefined ? undefined : String(v);
}

function gardenerStatus(gardenerId) {
  return kdJSON(['show', gardenerId, '--json']).status;
}

// createOpportunisticGardener authors the trigger bead's SHELL (the part the
// rich create form does not yet expose): a type=trigger + trigger:schedule +
// role:gardener bead in project:plansux (like the other ephemeral fixtures, so
// it never becomes project:gardenio's first gardener). It is deliberately born
// with a MINIMAL spec — no goal, no budget, no stop — because the real
// authoring (goal + budget + stop) happens through the browser config form in
// the test below. enabled=true so it starts running; the stop leg pauses it.
// Returns the gardener bead id.
function createOpportunisticGardener(suffix) {
  return kdCreateID([
    'create',
    `[E2E ephemeral] V3.1 opportunistic gardener ${suffix}`,
    '--type', 'trigger',
    '--label', 'trigger:schedule',
    '--label', 'role:gardener',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:v31_cap_${suffix}`,
    '--field', 'source=schedule',
    '--field', 'action=spawn_agent',
    // project FIELD (not just the label) is what the controller reads to
    // derive BOAT_PROJECT / the OAuth secret / the pod git clone; the daemon
    // rejects a project-less agent-spawning trigger (ka-0000000254).
    '--field', 'project=plansux',
    '--field', 'cron=17 * * * *',
    '--field', 'role=gardener',
    '--field', 'enabled=true',
    '--field', 'timezone=UTC',
    // A minimal gardener spec so isGardenerBead + the config form resolve a
    // (near-empty) GardenerSpec to overlay onto. runs_spawned is left out, as
    // the engine leaves it out of a never-woken gardener: GardenerSpec marshals
    // it omitempty, so an explicit 0 would vanish on the config save and the
    // daemon would read that as a counter change (gd-ZTu68qjfEC).
    '--field', 'gardener={"name":"E2E V3.1 opportunistic gardener"}',
  ]);
}

const uniqueSuffix = () => `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;

// A real stop spec an operator would author: keep looping until the target
// epic's MR merges (loop_end), with a hard wall-clock ceiling (hard_stop). This
// is the exact shape bridge's gardener stop evaluator parses
// (gardener_block_on_mr_test.go), so persisting it proves the form accepts a
// real engine-valid spec, not a toy `{}`.
const STOP_SPEC = {
  loop_end: { any: [{ kind: 'mr_merged' }] },
  hard_stop: { any: [{ kind: 'wall_clock', duration: '168h' }] },
};

test.describe('/gardener/{id} — author + configure + wake + stop an opportunistic gardener', () => {
  requireOIDCStub(test);
  test.use(oidcSession(OPERATOR_EMAIL, ['gardenio-admin']));

  test('configure goal/budget/stop through the UI, then wake (run-now) and stop (disable→retire)', async ({
    page,
  }) => {
    const suffix = uniqueSuffix();
    const gardenerId = createOpportunisticGardener(suffix);

    // ---- sanity: the freshly-authored shell has no goal/budget/stop yet ----
    const before = gardenerSpec(gardenerId);
    expect(before.terminal_goal ?? '', 'shell starts with no goal').toBe('');
    expect(before.run_budget ?? 0, 'shell starts with no budget').toBe(0);
    expect(before.stop ?? null, 'shell starts with no stop spec').toBeFalsy();

    // =====================================================================
    // LEG 1 — CONFIGURE through the browser config form.
    // =====================================================================
    await page.goto(`/gardener/${gardenerId}`);

    // The operator control bar renders (we are authenticated as an operator).
    const controls = page.locator('#gardener-controls');
    await expect(controls, 'operator control bar present').toBeVisible();

    // Open the inline config editor. It lazy-loads into #gardener-config-host.
    await controls.locator('button.gardener-ctl-edit').click();
    await waitForHtmxIdle(page);
    const form = page.locator('form#gardener-config-form');
    await expect(form, 'config editor form revealed').toBeVisible();

    const TERMINAL_GOAL = 'Drive epic gd-demoV31 to a merged MR, then stop.';
    const RUN_BUDGET = '750000';
    const MAX_RUNS = '12';

    await form.locator('textarea[name="terminal_goal"]').fill(TERMINAL_GOAL);
    await form.locator('input[name="run_budget"]').fill(RUN_BUDGET);
    await form.locator('input[name="max_runs"]').fill(MAX_RUNS);

    // Author the real stop spec through the schema-driven GardenerStopSpecBuilder
    // (task ka-0000000897 / I2.3): guided rows per condition kind, not raw JSON.
    // loop_end ← mr_merged (park until MRs merge); hard_stop ← wall_clock 168h.
    const builder = form.locator('fieldset[data-stop-builder]');
    await expect(builder, 'stop builder rendered').toBeVisible();

    // Add the loop_end mr_merged row via the per-slot add menu; the client JS
    // appends a guided row bound to stop_loop_end_0_*.
    await builder
      .locator('.stop-slot[data-slot="loop_end"] select.stop-add-kind')
      .selectOption('mr_merged');
    await expect(
      builder.locator('input[name="stop_loop_end_0_kind"]'),
      'mr_merged loop_end row added',
    ).toHaveValue('mr_merged');

    // Add the hard_stop wall_clock row and fill its duration field.
    await builder
      .locator('.stop-slot[data-slot="hard_stop"] select.stop-add-kind')
      .selectOption('wall_clock');
    await expect(
      builder.locator('input[name="stop_hard_stop_0_kind"]'),
      'wall_clock hard_stop row added',
    ).toHaveValue('wall_clock');
    await builder.locator('input[name="stop_hard_stop_0_duration"]').fill('168h');

    // Save → POST /gardener/{id}/config, swap #gardener-config-host in place.
    const savePromise = page.waitForResponse(
      (r) => r.url().includes(`/gardener/${gardenerId}/config`) && r.request().method() === 'POST',
    );
    await form.locator('button.gardener-ctl-save').click();
    const saveResp = await savePromise;
    expect(saveResp.status(), 'POST /gardener/{id}/config status').toBe(200);
    await waitForHtmxIdle(page);

    // The re-rendered form reports "Saved."
    await expect(page.locator('.gardener-config-saved'), 'saved confirmation shown').toBeVisible();

    // DAEMON-SIDE PROOF: the persisted spec carries exactly the authored knobs.
    const afterCfg = gardenerSpec(gardenerId);
    expect(afterCfg.terminal_goal, 'goal persisted').toBe(TERMINAL_GOAL);
    expect(afterCfg.run_budget, 'run_budget persisted as int').toBe(Number(RUN_BUDGET));
    expect(afterCfg.max_runs, 'max_runs persisted as int').toBe(Number(MAX_RUNS));
    // The stop spec survived the encode→persist→decode trip byte-equivalently.
    expect(afterCfg.stop, 'stop spec persisted structurally').toEqual(STOP_SPEC);
    // The pre-existing engine-owned counter was NOT clobbered by the overlay.
    expect(afterCfg.runs_spawned ?? 0, 'runs_spawned untouched by config save').toBe(0);

    // Re-opening the editor round-trips the authored values back into the form
    // (proves gardenerConfigViewFromBead decodes what the save encoded).
    await page.reload();
    await page.locator('#gardener-controls button.gardener-ctl-edit').click();
    await waitForHtmxIdle(page);
    const reopened = page.locator('form#gardener-config-form');
    await expect(reopened.locator('textarea[name="terminal_goal"]')).toHaveValue(TERMINAL_GOAL);
    await expect(reopened.locator('input[name="run_budget"]')).toHaveValue(RUN_BUDGET);
    await expect(reopened.locator('input[name="max_runs"]')).toHaveValue(MAX_RUNS);
    // The stop spec round-trips back into GUIDED ROWS (proves buildStopSpecBuilderView
    // decodes what parseStopSpecFromForm encoded), not just raw JSON.
    const reBuilder = reopened.locator('fieldset[data-stop-builder]');
    await expect(
      reBuilder.locator('input[name="stop_loop_end_0_kind"]'),
      'mr_merged loop_end row round-trips',
    ).toHaveValue('mr_merged');
    await expect(
      reBuilder.locator('input[name="stop_hard_stop_0_duration"]'),
      'wall_clock 168h duration round-trips',
    ).toHaveValue('168h');
    // Acceptance #2: a human-readable condition summary renders in the builder.
    await expect(
      reBuilder.locator('.stop-builder-summary'),
      'human-readable stop summary shown',
    ).toContainText('168h of wall-clock time');
    // The raw-JSON escape hatch (inside the Advanced <details>) still mirrors the
    // canonical spec for power authors.
    const stopBack = await reBuilder.locator('textarea[name="stop_json"]').inputValue();
    expect(JSON.parse(stopBack), 'stop spec round-trips through the raw mirror').toEqual(STOP_SPEC);

    // =====================================================================
    // LEG 2 — WAKE: run-now spawns one real iteration.
    // =====================================================================
    // Close the editor and re-navigate to a clean page so the control bar is the
    // top-level target (run-now swaps #gardener-controls).
    await page.goto(`/gardener/${gardenerId}`);
    const barBeforeWake = page.locator('#gardener-controls');
    await expect(barBeforeWake).toBeVisible();

    // run-now carries an hx-confirm — auto-accept it, then click.
    page.once('dialog', (d) => d.accept());
    const runNowPromise = page.waitForResponse(
      (r) => r.url().includes(`/gardener/${gardenerId}/run-now`) && r.request().method() === 'POST',
    );
    await barBeforeWake.locator('button.gardener-ctl-runnow').click();
    const runNowResp = await runNowPromise;
    expect(runNowResp.status(), 'POST /gardener/{id}/run-now status').toBe(200);
    await waitForHtmxIdle(page);

    // DAEMON-SIDE PROOF: runs_spawned bumped 0→1 and a LastAgentID is stamped.
    const afterWake = gardenerSpec(gardenerId);
    expect(afterWake.runs_spawned, 'runs_spawned bumped by run-now').toBe(1);
    const iterationId = afterWake.last_agent_id;
    expect(iterationId, 'run-now stamped last_agent_id').toBeTruthy();

    // The spawned iteration is a real type=agent bead stamped schedule_id == the
    // gardener id — the FK gardenerIterations filters the life-thread on.
    const iterBead = kdJSON(['show', iterationId, '--json']);
    expect(iterBead.fields?.schedule_id, 'iteration wired to gardener via schedule_id').toBe(gardenerId);
    expect(iterBead.type, 'iteration is an agent bead').toBe('agent');

    // The iteration renders as a life-thread card on the gardener page.
    await page.goto(`/gardener/${gardenerId}`);
    await expect(
      page.locator('ul.gardener-thread li.gardener-iter'),
      'the woken iteration renders in the life-thread',
    ).toHaveCount(1);

    // =====================================================================
    // LEG 3 — STOP as configured: Disable halts spawning, Retire closes the loop.
    // =====================================================================
    const bar = page.locator('#gardener-controls');
    await expect(bar.locator('button.gardener-ctl-disable'), 'running gardener shows Disable').toBeVisible();

    const disablePromise = page.waitForResponse(
      (r) => r.url().includes(`/gardener/${gardenerId}/disable`) && r.request().method() === 'POST',
    );
    await bar.locator('button.gardener-ctl-disable').click();
    const disableResp = await disablePromise;
    expect(disableResp.status(), 'POST /gardener/{id}/disable status').toBe(200);
    await waitForHtmxIdle(page);

    // DAEMON-SIDE PROOF: enabled flipped to false, and the bar now offers Enable.
    expect(gardenerEnabledField(gardenerId), 'gardener disabled on the daemon').toBe('false');
    await expect(
      page.locator('#gardener-controls button.gardener-ctl-enable'),
      'disabled gardener shows Enable (re-render swapped the lever)',
    ).toBeVisible();

    // Retire → close the trigger bead (terminates the loop). hx-confirm again.
    page.once('dialog', (d) => d.accept());
    const retirePromise = page.waitForResponse(
      (r) => r.url().includes(`/gardener/${gardenerId}/retire`) && r.request().method() === 'POST',
    );
    await page.locator('#gardener-controls button.gardener-ctl-retire').click();
    const retireResp = await retirePromise;
    expect(retireResp.status(), 'POST /gardener/{id}/retire status').toBe(200);
    await waitForHtmxIdle(page);

    // DAEMON-SIDE PROOF: the trigger bead is closed; the bar reads "retired".
    expect(gardenerStatus(gardenerId), 'gardener trigger bead closed by retire').not.toBe('open');
    await expect(
      page.locator('#gardener-controls .gardener-controls-closed'),
      'retired gardener bar shows the closed note',
    ).toBeVisible();

    // ---- cleanup: close the ephemeral iteration + gardener beads ----
    try {
      kd(['close', iterationId]);
    } catch {
      /* best-effort */
    }
    // The gardener is already closed by retire; a redundant close is swallowed by
    // kd (already-closed is not fatal here), but guard anyway.
    try {
      kd(['close', gardenerId]);
    } catch {
      /* best-effort */
    }
  });
});
