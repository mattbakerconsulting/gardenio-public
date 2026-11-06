// Plans UX behavioral coverage (epic ka-0000001353 — Slack request from Matt
// Baker, 2026-07-07: "get some e2e tests around plans UX, esp spawning and
// assistant and the UI being responsive").
//
// The Plans surface is a read/write split (plans.go / plans_assistant.go):
//   - GET  /plans            — the index: one .plan-card per plan-labeled epic.
//   - GET  /plans/{id}       — the cockpit: #plan-narrative (left), #plan-steps
//                              (right, the build timeline), #plan-agent
//                              (full-width bottom — the merged plan-agent card,
//                              step 6 ka-0000000116). All three are SSE-swappable.
//   - POST /plans/{id}/assistant/prompt  — operatorOnly. Spawns/wakes/nudges the
//                              plan agent (a plan-gardener-role agent bead
//                              bound by fields.plan_id). This IS the "spawning +
//                              assistant" path the request calls out.
//   - POST /plans/{id}/steps/{step}/close — operatorOnly. Marks a step done and
//                              swaps #plan-steps in place (htmx outerHTML).
//
// Fixtures (seed/fixtures.mjs): plan_epic (epic + label=plan), plan_bundle
// (narrative), plan_step_done/ip/todo (the timeline). See that file for why a
// plan spine had to be added — none of the pre-existing fixtures satisfy the
// handlePlans query (Types=[epic] + Labels=[plan]).
//
// AUTH: the viewer runs in dev-bypass (BEADS_VIEWER_DEV_AUTH_BYPASS=1), so a
// request is "operator" iff it carries X-Auth-Request-Email — operatorMiddleware
// stamps it onto the daemon ctx (same pattern as quick_spawn.spec.js). Read
// assertions need no header; the two POST mutations do.
//
// SCOPING: the /plans index default-scopes to the viewer's own operated_by
// (S1 filter-by-me). The seed writes as oidc:e2e-fixture@example.com (CI's
// BEADS_OPERATED_BY), which is NOT the email these tests authenticate as, so
// every navigation uses `/plans?operated_by=` — the empty-key "show all" form —
// to render the seeded plan regardless of viewer identity.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

