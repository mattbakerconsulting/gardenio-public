// From-scratch plan authoring — behavioral coverage (task 4b, gd-QmNG6n3Fe6;
// plan-of-record ka-0000005794 Phase 4). Mirrors the /plans e2e pattern
// (plans.spec.js) but covers the FOUR authoring surfaces plans.spec.js does not:
//
//   1. CREATE verb — the "+ New plan" form POSTs a one-line goal to POST /plans,
//      which mints a type=epic (born-on plan + project labels + plan_state=draft)
//      and 303-redirects into its cockpit (plans_create.go handleCreatePlan).
//   2. TRANSCRIPT — the persistent chat log: durable type=message turn beads
//      bound to a plan by fields.plan_id render through the shared bead_chat
//      direction-bubble partial, operator turns on the own side, assistant turns
//      on the other (plan_transcript_spike.go / plan_agent.html).
//   3. INLINE DECISION ANSWER — a clarifying-question decision rendered in the
//      #plan-decisions section and answered IN PLACE by clicking an option chip,
//      which POSTs the plan-scoped resolve route and swaps the section (task
//      ka-0000003157 / plan_decisions.html).
//   4. APPROVE→BUILD GATE — a ready plan with a bound gardener surfaces the single
//      deliberate "Approve & build" lever; the POST flips plan_state ready→building
//      and enables the gardener in one gesture (plans_lifecycle.go handlePlanBuild).
//
// AUTH + SCOPING mirror plans.spec.js verbatim: dev-bypass makes a request
// "operator" iff it carries X-Auth-Request-Email; the write tests set it. The
// /plans index default-scopes to the viewer's own operated_by, so every
// navigation to the index uses `/plans?operated_by=` (the empty-key "show all"
// form) to render the seeded plans regardless of viewer identity.
//
// SHARED-FIXTURE SAFETY: under fullyParallel:true, tests that MUTATE seeded state
// (answering the shared question, building the shared plan) would race the read
// tests. So each mutation test mints its OWN ephemeral plan/decision (mirroring
// plans.spec.js's createEphemeralPlan and decision_resolve.spec.js's
// createEphemeralDecision) and touches only its own beads. Leftovers carry a
// fixture:e2e:plan_auth_* label so a run's detritus is debuggable.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';
import { oidcSession, requireOIDCStub } from './_oidc.js';

// -----------------------------------------------------------------------------
// kd helpers — shell out to build ephemeral, isolated beads per mutation test
// (mirrors plans.spec.js's kd() / kdCreateJSON()).
// -----------------------------------------------------------------------------
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

