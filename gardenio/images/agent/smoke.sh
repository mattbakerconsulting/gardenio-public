#!/usr/bin/env bash
# smoke.sh — check that the agent image ships the tools crews were fetching
# by hand, at the pinned versions, and that $TMPDIR is usable (gd-hZl1cKiELM).
#
# Run as the last step of the Dockerfile's `agent` stage, as the agent user.
# The `agent` image is not built on PRs (images.yml), and clis.sh backgrounds
# its downloads behind a bare `wait`, so without this a failed install would
# ship a green image. smoke_test.sh exercises this script on PRs with stubs.
#
# Usage:  VERSIONS_ENV=/tmp/install/versions.env bash smoke.sh
# shellcheck source-path=SCRIPTDIR
set -euo pipefail

VERSIONS_ENV="${VERSIONS_ENV:-/tmp/install/versions.env}"
# shellcheck source=install/versions.env
. "$VERSIONS_ENV"

FAILED=0
fail() { echo "smoke: FAIL: $*" >&2; FAILED=1; }

# check_version BIN WANT
#   BIN --version must succeed and print WANT somewhere in its output.
check_version() {
  local bin="$1" want="$2" out
  if ! command -v "$bin" >/dev/null 2>&1; then
    fail "$bin is not on PATH"
    return
  fi
  if ! out="$("$bin" --version 2>&1)"; then
    fail "$bin --version exited non-zero: $out"
    return
  fi
  case "$out" in
    *"$want"*) echo "smoke: ok: $bin: ${out%%$'\n'*}" ;;
    *) fail "$bin --version printed '${out%%$'\n'*}', want '$want'" ;;
  esac
}

check_version promtool "version ${PROMTOOL_VERSION} "
check_version gofumpt "v${GOFUMPT_VERSION} "
check_version staticcheck "(v${STATICCHECK_VERSION})"

if [ -z "${TMPDIR:-}" ]; then
  fail "TMPDIR is unset; OUT=\$TMPDIR/x would write to /"
elif [ ! -d "$TMPDIR" ]; then
  fail "TMPDIR=$TMPDIR is not a directory"
elif ! probe="$(mktemp -p "$TMPDIR" smoke.XXXXXX 2>/dev/null)"; then
  fail "TMPDIR=$TMPDIR is not writable by $(id -un 2>/dev/null || id -u)"
else
  rm -f "$probe"
  echo "smoke: ok: TMPDIR=$TMPDIR is writable"
fi

exit "$FAILED"
