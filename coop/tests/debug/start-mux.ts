#!/usr/bin/env bun
// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// Debug helper: build and launch coopmux dashboard (sessions connect automatically).
//
// Usage:
//   bun tests/debug/start-mux.ts                              # start mux on default port
//   bun tests/debug/start-mux.ts --mux-port 9900              # custom port
//   bun tests/debug/start-mux.ts --launch 'coop -- claude'    # with launch command

import { parseArgs } from "node:util";
import { $ } from "bun";
import {
	buildAll,
	buildMux,
	buildWeb,
	coopBin,
	coopmuxBin,
	onExit,
	openBrowserUrl,
	waitForHealth,
} from "./lib/setup";
import { loadEnvFile, resolveCredential } from "./lib/credentials";

const { values } = parseArgs({
	args: Bun.argv.slice(2),
	options: {
		"mux-port": { type: "string", default: "9800" },
		launch: { type: "string" },
		"no-build": { type: "boolean", default: false },
		"no-open": { type: "boolean", default: false },
	},
	strict: false,
});

const muxPort = Number(values["mux-port"]);
const launch = values.launch ?? undefined;

// -- Build ------------------------------------------------------------------

if (!values["no-build"]) {
	await buildWeb();
	await buildAll();
}

const muxBin = coopmuxBin();
if (!(await Bun.file(muxBin).exists())) {
	console.error(`error: coopmux not found at ${muxBin}; run without --no-build`);
	process.exit(1);
}

// -- Build launch command ---------------------------------------------------

const muxArgs: string[] = ["--port", String(muxPort), "--hot"];

if (launch) {
	const launchCmd = `_wd="\${WORKING_DIR:-.}" && case "$_wd" in "~/"*) _wd="$HOME/\${_wd#"~/"}" ;; "~") _wd="$HOME" ;; esac && cd "$_wd" && ${coopBin()} --port 0 --log-format text --hot -- ${launch}`;
	muxArgs.push("--launch", launchCmd);
}

// -- Start coopmux ----------------------------------------------------------

console.log(`Starting coopmux on port ${muxPort}`);
const muxProc = Bun.spawn([muxBin, ...muxArgs], {
	stdout: "inherit",
	stderr: "inherit",
	stdin: launch ? "inherit" : "ignore",
});
onExit(() => muxProc.kill());

await waitForHealth(muxPort, { proc: muxProc });

// -- Auto-seed local credential ---------------------------------------------

const muxUrl = `http://127.0.0.1:${muxPort}`;

await loadEnvFile();
try {
	const cred = await resolveCredential();
	const envKey =
		cred.type === "oauth_token"
			? "CLAUDE_CODE_OAUTH_TOKEN"
			: "ANTHROPIC_API_KEY";
	const token = cred.type === "oauth_token" ? cred.token : cred.key;
	const coop = coopBin();
	const env = { ...process.env, COOP_MUX_URL: muxUrl };
	const acctName = "Local (macOS)";

	// Build optional flags shared by both new and set.
	const extraArgs: string[] = [];
	if (cred.type === "oauth_token" && cred.refreshToken) {
		extraArgs.push("--refresh-token", cred.refreshToken);
	}
	if (cred.type === "oauth_token" && cred.expiresAt) {
		const ttl = Math.max(0, Math.floor((cred.expiresAt - Date.now()) / 1000));
		extraArgs.push("--expires-in", String(ttl));
	}

	// Try `cred new`; fall back to `cred set` if the account already exists.
	const newArgs: string[] = [
		"cred", "new", acctName,
		"--provider", "claude",
		"--env-key", envKey,
		"--token", token,
		...extraArgs,
	];
	if (cred.type === "api_key" || (cred.type === "oauth_token" && !cred.refreshToken)) {
		newArgs.push("--no-reauth");
	}

	const created = await $`${coop} ${newArgs}`.env(env).quiet().nothrow();
	if (created.exitCode !== 0) {
		// Account likely exists from persisted state — update its token.
		const setArgs = ["cred", "set", acctName, "--token", token, ...extraArgs];
		await $`${coop} ${setArgs}`.env(env).quiet();
	}
	console.log(`Seeded local credential (${cred.type})`);
} catch (err) {
	console.log(
		`Note: no local credential found, skipping auto-seed (${err instanceof Error ? err.message : err})`,
	);
}

// -- Open dashboard ---------------------------------------------------------

if (!values["no-open"]) {
	await openBrowserUrl(`${muxUrl}/mux`);
}

console.log(`\nMux dashboard: ${muxUrl}/mux`);
console.log("Sessions will appear as they connect.");
console.log("Press Ctrl+C to stop coopmux.\n");

// Wait for mux to exit
const exitCode = await muxProc.exited;
process.exit(exitCode ?? 1);
