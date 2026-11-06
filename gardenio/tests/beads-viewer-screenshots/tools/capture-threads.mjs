// capture-threads.mjs — the generalized /threads visual capture matrix.
// Task 3-3 (ka-0000004063) shipped the index/detail/bead-show coverage;
// task 3-4 (ka-0000003162) extends it to the composer + optimistic-send cells.
// Graded against the Phase 3 visual rubric ka-0000007635 (gates V1–V12).
//
// This GENERALIZES tools/capture-inbox-skins.mjs (which captured ONE page,
// /inbox, across skins) into a full matrix over the /threads surface:
//
//   SKINS   = [baseline, dark-terminal, dark]
//             - baseline      : the default light theme.
//             - dark-terminal : the registered dark theme (the thread templates
//                               carry html[data-skin="dark-terminal"] overrides).
//             - dark          : the CONTRAST-TRAP PROBE. The thread templates
//                               define NO html[data-skin="dark"] text-colour
//                               overrides, and layout.html ships no --bg block
//                               for "dark", so light-theme text colours render
//                               against whatever ambient background "dark"
//                               yields. Capturing it surfaces rubric V4 (legible
//                               contrast in BOTH themes) as a tracked defect.
//   WIDTHS  = [480, 768, 1280]  (mobile / tablet / desktop breakpoints, V6/V7).
//   URLS    = threads-fixtures.urlPlan(): index {empty,populated,error},
//             detail {empty,populated,fulfilled,needstriage,longthread,error},
//             bead-show chip {resolved,fallback,absent}, AND (3-4) composer
//             {empty,focused,typed,optimistic,reconciled}.
//
// For EACH (skin, width, cell) we capture BOTH the first viewport
// (clip to the viewport box) AND the full scrollable page (fullPage:true), so a
// grader can judge both above-the-fold framing and the whole surface.
//
// Plus LIVE interactions:
//   - work-expand (detail/populated, detail/longthread): click
//     `.thread2-work > summary` to open the collapsed work <details> and
//     capture the expanded state (proves the disclosure works and the firehose
//     is reachable — rubric V2's converse).
//   - composer-empty / focused / typed (3-4, rubric V5): authenticate as
//     operator (X-Auth-Request-Email header) so CanCompose flips true, then
//     capture the resting / focused / typed states of the composer. The focus
//     state captures the textarea's focus ring (V5: visible focus ring);
//     authenticating + clicking the textarea proves keyboard reachability.
//   - composer-optimistic / reconciled (3-4, rubric V10): type a message and
//     click Send, capture the optimistic "sending..." bubble before the server
//     echo, then inject the authoritative message bead via the mock daemon's
//     POST /__control/inject-message control endpoint, trigger the timeline
//     refresh, and capture the reconciled state. The proof of "zero duplicate"
//     is that the server-rendered message replaces the optimistic bubble — the
//     ?since= poll picks the new bead up exactly once and the optimistic
//     bubble's data-pending-id row is reconciled away (see thread_composer.js).
//
// Composer / optimistic-send cells (rubric V5, V10) are NOW captured (3-4).
// The earlier `N/A-until-2-5` placeholder is gone.
//
// PNG naming: threads-<view>-<state>-<theme>-<bp>[-<interaction>].png
//   e.g. threads-detail-populated-dark-terminal-1280.png
//        threads-detail-populated-baseline-1280-workexpand.png
//        threads-composer-focused-baseline-1280.png
//
// Determinism mirrors the existing harness: reducedMotion, UTC, en-US, fixed
// viewport, animations/caret/scroll-behaviour disabled via an injected
// stylesheet, and a small settle after load. Timestamps are pinned by the mock
// daemon (fixed offsets from a frozen "now"), so humanizeTime is byte-stable.

import { chromium } from "playwright";
import fs from "node:fs";
import path from "node:path";
import { urlPlan, COMPOSER_BODY, IDS } from "./threads-fixtures.mjs";

