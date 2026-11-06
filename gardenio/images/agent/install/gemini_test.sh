#!/usr/bin/env bash
# gemini_test.sh — hermetic tests for install/gemini.sh.
#
# Same justification as codex_test.sh: the `agent` image is release-only
# (images.yml drops it on pull_request, owner ruling gd-9mtwbE3hoK), so without
# this a mis-wired digest variable, a wrong zip layout or a stale version pin
# would first surface while cutting a release tag. The tests run the REAL script
# against a stubbed `curl` and a synthetic bundle zip: no network, no 21MB
# download.
#
# Not covered on purpose: that the pinned digest matches what GitHub serves
# today (network; the spike re-hashed it) and that the real bundle runs (the
# phase 0 spike did, on Node 24).
#
# Usage:  bash gardenio/images/agent/install/gemini_test.sh

# shellcheck source-path=SCRIPTDIR
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

PASS=0
FAIL=0
TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# shellcheck source=./versions.env
WANT_VERSION="$(. "${SCRIPT_DIR}/versions.env"; echo "$GEMINI_VERSION")"
[ -n "$WANT_VERSION" ] || { echo "GEMINI_VERSION missing from versions.env"; exit 1; }
# shellcheck source=./checksums.env
WANT_SHA="$(. "${SCRIPT_DIR}/checksums.env"; echo "$GEMINI_SHA256")"
case "$WANT_SHA" in
  [0-9a-f]*) [ "${#WANT_SHA}" -eq 64 ] || { echo "GEMINI_SHA256 is not 64 hex chars"; exit 1; } ;;
  *) echo "GEMINI_SHA256 missing from checksums.env"; exit 1 ;;
esac

