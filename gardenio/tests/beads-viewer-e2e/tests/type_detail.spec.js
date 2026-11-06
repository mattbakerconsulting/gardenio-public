// Phase T5 (gd-mBAiw7o92M) — per-type detail templates: agent, advice.
// Renders each fixture bead and asserts the type-specific partial
// (`templates/partials/type_{agent,advice}.html`) shows the fields the
// partial is built around. Catches regressions like:
//
//   - agent_state badge missing or class typo (.agent-state-<state>)
//   - advice scope renders empty / always says "global" regardless of
//     `advice:project:<name>` label
//   - description markdown body stops rendering as HTML
//
// The original `type_schedule` describe block was retired in
// gd-vK3RKOkRPW: Phase 3b/D10 hard-removed `kd create --type=schedule`
// (kbeads/cmd/kd/schedule_lint.go), so the schedule fixture is now
// seeded as type=trigger label=trigger:schedule and is exercised via
// the /triggers listing suite (tests/schedules.spec.js) instead.
// type_schedule.html now has no real bead that can route to it on
// bead-show; behavioral coverage for that partial is retired with the
// underlying primitive.
//
// Sibling phase T6 (kd-...) will cover decision/formula/mr/generic in
// the same shape — keep this file scoped to agent/advice so failures
// point at one partial.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

test.describe('type_agent', () => {
  test('renders name, agent_state and pod_phase, and links current task', async ({ page }) => {
    const id = fixtureId('agent');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const card = page.locator('section.agent-card');
    await expect(card).toBeVisible();
    await expect(card.locator('h3')).toHaveText('Agent');

    // The agent terminal phase (gd-T-series) replaced the dt/dd
    // field-list with a chips-style `.agent-header-row`: each field
    // renders as its own `<span class="agent-<field>">` (some wrap
    // their value in `<code>`). Assert the per-field span exists with
    // the seeded value; class hooks (e.g. .agent-state-idle) are still
    // pinned so a CSS-classname regression is caught.

    // Name field — fixture seeds `agent=fixture-agent`. Rendered as
    // `<span class="agent-name"><code>...</code></span>`.
    await expect(card.locator('.agent-name code')).toHaveText('fixture-agent');

    // agent_state — fixture seeds `agent_state=idle`. Rendered as
    // `<span class="agent-state-pill agent-state-idle">idle</span>`.
    // Asserting both the value and the class hook pins template
    // rendering AND the CSS classname contract.
    const statePill = card.locator('.agent-state-pill');
    await expect(statePill).toHaveText('idle');
    await expect(statePill).toHaveClass(/agent-state-idle/);

    // pod_phase — fixture seeds `pod_phase=running`. Rendered as
    // `<span class="agent-pod-phase">running</span>`.
    await expect(card.locator('.agent-pod-phase')).toHaveText('running');

    // task_id — fixture seeds `task_id=gd-fixtureTaskId`. Rendered as
    // `<span class="agent-task">task <a href="/gd-fixtureTaskId"><code>gd-fixtureTaskId</code></a></span>`.
    // BasePath is "" under the E2E webServer, so href is "/<id>".
    const taskLink = card.locator('.agent-task a');
    await expect(taskLink).toHaveAttribute('href', /gd-fixtureTaskId$/);
    await expect(taskLink.locator('code')).toHaveText('gd-fixtureTaskId');
  });
});

test.describe('type_advice', () => {
  test('renders project scope and the markdown description body', async ({ page }) => {
    const id = fixtureId('advice');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const card = page.locator('section.advice-card');
    await expect(card).toBeVisible();
    await expect(card.locator('h3')).toHaveText('Advice');

    // Scope row: the fixture carries label `advice:project:gardenio`,
    // which buildAdviceView in type_views.go turns into a Scopes entry
    // (along with the `fixture:e2e:advice` seed label). Each scope is
    // rendered inside its own <code>; assert the project-scoped one is
    // present. The fixture has at least one scope, so the literal
    // "global" fallback branch must not fire — that is the regression
    // we want to catch (scope renders but always says global).
    const scopeRow = card.locator('dt', { hasText: 'Scope' }).locator('xpath=following-sibling::dd[1]');
    await expect(scopeRow.locator('code', { hasText: 'advice:project:gardenio' })).toBeVisible();
    // The bare "global" fallback only renders when Scopes is empty.
    // A <code>global</code> with exact text would mean the partial
    // hit the fallback branch despite labels being present.
    await expect(scopeRow.locator('code', { hasText: /^global$/ })).toHaveCount(0);

    // Markdown body: the description (seeded with **Why:** /
    // **How to apply:** markers) lands in `.description` after passing
    // through the markdown renderer — bold runs become <strong>, so
    // assert the strong text rather than the raw asterisks.
    const description = page.locator('div.description').first();
    await expect(description).toBeVisible();
    await expect(description.locator('strong', { hasText: /^Why:$/ })).toBeVisible();
    await expect(description.locator('strong', { hasText: /^How to apply:$/ })).toBeVisible();
    await expect(description).toContainText('markdown pipeline');
  });
});