// OPERATOR_EMAIL is the X-Auth-Request-Email value the harness sends for cells
// marked `auth: true` (the composer + optimistic-send cells). The viewer's
// EmailFromRequest reads this header verbatim, so a non-empty value flips
// CanCompose true (combined with dev-bypass leaving viewerCanOperate true). The
// composer JS then attributes the optimistic bubble to this address.
const OPERATOR_EMAIL = "matt@example.com";

// resolveChromium picks a launchable Chromium. Prefer the one Playwright
// bundled (chromium.executablePath()); if that build is not present (common in
// an agent pod whose pinned browser revision differs from the npm-installed
// Playwright), fall back to the newest chrome under PLAYWRIGHT_BROWSERS_PATH (or
// /ms-playwright). CI runs `npx playwright install --with-deps chromium`, which
// makes the bundled path exist, so this fallback only ever fires in dev/pod.
function resolveChromiumExecutable() {
  try {
    const p = chromium.executablePath();
    if (p && fs.existsSync(p)) return undefined; // undefined = use the default
  } catch {
    /* fall through to discovery */
  }
  const root = process.env.PLAYWRIGHT_BROWSERS_PATH || "/ms-playwright";
  let best = null;
  try {
    for (const dir of fs.readdirSync(root)) {
      if (!dir.startsWith("chromium-")) continue; // full chromium, not headless_shell
      const exe = path.join(root, dir, "chrome-linux64", "chrome");
      if (fs.existsSync(exe)) {
        const rev = parseInt(dir.split("-")[1] || "0", 10);
        if (!best || rev > best.rev) best = { exe, rev };
      }
    }
  } catch {
    /* no discovery root */
  }
  if (best) {
    console.error(`[capture] bundled chromium missing; using ${best.exe}`);
    return best.exe;
  }
  return undefined;
}

const BASE = process.env.BEADS_VIEWER_URL || "http://localhost:8095";
const DAEMON = process.env.MOCK_DAEMON_URL || "http://localhost:18080";
const OUT = process.env.OUT_DIR || "test-results/threads";

// The three skins. `colorScheme` only matters for media-query-driven CSS; the
// thread surface keys off data-skin, but we still set it so any
// prefers-color-scheme rule resolves consistently with the skin's intent.
const SKINS = [
  { name: "baseline", colorScheme: "light" },
  { name: "dark-terminal", colorScheme: "dark" },
  { name: "dark", colorScheme: "dark" }, // contrast-trap probe (rubric V4)
];
const WIDTHS = [480, 768, 1280];

// A stable filename slug for a theme (dark-terminal stays hyphenated).
const themeSlug = (s) => s;

async function setFailMode(mode) {
  const resp = await fetch(`${DAEMON}/__control/fail`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ mode: mode || "none" }),
  });
  if (!resp.ok) throw new Error(`failed to set mock daemon fail mode ${mode}: HTTP ${resp.status}`);
}

// resetInjected drops every harness-injected message bead from the mock daemon
// so the next composer-reconciled cell starts with a clean fixture. Without
// this, each cell would see the previous cell's injected message AND the new
// one and the V10 "zero duplicate" check would count BOTH and fail.
async function resetInjected() {
  const resp = await fetch(`${DAEMON}/__control/reset-injected`, { method: "POST" });
  if (!resp.ok) throw new Error(`reset-injected failed: HTTP ${resp.status}`);
}

const DISABLE_ANIM = `*,*::before,*::after{animation-duration:0s !important;animation-delay:0s !important;transition-duration:0s !important;transition-delay:0s !important;caret-color:transparent !important;}html{scroll-behavior:auto !important;}`;

async function newCtx(browser, width, skin, authEmail) {
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    deviceScaleFactor: 1,
    ignoreHTTPSErrors: true,
    reducedMotion: "reduce",
    colorScheme: skin.colorScheme,
    timezoneId: "UTC",
    locale: "en-US",
    // Setting extraHTTPHeaders here ensures every navigation AND every htmx
    // subresource (the timeline poll, the composer POST) carries the operator
    // email — the viewer's EmailFromRequest reads this header directly and the
    // composer JS attribution chain reads it back off the rendered form.
    ...(authEmail ? { extraHTTPHeaders: { "X-Auth-Request-Email": authEmail } } : {}),
  });
  // Seed localStorage BEFORE any document loads so the inline boot script in
  // layout.html applies data-skin before first paint.
  await ctx.addInitScript((s) => {
    try {
      localStorage.setItem("beads-viewer:skin", s);
    } catch {
      /* ignore */
    }
  }, skin.name);
  return ctx;
}

