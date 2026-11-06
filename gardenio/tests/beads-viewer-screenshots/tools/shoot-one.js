// shoot-one.js — reusable single-page beads-viewer screenshot tool.
//
// Shoots ONE beads-viewer route at both canonical CDQR viewports in a single
// invocation — desktop (1440x900) and narrow (430x932) — writing
//   <out>/desktop/<name>.png
//   <out>/narrow/<name>.png
//
// Every per-route CDQR audit (gardener, and any future single-page audit) can
// call this instead of hand-rolling a fresh playwright invocation each pour.
// It supersedes the gardener-specific, positional-arg `capture-gardener.mjs` with
// a flag-driven, `--name`-parameterized, multi-viewport-in-one-run contract.
//
// The OIDC-gated viewer requires an Authorization bearer header, which the
// built-in `playwright screenshot` CLI cannot attach — so this drives chromium
// directly with `extraHTTPHeaders`.
//
// Wait strategy: many beads-viewer routes hold an open SSE/htmx-poll connection
// (the gardener page polls, so the network is NEVER idle). So we attempt
// `networkidle` but TOLERATE its timeout, and ALSO wait for a configurable
// readiness `--selector` (default `.gardener-iter`). Whichever resolves, we then
// let in-flight htmx swaps settle before shooting. This makes the tool correct
// for both quiescent pages (networkidle fires) and live-polling pages (selector
// fires, networkidle times out harmlessly).
//
// Usage:
//   node tools/shoot-one.js --url <url> --out <dir> --name <name> [flags]
//
// Flags:
//   --url <url>          (required) page URL to capture
//   --out <dir>          (required) output base dir; PNGs land under desktop/ + narrow/
//   --name <name>        (required) basename (without .png) for each viewport shot
//   --bearer <token>     Authorization bearer token (or via BEADS_VIEWER_BEARER)
//   --selector <css>     readiness selector to wait for (default `.gardener-iter`);
//                        pass `--selector ''` to skip selector-waiting entirely
//   --selector-timeout <ms>   how long to wait for --selector (default 8000)
//   --settle <ms>        extra settle after readiness, for htmx swaps (default 1500)
//   --full-page          capture the full scrollable page instead of the viewport
//   --dump-html <path>   write the final rendered HTML to <path>-<viewport>.html
import { chromium } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';

// The two canonical CDQR viewports. Keep in lock-step with the audit spec.
const VIEWPORTS = [
  { key: 'desktop', width: 1440, height: 900 },
  { key: 'narrow', width: 430, height: 932 },
];

function parseArgs(argv) {
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith('--')) continue;
    const key = a.slice(2);
    // Boolean flags take no value.
    if (key === 'full-page') {
      flags[key] = true;
      continue;
    }
    const val = argv[i + 1];
    if (val === undefined || val.startsWith('--')) {
      // Allow an explicit empty value only when the next token is another flag.
      flags[key] = '';
      continue;
    }
    flags[key] = val;
    i++;
  }
  return flags;
}

const flags = parseArgs(process.argv.slice(2));
const url = flags.url;
const outDir = flags.out;
const name = flags.name;
if (!url || !outDir || !name) {
  console.error(
    'usage: node tools/shoot-one.js --url <url> --out <dir> --name <name> '
    + '[--bearer <t>] [--header \'K: V\' ...] [--selector <css>] '
    + '[--selector-timeout <ms>] [--settle <ms>] [--full-page] [--dump-html <path>]');
  process.exit(2);
}

