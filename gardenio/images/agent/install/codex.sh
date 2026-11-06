#!/bin/bash
# codex.sh — install the OpenAI Codex CLI for the gardenio agent image.
#
# Codex is the second agent harness (plan gd-8s8FdZu8UA): a role whose
# `harness` field is `codex` runs `codex exec` where a Claude role runs
# `claude`. This script only puts the binary in the image — nothing here
# configures it. Per-agent configuration ($CODEX_HOME/config.toml, AGENTS.md,
# hooks.json) is written at launch by `gb init` in Phase 2 (gd-kmduHGcH5z).
#
# Upstream ships static musl binaries, so unlike Claude Code there is no Node
# dependency: this script must NOT be made to depend on node.sh.
#
# It also installs ripgrep, which Codex needs as a real executable — see the
# comment on that block for why the image did not already have one.
#
# Every download is SHA256-verified against checksums.env. See clis.sh's header
# for the supply-chain rules this follows (ka-0000002782).

# shellcheck source-path=SCRIPTDIR
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/versions.env"
source "$SCRIPT_DIR/checksums.env"
source "$SCRIPT_DIR/lib.sh"

ARCH=${TARGETARCH:-amd64}
case "$ARCH" in arm64) ARCH_ALT=aarch64 ;; *) ARCH_ALT=x86_64 ;; esac

PREFIX=${INSTALL_PREFIX:-/usr/local}
mkdir -p "$PREFIX/bin"

# The release asset is a .tar.gz holding ONE bare executable named after the
# target triple (not a directory, and not `codex`) — verified against
# rust-v0.162.1 on both arches. Extract that member to stdout and land it as
# `codex`. tar is last in the pipeline, so `set -e` catches a short or empty
# body: fetch_verify_stdout emits nothing unless the sha256 matched, and tar
# then fails on unexpected EOF rather than writing a truncated binary.
TRIPLE="${ARCH_ALT}-unknown-linux-musl"
fetch_verify_stdout \
  "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-${TRIPLE}.tar.gz" \
  "$(pick_sha_alt CODEX_SHA256)" \
  | tar -xzO "codex-${TRIPLE}" > "$PREFIX/bin/codex"
chmod 0755 "$PREFIX/bin/codex"

# Smoke test. This is the ONLY automated check that this install works: the
# `agent` image is release-only and is not built on pull requests (owner
# ruling gd-9mtwbE3hoK), so a broken pin surfaces when a tag is cut unless it
# fails here. Assert the version too, which catches a CODEX_VERSION bumped
# without its CODEX_SHA256_* (and vice versa) rather than just "a binary ran".
actual_version="$("$PREFIX/bin/codex" --version)"
case "$actual_version" in
  *"${CODEX_VERSION}"*) ;;
  *)
    echo "codex.sh: version mismatch after install" >&2
    echo "  expected CODEX_VERSION=${CODEX_VERSION}" >&2
    echo "  got:      ${actual_version}" >&2
    exit 1
    ;;
esac

# ── codex-code-mode-host ──────────────────────────────────────────────
# Codex execs /usr/local/bin/codex-code-mode-host for every tool call. Without
# it each call fails ("codex-code-mode-host is missing. No commands ran"), so
# the agent reaches the model but can run no shell (gd-QpUS8SPrhc). Upstream
# publishes it as its own asset next to codex, same bare-member tarball shape.
# It has no --version flag, so the smoke test is "non-empty and executable".
fetch_verify_stdout \
  "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-code-mode-host-${TRIPLE}.tar.gz" \
  "$(pick_sha_alt CODEX_HOST_SHA256)" \
  | tar -xzO "codex-code-mode-host-${TRIPLE}" > "$PREFIX/bin/codex-code-mode-host"
chmod 0755 "$PREFIX/bin/codex-code-mode-host"
[ -s "$PREFIX/bin/codex-code-mode-host" ] || {
  echo "codex.sh: codex-code-mode-host is empty after install" >&2
  exit 1
}

# ── ripgrep ───────────────────────────────────────────────────────────
# Codex shells out to `rg` for search. The image appeared to have it already,
# but `rg` there is a bash FUNCTION from Claude Code's shell profile wrapping
# its vendored copy — `env rg` fails with ENOENT and there is no rg executable
# anywhere in the image. Codex execve's rg directly, so that function is
# invisible to it and `codex doctor` reports "search command could not be
# verified". Measured in an agent pod on 2026-10-14; see gd-HaGrzg53TS.
#
# Installed here rather than in clis.sh because Codex is the only thing in this
# image that needs rg as a real binary (Claude Code brings its own). If a
# second consumer ever appears, move it to clis.sh.
#
# Unlike the Codex asset, these tarballs wrap the binary in a versioned
# DIRECTORY, so this one strips a component.
RG_DIR="ripgrep-${RIPGREP_VERSION}-${ARCH_ALT}-unknown-linux-musl"
fetch_verify_stdout \
  "https://github.com/BurntSushi/ripgrep/releases/download/${RIPGREP_VERSION}/${RG_DIR}.tar.gz" \
  "$(pick_sha_alt RIPGREP_SHA256)" \
  | tar -xzO "${RG_DIR}/rg" > "$PREFIX/bin/rg"
chmod 0755 "$PREFIX/bin/rg"

rg_version="$("$PREFIX/bin/rg" --version | head -1)"
case "$rg_version" in
  *"${RIPGREP_VERSION}"*) ;;
  *)
    echo "codex.sh: ripgrep version mismatch after install" >&2
    echo "  expected RIPGREP_VERSION=${RIPGREP_VERSION}" >&2
    echo "  got:      ${rg_version}" >&2
    exit 1
    ;;
esac

# NB: no self-update guard is baked here, deliberately.
#
# Codex has no DISABLE_AUTOUPDATER-style env var (the knob is the config key
# `check_for_update_on_startup`, plus an admin `/etc/codex/managed_config.toml`
# whose precedence over $CODEX_HOME/config.toml is not yet established). Two
# reasons to leave it alone at image level: gardenio already disables Claude's
# updater in the POD ENV, not the image (podmanager/manager.go sets
# DISABLE_AUTOUPDATER=1), so the runtime layer is the consistent home for it;
# and baking an admin policy file risks silently overriding the per-agent
# model/provider config that Phase 2 exists to control. `codex update` cannot
# succeed in a pod regardless — this binary is root-owned under /usr/local/bin
# and agents run as UID 1000. The runtime decision lands with the Codex config
# writer (gd-kmduHGcH5z); the managed-config precedence question is recorded
# on the ADR step (gd-IpXL2HevnY).
#
# bwrap is also not installed. The same release publishes a small
# bwrap-${TRIPLE}.tar.gz, but agent pods are unprivileged and cannot create
# user namespaces, so Codex runs with sandbox_mode=danger-full-access; a bwrap
# binary would be dead weight. The spike (gd-JegnDXa3nE Q1) measures this.

echo "${actual_version} + ${rg_version} installed ($TRIPLE)."
