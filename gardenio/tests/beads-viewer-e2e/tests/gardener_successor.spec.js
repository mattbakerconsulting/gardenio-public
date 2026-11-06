// Card-graph AUTHORING round-trip coverage (task ka-0000005273 / "C3";
// backfill task ka-0000006323). This is the browser-level end-to-end for
// "draw a successor off a gardener card" — the C3 authoring surface whose
// handlers live in gardenio/controller/cmd/beads-viewer/gardener_successor.go
// (handleGardenerSuccessorForm :86, handleGardenerSuccessor :106, parseChainSpec
// :195) with the /maps rail render in maps_chains.go / maps_workflow.go.
//
// WHY A NEW SPEC (suite choice): the C3 target is a beads-viewer HTTP surface —
// POST an authoring form, assert the persisted `chain` field AND the re-rendered
// card graph. That is exactly what this Playwright suite is for (it already owns
// the /maps + /gardener coverage), and it runs the viewer against a REAL daemon,
// so the write actually round-trips through field validation. A shell module in
// tests/e2e/scripts/ would have to stand up the viewer + daemon itself and could
// not drive the htmx authoring dialog, so it is the wrong home here. No
// run-shard.sh wiring is needed — this file is picked up by the
// test-beads-viewer-e2e CI task automatically.
//
// WHAT THE LOAD-BEARING ASSERTION IS (anti-vacuity, advice ka-0000001521): a
// "page 200s" check is born vacuous. The assertion that proves the gap was real
// is the ROUND-TRIP — after drawing a successor with phase "impl-<uniq>", the
// /maps card-graph must render a PENDING successor node carrying that exact
// label INSIDE the predecessor gardener's own workflow rail (#wf-<predId>). That
// ties the drawn edge to the RIGHT predecessor (rail id) AND proves the heir
// content survived the chain.to serialize → persist → parseChainSpec → planned-
// edge → render pipeline. Breaking parseChainSpec (return nil) or the planned-
// edge emit (plannedHeirsFromBead / plannedChainEdges) turns the round-trip RED
// while the pre-existing maps.spec.js / gardeners_index.spec.js stay GREEN — the
// mutation proof recorded in the MR PROOF block.
//
// A SECOND, DEEPER GAP this spec guards (found while building it): the handler
// persists the drawn heir via UpdateBeadFieldsTyped({"chain": ...}) on the
// gardener's type:trigger bead. The `chain` field was NOT declared on the
// type:trigger schema (kbeads builtin config, prod's store config, or the
// controller init.go), and the type has no allow_additional_fields — so the
// write 400'd "chain: unknown field" against EVERY real daemon (prod and the
// stock hermetic daemon THIS CI task boots). The C3 Go unit tests
// (gardener_successor_test.go) never caught it because their fakeDaemon does no
// field validation. This spec exercises the real write path, so it is RED
// without the accompanying one-line kbeads fix (register chain:json on
// type:trigger, guarded by server.TestBuiltinGardenerChainFieldDeclared).
//
// Auth: dev-bypass is on (BEADS_VIEWER_DEV_AUTH_BYPASS=1), so
// oidcauth.EmailFromRequest returns "" unless X-Auth-Request-Email is set — and
// the authoring routes are operator-gated (viewerCanOperateAuthed / email==""
// → 401). Set the header at the suite level so the form renders and the POST is
// accepted, exactly as ForwardAuth would inject it in prod.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { waitForHtmxIdle } from './_helpers.js';

const AUTH_EMAIL = 'e2e-c3@example.test';

test.use({
  extraHTTPHeaders: {
    'X-Auth-Request-Email': AUTH_EMAIL,
  },
});

