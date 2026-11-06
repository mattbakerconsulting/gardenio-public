#!/usr/bin/env bash
# test-advice-supersession.sh — E2E for advice supersession in gb prime
#
# Covers the advice supersession path (ka-0000007289, commit 42bdef419,
# controller/internal/advice/supersession.go). When a matched advice bead's
# description claims to supersede another matched bead via plain-English
# "supersedes gd-XXX" text, the superseded bead is dropped from gb prime
# output while the superseder itself still renders. Unrelated advice and
# dangling supersedes references are never collaterally dropped.
#
# Scenarios:
#   1. Baseline — a global advice bead renders in gb prime.
#   2. Supersession — a second global advice whose first line says
#      "supersedes kd-<OLD>" drops the OLD bead while the superseder renders.
#   3. Non-target safety — an unrelated advice still renders, and a superseder
#      pointing at a non-existent id is a harmless no-op (it renders itself
#      and drops nothing).
#
# This is the e2e companion to the unit coverage in
# controller/internal/advice/supersession_test.go.

set -euo pipefail

NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { red "  ✗ $1"; [ -n "${2:-}" ] && dim "    $2"; FAIL=$((FAIL + 1)); ERRORS+=("$1: ${2:-}"); }

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19098
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-advice-supersession.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

cleanup() {
  for id in "${CREATED[@]}"; do
    "$KD_BIN" delete "$id" 2>/dev/null || true
  done
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── Helpers ───────────────────────────────────────────────────────
# Create a global advice bead; echo its id (empty on failure).
#
# NOTE: this only echoes the id. Callers invoke it in a command substitution
# ($(...)), which is a SUBSHELL — an array append here would never reach the
# parent CREATED[] the EXIT trap deletes from, so the seed bead would leak into
# the daemon (invisible under CI's reaped ephemeral namespace, but a real leak
# against any persistent/shared daemon — gd-PMKgwAC1na). Every caller MUST
# `track "$id"` in the parent shell right after capturing the id.
mk_advice() {
  local title="$1" desc="$2"
  "$KD_BIN" create "$title" --type=advice -l global -d "$desc" --json 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo ""
}

# Register a bead id for deletion by the EXIT trap. Runs in the parent shell
# (never a subshell) so the append actually sticks.
track() { [ -n "${1:-}" ] && CREATED+=("$1"); }

# gb prime output for a throwaway project context.
prime() {
  "$GB_BIN" prime --project "$1" 2>/dev/null || echo ""
}

# ── Test body ─────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " advice supersession E2E"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TAG="e2e-supsed-$$-$(date +%s)"
  local PROJ="$TAG-proj"

  # ── Scenario 1: baseline — old advice renders before supersession ──
  blue "Scenario 1: a global advice renders in gb prime (baseline)"
  # Marker tokens deliberately use "SUPSED" (not "supersed") so they can never
  # be mistaken for the supersedes keyword by the detector.
  local old_id
  old_id=$(mk_advice "$TAG-old" "SUPSEDOLD-$TAG old advice slated for supersession")
  track "$old_id"
  if [ -z "$old_id" ]; then
    fail "create old advice" "no id"
    # Without the old bead the rest of the suite is meaningless.
    blue "═══════════════════════════════════════════════════"
    red " ✗ setup failed"
    return 1
  fi
  pass "old advice created ($old_id)"

  local out
  out=$(prime "$PROJ")
  if grep -qF "SUPSEDOLD-$TAG" <<<"$out"; then
    pass "old advice present in prime before supersession"
  else
    fail "baseline render" "SUPSEDOLD-$TAG not found in prime output"
  fi
  echo

  # ── Scenario 2: supersession drops the old bead, keeps superseder ──
  blue "Scenario 2: superseder drops old advice, renders itself"
  # First line carries the marker AND the plain-English supersedes clause.
  # No period/newline before the gd-id so the detector's sentence-bounded
  # window (see supersession.go) captures it.
  local new_id
  new_id=$(mk_advice "$TAG-new" \
    "SUPSEDNEW-$TAG supersedes $old_id replacing the old advice")
  track "$new_id"
  if [ -z "$new_id" ]; then
    fail "create superseder advice" "no id"
  else
    pass "superseder advice created ($new_id)"
  fi

  out=$(prime "$PROJ")
  if grep -qF "SUPSEDNEW-$TAG" <<<"$out"; then
    pass "superseder present in prime"
  else
    fail "superseder render" "SUPSEDNEW-$TAG not found in prime output"
  fi
  if grep -qF "SUPSEDOLD-$TAG" <<<"$out"; then
    fail "old advice dropped" "SUPSEDOLD-$TAG still present after supersession"
  else
    pass "old advice dropped from prime after supersession"
  fi
  echo

  # ── Scenario 3: non-target safety ──────────────────────────────────
  blue "Scenario 3: unrelated advice survives; dangling ref is a no-op"
  local keep_id dangle_id
  keep_id=$(mk_advice "$TAG-keep" "SUPSEDKEEP-$TAG unrelated advice that must always render")
  track "$keep_id"
  [ -n "$keep_id" ] && pass "unrelated advice created ($keep_id)" || fail "create unrelated advice" "no id"

  # Superseder pointing at a non-existent bead: must render itself and drop
  # nothing (the target is not in the matched set).
  dangle_id=$(mk_advice "$TAG-dangle" \
    "SUPSEDDANGLE-$TAG supersedes gd-DoesNotExist000 harmless dangling reference")
  track "$dangle_id"
  [ -n "$dangle_id" ] && pass "dangling-ref advice created ($dangle_id)" || fail "create dangling advice" "no id"

  out=$(prime "$PROJ")
  if grep -qF "SUPSEDKEEP-$TAG" <<<"$out"; then
    pass "unrelated advice not collaterally dropped"
  else
    fail "unrelated advice survives" "SUPSEDKEEP-$TAG missing from prime output"
  fi
  if grep -qF "SUPSEDDANGLE-$TAG" <<<"$out"; then
    pass "dangling-supersedes advice renders itself (no-op)"
  else
    fail "dangling-supersedes no-op" "SUPSEDDANGLE-$TAG missing from prime output"
  fi
  # The superseded bead must remain gone even with more advice in the mix.
  if grep -qF "SUPSEDOLD-$TAG" <<<"$out"; then
    fail "supersession stable" "SUPSEDOLD-$TAG reappeared after adding more advice"
  else
    pass "supersession stable across additional advice"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
