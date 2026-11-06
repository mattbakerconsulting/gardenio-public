// Backfill (ka-0000003378): the self-only auth BOUNDARY on the /u/{slug}
// status-message editor (gd-3mo447FBcf). This is the one auth guarantee the
// per-user homepage makes — a peer must not be able to open the editor for,
// or write to, someone else's status — and it shipped with no e2e spec.
//
// WIRE (user_status_message.go): three routes hang off /u/{slug}/status-message:
//   - GET  …/status-message        VIEW    — open to every viewer (200).
//   - GET  …/status-message/edit   EDIT    — self-only; 403 for non-self.
//   - POST …/status-message        UPDATE  — self-only; 403 for non-self.
// "self" = the slug resolves to the same operator id the viewer's identity
// resolves to. In this harness the dev-bypass promotes X-Auth-Request-Email
// into the verified viewer identity, so we drive both sides of the boundary
// by choosing the header:
//   - PEOPLE_EMAIL (== the slug's operator) → self.
//   - PEER_EMAIL / no header               → non-self (peer / anonymous).
//
// This spec asserts BOTH legs of the contract:
//   - the AUTHZ boundary — self is admitted past the self-only gate, peer/anon
//     are refused (403) on edit + POST, and VIEW is open to all (200);
//   - the durable WRITE — the owner's POST persists `status_message` and the
//     value reads back on /u/{slug}. `status_message` IS a registered field on
//     the type:user builtin schema (kbeads config.go:825, added in bb8645d16),
//     so the write succeeds (200) and round-trips. Before that fix the field
//     was unknown to the schema: the daemon rejected the write HTTP 400 and the
//     viewer surfaced 502, and the owner-write test asserted only `not.toBe(403)`
//     — which PASSED on that very 502, making the one spec bearing the field
//     name insensitive to it (ka-0000001413). The round-trip below (200 + a
//     distinctive marker read back) is the regression net that reds if the
//     schema registration regresses.
// Keep PEOPLE_* in sync with seed/fixtures.mjs.

import { expect, test } from '@playwright/test';

const PEOPLE_SLUG = 'e2e-people';
const PEOPLE_EMAIL = 'e2e-people@example.test';
const PEER_EMAIL = 'e2e-peer@example.test';

const VIEW_PATH = `/u/${PEOPLE_SLUG}/status-message`;
const EDIT_PATH = `/u/${PEOPLE_SLUG}/status-message/edit`;
const POST_PATH = `/u/${PEOPLE_SLUG}/status-message`;

// A request context bound to one viewer identity (or none). Isolating the
// header per-context keeps parallel self/peer assertions from cross-talking.
async function ctxFor(browser, email) {
  return browser.newContext(
    email ? { extraHTTPHeaders: { 'X-Auth-Request-Email': email } } : undefined,
  );
}

test.describe('status-message VIEW is open to every viewer', () => {
  for (const [label, email] of [
    ['anonymous', null],
    ['self', PEOPLE_EMAIL],
    ['peer', PEER_EMAIL],
  ]) {
    test(`GET status-message → 200 (${label})`, async ({ browser }) => {
      const ctx = await ctxFor(browser, email);
      try {
        const res = await ctx.request.get(VIEW_PATH, { maxRedirects: 0 });
        expect(res.status(), `${label} VIEW status`).toBe(200);
      } finally {
        await ctx.close();
      }
    });
  }
});

test.describe('status-message EDIT is self-only', () => {
  test('self CAN open the inline editor (200)', async ({ browser }) => {
    const ctx = await ctxFor(browser, PEOPLE_EMAIL);
    try {
      const res = await ctx.request.get(EDIT_PATH, { maxRedirects: 0 });
      expect(res.status(), 'self EDIT status').toBe(200);
      // The editor partial carries the status_message input — proves it is the
      // editor, not the read-only block.
      const body = await res.text();
      expect(body, 'editor renders the status_message input').toContain(
        'name="status_message"',
      );
    } finally {
      await ctx.close();
    }
  });

  test('a PEER is refused the editor (403)', async ({ browser }) => {
    const ctx = await ctxFor(browser, PEER_EMAIL);
    try {
      const res = await ctx.request.get(EDIT_PATH, { maxRedirects: 0 });
      expect(res.status(), 'peer EDIT status').toBe(403);
    } finally {
      await ctx.close();
    }
  });

  test('an ANONYMOUS viewer is refused the editor (403)', async ({ request }) => {
    const res = await request.get(EDIT_PATH, { maxRedirects: 0 });
    expect(res.status(), 'anonymous EDIT status').toBe(403);
  });
});

test.describe('status-message UPDATE is self-only', () => {
  test('a PEER cannot POST an update (403)', async ({ browser }) => {
    const ctx = await ctxFor(browser, PEER_EMAIL);
    try {
      const res = await ctx.request.post(POST_PATH, {
        form: { status_message: 'peer should not be able to write this' },
        maxRedirects: 0,
      });
      expect(res.status(), 'peer UPDATE status').toBe(403);
    } finally {
      await ctx.close();
    }
  });

  test('an ANONYMOUS viewer cannot POST an update (403)', async ({ request }) => {
    const res = await request.post(POST_PATH, {
      form: { status_message: 'anon should not be able to write this' },
      maxRedirects: 0,
    });
    expect(res.status(), 'anonymous UPDATE status').toBe(403);
  });

  test('the OWNER writes a status message that durably persists (round-trip)', async ({
    browser,
  }) => {
    // The full contract for the owner: admitted past the self-only gate AND the
    // write durably persists. `status_message` IS a registered field on the
    // type:user schema (kbeads config.go:825, bb8645d16), so the POST returns
    // 200 (not the 502 the pre-fix schema gap produced) and the value is
    // readable back through the independent read path (lookupUserBeadByOperator
    // → statusMessageFromBead). A distinctive per-run marker keeps this test
    // insensitive to whatever value a prior run (or a parallel sibling) left.
    const marker = `e2e-status-roundtrip-${Date.now()}`;
    const ctx = await ctxFor(browser, PEOPLE_EMAIL);
    try {
      const res = await ctx.request.post(POST_PATH, {
        form: { status_message: marker },
        maxRedirects: 0,
      });
      // 200, NOT merely "not 403": a round-trip that only asserted not-403 would
      // pass on the very 502 the schema-gap bug produced (ka-0000001413). The
      // success response re-renders the read-only block carrying the new value.
      expect(res.status(), 'owner self-write succeeds').toBe(200);
      expect(
        await res.text(),
        'success response echoes the written status message',
      ).toContain(marker);

      // Durable persistence: re-GET the full profile page through a FRESH read
      // path and assert the exact string survived the write. This is the leg
      // that reds if config.go:825 regresses — the write would 502 and this
      // string would never appear.
      const readback = await ctx.request.get(`/u/${PEOPLE_SLUG}`, {
        maxRedirects: 0,
      });
      expect(readback.status(), 'profile re-GET status').toBe(200);
      expect(
        await readback.text(),
        'the persisted status_message renders on /u/{slug}',
      ).toContain(marker);
    } finally {
      await ctx.close();
    }
  });
});
