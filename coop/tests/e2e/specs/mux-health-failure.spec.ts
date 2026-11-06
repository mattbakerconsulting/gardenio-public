// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import { test, expect } from "@playwright/test";
import {
	registerSession,
	listSessions,
	waitForMuxHealth,
	openDashboard,
	sessionText,
	cleanupAllSessions,
	sleep,
} from "../lib/harness.js";
import { MockCoop, stopMocks } from "../lib/mock-coop.js";

const MOCK_BASE_PORT = 18_300;
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

// Health failure tests wait for multiple health-check cycles (5s each, 3
// failures to evict), so they need a generous timeout.
test.describe("health failure", () => {
	test.setTimeout(60_000);
	test("session accumulates health failures after mock stops", async () => {
		// Register with a reachable mock first (mux validates health on register)
		const mock = new MockCoop({
			port: MOCK_BASE_PORT,
			sessionId: "will-fail",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"will-fail",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "will-fail" },
		);

		// Stop the mock — health checks will now fail
		await mock.stop();

		// Wait for health failures to accumulate (health-check-ms=5000)
		let failures = 0;
		for (let i = 0; i < 30; i++) {
			await sleep(1000);
			const sessions = await listSessions();
			const session = sessions.find((s) => s.id === "will-fail");
			if (!session) {
				// Session was removed due to health failures
				failures = 999;
				break;
			}
			failures = session.health_failures ?? 0;
			if (failures > 0) break;
		}
		expect(failures).toBeGreaterThan(0);
	});

	test("session removed after max health failures exceeded", async () => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 1,
			sessionId: "will-die",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"will-die",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "will-die" },
		);

		// Stop the mock to trigger health failures
		await mock.stop();

		// Wait for max-health-failures (3) to be exceeded
		// With health-check-ms=5000, this takes ~15-20 seconds
		let removed = false;
		for (let i = 0; i < 30; i++) {
			await sleep(1000);
			const sessions = await listSessions();
			if (!sessions.find((s) => s.id === "will-die")) {
				removed = true;
				break;
			}
		}
		expect(removed).toBe(true);
	});

	test("dashboard removes tile after health failure eviction", async ({
		page,
	}) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 2,
			sessionId: "hf-evct",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"hf-evct",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "hf-evct" },
		);

		await openDashboard(page);

		// Session should appear initially
		await expect(sessionText(page, "hf-evct")).toBeVisible({
			timeout: 10_000,
		});

		// Stop the mock to trigger health failures
		await mock.stop();

		// Wait for eviction: poll the REST API instead of DOM to avoid
		// strict-mode issues. Health check fires every 5s, max 3 failures
		// → eviction in ~15-20s.
		let evicted = false;
		for (let i = 0; i < 30; i++) {
			await sleep(1000);
			const sessions = await listSessions();
			if (!sessions.find((s) => s.id === "hf-evct")) {
				evicted = true;
				break;
			}
		}
		expect(evicted).toBe(true);

		// After eviction, the dashboard should update via WS — tile gone
		await expect(sessionText(page, "hf-evct")).not.toBeVisible({
			timeout: 5_000,
		});
	});

	test("healthy session survives while unhealthy one is evicted", async ({
		page,
	}) => {
		// One healthy mock, one that will be stopped
		const survivor = new MockCoop({
			port: MOCK_BASE_PORT + 3,
			sessionId: "hf-live",
			initialState: "idle",
		});
		const doomed = new MockCoop({
			port: MOCK_BASE_PORT + 4,
			sessionId: "hf-doom",
			initialState: "idle",
		});
		await survivor.start();
		await doomed.start();
		mocks.push(survivor, doomed);

		await registerSession(
			"hf-live",
			`http://127.0.0.1:${survivor.port}`,
			{ pod_name: "hf-live" },
		);
		await registerSession(
			"hf-doom",
			`http://127.0.0.1:${doomed.port}`,
			{ pod_name: "hf-doom" },
		);

		await openDashboard(page);

		// Both should appear initially
		await expect(sessionText(page, "hf-live")).toBeVisible({
			timeout: 10_000,
		});
		await expect(sessionText(page, "hf-doom")).toBeVisible({
			timeout: 10_000,
		});

		// Stop the doomed mock
		await doomed.stop();

		// Wait for eviction via REST API
		let evicted = false;
		for (let i = 0; i < 30; i++) {
			await sleep(1000);
			const sessions = await listSessions();
			if (!sessions.find((s) => s.id === "hf-doom")) {
				evicted = true;
				break;
			}
		}
		expect(evicted).toBe(true);

		// Doomed should be gone from dashboard, survivor stays
		await expect(sessionText(page, "hf-doom")).not.toBeVisible({
			timeout: 5_000,
		});
		await expect(sessionText(page, "hf-live")).toBeVisible();
	});

	test("session reappears after stop and restart", async () => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 5,
			sessionId: "restart-me",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"restart-me",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "restart-me" },
		);

		let sessions = await listSessions();
		expect(sessions.find((s) => s.id === "restart-me")).toBeTruthy();

		// Stop the mock — health checks will fail
		await mock.stop();

		// Wait for eviction
		let removed = false;
		for (let i = 0; i < 30; i++) {
			await sleep(1000);
			sessions = await listSessions();
			if (!sessions.find((s) => s.id === "restart-me")) {
				removed = true;
				break;
			}
		}
		expect(removed).toBe(true);

		// Restart mock on same port
		const mock2 = new MockCoop({
			port: MOCK_BASE_PORT + 5,
			sessionId: "restart-me",
			initialState: "working",
		});
		await mock2.start();
		mocks.push(mock2);

		// Re-register
		await registerSession(
			"restart-me",
			`http://127.0.0.1:${mock2.port}`,
			{ pod_name: "restart-me" },
		);

		sessions = await listSessions();
		expect(sessions.find((s) => s.id === "restart-me")).toBeTruthy();
	});
});
