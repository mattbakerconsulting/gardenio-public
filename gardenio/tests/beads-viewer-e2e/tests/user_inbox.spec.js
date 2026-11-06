// Backfill (ka-0000002598): behavioral coverage for the /u/{slug}/inbox route
// and its lazy /u/{slug}/section/inbox partial (feat ka-0000005615, commit
// 43891abe) — the bidirectional, four-population conversation feed that shipped
// with Go unit tests (user_inbox_test.go) but no dedicated behavioral e2e.
//
// The feed merges FOUR bead populations into one chat log (user_inbox.go):
//   AGENT → USER (incoming, dir-from-agent bubble):
//     • mail   — type=mail addressed to a recipient handle (assignee column).
//     • squawk — type=message @-mentioning the operator's USER bead.
//   USER → AGENT (outgoing, dir-from-user bubble):
//     • reply  — type=message, label slack-thread-reply, operated_by = viewer.
// (The bot-mention outgoing label is a fourth population of the same shape; the
// slack-thread-reply fixture exercises the outgoing side.)
//
// The anchor operator is the dedicated one seeded by the inbox_* fixtures:
//   oidc:e2e-inbox@example.test → slug 'e2e-inbox', display 'E2E Inbox
//   Operator'. Its inbox contains EXACTLY the three seeded messages (mail +
//   squawk + reply), so counts are deterministic. Keep INBOX_* in sync with
//   seed/fixtures.mjs (inbox_provision_task / inbox_user_bead / inbox_mail /
//   inbox_squawk / inbox_thread_reply).
//
// WHAT THIS SPEC PINS — and a deliberate correction to the origin bead:
//   ka-0000002598 proposed asserting that a CROSS-USER viewer sees mail /
//   outgoing bodies REDACTED to subject-only. That behaviour was REMOVED before
//   this route shipped: gardenio is a FLAT org (ka-0000003111), so a user's page
//   is a total-transparency activity record and every body renders in full for
//   every viewer (user_inbox.go assembleUserInbox doc + the Go unit test
//   TestAssembleUserInbox_BodiesAlwaysRendered). This spec therefore pins the
//   CURRENT contract — a cross-user viewer sees the FULL bodies — the direct
//   inverse of, and strictly stronger than, the stale redaction line.
//
// Coverage:
//   1. /u/{slug}/inbox renders the page shell + <h1>{display} · Messages + the
//      inbox section, for a cross-user (anonymous) viewer.
//   2. The feed is BIDIRECTIONAL: agent→user (dir-from-agent) and user→agent
//      (dir-from-user) bubbles both render.
//   3. NO REDACTION: a cross-user viewer sees every body — mail, agent squawk,
//      and the operator's own outgoing reply — IN FULL.
//   4. The feed regroups into one card per counterpart, tagged with the
//      counterpart handle (redesign ka-0000001181).
//   5. Unread: the open mail marks the feed unread (badge + per-card marker),
//      independent of the last-visit machinery.
//   6. Kind badges classify mail vs squawk.
//   7. An unknown slug is a 404 (not a soft empty page).
//   8. Self vs peer: the OWNER sees the "(you)" marker; a PEER does not — but
//      still sees the full feed (no redaction for peers).
//   9. The lazy /u/{slug}/section/inbox route returns the inbox partial.

import { expect, test } from '@playwright/test';

const INBOX_SLUG = 'e2e-inbox';
const INBOX_EMAIL = 'e2e-inbox@example.test';
const INBOX_DISPLAY = 'E2E Inbox Operator';
// A DIFFERENT authenticated viewer — provisioned lazily by the daemon the
// moment we send its email, but never equal to INBOX_EMAIL, so it is always a
// peer of /u/e2e-inbox.
const PEER_EMAIL = 'e2e-inbox-peer@example.test';

// Distinctive body strings the fixtures seed, one per population. Asserting
// each renders is what pins the no-redaction contract.
const MAIL_BODY = 'MAIL-BODY-FULL';
const SQUAWK_BODY = 'SQUAWK-BODY-FULL';
const REPLY_BODY = 'REPLY-BODY-FULL';

