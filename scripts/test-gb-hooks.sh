#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Smoke-test the gb Claude Code hooks (the prime/stop-gate/etc. wiring that
# scripts/claude-hooks.json installs). Proves the onboarded setup actually
# *functions*, not just that kd/gb reach the daemon.
#
# Asserts (against the live daemon, using the operator env):
#   1. gb hook prime       -> exit 0, emits a <system-reminder> preamble
#   2. gb hook stop-gate    -> exit 0 (ALLOW) when no agent / nothing pending
#   3. gb hook stop-gate    -> exit 2 (BLOCK) for an agent with an unsatisfied
#                              decision gate, with a {"decision":"block"} verdict
#   4. gb hook check-mail   -> exit 0
#   5. gb hook load-state   -> exit 0
#
# Requires: gb on PATH and authed (GARDENIO_TOKEN + BEADS_HTTP_URL + BEADS_OPERATED_BY).
# Usage:    ./scripts/test-gb-hooks.sh
#
# Notes: uses STOP_GATE_COOLDOWN_SECS=0 and cleans the shared /tmp/stop-gate-*
# files so it won't perturb a concurrent interactive session. The BLOCK case
# uses a synthetic agent id; it leaves one inert gate row keyed to a non-existent
# agent (harmless — nothing will ever Stop as that id again).
# ─────────────────────────────────────────────────────────────────────────────
set -uo pipefail

command -v gb >/dev/null || { echo "FAIL: gb not on PATH"; exit 1; }
[ -n "${GARDENIO_TOKEN:-}" ] || { echo "FAIL: GARDENIO_TOKEN unset (run the operator env block)"; exit 1; }
export STOP_GATE_COOLDOWN_SECS=0
SE='{"session_id":"hook-smoke","cwd":"/tmp","hook_event_name":"Stop"}'
fails=0
pass() { printf '\033[1;32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*"; fails=$((fails+1)); }

# 1. prime
out="$(gb hook prime 2>/dev/null)"; rc=$?
{ [ $rc -eq 0 ] && grep -qi 'system-reminder' <<<"$out"; } \
  && pass "prime: exit 0 + system-reminder (${#out} bytes)" \
  || fail "prime: rc=$rc, ${#out} bytes (expected exit 0 + <system-reminder>)"

# 2. stop-gate ALLOW (no agent context, nothing pending)
printf '%s' "$SE" | gb hook stop-gate >/dev/null 2>&1; rc=$?
[ $rc -eq 0 ] && pass "stop-gate ALLOW: exit 0 with no pending gate" \
              || fail "stop-gate ALLOW: exit $rc (expected 0)"

# 3. stop-gate BLOCK (synthetic agent => unsatisfied decision gate)
err="$(printf '%s' "$SE" | KD_AGENT_ID="ka-hooksmoke-$$" gb hook stop-gate 2>&1 1>/dev/null)"; rc=$?
{ [ $rc -eq 2 ] && grep -q '"decision":"block"' <<<"$err"; } \
  && pass "stop-gate BLOCK: exit 2 + {\"decision\":\"block\"} on unsatisfied gate" \
  || fail "stop-gate BLOCK: exit $rc (expected 2 + block verdict)"

# 4. check-mail
gb hook check-mail >/dev/null 2>&1 && pass "check-mail: exit 0" || fail "check-mail: nonzero exit"

# 5. load-state
gb hook load-state >/dev/null 2>&1 && pass "load-state: exit 0" || fail "load-state: nonzero exit"

rm -f /tmp/stop-gate-last-block /tmp/stop-gate-block-count
echo
[ $fails -eq 0 ] && { pass "all gb hooks functional"; exit 0; } || { fail "$fails hook check(s) failed"; exit 1; }
