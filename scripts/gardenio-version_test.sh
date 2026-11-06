#!/usr/bin/env bash
# gardenio-version_test.sh — unit tests for gardenio-version.sh
#
# The interesting logic is the calver comparison, so that is what is tested:
# calver_days parsing (including the "dev" case that silently disabled the
# real skew signal — see ka-0000002965) and the report verdict/exit status
# against a stubbed daemon + stubbed kd/gb.
#
# Hermetic: kd/gb/curl are replaced by stubs on a temp PATH, so no daemon, no
# network, no token, no Go toolchain. --update is never exercised (it writes).
#
# Usage:  bash scripts/gardenio-version_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/gardenio-version.sh"

PASS=0; FAIL=0
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# ── calver_days: sourced directly out of the script ──────────────────────────
# Pull just the function body so the test does not run the whole script.
eval "$(sed -n '/^calver_days() {/,/^}/p' "$SUT")"

check_calver() {
  local in="$1" want="$2" got
  got="$(calver_days "$in")"
  [ "$got" = "$want" ] && pass "calver_days('$in') = '${want:-<empty>}'" \
    || fail "calver_days('$in')" "want '${want:-<empty>}', got '${got:-<empty>}'"
}

# Real calvers parse: year*366 + doy, matching buildinfo.calverDays.
# 2026*366 = 741516, so 2026.225 -> 741741 and 2026.100 -> 741616.
check_calver "2026.225.4" "741741"
check_calver "2026.225"   "741741"
check_calver "v2026.100"  "741616"
check_calver "2026.225-dirty" "741741"
# The load-bearing negative cases: an unstamped build must NOT parse, which is
# precisely why the in-CLI SkewCheck no-ops on onboard-mac.sh binaries.
check_calver "dev"        ""
check_calver ""           ""
check_calver "abc.def"    ""
check_calver "1999.100"   ""   # year out of range
check_calver "2026.400"   ""   # doy out of range
check_calver "2026"       ""   # no doy component

# ── report mode: stub kd/gb/curl, assert verdict + exit status ───────────────
# stub_env <kd_version> <gb_version> <daemon_version>
# Empty daemon_version stubs an unauthenticated/unreachable daemon (no header).
stub_env() {
  local kdv="$1" gbv="$2" dv="$3" dir="$TMP/bin"
  rm -rf "$dir"; mkdir -p "$dir"
  for b in kd gb; do
    local v; [ "$b" = kd ] && v="$kdv" || v="$gbv"
    if [ -n "$v" ]; then printf '#!/bin/sh\necho %s\n' "$v" > "$dir/$b"; chmod +x "$dir/$b"; fi
  done
  if [ -n "$dv" ]; then
    printf '#!/bin/sh\nprintf "HTTP/2 200\\r\\nx-kbeads-version: %s\\r\\n\\r\\n"\n' "$dv" > "$dir/curl"
  else
    printf '#!/bin/sh\nprintf "HTTP/2 401\\r\\n\\r\\n"\n' > "$dir/curl"
  fi
  chmod +x "$dir/curl"
  # A `go` that satisfies `command -v` but records any real invocation, so the
  # auto-update guard tests can assert no build was STARTED — not merely that a
  # refusal was printed. Nothing in this suite may compile or write to $HOME.
  printf '#!/bin/sh\necho "$@" >> "%s"\nexit 0\n' "$TMP/go-invoked" > "$dir/go"
  chmod +x "$dir/go"
  echo "$dir"
}

# assert_report <name> <kd> <gb> <daemon> <expected_exit> <expected_substring>
assert_report() {
  local name="$1" kdv="$2" gbv="$3" dv="$4" want_rc="$5" want_txt="$6"
  local dir out rc
  dir="$(stub_env "$kdv" "$gbv" "$dv")"
  out="$(PATH="$dir:/usr/bin:/bin" GARDENIO_TOKEN=stub-token \
         BEADS_HTTP_URL=https://stub.invalid/kd bash "$SUT" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" != "$want_rc" ]; then
    fail "$name" "expected exit $want_rc, got $rc. Output: $out"; return
  fi
  if grep -qi -- "$want_txt" <<<"$out"; then pass "$name"
  else fail "$name" "output missing '$want_txt'. Output: $out"; fi
}

D=2026.225.4   # daemon calver used throughout

# In sync → exit 0.
assert_report "in sync exits 0"          2026.225.4 2026.225.4 "$D" 0 "in sync"
# Behind but inside the 14d threshold → still exit 0.
assert_report "10d behind is tolerated"  2026.215   2026.215   "$D" 0 "within the"
# Materially behind → exit 1 and names the gap.
assert_report "125d behind exits 1"      2026.100   2026.100   "$D" 1 "days behind daemon"
# THE REGRESSION THIS WHOLE CHANGE IS ABOUT: an unstamped 'dev' binary must be
# reported as undetectable-skew and FAIL, not silently pass as it does in-CLI.
assert_report "dev build is flagged"     dev        dev        "$D" 1 "UNDETECTABLE"
# A missing binary is a hard error.
assert_report "missing kd exits 1"       ""         2026.225.4 "$D" 1 "kd not found"
# No daemon version (401/unreachable) is advisory, not a failure — the operator
# may legitimately be offline; the doctor covers auth/reachability.
assert_report "unknown daemon is advisory" 2026.225.4 2026.225.4 ""  0 "could not read the daemon version"
# A mismatched pair is called out even when the newer half looks fine.
assert_report "mismatched pair warned"   2026.225.4 2026.100   "$D" 1 "different builds"
# The verdict follows the OLDER binary regardless of argument order.
assert_report "older half drives verdict" 2026.100  2026.225.4 "$D" 1 "days behind daemon"

