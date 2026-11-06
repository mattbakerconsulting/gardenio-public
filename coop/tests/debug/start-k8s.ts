#!/usr/bin/env bun
// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// Debug helper: deploy coopmux + coop sessions in a local Kubernetes cluster.
//
// Usage:
//   bun tests/debug/start-k8s.ts                  # auto-detect kind/k3d
//   bun tests/debug/start-k8s.ts --tool kind       # force kind
//   bun tests/debug/start-k8s.ts --tool k3d        # force k3d
//   bun tests/debug/start-k8s.ts --no-build        # skip image builds
//   bun tests/debug/start-k8s.ts --port 9900       # custom local port

import { parseArgs } from "node:util";
import { $ } from "bun";
import {
	buildCoop,
	coopBin,
	findAvailablePort,
	onExit,
	openBrowserUrl,
	pullImage,
	rootDir,
	waitForHealth,
} from "./lib/setup";
import { loadEnvFile, resolveCredential } from "./lib/credentials";

const CLUSTER_NAME = "coop-dev";
const NAMESPACE = "coop";
const MANIFEST_PATH = `${rootDir()}/deploy/k8s-mux.yaml`;

const { values } = parseArgs({
	args: Bun.argv.slice(2),
	options: {
		tool: { type: "string" },
		port: { type: "string", default: "9810" },
		"no-build": { type: "boolean", default: false },
		"no-open": { type: "boolean", default: false },
	},
	strict: true,
});

// -- Detect cluster tool ------------------------------------------------------

type ClusterTool = "kind" | "k3d";

async function hasCommand(cmd: string): Promise<boolean> {
	try {
		await $`which ${cmd}`.quiet();
		return true;
	} catch {
		return false;
	}
}

async function detectTool(): Promise<ClusterTool> {
	if (values.tool) {
		if (values.tool !== "kind" && values.tool !== "k3d") {
			console.error(`Unknown tool: ${values.tool} (expected 'kind' or 'k3d')`);
			process.exit(1);
		}
		if (!(await hasCommand(values.tool))) {
			console.error(`${values.tool} is not installed`);
			process.exit(1);
		}
		return values.tool;
	}

	if (await hasCommand("kind")) return "kind";
	if (await hasCommand("k3d")) return "k3d";

	console.error(
		"Neither 'kind' nor 'k3d' found. Install one:\n" +
			"  brew install kind     # https://kind.sigs.k8s.io\n" +
			"  brew install k3d      # https://k3d.io",
	);
	process.exit(1);
}

const tool = await detectTool();
console.log(`Using ${tool} for local Kubernetes cluster`);

// -- Cluster lifecycle --------------------------------------------------------

async function clusterExists(): Promise<boolean> {
	try {
		if (tool === "kind") {
			const out = await $`kind get clusters`.quiet().text();
			return out.split("\n").some((l) => l.trim() === CLUSTER_NAME);
		}
		const out = await $`k3d cluster list -o json`.quiet().text();
		const clusters = JSON.parse(out);
		return clusters.some(
			(c: { name: string }) => c.name === CLUSTER_NAME,
		);
	} catch {
		return false;
	}
}

async function createCluster(): Promise<void> {
	if (await clusterExists()) {
		console.log(`Cluster '${CLUSTER_NAME}' already exists, reusing it`);
		return;
	}
	console.log(`Creating ${tool} cluster '${CLUSTER_NAME}'…`);
	if (tool === "kind") {
		await $`kind create cluster --name ${CLUSTER_NAME}`;
	} else {
		await $`k3d cluster create ${CLUSTER_NAME}`;
	}
}

async function deleteCluster(): Promise<void> {
	console.log(`Deleting ${tool} cluster '${CLUSTER_NAME}'…`);
	if (tool === "kind") {
		Bun.spawnSync(["kind", "delete", "cluster", "--name", CLUSTER_NAME]);
	} else {
		Bun.spawnSync(["k3d", "cluster", "delete", CLUSTER_NAME]);
	}
}

async function loadImage(tag: string): Promise<void> {
	console.log(`Loading image ${tag} into cluster…`);
	if (tool === "kind") {
		await $`kind load docker-image ${tag} --name ${CLUSTER_NAME}`;
	} else {
		await $`k3d image import ${tag} --cluster ${CLUSTER_NAME}`;
	}
}

// -- Build & load images ------------------------------------------------------

if (!values["no-build"]) {
	await buildCoop();
	await pullImage("coopmux", "coop:coopmux");
	await pullImage("claude", "coop:claude");
}

// -- Create cluster & deploy --------------------------------------------------

await createCluster();
onExit(() => deleteCluster());

await loadImage("coop:coopmux");
await loadImage("coop:claude");

console.log("Applying Kubernetes manifests…");
await $`kubectl apply -f ${MANIFEST_PATH}`;

console.log("Waiting for coopmux rollout…");
await $`kubectl rollout status deployment/coopmux -n ${NAMESPACE} --timeout=120s`;

// -- Port forward & health check ----------------------------------------------

const port = await findAvailablePort(Number(values.port));
console.log(`Port-forwarding localhost:${port} → coopmux:9800`);

const pfProc = Bun.spawn(
	[
		"kubectl",
		"port-forward",
		`svc/coopmux`,
		"-n",
		NAMESPACE,
		`${port}:9800`,
	],
	{ stdout: "pipe", stderr: "pipe" },
);
onExit(() => pfProc.kill());

// Give port-forward a moment to bind
await Bun.sleep(1000);

await waitForHealth(port, { maxAttempts: 30, delayMs: 500 });

// -- Auto-seed local credential -----------------------------------------------

await loadEnvFile();
try {
	const cred = await resolveCredential();
	const envKey =
		cred.type === "oauth_token"
			? "CLAUDE_CODE_OAUTH_TOKEN"
			: "ANTHROPIC_API_KEY";
	const token = cred.type === "oauth_token" ? cred.token : cred.key;
	const muxUrl = `http://localhost:${port}`;

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

// -- Check for credentials secret ---------------------------------------------

try {
	await $`kubectl get secret anthropic-credentials -n ${NAMESPACE}`.quiet();
} catch {
	console.log(
		"\nNote: No 'anthropic-credentials' secret found in the coop namespace.",
	);
	console.log("Credentials can be provided via the mux Credential Panel or a k8s secret:");
	console.log(
		`  kubectl create secret generic anthropic-credentials -n ${NAMESPACE} \\`,
	);
	console.log(
		'    --from-literal=api-key="$ANTHROPIC_API_KEY"',
	);
	console.log("");
}

// -- Open browser -------------------------------------------------------------

const muxUrl = `http://localhost:${port}`;

if (!values["no-open"]) {
	await openBrowserUrl(`${muxUrl}/mux`);
}

console.log(`\nMux dashboard: ${muxUrl}/mux`);
console.log(`Cluster: ${CLUSTER_NAME} (${tool})`);
console.log("Click 'Launch' in the dashboard to create session pods.");
console.log("Press Ctrl+C to tear down the cluster.\n");

// -- Tail coopmux logs until exit ---------------------------------------------

await $`kubectl logs -n ${NAMESPACE} -l app=coopmux -f --tail=100`.nothrow();
