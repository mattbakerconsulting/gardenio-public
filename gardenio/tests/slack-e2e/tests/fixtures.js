// Playwright test fixtures for the real-user Slack E2E suite.
//
// This is the "wire in slack-bridge test fixtures (auth, channel access)" piece
// of the scaffold (task ka-0000004549). It extends Playwright's base `test`
// with four fixtures that every slack-ui spec builds on, so specs assert on
// behavior rather than re-plumbing auth/reachability each time:
//
//   bridge        — a request context bound to the ephemeral slack-bridge under
//                   test (SLACK_E2E_BRIDGE_URL). Auto-asserts the bridge is
//                   reachable + ready (/healthz, /readyz) before the spec body
//                   runs, so a dead bridge fails as "bridge not ready", not as a
//                   downstream selector timeout.
//   beads         — a request context bound to the beads daemon (BEADS_HTTP_URL)
//                   with the Authorization bearer applied, for asserting the
//                   controller/bridge reacted (a decision resolved, a card bead
//                   updated, an agent spawned).
//   slackChannel  — the resolved test channel id (SLACK_E2E_CHANNEL), asserted
//                   to be a well-formed C0… id. This is the "channel access"
//                   fixture: the app was invited to exactly this channel, so
//                   every inbound/outbound assertion scopes to it.
//   workspace     — an authenticated Slack web-client Page. Injects the captured
//                   test-user storageState (via the config's use.storageState),
//                   navigates into the test channel, and asserts the workspace
//                   DOM loaded rather than the signed-out login screen — turning
//                   a lapsed session into a precise SESSION_EXPIRED pointer at
//                   the runbook instead of an opaque flake.
//
// All four are lazy: a `request`-only spec never boots a browser, and a spec
// that only touches the bridge never resolves a channel. Playwright instantiates
// a fixture only when a test names it.

import { test as base, expect, request as playwrightRequest } from '@playwright/test';
import {
  qa,
  slackEnv,
  channelURL,
  SLACK_SESSION_COOKIES,
  SESSION_EXPIRED_HINT,
} from './_slack.js';

// A well-formed Slack channel id: C/G/D prefix + base-36 body. The test rig
// uses a public channel (C…), but accept G…/D… so a private-channel or DM rig
// variant does not need a fixture change.
const CHANNEL_ID_RE = /^[CGD][A-Z0-9]{6,}$/;

export const test = base.extend({
  // ── bridge: reachable + ready request context ─────────────────────
  bridge: async ({}, use) => {
    const baseURL = slackEnv.bridgeURL();
    const ctx = await playwrightRequest.newContext({ baseURL });

    // Reachability + readiness gate. /healthz is always-200 with a version;
    // /readyz is gated on the bridge's Socket Mode + daemon health. A real
    // human-authored event has nowhere to flow if the bridge is not ready, so
    // fail here with a clear reason rather than deep in a spec.
    const health = await ctx.get('/healthz');
    expect(
      health.ok(),
      `slack-bridge /healthz not OK at ${baseURL} (status ${health.status()}) — ` +
        'is the ephemeral bridge deployed (slackBridge.enabled=true)?',
    ).toBeTruthy();

    const ready = await ctx.get('/readyz');
    if (!ready.ok()) {
      const body = await ready.text().catch(() => '');
      throw new Error(
        `slack-bridge /readyz not ready at ${baseURL} (status ${ready.status()}): ${body}. ` +
          'The Socket Mode attach or daemon connection is unhealthy.',
      );
    }

    await use(ctx);
    await ctx.dispose();
  },

  // ── beads: daemon request context with auth bearer ────────────────
  beads: async ({}, use) => {
    const baseURL = slackEnv.beadsURL();
    const token = slackEnv.beadsToken();
    const ctx = await playwrightRequest.newContext({
      baseURL,
      extraHTTPHeaders: token ? { Authorization: `Bearer ${token}` } : {},
    });
    await use(ctx);
    await ctx.dispose();
  },

  // ── slackChannel: the invited test channel id (channel access) ────
  slackChannel: async ({}, use) => {
    const channel = slackEnv.channel();
    expect(
      channel,
      'SLACK_E2E_CHANNEL is unset — the deploy overlay must inject the test ' +
        'channel id the app was invited to (--set slackBridge.slack.channel=C0…). ' +
        'See gardenio/docs/slack-e2e-provisioning.md Part A3.',
    ).toBeTruthy();
    expect(
      CHANNEL_ID_RE.test(channel),
      `SLACK_E2E_CHANNEL="${channel}" is not a well-formed Slack channel id (C0…).`,
    ).toBeTruthy();
    await use(channel);
  },

  // ── workspace: authenticated Slack web client, scoped to the channel ─
  workspace: async ({ page, slackChannel }, use) => {
    // storageState is applied globally by playwright.config.js `use.storageState`
    // when SLACK_E2E_STORAGE_STATE is set. Navigate straight into the test
    // channel (the injected session skips the MFA-gated login form).
    await page.goto(channelURL(slackChannel), { waitUntil: 'domcontentloaded' });

    // Session-liveness assertion: the authenticated workspace DOM must mount.
    // If the signed-out login screen appears instead, the storageState cookies
    // have expired — surface the runbook pointer, don't wait out a timeout.
    const loaded = page.locator(qa.workspaceLoaded).first();
    const signedOut = page.locator(qa.signedOut).first();
    await expect(
      loaded.or(signedOut),
      'neither the workspace composer nor the signed-out screen appeared — ' +
        'Slack web client did not boot',
    ).toBeVisible({ timeout: 30_000 });

    if (await signedOut.isVisible().catch(() => false)) {
      throw new Error(
        `Slack landed on the signed-out screen for channel ${slackChannel}. ` +
          `${SESSION_EXPIRED_HINT} (session cookies checked: ` +
          `${SLACK_SESSION_COOKIES.join(', ')}).`,
      );
    }
    await expect(loaded).toBeVisible();

    await use(page);
  },
});

export { expect };
