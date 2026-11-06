#!/usr/bin/env bash
# discriminating-restore-check_test.sh — hermetic unit tests for the PURE core of the
# zero-canary on-node discriminating restore check (bug ka-0000000357 req-1; durable
# regression artifact, task ka-0000002792).
#
# WHAT THIS GATES
# ---------------
# discriminating-restore-check.sh's load-bearing OFFLINE logic — the verdict ladder
# (drc_verdict) and the DEFECT-A `.overlay-opts` parser (build_lowers) — extracted into
# a pure, sourceable core above the script's BASH_SOURCE guard. This test sources that
# core and drives it with synthetic measurements, asserting the FULL ladder the task
# names as the durable artifact:
#
#     PRE-FIX            => FAIL  (restore rc!=0 with the dir-medium sparse-lower
#                                  9p-walk-ENOENT signature — the gate discriminates)
#     skeleton-only      => HOLLOW(restore rc=0, CWD populated, but base /usr empty —
#                                  the cycle-14 NO-GO tripwire)
#     skeleton+stacking  => PASS  (restore rc=0, CWD populated, base binary+file present)
#
# plus the two remaining ladder rungs (real-loss FAIL, flaked-exec INCONCLUSIVE), the
# exit-code contract (0 ONLY for PASS), and build_lowers' parse of all three
# .overlay-opts shapes (freeze-written single token, multi-segment plural_fix stack,
# cp-failure-fallback raw live opts with an upperdir).
#
# Fully offline: NO runsc, mount, kubectl, node, beads daemon, or network. It NEVER
# runs the script's live main (the BASH_SOURCE guard returns on source). This is the
# repeatable durable-regression artifact wired into CI as a cluster-free GATE
# (checks.yml script-selftests) — a refactor that silently re-breaks the
# classification, the parser, or the PASS-only-exit-0 contract fails CI here, NOT five
# days later on a burned canary. The LIVE half (the count-gated barrier restore that
# PRODUCES these measurements) still requires the on-node operator run — see
# discriminating-restore-check.md "What CI covers vs. on-node".
#
# Usage:  bash scripts/gvisor/discriminating-restore-check_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/gvisor/discriminating-restore-check.sh
source "${SCRIPT_DIR}/discriminating-restore-check.sh"

PASS=0
FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

CWD="/home/agent/workspace"

# ----------------------------------------------------------------------------
# Harness: run drc_verdict, capture stdout + exit code, assert RESULT token +
# expected exit status. Args mirror drc_verdict's positional contract:
#   verdict_is <test-name> <expected-RESULT> <expected-rc> \
#              <RC> <SBRC> <WALKMISS> <CWDRC> <NENTRIES> <HOLLOWRC> <HOLLOW_BYTES>
# ----------------------------------------------------------------------------
verdict_is() {
  local name="$1" want_result="$2" want_rc="$3"; shift 3
  local rc args=("$1" "$2" "$3" "$CWD" "$4" "$5" "$6" "$7")
  local out
  out="$(drc_verdict "${args[@]}")"; rc=$?
  local got_result
  got_result="$(printf '%s\n' "$out" | sed -n 's/^RESULT=\([A-Z]*\).*/\1/p' | head -1)"
  if [ "$got_result" != "$want_result" ]; then
    fail "$name" "RESULT=$got_result (want $want_result); out: $(printf '%s' "$out" | head -1)"
    return
  fi
  if [ "$rc" != "$want_rc" ]; then
    fail "$name" "exit rc=$rc (want $want_rc) for RESULT=$got_result"
    return
  fi
  pass "$name (RESULT=$got_result rc=$rc)"
}

# Assert a substring appears in drc_verdict's stdout (so the operator-facing VERDICT
# guidance the ladder rungs carry can never silently drop).
verdict_says() {
  local name="$1" needle="$2"; shift 2
  local args=("$1" "$2" "$3" "$CWD" "$4" "$5" "$6" "$7")
  local out; out="$(drc_verdict "${args[@]}")"
  if grep -qF "$needle" <<<"$out"; then
    pass "$name (says '$needle')"
  else
    fail "$name" "expected substring '$needle' not in verdict output"
  fi
}

echo "── drc_verdict: the named PRE-FIX/skeleton-only/skeleton+stacking ladder ──"

# RUNG: skeleton+stacking => PASS. restore rc=0, CWD populated, base binary execs (0)
# and a base file byte read (>0). The ONLY combination that exits 0.
#         RC SBRC WALKMISS CWDRC NENTRIES HOLLOWRC HOLLOW_BYTES
verdict_is "skeleton+stacking => PASS" PASS 0   0 0 "" 0 42 0 1

# RUNG: skeleton-only => HOLLOW. restore rc=0, CWD walk passed + populated, but the
# base-binary exec ENOENT'd (rc=91) — empty /usr. The cycle-14 NO-GO tripwire.
verdict_is "skeleton-only (base exec 91) => HOLLOW" HOLLOW 1   0 0 "" 0 42 91 0
# HOLLOW also when the base binary execs (rc=0) but reads ZERO bytes from a base file
# (dir present, content absent) — the other half of the hollow catch.
verdict_is "skeleton-only (base file 0 bytes) => HOLLOW" HOLLOW 1   0 0 "" 0 42 0 0

# RUNG: PRE-FIX => FAIL. restore rc!=0 (the dir-medium sparse-lower rc=128) carrying
# the 9p-walk-ENOENT signature. EXPECTED on a pre-fix bundle; proves discrimination.
verdict_is "PRE-FIX sparse-lower (walk-ENOENT) => FAIL" FAIL 1 \
  128 0 'failed to walk "usr" of type 040000' 0 0 0 0
