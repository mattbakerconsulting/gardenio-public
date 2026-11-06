#!/usr/bin/env bash
# canary-loop-driver_test.sh — hermetic unit tests for the Rig R4 bounded
# freeze→thaw canary loop driver (canary-loop-driver.sh, bead ka-0000006231).
#
# Fully offline: NO live kbeads daemon, NO canary, NO gVisor node, NO cluster,
# NO prod. The driver's collaborators are dependency-injected:
#   • R1 classifier  — the REAL sibling freeze-thaw-rungs.sh is sourced (we WANT
#     faithful rung classification; feeding it the same synthetic FT_* streams
#     the R1 test uses proves the driver reacts to genuine verdicts).
#   • R2 forensics    — replaced by a FAKE lib (FX_FORENSICS_LIB) that records the
#     call and returns a canned bundle id. (R2 has its own hermetic suite.)
#   • R3 bug filer    — replaced by a FAKE script (R4_FILE_BUG) that records its
#     argv and emits a canned R3_RESULT line.
#   • cycle runner    — a per-test FX_CYCLE_RUNNER that emits a scripted FT_*
#     stream (and/or a scripted exit code) for each cycle index.
#   • kd / gb         — PATH stubs that record every invocation to $CALL_LOG.
# Forbidden binaries (kubectl/helm/runsc) are stubbed to FAIL loudly, so any
# accidental live call is caught — the driver is HARNESS-ONLY.
#
# Usage:  bash scripts/gvisor/canary-loop-driver_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/canary-loop-driver.sh"

PASS=0
FAIL=0
fail_test() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }
pass_test() { echo "PASS: $1"; PASS=$((PASS + 1)); }

# ok_if_clean <test-name> — mark a test PASS iff IT recorded no failures. Uses a
# PER-TEST delta against $FAIL captured at test entry (see mark_start), NOT the
# module-global $FAIL — otherwise, once any earlier test fails, every later test
# (whose own assertions all pass) would be silently blanked from the report and
# the PASS count, making the tail misleading for triage.
TEST_FAIL_BASE=0
mark_start() { TEST_FAIL_BASE="$FAIL"; }
ok_if_clean() { [[ "$FAIL" -eq "$TEST_FAIL_BASE" ]] && pass_test "$1"; }

# Shared FX_HARD_HOLD hook: records the terminal result so a test can assert the
# orphan-sweep hook fired on its HARD-HOLD path. Defined ONCE (exported into the
# SUT's env by the tests that need it) rather than re-declared per test.
# shellcheck disable=SC2317  # invoked indirectly by the SUT via $FX_HARD_HOLD
hard_hold_hook() { echo "HOOK_CALLED result=$1" >> "$CALL_LOG"; }
export -f hard_hold_hook

# ---------------------------------------------------------------------------
# Synthetic FT_* sentinel streams — the exact shapes freeze-thaw-rungs_test.sh
# feeds the real classifier, so the driver's reaction is grounded in real R1
# verdicts, not a re-implementation.
# ---------------------------------------------------------------------------
freeze_ok() {
  cat <<'EOF'
FT_AGENT_CID=ag123
FT_CHECKPOINT_RC=0
FT_IMAGE_PRESENT=yes
FT_IMAGE_SIZE=3.0G
FT_CONTAINER_COUNT=2
FT_SANDBOX_STATE=running
FT_BUNDLE_OK=yes
FT_IMAGE_OK=yes
EOF
}
thaw_ok() {
  cat <<'EOF'
FT_B_NETNS=net:[4026532540]
FT_CNI_CONFIRMED=yes
FT_CNI_POD_IP=10.0.1.5/24
FT_SUB_RC=0
FT_ROOT_RC=0
FT_RESTORED_PID=37737
FT_RESTORED_NETNS=net:[4026532540]
FT_NETNS_IFACES=1
FT_NETNS_DEFROUTE=yes
FT_CONTAINERS_RUNNING=yes
EOF
}
# rung-5/6/7 harness signals, all passing (positively MEASURED).
harness_ok() {
  cat <<'EOF'
FT_REHYDRATE_GB_ACK=ok
FT_REHYDRATE_COOP_ACK=ok
FT_WORK_TOKEN=intact
FT_RAM_RESUMED=yes
EOF
}
# A fully clean, fully measured cycle.
stream_full_ok() { freeze_ok; thaw_ok; harness_ok; }
# A clean-looking cycle whose rung-5/6/7 harness signals are ABSENT (never
# measured) — R1 returns rc=0 (no rung FAILED) but the gating rungs are `skip`.
# This is the SILENT PASS the driver must refuse.
stream_unmeasured_tail() { freeze_ok; thaw_ok; }
# A cycle that FAILS a rung definitively: netns comes up lo-only (the #11 mode:
# ifaces present but no default route).
stream_netns_fail() {
  freeze_ok
  thaw_ok | sed 's/FT_NETNS_DEFROUTE=yes/FT_NETNS_DEFROUTE=no/'
  harness_ok
}
# A cycle that fails at freeze (rung 1): bad checkpoint rc.
stream_snapshot_fail() {
  freeze_ok | sed 's/FT_CHECKPOINT_RC=0/FT_CHECKPOINT_RC=137/'
  thaw_ok; harness_ok
}
# Clean + measured EXCEPT exactly ONE gating rung: ram_intact's sentinel is
# absent (acks + work ARE measured). R1 returns rc=0 (ram_intact=skip, not fail);
# the driver's silent-pass guard must still flag it inconclusive on the single
# missing gating signal — proving the guard is per-rung, not all-or-nothing.
stream_one_gating_unmeasured() {
  freeze_ok; thaw_ok
  cat <<'EOF'
FT_REHYDRATE_GB_ACK=ok
FT_REHYDRATE_COOP_ACK=ok
FT_WORK_TOKEN=intact
EOF
}
# Clean + fully measured on all SIX gating rungs, but the NON-gating
# cni_confirmed sentinel is absent (R1 → cni_confirmed=skip). The driver must
# still classify SUCCESS — a non-gating skip must NOT trip the guard.
stream_cni_skip_still_ok() {
  freeze_ok
  thaw_ok | sed '/FT_CNI_CONFIRMED=/d'
  harness_ok
}

