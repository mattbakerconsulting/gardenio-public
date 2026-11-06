// Centralized helpers + selectors + env contract for the real-user Slack E2E
// suite. One module so DOM selectors and env plumbing live in exactly one place
// (plan gardenio/plans/slack-e2e-playwright.md §Risks: "keep selectors
// centralized in one helpers module" — app.slack.com markup churns, so pin to
// Slack's `data-qa` attributes and resilient role/text locators, never CSS
// class names).
//
// ── Env contract ─────────────────────────────────────────────────────
// The module NEVER reads secrets off disk itself. Everything is handed in as
// env by the module runner gardenio/tests/e2e/scripts/test-slack-ui.sh, which
// gates the whole rig (scaffold present, node/npx, live bridge, materialized
// storageState with `d`/`d-s` cookies) and SKIP-passes when any piece is
// absent. The names below are that runner's export contract — keep them in
// sync with test-slack-ui.sh.
//
//   SLACK_E2E_STORAGE_STATE  path to the captured test-user storageState JSON
//                            (Playwright browser session; `d`/`d-s` cookies).
//                            Consumed by playwright.config.js `use.storageState`.
//   SLACK_E2E_BRIDGE_URL     base URL of the ephemeral slack-bridge under test
//                            (Socket Mode attach point; exposes /healthz + /readyz).
//   SLACK_E2E_CHANNEL        C0… id of the test channel the app was invited to.
//                            Optional — globalSetup resolves + asserts it.
//   BEADS_HTTP_URL           beads daemon HTTP base URL (assert bead reactions).
//   BEADS_AUTH_TOKEN         bearer for the daemon (optional; empty in bypass).
//   SLACK_E2E_WORKSPACE_URL  Slack web client base (default https://app.slack.com).
//   SLACK_E2E_BOT_HANDLE     bot's Slack display name typed after `@` to summon
//                            it (default "gardenio"); override when the test app
//                            was installed under a different display name.

const SLACK_WEB_BASE = process.env.SLACK_E2E_WORKSPACE_URL || 'https://app.slack.com';

// Read a required env var, throwing a precise, actionable error when absent.
// A missing var here means the runner's env contract drifted — the message
// points back at test-slack-ui.sh rather than surfacing an opaque `undefined`.
export function requireEnv(name) {
  const v = process.env[name];
  if (!v) {
    throw new Error(
      `${name} is not set. The slack-e2e suite is driven by ` +
        'gardenio/tests/e2e/scripts/test-slack-ui.sh, which exports the full env ' +
        'contract (SLACK_E2E_STORAGE_STATE / SLACK_E2E_BRIDGE_URL / BEADS_HTTP_URL / ' +
        'SLACK_E2E_CHANNEL). Run the suite via that module, or export the vars by hand.',
    );
  }
  return v;
}

// Read an optional env var with a fallback.
export function optionalEnv(name, fallback = '') {
  return process.env[name] || fallback;
}

// Env accessors — one function per contract var so a typo fails at the call
// site, and so the env → config mapping is greppable from the test files.
export const slackEnv = {
  storageState: () => optionalEnv('SLACK_E2E_STORAGE_STATE'),
  bridgeURL: () => requireEnv('SLACK_E2E_BRIDGE_URL').replace(/\/+$/, ''),
  channel: () => optionalEnv('SLACK_E2E_CHANNEL'),
  beadsURL: () => optionalEnv('BEADS_HTTP_URL', 'http://localhost:19090').replace(/\/+$/, ''),
  beadsToken: () => optionalEnv('BEADS_AUTH_TOKEN'),
  webBase: () => SLACK_WEB_BASE.replace(/\/+$/, ''),
};

// The two Slack session cookies that make a storageState a valid test-user
// session. Their absence is THE canonical "session expired / malformed
// capture" signal — surface it with a pointer to the refresh runbook rather
// than letting auth silently land on the Slack login screen and flake a
// selector downstream (gardenio/docs/slack-e2e-provisioning.md Part B).
export const SLACK_SESSION_COOKIES = ['d', 'd-s'];

