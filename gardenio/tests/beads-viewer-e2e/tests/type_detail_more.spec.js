// Phase T6 (gd-MLpvfKrhp4) — per-type detail templates: decision,
// formula, mr, generic. Sibling to type_detail.spec.js (T5 covers
// schedule/agent/advice). Each describe block targets exactly one
// partial under `templates/partials/type_<name>.html` so a failure
// points at one template.
//
// Catches regressions like:
//
//   - decision options list dropped or reordered
//   - decision resolve CTA leaks to unauthenticated viewers
//     (CanResolve gating in server.go relies on AuthorEmail; in dev
//     auth bypass the CTA must stay hidden)
//   - decision resolved-banner branch silently fails to render when
//     `chosen` / `responded_by` arrive as flat fields (the production
//     wire shape from POST /v1/decisions/{id}/resolve)
//   - formula step list ordering or current-step indicator regression
//   - mr partial loses pipeline class hook, MR url link, or branch
//     code blocks
//   - generic fallback stops dispatching and a typed card renders for
//     a plain task (or vice versa)

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

test.describe('type_decision (unresolved)', () => {
  test('renders prompt + options and hides resolve CTA without auth', async ({ page }) => {
    const id = fixtureId('decision');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const card = page.locator('section.decision-card');
    await expect(card).toBeVisible();
    await expect(card.locator('h3')).toHaveText('Decision');

    // Prompt body lands inside .decision-prompt (passed through markdown).
    // The fixture prompt contains the literal "pick option A or B." — guard
    // against PromptHTML wiring or markdown pipeline regressions.
    await expect(card.locator('.decision-prompt')).toContainText('pick option A or B');

    // Options heading + per-option <li.opt-{id}> with label spans. Both
    // seeded options must render in order; reordering or dropping one is
    // the regression we want to catch.
    await expect(card.locator('h4', { hasText: 'Options' })).toBeVisible();
    const opts = card.locator('ul.decision-options li.opt');
    await expect(opts).toHaveCount(2);
    await expect(opts.nth(0)).toHaveClass(/\bopt-a\b/);
    await expect(opts.nth(0).locator('.opt-label')).toHaveText('Pick A');
    await expect(opts.nth(0).locator('.opt-artifact')).toContainText('report');
    await expect(opts.nth(1)).toHaveClass(/\bopt-b\b/);
    await expect(opts.nth(1).locator('.opt-label')).toHaveText('Pick B');

    // Resolve CTA must not render when CanResolve=false (dev auth bypass
    // exposes no AuthorEmail, so server.go sets CanResolve=false). Three
    // CTA surfaces all gated together: the click-to-resolve hint, the
    // per-option Choose <id> button, and the optional response textarea.
    await expect(card.locator('.decision-resolve-hint')).toHaveCount(0);
    await expect(card.locator('button.decision-resolve-btn')).toHaveCount(0);
    await expect(card.locator('textarea[name=response_text]')).toHaveCount(0);

    // Unresolved → no resolved banner and no opt-chosen marker. This is
    // the contrapositive of the resolved-fixture test below; together
    // they catch the banner branch firing on the wrong state.
    await expect(card.locator('.banner-resolved')).toHaveCount(0);
    await expect(card.locator('li.opt-chosen')).toHaveCount(0);
  });
});

test.describe('type_decision (resolved)', () => {
  test('renders resolved banner with chosen option marker', async ({ page }) => {
    const id = fixtureId('decision_resolved');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const card = page.locator('section.decision-card');
    await expect(card).toBeVisible();

    // Resolved banner: contains the "Resolved" strong, the chosen option
    // id in <code>, and the responder. The fixture seeds chosen=a,
    // responded_by=fixture-operator@example.com. Asserting on the strong
    // text + the chosen <code> guards against a regression where the
    // banner renders an empty shell when flat fields are present but
    // parseDecisionResolution branch is preferred.
    const banner = card.locator('.banner-resolved');
    await expect(banner).toBeVisible();
    await expect(banner.locator('strong', { hasText: 'Resolved' })).toBeVisible();
    await expect(banner.locator('code', { hasText: /^a$/ })).toBeVisible();
    await expect(banner).toContainText('fixture-operator@example.com');
    await expect(banner.locator('.decision-response')).toContainText('Picked A');

    // Chosen-option marker on the <li> for option a. The "chosen" badge
    // pairs with the opt-chosen class — both must appear on the resolved
    // option, neither on the unchosen one.
    const optA = card.locator('ul.decision-options li.opt-a');
    await expect(optA).toHaveClass(/\bopt-chosen\b/);
    await expect(optA.locator('.opt-chosen-badge')).toHaveText('chosen');
    const optB = card.locator('ul.decision-options li.opt-b');
    await expect(optB).not.toHaveClass(/\bopt-chosen\b/);
    await expect(optB.locator('.opt-chosen-badge')).toHaveCount(0);

    // Resolve CTA must NOT render when Resolved=true, regardless of auth.
    // This is the second half of the CTA-visibility coverage.
    await expect(card.locator('button.decision-resolve-btn')).toHaveCount(0);
  });
});

