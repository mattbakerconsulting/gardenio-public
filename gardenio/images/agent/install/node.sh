#!/bin/bash
# node.sh — install Node.js and Claude Code for gardenio agent image.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/versions.env"
source "$SCRIPT_DIR/checksums.env"
source "$SCRIPT_DIR/lib.sh"

ARCH=${TARGETARCH:-amd64}
case "$ARCH" in arm64) NODE_ARCH=arm64 ;; *) NODE_ARCH=x64 ;; esac

PREFIX=${INSTALL_PREFIX:-/usr/local}

# Pin to NODE_FULL_VERSION from versions.env and verify against the sha256
# published in nodejs.org/dist/v<ver>/SHASUMS256.txt. No more "whatever
# nodejs.org/dist/index.json lists first today" lookups.
case "$NODE_ARCH" in
  x64)   NODE_SHA="${NODE_SHA256_X64}" ;;
  arm64) NODE_SHA="${NODE_SHA256_ARM64}" ;;
  *) echo "node.sh: unsupported NODE_ARCH=$NODE_ARCH" >&2; exit 1 ;;
esac
fetch_verify_stdout \
  "https://nodejs.org/dist/v${NODE_FULL_VERSION}/node-v${NODE_FULL_VERSION}-linux-${NODE_ARCH}.tar.xz" \
  "${NODE_SHA}" \
  | tar -xJ --strip-components=1 -C "$PREFIX"

# Pin Claude Code version from versions.env. Auto-update is disabled at
# runtime (DISABLE_AUTOUPDATER=1 in pod env), so this is the only install.
# To update: change CLAUDE_CODE_VERSION in versions.env and the three sha512s in checksums.env.
PATH="$PREFIX/bin:$PATH" npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"

# Ensure the `claude` symlink exists. Recent npm versions create a
# temporary .claude-<hash> file during install and may fail to rename
# it to `claude`. If the symlink is missing, create it from the
# package's declared bin entry.
if [ ! -e "$PREFIX/bin/claude" ]; then
  CLI_JS="$PREFIX/lib/node_modules/@anthropic-ai/claude-code/cli.js"
  if [ -f "$CLI_JS" ]; then
    ln -sf "$CLI_JS" "$PREFIX/bin/claude"
    echo "claude: created symlink (npm failed to link)"
  else
    echo "WARNING: claude binary not found after npm install" >&2
  fi
fi

echo "Node.js v${NODE_FULL_VERSION} + Claude Code installed."