# ---------------------------------------------------------------------------
# Shims: kd/gb PATH stubs (record calls); a fake R2 lib; a fake R3 filer; a
# per-test cycle runner sourced from $CYCLE_PLAN.
# ---------------------------------------------------------------------------
make_shims() {
    local bin="$1"
    mkdir -p "$bin"

    cat > "${bin}/kd" <<'SHIM'
#!/usr/bin/env bash
echo "kd $*" >> "$CALL_LOG"
echo "kd ${1:-} ${2:-}" >> "$VERB_LOG"
exit 0
SHIM
    chmod +x "${bin}/kd"

    cat > "${bin}/gb" <<'SHIM'
#!/usr/bin/env bash
echo "gb $*" >> "$CALL_LOG"
echo "gb ${1:-} ${2:-}" >> "$VERB_LOG"
exit 0
SHIM
    chmod +x "${bin}/gb"

    # HARNESS-ONLY guard: these must never run. Fail loudly if they do.
    for forbidden in kubectl helm runsc nsenter; do
        cat > "${bin}/${forbidden}" <<SHIM
#!/usr/bin/env bash
echo "${forbidden} \$*" >> "\$VERB_LOG"
echo "${forbidden}: harness-only driver must not call this" >&2
exit 97
SHIM
        chmod +x "${bin}/${forbidden}"
    done
}

# A fake R2 forensic lib: defines gvisor_forensic_capture, records the call, and
# returns a canned bundle id via the R2 return-channel sentinels/vars.
write_fake_forensics() {
    cat > "$1" <<'LIB'
#!/usr/bin/env bash
FT_FORENSIC_DIR=""
FT_FORENSIC_BUNDLE=""
FT_FORENSIC_URL=""
gvisor_forensic_capture() {
    echo "R2_CAPTURE rung=${FX_RUNG:-?} reason=${FX_REASON:-?} origin=${FX_ORIGIN_BEAD:-?} markers=${FX_MARKERS:-?}" >> "$CALL_LOG"
    FT_FORENSIC_DIR="/tmp/fake-forensic-dir"
    FT_FORENSIC_BUNDLE="gd-FAKEBUNDLE1"
    FT_FORENSIC_URL="/bundle/gd-FAKEBUNDLE1"
    printf 'FT_FORENSIC_DIR=%s\n' "$FT_FORENSIC_DIR"
    printf 'FT_FORENSIC_BUNDLE=%s\n' "$FT_FORENSIC_BUNDLE"
    printf 'FT_FORENSIC_URL=%s\n' "$FT_FORENSIC_URL"
    return 0
}
LIB
}

# A fake R3 filer: records its argv, emits a canned created result.
write_fake_r3() {
    cat > "$1" <<'R3'
#!/usr/bin/env bash
echo "R3_CALL $*" >> "$CALL_LOG"
echo "R3_RESULT=created bug=gd-SKKYDVVNQU sig=deadbeef1234"
exit 0
R3
    chmod +x "$1"
}

# A fake R3 filer that emits R3's WARN-recovery `created_unlinked` result — the
# real file-next-layer-bug.sh does this (returns 0) when a dep edge fails to
# wire. The driver must still parse the bug id, reach HARD HOLD, and NOT abort.
write_fake_r3_unlinked() {
    cat > "$1" <<'R3'
#!/usr/bin/env bash
echo "R3_CALL $*" >> "$CALL_LOG"
echo "R3_RESULT=created_unlinked bug=gd-BQK5NBDGVA sig=deadbeef1234 blocks=0 parent=1"
exit 0
R3
    chmod +x "$1"
}

# A fake R3 filer that FAILS (non-zero) — a transient daemon/auth blip. The
# driver must leave R4_FILED_BUG empty but STILL reach HARD HOLD (evidence is
# already captured by R2).
write_fake_r3_failing() {
    cat > "$1" <<'R3'
#!/usr/bin/env bash
echo "R3_CALL $*" >> "$CALL_LOG"
echo "file-next-layer-bug: dedup query failed" >&2
exit 3
R3
    chmod +x "$1"
}

# A fake R2 lib whose capture DEGRADES: no bundle created (gb unavailable), but
# the local staging dir IS set — R2's documented COMMON degraded path (returns
# 1, NOT an error). The driver must fall back to the dir as the evidence ref.
write_fake_forensics_degraded() {
    cat > "$1" <<'LIB'
#!/usr/bin/env bash
FT_FORENSIC_DIR=""
FT_FORENSIC_BUNDLE=""
FT_FORENSIC_URL=""
gvisor_forensic_capture() {
    echo "R2_CAPTURE_DEGRADED rung=${FX_RUNG:-?}" >> "$CALL_LOG"
    FT_FORENSIC_DIR="/tmp/fake-degraded-dir"
    FT_FORENSIC_BUNDLE=""      # gb unavailable — no bundle
    FT_FORENSIC_URL=""
    printf 'FT_FORENSIC_DIR=%s\n' "$FT_FORENSIC_DIR"
    printf 'FT_FORENSIC_BUNDLE=\n'
    printf 'FT_FORENSIC_URL=\n'
    return 1
}
LIB
}

# A fake gb that FAILS specifically on `decision create` (exit 0 otherwise) — a
# transient daemon blip on the success path. The driver must NOT crash: it still
# records the clean-cycle canary comment and reaches HARD HOLD with an empty
# R4_DECISION.
write_gb_decision_failing() {
    cat > "$1" <<'SHIM'
#!/usr/bin/env bash
echo "gb $*" >> "$CALL_LOG"
echo "gb ${1:-} ${2:-}" >> "$VERB_LOG"
if [[ "${1:-}" == "decision" && "${2:-}" == "create" ]]; then
    echo "gb: decision create failed (transient)" >&2
    exit 1
fi
exit 0
SHIM
    chmod +x "$1"
}

# A fake kd that FAILS specifically on `comment add` (exit 0 otherwise) — a
# transient daemon/auth blip while the driver is recording a canary comment. The
# `|| echo …` guards at handle_success AND the inconclusive_exhausted terminal
# are load-bearing: without them, this failure would trip set -e and abort BEFORE
# r4_hard_hold. The driver must still reach HARD HOLD on BOTH paths.
write_kd_comment_failing() {
    cat > "$1" <<'SHIM'
#!/usr/bin/env bash
echo "kd $*" >> "$CALL_LOG"
echo "kd ${1:-} ${2:-}" >> "$VERB_LOG"
if [[ "${1:-}" == "comment" && "${2:-}" == "add" ]]; then
    echo "kd: comment add failed (transient daemon blip)" >&2
    exit 5
fi
exit 0
SHIM
    chmod +x "$1"
}

