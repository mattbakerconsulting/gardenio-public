#!/usr/bin/env node
// Screenshot a Slack channel or thread in the web client and dump its visible text.
//
// Lets an agent see how its Slack messages actually render (UX acceptance and
// revisions). It loads a Playwright storageState for a test-user session,
// opens the target, waits for messages, then captures four screenshots
// (desktop 1280x800 and mobile 390x844, each in light and dark) plus a JSON
// file with the visible message text.
//
// Usage:
//   node tools/slack-view/slack-view.mjs --storage-state <file> --out <dir> \
//        <target>
//
// <target> is one of:
//   https://app.slack.com/client/<team>/<channel>        channel view
//   https://<ws>.slack.com/archives/<channel>/p<ts>      message / thread permalink
//   <team>/<channel>                                     shorthand for the first form
//   <channel>  (with --team, or SLACK_TEAM_ID)           shorthand for the first form
// A permalink carrying ?thread_ts= is rewritten to the client thread URL when
// the team id is known (--team / SLACK_TEAM_ID); otherwise it is opened as
// given and Slack's own redirect is followed. Any other http(s) or file: URL
// is opened as-is, which is how the hermetic self-test drives a fixture page.
//
// Options:
//   --storage-state <file>  Playwright storageState JSON (default $SLACK_STORAGE_STATE)
//   --out <dir>             output directory (default ./slack-view-out/<UTC timestamp>)
//   --team <T…>             Slack team id, for shorthand targets and thread rewrites
//   --timeout <ms>          wait for messages to appear (default 30000)
//   --settle <ms>           extra wait after messages appear, for images/emoji (default 1500)
//   --full-page             full-page screenshots instead of the viewport
//   --message-selector <s>  override the message container selector
//   --scope-selector <s>    restrict extraction to this subtree (default: thread pane for thread targets)
//
// Output (in --out):
//   desktop-light.png desktop-dark.png mobile-light.png mobile-dark.png
//   messages.json   { target, url, capturedAt, scope, messages[], screenshots[] }
// The final stdout line is the absolute path of messages.json, so a wrapper can
// read it; progress and errors go to stderr.
//
// Exit codes: 0 ok, 2 usage, 3 not signed in (storageState missing/expired),
// 4 no messages appeared within --timeout, 1 anything else.
//
// The storageState file holds session cookies. It is read, never copied into
// --out and never printed.
//
// Requires playwright (resolved from node_modules, then the agent image's
// global install) and a Chromium (PLAYWRIGHT_BROWSERS_PATH, or CHROME_PATH).

import fs from 'node:fs/promises';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

export const VIEWPORTS = [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'mobile', width: 390, height: 844 },
];
export const SCHEMES = ['light', 'dark'];

// Slack web client selectors. They are data-qa hooks Slack uses for its own
// e2e tests, which churn far less than the hashed class names, but they are
// still not a contract: override with --message-selector if Slack moves them.
export const DEFAULTS = {
  messageSelector: '[data-qa="message_container"]',
  threadScopeSelector: '[data-qa="threads_flexpane"], [data-qa="thread_view"]',
  timeout: 30000,
  settle: 1500,
};

const SIGNIN_URL_RE = /\/(signin|sign_in|ssb\/signin|workspace-signin)\b/;
const SIGNIN_FORM_SELECTOR = '[data-qa="signin_form"], form#signin_form, input[type="email"][name="email"]';

export class UsageError extends Error {}

const FLAGS = new Set(['full-page', 'help']);
const VALUE_OPTS = new Set([
  'storage-state', 'out', 'team', 'timeout', 'settle', 'message-selector', 'scope-selector',
]);

export function parseArgs(argv, env = process.env) {
  const opts = {
    storageState: env.SLACK_STORAGE_STATE,
    team: env.SLACK_TEAM_ID,
    timeout: DEFAULTS.timeout,
    settle: DEFAULTS.settle,
    fullPage: false,
    messageSelector: DEFAULTS.messageSelector,
  };
  const positional = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith('--')) {
      positional.push(a);
      continue;
    }
    const [rawName, inline] = a.slice(2).split(/=(.*)/s, 2);
    if (FLAGS.has(rawName)) {
      opts[camel(rawName)] = true;
    } else if (VALUE_OPTS.has(rawName)) {
      const v = inline ?? argv[++i];
      if (v === undefined) throw new UsageError(`--${rawName} needs a value`);
      opts[camel(rawName)] = v;
    } else {
      throw new UsageError(`unknown option --${rawName}`);
    }
  }
  if (opts.help) return opts;
  if (positional.length !== 1) throw new UsageError('exactly one <target> is required');
  opts.target = positional[0];
  for (const k of ['timeout', 'settle']) {
    opts[k] = Number(opts[k]);
    if (!Number.isFinite(opts[k]) || opts[k] < 0) throw new UsageError(`--${k} must be a non-negative number`);
  }
  if (!opts.storageState) throw new UsageError('--storage-state (or SLACK_STORAGE_STATE) is required');
  return opts;
}

