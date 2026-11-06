// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

// Regression for ka-0000006818 (follow-up to MR !6364 backend fix
// ka-0000002552): the expanded mux terminal used to send normal keystrokes
// as `input:send:raw` on the direct per-session WS. The edge now reserves
// `input:send:raw` for admin-tier raw byte injection (mirroring REST
// /input/raw), so an edge-authed gardenio-operator got plain typing refused.
// Keystrokes must be operator-tier `input:send` on the direct WS too.
//
// This drives the REAL frontend bundle through coopmux and asserts the frame
// the browser actually sends when a key is typed in the expanded terminal —
// captured two independent ways: (1) the browser's own WebSocket via
// Playwright's frame events, and (2) what the coopmux edge forwarded to the
// upstream mock coop.

import { test, expect } from "@playwright/test";
import {
	registerSession,
	waitForMuxHealth,
	openDashboard,
	sessionText,
	cleanupAllSessions,
	sleep,
} from "../lib/harness.js";
import { MockCoop, stopMocks } from "../lib/mock-coop.js";

const MOCK_BASE_PORT = 18_700;
let mocks: MockCoop[] = [];

test.beforeAll(async () => {
	await waitForMuxHealth();
});

test.beforeEach(async () => {
	await cleanupAllSessions();
});

test.afterAll(async () => {
	await cleanupAllSessions();
	await stopMocks(mocks);
	mocks = [];
});

test.describe("expanded terminal input tier", () => {
	test("normal keystrokes use operator-tier input:send, not admin input:send:raw", async ({
		page,
	}) => {
		const sessionId = "input-tier";
		const mock = new MockCoop({
			port: MOCK_BASE_PORT,
			sessionId,
			initialState: "idle",
			screenLines: ["type here"],
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			sessionId,
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: sessionId },
		);

		// Capture every frame the browser SENDS on the direct per-session WS
		// (/ws/<id>), the exact bytes the frontend produced. Registered before
		// navigation so the expanded view's socket is caught when it opens.
		const sentFrames: string[] = [];
		page.on("websocket", (ws) => {
			if (!ws.url().includes(`/ws/${sessionId}`)) return;
			ws.on("framesent", (payload) => {
				const data = payload.payload;
				if (typeof data === "string") sentFrames.push(data);
			});
		});

		await openDashboard(page);
		await expect(sessionText(page, sessionId)).toBeVisible({
			timeout: 10_000,
		});

		// Expand the session via its deep-link hash — deterministic, avoids
		// ambiguous tile-locator matching. This opens the direct /ws/<id>.
		await page.evaluate((id) => {
			window.location.hash = `#${id}`;
		}, sessionId);

		// Expanded view is up once the Close button renders.
		await expect(
			page.getByRole("button", { name: "Close" }),
		).toBeVisible({ timeout: 10_000 });

		// The mock answers replay:get, which flips the terminal out of its
		// pending state (disableStdin=false). Focus the terminal and type.
		await page.locator(".xterm-screen, .xterm").first().click();
		await sleep(300);
		await page.keyboard.type("hi");
		await sleep(500);

		// (1) Browser-sent frames: an input:send frame went out, no raw frame.
		const parsedSent = sentFrames
			.map((f) => {
				try {
					return JSON.parse(f);
				} catch {
					return null;
				}
			})
			.filter((m): m is Record<string, unknown> => m != null);

		const sentInput = parsedSent.filter((m) => m.event === "input:send");
		const sentRaw = parsedSent.filter(
			(m) => m.event === "input:send:raw",
		);
		expect(
			sentRaw,
			"frontend must not send admin-reserved input:send:raw for keystrokes",
		).toHaveLength(0);
		expect(
			sentInput.length,
			"frontend must send at least one operator-tier input:send frame",
		).toBeGreaterThan(0);
		// The text field carries the typed characters verbatim.
		const typed = sentInput.map((m) => m.text).join("");
		expect(typed).toContain("h");

		// (2) Independent cross-check: what the edge forwarded to the upstream.
		const upstreamRaw = mock.inboundByEvent("input:send:raw");
		const upstreamInput = mock.inboundByEvent("input:send");
		expect(
			upstreamRaw,
			"upstream coop must never receive input:send:raw for keystrokes",
		).toHaveLength(0);
		expect(upstreamInput.length).toBeGreaterThan(0);
	});
});
