// /plans card + cockpit SIGNAL-STRIP coverage backfill (task ka-0000000865,
// coverage-gap sprint cycle 16; parent epic ka-0000000958).
//
// NINE viewer MRs in the 7-day window ending 2026-08-05 shipped new rendered CSS
// hooks onto the /plans index card + cockpit crew roster with ZERO asserted
// coverage in gardenio/tests/. plans.spec.js LOOKS like it covers the plans
// surface, but every selector it asserts (.plan-progress-label, .plan-step-kind-*,
// .plan-agent-chip-*) is PRE-window — the name-match≠coverage trap. This spec
// closes the render leg for the six signals that were HEAD-verified zero-coverage
// and still had live runtime hooks at HEAD (gardener cycle-23 re-verification,
// 2026-08-05 15:09Z):
//
//   1. .plan-progress-seg          — segmented progress bar (ka-0000007226):
//                                     done/in-progress/blocked/not-started buckets.
//   2. .plan-prio-chip             — plan priority pill P0/P1/P3/P4 (ka-0000007970),
//                                     rendered only when priority ≠ the P2 baseline.
//   3. .plan-stall-badge-reason    — the ⚠ stall badge FACE discriminator
//                                     (ka-0000001697): "· no gardener" / "· gardener
//                                     paused" so an operator knows WHAT to fix.
//   4. .plan-card-crew-stuck-age   — oldest OPEN-stuck crew wait-age on the index
//                                     crew chip (ka-0000002016, iter-13).
//   5. .plan-crew-count-spend      — cumulative crew SPEND chip on the COCKPIT
//                                     crew roster (ka-0000004218, iter-12).
//   6. .plans-unhomed              — the global "Unhomed open decisions &
//                                     escalations" backstop section (ka-0000004983).
//
// ANTI-VACUITY (the guardrail this bead's origin task names explicitly): a bare
// `expect(sel).toBeVisible()` passes trivially if the fixture never produced the
// state. Every assertion below pins the VALUE the signal conveys — the stall
// reason TEXT, the spend DOLLARS, the specific unhomed decision's title/link, a
// real duration on the stuck age, and the FOUR distinct progress-segment classes —
// not merely that an element with the class exists.
//
// WHY NOT THE DEAD-AGENT STALL DISCRIMINATION (this bead's 14:42Z addendum). The
// loud "· N dead step(s)" reason requires an in_progress step frozen past the 1h
// planStallDeadAgentGrace with a dead/absent agent (plans_stall.go deadStepCount).
// The daemon stamps updated_at at write time and exposes NO backdating on create
// or update (createBeadInput/updateBeadInput carry no *_at field — verified in
// kbeads/internal/server/beads.go), and the seed runs immediately before the
// tests, so a freshly-seeded step is ALWAYS inside the grace window and can never
// count as dead here. The dead-step reason vocabulary is instead pinned by the Go
// unit tests TestPlanStall / TestPlanStallBadgeLabel (plans_stall_test.go). This
// spec owns the render leg via the SEEDABLE gardener-state reasons, which give a
// genuine, deterministic TWO-fixture discrimination of the SAME FaceLabel channel:
//   (a) open plan, NO gardener bound   → reason text "no gardener".
//   (b) open plan, PAUSED gardener     → reason text "gardener paused".
// Asserting the two distinct texts proves the FaceLabel is data-driven, not a
// static string — the same "distinguish it-worked from it's-always-on" discipline
// the sibling plans_mr_overflow_ci.spec.js applies to its loud-red tint.
//
// LOCAL-RUN NOTE (mirrors plans_mr_overflow_ci.spec.js): this pod has no Postgres
// server binary / pgvector and no docker daemon, and localhost:8080 is the PROD
// daemon (which must never be seeded), so the running-spec leg is verified in CI's
// hermetic `test-beads-viewer-e2e` task (apt postgresql-16 + pgvector → kd serve →
// beads-viewer). See the suite README.
//
// ISOLATION: every plan/crew/decision here is EPHEMERAL (unique suffix per test,
// its own beads labelled fixture:e2e:plan_sig_*), created in the dedicated
// project:plansux — never the shared plan_epic seed fixture — so under
// fullyParallel:true these tests never race the plans.spec.js read tests. AUTH:
// creating beads via `kd` attributes them to BEADS_OPERATED_BY (not the viewer
// identity), and the index reads with `?operated_by=` (the empty-key "show all"
// form) so the plan renders regardless of which identity the viewer authenticates
// as — the same scoping plans.spec.js / plans_mr_overflow_ci.spec.js document.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

