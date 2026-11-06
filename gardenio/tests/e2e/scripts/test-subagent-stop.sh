#!/usr/bin/env bash
# test-subagent-stop.sh — SubagentStop final-message forwarding e2e (from-source Go probe)
#
# Bead ka-0000004963 (parent ka-0000000958, the E2E coverage-gap sprint). This is
# the e2e backfill for the fix in gd-TdQnFDzJdo (MR !6760): before that fix the
# bridge flipped a subagent delegation chunk to "complete" with an EMPTY Output —
# the subagent's FINAL message was silently DROPPED on its way to the parent
# agent's stream. The fix threads evt.LastAssistantMessage from hookstream.go
# through HandleSubagentStop into the chunk Output.
#
# ── WHY A FROM-SOURCE GO PROBE (not a cluster test) ───────────────────────────
#
# The output-drop lived entirely in the bridge CONSUMER (hookstream.go +
# agentstream.go), so the highest-signal e2e drives that exact chain WITHOUT a
# cluster: an //go:build e2e-tagged Go test
# (internal/bridge/subagentstop_e2e_test.go) embeds a NATS server, wires the REAL
# HookStream subscriber to a real AgentStreamManager, invokes the REAL
# `gb hook relay` binary (the exact code a SubagentStop hook runs in production)
# with a subagent final message, and asserts that message reaches the stream
# chunk Output with status=complete. Only the terminal Slack sink is a mock (the
# seam where the chunk becomes observable) — everything upstream is production
# code. This makes the module FAIL against the pre-fix behaviour (empty Output),
# which a NATS-only relay assertion could not catch (the drop was downstream of
# NATS).
#
# The probe is `//go:build e2e`-tagged so its embedded nats-server stays off the
# default build/lint surface — the same discipline as the claudemore-hooks
# nats-relay probe (internal/claudemore/hooks_natsrelay_e2e_test.go), which this
# module mirrors.
#
# ── LEGS (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   relayed-e2e     SubagentStop relays end-to-end (relay→NATS→subscriber→handler)
#                   to a task_update stream chunk — the chain is intact.
#   status-complete The delegation chunk is marked complete.
#   chunk-id        The chunk targets the subagent delegation (subagent:<id>).
#   final-forwarded THE HEADLINE: the subagent's FINAL message is forwarded verbatim
#                   into the chunk Output (empty pre-fix → this is the regression gate).
#
# ── SKIP-CLEAN CONTRACT ───────────────────────────────────────────────────────
#
# Requires a Go toolchain (to build `gb` from source and run the probe). If it is
# absent the module SKIPS CLEANLY (exit 0) with a LOUD banner — an absent
# toolchain must never red the suite (mirrors test-claudemore-hooks). When Go is
# present, every leg is a HARD assertion. No cluster, no `claude` binary, $0.
#
# Standalone runnable:
#   ./gardenio/tests/e2e/scripts/test-subagent-stop.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# scripts/ -> e2e/ -> tests/ -> gardenio/ -> controller
CONTROLLER_DIR="$(cd "$SCRIPT_DIR/../../../controller" 2>/dev/null && pwd)" || CONTROLLER_DIR=""

GO_BIN="${GO_BIN:-go}"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/subagent-stop.XXXXXX")"
PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