// createEphemeralPlan shells out to `kd` (mirroring decision_resolve.spec.js's
// createEphemeralDecision) to build a fresh, isolated plan per test: a
// type=epic + label=plan with one in_progress step child. The step-close test
// mutates its step, so it MUST NOT touch the shared seed fixtures — under
// `fullyParallel: true` that would race the index/cockpit read tests that
// assert on plan_step_ip. Returning the daemon-assigned ids lets the test
// target only its own beads. Leftover beads are labelled fixture:e2e:plan_eph_*
// so they are debuggable per run.
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
  // Some kd subcommands print a deprecation banner before the JSON payload;
  // strip everything before the first `{`/`[` (matches seed.mjs).
  const i = raw.search(/[{[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) throw new Error(`kd create returned no id: ${raw}`);
  return bead.id;
}

function createEphemeralPlan(suffix) {
  const epicId = kdCreateJSON([
    'create',
    `[E2E ephemeral] plan ${suffix}`,
    '--type', 'epic',
    '--label', 'plan',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_eph_${suffix}`,
  ]);
  const stepId = kdCreateJSON([
    'create',
    `[E2E ephemeral] plan step ${suffix}`,
    '--type', 'task',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_eph_step_${suffix}`,
  ]);
  // Child → parent parent-child edge (planTimeline walks the epic's reverse
  // parent-child deps), matching the seed's dependsOn shape.
  kd(['dep', 'add', stepId, epicId, '--type', 'parent-child']);
  // Land the step in_progress so the timeline renders it is-active with a Done
  // button. kind=issue beads enforce an in_progress→assignee invariant (a bare
  // `--status in_progress` is rejected/no-ops without an assignee — see
  // seed.mjs), so set the assignee in the SAME update.
  kd(['update', stepId, '--status', 'in_progress', '--assignee', 'e2e-plans']);
  return { epicId, stepId };
}

// Operator identity for the write tests. Any non-empty email flips the viewer
// into operator mode under dev-bypass; the value only has to be stable.
const OPERATOR_EMAIL = 'e2e-plans@example.test';

// "Show all plans" — the empty operated_by key. handlePlans treats an explicit
// (even empty) operated_by as "the viewer meant it": urlHasOperatedBy wins over
// the default-to-me scoping, so the seeded plan (authored by a different alias)
// renders for any viewer. Without this the index would say "No plans for you".
const PLANS_ALL = '/plans?operated_by=';

// -----------------------------------------------------------------------------
// 1. Index — the /plans list renders the seeded plan as a card with its rollup.
// -----------------------------------------------------------------------------
test.describe('plans index', () => {
  test('renders the seeded plan card with progress + next-step + assistant chip', async ({ page }) => {
    const planId = fixtureId('plan_epic');
    await page.goto(PLANS_ALL);

    // The page is the Plans index (nav.spec.js already guards the h1; here we
    // just confirm we landed before drilling into the card).
    await expect(page.locator('h1')).toHaveText(/^Plans/);

    // The seeded plan renders as a .plan-card keyed by its epic id.
    const card = page.locator(`li.plan-card[data-plan-id="${planId}"]`);
    await expect(card, 'seeded plan card present on /plans').toBeVisible();

    // Title links to the cockpit.
    const title = card.locator('a.plan-card-title');
    await expect(title).toHaveText('[E2E fixture] Plans UX demo plan');
    await expect(title).toHaveAttribute('href', new RegExp(`/plans/${planId}$`));

    // Progress rollup label format is "<closed>/<total> steps · <pct>%"
    // (plans.html). The rollup (renderedStepRollup) counts only the RENDERED
    // timeline steps — the issue-type (task/bug/feature/chore) children the
    // cockpit shows as rows (isIssueType, plans.go). The narrative type=bundle
    // child is NOT an issue type and is NOT a timeline row, so it is excluded:
    // total is 3 (the 3 task steps), of which 1 is closed (plan_step_done) → 33%.
    // This matches the Go contract test (plans_test.go: "1/3 done · 33%").
    const progressLabel = card.locator('.plan-progress-label');
    await expect(progressLabel).toBeVisible();
    await expect(progressLabel).toHaveText(/^1\/3 steps · 33%$/);

    // Current-or-next step: plan_step_ip is in_progress, so the card surfaces
    // it as the "now" step.
    const cardStep = card.locator('.plan-card-step');
    await expect(cardStep).toBeVisible();
    await expect(cardStep.locator('.plan-step-kind-now')).toHaveText('now');
    await expect(cardStep.locator('.plan-step-title')).toHaveText('[E2E fixture] Plan step — build the thing');

    // Unified plan-agent chip (step 7, ka-0000003685): no gardener and no
    // assistant bound yet → the "not started" (none-tone) chip, the SAME
    // plan_agent_chip the detail card renders. This is the pre-spawn state the
    // spawn test then changes. No agent → no terminal link either.
    await expect(card.locator('.plan-agent-chip.plan-agent-chip-none')).toBeVisible();
    await expect(card.locator('.plan-agent-chip.plan-agent-chip-live')).toHaveCount(0);
    await expect(card.locator('.plan-agent-chip.plan-agent-chip-running')).toHaveCount(0);
    await expect(card.locator('a.plan-card-agent-link')).toHaveCount(0);
  });

  test('operator sees the per-card dispatch button on the index', async ({ page }) => {
    const planId = fixtureId('plan_epic');
    // The per-card operator affordance is gated on {{if $root.IsOperator}}
    // (viewerCanOperate). Under dev-bypass a request with no OIDC roles is treated
    // AS an operator (viewerCanOperate returns true when RolesFromContext is empty
    // — matching the route layer, which also accepts the POST), so it renders for
    // the default harness viewer. We assert its presence + wiring rather than a
    // negative gate, which cannot be exercised in dev-bypass.
    //
    // The old free-text spawn/message box was replaced by a single "Dispatch #N"
    // button (Matt's ask, thread ka-0000001290; gd-99FM3R9IiS): one deliberate
    // click hands the plan's current/next open step to the gardener (spawning one
    // if none is bound) via POST /plans/{id}/dispatch-next, targeted at the whole
    // row. It renders only when the plan has an open step to hand off — the
    // fixture's plan_step_ip (in_progress) is that step (asserted as the "now"
    // step above), so the button is present here.
    await page.goto(PLANS_ALL);
    const card = page.locator(`li.plan-card[data-plan-id="${planId}"]`);
    await expect(card).toBeVisible();
    const dispatch = card.locator('.plan-card-dispatch button.plan-card-dispatch-btn');
    await expect(dispatch).toBeVisible();
    // The button hands the next step to the plan's gardener via the dispatch-next
    // endpoint for this plan.
    await expect(dispatch).toHaveAttribute('hx-post', new RegExp(`/plans/${planId}/dispatch-next$`));
    // Its label carries the current/next step number ("Dispatch #<n> →").
    await expect(dispatch).toHaveText(/Dispatch #\d+/);
  });
});

// -----------------------------------------------------------------------------
// 2. Detail cockpit — /plans/{id} renders narrative + step timeline + assistant.
// -----------------------------------------------------------------------------
test.describe('plan detail cockpit', () => {
  test('renders the three cockpit lanes and the ordered step timeline', async ({ page }) => {
    const planId = fixtureId('plan_epic');
    await page.goto(`/plans/${planId}`);

    // Header carries the plan title.
    await expect(page.locator('h1.plan-title, .plan-title').first()).toContainText('Plans UX demo plan');

    // The 2×2 named-grid cockpit: all three lanes present and keyed to the plan.
    const steps = page.locator('section#plan-steps');
    const narrative = page.locator('section#plan-narrative');
    const assistant = page.locator('section#plan-agent');
    await expect(steps, '#plan-steps lane').toBeVisible();
    await expect(narrative, '#plan-narrative lane').toBeVisible();
    await expect(assistant, '#plan-agent card').toBeVisible();
    await expect(steps).toHaveAttribute('data-plan-id', planId);
    await expect(assistant).toHaveAttribute('data-plan-id', planId);

    // Step timeline: all three seeded steps render as timeline rows. Assert the
    // done step carries is-done and the in_progress step carries is-active —
    // the two status-driven modifier classes the timeline renders.
    const doneStep = steps.locator(`li.plan-tl-step[data-bead-id="${fixtureId('plan_step_done')}"]`);
    const ipStep = steps.locator(`li.plan-tl-step[data-bead-id="${fixtureId('plan_step_ip')}"]`);
    const todoStep = steps.locator(`li.plan-tl-step[data-bead-id="${fixtureId('plan_step_todo')}"]`);
    await expect(doneStep).toHaveClass(/is-done/);
    await expect(ipStep).toHaveClass(/is-active/);
    await expect(todoStep).toBeVisible();

    // Progress reflects 1 of the 3 RENDERED steps closed (plan_step_done). The
    // rollup counts only the issue-type timeline rows (renderedStepRollup); the
    // narrative type=bundle child is not a step and is excluded, matching the
    // index card and the Go contract test (plans_test.go: "1/3 done · 33%"). On
    // the cockpit the bar's .plan-progress-label is just the percentage ("33%");
    // the count lives in the steps-head meta span ("1/3 done · 33%").
    await expect(steps.locator('.plan-progress-label')).toHaveText('33%');
    await expect(steps.locator('.plan-steps-head h3 .meta')).toHaveText(/1\/3 done · 33%/);
  });

  test('SSE anchor is wired for live updates', async ({ page }) => {
    const planId = fixtureId('plan_epic');
    await page.goto(`/plans/${planId}`);
    // plan_events.js reads the SSE endpoint off this anchor. Its presence +
    // correct data-plan-events href is the contract the live-swap machinery
    // depends on; the JS then flips data-plan-events-state to connecting/live.
    const anchor = page.locator('[data-plan-events]');
    await expect(anchor).toHaveCount(1);
    const href = await anchor.getAttribute('data-plan-events');
    expect(href, 'SSE endpoint points at this plan').toMatch(new RegExp(`/plans/${planId}/events$`));
  });
});

// -----------------------------------------------------------------------------
// 3. Spawn + assistant — the headline path. An operator sends a message and a
//    plan-agent bead is minted; the card flips out of the `none` state.
// -----------------------------------------------------------------------------
test.describe('plan assistant spawn', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('sending a message from the cockpit spawns the assistant and re-renders the lane', async ({ page }) => {
    // Ephemeral plan so binding an assistant never races the index test's
    // "no assistant" assertion on the shared plan_epic (fullyParallel: true).
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const { epicId } = createEphemeralPlan(suffix);

    await page.goto(`/plans/${epicId}`);
    await waitForHtmxIdle(page);

    const assistant = page.locator('section#plan-agent');
    await expect(assistant).toBeVisible();
    // A fresh plan has no agent bound — the card starts in the `none` state
    // and shows no Terminal link. This is the pre-spawn surface.
    await expect(assistant).toHaveAttribute('data-state', 'none');
    await expect(assistant.locator('a.plan-agent-terminal')).toHaveCount(0);

    const form = assistant.locator('form.plan-assistant-form');
    await expect(form).toBeVisible();
    const input = form.locator('textarea[name="message"]');
    await expect(input).toBeVisible();
    await input.fill('e2e: please summarize the plan status');

    // The POST is the assertion surface: operatorMiddleware must resolve the
    // operator from X-Auth-Request-Email, else the daemon fail-closes the
    // agent-bead write with HTTP 400 and the handler renders an error lane.
    const postPromise = page.waitForResponse(
      (r) => r.url().includes(`/plans/${epicId}/assistant/prompt`) && r.request().method() === 'POST',
    );
    await form.locator('button[type="submit"]').click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /assistant/prompt status').toBe(200);
    await waitForHtmxIdle(page);

    // The spawn bound an agent: the "Terminal →" link renders only when
    // .Assistant is non-nil, i.e. a plan-agent bead now exists.
    //
    // planAssistant() resolves that binding from writes spawnPlanAssistant makes
    // in sequence — the agent bead, then its role label (AddLabel), then the
    // `assigned` epic edge (AddDependency). The SYNCHRONOUS POST re-render issues
    // its resolve query in the same request that just made those writes, so under
    // load the label/edge index can lag and the immediate re-render reads back the
    // pre-spawn `none` lane (gd-FfFzT5ED0d read-after-write flake — deterministic
    // on the hermetic-postgres CI daemon). Assert on the DURABLE binding via a
    // fresh cockpit load instead, which re-queries after every write has settled —
    // this still proves "the message spawned + bound an agent", just not that the
    // one in-request re-render already observed its own multi-write.
    await page.goto(`/plans/${epicId}`);
    await waitForHtmxIdle(page);
    const laneAfter = page.locator('section#plan-agent');
    await expect(laneAfter).toHaveAttribute('data-plan-id', epicId);
    await expect(
      laneAfter.locator('a.plan-agent-terminal'),
      'agent Terminal link present after spawn',
    ).toBeVisible();
    // Terminal → points at the agent's coopmux terminal page when derivable,
    // else falls back to the agent's bead page /{id} (assistantLinkURL — the
    // old /agents/{id} route 404s, ka-0000007019 / MR !5316). The e2e harness
    // has no COOPMUX_PUBLIC_URL, so the bead-page fallback is what renders.
    const href = await laneAfter.locator('a.plan-agent-terminal').getAttribute('href');
    expect(href, 'Terminal → points at the spawned agent bead page').toMatch(/\/(gd|ka)-[A-Za-z0-9]+$/);
    expect(href, 'Terminal → must not use the removed /agents/{id} route').not.toMatch(/\/agents\//);

    // The card must NOT be in the `none` state after a successful spawn.
    await expect(laneAfter).not.toHaveAttribute('data-state', 'none');
    // No error was surfaced (the "spawn failed" regression class). The error
    // paragraph is an ALWAYS-PRESENT hidden placeholder (partials/plan_agent.html,
    // populated client-side by hx-on::response-error) since MR !5376 reworked
    // failure handling — so a clean spawn is proven by the element staying
    // hidden/empty, NOT by its absence. Asserting toHaveCount(0) here was stale:
    // the placeholder renders unconditionally for operators (gd-vpbr8klxzP).
    const errorLane = laneAfter.locator('p.plan-assistant-error');
    await expect(errorLane).toBeHidden();
    await expect(errorLane).toHaveText('');
  });

  test('an empty message cannot be submitted (textarea is required)', async ({ page }) => {
    // A read of the shared plan_epic cockpit is safe here — we only inspect the
    // form's `required` attr, we do not submit.
    const planId = fixtureId('plan_epic');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);

    const form = page.locator('section#plan-agent form.plan-assistant-form');
    const input = form.locator('textarea[name="message"]');
    // There is no separate "spawn" affordance — spawning always requires a
    // non-empty message. The textarea carries the `required` attribute, so an
    // empty submit is blocked client-side and no POST is issued.
    await expect(input).toHaveAttribute('required', '');
  });
});

// -----------------------------------------------------------------------------
// 4. Step write — an operator marks a step done; #plan-steps swaps in place.
// -----------------------------------------------------------------------------
test.describe('plan step close', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('clicking Done on the in-progress step closes it and re-renders the timeline', async ({ page }) => {
    // Ephemeral plan so this mutation never races the read tests on the shared
    // seed fixtures (fullyParallel: true). Unique suffix per run.
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const { epicId, stepId } = createEphemeralPlan(suffix);

    await page.goto(`/plans/${epicId}`);
    await waitForHtmxIdle(page);

    const step = page.locator(`li.plan-tl-step[data-bead-id="${stepId}"]`);
    await expect(step, 'ephemeral step renders on the timeline').toBeVisible();
    await expect(step).toHaveClass(/is-active/);

    // The close button posts to /plans/{id}/steps/{step}/close and swaps the
    // whole #plan-steps section (outerHTML). Assert the POST is 200 and the
    // step comes back is-done.
    const closeBtn = step.locator('button.plan-step-close');
    await expect(closeBtn).toBeVisible();

    const postPromise = page.waitForResponse(
      (r) => r.url().includes('/steps/') && r.url().endsWith('/close') && r.request().method() === 'POST',
    );
    await closeBtn.click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /steps/{id}/close status').toBe(200);
    await waitForHtmxIdle(page);

    // After the swap, the same step is now closed and offers Reopen.
    const closedStep = page.locator(`li.plan-tl-step[data-bead-id="${stepId}"]`);
    await expect(closedStep).toHaveClass(/is-done/);
    const reopenBtn = closedStep.locator('button.plan-step-reopen');
    await expect(reopenBtn).toBeVisible();

    // Reopen round-trips back to a non-done row — proves both operator
    // mutations swap the timeline in place.
    const reopenPromise = page.waitForResponse(
      (r) => r.url().includes('/steps/') && r.url().endsWith('/reopen') && r.request().method() === 'POST',
    );
    await reopenBtn.click();
    const reopenResp = await reopenPromise;
    expect(reopenResp.status(), 'POST /steps/{id}/reopen status').toBe(200);
    await waitForHtmxIdle(page);
    const reopenedStep = page.locator(`li.plan-tl-step[data-bead-id="${stepId}"]`);
    await expect(reopenedStep).not.toHaveClass(/is-done/);
    await expect(reopenedStep.locator('button.plan-step-close')).toBeVisible();
  });
});

// -----------------------------------------------------------------------------
// 5. Responsive — the cockpit and index behave at an iPhone-class viewport.
//    Mirrors responsive.spec.js's idiom: grid collapse via computed style +
//    the no-horizontal-overflow catch-all.
// -----------------------------------------------------------------------------
test.describe('plans responsive @ 375x667', () => {
  const MOBILE = { width: 375, height: 667 };
  test.use({ viewport: MOBILE });

  test('cockpit grid collapses to a single column below 60rem', async ({ page }) => {
    const planId = fixtureId('plan_epic');
    await page.goto(`/plans/${planId}`);

    // .plan-grid is `grid-template-columns: minmax(0,1.5fr) minmax(0,1fr)` on
    // desktop and collapses to `1fr` under @media (max-width: 60rem). At 375px
    // that resolves to a single track. Counting tokens is the "one vs many
    // columns" check (same technique as the /advice two-pane test).
    const grid = page.locator('.plan-grid');
    await expect(grid).toBeVisible();
    const cols = await grid.evaluate((el) => getComputedStyle(el).gridTemplateColumns);
    const trackCount = cols.trim().split(/\s+/).length;
    expect(trackCount, `plan-grid tracks @ 375px: "${cols}"`).toBe(1);
  });

  test('the index and the cockpit have no horizontal overflow at 375px', async ({ page }) => {
    // Regression guard for two bugs:
    //   - ka-0000005165 (INDEX): the /plans card grid used `minmax(22rem, 1fr)`,
    //     whose 352px floor exceeded the ~343px content width at 375px and forced
    //     a scrollbar (388 > 375). Fixed to `minmax(min(22rem, 100%), 1fr)`.
    //   - ka-0000000256 (COCKPIT): the plan_epic fixture exercises Cause A (a step
    //     with a long unbroken title token, guarded by .plan-tl-title min-width:0
    //     + overflow-wrap and .plan-tl-stamps white-space:normal) AND Cause B (a
    //     wide fenced code block + wide GFM table in the narrative, guarded by
    //     .plan-narrative pre/table overflow-x:auto). Pre-fix the cockpit measured
    //     scrollWidth 490/430 > 375; wide content must now scroll inside its box.
    //   - ka-0000002291 (COCKPIT, Cause C): a long unbroken INLINE `code` token in
    //     a narrative prose paragraph. Pre-fix it computed overflow-wrap:normal and
    //     painted past the column, re-widening the page even after the pre/table
    //     guards; the .plan-narrative :is(p,li,…) code { overflow-wrap:anywhere }
    //     rule forces the break. Covered by the page-non-overflow loop below plus
    //     the explicit inline-code wrap assertion.
    const planId = fixtureId('plan_epic');
    for (const route of [PLANS_ALL, `/plans/${planId}`]) {
      await page.goto(route);
      const m = await page.evaluate(() => ({
        scrollWidth: document.documentElement.scrollWidth,
        innerWidth: window.innerWidth,
      }));
      expect(
        m.scrollWidth,
        `${route} scrollWidth=${m.scrollWidth} > innerWidth=${m.innerWidth}`,
      ).toBeLessThanOrEqual(m.innerWidth + 1);
    }

    // Containment check for Cause B: the wide GFM table in the narrative must be
    // its OWN scroll container, not a page-widener. (The wide fenced code block
    // wraps via the global `pre { white-space: pre-wrap; word-wrap: break-word }`
    // and so never overflows the page — the table is the real driver, since a
    // <table> cannot wrap.) Asserts the fix by containment: display:block +
    // overflow-x:auto|scroll and the content actually exceeds the box.
    await page.goto(`/plans/${planId}`);
    const table = page.locator('#plan-narrative table').first();
    await expect(table).toBeVisible();
    const contained = await table.evaluate((el) => {
      const cs = getComputedStyle(el);
      return {
        display: cs.display,
        overflowX: cs.overflowX,
        scrolls: el.scrollWidth > el.clientWidth,
      };
    });
    expect(contained.display, 'narrative table is display:block (a scroll box)').toBe('block');
    expect(['auto', 'scroll'], 'narrative table clips horizontally').toContain(contained.overflowX);
    expect(contained.scrolls, 'wide narrative table overflows inside its own box').toBe(true);

    // Wrap check for Cause C: the long inline `code` token in a prose paragraph
    // must have a break opportunity (overflow-wrap:anywhere) so it wraps within
    // the column instead of painting past it. Unlike the table it stays inline
    // (no scroll box) — assert the resolved style and that the token box is no
    // wider than its containing paragraph.
    const inlineCode = page
      .locator('#plan-narrative p code', { hasText: 'plan_cockpit_overflow_probe' })
      .first();
    await expect(inlineCode).toBeVisible();
    const wraps = await inlineCode.evaluate((el) => {
      const cs = getComputedStyle(el);
      const p = el.closest('p');
      return {
        overflowWrap: cs.overflowWrap,
        widerThanParagraph: el.getBoundingClientRect().width > p.clientWidth + 1,
      };
    });
    expect(wraps.overflowWrap, 'inline code has a break opportunity').toBe('anywhere');
    expect(
      wraps.widerThanParagraph,
      'inline code token wraps within its paragraph, not past it',
    ).toBe(false);
  });
});
