// Slack test-user session guard — a liveness check + one clear error, usable
// from anywhere (task gd-6Jq5oR9fpd, epic gd-y28dPqgbgg "agents can see Slack").
//
// The captured `d`/`d-s` cookies expire on a rolling cadence. When they lapse,
// anything that browses Slack as the test user (the slack-ui E2E suite, a
// `gb slack view` screenshot tool, a UX-acceptance loop) lands on the Slack
// login screen and fails somewhere downstream looking like a flaky selector.
// This module is the single place that detects that and says so, in the one
// wording the runbook promises:
//
//   SESSION EXPIRED — refresh per slack-e2e-provisioning.md Part B
//
// Two layers, cheapest first:
//   1. checkStorageState()  — offline. Parses the storageState JSON and checks
//      both session cookies are present and not past their `expires`. No
//      browser, no network; safe in globalSetup and in any script.
//   2. probeLiveness()      — online. Loads the Slack web client with the
//      storageState in headless chromium and reports whether the workspace
//      mounted or the signed-out screen appeared. Catches server-side
//      revocation that the cookie timestamps cannot see.
//
// It is also a CLI (see usage below), so a non-Playwright caller can gate on it
// with just an exit code. It has no dependencies of its own; `playwright` is
// imported lazily and only for --probe.
//
//   node session-guard.mjs [--storage-state <path>] [--probe] [--channel <C0…>] [--json]
//
// Exit codes: 0 live (or soon-to-expire warning only) · 2 SESSION EXPIRED ·
// 3 misconfigured (no path / unreadable / unparseable file).

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { qa, channelURL, SLACK_SESSION_COOKIES } from './tests/_slack.js';

export const RUNBOOK = 'gardenio/docs/slack-e2e-provisioning.md';

// The one error string. Callers match on it; keep it byte-stable.
export const SESSION_EXPIRED_MESSAGE =
  'SESSION EXPIRED — refresh per slack-e2e-provisioning.md Part B';

// Warn (not fail) when a session cookie expires inside this window, so the
// operator can refresh before a run breaks.
export const DEFAULT_WARN_DAYS = 14;

// Thrown for "the session is dead". `code` lets callers branch without string
// matching; the message always leads with SESSION_EXPIRED_MESSAGE.
export class SessionExpiredError extends Error {
  constructor(detail) {
    super(detail ? `${SESSION_EXPIRED_MESSAGE} (${detail})` : SESSION_EXPIRED_MESSAGE);
    this.name = 'SessionExpiredError';
    this.code = 'SESSION_EXPIRED';
  }
}

// Thrown for "cannot even evaluate" — no path, unreadable, not JSON.
export class SessionConfigError extends Error {
  constructor(detail) {
    super(`Slack session check misconfigured: ${detail}`);
    this.name = 'SessionConfigError';
    this.code = 'SESSION_CONFIG';
  }
}

export function loadStorageState(path) {
  if (!path) {
    throw new SessionConfigError(
      'no storageState path — set SLACK_E2E_STORAGE_STATE or pass --storage-state ' +
        `(${RUNBOOK} Part A7)`,
    );
  }
  let raw;
  try {
    raw = readFileSync(path, 'utf8');
  } catch (err) {
    throw new SessionConfigError(`cannot read storageState at ${path}: ${err.message}`);
  }
  try {
    return JSON.parse(raw);
  } catch (err) {
    // A truncated/garbled capture is not a config mistake an operator can fix
    // by editing env — it needs a re-capture, so report it as expired.
    throw new SessionExpiredError(`storageState at ${path} is not valid JSON: ${err.message}`);
  }
}

