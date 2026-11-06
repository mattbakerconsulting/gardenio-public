// Backfill (ka-0000000439): the /decisions page's viewer-owned SCOPE + the
// ?all=1 toggle + the per-row operator chip — end-to-end against the real
// daemon. None of these had a browser-level spec before this file.
//
// FEATURE (cmd/beads-viewer/decisions.go::handleDecisions):
//   The list DEFAULT-scopes to the decisions the viewer OWNS — the operator
//   the decision is addressed to (operated_by_id). Passing ?all=1 (the
//   "Show all" toggle) drops the scope and renders the full pending queue.
//   When the viewer's operator identity can't be resolved (anonymous /
//   unprovisioned) there is no "mine" set, so the handler falls back to the
//   full queue and hides the toggle. Each row carries an operator chip
//   linking to /u/{slug}.
//
// WHY THIS SPEC (not covered elsewhere):
//   - decisions_owner_scope_test.go (Go unit) drives handleDecisions against a
//     FAKE daemon (inboxFilteringDaemon) whose OperatedByID filter is faked in
//     memory — it proves the handler ISSUES the predicate, not that the real
//     daemon HONORS it. The whole point of the e2e ("the wire protocol IS what
//     we test", README) is the round-trip: kd create --operated-by X →
//     operators row + oidc alias → viewer resolver LookupOperatorByAlias →
//     daemon operated_by_id filter → scoped rows. This spec pins that chain.
//   - test-decision-reports.sh / test-decisions-yield.sh exercise the `gb
//     decision` CLI only; neither GETs the viewer /decisions HTTP page, so the
//     scope predicate + toggle were unasserted (gardener HEAD-verify on
//     ka-0000000439). The only `?all` coverage in the repo is maps.spec.js
//     (/maps?all=1) — a different handler.
//
// AUTH / IDENTITY MECHANICS (dev-bypass harness, playwright.config webServer):
//   BEADS_VIEWER_DEV_AUTH_BYPASS=1 promotes X-Auth-Request-Email into the
//   verified viewer identity (dev_auth.go::forwardAuthTrustedEmailToContext),
//   and the viewer resolves that email → operators(id) via the daemon's oidc
//   alias index. So we drive each side of the boundary by CHOOSING the header:
//     - opA's email  → sees only opA's decisions (scope active).
//     - ?all=1       → sees the full queue regardless of header.
//     - no header    → anonymous → unresolvable → full queue, toggle hidden.
//   The `--operated-by oidc:<email>` on the ephemeral create is what LAZILY
//   provisions the operators row + canonical slug + oidc alias on the first
//   attributed write (mirrors the /people directory operator, ka-0000003378),
//   so the viewer's resolver finds it. The email local-part is a valid,
//   non-reserved slug token, so slug === local-part (operator_slug.go).
//
// WHY EPHEMERAL FIXTURES (per-test unique operators): the seeded `decision`
// fixtures are all owned by the global e2e-fixture operator, and the suite
// runs fullyParallel. Each test mints its OWN decisions owned by OWN unique
// operators, then asserts on those SPECIFIC ids (present/absent) — never on a
// queue total — so seeded fixtures and concurrent workers can never perturb it.
// Detritus is bounded: a handful of open decisions per run, label-tagged
// (fixture:e2e:dscope_*) for trivial cleanup.

import { execFileSync } from 'node:child_process';

import { expect, test } from '@playwright/test';

// A request context bound to one viewer identity (or none). Isolating the
// header per-context keeps parallel scope assertions from cross-talking.
async function ctxFor(browser, email) {
  return browser.newContext(
    email ? { extraHTTPHeaders: { 'X-Auth-Request-Email': email } } : undefined,
  );
}

// uniqueSuffix builds a per-invocation token so concurrent workers never
// collide on the fixture label or the operator identity.
function uniqueSuffix() {
  return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

// Shell out to `kd create` (mirroring decision_resolve.spec.js) to mint a
// fresh open decision addressed to a SPECIFIC operator. Returns the
// daemon-assigned id. `operatorEmail` is provisioned (operators row + oidc
// alias + slug) on this first attributed write, so the viewer resolver can
// map that email → this decision's operated_by_id.
function createDecisionOwnedBy(operatorEmail, tag) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  const options = JSON.stringify([
    { id: 'a', short: 'Option A', label: 'Pick A', artifact_type: 'report' },
    { id: 'b', short: 'Option B', label: 'Pick B', artifact_type: 'report' },
  ]);
  const args = [
    'create',
    `[E2E dscope] decision ${tag}`,
    '--type',
    'decision',
    '--operated-by',
    `oidc:${operatorEmail}`,
    '--label',
    `fixture:e2e:dscope_${tag}`,
    '--field',
    'prompt=dscope ephemeral decision — pick option A or B.',
    '--field',
    `options=${options}`,
    '--json',
  ];
  return runKdCreate(args);
}

