// Tier-0 smoke — proves the real-user Slack E2E harness is wired end-to-end
// (task ka-0000004549; plan gardenio/plans/slack-e2e-playwright.md §Tier 0).
//
// This is deliberately MINIMAL. Its only job is to prove the scaffold + the
// slack-bridge fixtures (auth, channel access) work against a live rig:
//   1. the bridge is reachable + ready (bridge fixture),
//   2. the beads daemon answers (beads fixture),
//   3. the test channel id is well-formed and injected (slackChannel fixture),
//   4. the captured test-user session authenticates the Slack web client and
//      lands inside the test channel (workspace fixture).
//
// The real coverage — mention → agent card, decision-button clicks, modal
// submits, slash commands (Tiers 1–3) — lands in later specs, one file per
// concern. Do NOT bulk this file up; keep the harness itself the focus.

import { test, expect } from './fixtures.js';
import { qa } from './_slack.js';

test.describe('slack-e2e harness smoke', () => {
  test('slack-bridge is reachable and ready', async ({ bridge }) => {
    // The `bridge` fixture already asserts /healthz + /readyz before handing us
    // the context; re-read /healthz here to assert the response shape the
    // platform health page depends on (status + version).
    const res = await bridge.get('/healthz');
    expect(res.ok(), 'bridge /healthz').toBeTruthy();
    const body = await res.json();
    expect(body.status, 'bridge health status').toBe('ok');
    expect(body.version, 'bridge reports a version').toBeTruthy();
  });

  test('beads daemon answers a health probe', async ({ beads }) => {
    // The controller/bridge reactions Tier-1 specs assert on are read back from
    // the daemon; prove the daemon context is reachable + authorized. /v1/health
    // is the daemon's liveness endpoint.
    const res = await beads.get('/v1/health');
    expect(res.ok(), `beads /v1/health (status ${res.status()})`).toBeTruthy();
  });

  test('test channel id is well-formed and injected', async ({ slackChannel }) => {
    // The `slackChannel` fixture asserts the C0… shape; assert it is truthy here
    // so the "channel access" wiring is an explicit, named smoke case.
    expect(slackChannel, 'resolved test channel id').toBeTruthy();
    expect(slackChannel).toMatch(/^[CGD][A-Z0-9]{6,}$/);
  });

  test('captured session authenticates and lands in the test channel', async ({
    workspace,
  }) => {
    // The `workspace` fixture injects the storageState, navigates into the test
    // channel, and asserts the workspace DOM mounted (not the signed-out
    // screen). Here we just confirm the message composer — the "I am
    // authenticated and inside a channel" landmark — is interactable, which is
    // the precondition for every Tier-1 human-authored-message spec.
    await expect(workspace.locator(qa.messageInput).first()).toBeVisible();
  });
});
