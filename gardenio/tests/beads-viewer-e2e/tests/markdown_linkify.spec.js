// Phase T15 (gd-7Jgy5uEd76) of epic ka-0000004611 — bead description
// markdown rendering + bare gd-XXXX linkification, end-to-end through
// the real viewer + daemon.
//
// Wire:
//   server.go::handleBeadShow renders Bead.Description through goldmark
//   (GFM-enabled, Unsafe=false), then post-processes with LinkifyHTML
//   (linkify.go) to wrap bare gd-XXXX refs in anchor tags pointing at
//   the same viewer's bead-show route. The result lands in the
//   `<div class="description">` slot of bead_show.html.
//
// Catches regressions like:
//
//   - markdown pipeline silently disabled (description renders as
//     <pre> raw text instead of formatted HTML).
//   - linkify dropped or run on the wrong field (bare gd-XXXX stays as
//     plain text; agents lose nav-everywhere).
//   - linkify double-wraps a markdown <a> when the visible link text
//     happens to match the gd-XXXX shape.
//   - linkify stops descending into <pre><code> blocks (Matt's
//     "nav everywhere" guarantee — see TestBeadShow_LinkifiesBeadIDs
//     in bead_show_test.go which makes this an explicit invariant).
//   - GFM lists fall back to plain paragraphs (loses bullet structure).
//
// Fixture: seed/fixtures.mjs key 'markdown' (a task-type bead whose
// description packs all four shapes we care about). IDs in the body
// (gd-mdAlpha / gd-mdBeta / gd-mdGamma / gd-opxz5JIXYf) are synthetic
// — the linkify pass only emits anchors; it never resolves the target.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

test.describe('description markdown + gd-XXXX linkify', () => {
  test('renders code fences, bullet list, and linkified bead refs', async ({ page }) => {
    const id = fixtureId('markdown');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    // bead_show.html slots DescriptionHTML inside <div class="description">.
    // Scope all assertions inside that container so an unrelated <pre> /
    // <ul> on the page (metadata fieldlist, comments, etc.) cannot mask
    // a regression.
    const desc = page.locator('div.description');
    await expect(desc).toBeVisible();

    // GFM bullet list: three <li> in a <ul>. If goldmark's GFM ext is
    // dropped or the renderer falls back to text, the count goes to 0.
    const bullets = desc.locator('ul > li');
    await expect(bullets).toHaveCount(3);
    await expect(bullets.nth(0)).toContainText('bullet one');
    await expect(bullets.nth(1)).toContainText('bullet two');
    await expect(bullets.nth(2)).toContainText('bullet three');

    // Fenced code block → <pre><code>. The browser renders <code> with
    // a monospace user-agent style; we assert the structural presence
    // rather than computed font-family (which depends on UA + custom
    // CSS that lives outside this test's scope). The code block also
    // contains a bare gd-mdGamma ref — see the linkify assertion
    // below for the "nav everywhere" guarantee.
    const codeBlock = desc.locator('pre > code');
    await expect(codeBlock).toBeVisible();
    await expect(codeBlock).toContainText('code block referencing');

    // Bare gd-XXXX refs → <a href="/gd-XXXX"> via linkify.go. The
    // anchor's href is exactly basePath + "/" + id; in dev / test
    // basePath is empty so it's just "/gd-mdAlpha".
    const alphaAnchor = desc.locator('a[href="/gd-mdAlpha"]', { hasText: 'gd-mdAlpha' });
    await expect(alphaAnchor).toHaveCount(1);

    // gd-mdBeta sits inside a <li>; assert the anchor is scoped under
    // the bullet list so a regression that only linkifies in the lead
    // paragraph still fails.
    const betaAnchor = desc.locator('ul > li a[href="/gd-mdBeta"]', { hasText: 'gd-mdBeta' });
    await expect(betaAnchor).toHaveCount(1);

    // gd-mdGamma sits inside the fenced code block. linkify.go runs on
    // the rendered HTML (after goldmark) and skips only existing <a>
    // spans — <pre><code> is not anchor-shaped, so the bead ref is
    // wrapped. This is an explicit Matt invariant ("nav everywhere"):
    // bead_show_test.go::TestBeadShow_LinkifiesBeadIDs covers the
    // server-side render; this is the browser-level mirror.
    const gammaAnchor = desc.locator('pre code a[href="/gd-mdGamma"]', { hasText: 'gd-mdGamma' });
    await expect(gammaAnchor).toHaveCount(1);

    // Pre-existing markdown link [gd-opxz5JIXYf](https://example.test/...)
    // must survive intact:
    //   - href stays at the external URL (NOT rewritten to /gd-opxz5JIXYf).
    //   - visible text "gd-opxz5JIXYf" is preserved as the anchor's text,
    //     NOT shadowed by a second nested <a href="/gd-opxz5JIXYf">.
    // This mirrors the Go-level invariant in
    // bead_show_test.go::TestBeadShow_LinkifiesBeadIDs (gd-existing
    // case) at the browser layer: anchorRe in linkify.go skips the
    // entire <a>...</a> span so the gd-XXXX inside is not re-wrapped.
    const externalLink = desc.locator('a[href="https://example.test/gd-opxz5JIXYf"]');
    await expect(externalLink).toHaveCount(1);
    await expect(externalLink).toHaveText('gd-opxz5JIXYf');

    // Contrapositive: there must NOT be an internal anchor pointing at
    // /gd-opxz5JIXYf. If linkify regressed to double-wrap, this would
    // be the user-visible signature.
    await expect(desc.locator('a[href="/gd-opxz5JIXYf"]')).toHaveCount(0);
  });
});
