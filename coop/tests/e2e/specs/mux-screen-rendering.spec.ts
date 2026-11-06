// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import { test, expect } from "@playwright/test";
import {
	registerSession,
	waitForMuxHealth,
	openDashboard,
	sessionText,
	cleanupAllSessions,
	sleep,
	muxBaseUrl,
} from "../lib/harness.js";
import { MockCoop, stopMocks } from "../lib/mock-coop.js";

const MOCK_BASE_PORT = 18_400;
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

test.describe("screen rendering", () => {
	test("tile renders terminal preview with screen content", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT,
			sessionId: "scrn-1",
			initialState: "idle",
			screenLines: ["Hello from scrn-1!", "Line 2 of output"],
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"scrn-1",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "scrn-1" },
		);

		await openDashboard(page);

		// Wait for the session tile to appear
		await expect(sessionText(page, "scrn-1")).toBeVisible({
			timeout: 10_000,
		});

		// Tiles render screen content via TerminalPreview (<pre> element),
		// not xterm.js. Verify screen lines appear in the tile.
		await expect(page.getByText("Hello from scrn-1!")).toBeVisible({
			timeout: 15_000,
		});
	});

	test("screen_batch delivers content via WebSocket", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 1,
			sessionId: "scrn-ws",
			initialState: "working",
			screenLines: ["Initial output"],
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"scrn-ws",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "scrn-ws" },
		);

		await openDashboard(page);
		await expect(sessionText(page, "scrn-ws")).toBeVisible({
			timeout: 10_000,
		});

		// Wait for screen data to be polled and pushed via screen_batch.
		// Verify by checking the REST API which exposes cached screen.
		await sleep(3000);
		const resp = await fetch(
			`${muxBaseUrl}/api/v1/sessions/scrn-ws/screen`,
		);
		expect(resp.ok).toBe(true);
		const data = await resp.json();
		expect(data.lines[0]).toContain("Initial output");

		// Update the screen content
		mock.setScreen(["Updated output!", "New line here"]);
		await sleep(2000);

		const resp2 = await fetch(
			`${muxBaseUrl}/api/v1/sessions/scrn-ws/screen`,
		);
		expect(resp2.ok).toBe(true);
		const data2 = await resp2.json();
		expect(data2.lines[0]).toContain("Updated output!");
	});

	test("multiple sessions each get screen data", async ({ page }) => {
		const mock1 = new MockCoop({
			port: MOCK_BASE_PORT + 10,
			sessionId: "scrn-m1",
			initialState: "idle",
			screenLines: ["Screen 1 content"],
		});
		const mock2 = new MockCoop({
			port: MOCK_BASE_PORT + 11,
			sessionId: "scrn-m2",
			initialState: "working",
			screenLines: ["Screen 2 content"],
		});
		await mock1.start();
		await mock2.start();
		mocks.push(mock1, mock2);

		await registerSession(
			"scrn-m1",
			`http://127.0.0.1:${mock1.port}`,
			{ pod_name: "scrn-m1" },
		);
		await registerSession(
			"scrn-m2",
			`http://127.0.0.1:${mock2.port}`,
			{ pod_name: "scrn-m2" },
		);

		await openDashboard(page);

		await expect(sessionText(page, "scrn-m1")).toBeVisible({
			timeout: 10_000,
		});
		await expect(sessionText(page, "scrn-m2")).toBeVisible({
			timeout: 10_000,
		});

		// Both sessions should render their screen content in tile previews
		await expect(page.getByText("Screen 1 content")).toBeVisible({
			timeout: 15_000,
		});
		await expect(page.getByText("Screen 2 content")).toBeVisible({
			timeout: 15_000,
		});
	});

	test("screen API returns cached content after dashboard opens", async ({
		page,
	}) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 20,
			sessionId: "scrn-ca",
			initialState: "idle",
			screenLines: ["Cached line 1", "Cached line 2"],
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"scrn-ca",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "scrn-ca" },
		);

		// Open the dashboard to trigger WS subscription → pollers start
		await openDashboard(page);
		await expect(sessionText(page, "scrn-ca")).toBeVisible({
			timeout: 10_000,
		});

		// Wait for screen poll to cache data
		await sleep(3000);

		// The mux caches screen data from upstream
		const resp = await fetch(
			`${muxBaseUrl}/api/v1/sessions/scrn-ca/screen`,
		);
		expect(resp.ok).toBe(true);
		const data = await resp.json();
		expect(data.cols).toBe(80);
		expect(data.rows).toBe(24);
		expect(data.lines).toBeDefined();
		expect(data.lines.length).toBeGreaterThan(0);
	});
});
