#!/usr/bin/env bun

// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// Human-driven state capture for coop + claude onboarding.
//
// Builds the Docker claude image, mounts a local directory as
// CLAUDE_CONFIG_DIR, opens a browser terminal, and auto-snapshots
// whenever the config directory changes.
//
// Usage:
//   bun tests/debug/capture-claude.ts                       # empty config, full onboarding
//   bun tests/debug/capture-claude.ts --config trusted      # pre-trusted workspace
//   bun tests/debug/capture-claude.ts --config authorized   # auth'd, need workspace trust
//   bun tests/debug/capture-claude.ts --name my-session     # custom session name
//   bun tests/debug/capture-claude.ts --local               # run locally instead of Docker

import type { Dirent } from "node:fs";
import { cp, mkdir, mkdtemp, readdir, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, relative } from "node:path";
import { parseArgs } from "node:util";
import { $ } from "bun";
import {
	type Credential,
	loadEnvFile,
	resolveCredential,
	resolveOAuthAccount,
	writeCredentials,
} from "./lib/credentials";
import {
	buildCoop,
	coopBin,
	onExit,
	openBrowser,
	pullImage,
	scriptDir,
	waitForHealth,
} from "./lib/setup";

const { values } = parseArgs({
	args: Bun.argv.slice(2),
	options: {
		port: { type: "string", default: "7070" },
		config: { type: "string", default: "empty" },
		name: { type: "string" },
		docker: { type: "boolean", default: false },
		local: { type: "boolean", default: false },
		auth: { type: "boolean", default: false },
		"no-build": { type: "boolean", default: false },
		"no-open": { type: "boolean", default: false },
	},
	strict: true,
});

const port = Number(values.port);
const configMode = values.config as string;
const sessionName =
	values.name ?? new Date().toISOString().replace(/[T:]/g, "-").slice(0, 19);
const useDocker = !values.local; // Docker is the default
const passAuth = values.auth ?? false;


const captureDir = join(scriptDir(), "captures", sessionName);
const configDir = join(captureDir, "config");
const snapDir = join(captureDir, "snapshots");
const diffDir = join(captureDir, "diffs");
const agentDir = join(captureDir, "agent-state");
const screenDir = join(captureDir, "screens");

const workspace = await mkdtemp(join(tmpdir(), "coop-capture-"));
const containerWs = useDocker ? "/workspace" : workspace;

await mkdir(configDir, { recursive: true });
await mkdir(snapDir, { recursive: true });
await mkdir(diffDir, { recursive: true });
await mkdir(agentDir, { recursive: true });
await mkdir(screenDir, { recursive: true });


await loadEnvFile();

if (configMode === "trusted" || configMode === "authorized") {
	let credential: Credential;
	let account: Record<string, unknown>;
	try {
		credential = await resolveCredential();
	} catch (e) {
		console.error(`error: ${(e as Error).message}`);
		process.exit(1);
	}
	try {
		account = await resolveOAuthAccount();
	} catch (e) {
		console.error(`error: ${(e as Error).message}`);
		process.exit(1);
	}

	if (configMode === "trusted") {
		await writeCredentials(configDir, credential, account, {
			hasCompletedOnboarding: true,
			projects: {
				[containerWs]: {
					hasTrustDialogAccepted: true,
					allowedTools: [],
					hasCompletedProjectOnboarding: true,
				},
			},
		});
		await Bun.write(join(workspace, "CLAUDE.md"), "");
	} else {
		// authorized
		await writeCredentials(configDir, credential, account, {
			hasCompletedOnboarding: true,
			projects: {},
		});
	}
} else if (configMode !== "empty") {
	console.error(
		`Unknown config mode: ${configMode} (expected: empty, authorized, trusted)`,
	);
	process.exit(1);
}


let snapNum = 0;
let prevTag = "";

const RSYNC_EXCLUDES = [
	"--exclude=debug/",
	"--exclude=cache/",
	"--exclude=statsig/",
	"--exclude=.claude.json.backup.*",
];

/** Copy config into dest, filtering noisy files */
async function copyConfig(dest: string): Promise<void> {
	await mkdir(dest, { recursive: true });
	try {
		await $`rsync -a ${RSYNC_EXCLUDES} ${configDir}/ ${dest}/`.quiet();
	} catch {
		// rsync not available, fall back to cp + cleanup
		await cp(configDir, dest, { recursive: true });
		for (const name of ["debug", "cache", "statsig"]) {
			await $`rm -rf ${join(dest, name)}`.quiet().nothrow();
		}
	}
}