// createEphemeralQuestion mints a plan epic + one clarifying-question decision
// wired as a parent-child child of it, so the inline resolve/dismiss membership
// check (loadPlanDecision) passes. Returns { epicId, decId } so the test targets
// only its own beads. The epic carries the plan + project labels so it renders
// on the cockpit; the decision carries two options so the answer chips render.
function createEphemeralQuestion(suffix) {
  const epicId = kdCreateJSON([
    'create',
    `[E2E ephemeral] authoring plan ${suffix}`,
    '--type', 'epic',
    '--label', 'plan',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_auth_eph_${suffix}`,
  ]);
  const options = JSON.stringify([
    { id: 'step', short: 'Dock to step', label: 'Anchor to the step row', artifact_type: 'plan' },
    { id: 'narrative', short: 'Dock to narrative', label: 'Anchor to the narrative', artifact_type: 'plan' },
  ]);
  const decId = kdCreateJSON([
    'create',
    `[E2E ephemeral] clarifying question ${suffix}`,
    '--type', 'decision',
    '--label', `fixture:e2e:plan_auth_eph_q_${suffix}`,
    '--field', 'prompt=Which surface should the tray dock to?',
    '--field', `options=${options}`,
  ]);
  // The decision must be a DIRECT parent-child child of the plan epic — the
  // membership guard reads the decision's forward parent-child edge.
  kd(['dep', 'add', decId, epicId, '--type', 'parent-child']);
  return { epicId, decId };
}

// createEphemeralReadyPlan mints a plan epic in plan_state=ready WITH a bound
// gardener, so the approve→build lever renders and the build POST has a gardener
// to enable. Returns { epicId, gardenerId }.
function createEphemeralReadyPlan(suffix) {
  const epicId = kdCreateJSON([
    'create',
    `[E2E ephemeral] ready plan ${suffix}`,
    '--type', 'epic',
    '--label', 'plan',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_auth_ready_${suffix}`,
    '--field', 'plan_state=ready',
  ]);
  const gardenerSpec = JSON.stringify({
    name: `E2E ephemeral gardener ${suffix}`,
    terminal_goal: 'Build the ready plan.',
    max_runs: 3,
  });
  const gardenerId = kdCreateJSON([
    'create',
    `[E2E ephemeral] plan gardener ${suffix}`,
    '--type', 'trigger',
    '--label', 'trigger:schedule',
    '--label', 'role:gardener',
    '--label', 'plan-gardener',
    '--label', 'project:plansux',
    // gardener-epic:<epicId> is what planGardener() resolves the binding by.
    '--label', `gardener-epic:${epicId}`,
    '--label', `fixture:e2e:plan_auth_cap_${suffix}`,
    '--field', 'source=schedule',
    '--field', 'action=spawn_agent',
    // project FIELD (not just the label) is what the controller reads to
    // derive BOAT_PROJECT / the OAuth secret / the pod git clone; the daemon
    // rejects a project-less agent-spawning trigger (ka-0000000254).
    '--field', 'project=plansux',
    '--field', 'cron=17 * * * *',
    '--field', 'enabled=false',
    '--field', `gardener=${gardenerSpec}`,
  ]);
  return { epicId, gardenerId };
}

// Operator identity for the write tests (any non-empty email flips dev-bypass
// into operator mode; the value only has to be stable).
const OPERATOR_EMAIL = 'e2e-plan-authoring@example.test';

// "Show all plans" — the empty operated_by key (see plans.spec.js PLANS_ALL).
const PLANS_ALL = '/plans?operated_by=';

// -----------------------------------------------------------------------------
// 1. CREATE verb — the "+ New plan" compose form mints a plan and lands the
//    operator in its live cockpit (plans_create.go, task 3a/3b).
// -----------------------------------------------------------------------------
test.describe('plan create verb', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('the New-plan form renders with the goal + project fields for an operator', async ({ page }) => {
    await page.goto(PLANS_ALL);

    // The "+ New plan" disclosure holds the shared compose form (planNewForm).
    // Open it, then assert the form's wiring: a native POST to /plans carrying a
    // required goal textarea (capped at maxPlanGoalLen) + a project selector.
    const disclosure = page.locator('details.plan-new');
    await expect(disclosure).toBeVisible();
    await disclosure.locator('summary.plan-new-btn').click();

    const form = disclosure.locator('form.plan-new-form');
    await expect(form).toBeVisible();
    await expect(form).toHaveAttribute('method', 'post');
    await expect(form).toHaveAttribute('action', /\/plans$/);
    const goal = form.locator('textarea[name="goal"]');
    await expect(goal).toBeVisible();
    await expect(goal).toHaveAttribute('required', '');
    await expect(goal).toHaveAttribute('maxlength', '2000');
    await expect(form.locator('select[name="project"]')).toBeVisible();
  });

  test('submitting a goal mints a plan and 303-redirects into its cockpit', async ({ page }) => {
    await page.goto(PLANS_ALL);
    const disclosure = page.locator('details.plan-new');
    await disclosure.locator('summary.plan-new-btn').click();
    const form = disclosure.locator('form.plan-new-form');

    const goalText = `E2E create: author an inline critique tray ${Date.now()}`;
    await form.locator('textarea[name="goal"]').fill(goalText);
    // Pin the project selector to the isolated plansux project (present because
    // the plan fixtures seed project:plansux beads) so the new plan never lands
    // in project:gardenio.
    await form.locator('select[name="project"]').selectOption('plansux');

    // The native POST-redirect-GET (POST /plans → 303 → GET /plans/{id}) lands
    // in the minted plan's cockpit. Assert the POST returned a 303 (the
    // POST-redirect-GET contract that stops a refresh re-submitting) and that the
    // browser followed it to a real cockpit URL.
    const respPromise = page.waitForResponse(
      (r) => r.url().endsWith('/plans') && r.request().method() === 'POST',
    );
    await Promise.all([
      page.waitForURL(/\/plans\/(gd|ka)-[A-Za-z0-9]+$/),
      form.locator('button[type="submit"]').click(),
    ]);
    const resp = await respPromise;
    expect(resp.status(), 'POST /plans is a 303 See Other (POST-redirect-GET)').toBe(303);

    // We are in the minted plan's cockpit: the goal became the epic title (the
    // header) and description, and the three cockpit lanes render.
    const planUrl = page.url();
    const planId = planUrl.split('/').pop();
    expect(planId, 'redirect target is a kd- plan id').toMatch(/^(gd|ka)-[A-Za-z0-9]+$/);
    await expect(page.locator('h1.plan-title, .plan-title').first()).toContainText('E2E create: author an inline critique tray');
    await expect(page.locator('section#plan-steps')).toBeVisible();
    await expect(page.locator('section#plan-agent')).toBeVisible();

    // Born-on lifecycle: the create verb stamps plan_state=draft, so the cockpit
    // renders the draft lifecycle chip (plan_agent.html {{if .LifecycleGated}}).
    // This is the gate that a browser-created plan enters the authoring lifecycle
    // at mint time (plans_create.go; the 4a dogfood fix MR !5446).
    await expect(page.locator('section#plan-agent .plan-lifecycle-chip-draft')).toBeVisible();
    // The draft plan shows "Mark ready", never "Approve & build" (that needs ready).
    await expect(page.locator('.plan-lifecycle-ready')).toBeVisible();
    await expect(page.locator('.plan-lifecycle-approve')).toHaveCount(0);

    // Clean up the minted plan so runs don't accumulate open plans.
    kd(['close', planId]);
  });
});