// "Show all plans" — the empty operated_by key (see plans.spec.js PLANS_ALL). The
// ephemeral plan is authored as BEADS_OPERATED_BY, not the viewer identity, so the
// default filter-by-me would hide it; the explicit empty key opts into "show all".
const PLANS_ALL = '/plans?operated_by=';

// kd shells out to the CLI (mirrors plans.spec.js / plans_mr_overflow_ci.spec.js)
// to build fixtures against the same daemon the viewer reads. Surfaces the
// daemon's one-line error rather than kd's usage banner on failure.
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
  // seed.mjs / plans.spec.js / plans_mr_overflow_ci.spec.js).
  const i = raw.search(/[{[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) throw new Error(`kd create returned no id: ${raw}`);
  return bead.id;
}

function uniqueSuffix() {
  return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

// createPlanEpic mints an ephemeral plan-labeled epic in project:plansux. priority
// controls whether the P-chip renders (≠2 → renders; kd default is 2). Returns the
// epic id.
function createPlanEpic(suffix, { priority } = {}) {
  const args = [
    'create',
    `[E2E ephemeral] plan signal-strip ${suffix}`,
    '--type', 'epic',
    '--label', 'plan',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_sig_${suffix}`,
  ];
  if (priority !== undefined) args.push('--priority', String(priority));
  return kdCreateJSON(args);
}

// addStep creates one task step under the epic (child→parent parent-child edge,
// matching the seed's step shape) and returns its id. status is applied after
// create; an in_progress step needs an assignee (the kind=issue invariant), so
// the caller passes one when it wants a live/claimed step.
function addStep(epicId, suffix, tag, { status, assignee } = {}) {
  const stepId = kdCreateJSON([
    'create',
    `[E2E ephemeral] plan signal step ${tag} ${suffix}`,
    '--type', 'task',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_sig_step_${tag}_${suffix}`,
  ]);
  kd(['dep', 'add', stepId, epicId, '--type', 'parent-child']);
  if (status && status !== 'open') {
    const upd = ['update', stepId, '--status', status];
    // kind=issue beads reject an in_progress transition without an assignee
    // (checkInProgressInvariant, ka-0000006063). blocked/closed carry no such rule.
    if (status === 'in_progress') upd.push('--assignee', assignee || `e2e-sig-${tag}`);
    kd(upd);
  }
  return stepId;
}

// addCrew mints an ephemeral crew AGENT bead `assigned` to a step — the exact
// linkage planCrewBulk / planCrew walk (crew --assigned--> step; the step's
// REVERSE deps surface the crew). agentState drives the roster bucket
// (working→alive, rate_limited/failed→stuck). costUSD, when set, populates
// total_cost_usd so the spend rollups sum a real figure. Returns the crew id.
function addCrew(stepId, suffix, tag, { agentState, costUSD } = {}) {
  const args = [
    'create',
    `[E2E ephemeral] crew ${tag} ${suffix}`,
    '--type', 'agent',
    // The daemon's type=agent create invariant requires the top-level `role`
    // (the --role flag), so a bare `--field role=crew` is rejected with
    // "type=agent requires fields: [role]". Mirrors the gardenerIteration seed
    // fixtures, which pass role: 'crew' → the --role flag.
    '--role', 'crew',
    '--label', 'project:plansux',
    '--label', `fixture:e2e:plan_sig_crew_${tag}_${suffix}`,
    '--field', `agent=e2e-sig-crew-${tag}-${suffix}`,
    '--field', 'project=plansux',
    '--field', `agent_state=${agentState}`,
  ];
  if (costUSD !== undefined) args.push('--field', `total_cost_usd=${costUSD}`);
  const crewId = kdCreateJSON(args);
  // crew --assigned--> step : the reverse dep of the STEP points at the crew, the
  // edge SpawnAgent writes on a real dispatch (plans_crew.go doc). kd dep add
  // passes the type through verbatim (the sibling MR-overflow spec relies on the
  // same for --type produces), and the daemon accepts any non-empty edge type
  // (model.DependencyType.IsValid).
  kd(['dep', 'add', crewId, stepId, '--type', 'assigned']);
  return crewId;
}

// createPausedGardener binds a PAUSED gardener trigger to a plan epic via the
// gardener-epic:<id> label (gardenerOwnedEpicID) so gardenerStateOf reads "paused":
// an OPEN trigger bead with enabled=false. Mirrors the plan_ready_gardener seed
// fixture's shape (role:gardener + trigger:schedule + a gardener spec), minus the
// plan_state gate. Returns the gardener id.
function createPausedGardener(epicId, suffix) {
  return kdCreateJSON([
    'create',
    `[E2E ephemeral] paused gardener ${suffix}`,
    '--type', 'trigger',
    '--label', 'trigger:schedule',
    '--label', 'role:gardener',
    '--label', 'project:plansux',
    '--label', `gardener-epic:${epicId}`,
    '--label', `fixture:e2e:plan_sig_cap_${suffix}`,
    '--field', 'source=schedule',
    '--field', 'action=spawn_agent',
    '--field', 'cron=17 * * * *',
    '--field', 'project=plansux',
    '--field', 'role=gardener',
    '--field', 'prompt=fixture: paused signal-strip gardener',
    // enabled=false → buildTriggerRow(kind=trigger).Enabled is false → open +
    // disabled → gardenerStateOf returns "paused".
    '--field', 'enabled=false',
    '--field', 'timezone=UTC',
  ]);
}

// -----------------------------------------------------------------------------
// 1. Segmented progress bar — .plan-progress-seg (ka-0000007226).
//    A plan with one step in EACH lifecycle bucket paints all FOUR segment
//    classes; asserting the specific class SET (not a bare seg count) proves the
//    bar partitions by status, the whole point of the segmented bar over the old
//    single green fill.
// -----------------------------------------------------------------------------
test.describe('plans index — segmented progress bar (.plan-progress-seg)', () => {
  test('paints one segment per lifecycle bucket: done + in-progress + blocked + not-started', async ({ page }) => {
    const suffix = uniqueSuffix();
    // Four steps → one in each bucket the bar partitions StepsTotal into.
    const epicId = createPlanEpic(suffix); // priority 2 baseline → no P-chip here.
    // Add the CLOSED step LAST. This fixture epic carries `plan` but no
    // plan_state, so the daemon treats it as a plain epic: closing its only
    // child drains the subtree and auto-closes the epic. The closed-parent
    // guard (ka-0000005756) then 409s every later parent-child dep-add. Keeping
    // ≥1 open/in_progress/blocked sibling in place before the close means the
    // subtree never fully drains, so the epic stays open for all four steps.
    addStep(epicId, suffix, 'ip', { status: 'in_progress' });
    addStep(epicId, suffix, 'blocked', { status: 'blocked' });
    addStep(epicId, suffix, 'todo', { status: 'open' });
    addStep(epicId, suffix, 'done', { status: 'closed' });

    await page.goto(PLANS_ALL);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card, 'ephemeral plan card present on /plans').toBeVisible();

    const bar = card.locator('.plan-progress-bar');
    await expect(bar).toBeVisible();

    // Each bucket renders exactly its own segment (a zero-count bucket renders
    // none — ka-0000007226), so all four classes must be present, one each.
    await expect(bar.locator('.plan-progress-seg-done')).toHaveCount(1);
    await expect(bar.locator('.plan-progress-seg-inprogress')).toHaveCount(1);
    await expect(bar.locator('.plan-progress-seg-blocked')).toHaveCount(1);
    await expect(bar.locator('.plan-progress-seg-todo')).toHaveCount(1);
    // All four are .plan-progress-seg — the shared class the task's audit counts.
    await expect(bar.locator('span.plan-progress-seg')).toHaveCount(4);

    // Anti-vacuity: the bar's aria-label carries the true breakdown, proving the
    // segments reflect real per-bucket counts, not four empty spans. (1 of each.)
    await expect(bar).toHaveAttribute(
      'aria-label',
      '1 done, 1 in progress, 1 blocked, 1 not started of 4 steps',
    );
    // Each segment carries its own count in the hover title — the value channel.
    await expect(bar.locator('.plan-progress-seg-blocked')).toHaveAttribute('title', '1 blocked');
  });
});

// -----------------------------------------------------------------------------
// 2. Priority pill — .plan-prio-chip (ka-0000007970).
//    Rendered ONLY when the plan's priority differs from the P2 baseline. Assert
//    a P0 plan shows the loud chip with the RIGHT tone class + "P0" text, AND a
//    P2 plan shows NO chip — the "add signal, not ink" gate the feature ships on.
// -----------------------------------------------------------------------------
test.describe('plans index — priority pill (.plan-prio-chip)', () => {
  test('a P0 plan renders the loud P0 chip; a P2 baseline plan renders none', async ({ page }) => {
    const suffix = uniqueSuffix();
    const p0Epic = createPlanEpic(`${suffix}-p0`, { priority: 0 });
    addStep(p0Epic, `${suffix}-p0`, 'todo', { status: 'open' });
    const p2Epic = createPlanEpic(`${suffix}-p2`, { priority: 2 });
    addStep(p2Epic, `${suffix}-p2`, 'todo', { status: 'open' });

    await page.goto(PLANS_ALL);

    // P0 plan: the chip renders with the loud P0/P1 tone class and reads "P0".
    const p0Card = page.locator(`li.plan-card[data-plan-id="${p0Epic}"]`);
    await expect(p0Card).toBeVisible();
    const chip = p0Card.locator('.plan-prio-chip');
    await expect(chip).toBeVisible();
    await expect(chip).toHaveClass(/plan-prio-chip-0/);
    await expect(chip).toHaveText('P0');
    await expect(chip).toHaveAttribute('title', /Priority 0/);

    // P2 baseline plan: NO chip (the density gate). This is what proves the chip
    // is priority-driven, not always-on.
    const p2Card = page.locator(`li.plan-card[data-plan-id="${p2Epic}"]`);
    await expect(p2Card).toBeVisible();
    await expect(p2Card.locator('.plan-prio-chip')).toHaveCount(0);
  });
});

// -----------------------------------------------------------------------------
// 3. Stall badge reason — .plan-stall-badge-reason (ka-0000001697).
//    The FaceLabel discriminator. Two fixtures pin the two SEEDABLE reasons so the
//    assertion proves the text is data-driven (see the header note on why the
//    dead-agent reason is not seedable here).
//      (a) open plan, no gardener  → "no gardener"
//      (b) open plan, paused cap  → "gardener paused"
//    Both fixtures have NO live crew — a live crew would clear the badge via
//    reconcileStallWithCrew (plans.go:1119), so the reason must be crew-free.
// -----------------------------------------------------------------------------
test.describe('plans index — stall badge reason (.plan-stall-badge-reason)', () => {
  test('an unwatched open plan with no gardener reads "· no gardener"', async ({ page }) => {
    const suffix = uniqueSuffix();
    // Open step + no gardener + no live agent → deriveStall fires the "none" branch.
    const epicId = createPlanEpic(suffix);
    addStep(epicId, suffix, 'todo', { status: 'open' });

    await page.goto(PLANS_ALL);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card).toBeVisible();

    const badge = card.locator('.plan-stall-badge');
    await expect(badge, 'the ⚠ stall badge renders for an unwatched open plan').toBeVisible();
    // The headline is the loud plan-level "stalled" (not the re-scoped "orphaned",
    // which only applies to a dead-agent stall coexisting with live crew).
    await expect(badge).toContainText('⚠ stalled');

    const reason = badge.locator('.plan-stall-badge-reason');
    await expect(reason, 'the FaceLabel reason renders on the badge face').toBeVisible();
    // The VALUE — which condition fired, so the operator knows what to fix.
    await expect(reason).toHaveText('· no gardener');
  });

  test('an unwatched open plan with a PAUSED gardener reads "· gardener paused"', async ({ page }) => {
    const suffix = uniqueSuffix();
    const epicId = createPlanEpic(suffix);
    addStep(epicId, suffix, 'todo', { status: 'open' });
    // Bind a paused (open, enabled=false) gardener → gardenerStateOf = "paused".
    createPausedGardener(epicId, suffix);

    await page.goto(PLANS_ALL);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card).toBeVisible();

    const reason = card.locator('.plan-stall-badge .plan-stall-badge-reason');
    await expect(reason).toBeVisible();
    // The DIFFERENT value proves the FaceLabel is a projection of gardener state,
    // not a constant — the discrimination this selector's coverage exists to guard.
    await expect(reason).toHaveText('· gardener paused');
  });
});