// SESSION_EXPIRED_HINT — appended to any auth/session failure so a red run
// points an operator straight at the refresh procedure.
// Keep the wording in lockstep with SESSION_EXPIRED_MESSAGE in
// ../session-guard.mjs (the standalone liveness check).
export const SESSION_EXPIRED_HINT =
  'SESSION EXPIRED — refresh per slack-e2e-provisioning.md Part B ' +
  '(re-capture the test-user storageState; gardenio/docs/slack-e2e-provisioning.md).';

// Centralized Slack web-client selectors. Slack ships stable `data-qa`
// attributes on most interactive surfaces; prefer them over CSS classes, which
// are hashed and churn release-to-release. When a `data-qa` is unavailable,
// fall back to an ARIA role + accessible name. Keep EVERY app.slack.com
// selector here so a Slack DOM change is a one-file fix (plan §Risks 2).
export const qa = {
  // The workspace has loaded (not the signed-out login screen). The message
  // input composer is the cheapest "we are authenticated and inside a channel"
  // landmark; the client boot spinner clears before it mounts.
  workspaceLoaded: '[data-qa="message_input"]',
  // The signed-out landing / login form — its presence means the storageState
  // did not authenticate (expired cookies). Used to produce the SESSION_EXPIRED
  // hint instead of an opaque timeout.
  signedOut: '[data-qa="signin_button"], [data-qa="signin_form"]',
  // Message composer + send.
  messageInput: '[data-qa="message_input"]',
  // Rendered messages (the virtualized message list rows).
  messageRow: '[data-qa="virtual-list-item"]',
  message: '[data-qa="message_container"]',
  // Composer autocomplete/typeahead popover — Slack surfaces it when you type
  // `@<partial>` in the message input. Selecting a real member from it inserts
  // a proper `<@Uxxx>` user link (a plain-text "@gardenio" does NOT fire an
  // app_mention — the token must resolve to the bot user). Highlighted row is
  // what Enter/Tab commits.
  autocompleteMenu: '[data-qa="autocomplete_list"], [data-qa="texty_composer_suggestions"]',
  autocompleteOption: '[data-qa="autocomplete_option"], [role="option"]',
  autocompleteOptionSelected:
    '[data-qa="autocomplete_option"][aria-selected="true"], [role="option"][aria-selected="true"]',
  // Block Kit action button inside a bot message (agent/decision cards).
  blockKitButton: 'button[data-qa="bk_button"]',
  // Modal (view) dialog opened by an action / slash command.
  modal: '[data-qa="dialog"], [role="dialog"]',
  modalSubmit: '[data-qa="dialog_go"]',
};

// The gardenio bot's Slack display name — what a human types after `@` to summon
// it (`@gardenio <prompt>`). Defaults to "gardenio"; override per-workspace via
// SLACK_E2E_BOT_HANDLE when the test app was installed under a different name.
export function botHandle() {
  return optionalEnv('SLACK_E2E_BOT_HANDLE', 'gardenio');
}

// Marker text the slack-bridge stamps on every agent status card
// (buildAgentCardBlocks in controller/internal/bridge/bot_agents.go): the
// context line renders "`<agent-slug>` · Decisions thread below". This substring
// is the DOM-stable proof an agent card mounted, independent of the agent's
// name, state glyph, or role. Kept here so a card-copy change is a one-file fix.
export const AGENT_CARD_MARKER = 'Decisions thread below';

// Deep link into a specific channel of the test workspace via the Slack web
// client. Playwright authenticates by injecting the storageState (config
// use.storageState), so navigating straight to the channel skips the login
// form entirely (which is MFA/email-code gated and bot-hostile — the whole
// reason we inject a captured session).
export function channelURL(channelId, teamId = process.env.SLACK_E2E_TEAM_ID || '') {
  const base = slackEnv.webBase();
  if (teamId) return `${base}/client/${teamId}/${channelId}`;
  // Without a team id, /client resolves to the user's default workspace and
  // then the channel — enough for a single-workspace test rig.
  return `${base}/client/-/${channelId}`;
}