// -----------------------------------------------------------------------------
// 1b. NARRATIVE LANE — the ready plan's #plan-narrative renders the bundle's
//     doc-child prose (plan_ready_bundle → plan_ready_bundle_doc), exactly as
//     plan_epic's cockpit does via plan_bundle_doc. Guards that the authoring
//     cockpit surfaces plan prose, not just the epic-description fallback.
// -----------------------------------------------------------------------------
test.describe('plan narrative lane', () => {
  test('the ready plan cockpit renders the bundle doc-child prose in the narrative lane', async ({ page }) => {
    const planId = fixtureId('plan_ready_epic');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);

    // The narrative lane is present and scoped to this plan.
    const narrative = page.locator('section#plan-narrative');
    await expect(narrative, '#plan-narrative lane present').toBeVisible();

    // The seeded doc-child body (plan_ready_bundle_doc) renders here. Scope to the
    // rendered prose <p> — in operator mode the lane also mounts an inline editor
    // <textarea> carrying the same raw markdown, so a bare getByText matches twice
    // (strict-mode violation). The rendered paragraph is the RENDERED-prose proof.
    // The unique marker proves the bundle → doc body reached the lane, not the
    // epic-description fallback (planProse walks the bundle's doc children).
    await expect(
      narrative.locator('p', { hasText: 'e2e-ready-plan-narrative-marker' }),
      'ready-plan narrative doc-child body renders in the lane',
    ).toBeVisible();
    await expect(narrative.locator('h1', { hasText: 'Inline critique tray' })).toBeVisible();
  });
});