# Install a `mktemp` PATH shim that FAILS (rc 1, permission-denied style) when
# ANY argument contains the substring $MKTEMP_FAIL_GLOB, else delegates to the
# REAL mktemp. This lets a test force a SPECIFIC SUT mktemp site to fail by its
# filename prefix (r4-cycle | r4-markers | r4-rungs) without breaking the test
# harness's own mktemp calls or the real R1 classifier (R1 uses no mktemp).
# Preferred over chmod-000 on a dir: agents may run as root, where directory
# permissions are ignored and an "unwritable" dir is still writable — a stub is
# deterministic on every uid.
write_failing_mktemp() {
    cat > "${1}/mktemp" <<'STUB'
#!/usr/bin/env bash
real=""
for c in /usr/bin/mktemp /bin/mktemp; do
    [[ -x "$c" ]] && { real="$c"; break; }
done
if [[ -n "${MKTEMP_FAIL_GLOB:-}" ]]; then
    for a in "$@"; do
        case "$a" in
            *"${MKTEMP_FAIL_GLOB}"*)
                echo "mktemp: cannot create via '$a': Permission denied (test-injected)" >&2
                exit 1 ;;
        esac
    done
fi
exec "$real" "$@"
STUB
    chmod +x "${1}/mktemp"
}

# A stable base dir that outlives individual setup/teardown cycles, so the SUT's
# `mktemp` (which honors $TMPDIR) always has a valid, existing base — a per-test
# TMPDIR pointed at the dir teardown deletes would leave a stale export that
# breaks the NEXT test's mktemp.
BASE_TMPDIR="$(mktemp -d)"
export TMPDIR="$BASE_TMPDIR"
trap 'rm -rf "$BASE_TMPDIR"' EXIT

# Preserve the pristine PATH so each setup extends IT, not the previous test's
# already-extended PATH (otherwise stale shim dirs accumulate on PATH).
PRISTINE_PATH="$PATH"

setup() {
    TMP="$(mktemp -d "${BASE_TMPDIR}/case.XXXXXX")"
    BIN="${TMP}/bin"
    make_shims "$BIN"

    CALL_LOG="${TMP}/calls.log"; : > "$CALL_LOG"; export CALL_LOG
    VERB_LOG="${TMP}/verbs.log"; : > "$VERB_LOG"; export VERB_LOG

    FAKE_FORENSICS="${TMP}/lib-forensics.sh"; write_fake_forensics "$FAKE_FORENSICS"
    FAKE_R3="${TMP}/file-bug.sh"; write_fake_r3 "$FAKE_R3"

    export PATH="${BIN}:${PRISTINE_PATH}"
    export KD="kd" GB="gb"
    export FT_RUNGS_LIB="${SCRIPT_DIR}/freeze-thaw-rungs.sh"   # the REAL classifier
    export FX_FORENSICS_LIB="$FAKE_FORENSICS"
    export R4_FILE_BUG="$FAKE_R3"
}
teardown() { rm -rf "$TMP"; export PATH="$PRISTINE_PATH"; }

# run_driver <stream-fn-per-cycle...> — build an FX_CYCLE_RUNNER that emits the
# i-th named stream fn on cycle i (and repeats the LAST one for any further
# cycles), then run the SUT. Sets OUT (stdout) and RC (exit code).
# Each arg is either "fn" (emit fn's stream, rc 0) or "flake" (emit nothing, rc 1).
run_driver() {
    local plans=("$@")
    # Serialize the plan into an env var the runner reads by cycle index.
    export R4_TEST_PLAN="${plans[*]}"
    cycle_runner() {
        local idx="$1"
        # shellcheck disable=SC2206
        local arr=($R4_TEST_PLAN)
        local n="${#arr[@]}"
        local sel
        if (( idx <= n )); then sel="${arr[$((idx-1))]}"; else sel="${arr[$((n-1))]}"; fi
        if [[ "$sel" == "flake" ]]; then
            return 1
        fi
        "$sel"
        return 0
    }
    export -f cycle_runner freeze_ok thaw_ok harness_ok \
        stream_full_ok stream_unmeasured_tail stream_netns_fail stream_snapshot_fail \
        stream_one_gating_unmeasured stream_cni_skip_still_ok
    export FX_CYCLE_RUNNER=cycle_runner
    # Expand DRIVER_ARGS safely: an EMPTY array under `set -u` with the classic
    # "${arr[@]:-}" idiom would yield one spurious empty-string arg (which the
    # driver rejects as `unknown arg: ""`). Guard on the element count instead so
    # a flagless invocation passes ZERO args and exercises the driver defaults.
    if [[ "${#DRIVER_ARGS[@]}" -gt 0 ]]; then
        OUT="$(bash "$SUT" "${DRIVER_ARGS[@]}" 2>/dev/null)"
    else
        OUT="$(bash "$SUT" 2>/dev/null)"
    fi
    RC=$?
}

# grab <key> — last value of a R4_<key>=... sentinel from $OUT.
grab() { printf '%s\n' "$OUT" | grep -oE "^${1}=.*" | tail -n1 | cut -d= -f2-; }

# ===========================================================================
# T1: clean cycle → mark canary + gb decision SUCCESS + HARD HOLD, no bug, no R2.
# ===========================================================================
test_success_marks_and_decides() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_full_ok

    [[ "$(grab R4_RESULT)" == "success" ]]        || fail_test T1 "expected R4_RESULT=success, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]           || fail_test T1 "missing HARD HOLD"
    [[ "$(grab R4_CYCLES_RUN)" == "1" ]]          || fail_test T1 "should stop after 1 clean cycle, got $(grab R4_CYCLES_RUN)"
    [[ "$(grab R4_DECISION)" == "created" ]]      || fail_test T1 "expected decision created"
    [[ "$RC" -eq 0 ]]                             || fail_test T1 "clean success should exit 0, rc=$RC"
    # Canary marked clean (comment on ka-0000001355).
    grep -q 'kd comment add ka-0000001355' "$CALL_LOG" || fail_test T1 "no clean-cycle comment on canary"
    # gb decision SUCCESS opened, non-blocking, with real divergent options.
    grep -q 'gb decision create' "$CALL_LOG"      || fail_test T1 "no gb decision create"
    grep -q -- '--no-wait' "$CALL_LOG"            || fail_test T1 "decision must be --no-wait (never blocks)"
    grep -q 'flip_go_no_go' "$CALL_LOG"           || fail_test T1 "decision missing go/no-go option"
    # NO forensic capture, NO bug filed on a clean cycle.
    grep -q 'R2_CAPTURE' "$CALL_LOG"              && fail_test T1 "must NOT capture forensics on a clean cycle"
    grep -q 'R3_CALL'    "$CALL_LOG"              && fail_test T1 "must NOT file a bug on a clean cycle"
    # gb yield must NEVER be called (autonomous driver).
    grep -q 'gb yield'   "$CALL_LOG"              && fail_test T1 "driver must never gb yield"
    ok_if_clean T1
    teardown
}

