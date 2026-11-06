// Backfill (ka-0000003390, parent ka-0000000958) — the GV phase-2 `/up`
// action footer + global keymap (shipped by ka-0000004641 / MR !6842) had
// ZERO e2e coverage in either suite and was the ONLY uncovered surface among
// the merges since cycle 48's audit point. It is user-facing keyboard
// behavior, and a two-listener keydown race is only observable in a real
// browser — the Go unit tests (up_test.go / up_asset_test.go) only assert the
// asset is served and string-contains the contract markers; none issues a POST
// to a resolved endpoint. This spec closes that gap.
//
// Source under test:
//   templates/static/up.js      — the /up document keymap + action dispatch
//   templates/static/keys.js    — the GLOBAL viewer keymap (the collision peer)
//   up_actions.go / up.go       — the type-conditional footer + UP_KEYMAP island
//   templates/up.html           — the rendered footer + drawers
//
// THE HAZARD THIS PINS — a two-driver keydown collision. `/up` is the first
// viewer page that installs a SECOND document-level `keydown` listener (up.js)
// over the same keys as the global one (keys.js). up.js self-gates OFF on every
// non-/up page (its `[data-surface="up"]` presence check), but keys.js had no
// `/up` guard, so BOTH fired on one press. The most visible symptom: `?` opened
// TWO independent <dialog>s (up.js's `.up-help` AND keys.js's `.kbd-help`). The
// fix (this bead) is the SYMMETRIC mirror of up.js's self-gate: keys.js bails on
// `[data-surface="up"]`, making up.js the sole keyboard owner on /up. The
// anti-collision assertion below (`dialog[open]` count === 1) is what reds if
// that gate is ever removed.
//
// Auth: dev-bypass mode is on (BEADS_VIEWER_DEV_AUTH_BYPASS=1), so
// `oidcauth.EmailFromRequest` defaults to "" — the close/comment endpoints 401
// on an empty identity and the drawers would be inert. Set `X-Auth-Request-Email`
// at the test level to mimic a logged-in operator without standing up Keycloak;
// devBypassMiddleware is a passthrough, so the header reaches the handler the
// same way ForwardAuth would inject it in prod.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

const AUTH_EMAIL = 'e2e-up-keymap@example.test';

test.use({
  extraHTTPHeaders: {
    'X-Auth-Request-Email': AUTH_EMAIL,
  },
});

// PROJECT ISOLATION (project:upkeymap): the live-close test below CLOSES its
// ephemeral task. `kd create` defaults to project:gardenio, so a closed task
// there would be that project's first closed issue-macro bead — which activates
// filter_chips.spec.js's "multi-select carries both statuses" assertion (guarded
// on closedCount>0), which then fails because the project view's
// resolveProjectStatus() coerces `?status=open,closed` back to `open`. The
// seeded fixtures dodge this exact trap by keeping their closed beads out of
// project:gardenio (see the plan_step_done PROJECT ISOLATION note in
// seed/fixtures.mjs); this ephemeral does the same via KD_PROJECT below.
const UP_KEYMAP_PROJECT = 'upkeymap';