// -----------------------------------------------------------------------------
// 2. TRANSCRIPT — the persistent chat log renders the seeded operator +
//    assistant turns through the shared direction-bubble partial (task 2a/2b).
// -----------------------------------------------------------------------------
test.describe('plan transcript', () => {
  test('the cockpit renders both transcript turns with the operator/agent split', async ({ page }) => {
    const planId = fixtureId('plan_ready_epic');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);

    // The transcript lives inside the ONE #plan-agent card, rendered by the
    // shared bead_chat partial (self-gates on HasMessages, so its presence
    // proves turns were collected by fields.plan_id).
    const chat = page.locator('section#plan-agent .bead-chat');
    await expect(chat, 'transcript chat panel present (turns collected by plan_id)').toBeVisible();

    // Both turn bodies render (messageBodyText reads fields.text).
    const opMsg = chat.locator('.bead-chat-msg', { hasText: 'Draft me a plan to add an inline critique tray' });
    const asstMsg = chat.locator('.bead-chat-msg', { hasText: 'researching the existing critique routes' });
    await expect(opMsg).toBeVisible();
    await expect(asstMsg).toBeVisible();

    // Direction split: operator turn on the own side (dir-from-user), assistant
    // turn on the other side (dir-from-agent) — the planTurnDirection override
    // off fields.turn_role.
    await expect(opMsg).toHaveClass(/dir-from-user/);
    await expect(asstMsg).toHaveClass(/dir-from-agent/);

    // Oldest→newest chat-log order: the operator turn precedes the assistant turn.
    const opBox = await opMsg.boundingBox();
    const asstBox = await asstMsg.boundingBox();
    expect(opBox && asstBox && opBox.y < asstBox.y, 'operator turn renders above the assistant turn').toBe(true);
  });

  test('sending a message from the compose box persists an operator turn', async ({ page }) => {
    // This is the headline regression for the 4b daemon fix: mintOperatorTurn
    // writes a type=message bead carrying fields.plan_id + turn_role, which the
    // builtin type:message config rejected until 4b — silently dropping the turn.
    // Driving the real compose form proves the turn now persists AND renders.
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const epicId = kdCreateJSON([
      'create',
      `[E2E ephemeral] transcript plan ${suffix}`,
      '--type', 'epic',
      '--label', 'plan',
      '--label', 'project:plansux',
      '--label', `fixture:e2e:plan_auth_msg_${suffix}`,
    ]);

    // Operator context so the compose form renders and the POST is accepted.
    await page.setExtraHTTPHeaders({ 'X-Auth-Request-Email': OPERATOR_EMAIL });
    await page.goto(`/plans/${epicId}`);
    await waitForHtmxIdle(page);

    const form = page.locator('section#plan-agent form.plan-assistant-form');
    await expect(form).toBeVisible();
    const marker = `e2e-transcript-persist-${suffix}`;
    await form.locator('textarea[name="message"]').fill(marker);

    // The POST both mints the operator turn AND (best-effort) spawns the
    // assistant. We assert on the turn's PERSISTENCE via a fresh page load, not
    // the immediate re-render, so we prove the daemon accepted the write (the
    // fix), not just that the handler ran.
    const postPromise = page.waitForResponse(
      (r) => r.url().includes(`/plans/${epicId}/assistant/prompt`) && r.request().method() === 'POST',
    );
    await form.locator('button[type="submit"]').click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /assistant/prompt status').toBe(200);
    await waitForHtmxIdle(page);

    // Reload the cockpit: the operator turn must now be a persisted transcript
    // row (read back by planTranscriptMessagesQuery on fields.plan_id). Before
    // the 4b config fix the mint 400'd and this turn never appeared.
    await page.goto(`/plans/${epicId}`);
    await waitForHtmxIdle(page);
    const chat = page.locator('section#plan-agent .bead-chat');
    await expect(chat, 'transcript panel present after the operator turn persisted').toBeVisible();
    await expect(
      chat.locator('.bead-chat-msg', { hasText: marker }),
      'the sent operator message persisted as a transcript turn',
    ).toBeVisible();

    kd(['close', epicId]);
  });
});

