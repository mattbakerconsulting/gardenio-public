// Backfill (gd-k78OwCU630, parent gd-80TfM5mPtd): the /plans index's
// filter-by-me DEFAULT scope + the ?operated_by= "show all" escape + the header
// chip's ?project=-preserving switch links — end-to-end against the real daemon.
//
// WHY THIS SPEC (not covered by the existing plans.spec.js):
//   plans.spec.js deliberately SIDESTEPS scoping — every navigation there uses
//   `/plans?operated_by=` (the empty-key "show all" form) so the seeded plan
//   (authored by a DIFFERENT alias than the tests authenticate as) renders
//   regardless of viewer identity. That means the single biggest index behavior
//   — "when the URL carries no operated_by key and the viewer is authenticated,
//   scope the daemon query to their operator id" (handlePlans, plans.go S1) — has
//   NO browser-level coverage. The Go unit tests (plans_index_test.go,
//   operator_chip_test.go) drive the handler/renderer directly; only an e2e
//   proves the real daemon HONORS the operated_by alias filter on the round-trip
//   kd create --operated-by X → operators row + oidc alias → viewer resolver →
//   scoped rows (mirrors decisions_owner_scope.spec.js for /decisions).
//
// FEATURE (cmd/beads-viewer/plans.go::handlePlans + operator_chip.go
// ::renderPlanHeaderChip):
//   - No ?operated_by= key + authenticated viewer → default-scope to the viewer's
//     own operator id (resolveDefaultOperatedBy → canonicalOIDC(email) =
//     "oidc:<lowercased-email>"). "Mine" == plans the operator is INVOLVED in;
//     the creator facet (the plan epic's operated_by == the viewer) is the
//     load-bearing one here. The header chip renders data-state="mine" with an
//     "(auto)" pill (IsDefault — the scope came from the cold-load default).
//   - A DIFFERENT viewer's default scope is their OWN alias, so an owned plan is
//     hidden from them (the daemon's operated_by filter, not a client cull).
//   - Explicit ?operated_by= (empty value, "show all") drops the scope entirely →
//     chip data-state="all" ("Showing everyone's plans"), and the owned plan
//     renders for ANY viewer.
//   - Every header-chip switch-to link preserves the sibling ?project= filter
//     (ChipExtraQuery = the query minus operated_by; buildOperatorHeaderHref
//     re-encodes it), so switching scope never silently clears the project.
//
// AUTH / IDENTITY MECHANICS (dev-bypass harness, playwright.config webServer):
//   BEADS_VIEWER_DEV_AUTH_BYPASS=1 promotes X-Auth-Request-Email into the
//   verified viewer identity (dev_auth.go), and the default scope becomes
//   "oidc:<that-email-lowercased>". So we drive each side of the boundary by
//   CHOOSING the header per browser context:
//     - the owner's email  → default /plans shows their plan ("mine" chip).
//     - a peer's email     → default /plans HIDES it (scoped to the peer).
//     - ?operated_by=       → full universe, both viewers see it ("all" chip).
//   The `--operated-by oidc:<email>` on the ephemeral create LAZILY provisions
//   the operators row + oidc alias on the first attributed write, so the viewer
//   resolver maps that email → this plan's operated_by. (Same lazy-provision the
//   decisions scope spec relies on.)
//
// WHY EPHEMERAL FIXTURES (per-test unique operators + project): the suite runs
// fullyParallel and the seeded plan fixtures are owned by the global e2e-fixture
// operator. Each test mints its OWN plan owned by its OWN unique operator, tagged
// with a per-run project label, and asserts on that SPECIFIC plan id
// (present/absent) — never on a queue total — so seeded fixtures and concurrent
// workers can never perturb it. Detritus is bounded + label-tagged
// (fixture:e2e:pscope_*) for trivial cleanup.

import { execFileSync } from 'node:child_process';

import { expect, test } from '@playwright/test';

// A request context bound to one viewer identity. Isolating the header per
// context keeps parallel scope assertions from cross-talking.
async function ctxFor(browser, email) {
  return browser.newContext(
    email ? { extraHTTPHeaders: { 'X-Auth-Request-Email': email } } : undefined,
  );
}

