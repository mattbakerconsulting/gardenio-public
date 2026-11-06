#!/usr/bin/env bash
# smoke_test.sh — hermetic tests for smoke.sh (gd-hZl1cKiELM).
#
# The `agent` image is release-only (images.yml drops it on pull_request), so
# smoke.sh first runs for real while a release tag is cut. These tests run it
# on a PR against stub binaries that print the real upstream --version formats
# at the pinned versions, so a version-string or TMPDIR-check regression reds a
# PR instead of a cut. No network, no Docker; well under a second.
#
# Usage:  bash gardenio/images/agent/smoke_test.sh

# shellcheck source-path=SCRIPTDIR
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VERSIONS_ENV="${SCRIPT_DIR}/install/versions.env"

PASS=0
FAIL=0
TMPROOT="$(mktemp -d)"
trap 'chmod -R u+w "$TMPROOT"; rm -rf "$TMPROOT"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# shellcheck source=install/versions.env
. "$VERSIONS_ENV"

# stub DIR NAME OUTPUT
#   An executable NAME in DIR whose --version prints OUTPUT.
stub() {
  # shellcheck disable=SC2016 # $1 belongs to the stub, not to this shell
  printf '#!/bin/sh\n[ "$1" = "--version" ] || exit 2\ncat <<"EOF"\n%s\nEOF\n' "$3" > "$1/$2"
  chmod +x "$1/$2"
}

# good_bin DIR — stubs for every tool, in the formats upstream prints.
good_bin() {
  mkdir -p "$1"
  stub "$1" promtool "promtool, version ${PROMTOOL_VERSION} (branch: HEAD, revision: 6d7569113f1c)
  go version:       go1.23.2"
  stub "$1" gofumpt "v${GOFUMPT_VERSION} (go${GO_FULL_VERSION})"
  stub "$1" staticcheck "staticcheck 2026.1 (v${STATICCHECK_VERSION})"
}

# run_smoke BIN_DIR TMPDIR_VALUE — runs smoke.sh with only BIN_DIR plus the
# system dirs on PATH; prints its combined output, returns its exit code.
run_smoke() {
  local bin="$1" tmp="$2"
  PATH="$bin:/usr/bin:/bin" TMPDIR="$tmp" VERSIONS_ENV="$VERSIONS_ENV" \
    bash "$SCRIPT_DIR/smoke.sh" 2>&1
}

# expect NAME WANT_RC WANT_SUBSTRING BIN_DIR TMPDIR_VALUE
expect() {
  local name="$1" want_rc="$2" want="$3" out rc=0
  out="$(run_smoke "$4" "$5")" || rc=$?
  if [ "$rc" != "$want_rc" ]; then
    fail "$name" "exit $rc, want $want_rc; output: $out"
  elif [[ "$out" != *"$want"* ]]; then
    fail "$name" "output lacks '$want': $out"
  else
    pass "$name"
  fi
}

TMPW="$TMPROOT/tmp"
mkdir -p "$TMPW"

good_bin "$TMPROOT/good"
expect "all tools at pins, TMPDIR writable" 0 "TMPDIR=$TMPW is writable" "$TMPROOT/good" "$TMPW"

good_bin "$TMPROOT/nopromtool"
rm "$TMPROOT/nopromtool/promtool"
expect "missing promtool" 1 "promtool is not on PATH" "$TMPROOT/nopromtool" "$TMPW"

good_bin "$TMPROOT/oldgofumpt"
stub "$TMPROOT/oldgofumpt" gofumpt "v0.0.1 (go${GO_FULL_VERSION})"
expect "gofumpt off its pin" 1 "want 'v${GOFUMPT_VERSION} '" "$TMPROOT/oldgofumpt" "$TMPW"

good_bin "$TMPROOT/brokensc"
printf '#!/bin/sh\necho "cannot execute" >&2\nexit 126\n' > "$TMPROOT/brokensc/staticcheck"
expect "staticcheck --version fails" 1 "staticcheck --version exited non-zero" "$TMPROOT/brokensc" "$TMPW"

expect "TMPDIR unset" 1 "TMPDIR is unset" "$TMPROOT/good" ""
expect "TMPDIR missing" 1 "is not a directory" "$TMPROOT/good" "$TMPROOT/nope"

# root writes through a 0555 directory, so this case only means something
# for an unprivileged runner (the ARC runner pods are).
if [ "$(id -u)" != 0 ]; then
  mkdir -p "$TMPROOT/ro"
  chmod 0555 "$TMPROOT/ro"
  expect "TMPDIR not writable" 1 "is not writable" "$TMPROOT/good" "$TMPROOT/ro"
fi

echo "smoke_test: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