# A FILE-leaf miss (type 0100000, e.g. usr/bin/sleep) is ALSO the sparse-lower FAIL,
# not "no signature" — the empirical proof a skeleton-ONLY lower is insufficient.
verdict_is "PRE-FIX sparse-lower (file-leaf walk-ENOENT) => FAIL" FAIL 1 \
  128 0 'failed to walk "usr/bin/sleep" of type 0100000' 0 0 0 0

# RUNG: real workspace loss => FAIL. restore rc=0 and the base /usr layer is intact
# (HOLLOWRC=0, bytes>0 — NOT hollow), but the WORKSPACE upper specifically did not
# round-trip: CWD absent (CWDRC=1). The workspace upper and the base lowers are
# distinct overlay layers, so base-content-present + workspace-lost is exactly the
# dir-medium real-loss shape. (Base content MUST be present here, else the ladder
# classifies HOLLOW first — see the precedence test below.)
verdict_is "base intact but CWD absent => FAIL" FAIL 1   0 0 "" 1 0 0 1
# CWD present but EMPTY (entries==0), base intact, is also real loss.
verdict_is "base intact but CWD empty => FAIL"  FAIL 1   0 0 "" 0 0 0 1
# PRECEDENCE (load-bearing): when a hollow signal AND a CWD-loss signal BOTH hold,
# HOLLOW wins — the ladder checks ROOTFS_NOT_HOLLOW before the CWD-loss FAIL rung, so
# the cycle-14 NO-GO tripwire is never masked by a co-occurring CWD miss. Pin it so a
# refactor cannot silently reorder the elif chain.
verdict_is "hollow + CWD-absent both hold => HOLLOW wins" HOLLOW 1   0 0 "" 1 0 91 0

# RUNG: flaked in-sentry exec => INCONCLUSIVE (never PASS). CWD live + populated, but
# the base probe exec flaked (rc>=125: sentry not joinable / timeout), not 0 and not 91.
verdict_is "flaked base exec (rc=126) => INCONCLUSIVE" INCONCLUSIVE 1   0 0 "" 0 42 126 0
verdict_is "timed-out base exec (rc=124) => INCONCLUSIVE" INCONCLUSIVE 1   0 0 "" 0 42 124 0

echo "── drc_verdict: restore-failure sub-classification (signature vs none) ──"

# A restore failure WITHOUT a walk-ENOENT signature is still FAIL, but the verdict text
# must steer to capturing the runsc output (timeout/hang vs barrier-ordering), NOT claim
# the sparse-lower signature.
verdict_is "restore rc!=0, no walk signature => FAIL" FAIL 1   124 0 "" 0 0 0 0
verdict_says "no-signature FAIL steers to capture" "did NOT complete" \
  124 0 "" 0 0 0 0
verdict_says "walk-ENOENT FAIL names discrimination" "PROVES the gate discriminates" \
  128 0 'failed to walk "usr" of type 040000' 0 0 0 0
# A sandbox-only failure (agent rc=0 but SBRC!=0) is still a FAIL — the barrier needs
# BOTH halves to return 0.
verdict_is "sandbox restore failed (SBRC!=0) => FAIL" FAIL 1   0 1 "" 0 42 0 1

echo "── drc_verdict: the PASS gate is conjunctive (every condition required) ──"
# Each single missing condition must flip PASS to a non-PASS verdict — a guard against
# a refactor loosening the AND-chain into an OR.
verdict_is "PASS needs entries>0   (else FAIL)"   FAIL         1   0 0 "" 0 0  0 1
verdict_is "PASS needs base exec 0 (else HOLLOW)" HOLLOW       1   0 0 "" 0 42 91 1
verdict_is "PASS needs base bytes>0(else HOLLOW)" HOLLOW       1   0 0 "" 0 42 0 0
verdict_is "PASS needs CWDRC=0     (else FAIL)"   FAIL         1   0 0 "" 1 42 0 1

echo "── build_lowers: DEFECT-A .overlay-opts parser (all three shapes) ──"

lowers_is() {
  local name="$1" opts="$2" want="$3"
  local got; got="$(build_lowers "$opts")"
  if [ "$got" = "$want" ]; then
    pass "$name ('$got')"
  else
    fail "$name" "got '$got' want '$want'"
  fi
}

# Freeze-written form: a SINGLE `lowerdir=<seg>` token, NO upperdir, NO comma. The prior
# `sed 's/^[^:]*://'` left a literal `lowerdir=` in the value (DEFECT-A) — this asserts
# the token is stripped to the bare path.
lowers_is "freeze single-token lowerdir" \
  "lowerdir=/var/lib/gardenio/overlay/img" \
  "/var/lib/gardenio/overlay/img"

# plural_fix multi-segment stack: `lowerdir=$LOWER:img.0:img.1` (still no upperdir). The
# whole colon-joined segment list must survive verbatim.
lowers_is "plural_fix multi-segment lowerdir" \
  "lowerdir=/over/skel:/img/l.0:/img/l.1" \
  "/over/skel:/img/l.0:/img/l.1"

# cp-FAILURE fallback: the raw live opts carry rw flags + an upperdir; build_lowers must
# PREPEND the upper to the lowers (so written files win), matching thaw's own parser.
lowers_is "cp-failure-fallback raw opts (upper prepended)" \
  "rw,index=off,lowerdir=/a:/b,upperdir=/u,workdir=/w" \
  "/u:/a:/b"

# Order independence: upperdir BEFORE lowerdir in the token stream still prepends upper.
lowers_is "raw opts, upperdir-first ordering" \
  "rw,upperdir=/u,lowerdir=/a:/b,workdir=/w" \
  "/u:/a:/b"

# ----------------------------------------------------------------------------
echo
echo "──────────────────────────────────────────────"
echo "discriminating-restore-check_test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