// -----------------------------------------------------------------------------
// 4. Crew stuck age — .plan-card-crew-stuck-age (ka-0000002016, iter-13).
//    An OPEN stuck crew (agent_state=rate_limited) surfaces its wait-age beside
//    the "· ⚠ N" stuck count. Assert the age renders a REAL duration string, not
//    an empty span, and that the count facet reads the crew as stuck.
// -----------------------------------------------------------------------------
test.describe('plans index — crew stuck age (.plan-card-crew-stuck-age)', () => {
  test('an open stuck crew surfaces a wait-age beside the stuck count', async ({ page }) => {
    const suffix = uniqueSuffix();
    const epicId = createPlanEpic(suffix);
    // A step to hang the crew off (open is fine — planCrewBulk walks all rendered
    // steps regardless of status).
    const stepId = addStep(epicId, suffix, 'todo', { status: 'open' });
    // OPEN crew stuck rate-limited → classifyCrewBead marks it failed+stuck, and
    // (not closed) → stuckOpen++, so it gets a datable wait-age.
    addCrew(stepId, suffix, 'stuck', { agentState: 'rate_limited' });

    await page.goto(PLANS_ALL);
    const card = page.locator(`li.plan-card[data-plan-id="${epicId}"]`);
    await expect(card).toBeVisible();

    // The crew chip renders with the stuck-or-failed facet (HasStuckTally).
    const stuckFacet = card.locator('.plan-card-crew-failed');
    await expect(stuckFacet, 'the "· ⚠ N" stuck facet renders').toBeVisible();
    await expect(stuckFacet).toContainText('⚠');
    await expect(stuckFacet).toContainText('1'); // one stuck crew.

    const age = card.locator('.plan-card-crew-stuck-age');
    await expect(age, 'the stuck wait-age renders inside the facet').toBeVisible();
    // Anti-vacuity: the age is a REAL humanized duration (fmtTimeAgo: Ns/Nm/Nh/Nd),
    // not an empty node — a freshly-stuck crew reads a small value ("0s"/"3s"/…).
    await expect(age).toHaveText(/^\d+[smhd]$/);
  });
});

