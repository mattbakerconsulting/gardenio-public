// Phase T7 (gd-vb5HJAHc2F) of epic ka-0000004611 — comment write path
// (POST /b/{id}/comments) browser-level coverage.
//
// The handler shape lives in gardenio/controller/cmd/beads-viewer/server.go
// (handleAddComment + renderCommentError, shipped under epic ka-0000002538
// rooted at ka-0000002865). Go unit tests already cover the request /
// response surface end of the contract — see htmx_test.go::TestAddComment_*.
// This spec asserts what the real browser does end-to-end:
//
//   1. empty submit → HTML5 `required` validation blocks the form before
//      htmx fires a POST (the daemon never sees an empty payload).
//   2. valid submit → the comments fragment is swapped in, the new comment
//      renders in the list, and the textarea is reset for the next post.
//   3. mocked 5xx → htmx receives an error response; with htmx 2.x's default
//      `responseHandling` (no swap on 4xx/5xx), the existing form DOM is
//      left untouched so the user's draft survives a transient failure
//      without needing to retype.
//   4. multi-line markdown → the round-trip preserves paragraph breaks and
//      bold emphasis (via goldmark's WithHardWraps inline renderer; see
//      `mdInline` configuration in server.go::NewServer).
//
// Auth: dev-bypass mode is on (BEADS_VIEWER_DEV_AUTH_BYPASS=1), so
// `oidcauth.EmailFromRequest` defaults to "" — `CanComment=false` would
// hide the comment form entirely. Set `X-Auth-Request-Email` at the test
// level to mimic a logged-in user without standing up Keycloak. The
// devBypassMiddleware is a passthrough, so the header reaches the handler
// the same way ForwardAuth would inject it in prod.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

test.use({
  extraHTTPHeaders: {
    'X-Auth-Request-Email': 'e2e-t7@example.test',
  },
});

// Use the `generic` fixture (a plain task bead) so each test pays the
// cheapest type-detail render cost — comments-section is the same across
// types, so picking the simplest page keeps the spec focused on the
// write path. Tests use unique markers per submission so accumulated
// comments from prior runs (the daemon is real and shared) do not
// interfere with assertions.
const FIXTURE_KEY = 'generic';

