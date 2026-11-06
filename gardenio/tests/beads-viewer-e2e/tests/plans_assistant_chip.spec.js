// /plans INDEX — the assistant STATUS CHIP + its terminal-LINK contract, end to
// end against the real daemon (task gd-imf6Obom7F; epic gd-80TfM5mPtd lineage).
//
// S4 of the plans-UX epic surfaces each plan's bound plan-assistant as a status
// chip on the index card — three faces:
//
//   live   — an assistant in an alive agent_state (working/spawning/yielding)
//            updated within the 1h stale window (planagent.IsLive). The card
//            renders .plan-agent-chip-live AND a reachable "terminal →" link.
//   parked — an assistant NOT alive (agent_state=stopping, or alive-but-stale).
//            The card renders .plan-agent-chip-parked AND still a terminal link
//            (a parked agent is reachable — its bead page embeds the screen).
//   none   — no gardener and no assistant bound. The card renders
//            .plan-agent-chip-none and NO terminal link.
//
// WHAT THIS SPEC OWNS (the gap the sibling index specs left). plans_index_ux.spec.js
// and plans_index_arms.spec.js already pin that the live and parked FACES render
// (via the seeded live_assistant / parked_assistant fixtures), but both assert the
// terminal link's href only as `/.+/` — "some non-empty target". They do NOT pin
// the actual LINK CONTRACT the Go unit tests care about (TestPlansIndex_
// AssistantChipLive / …Parked): the link must point at the assistant's own BEAD
// PAGE `/{id}` fallback and must NEVER use the removed `/agents/{id}` route, which
// 404s (ka-0000007019 / MR !5316 — there is no GET /agents/{id} handler). With no
// COOPMUX_PUBLIC_URL in the harness (playwright.config webServer env) and no
// pod_name on these agents, assistantLinkURL falls back to `/{id}`, so the href is
// exactly `/<assistant-bead-id>`. This spec pins that contract for all three
// states — the real-daemon round-trip that COMPLEMENTS the Go unit tests.
//
// WHY BOTH SEEDED AND EPHEMERAL. The live/parked FACE + href legs read the durable
// label-stable fixtures (live_assistant_agent, parked_assistant_agent) so a
// regression is caught by a fixed id with no per-run wait. The task also asks
// specifically for a live assistant bound to an EPHEMERAL plan — the full
// kd→daemon→viewer round-trip, not a pre-baked fixture — so `assistant chip flips
// … end-to-end` seeds a fresh plan + assistant via `kd` and polls the index until
// the chip resolves. That poll is REQUIRED: planAssistantBulk memoizes its
// role-scoped agent list for planEnrichListCacheTTL (60s, plans_enrich_list_cache.go),
// so a just-seeded assistant can lag the index by up to that TTL. The seeded-
// fixture legs never pay it (the fixtures exist before the suite's first /plans
// load warms the cache); the ephemeral leg polls past it under an extended test
// timeout. The none arm is cache-immune (a plan with no assistant has no entry in
// the join regardless of cache freshness), so it needs no poll.
//
// AUTH / SCOPING (dev-bypass harness): BEADS_VIEWER_DEV_AUTH_BYPASS=1 promotes
// X-Auth-Request-Email into the verified viewer identity. `kd`-authored beads are
// attributed to BEADS_OPERATED_BY (not the viewer), and the /plans index
// default-scopes to the viewer's own operated_by, so every navigation uses
// `/plans?operated_by=` (the empty-key "show all" form) to render a plan authored
// by a different alias — the same scoping plans.spec.js / plans_index_ux.spec.js
// document.
//
// ISOLATION: the ephemeral plans/assistants are minted per-run in project:plansux
// with a unique suffix and labelled fixture:e2e:plan_asst_* — never the shared
// seed fixtures — so under fullyParallel:true they never race the read tests that
// assert "no assistant" on plan_epic.
//
// LOCAL-RUN NOTE (mirrors plans_signal_strip.spec.js): this pod has no hermetic
// Postgres and localhost:8080 is the PROD daemon (which must never be seeded), so
// the daemon-backed legs run only in CI's `test-beads-viewer-e2e` task
// (postgresql-16 + pgvector → kd serve → beads-viewer). See the suite README.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// "Show all plans" — the empty operated_by key drops the default "scope to me"
// filter so a plan authored by a different alias renders for any viewer (see
// plans.spec.js PLANS_ALL).
const PLANS_ALL = '/plans?operated_by=';

const planCard = (page, id) => page.locator(`li.plan-card[data-plan-id="${id}"]`);

