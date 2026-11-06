// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

/**
 * Browser entrypoint for the fidelity comparison page.
 * Imports the real ANSI renderer and constants — stays in sync automatically.
 * Bundled by Bun.build() at test startup and served as /bundle.js.
 *
 * IMPORTANT: The rendering logic here must exactly match the main app.
 * - xterm config mirrors ExpandedSession.tsx's XTerm constructor
 * - HTML preview uses renderAnsiPre() from ansi-render.ts (same as the app)
 *
 * ── Why this harness does NOT load @xterm/addon-webgl (gd-ZEdRaEXV7d) ──
 *
 * The real app (ExpandedSession.tsx) loads the WebGL addon, so it is tempting to
 * load it here too. Doing so makes the xterm side of this comparison unusable as
 * a fidelity oracle in headless chromium, for two independently measured reasons.
 * (All percentages below are whole-container, the basis the test used before this
 * change, so they compare directly against the 5.56% / 2.06% CI failure; the
 * spec's own baseline is grid-scoped and therefore larger.)
 *
 * 1. The addon's drawing buffer is not devicePixelRatio-scaled here. At dpr=2 the
 *    WebGL canvas gets `canvas.width = 640` for a `style.width = 640px` element,
 *    while the DOM-managed `xterm-link-layer` canvas in the same tree is correctly
 *    sized `1280x864`. GL's bottom-left origin plus a 2x-oversized viewport means
 *    the canvas shows the *bottom-left quadrant* of the intended render, upscaled
 *    2x — a different region of the fixture at a different scale, not a rendering
 *    of it. Measured diff: 4.83% (welcome-screen) / 1.80% (tool-output).
 *
 * 2. Even at dpr=1, where the backing store *is* correctly sized, the software-GL
 *    (SwiftShader) glyph rasterizer disagrees with the DOM/2d rasterizer on
 *    antialiasing and subpixel glyph positioning: 1.60% vs 0.22% for the very same
 *    layout. That is rasterizer noise, not preview infidelity.
 *
 * xterm derives cell metrics from CharSizeService for *every* renderer; WebGL and
 * DOM differ only in how they rasterize into those cells. So the DOM renderer is
 * an equally valid — and here, the only correctly-scaled — oracle for the cell
 * grid and glyph layout this test measures. With it, the same `renderAnsiPre()`
 * output scores 0.31% / 0.25% at dpr=2 — an ~16x improvement with no change to
 * the preview or the budget.
 *
 * `reportGeometry()` below exports the grid geometry so preview-fidelity.spec.ts
 * can assert the harness is sane *before* trusting any pixel number — including a
 * direct check that every xterm canvas backing store is dpr-scaled, which is what
 * catches a recurrence of (1).
 */

import { renderAnsiPre } from "../../../crates/web/src/lib/ansi-render";
import { MONO_FONT, THEME, TERMINAL_FONT_SIZE } from "../../../crates/web/src/lib/constants";

declare const Terminal: new (opts: Record<string, unknown>) => {
	open(el: HTMLElement): void;
	/** Async: parses on a later task. The callback fires once `data` is consumed. */
	write(data: string, cb?: () => void): void;
	onRender(cb: () => void): { dispose(): void };
	loadAddon(addon: unknown): void;
	element: HTMLElement | undefined;
};

/**
 * Ensure every font face the fixture needs is loaded before anything renders.
 *
 * `document.fonts.ready` alone is NOT sufficient: it settles once the *currently
 * pending* loads finish, and a `@font-face` with a `unicode-range` (the
 * iosevka-symbols face in fidelity-comparison.html) does not even begin loading
 * until a glyph in that range is first painted. So `fonts.ready` could resolve,
 * the fixture could then paint a symbol, and the fallback glyph — with a
 * different advance width — would be captured instead, shifting the rest of that
 * line by a pixel or two and inflating the diff nondeterministically
 * (gd-ZEdRaEXV7d: the same fixture measured 0.47% and 0.95% in one CI run).
 *
 * Passing the fixture's own text to `fonts.load()` forces exactly the faces this
 * render will use, symbol ranges included, then `fonts.ready` drains them.
 */