# ── quiet mode: one line, same exit semantics (for hooks/prompts) ────────────
qdir="$(stub_env dev dev "$D")"
qout="$(PATH="$qdir:/usr/bin:/bin" GARDENIO_TOKEN=stub BEADS_HTTP_URL=https://stub.invalid/kd \
        bash "$SUT" --quiet 2>&1)" && qrc=0 || qrc=$?
if [ "$qrc" = 1 ] && [ "$(printf '%s' "$qout" | wc -l)" -le 1 ] && grep -q unstamped <<<"$qout"
then pass "quiet mode: single line, exit 1 on unstamped"
else fail "quiet mode" "rc=$qrc out=$qout"; fi

qdir="$(stub_env 2026.225.4 2026.225.4 "$D")"
qout="$(PATH="$qdir:/usr/bin:/bin" GARDENIO_TOKEN=stub BEADS_HTTP_URL=https://stub.invalid/kd \
        bash "$SUT" --quiet 2>&1)" && qrc=0 || qrc=$?
if [ "$qrc" = 0 ] && grep -q current <<<"$qout"
then pass "quiet mode: exit 0 when current"
else fail "quiet mode current" "rc=$qrc out=$qout"; fi

# ── nudge mode: advisory-only, cached once per day ───────────────────────────
# It goes in a shell rc, so the contract is strict: never exit non-zero (that
# would break a login shell), print only when there is something to say, and
# cost the network at most once a day.
ndir="$(stub_env dev dev "$D")"
run_nudge() {
  PATH="$ndir:/usr/bin:/bin" XDG_CACHE_HOME="$1" GARDENIO_TOKEN=stub \
    BEADS_HTTP_URL=https://stub.invalid/kd bash "$SUT" --nudge 2>&1
}
CACHE="$TMP/cache"; rm -rf "$CACHE"
n1="$(run_nudge "$CACHE")"; nrc1=$?
n2="$(run_nudge "$CACHE")"; nrc2=$?
[ "$nrc1" = 0 ] && [ "$nrc2" = 0 ] \
  && pass "nudge always exits 0 (safe in a shell rc)" \
  || fail "nudge exit" "got $nrc1 / $nrc2"
grep -q update <<<"$n1" \
  && pass "nudge warns on first run when skewed" \
  || fail "nudge first run" "no warning: $n1"
[ -z "$n2" ] \
  && pass "nudge is silent on the second run same day (cached)" \
  || fail "nudge cache" "expected silence, got: $n2"

# Current binaries must produce no output at all, even with a cold cache.
ndir="$(stub_env 2026.225.4 2026.225.4 "$D")"
rm -rf "$CACHE"
n3="$(run_nudge "$CACHE")"
[ -z "$n3" ] && pass "nudge is silent when binaries are current" || fail "nudge current" "got: $n3"

# ── nudge auto-update: the ka-0000000468 ruling ──────────────────────────────
# Skew now triggers a real build rather than advice. Every guard below decides
# NOT to build, which is what makes these hermetic — the one path that would
# invoke `go` is covered by the live --update proof in the commit message, not
# here (this suite must not compile anything or write to $HOME).
#
# auto_nudge <cache> [env assignments...] -> stdout+stderr of a skewed nudge
# GARDENIO_BIN_DIR points at the stub dir so kd/gb resolve to $BIN and the
# PATH-shadow guard is satisfied; each case then breaks exactly one precondition.
ndir="$(stub_env dev dev "$D")"
auto_nudge() {
  local cache="$1"; shift
  rm -rf "$cache"
  env PATH="$ndir:/usr/bin:/bin" XDG_CACHE_HOME="$cache" GARDENIO_TOKEN=stub \
      BEADS_HTTP_URL=https://stub.invalid/kd GARDENIO_BIN_DIR="$ndir" \
      GARDENIO_REPO="$TMP/norepo" "$@" bash "$SUT" --nudge 2>&1
}

# The off switch must be honoured, and must say so rather than going quiet.
a1="$(auto_nudge "$TMP/c1" GARDENIO_NO_AUTO_UPDATE=1)"
if grep -q "behind\|unstamped" <<<"$a1" && grep -q "GARDENIO_NO_AUTO_UPDATE" <<<"$a1"
then pass "nudge: GARDENIO_NO_AUTO_UPDATE falls back to advising, and says why"
else fail "nudge off switch" "got: $a1"; fi

