// Playwright globalSetup for the real-user Slack E2E suite (task ka-0000004549).
//
// This runs ONCE before any spec and is the suite's fail-fast gate. It does NOT
// seed a daemon (unlike beads-viewer-e2e/globalSetup.mjs) — there is nothing
// local to seed. Instead it validates the env contract handed in by the module
// runner (gardenio/tests/e2e/scripts/test-slack-ui.sh) and, most importantly,
// asserts the captured storageState still carries live Slack session cookies.
//
// Why a dedicated up-front check: the `d`/`d-s` cookies expire on a rolling
// cadence (weeks-to-months), and an expired capture makes EVERY spec land on
// the Slack login screen and time out on a selector. That reads like a flaky
// suite. Detecting expiry here — once — turns it into a single, unmistakable
// "SESSION EXPIRED — refresh per slack-e2e-provisioning.md Part B" failure, exactly as
// gardenio/docs/slack-e2e-provisioning.md Part B specifies is this task's job.
//
// The runner already SKIP-passes the whole module when the rig is absent, so by
// the time globalSetup runs the vars are expected to be present. If a var is
// nonetheless missing (someone ran `npm test` by hand), fail with a pointer at
// the runner rather than deep inside a spec.

import { SLACK_SESSION_COOKIES as SESSION_COOKIES } from './tests/_slack.js';
import {
  loadStorageState,
  assertStorageStateLive,
  SessionExpiredError,
} from './session-guard.mjs';

function fail(msg) {
  throw new Error(`[slack-e2e globalSetup] ${msg}`);
}

export default function globalSetup() {
  // 1. Bridge URL is mandatory — every spec asserts against the bridge.
  const bridgeURL = process.env.SLACK_E2E_BRIDGE_URL;
  if (!bridgeURL) {
    fail(
      'SLACK_E2E_BRIDGE_URL is unset. Drive this suite via ' +
        'gardenio/tests/e2e/scripts/test-slack-ui.sh (module slack-ui), which ' +
        'exports the full env contract, or export the vars by hand.',
    );
  }

  // 2. storageState presence + shape. Absent path → the runner would normally
  //    SKIP; if we got here without one, it is a misconfiguration, not a flake.
  const ssPath = process.env.SLACK_E2E_STORAGE_STATE;
  if (!ssPath) {
    fail(
      'SLACK_E2E_STORAGE_STATE is unset — the captured test-user session was ' +
        'not materialized. Check externalSecrets.slackE2ESession.remoteRef ' +
        '(gardenio/docs/slack-e2e-provisioning.md Part A7).',
    );
  }

  // 3. Session-liveness (shared with the standalone CLI, session-guard.mjs):
  //    both `d`/`d-s` cookies present AND not past their `expires`. Either
  //    failing is the canonical "SESSION EXPIRED — refresh per
  //    slack-e2e-provisioning.md Part B" signal.
  try {
    const { warnings } = assertStorageStateLive(loadStorageState(ssPath));
    for (const w of warnings) console.warn(`[slack-e2e globalSetup] WARNING: ${w}`);
  } catch (err) {
    if (err instanceof SessionExpiredError) fail(err.message);
    fail(`${err.message}. Re-capture per slack-e2e-provisioning.md Part B.`);
  }

  console.log(
    `[slack-e2e globalSetup] env contract OK — bridge=${bridgeURL}, ` +
      `storageState has ${SESSION_COOKIES.join('/')} cookies, ` +
      `channel=${process.env.SLACK_E2E_CHANNEL || '<unset — resolved per-spec>'}`,
  );
}