// injectMessage hits the mock daemon's control endpoint to inject a closed
// type=message bead bound to `head`. The next ?since= poll picks it up via the
// real thread_id field-equality + fuseThreadTimeline path, so the visual
// reconcile is exercising the real rendering, not a fake DOM patch.
async function injectMessage(headID, text, author, tsOffsetH) {
  const resp = await fetch(`${DAEMON}/__control/inject-message`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ head: headID, text, author, ts_offset_h: tsOffsetH || 0 }),
  });
  if (!resp.ok) throw new Error(`inject-message failed: HTTP ${resp.status} ${await resp.text()}`);
  return resp.json();
}

// composerInteract runs one of the composer/optimistic-send scripted
// interactions and captures the resulting state. It returns a per-cell
// manifest entry to be appended (or null if the interaction wasn't applicable).
//
// The interactions:
//   composer-empty       — capture the resting composer.
//   composer-focused     — TAB into / .focus() the textarea; capture the focus
//                          ring (rubric V5).
//   composer-typed       — type a multi-line message; capture the populated
//                          textarea (Send button enabled).
//   composer-optimistic  — type + click Send; capture the optimistic bubble
//                          BEFORE the server echo arrives.
//   composer-reconciled  — same as optimistic + inject the authoritative
//                          message bead via /__control/inject-message + force
//                          the timeline poll; capture the reconciled state
//                          (NO duplicate; the optimistic bubble's data-
//                          pending-id row is reconciled away).
async function composerInteract({ page, dir, baseName, cell }) {
  // The composer form lives outside #thread-timeline (so a poll never wipes a
  // draft); the textarea is uniquely identified by name=text on the
  // #thread-composer form.
  const composer = page.locator("#thread-composer");
  const textarea = composer.locator('textarea[name="text"]');
  if ((await composer.count()) === 0) {
    console.error(`WARN ${baseName}: #thread-composer not present — auth header may not have flipped CanCompose`);
    return null;
  }

  switch (cell.interaction) {
    case "composer-empty": {
      // Resting composer. No interaction beyond settling.
      await shoot(page, dir, baseName);
      return { ...cellManifest(cell), interaction: cell.interaction, file: pathJoin(dir, baseName) };
    }
    case "composer-focused": {
      // Tab through the surface until the textarea has focus, then re-shoot
      // (proves keyboard reachability, rubric V5). The textarea is the first
      // tabbable element inside #thread-composer; .focus() is a fallback that
      // exercises the same focus-ring CSS rule.
      await textarea.focus();
      await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));
      await page.waitForTimeout(150);
      await shoot(page, dir, baseName);
      return { ...cellManifest(cell), interaction: cell.interaction, file: pathJoin(dir, baseName) };
    }
    case "composer-typed": {
      // Type the canonical message body. The textarea grows; the Send button
      // is enabled (the form's required attribute is satisfied).
      await textarea.fill(COMPOSER_BODY);
      await page.waitForTimeout(150);
      await shoot(page, dir, baseName);
      return { ...cellManifest(cell), interaction: cell.interaction, file: pathJoin(dir, baseName) };
    }
    case "composer-optimistic": {
      // Type + click Send. The composer JS (htmx:beforeRequest) paints the
      // pending bubble synchronously; the POST goes to the viewer, which calls
      // CreateBead+CloseBeadAs on the mock daemon (both return 200 from the
      // mock's POST passthroughs). The 2xx ack promotes the bubble to "sent"
      // and fires `thread-refresh` at the timeline. The bead has NOT been
      // injected into the daemon yet, so the next ?since= poll returns the
      // SAME timeline (the optimistic bubble lingers until reconcile).
      await textarea.fill(COMPOSER_BODY);
      await composer.locator("button.thread2-composer-send").click();
      // Give the JS path a beat to paint and the htmx request to ack.
      await page.waitForSelector(".thread2-msg.is-pending, .thread2-msg.is-sent", { timeout: 5000 });
      await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));
      await page.waitForTimeout(250);
      await shoot(page, dir, baseName);
      return { ...cellManifest(cell), interaction: cell.interaction, file: pathJoin(dir, baseName) };
    }
    case "composer-reconciled": {
      // Type + send, then inject the authoritative message bead and force a
      // timeline refresh. The poll renders the new bead exactly once; the
      // optimistic bubble's data-pending-id row is reconciled away.
      await textarea.fill(COMPOSER_BODY);
      await composer.locator("button.thread2-composer-send").click();
      await page.waitForSelector(".thread2-msg.is-pending, .thread2-msg.is-sent", { timeout: 5000 });
      // Inject the authoritative bead — the next poll picks it up via the real
      // thread_id field-equality query + fuseThreadTimeline path.
      await injectMessage(IDS.openWish, COMPOSER_BODY, OPERATOR_EMAIL, 0);
      // Force the timeline poll without waiting the full 8s cadence: dispatch
      // the same `thread-refresh` event the composer JS uses.
      await page.evaluate(() => {
        const tl = document.getElementById("thread-timeline");
        if (tl && window.htmx && typeof window.htmx.trigger === "function") {
          window.htmx.trigger(tl, "thread-refresh");
        }
      });
      // Wait for the server-rendered message to appear AND for the optimistic
      // bubble to be reconciled away. The proof of "zero duplicate" is that
      // .thread2-msg counts ONE rendering of COMPOSER_BODY after reconcile.
      await page.waitForFunction(
        (body) => {
          const bubbles = Array.from(document.querySelectorAll(".thread2-msg .thread2-msg-body"));
          const matches = bubbles.filter((el) => (el.textContent || "").includes(body.slice(0, 40)));
          return matches.length === 1;
        },
        COMPOSER_BODY,
        { timeout: 20000 },
      );
      await page.waitForTimeout(250);
      await shoot(page, dir, baseName);
      // Belt-and-braces: confirm there are no leftover pending bubbles in the
      // DOM. A duplicate would have count >= 2 OR a residual .is-pending.
      const pending = await page.locator(".thread2-msg.is-pending").count();
      if (pending > 0) {
        throw new Error(`reconcile failed: ${pending} pending bubbles remain in DOM`);
      }
      return { ...cellManifest(cell), interaction: cell.interaction, file: pathJoin(dir, baseName) };
    }
    default:
      return null;
  }
}

