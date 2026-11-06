#!/usr/bin/env bash
# SC2317: the fake_runner_* / *_step helpers are invoked INDIRECTLY (passed by
#         name to the lib functions), which shellcheck can't trace — the
#         "unreachable" reports are false positives.
# shellcheck disable=SC2317
# test-lib-gvisor-node.sh — cluster-free unit test for lib-gvisor-node.sh.
#
# Exercises gvisor_retry_step (bounded signature-keyed retry) and
# gvisor_warm_node_gate (bounded warm-node readiness poll) using a FAKE node
# runner — no kubectl / cluster needed. Runs in the lint job or on a laptop.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Fast: no real sleeping between attempts.
export GVISOR_WARM_DELAY=0
export GVISOR_RETRY_DELAY=0
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gvisor-node.sh"

PASS=0
FAIL=0
ERRORS=()
pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); ERRORS+=("$1"); echo "  FAIL: $1"; }

# ── gvisor_retry_step: success on first try (1 attempt consumed) ──────
if gvisor_retry_step demo 4 0 true; then
  if [ "$GVISOR_STEP_ATTEMPTS" -eq 1 ]; then
    pass "retry_step: true succeeds on first try (attempts=1)"
  else
    fail "retry_step: expected 1 attempt, got $GVISOR_STEP_ATTEMPTS"
  fi
else
  fail "retry_step: true should succeed"
fi

# ── gvisor_retry_step: hard failure exhausts the budget, sets diag ────
if gvisor_retry_step bad-sig 3 0 false; then
  fail "retry_step: false should never succeed"
else
  rc=$?
  if [ "$rc" -eq 1 ] && [ "$GVISOR_STEP_ATTEMPTS" -eq 3 ]; then
    case "$GVISOR_STEP_DIAG" in
      *bad-sig*3\ attempts*) pass "retry_step: exhausts budget, diag names signature + count" ;;
      *) fail "retry_step: diag missing signature/count: '$GVISOR_STEP_DIAG'" ;;
    esac
  else
    fail "retry_step: expected rc=1 attempts=3, got rc=$rc attempts=$GVISOR_STEP_ATTEMPTS"
  fi
fi

# ── gvisor_retry_step: flaky step self-heals on 3rd attempt ───────────
FLAKY=$(mktemp); echo 0 > "$FLAKY"
flaky_step() {
  local n; n=$(cat "$FLAKY"); n=$((n + 1)); echo "$n" > "$FLAKY"
  [ "$n" -ge 3 ]   # fail attempts 1-2, succeed on 3
}
if gvisor_retry_step node-asset-ship 5 0 flaky_step; then
  if [ "$GVISOR_STEP_ATTEMPTS" -eq 3 ]; then
    pass "retry_step: flaky step self-heals on 3rd attempt (rode out 2 transients)"
  else
    fail "retry_step: expected self-heal at attempt 3, got $GVISOR_STEP_ATTEMPTS"
  fi
else
  fail "retry_step: flaky step should have succeeded by attempt 3"
fi
rm -f "$FLAKY"

# ── gvisor_retry_step: attempts=0 falls back to env default ───────────
export GVISOR_RETRY_ATTEMPTS=2
CNT=$(mktemp); echo 0 > "$CNT"
count_fail() { local n; n=$(cat "$CNT"); echo $((n + 1)) > "$CNT"; false; }
gvisor_retry_step fallback 0 0 count_fail || true
if [ "$(cat "$CNT")" -eq 2 ]; then
  pass "retry_step: attempts=0 falls back to GVISOR_RETRY_ATTEMPTS (=2)"
else
  fail "retry_step: env-default fallback ran $(cat "$CNT")x, expected 2"
fi
rm -f "$CNT"
unset GVISOR_RETRY_ATTEMPTS

# ── gvisor_warm_node_gate: warm node passes on first poll ─────────────
# Fake runner: ignores the cap ($1), dispatches on the command ($2).
fake_runner_warm() {
  local cmd="$2"
  case "$cmd" in
    *--version*)   echo "runsc version release-20260525.0" ;;
    *var/tmp*)     echo "WRITABLE" ;;
    *containerd*)  echo "ROOT_OK" ;;
  esac
}
if gvisor_warm_node_gate fake_runner_warm 5 0; then
  pass "warm_gate: warm node (runsc+tmp+root all OK) passes"
