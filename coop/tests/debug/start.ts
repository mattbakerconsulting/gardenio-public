#!/usr/bin/env bun
// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// Debug helper: build coop, spawn it wrapping a command, open browser terminal.
//
// Usage:
//   bun tests/debug/start.ts                                  # wrap bash
//   bun tests/debug/start.ts --port 8080 -- python3 -i        # wrap python
//   COOP_AGENT=claude bun tests/debug/start.ts -- claude      # with agent detection

import { parseArgs } from "node:util";
import {
	buildCoop,
	buildWeb,
	coopBin,
	findAvailablePort,
	onExit,
	openBrowser,
	openBrowserUrl,
	waitForHealth,
} from "./lib/setup";

const { values, positionals } = parseArgs({
	args: Bun.argv.slice(2),
	options: {
		port: { type: "string", default: "7070" },
		"no-build": { type: "boolean", default: false },
		"no-open": { type: "boolean", default: false },
	},
	allowPositionals: true,
	strict: false,
});

const port = await findAvailablePort(Number(values.port));
const cmd = positionals.length ? positionals : ["/bin/bash"];

if (!values["no-build"]) {
	await buildWeb();
	await buildCoop();
}

const bin = coopBin();
if (!(await Bun.file(bin).exists())) {
	console.error(`error: ${bin} not found; run without --no-build`);
	process.exit(1);
}

console.log(`Starting coop on port ${port}: ${cmd.join(" ")}`);
const proc = Bun.spawn(
	[bin, "--port", String(port), "--log-format", "text", "--hot", "--", ...cmd],
	{
		stdout: "inherit",
		stderr: "inherit",
		stdin: "inherit",
	},
);
onExit(() => proc.kill());

await waitForHealth(port, { proc });

if (!values["no-open"]) {
	const muxUrl = "http://127.0.0.1:9800";
	const muxRunning = await fetch(`${muxUrl}/api/v1/health`)
		.then((r) => r.ok)
		.catch(() => false);
	if (muxRunning) {
		console.log(`Mux running — open ${muxUrl}/mux`);
		await openBrowserUrl(`${muxUrl}/mux`);
	} else {
		await openBrowser(port);
	}
}

const exitCode = await proc.exited;
process.exit(exitCode ?? 1);