const bearer = flags.bearer || process.env.BEADS_VIEWER_BEARER || '';
// `--header 'K: V'` (repeatable) attaches an arbitrary request header, e.g. the
// dev-bypass edge identity `X-Auth-Request-Email: <op>` so an operator-gated
// surface (the gardener operate bar) renders. Compose with or instead of
// `--bearer`. parseArgs keeps only the LAST value per flag, so collect repeats
// from argv directly.
function collectHeaders(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] !== '--header') continue;
    const raw = argv[i + 1];
    if (!raw || raw.startsWith('--')) continue;
    const idx = raw.indexOf(':');
    if (idx <= 0) continue;
    out[raw.slice(0, idx).trim()] = raw.slice(idx + 1).trim();
  }
  return out;
}
const extraHeaders = collectHeaders(process.argv.slice(2));
// `--selector` defaults to the gardener life-thread card; an explicit empty
// string (`--selector ''`) opts out of selector-waiting.
const selector = flags.selector === undefined ? '.gardener-iter' : flags.selector;
const selectorTimeout = parseInt(flags['selector-timeout'] || '8000', 10);
const settleMs = parseInt(flags.settle || '1500', 10);
const fullPage = !!flags['full-page'];
const dumpHtml = flags['dump-html'] || '';

async function shoot(browser, viewport) {
  const dir = path.join(outDir, viewport.key);
  fs.mkdirSync(dir, { recursive: true });
  const out = path.join(dir, `${name}.png`);

  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    ignoreHTTPSErrors: true,
    extraHTTPHeaders: {
      ...(bearer ? { Authorization: `Bearer ${bearer}` } : {}),
      ...extraHeaders,
    },
    reducedMotion: 'reduce',
    colorScheme: 'light',
  });
  try {
    const page = await context.newPage();
    // Land the DOM first, then RACE networkidle against the readiness selector:
    // a live-polling page never goes idle, so we must not block on networkidle
    // alone — but a quiescent page may never render the selector, so we must not
    // block on the selector alone either.
    const resp = await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 60000 });
    if (resp && resp.status() >= 400) {
      console.error(`shoot-one: ${url} returned HTTP ${resp.status()}`);
      process.exit(1);
    }

    const waiters = [
      page.waitForLoadState('networkidle', { timeout: selectorTimeout }).then(
        () => 'networkidle',
        () => null,
      ),
    ];
    if (selector) {
      waiters.push(
        page.waitForSelector(selector, { timeout: selectorTimeout }).then(
          () => 'selector',
          () => null,
        ),
      );
    }
    const settledBy = (await Promise.all(waiters)).filter(Boolean);
    if (settledBy.length === 0) {
      console.error(
        `shoot-one[${viewport.key}]: neither networkidle nor `
        + `${selector ? `selector \`${selector}\`` : 'a selector'} within `
        + `${selectorTimeout}ms — capturing as-is`);
    }

    // For full-page shots, scroll the whole document so any `intersect once`
    // (IntersectionObserver) lazy sections fire, then return to the top.
    if (fullPage) {
      await page.evaluate(async () => {
        const step = Math.max(400, Math.floor(window.innerHeight * 0.9));
        for (let y = 0; y <= document.body.scrollHeight; y += step) {
          window.scrollTo(0, y);
          await new Promise((r) => setTimeout(r, 120));
        }
        window.scrollTo(0, 0);
      });
    }

    // Let any in-flight htmx swaps settle before the shot.
    await page.waitForTimeout(settleMs);

    if (dumpHtml) {
      const htmlPath = `${dumpHtml}-${viewport.key}.html`;
      fs.writeFileSync(htmlPath, await page.content());
      console.error(`shoot-one[${viewport.key}]: wrote HTML ${htmlPath}`);
    }
    await page.screenshot({ path: out, fullPage });
    console.error(`shoot-one[${viewport.key}]: wrote ${out}`);
  } finally {
    await context.close();
  }
}

// Allow pinning to a pre-installed chromium in environments where the bundled
// browser download is unavailable (agent pods ship a fixed /ms-playwright build
// that may not match the npm playwright package's expected revision). Honor
// PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH if set.
const chromiumExecutablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined;
const browser = await chromium.launch(
  chromiumExecutablePath ? { executablePath: chromiumExecutablePath } : {});
try {
  // Sequential (not parallel) so one shared browser process is enough and the
  // console log reads cleanly per viewport.
  for (const viewport of VIEWPORTS) {
    await shoot(browser, viewport);
  }
} finally {
  await browser.close();
}
