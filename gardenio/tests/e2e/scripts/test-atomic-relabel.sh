#!/usr/bin/env bash
# test-atomic-relabel.sh — E2E for the atomic label MOVE verbs against a live daemon.
#
# Covers `kd label set`, `kd label replace`, and `gb advice relabel` — the verbs
# added by bug ka-0000003820 to make a label move ONE atomic PATCH instead of a
# non-atomic `kd label add` + `kd label remove` pair. The reason the pair was
# dangerous is the reason these tests exist: if the second call failed or was
# forgotten the bead was left carrying BOTH the old and the new label —
# double-scoped, which for advice silently WIDENS delivery (ka-0000006660).
#
# The correctness fence is that SetLabels is a FULL REPLACE, not a merge
# (beadsapi/client_labels.go): after a set/relabel the bead's label set is
# EXACTLY the target, so any label not in the target is GONE. A merge regression
# would leave the old labels behind while still adding the new one — so a test
# that only checked "new label present" would pass under the very bug we guard
# against (the vacuity trap called out on the work order). Every assertion here
# is therefore made NEGATIVE as well as positive: the old labels must be ABSENT,
# asserted both by exact-set equality (full replace ⇒ result == target) and by
# an explicit absence check on the dropped labels.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-atomic-relabel.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (or BEADS_HTTP_URL set)
#   - kd and gb binaries in PATH (or set KD_BIN / GB_BIN)
#   - jq and python3 installed
#
# Scenarios tested:
#   1. kd label set is a FULL REPLACE — old labels are gone, not merged
#   2. kd label set --dry-run writes NOTHING
#   3. kd label set refuses to strip every label (empty-set guard)
#   4. kd label replace swaps ONE label, keeping the rest
#   5. kd label replace of an ABSENT old label fails and writes nothing (typo guard)
#   6. gb advice relabel --scope swaps the scope: label (full replace on advice)
#   7. gb advice relabel refuses a non-advice bead without --force

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()
CREATED_ADVICE=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# Load shared retry helpers (kd_retry) to absorb transient PF drops.
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_exit() {
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

# assert_exit_nonzero — the MOVE verbs' guards (typo'd label, empty set, wrong
# bead type) are only useful if they FAIL loudly; a `... || fallback` that never
# fires is the whole class of bug these verbs replaced. So assert non-zero, not a
# specific code, since cobra arg-validation and RunE errors both exit 1.
assert_exit_nonzero() {
  local name="$1" actual="$2"
  if [ "$actual" -ne 0 ]; then
    pass "$name (exit $actual, non-zero as required)"
  else
    fail "$name" "expected non-zero exit, got 0 — a silent no-op reads as success"
  fi
}

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

# labels_of prints a bead's labels as a sorted, comma-joined string, so an exact
# set can be compared regardless of the order the daemon returns them in.
labels_of() {
  local id="$1"
  kd_retry "$KD_BIN" show "$id" --json 2>/dev/null \
    | python3 -c "import sys,json; print(','.join(sorted(json.load(sys.stdin).get('labels') or [])))" 2>/dev/null \
    || echo "__ERR__"
}

# assert_labels_exact is the anti-merge assertion in its strongest form: because
# set/relabel is a FULL REPLACE, the resulting label set must equal the target
# EXACTLY. This single check proves the new labels present AND every old label
# gone — a merge regression (old labels retained) fails it immediately.
assert_labels_exact() {
  local name="$1" id="$2" want="$3"   # want = comma-joined SORTED expected set
  local got
  got=$(labels_of "$id")
  if [ "$got" = "$want" ]; then
    pass "$name (labels == {$want})"
  else
    fail "$name" "labels are {$got}; want exactly {$want}"
  fi
}

# assert_label_absent makes the negative assertion legible on its own: the old
# label the move was supposed to drop must NOT be present. Called alongside the
# exact-set check so the test reads as "and specifically, X is gone".
assert_label_absent() {
  local name="$1" id="$2" label="$3"
  local got
  got=$(labels_of "$id")
  if grep -qE "(^|,)${label}(,|$)" <<< "$got"; then
    fail "$name" "label '$label' is still present — a MERGE left it behind: {$got}"
  else
    pass "$name ('$label' absent)"
  fi
}

# create_task_bead creates a task bead carrying EXACTLY the given labels and
# echoes its id. `--project ""` suppresses the implicit `project:$BOAT_PROJECT`
# label `kd create` otherwise appends (create.go defaults --project to
# $BOAT_PROJECT), which would otherwise leak into the exact-set assertions and
# make them depend on the runner's environment.
create_task_bead() {
  local title="$1"; shift
  local args=()
  local l
  for l in "$@"; do args+=(-l "$l"); done
  local out
  out=$(kd_retry "$KD_BIN" create "$title" --type=task --project "" "${args[@]}" --json 2>/dev/null) || return 1
  echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19118
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-relabel.log 2>&1 &
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
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_ADVICE[@]}"; do
    [ -n "$id" ] || continue
    "$GB_BIN" advice remove "$id" --hard 2>/dev/null || "$KD_BIN" delete "$id" 2>/dev/null || true
  done
  for id in "${CREATED_BEADS[@]}"; do
    [ -n "$id" ] || continue
    kd_retry "$KD_BIN" delete "$id" 2>/dev/null || kd_retry "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " atomic-relabel E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! kd_retry "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TAG="e2e-relabel-$$-$(date +%s)"
  dim "  Test tag: $TAG"
  echo

  # ── Scenario 1: kd label set is a FULL REPLACE (not a merge) ────────
  # The headline correctness fence. Bead starts with {A,B}; set to {C}. The
  # result must be EXACTLY {C}: A and B are dropped, not kept alongside C. A
  # test that only checked "C present" would pass under a merge regression —
  # the vacuity trap — so we assert exact-set equality AND explicit absence.
  blue "Scenario 1: kd label set — full replace, old labels GONE"
  local sA="${TAG}-a" sB="${TAG}-b" sC="${TAG}-c"
  local bid1
  bid1=$(create_task_bead "atomic-relabel set $TAG" "$sA" "$sB")
  if [ -z "$bid1" ]; then
    fail "scenario-1 setup" "could not create bead"
  else
    CREATED_BEADS+=("$bid1")
    dim "  bead $bid1 before: {$(labels_of "$bid1")}"
    local set_out set_rc
    set_out=$(kd_retry "$KD_BIN" label set "$bid1" "$sC" 2>&1) && set_rc=0 || set_rc=$?
    assert_exit "kd label set exits 0" 0 "$set_rc"
    assert_contains "set reports the removals" "removed:" "$set_out"
    assert_labels_exact "set produced EXACTLY the target set" "$bid1" "$sC"
    assert_label_absent "old label A dropped by set" "$bid1" "$sA"
    assert_label_absent "old label B dropped by set" "$bid1" "$sB"
  fi
  echo

  # ── Scenario 2: kd label set --dry-run writes nothing ───────────────
  # A preview must not mutate — otherwise "preview" is a footgun that silently
  # strips labels while the operator is still deciding.
  blue "Scenario 2: kd label set --dry-run does not write"
  if [ -n "${bid1:-}" ]; then
    local before2 after2 dry_out dry_rc
    before2=$(labels_of "$bid1")
    dry_out=$(kd_retry "$KD_BIN" label set "$bid1" "${TAG}-dryonly" --dry-run 2>&1) && dry_rc=0 || dry_rc=$?
    assert_exit "kd label set --dry-run exits 0" 0 "$dry_rc"
    assert_contains "dry-run announces itself" "DRY RUN" "$dry_out"
    after2=$(labels_of "$bid1")
    if [ "$before2" = "$after2" ]; then
      pass "dry-run left labels unchanged ({$after2})"
    else
      fail "dry-run mutated labels" "before {$before2} != after {$after2}"
    fi
  fi
  echo

  # ── Scenario 3: kd label set refuses to strip EVERY label ───────────
  # The strip-all guard (applyLabelSet): an empty target — an unquoted glob or
  # an empty shell variable — would drop the bead's project:/targeting labels
  # out of every scoped query. It must refuse and change nothing.
  blue "Scenario 3: kd label set empty-set guard (accidental strip-all)"
  if [ -n "${bid1:-}" ]; then
    local before3 after3 empty_out empty_rc
    before3=$(labels_of "$bid1")
    # An empty-string arg filters to a zero-length set, exercising the guard
    # (a bare zero-arg call is caught earlier by cobra's MinimumNArgs).
    empty_out=$(kd_retry "$KD_BIN" label set "$bid1" "" 2>&1) && empty_rc=0 || empty_rc=$?
    assert_exit_nonzero "kd label set '' is refused" "$empty_rc"
    assert_contains "refusal explains the strip-all guard" "refusing to remove every label" "$empty_out"
    after3=$(labels_of "$bid1")
    if [ "$before3" = "$after3" ]; then
      pass "refused strip-all left labels intact ({$after3})"
    else
      fail "strip-all guard did not protect labels" "before {$before3} != after {$after3}"
    fi
  fi
  echo

  # ── Scenario 4: kd label replace swaps ONE label, keeps the rest ────
  # replace's reason to exist over set: the caller names only the label being
  # moved and everything else survives, so nothing is dropped by omission.
  blue "Scenario 4: kd label replace — swap one, keep the others"
  local rKeep1="${TAG}-keep1" rKeep2="${TAG}-keep2" rOld="${TAG}-old" rNew="${TAG}-new"
  local bid2
  bid2=$(create_task_bead "atomic-relabel replace $TAG" "$rKeep1" "$rKeep2" "$rOld")
  if [ -z "$bid2" ]; then
    fail "scenario-4 setup" "could not create bead"
  else
    CREATED_BEADS+=("$bid2")
    dim "  bead $bid2 before: {$(labels_of "$bid2")}"
    local rep_out rep_rc
    rep_out=$(kd_retry "$KD_BIN" label replace "$bid2" "$rOld" "$rNew" 2>&1) && rep_rc=0 || rep_rc=$?
    assert_exit "kd label replace exits 0" 0 "$rep_rc"
    assert_contains "replace reports removing the old label" "removed: -$rOld" "$rep_out"
    # Sorted expected set: the two keeps plus the new label; old label gone.
    local want4
    want4=$(printf '%s\n%s\n%s\n' "$rKeep1" "$rKeep2" "$rNew" | sort | paste -sd, -)
    assert_labels_exact "replace kept the others and swapped in new" "$bid2" "$want4"
    assert_label_absent "replaced old label is gone" "$bid2" "$rOld"
  fi
  echo

  # ── Scenario 5: replace of an ABSENT old label fails, writes nothing ─
  # The typo guard (ka-0000000424 applied to replace): swapping a label the bead
  # never carried must fail loudly and mutate NOTHING, so a misspelled name
  # cannot read as a completed move.
  blue "Scenario 5: kd label replace of absent label — loud failure, no write"
  if [ -n "${bid2:-}" ]; then
    local before5 after5 miss_out miss_rc
    before5=$(labels_of "$bid2")
    miss_out=$(kd_retry "$KD_BIN" label replace "$bid2" "${TAG}-nope" "${TAG}-should-not-appear" 2>&1) && miss_rc=0 || miss_rc=$?
    assert_exit_nonzero "replace of absent label is refused" "$miss_rc"
    assert_contains "refusal names the missing label" "no such label" "$miss_out"
    after5=$(labels_of "$bid2")
    if [ "$before5" = "$after5" ]; then
      pass "failed replace mutated nothing ({$after5})"
    else
      fail "failed replace still mutated" "before {$before5} != after {$after5}"
    fi
    assert_label_absent "the would-be new label was never added" "$bid2" "${TAG}-should-not-appear"
  fi
  echo

  # ── Scenario 6: gb advice relabel --scope swaps the scope: label ────
  # The advice-specific sibling, and the highest-stakes case: advice targeting
  # is label-driven, so a MERGE bug that keeps scope:project-broadcast while
  # adding scope:on-demand silently changes delivery (ka-0000006660). Assert the
  # OLD scope is gone (exact set) and the non-scope targeting labels survive.
  blue "Scenario 6: gb advice relabel --scope — full replace on an advice bead"
  local aProj="project:gardenio" aRole="role:crew"
  local aid adv_out
  adv_out=$("$GB_BIN" advice add "E2E advice relabel $TAG: verify atomic scope swap" \
    -l "$aProj" -l "$aRole" -l "scope:project-broadcast" --json 2>/dev/null || echo "")
  aid=$(echo "$adv_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -z "$aid" ]; then
    # Fallback: create the advice bead directly if gb advice add --json gave no
    # id (older gb builds omit it — the same fallback test-config-advice.sh uses).
    local kd_adv
    kd_adv=$(kd_retry "$KD_BIN" create "E2E advice relabel $TAG" --type=advice \
      -l "$aProj" -l "$aRole" -l "scope:project-broadcast" --json 2>/dev/null || echo "{}")
    aid=$(echo "$kd_adv" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  fi
  if [ -z "$aid" ]; then
    fail "scenario-6 setup" "could not create advice bead"
  else
    CREATED_ADVICE+=("$aid")
    dim "  advice $aid before: {$(labels_of "$aid")}"
    local rl_out rl_rc
    rl_out=$("$GB_BIN" advice relabel "$aid" --scope on-demand 2>&1) && rl_rc=0 || rl_rc=$?
    assert_exit "gb advice relabel --scope exits 0" 0 "$rl_rc"
    assert_contains "relabel reports removing the old scope" "scope:project-broadcast" "$rl_out"
    local want6
    want6=$(printf '%s\n%s\n%s\n' "$aProj" "$aRole" "scope:on-demand" | sort | paste -sd, -)
    assert_labels_exact "relabel swapped scope, kept project+role" "$aid" "$want6"
    assert_label_absent "old scope:project-broadcast is gone" "$aid" "scope:project-broadcast"
  fi
  echo

  # ── Scenario 7: gb advice relabel refuses a NON-advice bead ─────────
  # relabel REPLACES the whole label set; on a non-advice bead (agent/trigger/
  # project) that can clobber labels other machinery owns. Without --force it
  # must refuse and leave the bead untouched.
  blue "Scenario 7: gb advice relabel non-advice guard"
  local bid7
  bid7=$(create_task_bead "atomic-relabel guard $TAG" "${TAG}-g1" "${TAG}-g2")
  if [ -z "$bid7" ]; then
    fail "scenario-7 setup" "could not create task bead"
  else
    CREATED_BEADS+=("$bid7")
    local before7 after7 guard_out guard_rc
    before7=$(labels_of "$bid7")
    guard_out=$("$GB_BIN" advice relabel "$bid7" "role:crew" 2>&1) && guard_rc=0 || guard_rc=$?
    assert_exit_nonzero "relabel of a task bead is refused" "$guard_rc"
    assert_contains "refusal explains the type guard" "not advice" "$guard_out"
    after7=$(labels_of "$bid7")
    if [ "$before7" = "$after7" ]; then
      pass "refused relabel left the task bead's labels intact ({$after7})"
    else
      fail "non-advice guard did not protect labels" "before {$before7} != after {$after7}"
    fi
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
