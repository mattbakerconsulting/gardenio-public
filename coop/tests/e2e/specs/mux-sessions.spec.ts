// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import { test, expect } from "@playwright/test";
import {
	registerSession,
	deregisterSession,
	listSessions,
	waitForMuxHealth,
	openDashboard,
	sessionText,
	cleanupAllSessions,
} from "../lib/harness.js";
import { MockCoop, startMockCoops, stopMocks } from "../lib/mock-coop.js";

const MOCK_BASE_PORT = 18_100;

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

test.describe("session registration", () => {
	test("registers a session via HTTP API", async () => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT,
			sessionId: "test-single",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"test-single",
			`http://127.0.0.1:${MOCK_BASE_PORT}`,
			{ pod_name: "test-pod", role: "crew" },
		);

		const sessions = await listSessions();
		expect(sessions).toHaveLength(1);
		expect(sessions[0].id).toBe("test-single");
	});

	test("registers multiple sessions", async () => {
		const newMocks = await startMockCoops(MOCK_BASE_PORT + 10, 3, "multi");
		mocks.push(...newMocks);

		for (const mock of newMocks) {
			await registerSession(
				mock.sessionId,
				`http://127.0.0.1:${mock.port}`,
				{ pod_name: mock.sessionId },
			);
		}

		const sessions = await listSessions();
		expect(sessions).toHaveLength(3);
		const ids = sessions.map((s) => s.id).sort();
		expect(ids).toEqual(["multi-0", "multi-1", "multi-2"]);
	});

	test("deregisters a session", async () => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 20,
			sessionId: "to-remove",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"to-remove",
			`http://127.0.0.1:${mock.port}`,
		);

		let sessions = await listSessions();
		expect(sessions).toHaveLength(1);

		await deregisterSession("to-remove");

		sessions = await listSessions();
		expect(sessions).toHaveLength(0);
	});
});

test.describe("dashboard UI", () => {
	test("shows registered sessions as tiles", async ({ page }) => {
		const newMocks = await startMockCoops(MOCK_BASE_PORT + 30, 3, "tile");
		mocks.push(...newMocks);

		for (const mock of newMocks) {
			await registerSession(
				mock.sessionId,
				`http://127.0.0.1:${mock.port}`,
				{ pod_name: mock.sessionId, role: "crew" },
			);
		}

		await openDashboard(page);

		// Wait for session tiles to appear — look for session IDs in the page.
		for (const mock of newMocks) {
			await expect(
				sessionText(page, mock.sessionId),
			).toBeVisible({ timeout: 10_000 });
		}
	});

	test("updates session count in header", async ({ page }) => {
		const newMocks = await startMockCoops(MOCK_BASE_PORT + 40, 2, "count");
		mocks.push(...newMocks);

		for (const mock of newMocks) {
			await registerSession(
				mock.sessionId,
				`http://127.0.0.1:${mock.port}`,
				{ pod_name: mock.sessionId },
			);
		}

		await openDashboard(page);

		// The header should show "2 sessions" in the stats
		await expect(
			page.getByText("2 sessions"),
		).toBeVisible({ timeout: 10_000 });
	});

	test("removes tile when session goes offline", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 50,
			sessionId: "ephemeral",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"ephemeral",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "ephemeral" },
		);

		await openDashboard(page);

		// Session should appear
		await expect(
			sessionText(page, "ephemeral"),
		).toBeVisible({ timeout: 10_000 });

		// Deregister — dashboard should receive session:offline via WS
		await deregisterSession("ephemeral");

		// Session should disappear
		await expect(
			sessionText(page, "ephemeral"),
		).not.toBeVisible({ timeout: 10_000 });
	});

	test("shows runtime class chip from k8s metadata", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 70,
			sessionId: "sandboxed",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"sandboxed",
			`http://127.0.0.1:${mock.port}`,
			{ k8s: { pod: "sandboxed-pod", runtime_class: "runsc" } },
		);

		await openDashboard(page);

		// The tile (and sidebar card) should render the runtime class chip.
		await expect(
			page.getByText("runsc").first(),
		).toBeVisible({ timeout: 10_000 });
	});

	test("shows session:online event for new registration", async ({
		page,
	}) => {
		await openDashboard(page);

		// No sessions initially
		const sessions = await listSessions();
		expect(sessions).toHaveLength(0);

		// Register while dashboard is open
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 60,
			sessionId: "late-joiner",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"late-joiner",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "late-joiner" },
		);

		// Should appear via session:online WebSocket event
		await expect(
			sessionText(page, "late-joiner"),
		).toBeVisible({ timeout: 10_000 });
	});
});