# ===========================================================================
# T2: rung failure → R2 capture → R3 file → HARD HOLD, and NO same-layer retry
#     (exactly ONE cycle runs even though max-cycles=5).
# ===========================================================================
test_rung_failure_captures_files_holds_no_retry() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_netns_fail

    [[ "$(grab R4_RESULT)" == "rung_failure" ]]         || fail_test T2 "expected rung_failure, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]                 || fail_test T2 "missing HARD HOLD"
    [[ "$(grab R4_CYCLES_RUN)" == "1" ]]                || fail_test T2 "NO same-layer retry: exactly 1 cycle, got $(grab R4_CYCLES_RUN)"
    [[ "$(grab R4_FIRST_FAILED_RUNG)" == "netns_routable" ]] || fail_test T2 "wrong failed rung: $(grab R4_FIRST_FAILED_RUNG)"
    [[ "$(grab R4_FORENSIC_BUNDLE)" == "gd-FAKEBUNDLE1" ]]   || fail_test T2 "R2 bundle not captured"
    [[ "$(grab R4_FILED_BUG)" == "gd-SKKYDVVNQU" ]]     || fail_test T2 "R3 bug not filed/parsed"
    [[ "$RC" -eq 1 ]]                                   || fail_test T2 "non-clean terminal should exit 1, rc=$RC"
    # ORDER: R2 capture happens BEFORE R3 file (evidence before the bug link).
    local r2_line r3_line
    r2_line="$(grep -n 'R2_CAPTURE' "$CALL_LOG" | head -n1 | cut -d: -f1)"
    r3_line="$(grep -n 'R3_CALL'    "$CALL_LOG" | head -n1 | cut -d: -f1)"
    [[ -n "$r2_line" && -n "$r3_line" && "$r2_line" -lt "$r3_line" ]] \
        || fail_test T2 "R2 capture must precede R3 file (r2@${r2_line} r3@${r3_line})"
    # R3 was passed the R2 bundle id and the failing rung.
    grep -q 'R3_CALL.*--bundle gd-FAKEBUNDLE1' "$CALL_LOG" || fail_test T2 "R3 not given R2 bundle"
    grep -q 'R3_CALL.*--rung netns_routable'   "$CALL_LOG" || fail_test T2 "R3 not given failing rung"
    # NO success side effects.
    grep -q 'gb decision create' "$CALL_LOG"    && fail_test T2 "must NOT open SUCCESS decision on failure"
    ok_if_clean T2
    teardown
}

# ===========================================================================
# T3: SILENT-PASS GUARD — a cycle where R1 returns "no failure" but the gating
#     rungs (acks/work/ram) were never measured must NOT be declared clean; the
#     driver re-drives (bounded). A LATER cycle that IS fully measured succeeds.
# ===========================================================================
test_silent_pass_guard_redrives_then_succeeds() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    # cycle 1: unmeasured tail (inconclusive) ; cycle 2: fully clean → success.
    run_driver stream_unmeasured_tail stream_full_ok

    [[ "$(grab R4_RESULT)" == "success" ]]  || fail_test T3 "expected eventual success, OUT:\n$OUT"
    [[ "$(grab R4_CYCLES_RUN)" == "2" ]]     || fail_test T3 "should re-drive once then succeed (2 cycles), got $(grab R4_CYCLES_RUN)"
    grep -q 'verdict=inconclusive kind=unmeasured_gating_rung' <<<"$OUT" \
        || fail_test T3 "cycle 1 should be flagged unmeasured/inconclusive"
    # The silent-pass cycle must NOT have marked the canary clean or filed a bug.
    # (Only the final success marks it; and a bug is never filed on inconclusive.)
    grep -q 'R3_CALL' "$CALL_LOG" && fail_test T3 "must NOT file a bug on a silent-pass cycle"
    ok_if_clean T3
    teardown
}

# ===========================================================================
# T4: bounded cap — all cycles inconclusive (silent-pass) → exhausts at
#     max-cycles, R2 captures last state, NO R3 bug, HARD HOLD.
# ===========================================================================
test_bounded_cap_exhausts_inconclusive() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 3)
    # Every cycle is an unmeasured tail → never conclusive.
    run_driver stream_unmeasured_tail

    [[ "$(grab R4_RESULT)" == "inconclusive_exhausted" ]] || fail_test T4 "expected inconclusive_exhausted, OUT:\n$OUT"
    [[ "$(grab R4_CYCLES_RUN)" == "3" ]]                  || fail_test T4 "should run exactly max-cycles=3, got $(grab R4_CYCLES_RUN)"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]                   || fail_test T4 "missing HARD HOLD"
    [[ "$RC" -eq 1 ]]                                     || fail_test T4 "exhausted should exit 1, rc=$RC"
    # R2 captured the last stream as evidence; but NO R3 bug (no classified fail).
    grep -q 'R2_CAPTURE' "$CALL_LOG" || fail_test T4 "should capture last-state evidence on exhaustion"
    grep -q 'R3_CALL'    "$CALL_LOG" && fail_test T4 "must NOT file a bug without a classified rung failure"
    ok_if_clean T4
    teardown
}

# ===========================================================================
# T5: infra flake → inconclusive (NOT a rung failure), retried; a later clean
#     cycle succeeds. Proves the runner's non-zero rc is treated as "layer never
#     exercised", not as a same-layer failure.
# ===========================================================================
test_infra_flake_retries_then_succeeds() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    run_driver flake flake stream_full_ok

    [[ "$(grab R4_RESULT)" == "success" ]] || fail_test T5 "expected success after flakes, OUT:\n$OUT"
    [[ "$(grab R4_CYCLES_RUN)" == "3" ]]    || fail_test T5 "2 flakes + 1 clean = 3 cycles, got $(grab R4_CYCLES_RUN)"
    grep -q 'verdict=inconclusive kind=infra_flake' <<<"$OUT" || fail_test T5 "flake not classified as infra_flake"
    grep -q 'R3_CALL' "$CALL_LOG" && fail_test T5 "must NOT file a bug on an infra flake"
    ok_if_clean T5
    teardown
}

