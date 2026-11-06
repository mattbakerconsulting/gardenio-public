#!/usr/bin/env bash
# test-jira-syncback-denybydefault.sh — jira-bridge SYNC-BACK deny-by-default +
# source:jira-dark early-return E2E (task ka-0000005719, coverage-backfill epic
# ka-0000000958; enforcement bead ka-0000005143, commit f30cae2).
#
# ── The gap this module closes ────────────────────────────────────────────────
#
# jira_sync write-back was flipped from a deny-LIST (FSM-only) to an enforced
# allow-LIST (ka-0000005143): only allowlisted projects (PE/DEVOPS) may write
# back, an explicit block (FSM) always wins, and a source:jira-dark bead
# early-returns in BOTH handleUpdated and handleClosed BEFORE any project gating
# so dark-universe work never leaks upstream. That flip has unit coverage
# (jira_sync_test.go: TestJiraSync_NonAllowlistedProject_* / _DarkMarker_*), and
# the SIBLING outbox / write-control modules cover the triage-label allowlist and
# the browse-only 403 — but NOTHING at the E2E layer drove the sync-back handlers
# themselves to assert the PROJECT deny-by-default nor the jira-dark early-return.
# The sync-back path (JiraSync.handleUpdated/handleClosed) had ZERO e2e coverage.
# This module is that leg.
#
# ── How (a from-source probe, the jira-conv-probe precedent) ──────────────────
#
# The jira-bridge is OFF in the ephemeral e2e namespace, so there is no
# live sync-back watcher to POST to, and the handlers are UNEXPORTED. So — exactly
# like the `jira-conversational` module (ka-0000003999) — this drives a from-source
# probe, cmd/jira-syncback-probe, that wires the REAL bridge.JiraSync (with the
# SAME config-driven WritePolicy production wires) onto a REAL bridge.SSEStream and
# feeds it real SSE bead frames, against a STUB Jira httptest server that records
# every outbound REST request per issue key. No live Jira, no daemon, no pods.
#
# Each leg emits its deny/dark TARGET bead AND an allowlisted PE positive-control
# bead into the same live engine, then asserts the target key made ZERO Jira
# requests AND the control key wrote back — so a "0 writes" pass can never be
# vacuous (a dead engine that writes nothing fails the control). The dark legs are
# the sharpest: target and control are BOTH allowlisted PE keys, so only the
# source:jira-dark label differs — proving the dark marker, not the project gate,
# is what suppresses the write. (See the probe's header for the barrier + control
# rationale; mutation-tested both directions during authoring.)
#
# Legs (each a hard sub-gate; any FAIL reddens the shard):
#
#   1  updated-deny-nonallowlisted  an arbitrary non-allowlisted key (ACME) →
#                                   handleUpdated writes NOTHING; PE control writes.
#   2  closed-deny-nonallowlisted   same via handleClosed.
#   3  updated-deny-fsm             the explicitly-blocked FSM project → NOTHING
#                                   in handleUpdated (block wins); PE control writes.
#   4  closed-deny-fsm              same via handleClosed.
#   5  updated-dark-early-return    source:jira-dark on an ALLOWLISTED PE key with
#                                   a claim + MR + mr_merged → handleUpdated
#                                   early-returns, writes NOTHING; non-dark PE control writes.
#   6  closed-dark-early-return     same via handleClosed.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The Go toolchain + the cmd/jira-syncback-probe source are present in every e2e
# shard container (the shard `use:` list includes the build-jira-syncback-probe
# task, and go is installed). If either is somehow absent the module SKIPS
# CLEANLY (exit 0) with a LOUD banner — an absent toolchain must never red the
# suite — mirroring test-jira-conversational.sh. When present, every leg is a
# HARD assertion; a probe BUILD failure is a HARD failure (the probe is the whole
# module).

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
PROBE_BIN=""

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
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# skip_all exits 0 (a missing toolchain must NOT red the suite) with a LOUD
# banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP jira-syncback-denybydefault (LOUD): $why"
  red " This run did NOT assert the jira_sync deny-by-default project allowlist"
  red " nor the source:jira-dark early-return (ka-0000005143). A green suite here"
  red " proves NOTHING about the sync-back write gate. The e2e shard is built with"
  red " a Go toolchain + the cmd/jira-syncback-probe source (build-jira-syncback-probe"
  red " task) — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