// Provision an operator that owns ZERO open decisions, so the empty-mine
// nudge test has a resolvable-but-empty viewer. Attributing any bead is what
// creates the operators row + oidc alias; a throwaway open task (never on
// /decisions, no project label so it can't perturb project:gardenio's
// filter-chip counts) does the job without adding a decision to the queue.
function provisionOperatorWithNoDecisions(operatorEmail, tag) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  const args = [
    'create',
    `[E2E dscope] throwaway provisioning task ${tag}`,
    '--type',
    'task',
    '--operated-by',
    `oidc:${operatorEmail}`,
    '--no-parent',
    '--label',
    `fixture:e2e:dscope_provision_${tag}`,
    '--json',
  ];
  runKdCreate(args);
}

function runKdCreate(args) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  let raw;
  try {
    raw = execFileSync('kd', args, { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (err) {
    const stderr = err.stderr ? err.stderr.toString() : '';
    const firstLine = stderr.split('\n').find((l) => l.trim().length > 0) || err.message;
    throw new Error(`kd create failed (exit ${err.status}): ${firstLine}`);
  }
  // Some kd subcommands print a deprecation banner before the JSON payload;
  // strip everything before the first `{` or `[` (matches seed.mjs).
  const i = raw.search(/[{\[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) {
    throw new Error(`kd create returned no id: ${raw}`);
  }
  return bead.id;
}

// Slug the daemon derives for an oidc operator = lowercased email local-part
// when it is a valid, non-reserved token (operator_slug.go::candidateOperatorSlug).
// Our local-parts (e2e-dscope-…) always satisfy that, so this is exact.
function slugFor(operatorEmail) {
  return operatorEmail.split('@')[0].toLowerCase();
}

const row = (page, id) => page.locator(`li.bead-row[data-bead-id="${id}"]`);
const scopeToggle = (page) => page.locator('p.decisions-scope-toggle');

test.describe('/decisions viewer-owned scoping', () => {
  test('default scope shows only the viewer’s own decisions + Show-all toggle + operator chip', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const opA = `e2e-dscope-a-${s}@example.test`;
    const opB = `e2e-dscope-b-${s}@example.test`;
    const decA1 = createDecisionOwnedBy(opA, `a1-${s}`);
    const decA2 = createDecisionOwnedBy(opA, `a2-${s}`);
    const decB = createDecisionOwnedBy(opB, `b-${s}`);

    const ctx = await ctxFor(browser, opA);
    try {
      const page = await ctx.newPage();
      const res = await page.goto('/decisions');
      expect(res.status(), 'decisions HTTP status').toBe(200);

      // opA's OWN decisions render; opB's does NOT leak into the scoped view.
      // This is the load-bearing scope predicate — a faked filter can pass the
      // Go unit test, but only the real daemon's operated_by_id filter keeps
      // decB out here.
      await expect(row(page, decA1)).toBeVisible();
      await expect(row(page, decA2)).toBeVisible();
      await expect(row(page, decB)).toHaveCount(0);

      // The scope is active: the h1 carries the "· yours" marker and the toggle
      // offers "Show all" (linking to ?all=1).
      await expect(page.locator('h1').first()).toContainText('yours');
      const toggle = scopeToggle(page);
      await expect(toggle).toBeVisible();
      const showAll = toggle.locator('a.chip', { hasText: 'Show all' });
      await expect(showAll).toBeVisible();
      await expect(showAll).toHaveAttribute('href', /\/decisions\?all=1$/);
      await expect(toggle.locator('a.chip', { hasText: 'Show mine' })).toHaveCount(0);

      // Per-row operator chip: links to opA's homepage (/u/{slug}) and is
      // flagged as the viewer's own (is-viewer-self) — proves the chip resolved
      // the row's operated_by to the viewer, not a bare id fallback.
      const chip = row(page, decA1).locator(`.bead-row-operator a[href="/u/${slugFor(opA)}"]`);
      await expect(chip).toBeVisible();
      await expect(chip).toHaveClass(/\bis-viewer-self\b/);
    } finally {
      await ctx.close();
    }
  });

  test('?all=1 drops the scope and reveals a peer’s decision + Show-mine toggle', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const opA = `e2e-dscope-a-${s}@example.test`;
    const opB = `e2e-dscope-b-${s}@example.test`;
    const decA = createDecisionOwnedBy(opA, `a-${s}`);
    const decB = createDecisionOwnedBy(opB, `b-${s}`);

    const ctx = await ctxFor(browser, opA);
    try {
      const page = await ctx.newPage();
      const res = await page.goto('/decisions?all=1');
      expect(res.status(), 'decisions?all=1 HTTP status').toBe(200);

      // Both the viewer's own AND the peer's decision render — the operator
      // predicate is dropped.
      await expect(row(page, decA)).toBeVisible();
      await expect(row(page, decB)).toBeVisible();

      // The toggle now flips to "Show mine" (linking back to the scoped view).
      const toggle = scopeToggle(page);
      await expect(toggle).toBeVisible();
      const showMine = toggle.locator('a.chip', { hasText: 'Show mine' });
      await expect(showMine).toBeVisible();
      await expect(showMine).toHaveAttribute('href', /\/decisions$/);
      await expect(toggle.locator('a.chip', { hasText: 'Show all' })).toHaveCount(0);
    } finally {
      await ctx.close();
    }
  });

  test('the scope toggle round-trips (Show all ⇄ Show mine) and re-scopes the list', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const opA = `e2e-dscope-a-${s}@example.test`;
    const opB = `e2e-dscope-b-${s}@example.test`;
    const decA = createDecisionOwnedBy(opA, `a-${s}`);
    const decB = createDecisionOwnedBy(opB, `b-${s}`);

    const ctx = await ctxFor(browser, opA);
    try {
      const page = await ctx.newPage();
      await page.goto('/decisions');

      // Scoped: peer decision hidden.
      await expect(row(page, decA)).toBeVisible();
      await expect(row(page, decB)).toHaveCount(0);

      // Click "Show all" → navigates to ?all=1 and the peer decision appears.
      await scopeToggle(page).locator('a.chip', { hasText: 'Show all' }).click();
      await expect(page).toHaveURL(/\/decisions\?all=1$/);
      await expect(row(page, decB)).toBeVisible();

      // Click "Show mine" → back to the default scope, peer decision hidden again.
      await scopeToggle(page).locator('a.chip', { hasText: 'Show mine' }).click();
      await expect(page).toHaveURL(/\/decisions$/);
      await expect(row(page, decA)).toBeVisible();
      await expect(row(page, decB)).toHaveCount(0);
    } finally {
      await ctx.close();
    }
  });

  test('an anonymous viewer sees the full queue with the scope toggle hidden', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const opA = `e2e-dscope-a-${s}@example.test`;
    const opB = `e2e-dscope-b-${s}@example.test`;
    const decA = createDecisionOwnedBy(opA, `a-${s}`);
    const decB = createDecisionOwnedBy(opB, `b-${s}`);

    // No X-Auth-Request-Email header → unresolvable viewer → fall back to all.
    const ctx = await ctxFor(browser, null);
    try {
      const page = await ctx.newPage();
      const res = await page.goto('/decisions');
      expect(res.status(), 'anonymous decisions HTTP status').toBe(200);

      // Both operators' decisions render (no scope), and the scope toggle is
      // absent entirely (CanScope=false hides it).
      await expect(row(page, decA)).toBeVisible();
      await expect(row(page, decB)).toBeVisible();
      await expect(scopeToggle(page)).toHaveCount(0);
      await expect(page.locator('a.chip', { hasText: 'Show mine' })).toHaveCount(0);
      await expect(page.locator('h1').first()).not.toContainText('yours');
    } finally {
      await ctx.close();
    }
  });

  test('a resolvable viewer with no owned decisions gets the empty-mine nudge', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const opEmpty = `e2e-dscope-empty-${s}@example.test`;
    // Provision the operator (resolvable) WITHOUT giving it any decision.
    provisionOperatorWithNoDecisions(opEmpty, s);

    const ctx = await ctxFor(browser, opEmpty);
    try {
      const page = await ctx.newPage();
      const res = await page.goto('/decisions');
      expect(res.status(), 'empty-mine decisions HTTP status').toBe(200);

      // The scoped empty-state copy renders and points the viewer at Show all —
      // NOT the unscoped "No open decisions." (which would mean the scope
      // silently fell back to the full queue).
      await expect(page.locator('p.empty')).toContainText('No open decisions assigned to you');
      await expect(
        page.locator('p.empty a[href*="all=1"]'),
        'empty-mine nudge links to Show all',
      ).toBeVisible();
    } finally {
      await ctx.close();
    }
  });
});
