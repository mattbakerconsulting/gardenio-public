# RWX Directory Guide

## WARNING: RWX reads ALL .yml files in .rwx/ directories

RWX automatically picks up every `.yml` file in `.rwx/` as a workflow
definition. **Do NOT store backup, draft, or old workflow files here** —
they will trigger duplicate runs on every push.

If you need to keep old versions for reference, move them outside `.rwx/`
(e.g. `docs/rwx-archive/`) or use git history instead.

## Lock File Overview

RWX tasks use `filter` directives to decide when cached results are valid.
Lock files are included in these filters as manual cache-bust triggers —
bumping `cache-epoch` in a lock file invalidates the task's cache and
forces a rebuild on the next run.

**How to bust a cache:**
```bash
# Increment cache-epoch in the relevant lock file
vim gardenio/.rwx/agent-node.lock  # bump cache-epoch=3 → cache-epoch=4
git add gardenio/.rwx/agent-node.lock
git commit -m "chore: bust agent node cache"
```

## Lock Files by Location

### Monorepo CI locks (`.rwx/`)

Used by `.rwx/ci.yml`, `.rwx/e2e.yml`, `.rwx/helm.yml`:

| File | Controls | When to bump |
|---|---|---|
| `e2e-system-deps.lock` | E2E task system deps (curl, jq, aws-cli, kubectl) | When system tool versions need updating |
| `helm-version.lock` | Helm CLI installation | When Helm version needs updating |
| `coop-precompiled.epoch` | Coop dependency precompilation cache (see below) | When Cargo.lock changes don't trigger a rebuild, or after Rust toolchain updates |

### Agent image locks (`gardenio/.rwx/`)

Used by `.rwx/docker.yml` agent image build tasks:

| File | RWX Task | What it caches | When to bump |
|---|---|---|---|
| `agent-syspackages.lock` | `agent-install-syspackages` | apt packages (git, gcc, cmake, ffmpeg, etc.) | After changing `BASE_PACKAGES` or `AGENT_PACKAGES` in `versions.env` |
| `agent-node.lock` | `agent-install-node` | Node.js + Claude Code | After changing Node major version or Claude Code version in `install/node.sh` |
| `agent-playwright.lock` | `agent-install-playwright` | Playwright + Chromium + Chrome | After changing Playwright version, adding browser deps, or fixing font/rendering issues |
| `agent-go.lock` | `agent-install-go` | Go toolchain + gopls + golangci-lint + Task CLI | After changing Go version or tool versions in `install/go-tools.sh` |
| `agent-rust.lock` | `agent-install-rust` | Rust toolchain + rust-analyzer + quench | After changing Rust version in `install/rust-tools.sh` |
| `agent-clis.lock` | `agent-install-clis` | CLI tools (kubectl, gh, glab, docker, aws, helm, terraform, etc.) | After changing any tool version in `install/clis.sh` |
| `agent-channel.lock` | `agent-install-channel` | Gardenio channel MCP server | After changing channel server source |
| `agent-versions.lock` | *(deprecated)* | Was the central version file; now each lock file is self-contained | Do not use — bump individual lock files instead |

### Legacy locks (`gardenio/.rwx/`)

| File | Status | Notes |
|---|---|---|
| `cache-config.lock` | Semi-deprecated | Contains version references for Go, coop, beads, golangci-lint. Some docker.yml tasks still reference it. Prefer updating `versions.env` + individual lock files. |
| `system-deps.lock` | Active | CI system deps cache (curl, unzip, apt packages) |

## Coop Precompiled Cache (`coop-precompiled.epoch`)

The `coop-precompiled` task in `.rwx/docker.yml` implements a stub-based
dependency precompilation strategy for Rust. This is the most complex
caching mechanism in the pipeline:

### How it works

1. **Stub out workspace crates**: Replace all workspace `.rs` sources with
   empty stubs (`fn main(){}` / empty lib.rs). This ensures only external
   dependencies are compiled.

2. **Build with stubs**: `cargo build --release` compiles all deps from
   `Cargo.lock` into rlibs/dylibs in the target directory.

3. **Archive deps layer**: Package `deps/`, `build/`, and `.fingerprint/`
   directories into `coop-precompiled.tar.gz` (~200-400 MB compressed vs
   ~3 GB full target).

4. **Downstream usage**: `build-coop` extracts the precompiled archive,
   then runs `cargo build` which only needs to compile the 2 workspace
   crates (fast — seconds instead of minutes).

### Cache key

The `coop-precompiled` task's filter includes:
- `coop/Cargo.lock` — dependency version changes
- `coop/**/Cargo.toml` — workspace structure changes
- `coop/proto/**` — protobuf changes (affects build scripts)
- `coop/rust-toolchain.toml` — Rust version changes
- `.rwx/coop-precompiled.epoch` — manual cache bust

### When to bump the epoch

- Rust toolchain version change (fingerprint hash mismatch)
- Suspected cache corruption (build-coop fails with "can't find crate")
- After changing build.rs scripts that affect dependency compilation

### Critical: Rust version pinning

The `rust` task in docker.yml pins to `1.93.0` (matching `coop/rust-toolchain.toml`).
This is essential — every Rust release changes the `rustc` binary hash, which
invalidates all Cargo fingerprints. Without pinning, the precompiled cache
would be rebuilt on every Rust stable release.

## Version Sources of Truth

| What | Source of truth | Lock file to bump |
|---|---|---|
| Agent apt packages | `gardenio/images/agent/install/versions.env` | `agent-syspackages.lock` |
| Agent CLI tools | `gardenio/images/agent/install/clis.sh` | `agent-clis.lock` |
| Agent Node.js | `gardenio/images/agent/install/node.sh` | `agent-node.lock` |
| Agent Go tools | `gardenio/images/agent/install/go-tools.sh` | `agent-go.lock` |
| Agent Rust tools | `gardenio/images/agent/install/rust-tools.sh` | `agent-rust.lock` |
| Agent Playwright | `gardenio/images/agent/install/playwright.sh` | `agent-playwright.lock` |
| CI Go version | `.rwx/ci.yml` (`go-version`) | N/A (auto from filter) |
| CI golangci-lint | `.rwx/ci.yml` (install script) | N/A (auto from filter) |
| CI Rust version | `coop/rust-toolchain.toml` | `coop-precompiled.epoch` if toolchain changes |
| Helm version | Latest Helm 3 (auto) | `helm-version.lock` |