test.describe('GET /u/{slug}/inbox — bidirectional message feed', () => {
  test('renders the page shell, the display-name heading, and the inbox section', async ({
    page,
  }) => {
    const res = await page.goto(`/u/${INBOX_SLUG}/inbox`);
    expect(res, 'navigation response').not.toBeNull();
    expect(res.status(), '/u/{slug}/inbox HTTP status').toBeLessThan(400);
    // Layout shell — a break in layout.html regresses every route at once.
    await expect(page.locator('nav').first()).toBeVisible();
    // The h1 is the operator's display name (resolved through the user-bead
    // join) followed by "· Messages".
    const h1 = page.locator('h1').first();
    await expect(h1).toContainText(INBOX_DISPLAY);
    await expect(h1).toContainText('Messages');
    // The outer feed element is <section id="inbox">.
    await expect(page.locator('section#inbox')).toBeAttached();
    await expect(page.locator('section#inbox')).toBeVisible();
  });

  test('the feed is bidirectional — agent→user and user→agent bubbles both render', async ({
    page,
  }) => {
    await page.goto(`/u/${INBOX_SLUG}/inbox`);
    // Exactly the three seeded messages land in this dedicated operator's feed.
    await expect(page.locator('[data-inbox-item]')).toHaveCount(3);
    // AGENT → USER lines (mail + squawk) render on the other side …
    await expect(page.locator('.user-inbox-msg.dir-from-agent')).toHaveCount(2);
    // … and the operator's own outgoing reply renders on the own side. Both
    // sides present is the bidirectional contract (the whole point of the route).
    await expect(page.locator('.user-inbox-msg.dir-from-user')).toHaveCount(1);
  });

  test('a cross-user viewer sees every body in full (no redaction — ka-0000003111)', async ({
    page,
  }) => {
    // The default page context carries no auth header, so this viewer is
    // categorically NOT the page owner — exactly the case where the removed
    // §3.6 redaction would have hidden the mail / outgoing bodies. All three
    // bodies must render in full instead.
    await page.goto(`/u/${INBOX_SLUG}/inbox`);
    const feed = page.locator('section#inbox');
    await expect(feed, 'agent→user mail body renders in full').toContainText(MAIL_BODY);
    await expect(feed, 'agent squawk body renders in full').toContainText(SQUAWK_BODY);
    await expect(feed, 'the operator’s own outgoing reply body renders in full').toContainText(
      REPLY_BODY,
    );
  });

  test('the feed regroups into one card per counterpart, tagged with the handle', async ({
    page,
  }) => {
    await page.goto(`/u/${INBOX_SLUG}/inbox`);
    // Each distinct counterpart handle gets its own card (redesign
    // ka-0000001181). The two agent counterparts are tagged with their handles.
    await expect(
      page.locator('.user-inbox-card[data-agent="e2e-inbox-mailer"]'),
      'mail counterpart card',
    ).toHaveCount(1);
    await expect(
      page.locator('.user-inbox-card[data-agent="e2e-inbox-gardener"]'),
      'squawk counterpart card',
    ).toHaveCount(1);
  });

  test('the open mail marks the feed unread', async ({ page }) => {
    await page.goto(`/u/${INBOX_SLUG}/inbox`);
    // The head unread badge renders whenever the feed has unread lines. The open
    // mail is unread by status=open alone (inboxMessageUnread), independent of
    // the last-visit machinery, so this badge is deterministic for any viewer.
    const badge = page.locator('[data-inbox-unread]');
    await expect(badge).toBeVisible();
    await expect(badge).toContainText('unread');
    // The unread tally is attributed to the OWNING card — the mail counterpart's
    // card carries a per-card unread marker.
    const mailCard = page.locator('.user-inbox-card[data-agent="e2e-inbox-mailer"]');
    await expect(mailCard.locator('.user-inbox-card-unread')).toBeVisible();
  });

  test('kind badges classify mail vs squawk', async ({ page }) => {
    await page.goto(`/u/${INBOX_SLUG}/inbox`);
    await expect(page.locator('.user-inbox-kind.kind-mail')).toHaveCount(1);
    await expect(page.locator('.user-inbox-kind.kind-squawk')).toHaveCount(1);
  });

  test('an unknown slug is a 404', async ({ request }) => {
    const res = await request.get('/u/no-such-inbox-slug-xyz/inbox', { maxRedirects: 0 });
    expect(res.status(), 'unknown slug inbox status').toBe(404);
  });
});

test.describe('viewer identity (self vs peer)', () => {
  test('the OWNER sees the "(you)" marker', async ({ browser }) => {
    // A fresh context scoped to the owner's identity header so this test's
    // viewer identity never leaks into the peer test running in parallel.
    const ctx = await browser.newContext({
      extraHTTPHeaders: { 'X-Auth-Request-Email': INBOX_EMAIL },
    });
    const page = await ctx.newPage();
    try {
      await page.goto(`/u/${INBOX_SLUG}/inbox`);
      // "(you)" marker in the h1 proves isViewerSelf resolved true.
      await expect(page.locator('h1').first()).toContainText('(you)');
    } finally {
      await ctx.close();
    }
  });

  test('a PEER sees no "(you)" marker but still sees the full feed', async ({
    browser,
  }) => {
    const ctx = await browser.newContext({
      extraHTTPHeaders: { 'X-Auth-Request-Email': PEER_EMAIL },
    });
    const page = await ctx.newPage();
    try {
      await page.goto(`/u/${INBOX_SLUG}/inbox`);
      // The page still renders for the peer …
      await expect(page.locator('h1').first()).toContainText(INBOX_DISPLAY);
      await expect(page.locator('section#inbox')).toBeVisible();
      // … without the self marker …
      await expect(page.locator('h1').first()).not.toContainText('(you)');
      // … and, because gardenio is flat, the peer still sees every body in full.
      const feed = page.locator('section#inbox');
      await expect(feed).toContainText(MAIL_BODY);
      await expect(feed).toContainText(REPLY_BODY);
    } finally {
      await ctx.close();
    }
  });
});

test.describe('GET /u/{slug}/section/inbox — lazy fragment', () => {
  test('returns the inbox partial fragment', async ({ page }) => {
    const res = await page.goto(`/u/${INBOX_SLUG}/section/inbox`);
    expect(res, 'navigation response').not.toBeNull();
    expect(res.status(), '/section/inbox HTTP status').toBeLessThan(400);
    // The partial's outer element is <section id="inbox"> so the lazy swap can
    // target #inbox; the same three seeded bodies render in the fragment.
    await expect(page.locator('section#inbox')).toBeAttached();
    await expect(page.locator('section#inbox')).toContainText(MAIL_BODY);
    await expect(page.locator('section#inbox')).toContainText(REPLY_BODY);
  });
});
