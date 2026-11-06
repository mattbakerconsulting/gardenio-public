#!/usr/bin/env bash
# acceptance-smoke_test.sh — Regression tests for the acceptance-smoke.sh
# argument parser (ka-0000002999).
#
# The bug: the `--on-box` / `--off-box-only` case arms set flags but never
# `shift`ed, so `while [ $# -gt 0 ]` spun forever. On a real sweep box the
# on-box acceptance gate then hung silently until the CI task's 60m timeout
# SIGKILLed the whole sweep — skipping the always-teardown trap and leaking
# the EC2 box (run db0b7a9d, box i-0123456789abcdef0).
#
# These tests only exercise the parser + early-exit paths, so they are
# hermetic: no cluster, no network. A stub PATH ensures kubectl/k3s are
# absent so the on-box half fails fast instead of probing a live cluster.
#
# Usage:  bash scripts/inbox/acceptance-smoke_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/acceptance-smoke.sh"

PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# Stub PATH: coreutils/bash/grep only — no kubectl, no k3s, no nc. The on-box
# half must then take its "neither kubectl nor k3s found" fast path.
STUB="$TMP/bin"; mkdir -p "$STUB"
for t in bash sh printf echo grep sed awk tr wc dirname mktemp rm cat curl; do
  p="$(command -v "$t" 2>/dev/null)" && ln -s "$p" "$STUB/$t" || true
done

# 1. THE regression: --on-box (a no-value flag) must not wedge the parser.
#    Pre-fix this spun forever; 124 = still hanging.
out="$(timeout 15 env PATH="$STUB" bash "$SUT" --on-box --namespace tns --release trel 2>&1)"; rc=$?
if [ "$rc" = 124 ]; then
  fail "--on-box terminates" "parser still hangs (timeout 124)"
elif grep -q "namespace: tns" <<<"$out"; then
  pass "--on-box parses through to the run (no infinite shift loop)"
else
  fail "--on-box terminates" "rc=$rc out=$(echo "$out" | head -3)"
fi

# 2. Same for --off-box-only (the sibling no-value flag).
out="$(timeout 15 env PATH="$STUB" bash "$SUT" --off-box-only --namespace tns 2>&1)"; rc=$?
if [ "$rc" = 124 ]; then
  fail "--off-box-only terminates" "parser still hangs (timeout 124)"
elif grep -q "OFF-BOX" <<<"$out"; then
  pass "--off-box-only parses through to the off-box half"
else
  fail "--off-box-only terminates" "rc=$rc out=$(echo "$out" | head -3)"
fi

# 3. Args AFTER the no-value flag must still be consumed (the hang ate them).
out="$(timeout 15 env PATH="$STUB" bash "$SUT" --on-box --namespace after-flag 2>&1)"; rc=$?
if grep -q "namespace: after-flag" <<<"$out"; then
  pass "args after --on-box are still parsed (--namespace honored)"
else
  fail "args after --on-box" "rc=$rc out=$(echo "$out" | head -3)"
fi

# 4. Unknown args still exit 2 (parser contract unchanged by the fix).
timeout 10 env PATH="$STUB" bash "$SUT" --no-such-flag >/dev/null 2>&1; rc=$?
if [ "$rc" = 2 ]; then
  pass "unknown arg still exits 2"
else
  fail "unknown arg exit code" "rc=$rc"
fi

# 5. --on-box with no cluster tooling FAILS (exit 1) but does not hang: the
#    on-box half must degrade to a fast 'neither kubectl nor k3s found' miss.
out="$(timeout 15 env PATH="$STUB" bash "$SUT" --on-box 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -qi "neither kubectl nor k3s" <<<"$out"; then
  pass "--on-box without kubectl: fast loud failure, no hang"
else
  fail "--on-box no-kubectl" "rc=$rc out=$(echo "$out" | tail -3)"
fi

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
