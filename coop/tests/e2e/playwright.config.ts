// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import { defineConfig } from "@playwright/test";
import { dirname, resolve } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const MUX_PORT = Number(process.env.MUX_PORT ?? 19_800);
const MUX_BIN =
	process.env.MUX_BIN ??
	resolve(__dirname, "../../target/debug/coopmux");
const MUX_STATE_DIR =
	process.env.COOP_MUX_STATE_DIR ??
	resolve(tmpdir(), `coopmux-e2e-${process.pid}`);

export default defineConfig({
	testDir: "./specs",
	timeout: 30_000,
	retries: process.env.CI ? 1 : 0,
	workers: 1, // tests share a mux instance, run sequentially
	reporter: process.env.CI
		? [["json", { outputFile: "results/playwright.json" }], ["list"]]
		: [["list"]],
	use: {
		baseURL: `http://localhost:${MUX_PORT}`,
		trace: "retain-on-failure",
		screenshot: "only-on-failure",
		video: "retain-on-failure",
	},
	outputDir: "results/artifacts",
	projects: [
		{
			name: "chromium",
			use: { browserName: "chromium" },
		},
	],
	webServer: {
		// COOP_ALLOW_PLAINTEXT_BOOTSTRAP: test-only escape hatch so the mux's
		// fail-closed credential-key policy doesn't block startup in local/CI
		// runs that have no encryption key configured. Never used in production.
		// COOP_MUX_HEALTH_CHECK_MS: fast health-check interval so the per-session
		// account reconcile (mux health poller → assigned_account) converges
		// within test timeouts.
		command: `COOP_MUX_STATE_DIR="${MUX_STATE_DIR}" COOP_HOT=true COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1 COOP_MUX_HEALTH_CHECK_MS=500 "${MUX_BIN}" --port ${MUX_PORT} --host 127.0.0.1`,
		url: `http://localhost:${MUX_PORT}/api/v1/health`,
		reuseExistingServer: !process.env.CI,
		timeout: 10_000,
	},
});
