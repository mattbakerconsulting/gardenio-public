// Phase T13 (gd-OC1xaK13uC) of epic ka-0000004611 — comment draft
// persistence (localStorage) browser-level coverage.
//
// Source: templates/static/drafts.js (per-textarea autosave, debounced
// 500ms, keyed at `bv:draft:<beadID>` with an LRU index at
// `bv:drafts:index`). Behavior under test:
//
//   1. type into a comment textarea, navigate away (full page nav) and
//      back — the draft is restored on bind.
//   2. submit a comment successfully — drafts.js clears the bead's
//      localStorage entry (on `htmx:beforeSwap`), so a fresh visit
//      finds the textarea empty.
//   3. drafts are scoped per-bead — typing on bead A and then bead B
//      does not cross-contaminate; navigating back to either restores
//      that bead's draft and leaves the other one alone.
//   4. a post made after the debounced save already fired does not
//      put the posted text back in the fresh composer (gd-cDR7idwi3b).
//   5. the textarea's own preview POST (on load and on input) does not
//      clear the stored draft (gd-cDR7idwi3b).
//
// Auth: like T7 (comment_write.spec.js), set X-Auth-Request-Email so
// CanComment renders the form under dev-bypass — the draft script only
// binds to `form.comment-form`, which is gated on CanComment.
//
// We deliberately avoid sleeping for the 500ms debounce; instead we
// poll localStorage via `page.waitForFunction` for the expected key/value
// to be present before navigating. This keeps the spec deterministic
// even on a slow runner without violating the suite-wide "no sleeps" rule.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

test.use({
  extraHTTPHeaders: {
    'X-Auth-Request-Email': 'e2e-t13@example.test',
  },
});

const KEY_PREFIX = 'bv:draft:';

// Wait for drafts.js's debounced save to land in localStorage. The
// script debounces the `input` listener at 500ms; we poll instead of
// sleeping so the spec stays stable regardless of runner load.
async function waitForStoredDraft(page, beadID, expected) {
  await page.waitForFunction(
    ({ key, want }) => window.localStorage.getItem(key) === want,
    { key: KEY_PREFIX + beadID, want: expected },
  );
}

async function readStoredDraft(page, beadID) {
  return page.evaluate((key) => window.localStorage.getItem(key), KEY_PREFIX + beadID);
}

const TEXTAREA = 'form.comment-form textarea[name="text"]';