# ===========================================================================
# T6: a rung failure that lands AFTER an inconclusive cycle still goes straight
#     to capture→file→HOLD on the failing cycle (no retry of the FAILURE).
# ===========================================================================
test_failure_after_inconclusive_holds_immediately() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    run_driver flake stream_snapshot_fail stream_full_ok   # 3rd never reached

    [[ "$(grab R4_RESULT)" == "rung_failure" ]]              || fail_test T6 "expected rung_failure, OUT:\n$OUT"
    [[ "$(grab R4_CYCLES_RUN)" == "2" ]]                     || fail_test T6 "flake then fail = 2 cycles (no retry of failure), got $(grab R4_CYCLES_RUN)"
    [[ "$(grab R4_FIRST_FAILED_RUNG)" == "snapshot_persisted" ]] || fail_test T6 "wrong failed rung: $(grab R4_FIRST_FAILED_RUNG)"
    grep -q 'R2_CAPTURE' "$CALL_LOG" || fail_test T6 "no R2 capture on failure"
    grep -q 'R3_CALL'    "$CALL_LOG" || fail_test T6 "no R3 file on failure"
    ok_if_clean T6
    teardown
}

# ===========================================================================
# T7: --dry-run mutates NOTHING (no kd/gb calls), still classifies + HARD HOLDs.
# ===========================================================================
test_dry_run_no_mutations() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5 --dry-run)
    run_driver stream_full_ok

    [[ "$(grab R4_RESULT)" == "success" ]] || fail_test T7 "dry-run should still classify success, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]    || fail_test T7 "dry-run missing HARD HOLD"
    # No REAL kd/gb mutations recorded (dry-run prints DRY-RUN lines instead).
    grep -q '^kd comment' "$CALL_LOG" && fail_test T7 "dry-run must not really comment"
    grep -q '^gb decision' "$CALL_LOG" && fail_test T7 "dry-run must not really open a decision"
    grep -q 'DRY-RUN:' <<<"$OUT"        || fail_test T7 "dry-run should print DRY-RUN lines"
    ok_if_clean T7
    teardown
}

# ===========================================================================
# T8: input validation — missing FX_CYCLE_RUNNER and bad --max-cycles die (rc 2).
# ===========================================================================
test_arg_validation() {
    setup
    mark_start
    # Missing FX_CYCLE_RUNNER.
    ( unset FX_CYCLE_RUNNER; bash "$SUT" --max-cycles 5 >/dev/null 2>&1 )
    [[ $? -eq 2 ]] || fail_test T8 "missing FX_CYCLE_RUNNER should exit 2"
    # Non-numeric max-cycles.
    FX_CYCLE_RUNNER=true bash "$SUT" --max-cycles abc >/dev/null 2>&1
    [[ $? -eq 2 ]] || fail_test T8 "non-numeric --max-cycles should exit 2"
    # Zero max-cycles.
    FX_CYCLE_RUNNER=true bash "$SUT" --max-cycles 0 >/dev/null 2>&1
    [[ $? -eq 2 ]] || fail_test T8 "--max-cycles 0 should exit 2"
    ok_if_clean T8
    teardown
}

# ===========================================================================
# T9: HARNESS-ONLY — across a full run, kubectl/helm/runsc/nsenter are NEVER
#     invoked (the driver only touches beads via kd/gb + the injected libs).
# ===========================================================================
test_harness_only_no_node_calls() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_netns_fail   # the busiest path: capture + file
    for forbidden in kubectl helm runsc nsenter; do
        grep -qE "^${forbidden} " "$VERB_LOG" && fail_test T9 "driver invoked forbidden binary: ${forbidden}"
    done
    ok_if_clean T9
    teardown
}

# ===========================================================================
# T10: FX_HARD_HOLD hook is invoked with the terminal result at HARD HOLD.
# ===========================================================================
test_hard_hold_hook_invoked() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    export FX_HARD_HOLD=hard_hold_hook
    run_driver stream_full_ok
    grep -q 'HOOK_CALLED result=success' "$CALL_LOG" || fail_test T10 "FX_HARD_HOLD hook not invoked with result"
    unset FX_HARD_HOLD
    ok_if_clean T10
    teardown
}

# ===========================================================================
# T11: R3 returns `created_unlinked` (a dep edge failed to wire) — the driver
#      must PARSE the bug id, surface the degraded state, and STILL reach HARD
#      HOLD with the terminal sentinels. Regression guard for the critical
#      set-e-abort-on-nonmatching-grep bug (findings 1+2): a `created_unlinked`
#      result once aborted the driver BEFORE r4_hard_hold (no orphan sweep).
# ===========================================================================
test_r3_created_unlinked_still_holds() {
    setup
    mark_start
    write_fake_r3_unlinked "$FAKE_R3"          # override the default fake R3
    export FX_HARD_HOLD=hard_hold_hook
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_netns_fail

    [[ "$(grab R4_RESULT)" == "rung_failure" ]] || fail_test T11 "expected rung_failure, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]         || fail_test T11 "HARD HOLD sentinel MUST be emitted on created_unlinked"
    [[ "$(grab R4_FILED_BUG)" == "gd-BQK5NBDGVA" ]] || fail_test T11 "must parse bug id from created_unlinked, got '$(grab R4_FILED_BUG)'"
    grep -q 'HOOK_CALLED result=rung_failure' "$CALL_LOG" || fail_test T11 "FX_HARD_HOLD orphan-sweep hook MUST run (zero orphans)"
    unset FX_HARD_HOLD
    ok_if_clean T11
    teardown
}

# ===========================================================================
# T12: R3 filer FAILS (non-zero) — evidence is already captured by R2; the
#      driver must leave R4_FILED_BUG empty but STILL reach HARD HOLD (the
#      set +e/set -e guard around the R3 call is load-bearing — finding 4).
# ===========================================================================
test_r3_failure_still_holds() {
    setup
    mark_start
    write_fake_r3_failing "$FAKE_R3"
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_netns_fail

    [[ "$(grab R4_RESULT)" == "rung_failure" ]] || fail_test T12 "expected rung_failure, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]         || fail_test T12 "HARD HOLD MUST be reached even when R3 fails"
    [[ "$(grab R4_FORENSIC_BUNDLE)" == "gd-FAKEBUNDLE1" ]] || fail_test T12 "R2 evidence must still be captured"
    [[ -z "$(grab R4_FILED_BUG)" ]]             || fail_test T12 "R4_FILED_BUG must be empty when R3 failed, got '$(grab R4_FILED_BUG)'"
    ok_if_clean T12
    teardown
}

