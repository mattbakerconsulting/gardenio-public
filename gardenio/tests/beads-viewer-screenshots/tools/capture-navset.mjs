// capture-navset.mjs — F8 (ka-0000005211) before/after nav-consistency capture.
// Boots the beads-viewer binary at $VIEWER_BIN against the live (port-forwarded)
// prod daemon under dev-auth-bypass, captures the nav-consistency pages at two
// viewports, then tears down. Run once per variant (before/after).
//
// Env:
//   VIEWER_BIN     path to the beads-viewer binary to boot
//   BEADS_HTTP_ADDR   daemon url (http://localhost:8080)
//   BEADS_DAEMON_TOKEN  bearer for the daemon
//   VARIANT        'before' | 'after' — output subdir
//   OUT_ROOT       output root dir
//   CHROME_PATH    chromium executable
import { chromium } from '@playwright/test';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import http from 'node:http';

const VIEWER_BIN = process.env.VIEWER_BIN;
const DAEMON = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
const TOKEN = process.env.BEADS_DAEMON_TOKEN || '';
const VARIANT = process.env.VARIANT || 'after';
const OUT_ROOT = process.env.OUT_ROOT || 'test-results/navset';
const CHROME = process.env.CHROME_PATH || undefined;
const VIEWER_PORT = parseInt(process.env.VIEWER_PORT || '8095', 10);
const VIEWER_URL = `http://localhost:${VIEWER_PORT}`;

// The nav-consistency pages that carry a second-row filter/sub-nav strip AND
// changed between the before/after commits (git diff aa92421d9..HEAD on the
// templates dir): agents, triggers, project, ratings, plans, extern. The
// index (/) mounts a long-lived SSE channel so it is captured in 'load' mode.
// NOTE: /inbox now 308s to the per-user dashboard (/u/me?view=dashboard),
// which needs a resolvable OIDC identity dev-auth-bypass does not provide —
// so the legacy inbox filter-strip is not reachable in this harness and is
// omitted (documented in the capture manifest).
const PAGES = [
  { name: 'index',       url: '/',            sse: true },
  { name: 'agents',      url: '/agents',      sse: true },
  { name: 'triggers',    url: '/triggers' },
  { name: 'project',     url: '/p/gardenio' },
  { name: 'ratings',     url: '/ratings' },
  { name: 'plans',       url: '/plans' },
  { name: 'extern',      url: '/extern' },
];

const VIEWPORTS = [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'narrow',  width: 480,  height: 800 },
];

function log(...a) { console.error(`[capture-navset:${VARIANT}]`, ...a); }

function waitHealthy(url, tries = 120, delayMs = 500) {
  return new Promise((resolve, reject) => {
    let n = 0;
    const tick = () => {
      const req = http.get(url + '/healthz', (res) => {
        res.resume();
        resolve(true);
      });
      req.on('error', () => {
        if (++n >= tries) return reject(new Error('viewer never came up'));
        setTimeout(tick, delayMs);
      });
      req.setTimeout(1000, () => { req.destroy(); });
    };
    tick();
  });
}

async function freezeRendering(page) {
  await page.addStyleTag({ content: `
    *, *::before, *::after {
      animation-duration: 0s !important; animation-delay: 0s !important;
      transition-duration: 0s !important; transition-delay: 0s !important;
      caret-color: transparent !important; scroll-behavior: auto !important;
    }
    html { scroll-behavior: auto !important; }
  `});
}

// --- boot viewer ---
log('booting viewer', VIEWER_BIN, '->', DAEMON);
const viewer = spawn(VIEWER_BIN, [], {
  env: {
    ...process.env,
    BEADS_VIEWER_DEV_AUTH_BYPASS: '1',
    BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD: '1',
    BEADS_HTTP_ADDR: DAEMON,
    BEADS_DAEMON_TOKEN: TOKEN,
    LISTEN_ADDR: `:${VIEWER_PORT}`,
    LOG_LEVEL: 'warn',
  },
  stdio: ['ignore', 'inherit', 'inherit'],
});

let exitCode = 0;
try {
  await waitHealthy(VIEWER_URL);
  log('viewer healthy');

  const browser = await chromium.launch(CHROME ? { executablePath: CHROME } : {});
  try {
    for (const vp of VIEWPORTS) {
      const ctx = await browser.newContext({
        viewport: { width: vp.width, height: vp.height },
        ignoreHTTPSErrors: true,
        reducedMotion: 'reduce',
        colorScheme: 'light',
        locale: 'en-US',
        timezoneId: 'UTC',
      });
      const page = await ctx.newPage();
      for (const p of PAGES) {
        const outDir = path.join(OUT_ROOT, VARIANT, vp.name);
        fs.mkdirSync(outDir, { recursive: true });
        const target = path.join(outDir, `${p.name}.png`);
        try {
          const resp = await page.goto(VIEWER_URL + p.url, {
            waitUntil: p.sse ? 'load' : 'networkidle',
            timeout: 45000,
          });
          if (p.sse) await page.waitForTimeout(3000);
          await freezeRendering(page);
          await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));
          await page.screenshot({ path: target, fullPage: true });
          const status = resp ? resp.status() : 'null';
          log(`shot ${vp.name}/${p.name} (HTTP ${status}) -> ${target}`);
        } catch (e) {
          log(`FAILED ${vp.name}/${p.name}: ${e.message}`);
        }
      }
      await ctx.close();
    }
  } finally {
    await browser.close();
  }
} catch (e) {
  log('ERROR', e.message);
  exitCode = 1;
} finally {
  viewer.kill('SIGTERM');
}
process.exit(exitCode);