# make_bundle <dir> <version> [layout]
#   Stand-in for gemini-cli-bundle.zip: gemini.js (an executable that answers
#   --version) plus a chunk file at the zip ROOT, as the real asset does.
#   layout=nested wraps it in a directory, the shape this script must reject.
#   Prints the zip's sha256.
make_bundle() {
  local dir="$1" version="$2" layout="${3:-flat}"
  mkdir -p "$dir/stage"
  cat > "$dir/stage/gemini.js" <<EOT
#!/bin/sh
[ "\$1" = "--version" ] && echo "${version}" && exit 0
exit 1
EOT
  chmod +x "$dir/stage/gemini.js"
  echo "// chunk" > "$dir/stage/chunk-AAAA.js"
  if [ "$layout" = nested ]; then
    mkdir -p "$dir/nest/bundle"; cp "$dir"/stage/* "$dir/nest/bundle/"
    (cd "$dir/nest" && python3 -m zipfile -c "$dir/bundle.zip" bundle)
  else
    (cd "$dir/stage" && python3 -m zipfile -c "$dir/bundle.zip" gemini.js chunk-AAAA.js)
  fi
  sha256sum "$dir/bundle.zip" | awk '{print $1}'
}

# run_install <case> <zip> <sha> [with_node]
#   Stages the real gemini.sh with the real versions.env/lib.sh and a synthetic
#   checksums.env, a `curl` stub serving <zip>, and (unless with_node=no) a
#   `node` stub so the node-presence guard passes. Echoes the exit status.
run_install() {
  local case_name="$1" served="$2" sha="$3" with_node="${4:-yes}"
  local d="$TMPROOT/$case_name"
  mkdir -p "$d/bin" "$d/prefix" "$d/install"
  cp "${SCRIPT_DIR}/gemini.sh" "${SCRIPT_DIR}/lib.sh" "${SCRIPT_DIR}/versions.env" "$d/install/"
  echo "export GEMINI_SHA256=${sha}" > "$d/install/checksums.env"

  cat > "$d/bin/curl" <<EOT
#!/bin/sh
out=""; prev=""
for a in "\$@"; do
  case "\$a" in https://*) echo "\$a" >> "$d/urls" ;; esac
  [ "\$prev" = "-o" ] && out="\$a"
  prev="\$a"
done
[ -n "\$out" ] || exit 1
cp "$served" "\$out"
EOT
  chmod +x "$d/bin/curl"
  if [ "$with_node" = yes ]; then printf '#!/bin/sh\nexit 0\n' > "$d/bin/node"; chmod +x "$d/bin/node"; fi

  # Without node the PATH must not leak the host's: keep only the stub dir plus
  # the system dirs the script's own tools live in (node is not among them in CI).
  local rc=0 path="$d/bin:$PATH"
  if [ "$with_node" != yes ]; then
    mkdir -p "$d/nonode"
    for t in bash dirname mktemp rm mkdir cat; do ln -sf "$(command -v "$t")" "$d/nonode/$t"; done
    path="$d/bin:$d/nonode"
  fi
  env PATH="$path" INSTALL_PREFIX="$d/prefix" bash "$d/install/gemini.sh" > "$d/out" 2>&1 || rc=$?
  echo "$rc"
}

# ── Test 1: good install ─────────────────────────────────────────────
fx="$TMPROOT/fx-ok"
sha="$(make_bundle "$fx" "$WANT_VERSION")"
rc="$(run_install ok "$fx/bundle.zip" "$sha")"
d="$TMPROOT/ok"
if [ "$rc" = "0" ]; then pass "install exits 0"; else fail "install exits 0" "rc=$rc: $(cat "$d/out")"; fi
if [ -x "$d/prefix/bin/gemini" ]; then pass "bin/gemini is executable"; else fail "bin/gemini is executable" "missing or not +x"; fi
if [ -L "$d/prefix/bin/gemini" ] && [ -s "$d/prefix/lib/gemini-cli/gemini.js" ]; then
  pass "bin/gemini links to lib/gemini-cli/gemini.js at the zip root"
else
  fail "bin/gemini links to lib/gemini-cli/gemini.js at the zip root" "$(find "$d/prefix" 2>&1 | head -20)"
fi
if [ -s "$d/prefix/lib/gemini-cli/chunk-AAAA.js" ]; then pass "sibling chunk files are extracted next to gemini.js"
else fail "sibling chunk files are extracted next to gemini.js" "chunk missing"; fi
if grep -qF "/releases/download/v${WANT_VERSION}/gemini-cli-bundle.zip" "$d/urls"; then
  pass "requests the v<ver> bundle asset"
else
  fail "requests the v<ver> bundle asset" "urls=$(cat "$d/urls" 2>/dev/null)"
fi

# ── Test 2: bad checksum fails closed ────────────────────────────────
fx="$TMPROOT/fx-bad"
make_bundle "$fx" "$WANT_VERSION" > /dev/null
rc="$(run_install badsha "$fx/bundle.zip" "0000000000000000000000000000000000000000000000000000000000000000")"
if [ "$rc" != "0" ]; then pass "checksum mismatch fails the install"; else fail "checksum mismatch fails the install" "rc=0"; fi
if [ ! -e "$TMPROOT/badsha/prefix/bin/gemini" ]; then pass "checksum mismatch installs nothing"
else fail "checksum mismatch installs nothing" "bin/gemini exists"; fi

# ── Test 3: version/digest mismatch fails closed ─────────────────────
# GEMINI_VERSION bumped without GEMINI_SHA256 (or the reverse): the bytes that
# verify are the OLD release. The smoke test is the only guard.
fx="$TMPROOT/fx-stale"
sha="$(make_bundle "$fx" "0.0.1-stale")"
rc="$(run_install stale "$fx/bundle.zip" "$sha")"
if [ "$rc" != "0" ]; then pass "version mismatch after install fails the build"
else fail "version mismatch after install fails the build" "rc=0"; fi
if grep -q "version mismatch" "$TMPROOT/stale/out"; then pass "version mismatch prints expected and actual"
else fail "version mismatch prints expected and actual" "$(cat "$TMPROOT/stale/out")"; fi

# ── Test 4: unexpected zip layout fails with a clear message ─────────
fx="$TMPROOT/fx-nested"
sha="$(make_bundle "$fx" "$WANT_VERSION" nested)"
rc="$(run_install nested "$fx/bundle.zip" "$sha")"
if [ "$rc" != "0" ] && grep -q "gemini.js missing" "$TMPROOT/nested/out"; then
  pass "a nested zip layout is rejected with a clear message"
else
  fail "a nested zip layout is rejected with a clear message" "rc=$rc: $(cat "$TMPROOT/nested/out")"
fi

# ── Test 5: no node on PATH fails early ──────────────────────────────
fx="$TMPROOT/fx-nonode"
sha="$(make_bundle "$fx" "$WANT_VERSION")"
rc="$(run_install nonode "$fx/bundle.zip" "$sha" no)"
if [ "$rc" != "0" ] && grep -q "node is not on PATH" "$TMPROOT/nonode/out"; then
  pass "a missing node fails with a clear message"
else
  fail "a missing node fails with a clear message" "rc=$rc: $(cat "$TMPROOT/nonode/out")"
fi

echo
echo "gemini_test.sh: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
