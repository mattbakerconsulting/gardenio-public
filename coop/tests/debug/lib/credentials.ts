// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import { join } from "node:path";
import { $ } from "bun";
import { scriptDir } from "./setup";

/** Load key=value pairs from tests/debug/.env (if it exists). */
export async function loadEnvFile(): Promise<void> {
	const envPath = join(scriptDir(), ".env");
	const file = Bun.file(envPath);
	if (!(await file.exists())) return;

	const text = await file.text();
	for (const line of text.split("\n")) {
		const trimmed = line.replace(/#.*$/, "").trim();
		if (!trimmed) continue;
		const eq = trimmed.indexOf("=");
		if (eq < 0) continue;
		const key = trimmed.slice(0, eq).trim();
		const value = trimmed.slice(eq + 1).trim();
		if (key && !(key in process.env)) {
			process.env[key] = value;
		}
	}
}

/**
 * Resolved credential — either an OAuth token (Flow A) or API key (Flow B).
 */
export type Credential =
	| { type: "oauth_token"; token: string; refreshToken?: string; expiresAt?: number }
	| { type: "api_key"; key: string };

/**
 * Resolve credential via fallback chain:
 *
 * Flow A (OAuth token): env → Keychain → ~/.claude/.credentials.json
 * Flow B (API key):     env → ~/.claude/.claude.json primaryApiKey
 */
export async function resolveCredential(): Promise<Credential> {
	
	// 1a. Environment variable
	if (process.env.CLAUDE_CODE_OAUTH_TOKEN) {
		return { type: "oauth_token", token: process.env.CLAUDE_CODE_OAUTH_TOKEN };
	}

	// 2a. macOS Keychain
	try {
		const kcJson =
			await $`security find-generic-password -s "Claude Code-credentials" -w`
				.quiet()
				.text();
		const data = JSON.parse(kcJson);
		const oauth = data?.claudeAiOauth;
		if (oauth?.accessToken) return {
			type: "oauth_token",
			token: oauth.accessToken,
			refreshToken: oauth.refreshToken || undefined,
			expiresAt: oauth.expiresAt || undefined,
		};
	} catch {
		// not found or not macOS
	}

	// 3a. ~/.claude/.credentials.json
	const credPath = join(process.env.HOME ?? "", ".claude/.credentials.json");
	try {
		const data = await Bun.file(credPath).json();
		const oauth = data?.claudeAiOauth;
		if (oauth?.accessToken) return {
			type: "oauth_token",
			token: oauth.accessToken,
			refreshToken: oauth.refreshToken || undefined,
			expiresAt: oauth.expiresAt || undefined,
		};
	} catch {
		// file doesn't exist
	}

	
	// 1b. Environment variable
	if (process.env.ANTHROPIC_API_KEY) {
		return { type: "api_key", key: process.env.ANTHROPIC_API_KEY };
	}

	// 2b. ~/.claude/.claude.json primaryApiKey
	const claudePath = join(process.env.HOME ?? "", ".claude/.claude.json");
	try {
		const data = await Bun.file(claudePath).json();
		if (data?.primaryApiKey) {
			return { type: "api_key", key: data.primaryApiKey };
		}
	} catch {
		// file doesn't exist
	}

	throw new Error(
		"No credential found. Set CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY, add tests/debug/.env, or log in with 'claude'.",
	);
}

/**
 * Resolve oauthAccount JSON via fallback chain:
 * env var → ~/.claude/.claude.json
 */
export async function resolveOAuthAccount(): Promise<Record<string, unknown>> {
	// 1. Environment variable
	if (process.env.CLAUDE_OAUTH_ACCOUNT) {
		return JSON.parse(process.env.CLAUDE_OAUTH_ACCOUNT);
	}

	// 2. ~/.claude/.claude.json
	const claudePath = join(process.env.HOME ?? "", ".claude/.claude.json");
	try {
		const data = await Bun.file(claudePath).json();
		if (data?.oauthAccount) return data.oauthAccount;
	} catch {
		// file doesn't exist
	}

	throw new Error(
		"No oauthAccount found. Set CLAUDE_OAUTH_ACCOUNT in env or tests/debug/.env, or log in with 'claude'.",
	);
}

export async function writeCredentials(
	configDir: string,
	credential: Credential,
	account: Record<string, unknown>,
	baseJson: Record<string, unknown>,
): Promise<void> {
	// Detect lastOnboardingVersion from claude --version
	const onboardingVer = await detectOnboardingVersion();

	if (credential.type === "oauth_token") {
		// Flow A: write .credentials.json + .claude.json
		const credentials = {
			claudeAiOauth: {
				accessToken: credential.token,
				refreshToken: "",
				expiresAt: 9999999999999,
				scopes: [
					"user:inference",
					"user:profile",
					"user:sessions:claude_code",
				],
			},
		};
		await Bun.write(
			join(configDir, ".credentials.json"),
			JSON.stringify(credentials),
		);
		const claudeJson = {
			...baseJson,
			oauthAccount: account,
			lastOnboardingVersion: onboardingVer,
		};
		await Bun.write(
			join(configDir, ".claude.json"),
			JSON.stringify(claudeJson, null, 2),
		);
	} else {
		// Flow B: write primaryApiKey into .claude.json (no .credentials.json)
		const claudeJson = {
			...baseJson,
			oauthAccount: account,
			primaryApiKey: credential.key,
			lastOnboardingVersion: onboardingVer,
		};
		await Bun.write(
			join(configDir, ".claude.json"),
			JSON.stringify(claudeJson, null, 2),
		);
	}
}

export async function detectOnboardingVersion(): Promise<string> {
	try {
		const ver = await $`claude --version`.quiet().text();
		const match = ver.match(/(\d+\.\d+\.\d+)/);
		return match?.[1] ?? "0.0.0";
	} catch {
		return "0.0.0";
	}
}