// -----------------------------------------------------------------------------
// 5. Crew spend chip — .plan-crew-count-spend (ka-0000004218, iter-12).
//    On the COCKPIT crew roster (plan_crew.html), not the index. A crew with a
//    known total_cost_usd makes the roster header render the cumulative-spend
//    chip; assert it reads the summed dollars, not a bare "$0".
// -----------------------------------------------------------------------------
test.describe('plans cockpit — crew spend chip (.plan-crew-count-spend)', () => {
  test('the crew roster header shows cumulative crew spend', async ({ page }) => {
    const suffix = uniqueSuffix();
    const epicId = createPlanEpic(suffix);
    const stepId = addStep(epicId, suffix, 'todo', { status: 'open' });
    // Two crew with known cost → the roster sums 1.25 + 2.50 = 3.75. A DONE
    // (closed-clean) crew and a working crew both contribute reconciled
    // total_cost_usd (agentSpendUSD adds live_cost_usd only while alive, and we
    // set none, so the sum is deterministic regardless of live state).
    addCrew(stepId, suffix, 'spendA', { agentState: 'working', costUSD: '1.25' });
    addCrew(stepId, suffix, 'spendB', { agentState: 'working', costUSD: '2.50' });

    // The spend chip lives on the DETAIL cockpit crew roster.
    await page.goto(`/plans/${epicId}`);
    const spend = page.locator('#plan-crew .plan-crew-count-spend');
    await expect(spend, 'the cumulative-spend chip renders on the crew roster').toBeVisible();
    // The VALUE: the summed dollars via fmtUSD ("$3.75"), and the "spent" label.
    await expect(spend).toContainText('$3.75');
    await expect(spend).toContainText('spent');
  });
});

