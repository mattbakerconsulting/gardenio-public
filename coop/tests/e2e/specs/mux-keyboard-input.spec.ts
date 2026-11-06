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
} from "../lib/harness.js";
import { MockCoop, stopMocks } from "../lib/mock-coop.js";

const MOCK_BASE_PORT = 18_500;
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

test.describe("keyboard input", () => {
	test("sidebar toggle with Ctrl+B", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT,
			sessionId: "kb-side",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"kb-side",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "kb-side" },
		);

		await openDashboard(page);
		await expect(sessionText(page, "kb-side")).toBeVisible({
			timeout: 10_000,
		});

		// Toggle with Ctrl+B — this is a smoke test that the shortcut
		// doesn't crash the dashboard
		await page.keyboard.press("Control+b");
		await sleep(300);

		// The dashboard should still be functional after the shortcut
		await expect(page.locator("h1")).toBeVisible();
	});

	test("clicking a tile opens expanded view", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 1,
			sessionId: "kb-clk",
			initialState: "idle",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"kb-clk",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "kb-clk" },
		);

		await openDashboard(page);
		await expect(sessionText(page, "kb-clk")).toBeVisible({
			timeout: 10_000,
		});

		// Click the tile — in the mux dashboard, clicking a tile opens
		// it in an expanded view with a terminal input and close button.
		// `.last()` is the grid tile; the sidebar card matches first.
		const namePattern = /kb-clk.*idle/i;
		const tile = page
			.getByRole("button", { name: namePattern })
			.last();
		await tile.click();

		// Expanded view should show a close button and terminal input. The
		// web-first assertion is the wait — no sleep() needed, and a generous
		// timeout so a slow first terminal mount is not a failure.
		await expect(
			page.getByRole("button", { name: "Close" }),
		).toBeVisible({ timeout: 10_000 });
	});

	test("Escape key closes expanded view", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 2,
			sessionId: "kb-esc",
			initialState: "idle",
			screenLines: ["Press Escape to close"],
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"kb-esc",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "kb-esc" },
		);

		await openDashboard(page);
		await expect(sessionText(page, "kb-esc")).toBeVisible({
			timeout: 10_000,
		});

		// Expand via the deep-link hash, the same deterministic path
		// mux-input-tier.spec.ts uses ("avoids ambiguous tile-locator
		// matching"). The previous `tile.dblclick()` was unsound: `toggleExpand`
		// *toggles*, and expanding removes the tile from the grid, so the second
		// click of the dblclick landed on whatever the expanded overlay had just
		// painted at those coordinates. Clicking a tile to expand is covered by
		// the test above; this one is about Escape.
		await page.evaluate(() => {
			window.location.hash = "#kb-esc";
		});

		// Expanded view is up once its Close button renders.
		const closeButton = page.getByRole("button", { name: "Close" });
		await expect(closeButton).toBeVisible({ timeout: 10_000 });

		// Press Escape to close expanded view.
		await page.keyboard.press("Escape");

		// The expanded view must actually be gone. The old assertion ("the tile
		// is still visible") was vacuous: `sessionText().first()` resolves to
		// the sidebar card, which is rendered whether or not a session is
		// expanded, so it passed even when Escape did nothing.
		await expect(closeButton).toBeHidden({ timeout: 10_000 });
		await expect(sessionText(page, "kb-esc")).toBeVisible();
	});
});