PATH_INSTALLED=0
cleanup() {
  # Only remove a temp binary we built; never a PATH-installed one.
  [ "$PATH_INSTALLED" != "1" ] && [ -n "$PROBE_BIN" ] && rm -f "$PROBE_BIN" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " jira-bridge sync-back deny-by-default + dark early-return E2E (ka-0000005719)"
echo " non-allowlisted deny -> FSM block -> source:jira-dark early-return (updated + closed)"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the probe binary ─────────────────────────────────────
# Prefer a binary pre-built + smoke-
# checked onto PATH (per-source caching). Fall back to an in-shell build from
# source (the jira-conv-probe / gvisor-probe precedent) for local dev and
# robustness if the pre-built binary is absent. Only when BOTH the pre-built
# binary is absent AND no Go toolchain/source is available do we skip clean.
if command -v jira-syncback-probe >/dev/null 2>&1; then
  PROBE_BIN="$(command -v jira-syncback-probe)"
  PATH_INSTALLED=1
  pass "using pre-built jira-syncback-probe ($PROBE_BIN)"
else
  PATH_INSTALLED=0
  command -v go >/dev/null 2>&1 \
    || skip_all "no pre-built jira-syncback-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/jira-syncback-probe" ] \
    || skip_all "no pre-built jira-syncback-probe on PATH and cmd/jira-syncback-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/jira-syncback-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/jira-syncback-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/jira-syncback-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/jira-syncback-probe/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/jira-syncback-probe built from source"
  else
    fail "build jira-syncback-probe" "go build failed: $(tail -8 "$BUILD_LOG")"
    # A build failure IS a hard failure — the probe is the whole module.
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (probe build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi
echo

# ── 2. Run the probe; capture per-leg lines ─────────────────────────
# The probe prints one "LEG <name>: PASS|FAIL <detail>" line per leg and exits
# non-zero if any leg failed. We tee its output, map each leg line to a pass/fail
# assertion, and require ALL six expected legs to be present (a missing leg is a
# failure, so a probe that silently drops a leg cannot pass).
blue "[run] driving sync-back handleUpdated/handleClosed through the real JiraSync + SSE stream"
PROBE_OUT="/tmp/jira-syncback-probe-${RUN_ID}.out"
probe_rc=0
"$PROBE_BIN" >"$PROBE_OUT" 2>/tmp/jira-syncback-probe-${RUN_ID}.err || probe_rc=$?

EXPECTED_LEGS="updated-deny-nonallowlisted closed-deny-nonallowlisted updated-deny-fsm closed-deny-fsm updated-dark-early-return closed-dark-early-return"
for leg in $EXPECTED_LEGS; do
  line=$(grep -E "^LEG ${leg}: " "$PROBE_OUT" 2>/dev/null | head -1 || true)
  if [ -z "$line" ]; then
    fail "leg $leg" "no result line emitted by the probe (leg dropped)"
    continue
  fi
  detail="${line#LEG ${leg}: }"
  case "$detail" in
    PASS*) pass "leg $leg — ${detail#PASS }" ;;
    FAIL*) fail "leg $leg" "${detail#FAIL }" ;;
    *)     fail "leg $leg" "unrecognized result: $detail" ;;
  esac
done

# Belt-and-suspenders: the probe's own exit code must agree with our parse. If
# the probe exited non-zero but every leg line parsed PASS (should be
# impossible), treat it as a failure so a probe-level error is never masked.
if [ "$probe_rc" -ne 0 ] && [ "$FAIL" -eq 0 ]; then
  fail "probe exit code" "probe exited $probe_rc but all parsed legs passed — probe-level error (stderr: $(tail -3 /tmp/jira-syncback-probe-${RUN_ID}.err 2>/dev/null))"
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed"
else
  red " ✗ $FAIL/$total assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  echo
  dim "  Probe output:"
  sed 's/^/    /' "$PROBE_OUT" 2>/dev/null || true
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