else
  fail "warm_gate: warm node should pass, diag='$GVISOR_WARM_DIAG'"
fi

# ── gvisor_warm_node_gate: cold node exhausts + diag names failing check ─
fake_runner_cold() {
  local cmd="$2"
  case "$cmd" in
    *--version*)   echo "" ;;            # runsc not staged yet
    *var/tmp*)     echo "WRITABLE" ;;
    *containerd*)  echo "ROOT_OK" ;;
  esac
}
if gvisor_warm_node_gate fake_runner_cold 3 0; then
  fail "warm_gate: cold node (no runsc) should NOT pass"
else
  case "$GVISOR_WARM_DIAG" in
    *"runsc handler not ready"*) pass "warm_gate: cold node fails, diag names runsc not ready" ;;
    *) fail "warm_gate: diag should name runsc readiness: '$GVISOR_WARM_DIAG'" ;;
  esac
fi

# ── gvisor_warm_node_gate: node warms up mid-poll (transient cold start) ─
WARM_STATE=$(mktemp); echo 0 > "$WARM_STATE"
fake_runner_warming() {
  local cmd="$2"
  case "$cmd" in
    *--version*)
      local n; n=$(cat "$WARM_STATE"); n=$((n + 1)); echo "$n" > "$WARM_STATE"
      # runsc absent for the first 2 polls, present on the 3rd.
      [ "$n" -ge 3 ] && echo "runsc version release-20260525.0" || echo "" ;;
    *var/tmp*)    echo "WRITABLE" ;;
    *containerd*) echo "ROOT_OK" ;;
  esac
}
if gvisor_warm_node_gate fake_runner_warming 6 0; then
  pass "warm_gate: node warming mid-poll eventually passes (rides out cold start)"
else
  fail "warm_gate: warming node should eventually pass, diag='$GVISOR_WARM_DIAG'"
fi
rm -f "$WARM_STATE"

# ── gvisor_warm_node_gate: /var/tmp not writable is reported distinctly ─
fake_runner_notmp() {
  local cmd="$2"
  case "$cmd" in
    *--version*)  echo "runsc version release-20260525.0" ;;
    *var/tmp*)    echo "" ;;             # tmpfs not mounted yet
    *containerd*) echo "ROOT_OK" ;;
  esac
}
if gvisor_warm_node_gate fake_runner_notmp 2 0; then
  fail "warm_gate: unwritable /var/tmp should NOT pass"
else
  case "$GVISOR_WARM_DIAG" in
    *var/tmp*) pass "warm_gate: unwritable /var/tmp reported distinctly in diag" ;;
    *) fail "warm_gate: diag should name /var/tmp: '$GVISOR_WARM_DIAG'" ;;
  esac
fi

# ── gvisor_warm_node_gate: REQUIRE_ROOT=0 passes with NO k8s runsc state root ─
# freeze-thaw / armed-guard build their own private runsc root and never populate
# /run/containerd/runsc/k8s.io — check (c) must be skipped for them.
fake_runner_noroot() {
  local cmd="$2"
  case "$cmd" in
    *--version*)  echo "runsc version release-20260525.0" ;;
    *var/tmp*)    echo "WRITABLE" ;;
    *containerd*) echo "ROOT_MISSING" ;;   # own-root module: k8s runsc root absent
  esac
}
if GVISOR_WARM_REQUIRE_ROOT=0 gvisor_warm_node_gate fake_runner_noroot 3 0; then
  pass "warm_gate: REQUIRE_ROOT=0 passes despite absent k8s runsc state root"
else
  fail "warm_gate: REQUIRE_ROOT=0 should skip check (c), diag='$GVISOR_WARM_DIAG'"
fi
# ... and the DEFAULT (require root) still fails that same node on check (c).
if gvisor_warm_node_gate fake_runner_noroot 2 0; then
  fail "warm_gate: default (require root) should fail when k8s runsc root absent"
