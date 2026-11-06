#!/usr/bin/env bash
# bootstrap-ec2_test.sh — Unit tests for the offline-assets helpers + repo-tree
# detection in bootstrap-ec2.sh (Tarboat T1: G1.1 ka-0000005747, G1.2 ka-0000008214).
#
# bootstrap-ec2.sh is an EXECUTE-on-load installer (it `set -euo pipefail`s and
# runs the full bring-up), so it cannot be sourced. We extract just the pure
# helper functions (offline_asset / offline_required / detect_repo_dir) plus the
# `die` they call and the vars they read, and exercise them against a synthetic
# staging dir. This locks the staged-vs-network resolution, the --airgap-strict
# hard-fail contract, AND the G1.2 property that a tarball-staged repo tree with
# NO .git is accepted — without standing up an EC2.
#
# Usage:  bash scripts/bootstrap-ec2_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/bootstrap-ec2.sh"

PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# Build a tiny sourceable harness that pulls ONLY the helper functions out of
# the installer (everything between the two helpers' defs), defines a `die`
# that exits non-zero, and seeds the two vars the helpers read.
HARNESS="$TMP/harness.sh"
{
  echo 'die() { echo "DIE: $*" >&2; exit 1; }'
  echo 'AIRGAP_STRICT="${AIRGAP_STRICT:-0}"'
  echo 'OFFLINE_ASSETS="${OFFLINE_ASSETS:-}"'
  echo 'REPO_DIR="${REPO_DIR:-}"'
  # Extract each helper's body verbatim: print from its `name() {` line through
  # the next top-level `}` (column-0 close brace).
  for fn in offline_asset offline_required detect_repo_dir; do
    awk -v fn="$fn" '
      $0 ~ ("^" fn "\\(\\) \\{") { p=1 }
      p { print }
      p && /^\}/ { exit }
    ' "$SCRIPT_UNDER_TEST"
  done
} > "$HARNESS"

# Sanity: the harness must actually define all extracted functions.
if ! grep -q 'offline_asset()' "$HARNESS" \
   || ! grep -q 'offline_required()' "$HARNESS" \
   || ! grep -q 'detect_repo_dir()' "$HARNESS"; then
  echo "FATAL: could not extract helpers from $SCRIPT_UNDER_TEST" >&2
  exit 2
fi

# run_helper <env-assignments...> -- <function + args>  → prints stdout, sets RC
run_helper() {
  local env_kv=() ; while [ "$1" != "--" ]; do env_kv+=("$1"); shift; done; shift
  env "${env_kv[@]}" bash -c "source '$HARNESS'; $*" 2>"$TMP/err"
}

# Stage a populated assets dir.
ASSETS="$TMP/tarboat"
mkdir -p "$ASSETS/bin" "$ASSETS/manifests"
echo "#!/bin/sh" > "$ASSETS/bin/helm"        # non-empty staged binary
: > "$ASSETS/bin/empty"                       # zero-byte → must NOT resolve

# 1. offline_asset returns the absolute path for a present, non-empty file.
out="$(run_helper "OFFLINE_ASSETS=$ASSETS" -- 'offline_asset bin/helm')" && rc=0 || rc=$?
if [ "$rc" = 0 ] && [ "$out" = "$ASSETS/bin/helm" ]; then
  pass "offline_asset resolves a staged file to its abs path"
else
  fail "offline_asset resolves a staged file" "rc=$rc out=$out"
fi

# 2. offline_asset fails for a missing file.
run_helper "OFFLINE_ASSETS=$ASSETS" -- 'offline_asset bin/nope' >/dev/null && rc=0 || rc=$?
if [ "$rc" != 0 ]; then pass "offline_asset fails on a missing file"; else fail "offline_asset missing file" "expected non-zero"; fi

# 3. offline_asset fails for a zero-byte file (-s guard).
run_helper "OFFLINE_ASSETS=$ASSETS" -- 'offline_asset bin/empty' >/dev/null && rc=0 || rc=$?
if [ "$rc" != 0 ]; then pass "offline_asset fails on a zero-byte file"; else fail "offline_asset zero-byte" "expected non-zero"; fi