test.describe('comment draft persistence', () => {
  test('typed draft survives navigation away and back', async ({ page }) => {
    const id = fixtureId('generic');
    const draft = `e2e-t13-restored-${Date.now()}`;

    await page.goto(`/${id}`);

    const textarea = page.locator(TEXTAREA);
    await expect(textarea, 'comment form must render under the dev-bypass + X-Auth-Request-Email header').toBeVisible();
    await textarea.fill(draft);
    await waitForStoredDraft(page, id, draft);

    // Full-page navigation away — drafts.js does not run here, so this
    // exercises the "page reload" recovery path, not just an in-memory
    // form retention quirk. Layout.html has no <main>/role="main"; assert
    // the /agents page's <h1> rendered instead (gd-BmKzpmLuIO).
    await page.goto('/agents');
    await expect(page.locator('h1', { hasText: 'Agents' })).toBeVisible();

    // Navigate back. drafts.js binds on DOMContentLoaded, sees the
    // textarea empty (server-rendered CommentDraft is ""), reads the
    // stored draft, and prefills the textarea.
    await page.goto(`/${id}`);
    await expect(page.locator(TEXTAREA)).toHaveValue(draft);
  });

  test('successful submit clears the stored draft', async ({ page }) => {
    const id = fixtureId('generic');
    const marker = `e2e-t13-clear-${Date.now()}`;

    await page.goto(`/${id}`);

    const textarea = page.locator(TEXTAREA);
    await expect(textarea).toBeVisible();
    await textarea.fill(marker);
    await waitForStoredDraft(page, id, marker);

    await page.locator('form.comment-form button[type="submit"]').click();
    await waitForHtmxIdle(page);

    // The new comment landed (sanity — confirms a real 2xx fired,
    // which is what triggers drafts.js's clear path).
    await expect(page.locator('.comment', { hasText: marker })).toBeVisible();

    // drafts.js removes the entry from localStorage on a 2xx for the
    // comment form's POST (htmx:beforeSwap, again on afterRequest). Poll
    // so the assertion does not depend on htmx's event timing.
    await page.waitForFunction(
      (key) => window.localStorage.getItem(key) == null,
      KEY_PREFIX + id,
    );

    // Re-visit cold: nothing in localStorage, server CommentDraft is
    // empty, textarea must be blank — proves the clear actually
    // persists, not just hidden by an in-flight DOM state.
    await page.goto(`/${id}`);
    await expect(page.locator(TEXTAREA)).toHaveValue('');
  });

  test('drafts are scoped per bead — A and B do not cross-contaminate', async ({ page }) => {
    const idA = fixtureId('generic');
    const idB = fixtureId('mr');
    expect(idA, 'fixtures A and B must be distinct beads to prove independence').not.toBe(idB);

    const draftA = `e2e-t13-A-${Date.now()}`;
    const draftB = `e2e-t13-B-${Date.now()}`;

    // Type on A, flush to localStorage.
    await page.goto(`/${idA}`);
    await expect(page.locator(TEXTAREA)).toBeVisible();
    await page.locator(TEXTAREA).fill(draftA);
    await waitForStoredDraft(page, idA, draftA);

    // Navigate to B. Critically, the textarea on B must NOT be
    // prefilled with A's draft — drafts.js keys by bead id.
    await page.goto(`/${idB}`);
    const textareaB = page.locator(TEXTAREA);
    await expect(textareaB).toBeVisible();
    await expect(textareaB, "bead B's textarea must not inherit bead A's draft").toHaveValue('');

    await textareaB.fill(draftB);
    await waitForStoredDraft(page, idB, draftB);

    // Both keys coexist.
    expect(await readStoredDraft(page, idA)).toBe(draftA);
    expect(await readStoredDraft(page, idB)).toBe(draftB);

    // Back to A — A's draft is restored, B's is untouched.
    await page.goto(`/${idA}`);
    await expect(page.locator(TEXTAREA)).toHaveValue(draftA);
    expect(await readStoredDraft(page, idB)).toBe(draftB);

    // And back to B — B's draft is still there.
    await page.goto(`/${idB}`);
    await expect(page.locator(TEXTAREA)).toHaveValue(draftB);
  });

  // gd-cDR7idwi3b: htmx 2 fires afterSwap (which re-binds and prefills an
  // empty textarea from storage) BEFORE afterRequest (where the draft used
  // to be cleared). So once the 500ms debounced save had landed — any user
  // who pauses before clicking Post — the swapped-in composer came back
  // holding the just-posted text. Waiting for the stored draft before
  // submitting guarantees more than 500ms passed since the fill, which
  // pins the race deterministically instead of leaving it to runner load.
  test('posting after the debounced save fired leaves the composer empty', async ({ page }) => {
    const id = fixtureId('generic');
    const marker = `e2e-t13-posted-${Date.now()}`;

    await page.goto(`/${id}`);
    const textarea = page.locator(TEXTAREA);
    await expect(textarea).toBeVisible();
    await textarea.fill(marker);
    await waitForStoredDraft(page, id, marker);

    await page.locator('form.comment-form button[type="submit"]').click();
    await waitForHtmxIdle(page);
    await expect(page.locator('.comment', { hasText: marker })).toBeVisible();

    // The re-rendered composer is empty, and nothing is left in storage.
    await expect(page.locator(TEXTAREA)).toHaveValue('');
    expect(await readStoredDraft(page, id)).toBeNull();
  });

  // The textarea's live preview is its own hx-post (.../comments/preview,
  // fired on load and on input). Its form ancestor is the comment form, so
  // the old afterRequest handler treated every 2xx preview as a successful
  // post and wiped the draft — right after it had been restored on load.
  test('a restored draft survives the preview request fired on load', async ({ page }) => {
    const id = fixtureId('generic');
    const draft = `e2e-t13-preview-${Date.now()}`;

    await page.goto(`/${id}`);
    await expect(page.locator(TEXTAREA)).toBeVisible();
    await page.locator(TEXTAREA).fill(draft);
    await waitForStoredDraft(page, id, draft);
    // Let this page's own input-preview settle, so the wait below can only
    // match the preview the reloaded page fires on load.
    await waitForHtmxIdle(page);

    const preview = page.waitForResponse(
      (r) => r.url().includes(`/b/${id}/comments/preview`) && r.request().method() === 'POST',
    );
    await page.goto(`/${id}`);
    await expect(page.locator(TEXTAREA)).toHaveValue(draft);
    await preview;
    await waitForHtmxIdle(page);

    expect(await readStoredDraft(page, id)).toBe(draft);
    await expect(page.locator(TEXTAREA)).toHaveValue(draft);
  });
});
