// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

/**
 * Lightweight mock coop server for E2E tests.
 *
 * Responds to the same HTTP endpoints the mux polls:
 * - GET /api/v1/health → { status: "running" }
 * - GET /api/v1/status → { session_id, state, ... }
 * - GET /api/v1/screen → { lines: [...], ansi: [...], cols, rows }
 *
 * Also serves a WebSocket endpoint at /ws?subscribe=state that pushes
 * transition events when state changes (matching real coop behaviour).
 *
 * State and screen content can be changed at runtime for testing transitions.
 */

import {
	createServer,
	type Server,
	type IncomingMessage,
	type ServerResponse,
} from "node:http";
import { WebSocketServer, type WebSocket } from "ws";

/**
 * Upper bound on how long `MockCoop.stop()` waits for the OS/Node to release a
 * listener. Teardown of a handful of loopback sockets is sub-millisecond when it
 * works at all; anything past this is a socket the mux has not released, and
 * waiting longer only eats the caller's hook budget.
 */
const STOP_TIMEOUT_MS = 2_000;

/** Resolve after `ms`, without holding the worker process open. */
function deadline(ms: number): Promise<void> {
	return new Promise((resolve) => {
		const timer = setTimeout(resolve, ms);
		timer.unref?.();
	});
}

export interface MockCoopOptions {
	port: number;
	sessionId: string;
	initialState?: string;
	screenLines?: string[];
	/**
	 * Account name this mock reports on /api/v1/health and /api/v1/status,
	 * mirroring real coop's `account_name` (= the Active credential profile's
	 * name). The mux health poller reconciles each session's assigned_account
	 * from this value, so it is what drives the per-tile account display.
	 */
	accountName?: string;
}

export class MockCoop {
	readonly port: number;
	readonly sessionId: string;
	private server: Server;
	private wss: WebSocketServer;
	private wsClients: Set<WebSocket> = new Set();
	private _state: string;
	private _screenLines: string[];
	private _accountName: string | null;
	private _cols = 80;
	private _rows = 24;
	private _seq = 0;
	/** Guards `stop()` so a double-teardown is a no-op rather than a throw. */
	private stopped = false;
	/**
	 * Every client→server frame received on the upstream /ws, in order. The mux
	 * edge forwards the browser's per-session /ws/{id} frames here verbatim, so
	 * this is where a test observes what the frontend actually sent (e.g. that
	 * normal keystrokes are operator-tier `input:send`, not admin `input:send:raw`).
	 */
	readonly wsInbound: unknown[] = [];

	constructor(opts: MockCoopOptions) {
		this.port = opts.port;
		this.sessionId = opts.sessionId;
		this._state = opts.initialState ?? "idle";
		this._accountName = opts.accountName ?? null;
		this._screenLines = opts.screenLines ?? [
			`Session ${opts.sessionId} ready`,
		];

		this.server = createServer((req, res) => this.handleRequest(req, res));
		this.wss = new WebSocketServer({ noServer: true });

		// Handle WebSocket upgrade requests on /ws
		this.server.on("upgrade", (req, socket, head) => {
			const url = new URL(
				req.url ?? "/",
				`http://localhost:${this.port}`,
			);
			if (url.pathname === "/ws") {
				this.wss.handleUpgrade(req, socket, head, (ws) => {
					this.wsClients.add(ws);
					ws.on("close", () => this.wsClients.delete(ws));
					ws.on("message", (raw: Buffer) =>
						this.handleWsMessage(ws, raw),
					);
					// Send synthetic initial state (matches real coop behaviour:
					// prev == next == current state on connect).
					ws.send(
						JSON.stringify({
							event: "transition",
							prev: this._state,
							next: this._state,
							seq: this._seq,
							cause: "",
						}),
					);
				});
			} else {
				socket.destroy();
			}
		});
	}

	/** Start listening. */
	async start(): Promise<void> {
		return new Promise((resolve) => {
			this.server.listen(this.port, "127.0.0.1", () => resolve());
		});
	}

	/**
	 * Stop the server. Idempotent, bounded, and never rejects.
	 *
	 * `http.Server#close` only stops *accepting* new connections — its callback
	 * does not fire until every already-open socket is gone, and the mux holds
	 * several against each mock it has a live session for: keep-alive sockets
	 * for the health/status/screen/usage pollers, plus the upgraded `/ws` socket
	 * of the event feed. Deregistering the session cancels those tasks, but that
	 * teardown is asynchronous and races this hook, so a plain `close()` here
	 * regularly waited on sockets the mux had not yet let go of.
	 *
	 * That is the `afterAll` hook timeout in gd-tY0Go9T2Cb: a hung `stop()`
	 * burns the whole 30s hook budget, and Playwright charges a hook timeout to
	 * the *last test in the file* — which is why `mux-sessions.spec.ts:190`
	 * showed up flaky while the real culprit was teardown.
	 *
	 * So: destroy the mux's sockets with `closeAllConnections()` (which is what
	 * actually lets the close callback fire), `terminate()` the WebSockets
	 * rather than asking politely with `close()` (a graceful close waits for a
	 * peer handshake that a cancelled feed will never send), and race the whole
	 * thing against a short deadline. Resolving on the deadline is safe: a
	 * leaked mock listener dies with the worker process, whereas a hung hook
	 * fails a passing test.
	 */
	async stop(): Promise<void> {
		if (this.stopped) return;
		this.stopped = true;

		for (const ws of this.wsClients) {
			try {
				ws.terminate();
			} catch {
				// Already gone — teardown is best-effort.
			}
		}
		this.wsClients.clear();
		try {
			this.wss.close();
		} catch {
			// Already closed.
		}

		const closed = new Promise<void>((resolve) => {
			this.server.close(() => resolve());
			// Must come after close(): destroys the sockets whose lingering open
			// state is what would otherwise keep the callback above from firing.
			this.server.closeAllConnections();
		});

		await Promise.race([closed, deadline(STOP_TIMEOUT_MS)]);
	}

