#!/bin/bash
# gemini.sh — install the Google Gemini CLI for the gardenio agent image.
#
# Gemini is the third agent harness (ADR 0005, plan gd-8s8FdZu8UA): a role whose
# `harness` is `gemini` will run `gemini` where a Claude role runs `claude`.
# This script only puts the binary in the image — nothing here configures it.
# Per-agent configuration (~/.gemini/settings.json, GEMINI.md) is a later phase
# (`gb setup gemini`, ADR 0005 Phase 2).
#
# Unlike codex.sh this one DOES need Node: upstream ships the CLI as a single
# platform-independent JavaScript bundle (gemini-cli-bundle.zip: gemini.js plus
# its chunk files, ~96MB unpacked, Node >=20) and publishes no native Linux
# binary. So it must run after node.sh. The bundle is one asset for both
# arches, hence one GEMINI_SHA256 rather than an _X86_64/_AARCH64 pair. The
# optional native deps (node-pty, keytar) are not in the bundle and the CLI
# does not need them headless (verified in the phase 0 spike).
#
# Every download is SHA256-verified against checksums.env. See clis.sh's header
# for the supply-chain rules this follows (ka-0000002782). Provenance is weaker
# than we would like: upstream publishes no checksum file and no SLSA/sigstore
# attestation for the release (spike.md R9), so the pin is the GitHub release
# asset `digest`, re-hashed from a fresh download and cross-checked against the
# npm tarball contents.

# shellcheck source-path=SCRIPTDIR
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/versions.env"
source "$SCRIPT_DIR/checksums.env"
source "$SCRIPT_DIR/lib.sh"

PREFIX=${INSTALL_PREFIX:-/usr/local}
LIBDIR="$PREFIX/lib/gemini-cli"
mkdir -p "$PREFIX/bin" "$PREFIX/lib"

# Node is a hard dependency; fail here with a clear message rather than at the
# first `gemini` invocation in a pod.
command -v node > /dev/null || {
  echo "gemini.sh: node is not on PATH; run node.sh first" >&2
  exit 1
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# fetch_verify aborts (and removes the file) on a digest mismatch, so unzip
# below never sees unverified bytes.
fetch_verify \
  "https://github.com/google-gemini/gemini-cli/releases/download/v${GEMINI_VERSION}/gemini-cli-bundle.zip" \
  "$GEMINI_SHA256" \
  "$TMP/gemini-cli-bundle.zip"

rm -rf "$LIBDIR"
mkdir -p "$LIBDIR"
# python3, not unzip: this runs in the `base` stage, whose BASE_PACKAGES has
# python3 but not unzip (that arrives later via AGENT_PACKAGES).
python3 -m zipfile -e "$TMP/gemini-cli-bundle.zip" "$LIBDIR"
[ -s "$LIBDIR/gemini.js" ] || {
  echo "gemini.sh: gemini.js missing from the bundle (asset layout changed?)" >&2
  rm -rf "$LIBDIR"
  exit 1
}
chmod 0755 "$LIBDIR/gemini.js"
ln -sf "$LIBDIR/gemini.js" "$PREFIX/bin/gemini"

# Smoke test. The `agent` image is release-only and is not built on pull
# requests (owner ruling gd-9mtwbE3hoK), so this is the only automated check
# that the pin works. Asserting the version catches a GEMINI_VERSION bumped
# without GEMINI_SHA256 (and vice versa), not just "a binary ran".
actual_version="$("$PREFIX/bin/gemini" --version)"
case "$actual_version" in
  *"${GEMINI_VERSION}"*) ;;
  *)
    echo "gemini.sh: version mismatch after install" >&2
    echo "  expected GEMINI_VERSION=${GEMINI_VERSION}" >&2
    echo "  got:      ${actual_version}" >&2
    exit 1
    ;;
esac

# NB: no auto-update / telemetry / trust policy is baked here, deliberately, for
# the reasons codex.sh gives: the runtime layer (pod env + `gb setup gemini`)
# is the consistent home for it, and the CLI cannot update itself anyway (the
# bundle is root-owned under /usr/local/lib and agents run as UID 1000).
# What the runtime layer MUST set is recorded in docs/research/gemini-harness/
# spike.md R7: GEMINI_CLI_TRUST_WORKSPACE=true (else headless exits 55),
# GEMINI_CLI_NO_RELAUNCH=true, and security.auth.selectedType=gemini-api-key in
# the user settings file (else exit 41 "Invalid auth method").

echo "gemini ${actual_version} installed ($LIBDIR)."