function camel(s) {
  return s.replace(/-(\w)/g, (_, c) => c.toUpperCase());
}

// resolveTarget turns the user-facing target into the URL to open, and says
// whether it points at a thread (which changes where messages are read from).
export function resolveTarget(target, team) {
  const channelRe = /^[CGD][A-Z0-9]{6,}$/;
  const teamRe = /^T[A-Z0-9]{6,}$/;

  const short = target.split('/');
  if (short.length === 2 && teamRe.test(short[0]) && channelRe.test(short[1])) {
    return { url: `https://app.slack.com/client/${short[0]}/${short[1]}`, thread: false };
  }
  if (channelRe.test(target)) {
    if (!team) throw new UsageError(`channel id ${target} needs --team (or SLACK_TEAM_ID)`);
    return { url: `https://app.slack.com/client/${team}/${target}`, thread: false };
  }

  let u;
  try {
    u = new URL(target);
  } catch {
    throw new UsageError(`cannot interpret target ${JSON.stringify(target)}`);
  }
  if (!['http:', 'https:', 'file:'].includes(u.protocol)) {
    throw new UsageError(`unsupported target scheme ${u.protocol}`);
  }

  const clientThread = u.pathname.match(/\/client\/[^/]+\/[^/]+\/thread\//);
  const archive = u.pathname.match(/^\/archives\/([CGD][A-Z0-9]+)\/p(\d{10})(\d{6})\/?$/);
  if (archive && team) {
    const [, channel, sec, micro] = archive;
    const threadTs = u.searchParams.get('thread_ts') ?? `${sec}.${micro}`;
    return {
      url: `https://app.slack.com/client/${team}/${channel}/thread/${channel}-${threadTs}`,
      thread: true,
    };
  }
  return { url: u.href, thread: Boolean(clientThread || (archive && u.searchParams.has('thread_ts'))) };
}

// loadPlaywright prefers a project-local install, then the agent image's global one.
async function loadPlaywright() {
  const attempts = [];
  const roots = [import.meta.url, pathToFileURL(path.join(process.cwd(), '/')).href];
  for (const root of roots) {
    try {
      return createRequire(root)('playwright');
    } catch (e) {
      attempts.push(e.message.split('\n')[0]);
    }
  }
  for (const dir of [process.env.PLAYWRIGHT_MODULE, '/usr/local/lib/node_modules/playwright'].filter(Boolean)) {
    try {
      return createRequire(import.meta.url)(dir);
    } catch (e) {
      attempts.push(e.message.split('\n')[0]);
    }
  }
  throw new Error(`playwright not found (${attempts.join('; ')})`);
}

// extractMessages runs in the page. It is a plain function so Playwright can
// serialise it; it must not close over anything.
export function extractMessages({ messageSelector, scopeSelector }) {
  const scope = (scopeSelector && document.querySelector(scopeSelector)) || document;
  const text = (el) => (el ? el.innerText.trim() : '');
  const inView = (el) => {
    const r = el.getBoundingClientRect();
    return r.bottom > 0 && r.top < window.innerHeight && r.right > 0 && r.left < window.innerWidth;
  };
  // Slack renders consecutive posts from one author as separate containers
  // with the sender name only on the first, so carry the author forward.
  let lastAuthor = null;
  const messages = [...scope.querySelectorAll(messageSelector)].map((el) => {
    const sender = text(el.querySelector('[data-qa="message_sender_name"], [data-qa="message_sender"]'));
    if (sender) lastAuthor = sender;
    const ts = el.querySelector('[data-ts], a.c-timestamp');
    return {
      author: sender || lastAuthor,
      ts: ts?.getAttribute('data-ts') ?? null,
      time: ts ? ts.getAttribute('aria-label') || text(ts) : null,
      text: text(el.querySelector('[data-qa="message-text"], .p-rich_text_section')) || text(el),
      reactions: [...el.querySelectorAll('[data-qa="reaction"]')].map(
        (r) => r.getAttribute('aria-label') || text(r),
      ),
      inViewport: inView(el),
    };
  });
  return {
    scoped: scope !== document,
    title: document.title,
    messages,
    visibleText: (scope === document ? document.body : scope).innerText.trim(),
  };
}

async function waitForMessages(page, selector, timeout) {
  // Race the message list against a signed-out page, so an expired session
  // fails fast with its own exit code instead of timing out as "no messages".
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (SIGNIN_URL_RE.test(new URL(page.url()).pathname)) throw new AuthError(page.url());
    if (await page.locator(SIGNIN_FORM_SELECTOR).count()) throw new AuthError(page.url());
    if (await page.locator(selector).count()) return;
    await page.waitForTimeout(250);
  }
  throw new NoMessagesError(selector, timeout);
}