# ===========================================================================
# T13: --dry-run + a rung FAILURE — the R3 invocation is a `DRY-RUN:` echo (no
#      R3_RESULT line). The driver must NOT abort on the non-matching parse and
#      must reach HARD HOLD (the second trigger of the critical set-e bug).
# ===========================================================================
test_dry_run_failure_still_holds() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5 --dry-run)
    run_driver stream_netns_fail

    [[ "$(grab R4_RESULT)" == "rung_failure" ]] || fail_test T13 "expected rung_failure, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]         || fail_test T13 "HARD HOLD MUST be reached on dry-run failure"
    [[ -z "$(grab R4_FILED_BUG)" ]]             || fail_test T13 "dry-run files nothing, R4_FILED_BUG must be empty"
    grep -qE '^R3_CALL' "$CALL_LOG" && fail_test T13 "dry-run must not really invoke R3"
    ok_if_clean T13
    teardown
}

# ===========================================================================
# T14: R2 DEGRADED capture (no bundle, dir only) — the driver must fall back to
#      the staging dir as the evidence ref and pass THAT to R3 (finding 5). The
#      real R2's common degraded path returns 1 with FT_FORENSIC_BUNDLE empty.
# ===========================================================================
test_r2_degraded_uses_dir_ref() {
    setup
    mark_start
    write_fake_forensics_degraded "$FAKE_FORENSICS"
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_netns_fail

    [[ "$(grab R4_RESULT)" == "rung_failure" ]] || fail_test T14 "expected rung_failure, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]         || fail_test T14 "HARD HOLD MUST be reached"
    [[ -z "$(grab R4_FORENSIC_BUNDLE)" ]]       || fail_test T14 "no bundle in degraded mode, got '$(grab R4_FORENSIC_BUNDLE)'"
    grep -q 'R2_CAPTURE_DEGRADED' "$CALL_LOG"   || fail_test T14 "degraded R2 not called"
    # R3 must receive the staging-DIR ref (not empty) as --bundle.
    grep -q 'R3_CALL.*--bundle /tmp/fake-degraded-dir' "$CALL_LOG" || fail_test T14 "R3 not given the degraded dir ref as --bundle"
    ok_if_clean T14
    teardown
}

# ===========================================================================
# T15: gb decision create FAILS on a clean cycle — the driver must NOT crash;
#      it still records the clean-cycle canary comment, reaches HARD HOLD, and
#      emits an EMPTY R4_DECISION (autonomy invariant — finding 8).
# ===========================================================================
test_decision_failure_still_marks_and_holds() {
    setup
    mark_start
    write_gb_decision_failing "${BIN}/gb"      # override the gb PATH stub
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_full_ok

    [[ "$(grab R4_RESULT)" == "success" ]] || fail_test T15 "clean cycle must still classify success, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]    || fail_test T15 "HARD HOLD MUST be reached even if decision create fails"
    [[ -z "$(grab R4_DECISION)" ]]         || fail_test T15 "R4_DECISION must be empty when create failed, got '$(grab R4_DECISION)'"
    grep -q 'kd comment add ka-0000001355' "$CALL_LOG" || fail_test T15 "clean-cycle canary comment must still be recorded"
    [[ "$RC" -eq 0 ]]                      || fail_test T15 "a clean cycle is still success even if the decision blip'd, rc=$RC"
    ok_if_clean T15
    teardown
}

# ===========================================================================
# T16: silent-pass guard is PER-RUNG — a cycle missing exactly ONE gating rung
#      (ram_intact) while acks+work ARE measured is still inconclusive; a
#      later fully-measured cycle succeeds. Guards against a guard-refactor to
#      an all-or-nothing check (finding 9).
# ===========================================================================
test_silent_pass_guard_is_per_rung() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_one_gating_unmeasured stream_full_ok

    [[ "$(grab R4_RESULT)" == "success" ]] || fail_test T16 "expected eventual success, OUT:\n$OUT"
    [[ "$(grab R4_CYCLES_RUN)" == "2" ]]    || fail_test T16 "should re-drive on the single missing rung then succeed, got $(grab R4_CYCLES_RUN)"
    grep -q 'kind=unmeasured_gating_rung' <<<"$OUT"  || fail_test T16 "single missing rung not flagged inconclusive"
    grep -q 'ram_intact=' <<<"$OUT"                  || fail_test T16 "the specific missing rung (ram_intact) not named in not_ok"
    grep -q 'gb decision create' "$CALL_LOG"         || fail_test T16 "final clean cycle should open a decision"
    ok_if_clean T16
    teardown
}

# ===========================================================================
# T17: a non-gating rung (cni_confirmed) left as `skip` must NOT trip the
#      silent-pass guard — a fully-measured-gating cycle succeeds on cycle 1
#      even with cni_confirmed absent (finding 7).
# ===========================================================================
test_non_gating_skip_still_succeeds() {
    setup
    mark_start
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_cni_skip_still_ok

    [[ "$(grab R4_RESULT)" == "success" ]] || fail_test T17 "cni_confirmed=skip must still succeed, OUT:\n$OUT"
    [[ "$(grab R4_CYCLES_RUN)" == "1" ]]    || fail_test T17 "should succeed on cycle 1 (non-gating skip), got $(grab R4_CYCLES_RUN)"
    ok_if_clean T17
    teardown
}

# ===========================================================================
# T18: DEFAULT cap — with NO --max-cycles flag, the loop honors the spec default
#      (~5). All-flake plan runs exactly DEFAULT_MAX_CYCLES cycles then exhausts.
#      Pins the ONE constant the bead names as spec (finding 10).
# ===========================================================================
test_default_max_cycles() {
    setup
    mark_start
    # Read the driver's own default so the test tracks the source of truth.
    local def
    def="$(grep -oE '^DEFAULT_MAX_CYCLES=[0-9]+' "$SUT" | head -n1 | cut -d= -f2)"
    [[ -n "$def" ]] || fail_test T18 "could not read DEFAULT_MAX_CYCLES from SUT"
    DRIVER_ARGS=()                             # NO --max-cycles → use the default
    run_driver flake                           # every cycle flakes → exhausts at cap

    [[ "$(grab R4_RESULT)" == "inconclusive_exhausted" ]] || fail_test T18 "all-flake should exhaust, OUT:\n$OUT"
    [[ "$(grab R4_CYCLES_RUN)" == "$def" ]] || fail_test T18 "default cap not honored: ran $(grab R4_CYCLES_RUN), expected $def"
    ok_if_clean T18
    teardown
}

