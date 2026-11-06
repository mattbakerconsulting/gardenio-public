// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

/**
 * Preview fidelity: does the mux tile's `<pre>` preview reproduce xterm.js's
 * cell grid and glyph layout for the same fixture, in the same browser?
 *
 * The budget is `< 1%` of *terminal-grid* pixels (gd-ZEdRaEXV7d). Measured
 * baseline at dpr=2 with the DOM renderer:
 *
 *   welcome-screen  0.53%
 *   tool-output     0.44%
 *
 * The residual is one identified class of difference: long horizontal
 * box-drawing rules (`─`) and the vertical divider (`│`) sit a sub-pixel off,
 * so their thin strokes land on a neighbouring row of device pixels. Every text
 * glyph matches. Tighten the preview's box-drawing advance and this number drops;
 * it is a real signal, not headroom fitted to current output.
 *
 * Read the `*-diff.png` artifact before touching the budget. A diff whose ONLY
 * red is those two rules is the expected steady state. Red on *text* means the
 * two sides disagree about glyph advance and the rest of the line has shifted —
 * the failure mode that made one CI run measure this same fixture at both 0.47%
 * and 0.95%, from a `unicode-range` font face that had not finished loading (see
 * `loadFonts` in lib/fidelity-entry.ts). That is a harness race, not a fidelity
 * regression, and it is fixed rather than budgeted for.
 *
 * Before trusting any percentage the test asserts the harness itself is sane —
 * same grid, same cell size, correctly dpr-scaled canvases. A broken xterm side
 * produces a *low-looking* diff (most of a terminal is background), so these
 * checks are what keep the budget from silently asserting nothing.
 */