// checkStorageState — pure, offline. `now` is injectable (ms) for tests.
// Returns { ok, problems[], warnings[], cookies: {name: {expires, daysLeft}} }.
// `ok` is false when a session cookie is missing or already expired.
export function checkStorageState(state, { now = Date.now(), warnDays = DEFAULT_WARN_DAYS } = {}) {
  const problems = [];
  const warnings = [];
  const cookies = {};
  const list = Array.isArray(state?.cookies) ? state.cookies : [];

  for (const name of SLACK_SESSION_COOKIES) {
    // A real capture can carry the same name on several domains; the session
    // is live if ANY copy is.
    const copies = list.filter((c) => c.name === name);
    if (copies.length === 0) {
      problems.push(`missing session cookie "${name}"`);
      continue;
    }
    // Playwright: expires is unix seconds, or -1 for a browser-session cookie.
    const live = copies.filter((c) => !(c.expires > 0) || c.expires * 1000 > now);
    if (live.length === 0) {
      const newest = Math.max(...copies.map((c) => c.expires));
      problems.push(`session cookie "${name}" expired ${new Date(newest * 1000).toISOString()}`);
      continue;
    }
    const finite = live.filter((c) => c.expires > 0).map((c) => c.expires);
    if (finite.length === 0) {
      cookies[name] = { expires: null, daysLeft: null };
      continue;
    }
    const expires = Math.max(...finite);
    const daysLeft = (expires * 1000 - now) / 86_400_000;
    cookies[name] = { expires, daysLeft };
    if (daysLeft < warnDays) {
      warnings.push(
        `session cookie "${name}" expires in ${daysLeft.toFixed(1)} days — refresh soon ` +
          `(${RUNBOOK} Part B)`,
      );
    }
  }
  return { ok: problems.length === 0, problems, warnings, cookies };
}

// assertStorageStateLive — offline check that throws SessionExpiredError.
// Returns the check result (with any warnings) when live.
export function assertStorageStateLive(state, opts) {
  const result = checkStorageState(state, opts);
  if (!result.ok) throw new SessionExpiredError(result.problems.join('; '));
  return result;
}

// probeLiveness — online. Opens the Slack web client in headless chromium with
// the storageState and decides: workspace mounted (live) vs signed-out screen
// (expired). Anything else (client never booted, network down) is NOT reported
// as expiry — it throws a plain Error so a Slack outage is not mistaken for a
// stale capture.
export async function probeLiveness({
  storageState,
  channel = process.env.SLACK_E2E_CHANNEL || '',
  timeoutMs = 30_000,
  launch,
} = {}) {
  if (!storageState) throw new SessionConfigError('probeLiveness needs a storageState path');
  const url = channel ? channelURL(channel) : channelURL('').replace(/\/$/, '');

  let browser;
  try {
    const chromium = launch ? { launch } : (await import('@playwright/test')).chromium;
    browser = await chromium.launch();
    const context = await browser.newContext({ storageState, locale: 'en-US' });
    const page = await context.newPage();
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: timeoutMs });

    const loaded = page.locator(qa.workspaceLoaded).first();
    const signedOut = page.locator(qa.signedOut).first();
    try {
      await loaded.or(signedOut).waitFor({ state: 'visible', timeout: timeoutMs });
    } catch {
      throw new Error(
        `Slack web client at ${url} neither mounted the workspace nor showed the ` +
          'signed-out screen — not reporting expiry (Slack outage or selector drift?)',
      );
    }
    if (await signedOut.isVisible().catch(() => false)) {
      throw new SessionExpiredError(`Slack showed the signed-out screen at ${url}`);
    }
    return { ok: true, url };
  } finally {
    await browser?.close().catch(() => {});
  }
}

// ── CLI ──────────────────────────────────────────────────────────────
function parseArgs(argv) {
  const out = { probe: false, json: false, storageState: process.env.SLACK_E2E_STORAGE_STATE || '' };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--probe') out.probe = true;
    else if (a === '--json') out.json = true;
    else if (a === '--storage-state') out.storageState = argv[++i] || '';
    else if (a === '--channel') out.channel = argv[++i] || '';
    else if (a === '--warn-days') out.warnDays = Number(argv[++i]);
    else throw new SessionConfigError(`unknown argument ${a}`);
  }
  return out;
}

export async function main(argv, { log = console.log, err = console.error } = {}) {
  let opts;
  const emit = (obj, text, stream = log) => (opts?.json ? log(JSON.stringify(obj)) : stream(text));
  try {
    opts = parseArgs(argv);
    const state = loadStorageState(opts.storageState);
    const result = assertStorageStateLive(state, { warnDays: opts.warnDays ?? DEFAULT_WARN_DAYS });
    if (opts.probe) await probeLiveness({ storageState: opts.storageState, channel: opts.channel });
    for (const w of result.warnings) if (!opts.json) err(`WARNING: ${w}`);
    emit(
      { status: 'live', probed: opts.probe, warnings: result.warnings, cookies: result.cookies },
      `Slack session live (${opts.probe ? 'cookies + browser probe' : 'cookies only'})`,
    );
    return 0;
  } catch (e) {
    if (e instanceof SessionExpiredError) {
      emit({ status: 'expired', error: e.message }, e.message, err);
      return 2;
    }
    emit({ status: 'error', error: e.message }, e.message, err);
    return 3;
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.exitCode = await main(process.argv.slice(2));
}
