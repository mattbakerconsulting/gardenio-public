// Phase T9 (ka-0000005332) of epic ka-0000004611 — decision resolve flow.
//
// The handler under test is `handleResolveDecision` in
// gardenio/controller/cmd/beads-viewer/decisions.go (POST /b/{id}/decision/resolve).
// Go unit tests already cover the request/response surface of that handler —
// see decision_resolve_test.go::TestResolveDecision_*. This spec asserts the
// browser-level end-to-end flow that those Go tests cannot:
//
//   1. Click a chip on an unresolved decision card → JS opens the resolve
//      dialog, the operator confirms, JS POSTs to /b/{id}/decision/resolve,
//      the server merges the resolution into the bead, re-fetches it, and
//      ships the type_decision partial with the resolved banner. The card
//      swaps in via JS replaceWith.
//   2. After the swap, the unresolved CTA surfaces (resolve hint and the
//      per-option resolve chips) MUST be gone — leaking any of them would
//      let an operator double-resolve a closed decision.
//   3. The optional response_text is sourced from the dialog textarea
//      (#decision-resolve-reason, name="response_text"). The server merges
//      it into the resolution fields and the resolved banner re-renders
//      the body in `.decision-response`.
//   4. Submitting WITHOUT a response_text still resolves cleanly — the
//      `.decision-response` div is absent (server only renders it when
//      Resolution.ResponseText is non-empty).
//
// Why ephemeral fixtures: the seeded `decision` fixture (seed/fixtures.mjs)
// is reused across runs — once we resolve it the daemon closes it, so a
// re-run would hit the "already resolved" 409 branch. Each test creates its
// own freshly-labelled decision via `kd create` and resolves it. Detritus
// is bounded (one closed decision per test invocation, label-tagged for
// trivial cleanup).
//
// Auth (gd-ZTu68qjfEC): kbeads lets only a human session (OIDC / edge) or a
// bridge answer a decision (decision_resolver_authz.go, gd-Du6wHclx4g), never
// the shared static token the dev-bypass viewer writes with. So this suite runs
// against the OIDC-mode viewer with a bearer from the harness issuer
// (tests/_oidc.js): the viewer verifies it, renders the resolve chips for its
// email, and forwards it to the daemon as the write credential. The role is
// gardenio-operator, the floor the daemon's resolve route requires.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { oidcSession, requireOIDCStub } from './_oidc.js';

const AUTH_EMAIL = 'e2e-t9@example.test';

