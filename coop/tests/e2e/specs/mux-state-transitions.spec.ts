// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import { test, expect, type Page } from "@playwright/test";
import {
	registerSession,
	waitForMuxHealth,
	openDashboard,
	sessionText,
	cleanupAllSessions,
} from "../lib/harness.js";
import { MockCoop, stopMocks } from "../lib/mock-coop.js";

const MOCK_BASE_PORT = 18_200;
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

/**
 * Wait for the state badge on a session tile to show the expected state.
 *
 * The tile is a button that contains the (truncated) session name and
 * a badge span with the state text. We locate the tile button in the
 * main grid (the second matching button — the first is in the sidebar)
 * and wait for its accessible name to include the expected state.
 */
async function expectTileState(
	page: Page,
	sessionName: string,
	expectedState: string,
	timeout = 10_000,
): Promise<void> {
	// The tile button's accessible name includes session name + badge text.
	// e.g. "127.0.0.1:18200 idle-tes idle"
	// Both sidebar and tile buttons match, so use .last() for the tile.
	const namePattern = new RegExp(
		`${sessionName.slice(0, 8)}.*${expectedState}`,
		"i",
	);
	const tile = page.getByRole("button", { name: namePattern }).last();
	await expect(tile).toBeVisible({ timeout });
}

test.describe("state transitions", () => {
	test("idle session shows idle badge", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT,
			sessionId: "idle-test",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession("idle-test", `http://127.0.0.1:${mock.port}`, {
			pod_name: "idle-test",
		});

		await openDashboard(page);
		await expect(sessionText(page, "idle-test")).toBeVisible({
			timeout: 10_000,
		});

		// Wait for the mux event feed to connect and push initial state
		await expectTileState(page, "idle-test", "idle");
	});

	test("working session shows working badge", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 1,
			sessionId: "working-test",
			initialState: "working",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"working-test",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "working-test" },
		);

		await openDashboard(page);
		await expectTileState(page, "working-test", "working");
	});

	test("state transition from idle to working updates badge", async ({
		page,
	}) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 2,
			sessionId: "transition-test",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"transition-test",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "transition-test" },
		);

		await openDashboard(page);
		await expectTileState(page, "transition-test", "idle");

		// Change state on the mock
		mock.setState("working");

		// Dashboard should update via WebSocket transition event
		await expectTileState(page, "transition-test", "working");
	});

	test("state transition from working back to idle", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 3,
			sessionId: "cycle-test",
			initialState: "working",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"cycle-test",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "cycle-test" },
		);

		await openDashboard(page);
		await expectTileState(page, "cycle-test", "working");

		mock.setState("idle");

		await expectTileState(page, "cycle-test", "idle");
	});

	test("multiple sessions with different states", async ({ page }) => {
		const mockIdle = new MockCoop({
			port: MOCK_BASE_PORT + 10,
			sessionId: "multi-idle",
			initialState: "idle",
		});
		const mockWorking = new MockCoop({
			port: MOCK_BASE_PORT + 11,
			sessionId: "multi-working",
			initialState: "working",
		});
		await mockIdle.start();
		await mockWorking.start();
		mocks.push(mockIdle, mockWorking);

		await registerSession(
			"multi-idle",
			`http://127.0.0.1:${mockIdle.port}`,
			{ pod_name: "multi-idle" },
		);
		await registerSession(
			"multi-working",
			`http://127.0.0.1:${mockWorking.port}`,
			{ pod_name: "multi-working" },
		);

		await openDashboard(page);

		// Both should be visible
		await expect(sessionText(page, "multi-idle")).toBeVisible({
			timeout: 10_000,
		});
		await expect(sessionText(page, "multi-working")).toBeVisible({
			timeout: 10_000,
		});

		// Stats should show 2 sessions
		await expect(page.getByText("2 sessions")).toBeVisible({
			timeout: 10_000,
		});
	});

	test("session healthy count updates on state change", async ({
		page,
	}) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 20,
			sessionId: "health-count",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"health-count",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "health-count" },
		);

		await openDashboard(page);
		await expect(page.getByText("1 session")).toBeVisible({
			timeout: 10_000,
		});

		// healthy count should show 1 once event feed connects
		await expect(page.getByText("1 healthy")).toBeVisible({
			timeout: 10_000,
		});
	});
});