# 4. offline_asset fails when OFFLINE_ASSETS is empty.
run_helper "OFFLINE_ASSETS=" -- 'offline_asset bin/helm' >/dev/null && rc=0 || rc=$?
if [ "$rc" != 0 ]; then pass "offline_asset fails when OFFLINE_ASSETS unset"; else fail "offline_asset empty base" "expected non-zero"; fi

# 5. offline_required is a no-op (rc 0) when NOT strict.
run_helper "AIRGAP_STRICT=0" -- 'offline_required "bin/helm"' >/dev/null && rc=0 || rc=$?
if [ "$rc" = 0 ]; then pass "offline_required is a no-op when not strict"; else fail "offline_required non-strict" "expected rc=0"; fi

# 6. offline_required HARD-FAILS (die → non-zero) under --airgap-strict.
run_helper "AIRGAP_STRICT=1" -- 'offline_required "bin/helm"' >/dev/null && rc=0 || rc=$?
if [ "$rc" != 0 ] && grep -q "airgap-strict" "$TMP/err"; then
  pass "offline_required hard-fails under --airgap-strict"
else
  fail "offline_required strict" "rc=$rc err=$(cat "$TMP/err")"
fi

# ── detect_repo_dir: a tarball-staged tree (NO .git) must be accepted (G1.2) ──
# The defining Tarboat property: a friend untars the bundle (no private clone,
# no .git anywhere) and the repo tree is still found. detect_repo_dir keys on
# the sentinel scripts/deploy-byo.sh, never on .git.

# Stage a synthetic offline-assets dir with a repo/ subtree that has NO .git.
STAGED_REPO="$TMP/tarboat-assets/repo"
mkdir -p "$STAGED_REPO/scripts"
echo "#!/usr/bin/env bash" > "$STAGED_REPO/scripts/deploy-byo.sh"   # the sentinel
[ -d "$STAGED_REPO/.git" ] && { echo "FATAL: test staged a .git — invalid fixture" >&2; exit 2; }

# 7. detect_repo_dir resolves the OFFLINE_ASSETS/repo tree (no .git present).
out="$(run_helper "OFFLINE_ASSETS=$TMP/tarboat-assets" "REPO_DIR=" -- 'detect_repo_dir')" && rc=0 || rc=$?
if [ "$rc" = 0 ] && [ "$out" = "$STAGED_REPO" ]; then
  pass "detect_repo_dir accepts a tarball-staged repo tree with NO .git"
else
  fail "detect_repo_dir staged-no-git" "rc=$rc out=$out (want $STAGED_REPO)"
fi

# 8. The sentinel is scripts/deploy-byo.sh, not .git: removing it (leaving the
#    dir) must make the OFFLINE_ASSETS probe miss — proving we key on the file.
rm -f "$STAGED_REPO/scripts/deploy-byo.sh"
out="$(run_helper "OFFLINE_ASSETS=$TMP/tarboat-assets" "REPO_DIR=" -- 'detect_repo_dir' 2>/dev/null)" && rc=0 || rc=$?
# With the sentinel gone, the OFFLINE_ASSETS probe must NOT resolve to it
# (it may fall through to /opt/gardenio/repo on a real box, or return non-zero
# here — either way it must NOT claim the staged dir).
if [ "$out" != "$STAGED_REPO" ]; then
  pass "detect_repo_dir keys on scripts/deploy-byo.sh sentinel, not the dir"
else
  fail "detect_repo_dir sentinel" "resolved $out despite missing sentinel"
fi
echo "$STAGED_REPO" >/dev/null  # keep shellcheck happy about the var

# 9. Explicit REPO_DIR wins over everything (probe 1, no .git required).
EXPLICIT="$TMP/explicit-repo"
mkdir -p "$EXPLICIT/scripts"
out="$(run_helper "REPO_DIR=$EXPLICIT" "OFFLINE_ASSETS=$TMP/tarboat-assets" -- 'detect_repo_dir')" && rc=0 || rc=$?
if [ "$rc" = 0 ] && [ "$out" = "$EXPLICIT" ]; then
  pass "detect_repo_dir honors explicit REPO_DIR (no .git needed)"
else
  fail "detect_repo_dir explicit REPO_DIR" "rc=$rc out=$out (want $EXPLICIT)"
fi

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