async function loadFonts(sampleText: string): Promise<void> {
	const spec = `${TERMINAL_FONT_SIZE}px ${MONO_FONT}`;
	await Promise.all([
		document.fonts.load(spec, sampleText),
		document.fonts.load(`bold ${spec}`, sampleText),
	]);
	await document.fonts.ready;

	const loaded = document.fonts.check(spec, sampleText);
	console.log(
		"font loaded:",
		loaded,
		"families:",
		[...document.fonts].map((f) => `${f.family}/${f.weight}:${f.status}`).join(", "),
	);
}

/** Resolve after the next composited frame. */
function nextFrame(): Promise<void> {
	return new Promise((resolve) => requestAnimationFrame(() => resolve()));
}

/** Resolve once xterm has parsed `data` *and* painted the result. */
async function writeAndRender(
	term: { write(d: string, cb?: () => void): void; onRender(cb: () => void): { dispose(): void } },
	data: string,
): Promise<void> {
	// Step 1: the write callback fires once the parser has consumed `data`, so
	// the buffer is complete. Subscribing to onRender *before* this point would
	// risk latching the initial blank render that term.open() schedules.
	await new Promise<void>((resolve) => term.write(data, resolve));

	// Step 2: the now-dirty rows still have to be drawn. Wait for the render
	// event, but never block on it — if the renderer decided nothing was dirty,
	// no event comes and a couple of frames is already enough.
	await Promise.race([
		new Promise<void>((resolve) => {
			const sub = term.onRender(() => {
				sub.dispose();
				resolve();
			});
		}),
		nextFrame().then(nextFrame).then(nextFrame),
	]);

	// Step 3: one more frame so that paint is composited before we screenshot.
	await nextFrame();
}

async function main() {
	const fixtureName = new URLSearchParams(location.search).get("fixture") ?? "screen-snapshot";
	const resp = await fetch(`/fixture.json?name=${encodeURIComponent(fixtureName)}`);
	const lines: string[] = await resp.json();

	// Fixture first: its text is what decides which font faces must be loaded.
	await loadFonts(lines.join(""));

	// --- Render xterm.js (matches ExpandedSession.tsx XTerm constructor) ---
	const xtermContainer = document.getElementById("xterm-container")!;
	const term = new Terminal({
		fontSize: TERMINAL_FONT_SIZE,
		fontFamily: MONO_FONT,
		theme: THEME,
		scrollback: 0,
		cursorBlink: false,
		cursorInactiveStyle: "none",
		disableStdin: true,
		convertEol: false,
	});

	term.open(xtermContainer);
	// Deliberately no WebGL addon — see the header comment. xterm falls back to
	// its DOM renderer, which shares CharSizeService cell metrics with WebGL and
	// is the only renderer that scales correctly under headless chromium.

	// One write, then wait for the paint. Splitting this into a write-per-line
	// and guessing a settle delay is what made this page nondeterministic:
	// term.write() parses on a *later* task, so a fixed timeout can screenshot a
	// partially-parsed screen under CI load (gd-ZEdRaEXV7d).
	await writeAndRender(term, lines.join("\r\n"));

	// --- Render HTML preview (shared renderAnsiPre from the main app) ---
	const previewEl = document.getElementById("html-preview")!;
	const pre = renderAnsiPre(lines, {
		fontSize: TERMINAL_FONT_SIZE,
		background: THEME.background,
	});
	previewEl.appendChild(pre);
	previewEl.setAttribute("data-ready", "true");

	// Both sides have now painted (xterm via writeAndRender above, the preview
	// synchronously on append), so readiness is a fact rather than a timeout.
	reportGeometry(xtermContainer, pre, lines);
	xtermContainer.setAttribute("data-ready", "true");
}