# ===========================================================================
# T19: [CRITICAL] loop-top stream_file mktemp FAILS — the driver cannot stage a
#      cycle stream. It must NOT abort under set -e (the orphaned-canary /
#      HARD-HOLD-not-reached bug the rig prevents); it must terminate
#      inconclusive_exhausted, fire the FX_HARD_HOLD hook, reach HARD HOLD, and
#      touch NOTHING (no canary comment, no decision, no bug — zero orphans).
# ===========================================================================
test_looptop_mktemp_failure_hard_holds() {
    setup
    mark_start
    write_failing_mktemp "$BIN"
    export MKTEMP_FAIL_GLOB="r4-cycle"
    export FX_HARD_HOLD=hard_hold_hook
    DRIVER_ARGS=(--max-cycles 3)
    run_driver stream_full_ok                    # would be clean, but can't be staged

    [[ "$(grab R4_RESULT)" == "inconclusive_exhausted" ]] || fail_test T19 "loop-top mktemp fail must terminate inconclusive_exhausted, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]  || fail_test T19 "HARD HOLD MUST be reached when a cycle stream cannot be staged (no orphaned canary)"
    grep -q 'kind=mktemp_failed' <<<"$OUT"          || fail_test T19 "the mktemp failure should be classified in the cycle line"
    grep -q 'HOOK_CALLED result=inconclusive_exhausted' "$CALL_LOG" || fail_test T19 "FX_HARD_HOLD orphan-sweep hook MUST run"
    [[ "$RC" -eq 1 ]]                               || fail_test T19 "non-clean terminal should exit 1, rc=$RC"
    # ZERO orphan side effects — the whole point of routing to HARD HOLD.
    grep -q 'kd comment add' "$CALL_LOG"   && fail_test T19 "must NOT touch the canary when a cycle cannot even be staged"
    grep -q 'gb decision create' "$CALL_LOG" && fail_test T19 "must NOT open a decision"
    grep -q 'R3_CALL' "$CALL_LOG"          && fail_test T19 "must NOT file a bug"
    grep -q 'R2_CAPTURE' "$CALL_LOG"       && fail_test T19 "must NOT capture (nowhere writable to stage evidence)"
    unset MKTEMP_FAIL_GLOB FX_HARD_HOLD
    ok_if_clean T19
    teardown
}

# ===========================================================================
# T20: [CRITICAL] capture_forensics markers mktemp FAILS on a rung_failure — the
#      ALREADY-classified failure must NOT be lost to a set -e abort. The driver
#      falls back to the raw cycle stream as the markers ref, still runs R2 + R3,
#      fires the hook, and reaches HARD HOLD.
# ===========================================================================
test_markers_mktemp_failure_still_holds() {
    setup
    mark_start
    write_failing_mktemp "$BIN"
    export MKTEMP_FAIL_GLOB="r4-markers"
    export FX_HARD_HOLD=hard_hold_hook
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_netns_fail

    [[ "$(grab R4_RESULT)" == "rung_failure" ]] || fail_test T20 "classified rung_failure must survive a markers-mktemp failure, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]         || fail_test T20 "HARD HOLD MUST be reached (no orphaned canary)"
    grep -q 'HOOK_CALLED result=rung_failure' "$CALL_LOG" || fail_test T20 "FX_HARD_HOLD hook MUST run"
    # Evidence not lost: R2 still runs (markers falls back to the raw stream) + R3 files.
    grep -q 'R2_CAPTURE' "$CALL_LOG"            || fail_test T20 "R2 capture must still run on the markers-mktemp fallback"
    [[ "$(grab R4_FORENSIC_BUNDLE)" == "gd-FAKEBUNDLE1" ]] || fail_test T20 "R2 bundle must still be captured"
    [[ "$(grab R4_FILED_BUG)" == "gd-SKKYDVVNQU" ]]       || fail_test T20 "R3 bug must still be filed"
    unset MKTEMP_FAIL_GLOB FX_HARD_HOLD
    ok_if_clean T20
    teardown
}

# ===========================================================================
# T21: [CRITICAL] handle_rung_failure rungs_file mktemp FAILS — R3 needs the
#      rungs ladder on disk, so it is SKIPPED, but the classified rung_failure +
#      the already-captured R2 evidence are preserved and the driver STILL
#      reaches HARD HOLD (losing the bug is acceptable; losing HARD HOLD is not).
# ===========================================================================
test_rungs_mktemp_failure_skips_r3_still_holds() {
    setup
    mark_start
    write_failing_mktemp "$BIN"
    export MKTEMP_FAIL_GLOB="r4-rungs"
    export FX_HARD_HOLD=hard_hold_hook
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_netns_fail

    [[ "$(grab R4_RESULT)" == "rung_failure" ]] || fail_test T21 "classified rung_failure must survive a rungs-mktemp failure, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]         || fail_test T21 "HARD HOLD MUST be reached"
    grep -q 'HOOK_CALLED result=rung_failure' "$CALL_LOG" || fail_test T21 "FX_HARD_HOLD hook MUST run"
    # R2 evidence captured BEFORE the rungs failure; R3 skipped (no ladder to hand it).
    [[ "$(grab R4_FORENSIC_BUNDLE)" == "gd-FAKEBUNDLE1" ]] || fail_test T21 "R2 evidence must still be captured before the rungs mktemp failure"
    grep -q 'R3_CALL' "$CALL_LOG"               && fail_test T21 "R3 must be SKIPPED when the rungs file cannot be staged"
    [[ -z "$(grab R4_FILED_BUG)" ]]             || fail_test T21 "R4_FILED_BUG must be empty when R3 is skipped, got '$(grab R4_FILED_BUG)'"
    unset MKTEMP_FAIL_GLOB FX_HARD_HOLD
    ok_if_clean T21
    teardown
}