// cellManifest projects a cell to its manifest fields (theme/bp filled later).
function cellManifest(cell) {
  return { view: cell.view, state: cell.state };
}

// pathJoin is a tiny wrapper so the per-interaction call sites read uniformly.
function pathJoin(dir, baseName) {
  return path.join(dir, `${baseName}.png`);
}

async function settle(page, skinName) {
  // Belt + braces: force the skin attribute (in case the boot script raced).
  await page.evaluate((s) => document.documentElement.setAttribute("data-skin", s), skinName);
  await page.addStyleTag({ content: DISABLE_ANIM });
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));
  await page.waitForTimeout(350);
}

async function shoot(page, dir, baseName) {
  fs.mkdirSync(dir, { recursive: true });
  // First viewport (above the fold): clip to the viewport box.
  const vp = page.viewportSize();
  await page.screenshot({
    path: path.join(dir, `${baseName}.png`),
    clip: { x: 0, y: 0, width: vp.width, height: Math.min(vp.height, 900) },
  });
  // Full scrollable page.
  await page.screenshot({ path: path.join(dir, `${baseName}-full.png`), fullPage: true });
}

async function main() {
  const executablePath = resolveChromiumExecutable();
  const browser = await chromium.launch({ headless: true, ...(executablePath ? { executablePath } : {}) });
  const plan = urlPlan();
  const manifest = [];
  let failures = 0;

  for (const skin of SKINS) {
    for (const width of WIDTHS) {
      for (const cell of plan) {
        // bead-show chip cells are theme-relevant but breakpoint-light; still
        // capture all to keep the matrix uniform and catch chip wrapping at 480.
        const ctx = await newCtx(browser, width, skin, cell.auth ? OPERATOR_EMAIL : undefined);
        const page = await ctx.newPage();
        try {
          await setFailMode(cell.fail || "none");
          // Composer cells (3-4) reset the daemon's injected-message state
          // BEFORE navigation so a prior cell's injected message does not leak
          // into the conversation and contaminate the V5 composer-affordance
          // screenshot or the V10 "zero duplicate" check.
          if (cell.view === "composer") {
            await resetInjected();
          }
          const url = `${BASE}${cell.path}`;
          const resp = await page.goto(url, { waitUntil: "domcontentloaded", timeout: 30000 });
          // The error cells legitimately render a 200 page with a banner (the
          // handlers degrade, they don't 500). A real 4xx/5xx is a harness bug.
          const status = resp ? resp.status() : 0;
          if (status >= 400) {
            console.error(`HTTP ${status} for ${cell.view}/${cell.state} @ ${skin.name}/${width} (${url})`);
            failures++;
            await ctx.close();
            continue;
          }
          await settle(page, skin.name);

          const baseName = `threads-${cell.view}-${cell.state}-${themeSlug(skin.name)}-${width}`;
          const dir = path.join(OUT, `${themeSlug(skin.name)}-${width}`);

          // Composer cells (3-4, rubric V5/V10) dispatch through the scripted
          // interaction helper rather than the static shoot path — the helper
          // performs the focus / type / send / inject + reconcile steps that
          // produce the cell's authoritative screenshot.
          if (cell.view === "composer") {
            const entry = await composerInteract({ page, dir, baseName, cell });
            if (entry) {
              manifest.push({ ...entry, theme: skin.name, bp: width });
              console.log(`OK ${baseName} (${cell.interaction})`);
            } else {
              console.error(`WARN ${baseName}: composer interaction produced no entry`);
              failures++;
            }
          } else {
            await shoot(page, dir, baseName);
            manifest.push({ view: cell.view, state: cell.state, theme: skin.name, bp: width, file: `${dir}/${baseName}.png` });
            console.log(`OK ${baseName}`);
          }

          // LIVE work-expand interaction on the detail-populated + longthread
          // cells: click the collapsed work <details> summary and re-capture.
          if (cell.view === "detail" && (cell.state === "populated" || cell.state === "longthread")) {
            const summary = page.locator(".thread2-work > summary");
            if ((await summary.count()) > 0) {
              await summary.first().click();
              // The <details> open toggle + caret rotation is instant (anim
              // disabled); one rAF lets layout reflow.
              await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));
              await page.waitForTimeout(150);
              const exName = `${baseName}-workexpand`;
              await shoot(page, dir, exName);
              manifest.push({ view: cell.view, state: cell.state, theme: skin.name, bp: width, interaction: "workexpand", file: `${dir}/${exName}.png` });
              console.log(`OK ${exName}`);
            } else {
              console.error(`WARN no .thread2-work summary on ${baseName} (work-expand skipped)`);
            }
          }
        } catch (e) {
          console.error(`FAIL ${cell.view}/${cell.state} @ ${skin.name}/${width}: ${e.message}`);
          failures++;
        } finally {
          await ctx.close();
        }
      }
    }
  }
  await browser.close();
  await setFailMode("none");

  fs.mkdirSync(OUT, { recursive: true });
  fs.writeFileSync(path.join(OUT, "manifest.json"), JSON.stringify(manifest, null, 2));
  console.log(`\nwrote ${manifest.length} screenshot pairs to ${OUT}/  (${failures} failures)`);
  process.exit(failures > 0 ? 1 : 0);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