skip_clean() {
  blue "SKIP subagent-stop: $1"
  blue "  The SubagentStop e2e builds gb from source and drives an embedded-NATS"
  blue "  Go probe; without the Go toolchain there is nothing to drive. Skipping"
  blue "  cleanly (exit 0) — an absent toolchain must never red the suite (see"
  blue "  test-claudemore-hooks)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() { rm -rf "$WORKDIR" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# ── Preflight: skip cleanly if the toolchain is missing ─────────────
blue "╔══════════════════════════════════════════════════════════╗"
blue "║  SubagentStop final-message forwarding e2e (ka-0000004963) ║"
blue "╚══════════════════════════════════════════════════════════╝"
echo ""

command -v "$GO_BIN" >/dev/null 2>&1 || skip_clean "'$GO_BIN' toolchain not on PATH"
[ -n "$CONTROLLER_DIR" ] && [ -f "$CONTROLLER_DIR/go.mod" ] || skip_clean "controller module not found (looked at '${CONTROLLER_DIR:-<unresolved>}')"

dim "  go:         $(command -v "$GO_BIN")  ($("$GO_BIN" version 2>/dev/null))"
dim "  controller: $CONTROLLER_DIR"
dim "  workdir:    $WORKDIR"
echo ""

# ── Build gb from source ────────────────────────────────────────────
# The probe drives the REAL `gb hook relay` — the exact code a SubagentStop hook
# runs in production — so build gb and hand its path to the probe via GB_BIN.
blue "Building gb from source..."
GB_BIN="$WORKDIR/gb"
BUILD_LOG="$WORKDIR/build.log"
if ! ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 "$GO_BIN" build -o "$GB_BIN" ./cmd/gb/ ) >"$BUILD_LOG" 2>&1; then
  # A build failure of OUR code is a real failure, not a skip — but a missing
  # network / module-download failure in a locked-down shard is an environment
  # gap that must not red the suite. Distinguish by grepping the log.
  if grep -qiE "cannot find module|dial tcp|no such host|timeout|connection refused|proxy" "$BUILD_LOG"; then
    skip_clean "go build could not fetch deps (offline shard): $(tail -3 "$BUILD_LOG" | tr '\n' ' ')"
  fi
  red "  ✗ build failed:"
  dim "$(tail -15 "$BUILD_LOG")"
  exit 1
fi
green "  ✓ built gb"
echo ""

# ══════════════════════════════════════════════════════════════════
#  SubagentStop → stream chunk (embedded-NATS go test)
# ══════════════════════════════════════════════════════════════════
blue "SubagentStop. final message forwarded end-to-end to the parent stream..."
PROBE_OUT="$WORKDIR/subagentstop.out"
# The probe is an //go:build e2e-tagged Go test that embeds a NATS server and
# drives the REAL `gb hook relay` → real HookStream subscriber → HandleSubagentStop.
# It emits PASS:/FAIL:/PROBE_SKIP: log lines (go test -v prefixes them with
# whitespace) which we parse below.
( cd "$CONTROLLER_DIR" && GB_BIN="$GB_BIN" "$GO_BIN" test -tags e2e -v \
    ./internal/bridge/ -run TestSubagentStopForwardsFinalMessageE2E -count=1 -timeout 120s ) \
    >"$PROBE_OUT" 2>&1
PROBE_RC=$?
if grep -q 'PROBE_SKIP:' "$PROBE_OUT"; then
  fail "subagent-stop probe ran" "probe self-skipped: $(grep -o 'PROBE_SKIP:.*' "$PROBE_OUT" | head -1)"
elif [ "$PROBE_RC" -eq 0 ]; then
  while IFS= read -r line; do
    case "$line" in
      *PASS:*) pass "${line##*PASS: }" ;;
    esac
  done < "$PROBE_OUT"
else
  while IFS= read -r line; do
    case "$line" in
      *FAIL:*) fail "subagent-stop: ${line##*FAIL: }" ;;
    esac
  done < "$PROBE_OUT"
  [ "$FAIL" -eq 0 ] && fail "subagent-stop probe" "go test exit $PROBE_RC: $(tail -5 "$PROBE_OUT" | tr '\n' ' ')"
fi
echo ""

# ── Summary ──────────────────────────────────────────────────────
blue "══════════════════════════════════════════════"
if [ "$FAIL" -eq 0 ]; then
  green "  All $PASS SubagentStop e2e assertions passed"
else
  red "  $PASS passed, $FAIL failed"
  echo ""
  for err in "${ERRORS[@]}"; do
    red "  - $err"
  done
fi
blue "══════════════════════════════════════════════"

[ "$FAIL" -eq 0 ] || exit 1
exit 0
