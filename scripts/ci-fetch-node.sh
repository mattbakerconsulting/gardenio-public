#!/usr/bin/env bash
# ci-fetch-node.sh EXT DEST -- fetch the pinned Node tarball for CI.
#
# EXT is tar.gz or tar.xz. The version and sha256 come from the same two
# files the agent image installs from (images/agent/install/node.sh), so CI
# and the image cannot drift apart. The download itself, the sha256 check
# and the node-local cache are ci-fetch-verified.sh's.
#
# Mirrors (gd-zEF9GEEgkp): nodejs.org has served under 10 KB/s for long
# enough to fail ci, so attempts rotate onto npmmirror (registry.npmmirror.com,
# Alibaba's CDN), which re-serves nodejs.org/dist unchanged -- checked
# 2026-10-28: both v24.15.0 linux-x64 tarballs match NODE_SHA256_X64 and
# NODE_SHA256_X64_TARGZ byte for byte. Only add a mirror that serves the
# same bytes; the one sha256 is what makes any source safe to use.
set -euo pipefail

ext=$1 dest=$2
here=$(cd "$(dirname "$0")" && pwd)
install="$here/../gardenio/images/agent/install"
# shellcheck source=gardenio/images/agent/install/versions.env
. "$install/versions.env"
# shellcheck source=gardenio/images/agent/install/checksums.env
. "$install/checksums.env"

case "$ext" in
  tar.gz) sha=$NODE_SHA256_X64_TARGZ ;;
  tar.xz) sha=$NODE_SHA256_X64 ;;
  *) echo "ci-fetch-node: EXT must be tar.gz or tar.xz, got '$ext'" >&2; exit 2 ;;
esac

file="node-v${NODE_FULL_VERSION}-linux-x64.$ext"
exec "$here/ci-fetch-verified.sh" \
  "https://nodejs.org/dist/v${NODE_FULL_VERSION}/$file" "$sha" "$dest" \
  "https://registry.npmmirror.com/-/binary/node/v${NODE_FULL_VERSION}/$file"
