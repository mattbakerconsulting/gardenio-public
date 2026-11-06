// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

/**
 * Test harness for mux E2E tests.
 *
 * In CI (RWX), coopmux and coop instances run as background-processes with
 * ready-checks. The harness only manages session registration via the HTTP API.
 *
 * Locally, the harness can optionally spawn processes directly for convenience.
 */

import { type Locator, type Page } from "@playwright/test";

const MUX_PORT = Number(process.env.MUX_PORT ?? 19_800);
const MUX_AUTH_TOKEN = process.env.MUX_AUTH_TOKEN ?? "";

/** Base URL for the mux API. */
export const muxBaseUrl = `http://localhost:${MUX_PORT}`;

/**
 * Upper bound on every mux REST call this harness makes. `fetch` has no default
 * timeout, so a mux that accepts the connection and then stalls would hang the
 * caller until the enclosing test/hook timeout fired — the failure mode that
 * makes a teardown helper eat a 30s `afterAll` budget (gd-tY0Go9T2Cb).
 */
const API_TIMEOUT_MS = 5_000;

/** `fetch` against the mux, bounded by API_TIMEOUT_MS. */
function muxFetch(path: string, init?: RequestInit): Promise<Response> {
	return fetch(`${muxBaseUrl}${path}`, {
		...init,
		signal: AbortSignal.timeout(API_TIMEOUT_MS),
	});
}

/** Headers for authenticated mux API calls. */
function authHeaders(): Record<string, string> {
	const headers: Record<string, string> = {
		"Content-Type": "application/json",
	};
	if (MUX_AUTH_TOKEN) {
		headers["Authorization"] = `Bearer ${MUX_AUTH_TOKEN}`;
	}
	return headers;
}

/** Register a mock session with the mux. */
export async function registerSession(
	id: string,
	url: string,
	metadata?: Record<string, unknown>,
	accountName?: string,
): Promise<void> {
	const resp = await muxFetch(`/api/v1/sessions`, {
		method: "POST",
		headers: authHeaders(),
		body: JSON.stringify({
			id,
			url,
			metadata: metadata ?? {},
			// account_name mirrors what gb/coop send at registration (BOAT_ACCOUNT
			// / the session's active credential profile). Omitted when undefined.
			...(accountName != null ? { account_name: accountName } : {}),
		}),
	});
	if (!resp.ok) {
		const body = await resp.text();
		throw new Error(
			`Failed to register session ${id}: ${resp.status} ${body}`,
		);
	}
}

/** Deregister a session from the mux. */
export async function deregisterSession(id: string): Promise<void> {
	const resp = await muxFetch(`/api/v1/sessions/${id}`, {
		method: "DELETE",
		headers: authHeaders(),
	});
	if (!resp.ok && resp.status !== 404) {
		const body = await resp.text();
		throw new Error(
			`Failed to deregister session ${id}: ${resp.status} ${body}`,
		);
	}
}

/** List all registered sessions. */
export async function listSessions(): Promise<MuxSession[]> {
	const resp = await muxFetch(`/api/v1/sessions`, {
		headers: authHeaders(),
	});
	if (!resp.ok) {
		const body = await resp.text();
		throw new Error(`Failed to list sessions: ${resp.status} ${body}`);
	}
	return resp.json();
}

/** Wait for the mux health endpoint to respond. */
export async function waitForMuxHealth(
	timeoutMs = 10_000,
): Promise<void> {
	const start = Date.now();
	while (Date.now() - start < timeoutMs) {
		try {
			const resp = await muxFetch(`/api/v1/health`);
			if (resp.ok) return;
		} catch {
			// not ready
		}
		await sleep(200);
	}
	throw new Error(`Mux not healthy after ${timeoutMs}ms`);
}

/** Navigate to the mux dashboard and wait for WebSocket connection. */
export async function openDashboard(page: Page): Promise<void> {
	// Listen for WebSocket connection before navigating
	const wsConnected = page.waitForEvent("websocket", {
		predicate: (ws: { url(): string }) => ws.url().includes("/ws/mux"),
		timeout: 10_000,
	});

	const dashboardUrl = MUX_AUTH_TOKEN
		? `${muxBaseUrl}/mux?token=${MUX_AUTH_TOKEN}`
		: `${muxBaseUrl}/mux`;
	await page.goto(dashboardUrl);
	await wsConnected;
}

/**
 * Locator for a session id as rendered in the mux dashboard.
 *
 * The dashboard renders a session id in up to two places at once, and both are
 * intentional UI:
 *
 *   1. the sidebar card's flattened-metadata line — `pod_name: <id>`, because
 *      these specs register `pod_name` = the session id;
 *   2. the tile subtitle, which falls back to `id.slice(0, 8)` for a session
 *      that carries no `k8s.pod` metadata.
 *
 * Any session whose id is 8 characters or shorter therefore matches twice, and
 * a bare `page.getByText(id)` trips Playwright strict mode (gd-0izsavgity).
 * Specs asserting only "this session did / did not show up on the dashboard"
 * scope to the first match instead of the page-wide set. Use `getByRole`
 * against the tile button's accessible name when a spec needs the *tile*
 * specifically rather than any rendering of the id.
 */
export function sessionText(page: Page, id: string): Locator {
	return page.getByText(id).first();
}

/** Deregister all sessions (cleanup between tests). */
export async function cleanupAllSessions(): Promise<void> {
	try {
		const sessions = await listSessions();
		await Promise.all(sessions.map((s) => deregisterSession(s.id)));
	} catch {
		// best-effort cleanup
	}
}

export function sleep(ms: number): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, ms));
}

export interface MuxSession {
	id: string;
	url: string;
	metadata?: Record<string, unknown>;
	registered_at_ms?: number;
	health_failures?: number;
	cached_state?: string;
	// The per-session account, under the same key on REST and the WebSocket
	// feed (unified in ka-0000007897). The deprecated pre-unification REST
	// duplicate `account` was removed in ka-0000001779.
	assigned_account?: string | null;
}

/** Poll listSessions() until the named session's account matches, or time out. */
export async function waitForAssignedAccount(
	id: string,
	expected: string,
	timeoutMs = 10_000,
): Promise<void> {
	const start = Date.now();
	let last: string | null | undefined;
	while (Date.now() - start < timeoutMs) {
		const sessions = await listSessions();
		const s = sessions.find((x) => x.id === id);
		last = s?.assigned_account;
		if (last === expected) return;
		await sleep(200);
	}
	throw new Error(
		`session ${id} account never became ${expected} (last: ${String(last)})`,
	);
}