// createEphemeralTask shells out to `kd create` (mirroring
// decision_resolve.spec.js::createEphemeralDecision) to make a fresh OPEN task
// per test. The live-close round-trip mutates the bead (closes it), so it must
// NOT touch the shared `generic` fixture — a second run would then try to close
// an already-closed bead. Each ephemeral task is label-tagged for trivial
// cleanup and its daemon-assigned id lets the test target its own bead.
function createEphemeralTask(suffix) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  // KD_PROJECT overrides the default project:gardenio label so the closed
  // ephemeral lands in the isolated project:upkeymap (see UP_KEYMAP_PROJECT).
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr, KD_PROJECT: UP_KEYMAP_PROJECT };
  const args = [
    'create',
    `[E2E up-keymap ephemeral] task ${suffix}`,
    '--type',
    'task',
    '--label',
    `fixture:e2e:up_keymap_${suffix}`,
    '--json',
  ];
  let raw;
  try {
    raw = execFileSync('kd', args, { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (err) {
    const stderr = err.stderr ? err.stderr.toString() : '';
    const firstLine = stderr.split('\n').find((l) => l.trim().length > 0) || err.message;
    throw new Error(`kd create failed (exit ${err.status}): ${firstLine}`);
  }
  // Strip any deprecation banner printed before the JSON payload (matches
  // seed.mjs / decision_resolve.spec.js).
  const i = raw.search(/[{[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) {
    throw new Error(`kd create returned no id: ${raw}`);
  }
  return bead.id;
}

test.describe('/up type-conditional footer', () => {
  test('task card renders the primary + mnemonic-keyed secondaries', async ({ page }) => {
    // The shared `generic` fixture is a plain task bead — non-mutating tests
    // deep-link it via /up/{id} (handleUpBead), which bypasses the auto-picker
    // so the render is deterministic regardless of daemon queue state.
    const id = fixtureId('generic');
    await page.goto(`/up/${id}`);

    const surface = page.locator('[data-surface="up"]');
    await expect(surface).toBeVisible();

    const card = page.locator('.up-card');
    await expect(card).toHaveClass(/\bup-card-task\b/);
    await expect(card).toHaveAttribute('data-bead-type', 'task');

    // Primary is the Enter-bound Close: a live POST to the close endpoint that
    // auto-advances (§4.4 row 1). The data-up-* attributes are the contract
    // up.js dispatches BOTH the key and the click from — losing any of them
    // desyncs the button from the keymap island.
    const primary = card.locator('.up-action-primary');
    await expect(primary).toHaveAttribute('data-up-key', 'Enter');
    await expect(primary).toHaveAttribute('data-up-kind', 'post');
    await expect(primary).toHaveAttribute('data-up-endpoint', '/b/{id}/close');
    await expect(primary).toHaveAttribute('data-up-advance', '1');

    // Type secondaries: the footer renders the ≤4 per-type secondaries (task =
    // comment/reassign/snooze/escalate) PLUS the always-present ⋯more palette
    // button. Assert the type secondaries are 1..4 (PRD §4.3 "primary + up to
    // four secondaries"), excluding the palette overflow entry.
    const typeSecondaries = card.locator('.up-card-footer-keys .up-secondary:not(.up-action-more)');
    const secCount = await typeSecondaries.count();
    expect(secCount, 'PRD §4.3 caps type secondaries at four').toBeLessThanOrEqual(4);
    expect(secCount, 'a non-read-only type footer shows at least one secondary').toBeGreaterThanOrEqual(1);

    // Mnemonic underline markup (upMnemonic output): each letter-keyed secondary
    // underlines the mnemonic char (e.g. `<kbd>c</kbd> <u>C</u>omment`). At least
    // one secondary carries the <u>; dropping upMnemonic would render them plain.
    await expect(card.locator('.up-card-footer-keys .up-secondary u').first()).toBeVisible();

    // The ⋯more overflow entry is the single palette entry point (cmd-K).
    await expect(card.locator('.up-action-more')).toHaveAttribute('data-up-kind', 'palette');
  });

  test('decision card renders a distinct (type-conditional) primary + resolve chips', async ({ page }) => {
    // The footer is type-conditional: a decision's primary is "Resolve" against
    // the decision endpoint, NOT the task's Close — asserting both fixtures pins
    // that buildUpFooter actually branches on type rather than emitting one
    // footer for everything. Non-mutating (renders only; no resolve is issued).
    const id = fixtureId('decision');
    await page.goto(`/up/${id}`);

    const card = page.locator('.up-card');
    await expect(card).toHaveAttribute('data-bead-type', 'decision');

    const primary = card.locator('.up-action-primary');
    await expect(primary).toHaveAttribute('data-up-endpoint', '/b/{id}/decision/resolve');
    // The seeded decision carries two options, so the label gains the
    // "recommended" hint and the one-tap chip row renders.
    await expect(primary).toContainText('Resolve');
    const chips = card.locator('.up-decision-chip');
    await expect(chips).toHaveCount(2);
    await expect(chips.first()).toHaveAttribute('data-up-option', 'a');
  });
});

test.describe('/up two-driver keydown collision', () => {
  test('? opens EXACTLY ONE help dialog (anti-collision) and Esc closes it', async ({ page }) => {
    // THE anti-collision assertion. Pre-fix, `?` on /up opened two <dialog>s:
    // up.js's `.up-help` AND keys.js's `.kbd-help`. With keys.js gated OFF on
    // /up, only up.js's dialog opens — `dialog[open]` count is exactly 1, and
    // keys.js's dialog is never even created (its keydown handler bails before
    // ensureHelpDialog runs). Either regression — un-gating keys.js, or up.js's
    // helpOpen() guard breaking — flips this count.
    const id = fixtureId('generic');
    await page.goto(`/up/${id}`);
    await expect(page.locator('[data-surface="up"]')).toBeVisible();

    // Clean load: no open dialog yet.
    await expect(page.locator('dialog[open]')).toHaveCount(0);

    // `?` is Shift+/ on a US keyboard.
    await page.keyboard.press('Shift+Slash');

    // Exactly one dialog is open, and it is up.js's — NOT the global one.
    await expect(page.locator('dialog[open]')).toHaveCount(1);
    await expect(page.locator('dialog.up-help[open]')).toHaveCount(1);
    // keys.js's help dialog must not exist at all on /up — proof the global
    // handler was gated off before it could lazily createElement one.
    await expect(page.locator('dialog.kbd-help')).toHaveCount(0);

    // Esc closes the (single) dialog — native <dialog> Esc + up.js both agree.
    await page.keyboard.press('Escape');
    await expect(page.locator('dialog[open]')).toHaveCount(0);
  });

  test('c opens the /up comment drawer with focus in #up-comment-text; no global steal', async ({ page }) => {
    // `c` collides with keys.js's focusComment(), which targets the GLOBAL
    // `#comment-text` textarea. /up names its drawer textarea `#up-comment-text`
    // (a different id) AND keys.js is gated off, so `c` opens exactly the /up
    // comment drawer and focus lands in ITS textarea — the global handler
    // neither fires nor has a `#comment-text` on the page to grab.
    const id = fixtureId('generic');
    await page.goto(`/up/${id}`);
    await expect(page.locator('[data-surface="up"]')).toBeVisible();

    const drawer = page.locator('[data-up-drawer="comment"]');
    await expect(drawer).toBeHidden();

    await page.keyboard.press('c');

    await expect(drawer).toBeVisible();
    await expect(page.locator('#up-comment-text')).toBeFocused();
    // The global keys.js comment target is absent on /up — the collision is
    // structurally impossible to win as long as the drawer keeps its own id.
    await expect(page.locator('#comment-text')).toHaveCount(0);
  });

  test('j skips the card (advances) and no global row cursor wakes up', async ({ page }) => {
    // `j` is a skip alias in up.js (§4.2) → a full navigation to /up (the
    // auto-picker), which is how the proto advances the stack. In keys.js `j`
    // moves a `.bead-row` cursor — a different action on the same key. With the
    // gate, only up.js's skip runs. /up renders no `.bead-row`, so even an
    // un-gated keys.js could not move a cursor here — the collision is latent —
    // but asserting the skip navigation AND the absence of a keys.js cursor
    // stamp pins both halves of the contract.
    const id = fixtureId('generic');
    await page.goto(`/up/${id}`);
    await expect(page.locator('[data-surface="up"]')).toBeVisible();

    // No global cursor is possible: /up carries no rows for keys.js to target.
    await expect(page.locator('.bead-row')).toHaveCount(0);

    // Pressing j navigates to /up (skip) — the id drops from the path.
    await Promise.all([
      page.waitForURL((url) => /\/up\/?$/.test(url.pathname) && !/\/up\/kd-/.test(url.pathname)),
      page.keyboard.press('j'),
    ]);

    // Landed on the auto-picked card, and keys.js never stamped a row cursor.
    await expect(page.locator('[data-surface="up"]')).toBeVisible();
    await expect(page.locator('[data-kbd-focus]')).toHaveCount(0);
  });
});

test.describe('/up action dispatch', () => {
  test('primary on a task POSTs /b/{id}/close and the card auto-advances', async ({ page }) => {
    // Live round-trip (§4.4 success-done): Enter on a task fires the close POST;
    // on 200 up.js auto-advances by navigating to /up. Uses an EPHEMERAL task so
    // the close is real (the seeded fixture stays open for the render tests and
    // for re-runs).
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const id = createEphemeralTask(suffix);

    await page.goto(`/up/${id}`);
    await expect(page.locator('.up-card')).toHaveAttribute('data-bead-type', 'task');

    // Wait for the close POST so we know it fired and returned 200 — up.js only
    // advances on r.ok, so a non-200 would leave us on the same card and the
    // URL assertion below would be the real signal, not a flake.
    const responsePromise = page.waitForResponse(
      (r) => r.url().includes(`/b/${id}/close`) && r.request().method() === 'POST',
    );
    await page.keyboard.press('Enter');
    const resp = await responsePromise;
    expect(resp.status(), 'close POST status').toBe(200);

    // Auto-advance: up.js navigates to /up (the next card), dropping the id.
    await page.waitForURL((url) => /\/up\/?$/.test(url.pathname) && !/\/up\/kd-/.test(url.pathname));

    // The bead really closed (not just a 200 that no-op'd): the daemon now
    // reports it closed. Re-fetch via kd so the assertion is on daemon truth,
    // not the viewer's optimistic navigation.
    const status = beadStatus(id);
    expect(status, 'ephemeral task is closed after the primary action').toBe('closed');
  });

  test('a deferred action shows the phase toast instead of POSTing', async ({ page }) => {
    // Deferred actions (sibling-owned / not-yet-built endpoints) must surface an
    // honest "lands with a later phase" toast rather than hit a missing endpoint
    // or lie about success (PRD §10). On a task, Snooze (`s`) is the deferred
    // secondary — it carries data-up-deferred="1" and dispatch short-circuits to
    // the info toast with NO network write.
    const id = fixtureId('generic');
    await page.goto(`/up/${id}`);
    await expect(page.locator('[data-surface="up"]')).toBeVisible();

    // The Snooze secondary is rendered muted + marked deferred.
    const snooze = page.locator('.up-secondary[data-up-key="s"]');
    await expect(snooze).toHaveAttribute('data-up-deferred', '1');

    // Fail the test if ANY POST to a bead endpoint fires — the whole point of
    // "deferred" is that it does not hit the wire.
    let posted = false;
    const onReq = (req) => {
      if (req.method() === 'POST' && req.url().includes('/b/')) posted = true;
    };
    page.on('request', onReq);

    try {
      await page.keyboard.press('s');

      // The footer toast surfaces (info kind, self-dismissing).
      const toast = page.locator('.up-footer-toast');
      await expect(toast).toBeVisible();
      await expect(toast).toHaveClass(/\bup-toast-info\b/);

      // No POST fired — assert after the toast is up so any in-flight request
      // would already have been observed.
      expect(posted, 'a deferred action must not POST').toBe(false);
    } finally {
      page.off('request', onReq);
    }
  });
});

// beadStatus re-fetches a bead's status straight from the daemon via `kd show
// --json`, so the close assertion is anchored on daemon truth rather than the
// viewer's post-action navigation. Mirrors createEphemeralTask's kd shell-out.
function beadStatus(id) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  const raw = execFileSync('kd', ['show', id, '--json'], {
    env,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const i = raw.search(/[{[]/);
  return JSON.parse(i === -1 ? raw : raw.slice(i)).status;
}