// createEphemeralDecision shells out to `kd create` (mirroring
// seed/seed.mjs's pattern) to make a fresh unresolved decision bead per
// test. Returning the daemon-assigned `kd-...` id lets each test target
// its own bead so resolves don't bleed between tests or runs.
function createEphemeralDecision(suffix) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  const options = JSON.stringify([
    { id: 'a', short: 'Option A', label: 'Pick A', artifact_type: 'report' },
    { id: 'b', short: 'Option B', label: 'Pick B', artifact_type: 'report' },
  ]);
  const args = [
    'create',
    `[E2E T9 ephemeral] decision ${suffix}`,
    '--type',
    'decision',
    '--label',
    `fixture:e2e:t9_resolve_${suffix}`,
    '--field',
    'prompt=T9 ephemeral decision — pick option A or B.',
    '--field',
    `options=${options}`,
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
  // Some kd subcommands print a deprecation banner before the JSON payload;
  // strip everything before the first `{` or `[` (matches seed.mjs).
  const i = raw.search(/[{\[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) {
    throw new Error(`kd create returned no id: ${raw}`);
  }
  return bead.id;
}

test.describe('decision resolve flow', () => {
  requireOIDCStub(test);
  test.use(oidcSession(AUTH_EMAIL, ['gardenio-operator']));

  test('option chip + response_text swaps in resolved card and removes CTA', async ({ page }) => {
    // Unique suffix so concurrent workers do not collide on the fixture
    // label and so the manifest of leftover beads is debuggable by run.
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const id = createEphemeralDecision(suffix);

    await page.goto(`/${id}`);

    const card = page.locator('section.decision-card');
    await expect(card).toBeVisible();

    // Sanity: unresolved state with the CTA surfaces present. If these
    // assertions fail we are not testing what we think — bail before the
    // POST so the failure mode is clearly "chips did not render" not
    // "server rejected the resolve".
    await expect(card.locator('.banner-resolved')).toHaveCount(0);
    await expect(card.locator('.decision-resolve-hint')).toBeVisible();
    await expect(card.locator('button.decision-resolve-chip')).toHaveCount(2);

    // The dialog lives outside the card section (sibling at the partial
    // root) so it survives the card swap. Locate it on the page root.
    const dialog = page.locator('dialog#decision-resolve-dialog');
    await expect(dialog).toHaveCount(1);
    // Closed by default — showModal() flips it open via the chip click.
    await expect(dialog).toHaveJSProperty('open', false);

    // Optional response — must round-trip into Resolution.ResponseText and
    // render in `.decision-response`. The marker proves the server's
    // re-fetch+re-render flow actually plumbs the value through (the daemon
    // merges it into bead.fields.response_text on /v1/decisions/{id}/resolve).
    const marker = `t9-rt-${suffix}`;

    // Click chip for option b → JS opens the dialog with data-option-id=b.
    await card.locator('button.decision-resolve-chip[data-option-id="b"]').click();
    await expect(dialog).toHaveJSProperty('open', true);

    // Dialog confirms the chosen option id; protects against a regression
    // where the chip click handler passes the wrong option to the dialog.
    await expect(dialog.locator('#decision-resolve-option')).toHaveText('b');

    const reason = dialog.locator('#decision-resolve-reason');
    await expect(reason).toHaveAttribute('name', 'response_text');
    await reason.fill(`Going with B because: ${marker}`);

    // Wait for the POST so we can assert the form actually fired and the
    // server returned 200 — the JS swap path only fires on r.ok, so a
    // non-200 here would silently leave the unresolved card and the
    // post-swap assertions below would be misleading.
    const responsePromise = page.waitForResponse(
      (r) => r.url().includes(`/b/${id}/decision/resolve`) && r.request().method() === 'POST',
    );
    await dialog.locator('#decision-resolve-submit').click();
    const resp = await responsePromise;
    expect(resp.status(), 'resolve POST status').toBe(200);

    // The card was swapped via JS replaceWith — re-locate from the page
    // root so the stale ElementHandle does not poison the next assertion.
    const resolvedCard = page.locator('section.decision-card');
    await expect(resolvedCard).toBeVisible();

    const banner = resolvedCard.locator('.banner-resolved');
    await expect(banner).toBeVisible();
    await expect(banner.locator('strong', { hasText: 'Resolved' })).toBeVisible();
    // Chosen option id rendered as <code>b</code>; regex-anchor so a stray
    // `b` substring in the responder/text doesn't flatter the assertion.
    await expect(banner.locator('code', { hasText: /^b$/ })).toBeVisible();
    // Identity comes from the verified bearer — the handler ignores any
    // form-supplied responder. Asserting the email here is the contract
    // that protects against a regression where the form value wins.
    await expect(banner).toContainText(AUTH_EMAIL);
    await expect(banner.locator('.decision-response')).toContainText(marker);

    // Chosen-option marker on the resolved <li>, badge present.
    const optB = resolvedCard.locator('ul.decision-options li.opt-b');
    await expect(optB).toHaveClass(/\bopt-chosen\b/);
    await expect(optB.locator('.opt-chosen-badge')).toHaveText('chosen');
    const optA = resolvedCard.locator('ul.decision-options li.opt-a');
    await expect(optA).not.toHaveClass(/\bopt-chosen\b/);

    // Unresolved CTA surfaces are gone — the whole point of T9. The
    // template gates the hint and the chip-strip on the same
    // `CanResolve && !Resolved` predicate, so any one of them surviving
    // points at a specific gating regression.
    await expect(resolvedCard.locator('.decision-resolve-hint')).toHaveCount(0);
    await expect(resolvedCard.locator('button.decision-resolve-chip')).toHaveCount(0);
    await expect(resolvedCard.locator('.chip-strip')).toHaveCount(0);
  });

  test('option chip without response_text resolves and omits the response body', async ({ page }) => {
    const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const id = createEphemeralDecision(suffix);

    await page.goto(`/${id}`);

    const card = page.locator('section.decision-card');
    await expect(card.locator('button.decision-resolve-chip')).toHaveCount(2);

    const dialog = page.locator('dialog#decision-resolve-dialog');

    // No response_text fill — the dialog textarea stays empty. Click
    // chip for option a → JS opens dialog → submit without filling.
    await card.locator('button.decision-resolve-chip[data-option-id="a"]').click();
    await expect(dialog).toHaveJSProperty('open', true);

    const responsePromise = page.waitForResponse(
      (r) => r.url().includes(`/b/${id}/decision/resolve`) && r.request().method() === 'POST',
    );
    await dialog.locator('#decision-resolve-submit').click();
    const resp = await responsePromise;
    expect(resp.status(), 'resolve POST status').toBe(200);

    const resolvedCard = page.locator('section.decision-card');
    const banner = resolvedCard.locator('.banner-resolved');
    await expect(banner).toBeVisible();
    await expect(banner.locator('code', { hasText: /^a$/ })).toBeVisible();

    // type_decision.html only emits `.decision-response` when
    // Resolution.ResponseText is non-empty. An empty submit must leave
    // that div out — guards against a regression where the template
    // unconditionally renders an empty <div class="decision-response">.
    await expect(banner.locator('.decision-response')).toHaveCount(0);

    // CTA surfaces gone, same contract as the round-trip case.
    await expect(resolvedCard.locator('button.decision-resolve-chip')).toHaveCount(0);
    await expect(resolvedCard.locator('.chip-strip')).toHaveCount(0);
  });
});