// kd shells out to the CLI (mirrors plans.spec.js / plans_signal_strip.spec.js) to
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

function uniqueSuffix() {
  return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

// createEphemeralPlan mints a fresh plan-labeled epic (one in_progress step) in
// project:plansux. No gardener and no assistant are bound, so its index chip starts
// in the `none` state — the pre-spawn surface the assistant seed then changes.
function createEphemeralPlan(suffix) {
  const epicId = kdCreateJSON([
    'create',
    `[E2E ephemeral] assistant-chip plan ${suffix}`,
    '--type', 'epic',
    '--label', 'plan',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_asst_${suffix}`,
  ]);
  const stepId = kdCreateJSON([
    'create',
    `[E2E ephemeral] assistant-chip step ${suffix}`,
    '--type', 'task',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_asst_step_${suffix}`,
  ]);
  kd(['dep', 'add', stepId, epicId, '--type', 'parent-child']);
  // in_progress on a kind=issue bead requires an assignee in the same update
  // (the daemon's in_progress→assignee invariant, documented in seed.mjs).
  kd(['update', stepId, '--status', 'in_progress', '--assignee', 'e2e-plans']);
  return { epicId, stepId };
}

// seedPlanAssistant binds a plan-assistant agent bead to `epicId` by the same
// contract isPlanAssistantBead enforces: the role:plan-gardener label
// (planagent.RoleLabel) + fields.plan_id. `state` is the agent_state that drives
// liveness — "working" (alive, fresh → live) or "stopping" (not alive → parked).
// Returns the assistant bead id (the target the card's terminal link must point
// at, via the /{id} bead-page fallback).
function seedPlanAssistant(epicId, suffix, state) {
  return kdCreateJSON([
    'create',
    `[E2E ephemeral] plan assistant ${suffix} (${state})`,
    '--type', 'agent',
    '--label', 'role:plan-gardener',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_asst_agent_${suffix}`,
    // The `agent` AND `role` fields are both required on type=agent (the e2e
    // daemon's stored type:agent schema — `type=agent requires fields: [role]`);
    // `project` mirrors the seed fixtures. The role FIELD is `plan-gardener`, the
    // same value the seed assistant fixtures use — distinct from the
    // role:plan-gardener LABEL below that isPlanAssistantBead matches on. plan_id
    // is the binding the index join reads (isPlanAssistantBead); agent_state
    // drives liveness.
    '--field', `agent=e2e-plan-asst-${suffix}`,
    '--field', 'role=plan-gardener',
    '--field', 'project=plansux',
    '--field', `agent_state=${state}`,
    '--field', `plan_id=${epicId}`,
    '--field', `task_id=${epicId}`,
  ]);
}

// -----------------------------------------------------------------------------
// Seeded-fixture legs — deterministic (no cache wait): pin the terminal-LINK
// contract the sibling index specs left at `/.+/`.
// -----------------------------------------------------------------------------
test.describe('/plans index — assistant chip terminal-link contract', () => {
  test('the LIVE assistant chip links to the agent bead page, never /agents/{id}', async ({ page }) => {
    const planId = fixtureId('live_assistant');
    const agentId = fixtureId('live_assistant_agent');
    await page.goto(PLANS_ALL);

    const card = planCard(page, planId);
    await expect(card, 'live-assistant plan renders on the index').toBeVisible();

    // The live face resolves (PickBetter prefers the live agent over the
    // co-bound parked one).
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-live'),
      'the live assistant resolves the chip to the live face',
    ).toHaveCount(1);

    // The link contract: the terminal link points at the LIVE assistant's own
    // bead page /{id} (the assistantLinkURL fallback with no coopmux configured),
    // NOT the parked co-bind and NOT the removed /agents/{id} route.
    const link = card.locator('a.plan-card-agent-link');
    await expect(link, 'a reachable terminal link renders for the live assistant').toBeVisible();
    await expect(link).toHaveAttribute('href', `/${agentId}`);
    const href = await link.getAttribute('href');
    expect(href, 'terminal link must not use the removed /agents/{id} route').not.toMatch(/\/agents\//);
  });

  test('the PARKED assistant chip links to the agent bead page, never /agents/{id}', async ({ page }) => {
    const planId = fixtureId('parked_assistant');
    const agentId = fixtureId('parked_assistant_agent');
    await page.goto(PLANS_ALL);

    const card = planCard(page, planId);
    await expect(card, 'parked-assistant plan renders on the index').toBeVisible();

    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-parked'),
      'a parked-only plan resolves the chip to the parked face',
    ).toHaveCount(1);
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-live, .plan-agent-chip.plan-agent-chip-none'),
      'a parked assistant is neither live nor "not started"',
    ).toHaveCount(0);

    // A parked agent is still reachable — the link renders and obeys the same
    // bead-page contract.
    const link = card.locator('a.plan-card-agent-link');
    await expect(link, 'a reachable terminal link renders for the parked assistant').toBeVisible();
    await expect(link).toHaveAttribute('href', `/${agentId}`);
    const href = await link.getAttribute('href');
    expect(href, 'terminal link must not use the removed /agents/{id} route').not.toMatch(/\/agents\//);
  });
});

// -----------------------------------------------------------------------------
// Ephemeral round-trip — the headline task: a live assistant bound to a FRESH
// plan flips the index chip out of `none` into `live` with the bead-page link,
// and a stopped assistant reads `parked`. Full kd→daemon→viewer path.
// -----------------------------------------------------------------------------
test.describe('/plans index — assistant chip flips end-to-end on an ephemeral plan', () => {
  // The ephemeral assistant can lag the index by up to planEnrichListCacheTTL
  // (60s), so this describe polls past it under an extended per-test timeout.
  test.setTimeout(120 * 1000);

  test('a live assistant flips the ephemeral plan chip to live with a bead-page link', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { epicId } = createEphemeralPlan(suffix);

    // Pre-spawn: no gardener, no assistant → the `none` chip and no terminal link.
    // This is cache-immune (no assistant is bound), so it reads immediately.
    await page.goto(PLANS_ALL);
    const preCard = planCard(page, epicId);
    await expect(preCard, 'fresh ephemeral plan renders on the index').toBeVisible();
    await expect(
      preCard.locator('.plan-agent-chip.plan-agent-chip-none'),
      'a plan with no gardener/assistant reads "not started"',
    ).toHaveCount(1);
    await expect(
      preCard.locator('a.plan-card-agent-link'),
      'no assistant → no terminal link',
    ).toHaveCount(0);

    // Bind a LIVE assistant, then poll the index until the memoized assistant
    // join refreshes (≤ planEnrichListCacheTTL) and the chip flips to live.
    const agentId = seedPlanAssistant(epicId, suffix, 'working');
    await expect(async () => {
      await page.goto(PLANS_ALL);
      await expect(
        planCard(page, epicId).locator('.plan-agent-chip.plan-agent-chip-live'),
        'the seeded live assistant resolves the chip to the live face',
      ).toHaveCount(1);
    }).toPass({ timeout: 90 * 1000, intervals: [1000, 2000, 5000, 10000] });

    // The flipped chip carries the terminal link to the agent's bead page — the
    // /{id} fallback (no coopmux configured), never the removed /agents/{id}.
    const card = planCard(page, epicId);
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-none'),
      'a live assistant is not "not started"',
    ).toHaveCount(0);
    const link = card.locator('a.plan-card-agent-link');
    await expect(link, 'the live assistant is reachable via a terminal link').toBeVisible();
    await expect(link).toHaveAttribute('href', `/${agentId}`);
    const href = await link.getAttribute('href');
    expect(href, 'terminal link must not use the removed /agents/{id} route').not.toMatch(/\/agents\//);
  });

  test('a stopped assistant reads parked on the ephemeral plan', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { epicId } = createEphemeralPlan(suffix);
    const agentId = seedPlanAssistant(epicId, suffix, 'stopping');

    // agent_state=stopping is not in the alive bucket, so IsLive is false
    // regardless of freshness → the parked face. Poll past the assistant-join
    // cache TTL for the just-seeded agent to appear.
    await expect(async () => {
      await page.goto(PLANS_ALL);
      await expect(
        planCard(page, epicId).locator('.plan-agent-chip.plan-agent-chip-parked'),
        'the stopped assistant resolves the chip to the parked face',
      ).toHaveCount(1);
    }).toPass({ timeout: 90 * 1000, intervals: [1000, 2000, 5000, 10000] });

    const card = planCard(page, epicId);
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-live, .plan-agent-chip.plan-agent-chip-none'),
      'a stopped assistant is neither live nor "not started"',
    ).toHaveCount(0);
    // Still reachable, and obeys the bead-page link contract.
    const link = card.locator('a.plan-card-agent-link');
    await expect(link).toHaveAttribute('href', `/${agentId}`);
    expect(await link.getAttribute('href'), 'no /agents/{id} route').not.toMatch(/\/agents\//);
  });
});
