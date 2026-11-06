// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

/**
 * Account-display accuracy (ka-0000002509).
 *
 * The dashboard must show the account a session is ACTUALLY using, sourced from
 * the session's reported `account_name` (= its Active credential profile), and
 * reconciled by the mux health poller into `assigned_account`. These tests pin
 * that contract end-to-end against the real coopmux:
 *
 *  - the account reported at registration surfaces on the tile + the API;
 *  - when a session later reports a DIFFERENT account (a credential switch),
 *    the display follows the live account, not the registration intent;
 *  - the header account indicator reflects the real per-session account(s),
 *    not a fleet pool heuristic (the old "first healthy account" bug).
 */

import { test, expect } from "@playwright/test";
import {
	registerSession,
	listSessions,
	waitForMuxHealth,
	waitForAssignedAccount,
	openDashboard,
	sessionText,
	cleanupAllSessions,
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

test.describe("account display accuracy", () => {
	test("session-reported account surfaces via the API", async () => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT,
			sessionId: "acct-api",
			accountName: "matthewcbaker0122",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"acct-api",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "acct-api", role: "thread", project: "gardenio" },
			"matthewcbaker0122",
		);

		// Seeded at registration AND confirmed by the health reconcile.
		await waitForAssignedAccount("acct-api", "matthewcbaker0122");
		const sessions = await listSessions();
		const s = sessions.find((x) => x.id === "acct-api");
		expect(s?.assigned_account).toBe("matthewcbaker0122");
		// The deprecated REST duplicate key `account` was removed in
		// ka-0000001779 — the response must carry the canonical key only.
		expect(
			(s as Record<string, unknown> | undefined)?.account,
		).toBeUndefined();
	});

	test("tile shows the account the session is using", async ({ page }) => {
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 1,
			sessionId: "acct-tile",
			accountName: "gardenio2",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"acct-tile",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "acct-tile", role: "crew", project: "gardenio" },
			"gardenio2",
		);
		// Ensure the account is reconciled before opening the dashboard so the
		// initial session snapshot carries it (avoids a render race).
		await waitForAssignedAccount("acct-tile", "gardenio2");

		await openDashboard(page);

		await expect(sessionText(page, "acct-tile")).toBeVisible({ timeout: 10_000 });
		// The account label (Tile.tsx) renders the reconciled account. The name
		// appears in several places (header chip, sidebar, tile badge); assert at
		// least one account label with the title "Account: gardenio2" is visible —
		// that title is unique to the per-session Tile badge.
		await expect(
			page.getByTitle("Account: gardenio2", { exact: false }).first(),
		).toBeVisible({ timeout: 10_000 });
	});

	test("display follows a credential switch (health reconcile)", async () => {
		// Register reporting account A, then have the session report account B —
		// as happens after coopmux switches the session's active profile. The
		// displayed account must follow the live value, not the registration
		// intent.
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 2,
			sessionId: "acct-switch",
			accountName: "gardenio2",
		});
		await mock.start();
		mocks.push(mock);

		await registerSession(
			"acct-switch",
			`http://127.0.0.1:${mock.port}`,
			{ pod_name: "acct-switch", role: "crew", project: "gardenio" },
			"gardenio2",
		);
		await waitForAssignedAccount("acct-switch", "gardenio2");

		// Simulate a profile switch: the session now reports a different account.
		mock.setAccountName("gardenio3");
		await waitForAssignedAccount("acct-switch", "gardenio3");
	});

	test("missing reported account does not invent one", async () => {
		// A session that reports no account_name (e.g. a profile was never seeded)
		// must show a null assigned_account — never a fabricated/arbitrary value.
		const mock = new MockCoop({
			port: MOCK_BASE_PORT + 3,
			sessionId: "acct-none",
			// no accountName
		});
		await mock.start();
		mocks.push(mock);

		await registerSession("acct-none", `http://127.0.0.1:${mock.port}`, {
			pod_name: "acct-none",
			role: "crew",
			project: "gardenio",
		});

		// Give the health poller a couple of cycles to run.
		await waitForMuxHealth();
		const sessions = await listSessions();
		const s = sessions.find((x) => x.id === "acct-none");
		expect(s).toBeTruthy();
		expect(s?.assigned_account ?? null).toBeNull();
		// The deprecated REST duplicate key `account` was removed in
		// ka-0000001779 — it must not appear on the response at all.
		expect(
			(s as Record<string, unknown> | undefined)?.account,
		).toBeUndefined();
	});

	test("header reflects the real per-session account, not a pool pick", async ({
		page,
	}) => {
		// Two sessions, both on the same account. The header indicator must show
		// that account (derived from the live sessions), not a pool heuristic.
		const m1 = new MockCoop({
			port: MOCK_BASE_PORT + 4,
			sessionId: "hdr-1",
			accountName: "gardenio2",
		});
		const m2 = new MockCoop({
			port: MOCK_BASE_PORT + 5,
			sessionId: "hdr-2",
			accountName: "gardenio2",
		});
		await m1.start();
		await m2.start();
		mocks.push(m1, m2);

		for (const m of [m1, m2]) {
			await registerSession(
				m.sessionId,
				`http://127.0.0.1:${m.port}`,
				{ pod_name: m.sessionId, role: "crew", project: "gardenio" },
				"gardenio2",
			);
		}
		await waitForAssignedAccount("hdr-1", "gardenio2");
		await waitForAssignedAccount("hdr-2", "gardenio2");

		await openDashboard(page);

		// Header chip shows the single shared account (not "N accounts", not a
		// pool-heuristic account no session is on).
		const header = page.locator("header");
		await expect(header.getByText("gardenio2", { exact: false })).toBeVisible({
			timeout: 10_000,
		});
	});
});