/** Recursively list files in a directory, returning relative paths */
async function listFiles(dir: string): Promise<string[]> {
	const results: string[] = [];
	async function walk(current: string): Promise<void> {
		let entries: Dirent[];
		try {
			entries = await readdir(current, { withFileTypes: true });
		} catch {
			return;
		}
		for (const entry of entries) {
			const full = join(current, entry.name);
			if (entry.isDirectory()) {
				await walk(full);
			} else {
				results.push(relative(dir, full));
			}
		}
	}
	await walk(dir);
	return results.sort();
}

async function snapshot(name: string): Promise<void> {
	const tag = `${String(snapNum).padStart(3, "0")}-${name}`;
	const dest = join(snapDir, tag);

	// Copy config to temp dir first to check for changes
	const tmpSnap = await mkdtemp(join(tmpdir(), "coop-snap-"));
	await copyConfig(tmpSnap);

	// If we have a previous snapshot, diff against it — skip if nothing changed
	if (prevTag) {
		const diff = await $`diff -rq ${join(snapDir, prevTag)} ${tmpSnap}`
			.quiet()
			.nothrow();
		if (diff.exitCode === 0) {
			await $`rm -rf ${tmpSnap}`.quiet().nothrow();
			return;
		}
	}

	// Something changed (or first snapshot) — commit it
	await $`mv ${tmpSnap} ${dest}`.quiet();

	console.log("");
	console.log(`━━━ [${String(snapNum).padStart(3, "0")}] ${tag} ━━━`);

	// Capture coop agent state
	try {
		const state = await fetch(
			`http://localhost:${port}/api/v1/agent`,
		).then((r) => r.text());
		await Bun.write(join(agentDir, `${tag}.json`), state);
	} catch {
		await Bun.write(join(agentDir, `${tag}.json`), "{}");
	}

	// Capture terminal screen text
	try {
		const screen = await fetch(
			`http://localhost:${port}/api/v1/screen/text`,
		).then((r) => r.text());
		await Bun.write(join(screenDir, `${tag}.txt`), screen);
	} catch {
		// skip
	}

	// Show .claude.json if it exists
	const claudeJsonPath = join(dest, ".claude.json");
	if (await Bun.file(claudeJsonPath).exists()) {
		console.log("  .claude.json:");
		try {
			const content = await readFile(claudeJsonPath, "utf-8");
			const formatted = JSON.stringify(JSON.parse(content), null, 2);
			const lines = formatted.split("\n");
			for (const line of lines.slice(0, 30)) {
				console.log(`    ${line}`);
			}
			if (lines.length > 30) {
				console.log(`    … (${lines.length} lines total)`);
			}
		} catch {
			const content = await readFile(claudeJsonPath, "utf-8");
			console.log(`    ${content.slice(0, 500)}`);
		}
	}

	// Generate diff from previous snapshot
	if (prevTag) {
		const diffFile = join(diffDir, `${tag}.diff`);
		await $`diff -ruN --label a/${prevTag} ${join(snapDir, prevTag)} --label b/${tag} ${dest}`
			.quiet()
			.nothrow()
			.text()
			.then((text) => Bun.write(diffFile, text));

		const diffContent = await readFile(diffFile, "utf-8");
		console.log("");
		console.log(`  Changes from ${prevTag}:`);
		const changeLines = diffContent
			.split("\n")
			.filter((l) => /^(---|[+][+][+]|Only in)/.test(l))
			.filter(
				(l) => !l.startsWith("--- /dev/null") && !l.startsWith("+++ /dev/null"),
			)
			.map((l) =>
				l.replace(/^--- a\/[^/]*\//, "").replace(/^\+\+\+ b\/[^/]*\//, ""),
			)
			.slice(0, 15);
		for (const line of [...new Set(changeLines)]) {
			console.log(`    ${line}`);
		}

		if (diffContent.includes(".claude.json")) {
			console.log("");
			console.log("  .claude.json diff:");
			const jsonDiffLines = diffContent.split("\n");
			let inJson = false;
			let printed = 0;
			for (const line of jsonDiffLines) {
				if (/^diff.*\.claude\.json/.test(line)) inJson = true;
				if (inJson && /^diff [^.]/.test(line) && printed > 0) break;
				if (inJson) {
					console.log(`    ${line}`);
					printed++;
					if (printed >= 40) break;
				}
			}
		}
	} else {
		const files = await listFiles(dest);
		if (files.length > 0) {
			console.log("");
			console.log(`  Files (${files.length}):`);
			for (const f of files) {
				console.log(`    ./${f}`);
			}
		} else {
			console.log("  (empty config directory)");
		}
	}

	prevTag = tag;
	snapNum++;
}


const modeLabel = useDocker ? "docker" : "local";

console.log("");
console.log("╔═══════════════════════════════════════════════╗");
console.log(`║  State Capture: ${sessionName}`);
console.log(`║  Config: ${configMode.padEnd(37)}║`);
console.log(`║  Mode:   ${modeLabel.padEnd(37)}║`);
console.log(`║  Port:   ${String(port).padEnd(37)}║`);
console.log("╚═══════════════════════════════════════════════╝");
console.log("");
console.log(`  Workspace: ${workspace}`);
console.log(`  Output:    ${captureDir}`);
console.log("");

console.log("━━━ [000] initial ━━━");
await snapshot("initial");
console.log("");


let coopProc: ReturnType<typeof Bun.spawn> | null = null;
let containerId = "";

onExit(() => {
	if (containerId) {
		console.log("Stopping container…");
		Bun.spawnSync(["docker", "rm", "-f", containerId]);
	}
	if (coopProc) {
		coopProc.kill();
	}
	// Clean up workspace if it's in tmp
	if (workspace.startsWith(tmpdir()) || workspace.startsWith("/private/tmp/")) {
		Bun.spawnSync(["rm", "-rf", workspace]);
	}
	console.log("");
	console.log("═══════════════════════════════════════════");
	console.log(`  ${snapNum} snapshots captured`);
	console.log("");
	console.log(`  Snapshots: ${snapDir}`);
	console.log(`  Diffs:     ${diffDir}`);
	console.log(`  Screens:   ${screenDir}`);
	console.log(`  Agent:     ${agentDir}`);
	console.log("═══════════════════════════════════════════");
});

if (useDocker) {
	const imageTag = "coop:claude";
	if (!values["no-build"]) {
		await pullImage("claude", imageTag);
	}

	console.log(`Starting container on port ${port}…`);
	const dockerArgs = [
		"-d",
		"-p",
		`${port}:7070`,
		"-v",
		`${configDir}:/config`,
		"-v",
		`${workspace}:/workspace`,
		"-w",
		"/workspace",
		"-e",
		"CLAUDE_CONFIG_DIR=/config",
	];

	if (passAuth) {
		if (process.env.CLAUDE_CODE_OAUTH_TOKEN) {
			dockerArgs.push(
				"-e",
				`CLAUDE_CODE_OAUTH_TOKEN=${process.env.CLAUDE_CODE_OAUTH_TOKEN}`,
			);
		}
		if (process.env.ANTHROPIC_API_KEY) {
			dockerArgs.push(
				"-e",
				`ANTHROPIC_API_KEY=${process.env.ANTHROPIC_API_KEY}`,
			);
		}
	}

	const result =
		await $`docker run ${dockerArgs} ${imageTag} --port 7070 --log-format text --agent claude -- claude`
			.quiet()
			.nothrow()
			.text();
	containerId = result.trim();

	if (!containerId) {
		console.error("error: docker run failed");
		process.exit(1);
	}
	console.log(`Container: ${containerId.slice(0, 12)}`);
} else {
	// Local mode
	if (!values["no-build"]) {
		await buildCoop();
	}

	const bin = coopBin();
	if (!(await Bun.file(bin).exists())) {
		console.error(`error: ${bin} not found`);
		process.exit(1);
	}

	console.log(`Starting coop on port ${port}…`);
	coopProc = Bun.spawn(
		[
			bin,
			"--port",
			String(port),
			"--log-format",
			"text",
			"--agent",
			"claude",
			"--",
			"claude",
		],
		{
			stdout: "inherit",
			stderr: "inherit",
			cwd: workspace,
			env: {
				...process.env,
				COOP_AGENT: "claude",
				CLAUDE_CONFIG_DIR: configDir,
			},
		},
	);
}

await waitForHealth(port, {
	proc: coopProc ?? undefined,
	containerId: containerId || undefined,
});

if (!values["no-open"]) {
	await openBrowser(port);
}

console.log("");
console.log("Watching for config changes (Ctrl-C to stop)…");
console.log("");

while (true) {
	await Bun.sleep(1000);

	// Check if process/container is still alive
	if (containerId) {
		const ps = await $`docker ps -q --filter id=${containerId}`.quiet().text();
		if (!ps.trim()) break;
	} else if (coopProc && coopProc.exitCode !== null) {
		break;
	}

	// snapshot() diffs internally and returns early if nothing changed
	await snapshot("snap");
}