else
  case "$GVISOR_WARM_DIAG" in
    *"runsc state root"*) pass "warm_gate: default still asserts k8s runsc state root (check (c))" ;;
    *) fail "warm_gate: diag should name state root: '$GVISOR_WARM_DIAG'" ;;
  esac
fi

# ── gvisor_warm_gate_via_run: stdin-streaming node_run shim adapts cleanly ─
# Simulate freeze-thaw's `node_run <cap>` (reads script from stdin, ignores cap).
fake_node_run() {
  local script; script="$(cat)"
  case "$script" in
    *--version*)  echo "runsc version release-20260525.0" ;;
    *var/tmp*)    echo "WRITABLE" ;;
    *containerd*) echo "ROOT_OK" ;;
  esac
}
if GVISOR_WARM_REQUIRE_ROOT=0 gvisor_warm_gate_via_run fake_node_run 3 0; then
  pass "warm_gate_via_run: stdin-streaming node_run adapted to the gate contract"
else
  fail "warm_gate_via_run: shim should pass a warm node, diag='$GVISOR_WARM_DIAG'"
fi

# ── gvisor_require_substrate_guard: REQUIRE_SUBSTRATE=1 → hard fail (exit 1) ─
# The guard always exit()s, so run it in a subshell and inspect the exit code +
# banner. This is the readiness-gate decision: a missing substrate under the gate
# is a NO-GO, never a silent pass (task ka-0000002867).
GUARD_OUT=$(REQUIRE_SUBSTRATE=1 bash -c '
  source "'"$SCRIPT_DIR"'/lib-gvisor-node.sh"
  gvisor_require_substrate_guard demo-module "runsc RuntimeClass absent"
' 2>&1); GUARD_RC=$?
if [ "$GUARD_RC" -eq 1 ]; then
  case "$GUARD_OUT" in
    *NO-GO*demo-module*) pass "require_substrate_guard: REQUIRE_SUBSTRATE=1 hard-fails (exit 1, NO-GO banner)" ;;
    *) fail "require_substrate_guard: exit 1 but banner missing NO-GO/module: '$GUARD_OUT'" ;;
  esac
else
  fail "require_substrate_guard: REQUIRE_SUBSTRATE=1 must exit 1, got rc=$GUARD_RC"
fi

# ── gvisor_require_substrate_guard: default (unset) → clean skip (exit 0) ─
GUARD_OUT=$(bash -c '
  source "'"$SCRIPT_DIR"'/lib-gvisor-node.sh"
  gvisor_require_substrate_guard demo-module "runsc RuntimeClass absent"
' 2>&1); GUARD_RC=$?
if [ "$GUARD_RC" -eq 0 ]; then
  case "$GUARD_OUT" in
    *SKIP*demo-module*) pass "require_substrate_guard: default unset → clean SKIP (exit 0)" ;;
    *) fail "require_substrate_guard: exit 0 but banner missing SKIP/module: '$GUARD_OUT'" ;;
  esac
else
  fail "require_substrate_guard: default unset must exit 0, got rc=$GUARD_RC"
fi

# ── gvisor_require_substrate_guard: REQUIRE_SUBSTRATE=0 explicit → clean skip ─
GUARD_OUT=$(REQUIRE_SUBSTRATE=0 bash -c '
  source "'"$SCRIPT_DIR"'/lib-gvisor-node.sh"
  gvisor_require_substrate_guard demo-module "absent"
' 2>&1); GUARD_RC=$?
if [ "$GUARD_RC" -eq 0 ]; then
  pass "require_substrate_guard: REQUIRE_SUBSTRATE=0 → clean SKIP (exit 0)"
else
  fail "require_substrate_guard: REQUIRE_SUBSTRATE=0 must exit 0, got rc=$GUARD_RC"
fi

# ── Summary ───────────────────────────────────────────────────────────
echo ""
echo "════════ test-lib-gvisor-node Summary: $PASS passed, $FAIL failed ════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do echo "  - $e"; done
  exit 1
fi
exit 0