// Per-invocation token so concurrent workers never collide on the fixture label,
// the operator identity, or the project label.
function uniqueSuffix() {
  return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

function runKdCreate(args) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  let raw;
  try {
    raw = execFileSync('kd', [...args, '--json'], {
      env,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    });
  } catch (err) {
    const stderr = err.stderr ? err.stderr.toString() : '';
    const firstLine = stderr.split('\n').find((l) => l.trim().length > 0) || err.message;
    throw new Error(`kd create failed (exit ${err.status}): ${firstLine}`);
  }
  // Some kd subcommands print a deprecation banner before the JSON payload;
  // strip everything before the first `{`/`[` (matches seed.mjs).
  const i = raw.search(/[{[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) throw new Error(`kd create returned no id: ${raw}`);
  return bead.id;
}

// Mint a fresh plan (type=epic + label=plan) OWNED by a specific operator and
// tagged with a per-run project label. Returns the epic id. The
// `--operated-by oidc:<email>` is what makes the plan's creator facet resolve to
// this operator, so the /plans default scope treats it as that viewer's "mine".
// A single in_progress step is attached (mirroring plans.spec.js's
// createEphemeralPlan) so the card renders a realistic rollup — scoping keys off
// the EPIC's operated_by, not the step, so the step's owner is irrelevant here.
function createOwnedPlan(operatorEmail, project, tag) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  const epicId = runKdCreate([
    'create',
    `[E2E pscope] plan ${tag}`,
    '--type',
    'epic',
    '--operated-by',
    `oidc:${operatorEmail}`,
    '--label',
    'plan',
    '--label',
    `project:${project}`,
    '--label',
    `fixture:e2e:pscope_${tag}`,
  ]);
  const stepId = runKdCreate([
    'create',
    `[E2E pscope] plan step ${tag}`,
    '--type',
    'task',
    '--label',
    `project:${project}`,
    '--label',
    `fixture:e2e:pscope_step_${tag}`,
  ]);
  // Child → parent parent-child edge so planTimeline (which walks the epic's
  // reverse parent-child deps) renders the step.
  execFileSync('kd', ['dep', 'add', stepId, epicId, '--type', 'parent-child'], {
    env,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  // kind=issue beads enforce an in_progress→assignee invariant (a bare
  // `--status in_progress` no-ops without an assignee — see seed.mjs), so set
  // the assignee in the SAME update.
  execFileSync(
    'kd',
    ['update', stepId, '--status', 'in_progress', '--assignee', 'e2e-pscope'],
    { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] },
  );
  return epicId;
}

const planCard = (page, id) => page.locator(`li.plan-card[data-plan-id="${id}"]`);
const headerChip = (page) => page.locator('span.operator-header-chip');

test.describe('/plans filter-by-me default scoping', () => {
  test('the owner sees their plan on the default /plans with the "mine" (auto) chip', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const owner = `e2e-pscope-owner-${s}@example.test`;
    const project = `pscope-${s}`;
    const planId = createOwnedPlan(owner, project, `owner-${s}`);

    const ctx = await ctxFor(browser, owner);
    try {
      const page = await ctx.newPage();
      // NO ?operated_by= key → the cold-load default scopes the query to the
      // viewer's own operator id.
      const res = await page.goto('/plans');
      expect(res.status(), 'default /plans HTTP status').toBe(200);

      // The owned plan renders as a card under the viewer's own default scope.
      await expect(planCard(page, planId), 'owner sees their own plan by default').toBeVisible();

      // The header chip is in the "mine" state and carries the "(auto)" pill —
      // proof the scope was applied by the cold-load DEFAULT (URL had no
      // operated_by key), not by an explicit ?operated_by=me.
      const chip = headerChip(page);
      await expect(chip).toHaveAttribute('data-state', 'mine');
      await expect(chip.locator('.operator-header-chip-headline')).toContainText('Showing your');
      await expect(
        chip.locator('.operator-header-chip-default'),
        'the (auto) default pill marks a cold-load scope',
      ).toBeVisible();

      // The widen-out affordance points at the empty ?operated_by= ("everyone's
      // plans" discovery link).
      const showAll = chip.locator('a.operator-header-chip-link', { hasText: "everyone's plans" });
      await expect(showAll).toBeVisible();
      await expect(showAll).toHaveAttribute('href', /operated_by=$/);
    } finally {
      await ctx.close();
    }
  });

  test('a different viewer does NOT see the owned plan on their default /plans', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const owner = `e2e-pscope-owner-${s}@example.test`;
    const other = `e2e-pscope-other-${s}@example.test`;
    const project = `pscope-${s}`;
    const planId = createOwnedPlan(owner, project, `hidden-${s}`);

    const ctx = await ctxFor(browser, other);
    try {
      const page = await ctx.newPage();
      // The peer's default scope is their OWN alias, so the owner's plan is
      // filtered out by the daemon's operated_by predicate — not visible.
      const res = await page.goto('/plans');
      expect(res.status(), 'peer default /plans HTTP status').toBe(200);

      await expect(
        planCard(page, planId),
        "a peer's default scope hides the owner's plan",
      ).toHaveCount(0);

      // The peer's view IS scoped (their own "mine"), proving the absence above
      // is a real scope, not an unscoped-empty fallback.
      await expect(headerChip(page)).toHaveAttribute('data-state', 'mine');
    } finally {
      await ctx.close();
    }
  });

  test('?operated_by= (show all) reveals the owned plan to a different viewer', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const owner = `e2e-pscope-owner-${s}@example.test`;
    const other = `e2e-pscope-other-${s}@example.test`;
    const project = `pscope-${s}`;
    const planId = createOwnedPlan(owner, project, `showall-${s}`);

    const ctx = await ctxFor(browser, other);
    try {
      const page = await ctx.newPage();
      // Explicit empty ?operated_by= drops the scope → the full plan universe.
      const res = await page.goto('/plans?operated_by=');
      expect(res.status(), 'show-all /plans HTTP status').toBe(200);

      await expect(
        planCard(page, planId),
        "show-all reveals a peer's plan to any viewer",
      ).toBeVisible();

      // The chip flips to the "all" state and offers a scope-back-to-me link.
      const chip = headerChip(page);
      await expect(chip).toHaveAttribute('data-state', 'all');
      await expect(chip.locator('.operator-header-chip-headline')).toContainText(
        "Showing everyone's plans",
      );
      const scopeMine = chip.locator('a.operator-header-chip-link', { hasText: 'just your plans' });
      await expect(scopeMine).toBeVisible();
      await expect(scopeMine).toHaveAttribute('href', /operated_by=me$/);
    } finally {
      await ctx.close();
    }
  });

  test('the round-trip mine ⇄ all re-scopes the list (owner ↔ peer views the same plan)', async ({
    browser,
  }) => {
    const s = uniqueSuffix();
    const owner = `e2e-pscope-owner-${s}@example.test`;
    const other = `e2e-pscope-other-${s}@example.test`;
    const project = `pscope-${s}`;
    const planId = createOwnedPlan(owner, project, `roundtrip-${s}`);

    // The peer starts scoped (plan hidden), clicks "everyone's plans" → all
    // (plan visible), then "just your plans" → back to their empty scope.
    const ctx = await ctxFor(browser, other);
    try {
      const page = await ctx.newPage();
      await page.goto('/plans');
      await expect(planCard(page, planId)).toHaveCount(0);

      await headerChip(page)
        .locator('a.operator-header-chip-link', { hasText: "everyone's plans" })
        .click();
      await expect(page).toHaveURL(/\/plans\?operated_by=$/);
      await expect(planCard(page, planId)).toBeVisible();

      await headerChip(page)
        .locator('a.operator-header-chip-link', { hasText: 'just your plans' })
        .click();
      await expect(page).toHaveURL(/operated_by=me$/);
      await expect(planCard(page, planId)).toHaveCount(0);
    } finally {
      await ctx.close();
    }
  });

  test('header-chip switch links preserve the ?project= filter', async ({ browser }) => {
    const s = uniqueSuffix();
    const owner = `e2e-pscope-owner-${s}@example.test`;
    const project = `pscope-${s}`;
    const planId = createOwnedPlan(owner, project, `project-${s}`);

    const ctx = await ctxFor(browser, owner);
    try {
      const page = await ctx.newPage();
      // Default scope + a ?project= sibling filter. The plan carries the
      // matching project label, so it still renders here.
      const res = await page.goto(`/plans?project=${project}`);
      expect(res.status(), 'default /plans?project= HTTP status').toBe(200);
      await expect(
        planCard(page, planId),
        'owned plan visible under its project filter + default scope',
      ).toBeVisible();

      // The chip's widen-out link (switching operated_by scope) must carry the
      // ?project= filter forward — ChipExtraQuery preserves every sibling filter
      // so no dimension silently resets on a scope switch.
      const chip = headerChip(page);
      await expect(chip).toHaveAttribute('data-state', 'mine');
      const showAll = chip.locator('a.operator-header-chip-link', { hasText: "everyone's plans" });
      await expect(showAll).toBeVisible();
      const href = await showAll.getAttribute('href');
      expect(href, 'switch-to-all link preserves ?project=').toContain(`project=${project}`);
      expect(href, 'switch-to-all link still drops the scope').toMatch(/operated_by=$/);

      // Following it lands on the show-all view that still carries the project
      // filter, and the plan remains visible.
      await showAll.click();
      await expect(page).toHaveURL(new RegExp(`project=${project}`));
      await expect(page).toHaveURL(/operated_by=$/);
      await expect(planCard(page, planId)).toBeVisible();

      // The reverse link (scope back to "just your plans") also keeps ?project=.
      // The chip re-rendered on the show-all navigation, so re-query it fresh.
      const scopeMine = headerChip(page).locator('a.operator-header-chip-link', {
        hasText: 'just your plans',
      });
      await expect(scopeMine).toBeVisible();
      expect(
        await scopeMine.getAttribute('href'),
        'scope-to-mine link preserves ?project=',
      ).toContain(`project=${project}`);
    } finally {
      await ctx.close();
    }
  });
});