# ===========================================================================
# T22: [MAJOR] kd `comment add` FAILS on the SUCCESS path — the `|| echo …`
#      guard at handle_success is load-bearing (without it, set -e aborts before
#      the decision + HARD HOLD). The clean cycle must still classify success,
#      still attempt the canary comment, still open the decision, fire the
#      FX_HARD_HOLD hook, and reach HARD HOLD.
# ===========================================================================
test_kd_comment_failure_success_path_still_holds() {
    setup
    mark_start
    write_kd_comment_failing "${BIN}/kd"          # override the kd PATH stub
    export FX_HARD_HOLD=hard_hold_hook
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_full_ok

    [[ "$(grab R4_RESULT)" == "success" ]] || fail_test T22 "clean cycle must still classify success when the canary comment blips, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]    || fail_test T22 "HARD HOLD MUST be reached even when kd comment add fails on the success path"
    grep -q 'HOOK_CALLED result=success' "$CALL_LOG" || fail_test T22 "FX_HARD_HOLD hook MUST run on the success path"
    grep -q 'kd comment add ka-0000001355' "$CALL_LOG" || fail_test T22 "the clean-cycle canary comment must still be attempted"
    [[ "$(grab R4_DECISION)" == "created" ]] || fail_test T22 "gb decision must still open after the comment blip"
    [[ "$RC" -eq 0 ]]                      || fail_test T22 "a clean cycle is still success even if the comment blip'd, rc=$RC"
    unset FX_HARD_HOLD
    ok_if_clean T22
    teardown
}

# ===========================================================================
# T23: [MAJOR] kd `comment add` FAILS on the inconclusive_exhausted path — the
#      `|| echo …` guard at the exhaustion terminal is load-bearing. A
#      never-conclusive run (non-empty unmeasured tail → capture + comment fire)
#      must still fire the FX_HARD_HOLD hook and reach HARD HOLD.
# ===========================================================================
test_kd_comment_failure_inconclusive_path_still_holds() {
    setup
    mark_start
    write_kd_comment_failing "${BIN}/kd"
    export FX_HARD_HOLD=hard_hold_hook
    DRIVER_ARGS=(--max-cycles 2)
    run_driver stream_unmeasured_tail             # non-empty stream, never conclusive

    [[ "$(grab R4_RESULT)" == "inconclusive_exhausted" ]] || fail_test T23 "unmeasured tail must exhaust inconclusive, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]  || fail_test T23 "HARD HOLD MUST be reached even when the exhaustion canary comment blips"
    grep -q 'HOOK_CALLED result=inconclusive_exhausted' "$CALL_LOG" || fail_test T23 "FX_HARD_HOLD hook MUST run on the inconclusive_exhausted path"
    grep -q 'kd comment add ka-0000001355' "$CALL_LOG" || fail_test T23 "the exhaustion canary comment must still be attempted"
    [[ "$RC" -eq 1 ]] || fail_test T23 "exhausted should exit 1, rc=$RC"
    unset FX_HARD_HOLD
    ok_if_clean T23
    teardown
}

# ===========================================================================
# T24: [MINOR] a value flag given as the final (value-less) arg must die() with
#      rc 2 (the intended diagnostic), NOT a bare set -e `shift 2` abort (rc 1).
# ===========================================================================
test_trailing_valueless_flag_dies() {
    setup
    mark_start
    local rc
    for flag in --max-cycles --canary-task --epic; do
        FX_CYCLE_RUNNER=true bash "$SUT" "$flag" >/dev/null 2>&1
        rc=$?
        [[ "$rc" -eq 2 ]] || fail_test T24 "trailing value-less '$flag' should die rc 2, got $rc"
    done
    ok_if_clean T24
    teardown
}

# ===========================================================================
# T25: [MINOR] R3 filer path does NOT exist (distinct from T12, where the filer
#      is present but exits non-zero) — the outer-else. The driver must capture
#      R2 evidence, file NO bug, leave R4_FILED_BUG empty, and STILL HARD HOLD.
# ===========================================================================
test_r3_filer_not_found_still_holds() {
    setup
    mark_start
    export R4_FILE_BUG="${TMP}/nonexistent-r3-filer.sh"   # a path that does not exist
    DRIVER_ARGS=(--max-cycles 5)
    run_driver stream_netns_fail

    [[ "$(grab R4_RESULT)" == "rung_failure" ]] || fail_test T25 "expected rung_failure, OUT:\n$OUT"
    [[ "$(grab R4_HARD_HOLD)" == "1" ]]         || fail_test T25 "HARD HOLD MUST be reached when the R3 filer path does not exist"
    [[ -z "$(grab R4_FILED_BUG)" ]]             || fail_test T25 "no filer → R4_FILED_BUG must be empty, got '$(grab R4_FILED_BUG)'"
    grep -q 'R3_CALL' "$CALL_LOG"               && fail_test T25 "R3 must NOT be invoked when the filer path does not exist"
    [[ "$(grab R4_FORENSIC_BUNDLE)" == "gd-FAKEBUNDLE1" ]] || fail_test T25 "R2 evidence must still be captured"
    ok_if_clean T25
    teardown
}

# --- shellcheck (if available) ----------------------------------------------
test_shellcheck_clean() {
    if command -v shellcheck >/dev/null 2>&1; then
        if shellcheck -x -S warning "$SUT" "${SCRIPT_DIR}/canary-loop-driver_test.sh" >/dev/null 2>&1; then
            pass_test "shellcheck"
        else
            fail_test "shellcheck" "shellcheck reported issues"
            shellcheck -x -S warning "$SUT" "${SCRIPT_DIR}/canary-loop-driver_test.sh" || true
        fi
    else
        echo "SKIP: shellcheck not installed"
    fi
}

echo "== canary-loop-driver.sh tests =="
test_success_marks_and_decides
test_rung_failure_captures_files_holds_no_retry
test_silent_pass_guard_redrives_then_succeeds
test_bounded_cap_exhausts_inconclusive
test_infra_flake_retries_then_succeeds
test_failure_after_inconclusive_holds_immediately
test_dry_run_no_mutations
test_arg_validation
test_harness_only_no_node_calls
test_hard_hold_hook_invoked
test_r3_created_unlinked_still_holds
test_r3_failure_still_holds
test_dry_run_failure_still_holds
test_r2_degraded_uses_dir_ref
test_decision_failure_still_marks_and_holds
test_silent_pass_guard_is_per_rung
test_non_gating_skip_still_succeeds
test_default_max_cycles
test_looptop_mktemp_failure_hard_holds
test_markers_mktemp_failure_still_holds
test_rungs_mktemp_failure_skips_r3_still_holds
test_kd_comment_failure_success_path_still_holds
test_kd_comment_failure_inconclusive_path_still_holds
test_trailing_valueless_flag_dies
test_r3_filer_not_found_still_holds
test_shellcheck_clean

echo
echo "PASS=${PASS} FAIL=${FAIL}"
[[ $FAIL -eq 0 ]]