// -----------------------------------------------------------------------------
// 3. INLINE DECISION ANSWER — a clarifying question rendered in #plan-decisions
//    and answered in place by clicking an option chip (task ka-0000003157).
// -----------------------------------------------------------------------------
test.describe('plan inline decision', () => {
  test('an operator sees the clarifying question with inline answer chips', async ({ page }) => {
    const planId = fixtureId('plan_ready_epic');
    // Operator so the inline resolve chips render (gated on $.IsOperator).
    await page.setExtraHTTPHeaders({ 'X-Auth-Request-Email': OPERATOR_EMAIL });
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);

    const section = page.locator('section#plan-decisions');
    await expect(section, '#plan-decisions section present').toBeVisible();

    // The seeded question renders in place with its prompt + one option form per
    // option, each posting the plan-scoped resolve route.
    const question = section.locator('li.plan-decision', {
      hasText: 'Which surface should the critique tray dock to',
    });
    await expect(question).toBeVisible();
    const optForms = question.locator('form.plan-decision-opt-form');
    await expect(optForms).toHaveCount(2);
    await expect(optForms.first()).toHaveAttribute('hx-post', new RegExp(`/plans/${planId}/decisions/(gd|ka)-[A-Za-z0-9]+/resolve$`));
    await expect(optForms.first()).toHaveAttribute('hx-target', '#plan-decisions');
    // The free-text "Other answer" + Dismiss forms live under the disclosure.
    await expect(question.locator('textarea[name="response_text"]')).toHaveCount(1);
    await expect(question.locator('form.plan-decision-dismiss')).toHaveCount(1);
  });

  // Answering needs a human session: kbeads refuses a decision answer from the
  // shared static token the dev-bypass viewer writes with (gd-Du6wHclx4g). So
  // this one test runs on the OIDC-mode viewer with a harness-issued bearer
  // (tests/_oidc.js, gd-ZTu68qjfEC).
  test.describe('as an OIDC operator', () => {
    requireOIDCStub(test);
    test.use(oidcSession(OPERATOR_EMAIL, ['gardenio-operator']));

    test('clicking an option chip answers the question in place and swaps the section', async ({ page }) => {
      // Ephemeral plan + question so answering never races the shared read test.
      const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
      const { epicId, decId } = createEphemeralQuestion(suffix);

      // Auto-accept the hx-confirm ("Answer with …?") dialog.
      page.on('dialog', (d) => d.accept());
      await page.goto(`/plans/${epicId}`);
      await waitForHtmxIdle(page);

      const section = page.locator('section#plan-decisions');
      const question = section.locator(`li.plan-decision[data-bead-id="${decId}"]`);
      await expect(question, 'ephemeral question renders open').toBeVisible();
      await expect(question).not.toHaveClass(/plan-decision-resolved/);
      // Header chip shows one open question.
      await expect(section).toHaveAttribute('data-open-decisions', '1');

      // Click the "step" option's submit button → POSTs resolve → swaps #plan-decisions.
      const stepForm = question.locator('form.plan-decision-opt-form', { has: page.locator('input[value="step"]') });
      const postPromise = page.waitForResponse(
        (r) => r.url().includes(`/plans/${epicId}/decisions/${decId}/resolve`) && r.request().method() === 'POST',
      );
      await stepForm.locator('button[type="submit"]').click();
      const resp = await postPromise;
      expect(resp.status(), 'inline resolve POST status').toBe(200);
      await waitForHtmxIdle(page);

      // After the swap the question flips to its resolved skin: the answered
      // question carries plan-decision-resolved, shows the "Answered — chose step"
      // banner, and the open count drops to zero.
      const answered = page.locator(`section#plan-decisions li.plan-decision[data-bead-id="${decId}"]`);
      await expect(answered).toHaveClass(/plan-decision-resolved/);
      await expect(answered.locator('.plan-decision-outcome')).toContainText('Answered');
      await expect(answered.locator('.plan-decision-outcome code', { hasText: /^step$/ })).toBeVisible();
      await expect(page.locator('section#plan-decisions')).toHaveAttribute('data-open-decisions', '0');
      // The answered question no longer offers resolve chips (CanResolve gates on !Resolved).
      await expect(answered.locator('form.plan-decision-opt-form')).toHaveCount(0);

      kd(['close', epicId]);
    });
  });
});

