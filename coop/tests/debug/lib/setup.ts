// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import { dirname, join } from "node:path";
import { $ } from "bun";

/** Directory containing the calling script (tests/debug/) */
export function scriptDir(): string {
	return dirname(Bun.main);
}

/** Repository root (two levels up from tests/debug/) */
export function rootDir(): string {
	return dirname(dirname(scriptDir()));
}

/** Path to the debug coop binary */
export function coopBin(): string {
	return join(rootDir(), "target/debug/coop");
}

/** Path to the debug coopmux binary */
export function coopmuxBin(): string {
	return join(rootDir(), "target/debug/coopmux");
}

export async function buildWeb(): Promise<void> {
	console.log("Building web assets…");
	await $`bun run build`.cwd(join(rootDir(), "crates/web"));
}

export async function buildCoop(): Promise<void> {
	console.log("Building coop…");
	await $`cargo build -p coop --manifest-path ${rootDir()}/Cargo.toml`;
}

export async function buildMux(): Promise<void> {
	console.log("Building coopmux…");
	await $`cargo build -p coopmux --manifest-path ${rootDir()}/Cargo.toml`;
}

export async function buildAll(): Promise<void> {
	console.log("Building coop + coopmux…");
	await $`cargo build -p coop -p coopmux --manifest-path ${rootDir()}/Cargo.toml`;
}

export async function pullImage(variant: string, tag: string): Promise<void> {
	console.log(`Pulling ${tag} from GHCR…`);
	// coop's per-variant OCI images (empty/claude/gemini/coopmux/claudeless) were
	// published by the decommissioned upstream coop pipeline and are not yet
	// built in this monorepo. Fail loudly rather than pull a dead registry.
	throw new Error(`coop debug image for variant ${variant} is not built in this repo yet; see .github/workflows/images.yml`);
}

interface HealthCheckOpts {
	/** Native process to check liveness */
	proc?: { readonly exitCode: number | null; kill(): void };
	/** Docker container ID to check liveness */
	containerId?: string;
	/** Max attempts (default 50) */
	maxAttempts?: number;
	/** Delay between attempts in ms (default 200) */
	delayMs?: number;
}

export async function waitForHealth(
	port: number,
	opts: HealthCheckOpts = {},
): Promise<void> {
	const { maxAttempts = 50, delayMs = 200 } = opts;
	process.stdout.write("Waiting for coop to be ready");

	for (let i = 0; i < maxAttempts; i++) {
		try {
			const res = await fetch(`http://localhost:${port}/api/v1/health`);
			if (res.ok) {
				console.log(" ok");
				return;
			}
		} catch {
			// not ready yet
		}

		// Check if the underlying process/container is still alive
		if (opts.proc && opts.proc.exitCode !== null) {
			console.log(" failed (process exited)");
			process.exit(1);
		}
		if (opts.containerId) {
			const ps = await $`docker ps -q --filter id=${opts.containerId}`
				.quiet()
				.text();
			if (!ps.trim()) {
				console.log(" failed (container exited)");
				await $`docker logs ${opts.containerId}`.quiet().nothrow();
				process.exit(1);
			}
		}

		process.stdout.write(".");
		await Bun.sleep(delayMs);
	}

	// Final check
	try {
		const res = await fetch(`http://localhost:${port}/api/v1/health`);
		if (res.ok) {
			console.log(" ok");
			return;
		}
	} catch {
		// fall through
	}
	console.log(" timed out");
	process.exit(1);
}

export async function openBrowser(port: number): Promise<void> {
	await openBrowserUrl(`http://localhost:${port}/`);
}

export async function openBrowserUrl(url: string): Promise<void> {
	console.log(`Opening ${url}`);

	try {
		// macOS
		await $`open ${url}`.quiet();
	} catch {
		try {
			// Linux
			await $`xdg-open ${url}`.quiet();
		} catch {
			console.log(`Open manually: ${url}`);
		}
	}
}

type CleanupFn = () => void | Promise<void>;
const cleanupFns: CleanupFn[] = [];
let cleanupRegistered = false;

export function onExit(fn: CleanupFn): void {
	cleanupFns.push(fn);
	if (!cleanupRegistered) {
		cleanupRegistered = true;
		const run = () => {
			for (const f of cleanupFns) {
				try {
					f();
				} catch {
					// best-effort
				}
			}
			process.exit();
		};
		process.on("SIGINT", run);
		process.on("SIGTERM", run);
		process.on("exit", () => {
			for (const f of cleanupFns) {
				try {
					f();
				} catch {
					// best-effort
				}
			}
		});
	}
}

/** Check if a port is available by trying to fetch from it. */
async function isPortAvailable(port: number): Promise<boolean> {
	try {
		await fetch(`http://localhost:${port}/`, {
			signal: AbortSignal.timeout(200),
		});
		return false; // something responded
	} catch {
		return true; // nothing listening
	}
}

/** Find an available port starting from the given port, trying up to maxAttempts consecutive ports. */
export async function findAvailablePort(
	startPort: number,
	maxAttempts = 10,
): Promise<number> {
	for (let i = 0; i < maxAttempts; i++) {
		const port = startPort + i;
		if (await isPortAvailable(port)) {
			return port;
		}
		console.log(`Port ${port} is in use, trying ${port + 1}…`);
	}
	console.error(
		`No available port found in range ${startPort}–${startPort + maxAttempts - 1}`,
	);
	process.exit(1);
}

export type Profile = "empty" | "authorized" | "trusted";

export interface ProfileOpts {
	configDir: string;
	workspace: string;
}