test.describe('type_formula', () => {
  test('renders ordered step list as a recipe with no execution chrome', async ({ page }) => {
    const id = fixtureId('formula');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const card = page.locator('section.formula-card');
    await expect(card).toBeVisible();
    await expect(card.locator('h3')).toHaveText('Formula');

    // The seeded fixture carries NO step_status_<id> fields, so this is a
    // pristine recipe (template), not a molecule run. Per ka-0000003537 the
    // partial must NOT render molecule-execution chrome on a recipe: the
    // Steps summary is a definition count ("2 steps"), not "X / N completed".
    const stepsRow = card.locator('dt', { hasText: /^Steps$/ }).locator('xpath=following-sibling::dd[1]');
    await expect(stepsRow).toContainText('2 steps');
    await expect(stepsRow).not.toContainText('completed');

    // No "Current step" indicator on a template — currentFormulaStep only
    // fires once the formula is executing (HasRunState).
    await expect(card.locator('dt', { hasText: 'Current step' })).toHaveCount(0);

    // Ordered step list — exactly two steps, in seeded order. Each <li>
    // exposes a step-id <code> plus a title. On a recipe there is NO per-step
    // run-status badge (the "pending" badge is gated on HasRunState).
    const steps = card.locator('ol.formula-steps li.step');
    await expect(steps).toHaveCount(2);
    await expect(steps.nth(0).locator('.step-id')).toHaveText('first');
    await expect(steps.nth(0).locator('.step-title')).toHaveText('First step');
    await expect(steps.nth(1).locator('.step-id')).toHaveText('second');
    await expect(steps.nth(1).locator('.step-title')).toHaveText('Second step');
    await expect(card.locator('ol.formula-steps .step-status')).toHaveCount(0);
  });
});

test.describe('type_mr', () => {
  test('renders pipeline badge, URL link, and branch fields', async ({ page }) => {
    const id = fixtureId('mr');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const card = page.locator('section.mr-card');
    await expect(card).toBeVisible();
    await expect(card.locator('h3')).toHaveText('Merge Request');

    // MR state badge — the partial wraps the value in <p class="mr-state
    // mr-state-{value}">. Class hook is what styling keys off, so assert
    // the class not just the text.
    const stateP = card.locator('p.mr-state');
    await expect(stateP).toHaveClass(/mr-state-opened/);
    await expect(stateP).toContainText('opened');

    // URL link — the partial renders <a href="{mr_url}" target="_blank">
    // wrapping the URL text. Assert href + target so a partial that
    // dropped target=_blank (security regression for external links)
    // also fails.
    const urlLink = card.locator('a', { hasText: /merge_requests\/1$/ });
    await expect(urlLink).toHaveAttribute('href', 'https://gitlab.example.com/group/project/-/merge_requests/1');
    await expect(urlLink).toHaveAttribute('target', '_blank');
    await expect(urlLink).toHaveAttribute('rel', /\bnoopener\b/);

    // Pipeline badge — the partial assigns class="pipe-{status}" to the
    // <dd>. CSS hooks off this class for the green/red colour. Asserting
    // both class and visible text catches a class-name typo.
    const pipeRow = card.locator('dt', { hasText: 'Pipeline' }).locator('xpath=following-sibling::dd[1]');
    await expect(pipeRow).toHaveClass(/\bpipe-success\b/);
    await expect(pipeRow).toHaveText('success');

    // Branch fields — both source and target are seeded; each renders
    // inside <code>. Drop either and the resulting MR card is a lot
    // less useful at a glance, so guard both.
    const sourceRow = card.locator('dt', { hasText: 'Source branch' }).locator('xpath=following-sibling::dd[1]');
    await expect(sourceRow.locator('code')).toHaveText('feat/fixture-branch');
    const targetRow = card.locator('dt', { hasText: 'Target branch' }).locator('xpath=following-sibling::dd[1]');
    await expect(targetRow.locator('code')).toHaveText('main');

    // Author from the fixture's `author` field — falls back to
    // bead.CreatedBy if missing, but the fixture pins it so this stays
    // deterministic.
    const authorRow = card.locator('dt', { hasText: 'Author' }).locator('xpath=following-sibling::dd[1]');
    await expect(authorRow).toContainText('fixture-author');
  });
});

test.describe('type_generic (fallback)', () => {
  test('renders generic field list for plain task and dispatches no typed card', async ({ page }) => {
    const id = fixtureId('generic');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    // The generic partial renders an <h3>Fields</h3> followed by a
    // <dl class="field-list field-list-rich"> only when the bead has fields.
    // The fixture seeds mention_author=fixture-author, so both must appear.
    // Scope by the dt's title attribute, which carries the exact snake_case
    // key — the visible <dt> text is the humanized label ("Mention author",
    // ka-0000006884), and the page also has a metadata <dl class="field-list">
    // at the top whose <dt>s are bead metadata names (Slug, Kind, …).
    await expect(page.locator('h3', { hasText: /^Fields$/ })).toBeVisible();
    const fieldDt = page.locator('dt[title="mention_author"]');
    await expect(fieldDt).toBeVisible();
    await expect(fieldDt).toHaveText('Mention author');
    const fieldDd = fieldDt.locator('xpath=following-sibling::dd[1]');
    // Rich rendering, NOT the old raw <pre> dump: a one-line scalar renders
    // inline (goldmark's sole wrapping <p> is unwrapped) so the value sits in
    // its grid cell rather than as preformatted text.
    await expect(fieldDd).toHaveText('fixture-author');
    await expect(fieldDd.locator('pre'), 'field values must no longer render as <pre>').toHaveCount(0);

    // Fallback dispatch contract: a plain task fixture must NOT pull in
    // any of the typed cards. If a future change in bead_show.html
    // accidentally falls through into a typed branch (e.g. by removing
    // the eq-checks), one of these assertions catches it.
    const typedCards = [
      'section.decision-card',
      'section.schedule-card',
      'section.formula-card',
      'section.mr-card',
      'section.agent-card',
      'section.advice-card',
    ];
    for (const sel of typedCards) {
      await expect(page.locator(sel), `typed card ${sel} should not render for generic task`).toHaveCount(0);
    }
  });
});