// -----------------------------------------------------------------------------
// 4. APPROVE→BUILD GATE — a ready plan with a bound gardener surfaces the single
//    "Approve & build" lever; the POST flips ready→building + enables the
//    gardener in one gesture (plans_lifecycle.go handlePlanBuild).
// -----------------------------------------------------------------------------
test.describe('plan approve → build gate', () => {
  test('a ready plan with a bound gardener shows the Approve & build lever', async ({ page }) => {
    const planId = fixtureId('plan_ready_epic');
    await page.setExtraHTTPHeaders({ 'X-Auth-Request-Email': OPERATOR_EMAIL });
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);

    const card = page.locator('section#plan-agent');
    // The ready lifecycle chip renders (draft→ready→building), and because a
    // gardener is bound the deliberate "Approve & build" lever is present.
    await expect(card.locator('.plan-lifecycle-chip-ready')).toBeVisible();
    const approve = card.locator('.plan-lifecycle-approve');
    await expect(approve).toBeVisible();
    await expect(approve).toHaveAttribute('hx-post', new RegExp(`/plans/${planId}/build$`));
    await expect(approve).toHaveAttribute('hx-target', '#plan-agent');
    // "Keep editing" (reopen to draft) is always offered on a ready plan.
    await expect(card.locator('.plan-lifecycle-edit')).toBeVisible();
    // A ready plan is not building yet, so no "Approved — the bound gardener is
    // building" note.
    await expect(card).toHaveAttribute('data-plan-state', 'ready');
  });

  test('clicking Approve & build flips the plan to building and enables the gardener', async ({ page }) => {
    // Ephemeral ready plan + gardener so approving never mutates the shared fixture.
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const { epicId } = createEphemeralReadyPlan(suffix);

    page.on('dialog', (d) => d.accept()); // hx-confirm "Approve this plan…?"
    await page.setExtraHTTPHeaders({ 'X-Auth-Request-Email': OPERATOR_EMAIL });
    await page.goto(`/plans/${epicId}`);
    await waitForHtmxIdle(page);

    const card = page.locator('section#plan-agent');
    await expect(card).toHaveAttribute('data-plan-state', 'ready');
    const approve = card.locator('.plan-lifecycle-approve');
    await expect(approve).toBeVisible();

    const postPromise = page.waitForResponse(
      (r) => r.url().includes(`/plans/${epicId}/build`) && r.request().method() === 'POST',
    );
    await approve.click();
    const resp = await postPromise;
    expect(resp.status(), 'POST /build status').toBe(200);
    await waitForHtmxIdle(page);

    // The card swapped in place (hx-target=#plan-agent): the plan is now building.
    const built = page.locator('section#plan-agent');
    await expect(built).toHaveAttribute('data-plan-state', 'building');
    await expect(built.locator('.plan-lifecycle-chip-building')).toBeVisible();
    // The building state retires the approve/mark-ready levers (they only render
    // for draft/ready) and shows the "Approved — the bound gardener is building"
    // note instead.
    await expect(built.locator('.plan-lifecycle-approve')).toHaveCount(0);
    await expect(built.locator('.plan-lifecycle-ready')).toHaveCount(0);
    await expect(built.locator('.plan-lifecycle-note')).toContainText('building');

    // Verify the daemon state actually flipped (not just the render): the epic's
    // plan_state field is now "building".
    const epicJSON = kd(['show', epicId, '--json']);
    const epic = JSON.parse(epicJSON.slice(epicJSON.search(/[{[]/)));
    expect(epic.fields?.plan_state, 'epic plan_state is building on the daemon').toBe('building');

    kd(['close', epicId]);
  });

  test('the Approve & build lever is a deliberate, confirmed gate', async ({ page }) => {
    // The gate is "deliberate" (ExitPlanMode-analogous, §6.4): the lever carries
    // an hx-confirm so a stray click never silently starts a build. Assert that
    // guard is wired — it is the difference between a build gate and a plain
    // toggle. (The negative operator gate — a read-only viewer sees no lever — is
    // covered by the Go unit tests plans_gardener_test.go / plans_lifecycle_test.go;
    // dev-bypass treats a role-less request AS operator, so it cannot be
    // exercised here.)
    const planId = fixtureId('plan_ready_epic');
    await page.setExtraHTTPHeaders({ 'X-Auth-Request-Email': OPERATOR_EMAIL });
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);
    const approve = page.locator('section#plan-agent .plan-lifecycle-approve');
    await expect(approve).toBeVisible();
    await expect(approve).toHaveAttribute('hx-confirm', /Approve this plan/);
  });
});
