#!/bin/bash
# lib.sh — shared helpers for agent image install scripts.
#
# Provides SHA256-verified download primitives. Supply-chain hardening per
# ka-0000002782 / ka-0000000248 (AUDIT.md §H3).
#
# Usage: `source "$SCRIPT_DIR/lib.sh"` after sourcing versions.env and
# checksums.env.

# CURL_RETRY — retry transient network failures (a reset TLS handshake, a 5xx
# from a release CDN) before failing the download. Without it one blip lost
# helm from a release image (get.helm.sh SSL_ERROR_SYSCALL, gd-POfKpGi6Kj). The
# sha256 check still runs on whatever finally arrives.
CURL_RETRY=(--retry 5 --retry-delay 2 --retry-all-errors)

# fetch_verify URL SHA256 DEST
#   Downloads URL to DEST and aborts if its sha256 does not match SHA256.
fetch_verify() {
  local url="$1" sum="$2" dest="$3"
  if [ -z "$url" ] || [ -z "$sum" ] || [ -z "$dest" ]; then
    echo "fetch_verify: usage: fetch_verify URL SHA256 DEST" >&2
    return 2
  fi
  curl -fsSL "${CURL_RETRY[@]}" "$url" -o "$dest"
  if ! echo "${sum}  ${dest}" | sha256sum -c --status -; then
    local actual
    actual=$(sha256sum "$dest" | awk '{print $1}')
    echo "fetch_verify: checksum mismatch for $url" >&2
    echo "  expected: $sum" >&2
    echo "  actual:   $actual" >&2
    rm -f "$dest"
    return 1
  fi
}

# fetch_verify_stdout URL SHA256
#   Downloads URL, verifies sha256, then writes bytes to stdout (for piping
#   into tar / unzip). Uses a temp file so the stream can be hashed before
#   being consumed.
fetch_verify_stdout() {
  local url="$1" sum="$2"
  if [ -z "$url" ] || [ -z "$sum" ]; then
    echo "fetch_verify_stdout: usage: fetch_verify_stdout URL SHA256" >&2
    return 2
  fi
  local tmp
  tmp=$(mktemp)
  if ! curl -fsSL "${CURL_RETRY[@]}" "$url" -o "$tmp"; then
    rm -f "$tmp"
    return 1
  fi
  if ! echo "${sum}  ${tmp}" | sha256sum -c --status -; then
    local actual
    actual=$(sha256sum "$tmp" | awk '{print $1}')
    echo "fetch_verify_stdout: checksum mismatch for $url" >&2
    echo "  expected: $sum" >&2
    echo "  actual:   $actual" >&2
    rm -f "$tmp"
    return 1
  fi
  cat "$tmp"
  rm -f "$tmp"
}

# pick_sha VAR_PREFIX
#   Given a prefix and the current $ARCH (amd64|arm64), print the sha256 from
#   the env var ${VAR_PREFIX}_AMD64 / ${VAR_PREFIX}_ARM64. Callers choose the
#   prefix based on whichever naming convention the upstream uses (some
#   binaries are published as x86_64/aarch64 — use a matching prefix like
#   "FOO_SHA256" with _X86_64/_AARCH64 pairs in checksums.env).
pick_sha() {
  local prefix="$1" arch="${ARCH:-amd64}" var val
  case "$arch" in
    amd64) var="${prefix}_AMD64" ;;
    arm64) var="${prefix}_ARM64" ;;
    *) echo "pick_sha: unsupported arch=$arch" >&2; return 2 ;;
  esac
  val="${!var:-}"
  if [ -z "$val" ]; then
    echo "pick_sha: ${var} not set in checksums.env" >&2
    return 1
  fi
  echo "$val"
}

# pick_sha_alt — like pick_sha but uses _X86_64/_AARCH64 suffixes (matches
# the naming convention of docker, aws, claudeless, hadolint).
pick_sha_alt() {
  local prefix="$1" arch="${ARCH:-amd64}" var val
  case "$arch" in
    amd64) var="${prefix}_X86_64" ;;
    arm64) var="${prefix}_AARCH64" ;;
    *) echo "pick_sha_alt: unsupported arch=$arch" >&2; return 2 ;;
  esac
  val="${!var:-}"
  if [ -z "$val" ]; then
    echo "pick_sha_alt: ${var} not set in checksums.env" >&2
    return 1
  fi
  echo "$val"
}