// kd shells out to the `kd` CLI against the same daemon the viewer talks to
// (mirrors decision_resolve.spec.js / plans_authoring.spec.js). process.env is
// carried through so BEADS_AUTH_TOKEN (the primary static token the CI daemon
// runs with) authenticates the create/show writes.
function kd(args) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  let raw;
  try {
    raw = execFileSync('kd', args, { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (err) {
    const stderr = err.stderr ? err.stderr.toString() : '';
    const firstLine = stderr.split('\n').find((l) => l.trim().length > 0) || err.message;
    throw new Error(`kd ${args.join(' ')} failed (exit ${err.status}): ${firstLine}`);
  }
  return raw;
}

function kdJSON(args) {
  const raw = kd([...args, '--json']);
  // Some kd subcommands print a deprecation banner before the JSON payload;
  // strip everything before the first `{` or `[` (matches seed.mjs).
  const i = raw.search(/[{[]/);
  return JSON.parse(i === -1 ? raw : raw.slice(i));
}

// createGardener makes a fresh, unique type=trigger gardener bead per test so the
// additive draw-successor POST never bleeds between tests or across the parallel
// workers / prior runs. Carries the minimal required trigger schema fields
// (source/action/cron/enabled) plus role:gardener so it renders as a gardener card
// on /maps with the "+ successor" affordance. Returns the daemon-assigned id.
function createGardener(suffix) {
  const bead = kdJSON([
    'create',
    `[E2E C3 ephemeral] gardener ${suffix}`,
    '--type',
    'trigger',
    '--label',
    'trigger:schedule',
    '--label',
    'role:gardener',
    '--label',
    'project:gardenio',
    '--label',
    `fixture:e2e:c3_${suffix}`,
    '--field',
    'source=schedule',
    '--field',
    'action=spawn_agent',
    '--field',
    'cron=0 9 * * *',
    '--field',
    'project=gardenio',
    '--field',
    'role=gardener',
    '--field',
    'enabled=true',
    '--field',
    'prompt=C3 e2e ephemeral gardener',
  ]);
  if (!bead.id) {
    throw new Error(`kd create returned no id: ${JSON.stringify(bead)}`);
  }
  return bead.id;
}

// chainField reads back the gardener bead's persisted `chain` field (the
// serialized bridge.ChainSpec the handler wrote). Returns undefined when the
// field is absent — the reject path asserts on that.
function chainField(id) {
  const bead = kdJSON(['show', id]);
  return bead.fields ? bead.fields.chain : undefined;
}

// Open the authoring sheet for a given gardener by clicking its card's
// "+ successor" affordance and waiting for the htmx swap to land. Returns the
// #quick-spawn-sheet locator (the swap target).
async function openSuccessorSheet(page, predId) {
  await page.goto('/maps?all=1');
  // Scope to THIS gardener via the button's hx-get target so the locator is
  // unambiguous no matter how many other gardener cards the board renders.
  const succBtn = page.locator(
    `button.maps-card-successor[hx-get$="/gardener/${predId}/successor"]`,
  );
  await expect(succBtn, 'the gardener card must expose a "+ successor" affordance').toBeVisible();
  await succBtn.click();
  await waitForHtmxIdle(page);
  const sheet = page.locator('#quick-spawn-sheet');
  await expect(sheet).toBeVisible();
  await expect(sheet.locator('h2', { hasText: 'Draw successor gardener' })).toBeVisible();
  return sheet;
}

// Submit the current authoring form and assert the POST returned 200. Returns
// after htmx settles so the swapped-in result is asserted on the caller side.
async function submitDraw(page, sheet, predId) {
  const postPromise = page.waitForResponse(
    (r) => r.url().includes(`/gardener/${predId}/successor`) && r.request().method() === 'POST',
  );
  await sheet.locator('button[type="submit"]', { hasText: 'Draw successor' }).click();
  const resp = await postPromise;
  expect(resp.status(), 'draw-successor POST status').toBe(200);
  await waitForHtmxIdle(page);
}

test.describe('C3 card-graph authoring — successor round-trip', () => {
  test('drawing a successor persists chain.to and renders a pending node on the predecessor rail', async ({
    page,
  }) => {
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const predId = createGardener(suffix);
    // A unique, short phase label — this is the string that must survive the
    // whole round-trip and appear on the rendered pending node.
    const phase = `impl-${suffix.slice(-6)}`;

    const sheet = await openSuccessorSheet(page, predId);
    await sheet.locator('input[name="phase"]').fill(phase);
    await sheet.locator('textarea[name="terminal_goal"]').fill('All children merged & green.');
    await submitDraw(page, sheet, predId);

    // The form swapped to its saved-confirmation state — no error banner.
    await expect(sheet.locator('.quick-spawn-error')).toHaveCount(0);
    await expect(sheet.locator('.quick-spawn-success')).toBeVisible();

    // (a) chain.to PERSISTS on the predecessor bead. This is the write half —
    // the field the handler serializes through UpdateBeadFieldsTyped. Absent the
    // kbeads `chain` field registration, this write 400s and the field never
    // appears (the deeper gap this spec guards).
    const chain = chainField(predId);
    expect(chain, 'chain field persisted on the gardener bead').toBeTruthy();
    const parsed = JSON.parse(chain);
    // Single heir → `to` is a bare object (byte-compatible with C1's shape).
    expect(Array.isArray(parsed.to), 'single heir → chain.to is a bare object').toBe(false);
    expect(parsed.to.phase).toBe(phase);

    // (b) ROUND-TRIP — the load-bearing assertion. Reload /maps and assert the
    // drawn heir renders as a PENDING successor node carrying the typed phase
    // label, INSIDE this predecessor's own workflow rail (#wf-<predId>). The
    // rail id ties the edge to the RIGHT predecessor; the label proves the heir
    // content survived parseChainSpec → planned-edge → render.
    await page.goto('/maps?all=1');
    const rail = page.locator(`#wf-${predId}`);
    await expect(rail, 'predecessor gains a workflow rail once it declares a chain.to').toBeVisible();
    const pendingLabel = rail.locator('.maps-rail-pending .maps-rail-pending-label', {
      hasText: phase,
    });
    await expect(pendingLabel, 'the pending successor node carries the drawn phase').toHaveCount(1);
    // The board also flags the workflow as carrying a pending (not dangling)
    // successor — the C3 authoring accent distinct from a broken chain.
    await expect(rail.locator('.maps-workflow-pending')).toBeVisible();
  });

  test('a second draw fans out — chain.to becomes an array and both pending nodes render', async ({
    page,
  }) => {
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const predId = createGardener(suffix);
    const phaseA = `impl-${suffix.slice(-6)}`;
    const phaseB = `vfy-${suffix.slice(-6)}`;

    // Draw the first heir.
    let sheet = await openSuccessorSheet(page, predId);
    await sheet.locator('input[name="phase"]').fill(phaseA);
    await submitDraw(page, sheet, predId);
    await expect(sheet.locator('.quick-spawn-success')).toBeVisible();

    // The success state offers "Add another (fan out)" — click it to reopen the
    // form (htmx GET back into #quick-spawn-sheet) and draw a SECOND heir.
    await sheet.locator('button', { hasText: 'Add another' }).click();
    await waitForHtmxIdle(page);
    sheet = page.locator('#quick-spawn-sheet');
    await expect(sheet.locator('h2', { hasText: 'Draw successor gardener' })).toBeVisible();
    // The reopened form notes the gardener already declares an heir (fan-out).
    await expect(sheet.locator('.quick-spawn-multi-note')).toContainText('fans out');
    await sheet.locator('input[name="phase"]').fill(phaseB);
    await submitDraw(page, sheet, predId);
    await expect(sheet.locator('.quick-spawn-success')).toBeVisible();

    // Persistence: chain.to is now an ARRAY of two heirs (the fan-out shape).
    const chain = chainField(predId);
    expect(chain, 'chain field persisted').toBeTruthy();
    const parsed = JSON.parse(chain);
    expect(Array.isArray(parsed.to), 'fan-out → chain.to is an array').toBe(true);
    expect(parsed.to.map((h) => h.phase).sort()).toEqual([phaseA, phaseB].sort());

    // ROUND-TRIP: the predecessor's rail carries TWO pending nodes, both labelled
    // and both flagged as fan-out.
    await page.goto('/maps?all=1');
    const rail = page.locator(`#wf-${predId}`);
    await expect(rail).toBeVisible();
    await expect(rail.locator('.maps-rail-pending')).toHaveCount(2);
    await expect(
      rail.locator('.maps-rail-pending .maps-rail-pending-label', { hasText: phaseA }),
    ).toHaveCount(1);
    await expect(
      rail.locator('.maps-rail-pending .maps-rail-pending-label', { hasText: phaseB }),
    ).toHaveCount(1);
    await expect(rail.locator('.maps-rail-pending .meta', { hasText: 'fan-out' }).first()).toBeVisible();
  });

  test('join / branch fields persist on the drawn heir', async ({ page }) => {
    // The fan-in (join) and conditional-branch shapes the design asks for are
    // authored via the advanced fields; assert they round-trip onto chain.to.
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const predId = createGardener(suffix);
    const umbrella = `gd-umb${suffix.slice(-6)}`;
    const decision = `gd-dec${suffix.slice(-6)}`;

    const sheet = await openSuccessorSheet(page, predId);
    await sheet.locator('input[name="phase"]').fill(`ship-${suffix.slice(-6)}`);
    // The join/branch inputs live behind a <details> — open it, then fill.
    await sheet.locator('details.quick-spawn-advanced summary').click();
    await sheet.locator('input[name="join_parent"]').fill(umbrella);
    await sheet.locator('input[name="branch_decision_key"]').fill(decision);
    await submitDraw(page, sheet, predId);
    await expect(sheet.locator('.quick-spawn-success')).toBeVisible();

    const parsed = JSON.parse(chainField(predId));
    expect(parsed.to.join_parent, 'join_parent persists (fan-in)').toBe(umbrella);
    expect(parsed.to.branch_decision_key, 'branch_decision_key persists (conditional branch)').toBe(
      decision,
    );
  });

  test('a malformed heir (nothing identity-bearing) is rejected inline with no write', async ({
    page,
  }) => {
    // The handler drops an heir that names nothing identity-bearing (no phase,
    // goal, or prompt-advice) — else bridge.hasHeir would silently no-op the
    // write. This is the reject path for a malformed/unresolvable target: fill
    // ONLY the optional epic, leave the identity fields blank.
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const predId = createGardener(suffix);

    const sheet = await openSuccessorSheet(page, predId);
    await sheet.locator('input[name="epic"]').fill(`gd-epic${suffix.slice(-6)}`);
    await submitDraw(page, sheet, predId);

    // Inline validation error, form retained (rendered at 200 so htmx swaps it).
    await expect(sheet.locator('.quick-spawn-error')).toBeVisible();
    await expect(sheet.locator('.quick-spawn-error')).toContainText('at least a phase');
    await expect(sheet.locator('.quick-spawn-success')).toHaveCount(0);

    // No chain persisted — the malformed draw was NOT written.
    expect(chainField(predId), 'rejected draw must not persist a chain').toBeFalsy();

    // And the board shows no workflow rail / pending node for this gardener: it
    // stays a plain standalone card.
    await page.goto('/maps?all=1');
    await expect(page.locator(`#wf-${predId}`)).toHaveCount(0);
  });
});