test.describe('comment write path', () => {
  test('empty submit is rejected client-side (no POST fires)', async ({ page }) => {
    const id = fixtureId(FIXTURE_KEY);
    await page.goto(`/${id}`);

    const textarea = page.locator('form.comment-form textarea[name="text"]');
    await expect(textarea, 'comment form must render under the dev-bypass + X-Auth-Request-Email header').toBeVisible();

    // Track POSTs to the comments endpoint. The HTML5 `required` attribute
    // on #comment-text means a submit on an empty value short-circuits in
    // the browser; htmx respects form validity and never fires the request.
    let postCount = 0;
    const onReq = (req) => {
      if (req.method() === 'POST' && req.url().includes(`/b/${id}/comments`)) postCount += 1;
    };
    page.on('request', onReq);

    try {
      await textarea.fill('');
      await page.locator('form.comment-form button[type="submit"]').click();

      // The textarea reports invalid (validity.valueMissing). This is the
      // user-visible signal that the form refused to submit — losing
      // `required` on the textarea would let empty comments through to the
      // daemon and is exactly what this test is built to catch.
      await expect(textarea).toHaveJSProperty('validity.valid', false);
      await expect(textarea).toHaveJSProperty('validity.valueMissing', true);

      // Settle any pending htmx work so the post-count assertion is stable.
      await waitForHtmxIdle(page);
      expect(postCount, 'no POST should fire for an empty submit').toBe(0);
    } finally {
      page.off('request', onReq);
    }
  });

  test('valid submit swaps in the new comment and clears the textarea', async ({ page }) => {
    const id = fixtureId(FIXTURE_KEY);
    await page.goto(`/${id}`);

    // Unique marker so we can find this specific comment in the rendered
    // list, regardless of comments seeded by prior runs.
    const marker = `e2e-t7-success-${Date.now()}`;

    const textarea = page.locator('form.comment-form textarea[name="text"]');
    await expect(textarea).toBeVisible();
    await textarea.fill(marker);
    await page.locator('form.comment-form button[type="submit"]').click();
    await waitForHtmxIdle(page);

    // The new comment renders inside the swapped-in fragment. We scope by
    // the marker so prior runs' fixture comments don't muddy the match.
    const newComment = page.locator('.comment', { hasText: marker });
    await expect(newComment).toBeVisible();
    // Authorship IS asserted (ka-0000006262, gd-qZER8OOiIb). The daemon
    // ignores the request-body Author and derives comment.Author from the auth
    // identity. In production the viewer writes with the user's own OIDC
    // bearer, so the author is the human. This dev-bypass viewer has no user
    // bearer: it writes on the harness fleet token (e2e.yml phase 2,
    // e2e-fleet-token:e2e-admin), and a fleet token's author is its own
    // subject — no X-Actor override is honoured (the shared static token that
    // honoured one is retired, gd-ke6PC7bRto) — so the author stores as
    // "e2e-admin". The assertion below pins that the author comes from the
    // credential, not from anything the viewer could forge. The human still
    // reaches the comment as its operator: the viewer stamps X-Operated-By
    // oidc:<lowercased email>
    // (operatorMiddleware), and the comment row renders it as the operator
    // chip next to the author.
    //
    // Direct-child `.comment-meta > strong` selector: the operator chip nested
    // under .comment-meta renders its OWN <strong> for the human display name,
    // so a descendant selector matches two elements and trips Playwright
    // strict mode. Scope to newComment (by the unique marker) so accumulated
    // fixture comments from prior runs don't muddy the match.
    await expect(newComment.locator('.comment-meta > strong')).toHaveText('e2e-admin');
    await expect(
      newComment.locator('.comment-meta .operator-chip[data-operated-by="oidc:e2e-t7@example.test"]'),
    ).toHaveCount(1);

    // Server re-rendered the form with no `CommentDraft` set — the textarea
    // must be empty so the next comment doesn't accidentally include the
    // previous one. Re-locate after the swap because the element is fresh.
    await expect(page.locator('form.comment-form textarea[name="text"]')).toHaveValue('');

    // No error banner on the happy path.
    await expect(page.locator('.comment-error')).toHaveCount(0);
  });

  test('failure (mocked 5xx) preserves draft and does not append a comment', async ({ page }) => {
    const id = fixtureId(FIXTURE_KEY);
    await page.goto(`/${id}`);

    // Snapshot the comment count before the failed submit so we can assert
    // the list did not grow (which would mean htmx swapped the partial
    // despite the 5xx, which would mask real prod failures).
    const before = await page.locator('.comment').count();

    // Intercept the POST and return a synthetic 502. The body mimics what
    // server.go::renderCommentError produces (the comments partial with an
    // inline error and the draft echoed back), but htmx 2.x's default
    // `responseHandling` does NOT swap on 4xx/5xx — so the body is
    // intentionally cosmetic. The behavior we assert is what the user sees:
    // no swap, draft retained in the existing textarea.
    await page.route(`**/b/${id}/comments`, async (route) => {
      if (route.request().method() !== 'POST') {
        return route.fallback();
      }
      await route.fulfill({
        status: 502,
        contentType: 'text/html; charset=utf-8',
        body:
          '<section id="comments-section" class="comments-section">' +
          '<p class="comment-error" role="alert">Failed to post comment.</p>' +
          '</section>',
      });
    });

    const draft = `e2e-t7-failed-draft-${Date.now()}`;
    const textarea = page.locator('form.comment-form textarea[name="text"]');
    await expect(textarea).toBeVisible();
    await textarea.fill(draft);

    // Wait for the response so we know htmx has fully processed the failure
    // (afterRequest fires, hx-disabled-elt is released, no swap occurs).
    const responsePromise = page.waitForResponse(
      (r) => r.url().includes(`/b/${id}/comments`) && r.request().method() === 'POST',
    );
    await page.locator('form.comment-form button[type="submit"]').click();
    const resp = await responsePromise;
    expect(resp.status(), 'mocked POST status').toBe(502);
    await waitForHtmxIdle(page);

    // Draft is preserved — the existing form DOM was not swapped.
    await expect(page.locator('form.comment-form textarea[name="text"]')).toHaveValue(draft);

    // Comment list did not grow. The daemon may or may not have been
    // called (the route was intercepted at the browser layer), but the
    // visible list must be unchanged.
    await expect(page.locator('.comment')).toHaveCount(before);
  });

  test('multi-line markdown round-trips through the server renderer', async ({ page }) => {
    const id = fixtureId(FIXTURE_KEY);
    await page.goto(`/${id}`);

    // Three things we want to round-trip:
    //   - paragraph break (blank line) → two <p> elements
    //   - intra-paragraph newline (hard wrap) → <br> via WithHardWraps
    //   - **bold** → <strong>bold</strong>
    // The marker stays on its own paragraph at the top so the locator
    // (`hasText: marker`) reliably resolves this run's comment.
    const marker = `e2e-t7-mlmd-${Date.now()}`;
    const text = `${marker}\n\nline one\nline two with **bold**\n\nfinal paragraph`;

    const textarea = page.locator('form.comment-form textarea[name="text"]');
    await expect(textarea).toBeVisible();
    await textarea.fill(text);
    await page.locator('form.comment-form button[type="submit"]').click();
    await waitForHtmxIdle(page);

    const myComment = page.locator('.comment', { hasText: marker });
    await expect(myComment).toBeVisible();
    const body = myComment.locator('.comment-body');

    // Two blank-line breaks → three paragraphs (marker / lines / final).
    await expect(body.locator('p')).toHaveCount(3);

    // Hard-wrap inside the middle paragraph: "line one" then <br> then
    // "line two with **bold**". Goldmark with WithHardWraps emits a
    // single <br>; if WithHardWraps gets dropped the count drops to 0.
    await expect(body.locator('p', { hasText: 'line one' }).locator('br')).toHaveCount(1);

    // **bold** survives the markdown pipeline as <strong>.
    await expect(body.locator('strong', { hasText: 'bold' })).toBeVisible();

    // Final paragraph as plain text — guards against the renderer
    // collapsing the second paragraph break into one.
    await expect(body.locator('p', { hasText: 'final paragraph' })).toBeVisible();
  });
});