// -----------------------------------------------------------------------------
// 6. Unhomed forks section — .plans-unhomed (ka-0000004983).
//    An open decision that belongs to NO plan and has no operator surface pointing
//    at it must surface in the global backstop section on /plans. Assert the
//    section renders AND contains a row linking to our specific ephemeral
//    decision (by title + href) — not merely that the section exists (it could be
//    populated by unrelated forks).
// -----------------------------------------------------------------------------
test.describe('plans index — unhomed forks section (.plans-unhomed)', () => {
  test('an unhomed open decision surfaces as a row in the backstop section', async ({ page }) => {
    const suffix = uniqueSuffix();
    // A distinctive title so the row is unambiguously OUR decision, not incidental
    // unhomed noise from other fixtures.
    const title = `[E2E ephemeral] unhomed signal decision ${suffix}`;
    const decisionId = kdCreateJSON([
      'create',
      title,
      '--type', 'decision',
      '--label', 'project:plansux',
      '--label', `fixture:e2e:plan_sig_unhomed_${suffix}`,
      '--field', 'prompt=Unhomed fixture decision — pick A or B.',
      '--field', 'requested_by=e2e-signal-strip',
      '--field',
      `options=${JSON.stringify([
        { id: 'a', short: 'Option A', label: 'Pick A', artifact_type: 'report' },
        { id: 'b', short: 'Option B', label: 'Pick B', artifact_type: 'report' },
      ])}`,
    ]);
    // Deliberately NO parent-child edge to a plan and NO requesting_plan pin, so
    // buildUnhomedForks cannot subtract it → it lands in the unhomed section.

    await page.goto(PLANS_ALL);

    const section = page.locator('section.plans-unhomed#plans-unhomed');
    await expect(section, 'the unhomed-forks backstop section renders').toBeVisible();

    // The section aria-labels itself as decisions+escalations, and carries a
    // decisions sub-list. Our decision must appear there as a titled link to its
    // resolve page (/{id}).
    const row = section.locator('.plans-unhomed-row[data-kind="decision"]', { hasText: title });
    await expect(row, 'our ephemeral decision surfaces as an unhomed row').toBeVisible();
    const link = row.locator('a.plans-unhomed-title');
    await expect(link).toHaveText(title);
    await expect(link).toHaveAttribute('href', new RegExp(`/${decisionId}$`));
  });
});