# A missing/!git checkout must degrade to advice, never to a half-run build.
a2="$(auto_nudge "$TMP/c2")"
if grep -q "not auto-updating" <<<"$a2" && grep -q "no gardenio checkout" <<<"$a2"
then pass "nudge: no checkout falls back to advising"
else fail "nudge no checkout" "got: $a2"; fi

# A dirty tree is the operator's work in progress — auto-building it unattended
# would install their WIP as the kd everything else on the machine then runs.
DIRTY="$TMP/dirty"
mkdir -p "$DIRTY/kbeads" "$DIRTY/gardenio/controller"
git -C "$DIRTY" init -q 2>/dev/null
git -C "$DIRTY" config user.email t@t && git -C "$DIRTY" config user.name t
: > "$DIRTY/kbeads/keep"; git -C "$DIRTY" add -A 2>/dev/null
git -C "$DIRTY" commit -qm init 2>/dev/null
git -C "$DIRTY" branch -M main 2>/dev/null
: > "$DIRTY/uncommitted"
a3="$(auto_nudge "$TMP/c3" GARDENIO_REPO="$DIRTY")"
if grep -q "uncommitted changes" <<<"$a3"
then pass "nudge: dirty checkout falls back to advising"
else fail "nudge dirty" "got: $a3"; fi

# Same for a checkout parked on a feature branch: pulling main is not what that
# operator asked for, and building the branch is not an "update" either.
git -C "$DIRTY" checkout -q -b feat/x 2>/dev/null; rm -f "$DIRTY/uncommitted"
a4="$(auto_nudge "$TMP/c4" GARDENIO_REPO="$DIRTY")"
if grep -q "not on main" <<<"$a4"
then pass "nudge: off-main checkout falls back to advising"
else fail "nudge off-main" "got: $a4"; fi
git -C "$DIRTY" checkout -q main 2>/dev/null

# PATH shadowing: installing into $BIN cannot help if kd resolves elsewhere, and
# the skew would never clear — so this must refuse rather than rebuild daily.
SHADOW="$TMP/shadow"; mkdir -p "$SHADOW"
a5="$(auto_nudge "$TMP/c5" GARDENIO_REPO="$DIRTY" GARDENIO_BIN_DIR="$SHADOW")"
if grep -q "would not change it" <<<"$a5"
then pass "nudge: PATH-shadowed binaries refuse to auto-update"
else fail "nudge shadow" "got: $a5"; fi

# A failed background update must reach a human PROMPTLY — i.e. before the
# once-a-day gate, and exactly once.
FCACHE="$TMP/failcache"; rm -rf "$FCACHE"; mkdir -p "$FCACHE/gardenio"
: > "$FCACHE/gardenio/update-failed"; : > "$FCACHE/gardenio/version-check"   # gate CLOSED
f1="$(PATH="$ndir:/usr/bin:/bin" XDG_CACHE_HOME="$FCACHE" bash "$SUT" --nudge 2>&1)"; frc=$?
f2="$(PATH="$ndir:/usr/bin:/bin" XDG_CACHE_HOME="$FCACHE" bash "$SUT" --nudge 2>&1)"
if [ "$frc" = 0 ] && grep -q "update failed" <<<"$f1" && [ -z "$f2" ]
then pass "nudge: reports a failed background update once, even inside the daily cache"
else fail "nudge failure marker" "rc=$frc first='$f1' second='$f2'"; fi

# Concurrency: two shells opening at once must not both start a build.
LCACHE="$TMP/lockcache"; rm -rf "$LCACHE"; mkdir -p "$LCACHE/gardenio/update.lock"
l1="$(PATH="$ndir:/usr/bin:/bin" XDG_CACHE_HOME="$LCACHE" \
      GARDENIO_TOKEN=stub BEADS_HTTP_URL=https://stub.invalid/kd GARDENIO_BIN_DIR="$ndir" \
      GARDENIO_REPO="$DIRTY" bash "$SUT" --nudge 2>&1)"
if ! grep -q "updating in the background" <<<"$l1"
then pass "nudge: a held lock stops a second shell from starting a rival build"
else fail "nudge lock" "second shell started a build anyway: $l1"; fi

# The load-bearing assertion for every guard above: none of them reached `go`.
# Without this the tests would pass on a script that printed a refusal and then
# built anyway.
[ ! -f "$TMP/go-invoked" ] \
  && pass "nudge: no guarded case ever invoked the Go toolchain" \
  || fail "nudge guards" "a build was started: $(cat "$TMP/go-invoked")"

# ── bash -n + --help ────────────────────────────────────────────────────────
bash -n "$SUT" 2>/dev/null && pass "syntax: gardenio-version.sh" || fail "syntax" "bash -n failed"
bash "$SUT" --help >/dev/null 2>&1 && pass "--help exits 0" || fail "--help" "non-zero exit"
bash "$SUT" --bogus >/dev/null 2>&1; [ $? = 2 ] && pass "unknown flag exits 2" || fail "unknown flag" "wrong exit"

echo
echo "passed: $PASS  failed: $FAIL"
[ "$FAIL" -eq 0 ] || exit 1