import { test, expect } from "@playwright/test";
import { writeFileSync, mkdirSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { FidelityServer } from "../lib/fidelity-server.js";
import type { HarnessGeometry } from "../lib/fidelity-entry.js";
import { compareScreenshots } from "../lib/pixel-diff.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const RESULTS_DIR = resolve(__dirname, "../results/fidelity");
const FIDELITY_PORT = 18_900;

/** Share of terminal-grid pixels allowed to differ. */
const DIFF_BUDGET_PERCENT = 1;

/** Screenshots are taken at dpr=2 to catch the rounding Retina displays expose. */
const DEVICE_SCALE_FACTOR = 2;

let server: FidelityServer;

test.beforeAll(async () => {
	mkdirSync(RESULTS_DIR, { recursive: true });
	server = new FidelityServer(FIDELITY_PORT);
	await server.start();
});

test.afterAll(async () => {
	await server.stop();
});

const FIXTURES = [
	{ name: "screen-snapshot", label: "welcome-screen" },
	{ name: "screen-snapshot-tools", label: "tool-output" },
];

test.describe("preview fidelity", () => {
	for (const fixture of FIXTURES) {
		test(`preview matches xterm.js — ${fixture.label}`, async ({ browser }) => {
			const context = await browser.newContext({
				deviceScaleFactor: DEVICE_SCALE_FACTOR,
			});
			const page = await context.newPage();

			// Capture browser console for debugging cell height measurements
			page.on("console", (msg) => console.log(`[browser] ${msg.text()}`));

			await page.goto(
				`http://localhost:${FIDELITY_PORT}/?fixture=${fixture.name}`,
			);

			// Wait for both renderers to signal completion
			await page.waitForSelector("#html-preview[data-ready='true']", {
				timeout: 10_000,
			});
			await page.waitForSelector("#xterm-container[data-ready='true']", {
				timeout: 10_000,
			});

			// Give xterm canvas an extra beat to flush
			await page.waitForTimeout(300);

			// ── Harness pre-flight ────────────────────────────────────────────
			// Everything below compares pixels; none of it means anything unless
			// the two renderers actually drew the same fixture at the same scale.
			const geo = (await page.evaluate(
				() =>
					(window as unknown as { __fidelityGeometry?: HarnessGeometry })
						.__fidelityGeometry,
			)) as HarnessGeometry | undefined;

			expect(geo, "harness did not report geometry").toBeDefined();
			if (!geo) return;

			expect(
				geo.dpr,
				"browser dpr does not match the requested deviceScaleFactor",
			).toBe(DEVICE_SCALE_FACTOR);

			// A canvas whose backing store is not dpr-scaled is being upscaled by
			// the browser: wrong glyph size, and (with GL's bottom-left origin)
			// wrong region of the fixture. xterm's DOM renderer creates no visible
			// canvas at all, so this list is normally empty; it exists as a
			// tripwire for anyone re-introducing a canvas/WebGL renderer here
			// (verified to fire on addon-webgl — gd-ZEdRaEXV7d).
			expect(
				geo.canvasesDprScaled,
				"an xterm canvas backing store is not devicePixelRatio-scaled — " +
					"its output is upscaled, so no pixel diff is meaningful",
			).not.toContain(false);

			// xterm must have rendered the fixture's own grid, unwrapped and
			// unscrolled — the fixtures are authored at exactly 80x24.
			expect(geo.fixtureCols, "fixture is not 80 columns wide").toBe(80);
			expect(geo.fixtureRows, "fixture is not 24 rows tall").toBe(24);

			// Cell metrics must land on whole CSS pixels at this dpr. When they do,
			// xterm's floor-vs-raw cell-width rounding (see ansi-render.ts's
			// xtermCellAdjust) is provably identical across renderers, so the
			// preview cannot be advantaged or penalised by which one we compare to.
			expect(
				geo.cellWidth,
				"cell width is not a whole number of CSS px — renderer rounding differs",
			).toBe(Math.round(geo.cellWidth));
			expect(
				geo.cellHeight,
				"cell height is not a whole number of CSS px — renderer rounding differs",
			).toBe(Math.round(geo.cellHeight));

			// The preview must lay out the same grid xterm did: 80 columns of
			// advance and one row per fixture line, to the CSS pixel. If these
			// drift, the clipped regions below no longer hold the same cells.
			expect(
				geo.previewRowWidth,
				"preview row advance != xterm's 80-column grid width",
			).toBeCloseTo(geo.xtermGrid.width, 1);
			expect(
				geo.previewRowHeight,
				"preview row height != xterm's cell height",
			).toBeCloseTo(geo.cellHeight, 1);

			// ── Compare ───────────────────────────────────────────────────────
			// Clip both screenshots to their renderer's grid, so the percentage is
			// a share of terminal cells rather than of the surrounding (identically
			// empty) container padding. `fullPage` is required: the two 800px
			// containers sit side by side, so the second one is outside the
			// viewport and a viewport-relative clip would capture blank pixels.
			const previewBuf = await page.screenshot({
				fullPage: true,
				clip: geo.previewGrid,
			});
			const xtermBuf = await page.screenshot({
				fullPage: true,
				clip: geo.xtermGrid,
			});

			const { diffPercent, diffBuffer } = compareScreenshots(
				previewBuf,
				xtermBuf,
			);

			// Save artifacts
			writeFileSync(resolve(RESULTS_DIR, `${fixture.label}-preview.png`), previewBuf);
			writeFileSync(resolve(RESULTS_DIR, `${fixture.label}-xterm.png`), xtermBuf);
			writeFileSync(resolve(RESULTS_DIR, `${fixture.label}-diff.png`), diffBuffer);

			console.log(
				`Preview fidelity diff (${fixture.label}): ${diffPercent.toFixed(2)}% ` +
					`of ${geo.fixtureCols}x${geo.fixtureRows} grid ` +
					`(${geo.cellWidth}x${geo.cellHeight} CSS px cells, dpr ${geo.dpr})`,
			);

			expect(diffPercent).toBeLessThan(DIFF_BUDGET_PERCENT);
		});
	}
});