	/**
	 * Handle a client→server frame on the upstream /ws. Records it, then
	 * answers `replay:get` with an empty `replay` frame — that flips the
	 * ReplayGate out of its pending state so the expanded terminal enables
	 * stdin (disableStdin=false), which is what lets a subsequent keystroke
	 * reach `term.onData` and produce an `input:send` frame. All other frames
	 * (input:send, resize, …) are recorded only.
	 */
	private handleWsMessage(ws: WebSocket, raw: Buffer): void {
		let msg: Record<string, unknown>;
		try {
			msg = JSON.parse(raw.toString());
		} catch {
			return;
		}
		this.wsInbound.push(msg);

		if (msg.event === "replay:get") {
			ws.send(
				JSON.stringify({
					event: "replay",
					data: "",
					offset: 0,
					next_offset: 0,
					total_written: 0,
				}),
			);
		}
	}

	/** Client→server frames whose `event` matches, in receive order. */
	inboundByEvent(event: string): Record<string, unknown>[] {
		return this.wsInbound.filter(
			(m): m is Record<string, unknown> =>
				typeof m === "object" &&
				m !== null &&
				(m as Record<string, unknown>).event === event,
		);
	}

	/** Update the reported state (triggers transition event on WebSocket). */
	setState(state: string): void {
		const prev = this._state;
		this._state = state;
		this._seq++;

		// Push transition event to all connected WebSocket clients
		const event = JSON.stringify({
			event: "transition",
			prev,
			next: state,
			seq: this._seq,
			cause: "test",
		});
		for (const ws of this.wsClients) {
			if (ws.readyState === ws.OPEN) {
				ws.send(event);
			}
		}
	}

	/** Update screen content. */
	setScreen(lines: string[]): void {
		this._screenLines = lines;
		this._seq++;
	}

	/**
	 * Update the reported account name (simulates a credential profile switch).
	 * The mux health poller picks this up on its next tick and reconciles the
	 * session's assigned_account to it.
	 */
	setAccountName(account: string | null): void {
		this._accountName = account;
	}

	get state(): string {
		return this._state;
	}

	private handleRequest(req: IncomingMessage, res: ServerResponse): void {
		const url = new URL(req.url ?? "/", `http://localhost:${this.port}`);

		if (url.pathname === "/api/v1/health") {
			this.json(res, {
				status: "running",
				session_id: this.sessionId,
				// Real coop includes account_name on health; the mux reconciles
				// each session's assigned_account from it. Omit the key entirely
				// when null so we mirror coop's serde skip-if-none behaviour.
				...(this._accountName != null
					? { account_name: this._accountName }
					: {}),
			});
			return;
		}

		if (url.pathname === "/api/v1/status") {
			this.json(res, {
				session_id: this.sessionId,
				state: this._state,
				pid: process.pid,
				uptime_secs: 100,
				exit_code: null,
				screen_seq: this._seq,
				bytes_read: 0,
				bytes_written: 0,
				ws_clients: this.wsClients.size,
				...(this._accountName != null
					? { account_name: this._accountName }
					: {}),
			});
			return;
		}

		if (url.pathname === "/api/v1/screen") {
			// Pad lines to fill rows
			const lines = [...this._screenLines];
			while (lines.length < this._rows) {
				lines.push("");
			}
			this.json(res, {
				lines: lines.slice(0, this._rows),
				ansi: lines.slice(0, this._rows),
				cols: this._cols,
				rows: this._rows,
			});
			return;
		}

		// POST /api/v1/input — accept keyboard input (for input tests)
		if (
			url.pathname === "/api/v1/input" &&
			req.method === "POST"
		) {
			let body = "";
			req.on("data", (chunk: Buffer) => {
				body += chunk.toString();
			});
			req.on("end", () => {
				this.json(res, { ok: true });
			});
			return;
		}

		res.writeHead(404);
		res.end("not found");
	}

	private json(res: ServerResponse, data: unknown): void {
		res.writeHead(200, { "Content-Type": "application/json" });
		res.end(JSON.stringify(data));
	}
}

/**
 * Stop every mock in parallel, best-effort. `stop()` is already bounded and
 * non-throwing, so this is the one-liner every spec's `afterAll` should use
 * instead of hand-rolling `Promise.all(mocks.map(...))` — two of the seven mux
 * specs had drifted into a version of that with no `.catch`, where one rejected
 * teardown discarded the rest.
 */
export async function stopMocks(mocks: MockCoop[]): Promise<void> {
	await Promise.all(mocks.map((m) => m.stop().catch(() => {})));
}

/** Start multiple mock coop servers on consecutive ports. */
export async function startMockCoops(
	basePort: number,
	count: number,
	namePrefix = "session",
): Promise<MockCoop[]> {
	const mocks: MockCoop[] = [];
	for (let i = 0; i < count; i++) {
		const mock = new MockCoop({
			port: basePort + i,
			sessionId: `${namePrefix}-${i}`,
		});
		await mock.start();
		mocks.push(mock);
	}
	return mocks;
}
