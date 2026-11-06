// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

/**
 * Self-contained HTTP server for preview fidelity comparison tests.
 *
 * Serves a comparison page rendering the same ANSI fixture data two ways:
 * - HTML preview (DOM spans, matching TerminalPreview.tsx logic)
 * - xterm.js (canvas renderer)
 *
 * At startup, bundles fidelity-entry.ts with Bun.build() so the browser code
 * imports from the real ansi.ts / constants.ts — no copy-paste drift.
 *
 * Routes:
 * - GET /              → comparison HTML page
 * - GET /bundle.js     → bundled entrypoint (ANSI parser + renderer)
 * - GET /vendor/xterm.js  → xterm.js library
 * - GET /vendor/xterm.css → xterm.js styles
 * - GET /fixture.json     → screen snapshot fixture
 * - GET /font/*           → bundled font files
 */

import {
	createServer,
	type Server,
	type IncomingMessage,
	type ServerResponse,
} from "node:http";
import { readFileSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { execSync } from "node:child_process";
import { tmpdir } from "node:os";

const __dirname = dirname(fileURLToPath(import.meta.url));
const PROJECT_ROOT = resolve(__dirname, "../../..");
const WEB_NODE_MODULES = resolve(PROJECT_ROOT, "crates/web/node_modules");
const FIXTURES_DIR = resolve(__dirname, "../fixtures");
const WEB_FONTS_DIR = resolve(PROJECT_ROOT, "crates/web/src/fonts");
const ENTRY_FILE = resolve(__dirname, "fidelity-entry.ts");

export class FidelityServer {
	readonly port: number;
	private server: Server;
	private bundleJs: Buffer | null = null;

	constructor(port: number) {
		this.port = port;
		this.server = createServer((req, res) => this.handleRequest(req, res));
	}

	async start(): Promise<void> {
		this.bundleJs = await bundle(ENTRY_FILE);
		return new Promise((resolve) => {
			this.server.listen(this.port, "127.0.0.1", () => resolve());
		});
	}

	async stop(): Promise<void> {
		return new Promise((resolve, reject) => {
			// close() only fires its callback once every socket has ended, and the
			// browser holds keep-alive sockets open well past the last request — so
			// close() alone hangs until Playwright's 30s afterAll timeout kills the
			// hook and reports the last-run test as failed (gd-ZEdRaEXV7d).
			this.server.close((err) => (err ? reject(err) : resolve()));
			this.server.closeAllConnections();
		});
	}

	private handleRequest(req: IncomingMessage, res: ServerResponse): void {
		const url = new URL(req.url ?? "/", `http://localhost:${this.port}`);

		if (url.pathname === "/") {
			const html = readFileSync(
				resolve(FIXTURES_DIR, "fidelity-comparison.html"),
			);
			res.writeHead(200, { "Content-Type": "text/html" });
			res.end(html);
			return;
		}

		if (url.pathname === "/bundle.js") {
			res.writeHead(200, { "Content-Type": "application/javascript" });
			res.end(this.bundleJs);
			return;
		}

		if (url.pathname === "/vendor/xterm.js") {
			const content = readFileSync(
				resolve(WEB_NODE_MODULES, "@xterm/xterm/lib/xterm.js"),
			);
			res.writeHead(200, { "Content-Type": "application/javascript" });
			res.end(content);
			return;
		}

		if (url.pathname === "/vendor/xterm.css") {
			const content = readFileSync(
				resolve(WEB_NODE_MODULES, "@xterm/xterm/css/xterm.css"),
			);
			res.writeHead(200, { "Content-Type": "text/css" });
			res.end(content);
			return;
		}

		if (url.pathname.startsWith("/font/")) {
			const filename = url.pathname.slice("/font/".length);
			try {
				const content = readFileSync(resolve(WEB_FONTS_DIR, filename));
				const ct = filename.endsWith(".woff2") ? "font/woff2" : "font/ttf";
			res.writeHead(200, { "Content-Type": ct });
				res.end(content);
			} catch {
				res.writeHead(404);
				res.end("not found");
			}
			return;
		}

		if (url.pathname === "/fixture.json") {
			const name = url.searchParams.get("name") ?? "screen-snapshot";
			const safe = name.replace(/[^a-zA-Z0-9_-]/g, "");
			try {
				const content = readFileSync(
					resolve(FIXTURES_DIR, `${safe}.json`),
				);
				res.writeHead(200, { "Content-Type": "application/json" });
				res.end(content);
			} catch {
				res.writeHead(404);
				res.end("not found");
			}
			return;
		}

		res.writeHead(404);
		res.end("not found");
	}
}

/** Bundle the browser entrypoint with bun. */
async function bundle(entrypoint: string): Promise<Buffer> {
	const outdir = resolve(tmpdir(), `fidelity-bundle-${process.pid}`);
	execSync(
		`bun build ${JSON.stringify(entrypoint)} --outdir ${JSON.stringify(outdir)} --target browser --format esm`,
	);
	return readFileSync(resolve(outdir, "fidelity-entry.js"));
}