class AuthError extends Error {
  constructor(url) {
    super(`not signed in: ${new URL(url).origin}${new URL(url).pathname} is a sign-in page (storageState missing or expired)`);
  }
}
class NoMessagesError extends Error {
  constructor(selector, timeout) {
    super(`no element matching ${selector} appeared within ${timeout}ms`);
  }
}

export async function run(opts) {
  const { url, thread } = resolveTarget(opts.target, opts.team);
  const scopeSelector = opts.scopeSelector ?? (thread ? DEFAULTS.threadScopeSelector : undefined);

  let storageState;
  try {
    storageState = JSON.parse(await fs.readFile(opts.storageState, 'utf8'));
  } catch (e) {
    throw new UsageError(`cannot read storageState ${opts.storageState}: ${e.code ?? e.message}`);
  }

  const outDir = path.resolve(opts.out ?? path.join('slack-view-out', new Date().toISOString().replace(/[:.]/g, '-')));
  await fs.mkdir(outDir, { recursive: true });

  const { chromium } = await loadPlaywright();
  const browser = await chromium.launch({
    executablePath: process.env.CHROME_PATH || undefined,
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  try {
    const context = await browser.newContext({
      storageState,
      viewport: { width: VIEWPORTS[0].width, height: VIEWPORTS[0].height },
      colorScheme: 'light',
      locale: 'en-US',
    });
    const page = await context.newPage();
    log(`opening ${url}`);
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: opts.timeout });
    await waitForMessages(page, opts.messageSelector, opts.timeout);
    await page.waitForLoadState('networkidle', { timeout: 5000 }).catch(() => {});
    await page.waitForTimeout(opts.settle);

    const screenshots = [];
    let capture;
    for (const vp of VIEWPORTS) {
      for (const scheme of SCHEMES) {
        // Resize + emulateMedia re-lays-out the loaded app without reloading,
        // which keeps the thread/scroll position and avoids four sign-in checks.
        await page.setViewportSize({ width: vp.width, height: vp.height });
        await page.emulateMedia({ colorScheme: scheme });
        await page.waitForTimeout(500);
        const file = `${vp.name}-${scheme}.png`;
        await page.screenshot({ path: path.join(outDir, file), fullPage: opts.fullPage });
        screenshots.push({ file, viewport: vp.name, width: vp.width, height: vp.height, colorScheme: scheme });
        log(`wrote ${file}`);
        // Messages are read from the first (desktop, light) layout: it is the
        // widest, so the least text is hidden behind truncation.
        capture ??= await page.evaluate(extractMessages, {
          messageSelector: opts.messageSelector,
          scopeSelector,
        });
      }
    }

    const result = {
      target: opts.target,
      url: page.url(),
      capturedAt: new Date().toISOString(),
      title: capture.title,
      scope: capture.scoped ? scopeSelector : 'page',
      messages: capture.messages,
      visibleText: capture.visibleText,
      screenshots,
    };
    const jsonPath = path.join(outDir, 'messages.json');
    await fs.writeFile(jsonPath, JSON.stringify(result, null, 2) + '\n');
    return jsonPath;
  } finally {
    await browser.close();
  }
}

function log(msg) {
  process.stderr.write(`slack-view: ${msg}\n`);
}

const USAGE = `usage: slack-view.mjs --storage-state <file> [--out <dir>] [--team <T…>] [--timeout ms]
                      [--settle ms] [--full-page] [--message-selector s] [--scope-selector s] <target>
(see the header of this file for target forms and output layout)`;

async function main() {
  let opts;
  try {
    opts = parseArgs(process.argv.slice(2));
    if (opts.help) {
      console.log(USAGE);
      return 0;
    }
    console.log(await run(opts));
    return 0;
  } catch (e) {
    if (e instanceof UsageError) {
      log(e.message);
      console.error(USAGE);
      return 2;
    }
    log(e.message);
    if (e instanceof AuthError) return 3;
    if (e instanceof NoMessagesError) return 4;
    return 1;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(await main());
}