/** Geometry the spec asserts on before it trusts a pixel diff. */
export interface Rect {
	x: number;
	y: number;
	width: number;
	height: number;
}

export interface HarnessGeometry {
	dpr: number;
	/** Fixture dimensions, in cells. */
	fixtureCols: number;
	fixtureRows: number;
	/** xterm's rendered grid, in page CSS px. */
	xtermGrid: Rect;
	/** The same-sized grid region of the preview `<pre>`, in page CSS px. */
	previewGrid: Rect;
	/** Per-cell CSS px, derived from the xterm grid. */
	cellWidth: number;
	cellHeight: number;
	/** Inked advance width of one full 80-column preview row, in CSS px. */
	previewRowWidth: number;
	/** Rendered height of one preview row, in CSS px. */
	previewRowHeight: number;
	/**
	 * One entry per canvas xterm created: `true` when the backing store is
	 * devicePixelRatio-scaled relative to its CSS box. A `false` here means the
	 * renderer is drawing at the wrong scale and no pixel diff is meaningful.
	 */
	canvasesDprScaled: boolean[];
}

/** Inked advance width of a row div's spans, excluding its trailing newline. */
function rowAdvanceWidth(row: Element): number {
	const spans = [...row.children];
	const first = spans[0];
	const last = spans[spans.length - 1];
	if (!first || !last) return 0;
	const range = document.createRange();
	range.setStartBefore(first);
	range.setEndAfter(last);
	const width = range.getBoundingClientRect().width;
	range.detach();
	return width;
}

/**
 * Measure both renderers' grid geometry and stash it on `window` for the spec.
 *
 * The preview `<pre>` is a block that fills its container, so its own box is
 * wider than the 80 columns it inks. Both grids are therefore reported as the
 * xterm-sized region anchored at each renderer's content origin, and the spec
 * clips its two page screenshots to them — that keeps the reported percentage a
 * share of *terminal cells* rather than of surrounding container padding.
 */
function reportGeometry(
	xtermContainer: HTMLElement,
	pre: HTMLPreElement,
	lines: string[],
): void {
	const dpr = window.devicePixelRatio || 1;
	const screen = xtermContainer.querySelector(".xterm-screen") as HTMLElement | null;
	const screenBox = (screen ?? xtermContainer).getBoundingClientRect();
	const preBox = pre.getBoundingClientRect();
	const firstRow = pre.firstElementChild;

	const rows = lines.length;
	const cols = Math.max(
		...lines.map((l) => [...l.replace(/\u001b\[[0-9;]*m/g, "")].length),
	);

	const canvasesDprScaled = [...xtermContainer.querySelectorAll("canvas")]
		// Zero-sized offscreen canvases (glyph atlases) have no CSS box to compare.
		.filter((c) => c.getBoundingClientRect().width > 0)
		.map((c) => {
			const box = c.getBoundingClientRect();
			return (
				Math.abs(c.width - box.width * dpr) < 1 &&
				Math.abs(c.height - box.height * dpr) < 1
			);
		});

	const geometry: HarnessGeometry = {
		dpr,
		fixtureCols: cols,
		fixtureRows: rows,
		xtermGrid: {
			x: screenBox.x,
			y: screenBox.y,
			width: screenBox.width,
			height: screenBox.height,
		},
		previewGrid: {
			x: preBox.x,
			y: preBox.y,
			width: screenBox.width,
			height: screenBox.height,
		},
		cellWidth: screenBox.width / cols,
		cellHeight: screenBox.height / rows,
		previewRowWidth: firstRow ? rowAdvanceWidth(firstRow) : 0,
		previewRowHeight: firstRow ? firstRow.getBoundingClientRect().height : 0,
		canvasesDprScaled,
	};

	(window as unknown as { __fidelityGeometry?: HarnessGeometry }).__fidelityGeometry =
		geometry;
	console.log(`geometry: ${JSON.stringify(geometry)}`);
}

main();
