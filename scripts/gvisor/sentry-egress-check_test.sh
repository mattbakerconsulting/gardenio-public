#!/usr/bin/env bash
# sentry-egress-check_test.sh — hermetic unit tests for the PURE core of the
# zero-canary on-node sentry-egress check (bug ka-0000000514, task ka-0000008207).
#
# WHAT THIS GATES
# ---------------
# sentry-egress-check.sh's load-bearing OFFLINE logic — the verdict ladder
# (seg_verdict) and the per-dial classifier (seg_dial_verdict) — which mirror the
# controller thaw [6c] in-sentry egress assertion (freezethaw_node.go). This test
# SOURCES that pure core and drives it with synthetic measurements, asserting the
# FULL ladder the task names as the durable artifact:
#
#   routable sentry + routable dials => PASS  (safe to canary)
#   no default route / no non-lo iface => FAIL (the prod-.13 link-only bug)
#   in-sentry dial network-unreachable => FAIL (the exact coop+gb 180s errno)
#   flaked in-sentry exec (unknown/-1)  => INCONCLUSIVE (never PASS, suggest redrive)
#   refused/timeout/dns dial            => routable (a service/name blip is NOT
#                                          link-only — must never NO-GO a good fix)
#
# Fully offline: NO runsc, mount, kubectl, node, NATS, beads daemon, or network. It
# NEVER runs the script's live main (the BASH_SOURCE guard returns on source). This
# is the repeatable durable-regression artifact: a refactor that silently re-breaks
# the link-only classification or the dial-errno discrimination fails CI HERE, not
# days later on a burned canary. The LIVE half (the count-gated barrier restore that
# PRODUCES these measurements) still requires the on-node operator run.
#
# Usage:  bash scripts/gvisor/sentry-egress-check_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/gvisor/sentry-egress-check.sh
source "${SCRIPT_DIR}/sentry-egress-check.sh"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ok: $1"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL: $1"; }

# assert_verdict <name> <want_rc> <want_result> -- <DEFROUTE> <NONLO> <NATS> <BEADS>
assert_verdict() {
  local name="$1" want_rc="$2" want_result="$3"; shift 3
  [ "$1" = "--" ] && shift
  local out rc
  out=$(seg_verdict "$1" "$2" "$3" "$4"); rc=$?
  if [ "$rc" -ne "$want_rc" ]; then
    bad "$name: rc=$rc want $want_rc :: $out"; return
  fi
  if ! grep -q "RESULT=$want_result" <<<"$out"; then
    bad "$name: RESULT not $want_result :: $out"; return
  fi
  ok "$name (rc=$rc RESULT=$want_result)"
}

echo "== seg_verdict ladder =="
# PASS: routable sentry, dials routable (ok / refused / timeout / skip all routable).
assert_verdict "routable + both dial ok"        0 PASS         -- yes 1 ok ok
assert_verdict "routable + dials skipped"        0 PASS         -- yes 1 skip skip
assert_verdict "routable + refused is routable"  0 PASS         -- yes 2 refused refused
assert_verdict "routable + timeout is routable"  0 PASS         -- yes 1 timeout ok
assert_verdict "routable + dns is routable"      0 PASS         -- yes 1 dns skip

# FAIL: the prod-.13 link-only signatures.
assert_verdict "no default route"                1 FAIL         -- no 1 skip skip
assert_verdict "no non-lo iface"                 1 FAIL         -- yes 0 skip skip
assert_verdict "nats dial network-unreachable"   1 FAIL         -- yes 1 netunreach ok
assert_verdict "beads dial host-unreachable"     1 FAIL         -- yes 1 ok hostunreach
assert_verdict "dial netdown"                    1 FAIL         -- yes 1 netdown skip

# INCONCLUSIVE: a flaked in-sentry exec — never PASS, distinct from a real FAIL.
assert_verdict "flaked route read (unknown)"     2 INCONCLUSIVE -- unknown 1 skip skip
assert_verdict "flaked iface read (-1)"          2 INCONCLUSIVE -- yes -1 skip skip

# A link-only dial OUTRANKS an otherwise-routable sentry read: even with a default
# route, a network-unreachable dial means the sentry cannot actually egress (host-side
# veth severed) — the exact second-layer the [6c-2] dial catches. Must be FAIL.
assert_verdict "route ok but dial no-routes"     1 FAIL         -- yes 1 ok netunreach

echo "== seg_dial_verdict classifier =="
assert_dial() { # <name> <raw> <want>
  local got; got=$(seg_dial_verdict "$2")
  if [ "$got" = "$3" ]; then ok "dial $1 ($2 -> $got)"; else bad "dial $1: got $got want $3"; fi
}
assert_dial "ok routable"          ok         routable
assert_dial "refused routable"     refused    routable
assert_dial "timeout routable"     timeout    routable
assert_dial "dns routable"         dns        routable
assert_dial "other routable"       other      routable
assert_dial "netunreach linkonly"  netunreach link-only
assert_dial "hostunreach linkonly" hostunreach link-only
assert_dial "netdown linkonly"     netdown    link-only
assert_dial "empty skip"           ""         skip
assert_dial "skip skip"            skip       skip

echo "== exit-code contract: PASS returns 0, everything else non-zero =="
if seg_verdict yes 1 ok ok >/dev/null; then ok "PASS => exit 0"; else bad "PASS did not exit 0"; fi
if seg_verdict no 1 ok ok >/dev/null; then bad "FAIL exited 0"; else ok "FAIL => exit non-zero"; fi

echo
echo "RESULTS: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
echo "ALL PASS — sentry-egress-check.sh verdict core is sound (offline)."
