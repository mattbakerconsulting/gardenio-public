#!/usr/bin/env bash
# canary-loop-driver.sh — Rig R4 (ka-0000006231).
#
# The BOUNDED self-loop driver that ties the gVisor freeze→thaw canary rig
# together. It drives at most N freeze→thaw cycles of the gvisor-poc canary
# (role ka-0000003266, redrive task ka-0000001355) and, on each cycle, harvests
# the assertion ladder into a verdict, then acts ONCE and HARD HOLDs:
#
#   • ALL gating rungs positively ok  → mark the canary a CLEAN cycle
#                                        + open a gb decision SUCCESS (go/no-go)
#                                        + HARD HOLD.
#   • ANY rung definitively FAILS     → R2 forensic capture (ka-0000006948)
#                                        + R3 file deduped next-layer bug
#                                          (ka-0000004124)
#                                        + HARD HOLD.  NO same-layer auto-retry.
#   • cycle inconclusive (infra flake  → retry (bounded by the cap). This is NOT
#     OR a required rung never           a same-layer retry: no freeze→thaw
#     MEASURED — a "silent pass")        assertion FAILED; the layer was simply
#                                        not exercised, so re-driving to actually
#                                        MEASURE it is correct and is exactly what
#                                        bug ka-0000002907 demands.
#   • cap exhausted, no conclusive      → R2 capture the last stream (evidence)
#     cycle                               + note the canary + HARD HOLD. NO R3
#                                          bug (no classified rung failure to
#                                          file). R4_RESULT=inconclusive_exhausted.
#
# ── HARD HOLD (one canary, zero orphans, prod untouched) ─────────────────────
#   HARD HOLD means: STOP. The driver breaks the loop and returns. It never
#   respawns a canary and never retries a *classified rung failure*. The
#   freezeThaw flag / controller / prod are NEVER touched — like R1/R2/R3 this
#   driver is HARNESS-ONLY: it drives an INJECTED cycle runner, sources the R1
#   classifier + R2 forensic lib, shells out to the R3 bug filer, and mutates
#   only beads (a canary comment, a gb decision, an R3 bug). The actual on-node
#   canary driving is the caller's FX_CYCLE_RUNNER; on-node verification belongs
#   to the gVisor doneness gardener, not here.
#
# ── WHY "NO same-layer auto-retry" but retry-on-inconclusive is CORRECT ───────
#   The anti-pattern the epic forbids (bug ka-0000002907) is spinning the SAME
#   layer after it FAILED an assertion — e.g. re-applying an L3 route that just
#   raced the CNI DEL (proven-failing 4×). R4 never does that: a definitive rung
#   FAILURE goes straight to capture→file→HOLD.
#   An INCONCLUSIVE cycle is the opposite: either the harness could not even
#   attempt the cycle (infra flake — FX_CYCLE_RUNNER exited non-zero, no
#   assertion ran), or R1 returned "no rung failed" but a GATING rung is `skip`
#   because its sentinel was never emitted (acks/work/ram not measured). R1
#   returns success on an all-`skip` tail, which is precisely the SILENT PASS the
#   bug warns about ("a canary that succeeds WITHOUT positive evidence must be
#   re-driven"). Re-driving to MEASURE the signal is the required fix, not the
#   forbidden retry.
#
# ── USAGE ────────────────────────────────────────────────────────────────────
#   FX_CYCLE_RUNNER=drive_one_cycle \
#     scripts/gvisor/canary-loop-driver.sh [--max-cycles 5] \
#       [--canary-task ka-0000001355] [--epic ka-0000000867] [--dry-run]
#
#   FX_CYCLE_RUNNER (REQUIRED) — a function OR binary that drives ONE freeze→thaw
#     cycle and writes the COMBINED FT_* sentinel stream (freeze node stdout +
#     thaw node stdout + the harness's post-thaw rung-5/6/7 observations) to
#     stdout. It is called as:  FX_CYCLE_RUNNER <cycle-index>
#     Exit-code contract:
#       0        → the cycle was driven; its FT_* stream is authoritative.
#                  Classify it (R1) and act on the verdict.
#       non-zero → INFRA FLAKE: the driver could not attempt the cycle (node
#                  unreachable, canary spawn failed, pre-flight failed). No
#                  freeze→thaw assertion was evaluated ⇒ INCONCLUSIVE ⇒ retry.
#
# ── RETURN CHANNEL (stdout sentinels; last block is machine-readable) ─────────
#   R4_CYCLE=<n> ...            per-cycle progress line
#   R4_RESULT=success|rung_failure|inconclusive_exhausted
#   R4_CYCLES_RUN=<n>
#   R4_FIRST_FAILED_RUNG=<name|none>
#   R4_FORENSIC_BUNDLE=<id|>    (rung_failure / inconclusive_exhausted)
#   R4_FILED_BUG=<id|>          (rung_failure; from R3_RESULT)
#   R4_DECISION=created|        (success)
#   R4_HARD_HOLD=1              always emitted at the terminal state
#
# Dependency injection (mirrors R1/R2/R3 so the hermetic test drives it with
# fakes — no daemon, no node, no cluster, no prod):
#   $KD / $GB               kd / gb binaries (default kd / gb).
#   FX_CYCLE_RUNNER         the one-cycle driver (see above).
#   FX_HARD_HOLD            optional hook fn/binary, called `FX_HARD_HOLD <result>`
#                           at HARD HOLD (e.g. the caller's orphan-sweep). Absent
#                           ⇒ HARD HOLD is simply "stop looping" (nothing
#                           respawns), which is the safe default.
#   FT_RUNGS_LIB            R1 classifier lib to source (default: sibling
#                           freeze-thaw-rungs.sh).
#   FX_FORENSICS_LIB        R2 forensic lib to source (default: the e2e-scripts
#                           lib-gvisor-forensics.sh).
#   R4_FILE_BUG             R3 bug-filer script (default: sibling
#                           file-next-layer-bug.sh).
#   FX_NODE_RUNNER          passed through to R2 so its node-side collectors can
#                           snapshot the failing netns/runsc/cni (absent ⇒ R2
#                           writes skip notes; still bundles the marker set).

set -euo pipefail

KD="${KD:-kd}"
GB="${GB:-gb}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Canonical R4 contract targets (overridable for testability + reuse).
DEFAULT_CANARY_TASK="ka-0000001355"   # the freeze→thaw canary re-drive task
DEFAULT_EPIC="ka-0000000867"          # Phase-4 gVisor rollout epic
DEFAULT_MAX_CYCLES=5                  # bounded loop cap (~5 per the bead)

# Lib / helper locations (DI-overridable).
FT_RUNGS_LIB="${FT_RUNGS_LIB:-${SCRIPT_DIR}/freeze-thaw-rungs.sh}"
FX_FORENSICS_LIB="${FX_FORENSICS_LIB:-${SCRIPT_DIR}/../../gardenio/tests/e2e/scripts/lib-gvisor-forensics.sh}"
R4_FILE_BUG="${R4_FILE_BUG:-${SCRIPT_DIR}/file-next-layer-bug.sh}"

# (Bundle-link rendering to a viewer URL is R3's job — R4 passes the R2 bundle id
# or staging-dir ref straight through to file-next-layer-bug.sh, which renders it
# with the mandatory /beads/ prefix. R4 does not build viewer URLs itself.)

# The rungs that MUST be positively `ok` for a cycle to count as CLEAN. This is
# the canary's own success definition (task ka-0000001355: "/work + RAM intact +
# gb/coop acks"). cni_confirmed is DELIBERATELY excluded — R1 classifies it as a
# NON-GATING diagnostic (ok|skip only, never fail), and the controller never
# asserts it. A rung that is merely `skip` here (sentinel never emitted) is a
# NOT-MEASURED signal ⇒ INCONCLUSIVE ⇒ re-drive (the silent-pass guard). This is
# a COMPLETENESS check, not a network fix; it reimplements no refuted L3 remedy.
R4_REQUIRED_OK_RUNGS="${R4_REQUIRED_OK_RUNGS:-snapshot_persisted restore_rc0 netns_routable rehydrate_acks work_intact ram_intact}"

die() { echo "canary-loop-driver: $*" >&2; exit 2; }

# --- arg parse --------------------------------------------------------------
MAX_CYCLES="$DEFAULT_MAX_CYCLES"
CANARY_TASK="$DEFAULT_CANARY_TASK"
EPIC="$DEFAULT_EPIC"
DRY_RUN=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        # Guard the value flags BEFORE `shift 2`: a trailing value-less flag
        # (e.g. `--max-cycles` given as the final arg) leaves $# == 1, so a bare
        # `shift 2` fails under `set -e` and aborts arg-parse with a bare rc 1 —
        # bypassing die()'s rc-2 diagnostic. Require the value explicitly so the
        # operator gets the intended error.
        --max-cycles)  [[ $# -ge 2 ]] || die "--max-cycles requires a value"; MAX_CYCLES="$2"; shift 2 ;;
        --canary-task) [[ $# -ge 2 ]] || die "--canary-task requires a value"; CANARY_TASK="$2"; shift 2 ;;
        --epic)        [[ $# -ge 2 ]] || die "--epic requires a value"; EPIC="$2"; shift 2 ;;
        --dry-run)     DRY_RUN=1; shift ;;
        -h|--help)     sed -n '2,90p' "$0"; exit 0 ;;
        *)             die "unknown arg: $1" ;;
    esac
done

case "$MAX_CYCLES" in
    ''|*[!0-9]*) die "--max-cycles must be a positive integer (got '${MAX_CYCLES}')" ;;
esac
[[ "$MAX_CYCLES" -ge 1 ]] || die "--max-cycles must be >= 1 (got ${MAX_CYCLES})"
[[ -n "${FX_CYCLE_RUNNER:-}" ]] || die "FX_CYCLE_RUNNER is required (fn or binary that drives one freeze→thaw cycle)"

[[ -f "$FT_RUNGS_LIB" ]] || die "R1 classifier lib not found: $FT_RUNGS_LIB"

# --- source R1 (pure classifier) + R2 (forensic capture) --------------------
# Both are side-effect-free on source (R1 defines functions only; R2 defines
# functions + FX_* defaults). R2 is optional-ish: if it is missing we degrade to
# capturing the raw stream to a local file, but the canonical wiring sources it.
# shellcheck source=scripts/gvisor/freeze-thaw-rungs.sh
source "$FT_RUNGS_LIB"

FORENSICS_AVAILABLE=0
if [[ -f "$FX_FORENSICS_LIB" ]]; then
    # R2 uses FX_GB for its bundle calls; align it with our $GB so the same
    # (possibly shimmed) gb is used everywhere.
    export FX_GB="${FX_GB:-$GB}"
    # shellcheck source=/dev/null
    source "$FX_FORENSICS_LIB" && FORENSICS_AVAILABLE=1
fi

# --- small helpers ----------------------------------------------------------
ts_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# r4_field <block> <KEY> — echo the value of a KEY=value line from a marker
# block (last-wins, matching the node/classifier re-emit semantics). ABORT-PROOF:
# a KEY that is absent makes grep no-match (rc 1) which, under `set -euo
# pipefail` inside a `v="$(r4_field …)"` command substitution, would trip set -e
# and kill the whole driver BEFORE it can reach r4_hard_hold — the exact
# HARD-HOLD-not-reached failure the rig forbids. The `{ grep …|| true; }` wrap
# neutralizes the no-match so the field-extractor always returns 0.
r4_field() {
    printf '%s\n' "$1" | { grep -E "^${2}=" || true; } | tail -n1 | cut -d= -f2-
}

# run — execute a command (returning its output/rc), or print it under --dry-run
# without executing. Used where the caller needs the command's stdout (the R3
# filer).
run() {
    if [[ "$DRY_RUN" -eq 1 ]]; then
        printf 'DRY-RUN:'; printf ' %q' "$@"; printf '\n'
        return 0
    fi
    "$@"
}

# run_quiet — a fire-and-forget mutation whose output we don't need. In real mode
# its stdout/stderr are dropped (they'd pollute the R4_* sentinel stream); in
# dry-run it prints the DRY-RUN line (to the sentinel stream, so a caller can see
# exactly what WOULD have run) and executes nothing.
run_quiet() {
    if [[ "$DRY_RUN" -eq 1 ]]; then
        printf 'DRY-RUN:'; printf ' %q' "$@"; printf '\n'
        return 0
    fi
    "$@" >/dev/null 2>&1
}

# r4_hard_hold <result> — the terminal action. Emits the HARD HOLD sentinel and
# invokes the optional caller hook. It NEVER respawns and NEVER retries.
r4_hard_hold() {
    local result="$1"
    if [[ -n "${FX_HARD_HOLD:-}" ]]; then
        if [[ "$DRY_RUN" -eq 1 ]]; then
            printf 'DRY-RUN: %q %q\n' "$FX_HARD_HOLD" "$result"
        else
            # Fail-soft: a hook error must not turn a clean HARD HOLD into a crash.
            "$FX_HARD_HOLD" "$result" || echo "canary-loop-driver: FX_HARD_HOLD hook exited non-zero (HARD HOLD still enforced)" >&2
        fi
    fi
    echo "R4_HARD_HOLD=1"
}

# --- terminal sentinels (populated along the way) ---------------------------
R4_RESULT=""
R4_FIRST_FAILED_RUNG="none"
R4_FORENSIC_BUNDLE=""
R4_BUNDLE_REF=""
R4_FILED_BUG=""
R4_DECISION=""

# capture_forensics <stream-file> <classify-block> <rung> <reason>
# Sources-in R2's gvisor_forensic_capture (or degrades to keeping the raw
# stream). Sets two GLOBALS (it must NOT run in a $(...) subshell, or the global
# assignments would be lost):
#   R4_FORENSIC_BUNDLE — the created bundle bead id (empty if gb degraded).
#   R4_BUNDLE_REF      — the evidence pointer R3 / the operator should follow:
#                        the bundle id when present, else the local staging dir,
#                        else the raw marker file — so evidence is never dropped.
capture_forensics() {
    local stream_file="$1" block="$2" rung="$3" reason="$4"
    local markers_file
    R4_BUNDLE_REF=""
    # R2's markers artifact should carry BOTH the raw lifecycle sentinels and the
    # classified rung verdicts, so the bundle is self-describing. ABORT-PROOF: a
    # bare mktemp here (reached AFTER a rung_failure verdict is already
    # classified, and on the inconclusive_exhausted terminal) would, on failure,
    # trip set -e and lose the classified failure BEFORE R2/R3/HARD HOLD. Guard
    # it (2>/dev/null + writability + /tmp fallback); if no temp file can be made
    # at all, fall back to the raw cycle stream as the markers ref so the
    # evidence is NOT lost, then continue to r4_hard_hold.
    markers_file="$(mktemp "${TMPDIR:-/tmp}/r4-markers.XXXXXX" 2>/dev/null || true)"
    if [[ -z "$markers_file" || ! -w "$markers_file" ]]; then
        markers_file="$(mktemp "/tmp/r4-markers.XXXXXX" 2>/dev/null || true)"
    fi
    if [[ -n "$markers_file" && -w "$markers_file" ]]; then
        { cat "$stream_file"; printf '\n# ---- R1 classification ----\n'; printf '%s\n' "$block"; } >"$markers_file" 2>/dev/null || true
    else
        echo "canary-loop-driver: could not create a writable markers temp file (TMPDIR='${TMPDIR:-/tmp}') — using the raw cycle stream as the evidence markers ref; classified verdict preserved" >&2
        markers_file="$stream_file"
    fi

    if [[ "$FORENSICS_AVAILABLE" -eq 1 ]]; then
        if [[ "$DRY_RUN" -eq 1 ]]; then
            printf 'DRY-RUN: gvisor_forensic_capture (rung=%q reason=%q origin=%q)\n' "$rung" "$reason" "$CANARY_TASK"
        else
            # Call it BARE (per R2's contract: it returns 1 on the common degraded
            # path, which is NOT an error). Read the FT_FORENSIC_* sentinels; do
            # not branch on the exit code.
            FX_ORIGIN_BEAD="$CANARY_TASK" \
            FX_RUNG="$rung" FX_REASON="$reason" \
            FX_MARKERS="$markers_file" \
                gvisor_forensic_capture >/dev/null 2>&1 || true
            R4_FORENSIC_BUNDLE="${FT_FORENSIC_BUNDLE:-}"
            if [[ -n "$R4_FORENSIC_BUNDLE" ]]; then
                R4_BUNDLE_REF="$R4_FORENSIC_BUNDLE"
            elif [[ -n "${FT_FORENSIC_DIR:-}" ]]; then
                # No bundle (gb degraded), but evidence IS staged on disk — surface
                # the dir so it is not lost. R3 accepts an opaque ref for --bundle.
                R4_BUNDLE_REF="$FT_FORENSIC_DIR"
            fi
        fi
    fi
    # Degraded / no-R2 fallback: keep the marker file itself as the evidence ref.
    [[ -n "$R4_BUNDLE_REF" ]] || R4_BUNDLE_REF="$markers_file"
}

# handle_rung_failure <stream-file> <block> <cycle>
# R2 capture → R3 file (deduped) → HARD HOLD. No retry.
handle_rung_failure() {
    local stream_file="$1" block="$2" cycle="$3"
    local rung reason r3_out r3_rc rungs_file

    rung="$(r4_field "$block" FT_FIRST_FAILED_RUNG)"
    reason="$(r4_field "$block" FT_FIRST_FAILED_REASON)"
    R4_FIRST_FAILED_RUNG="$rung"
    echo "R4_CYCLE=${cycle} verdict=rung_failure rung=${rung} reason=${reason}"

    # R2 — snapshot the ephemeral failure evidence BEFORE the HARD HOLD. Sets the
    # globals R4_FORENSIC_BUNDLE + R4_BUNDLE_REF (called BARE, not in $(...)).
    capture_forensics "$stream_file" "$block" "$rung" "$reason"
    echo "R4_FORENSIC_BUNDLE=${R4_FORENSIC_BUNDLE}"

    # R3 — file (or dedup-comment) the next-layer bug, keyed on (rung + reason).
    # Pass the R1 classification block as the embedded rung ladder, and the
    # failing rung name as the classified --mode (R1 IS the classifier, so the
    # first-failed rung is the most stable dedup key).
    #
    # ABORT-PROOF rungs_file: a bare `rungs_file=$(mktemp …)` under `set -euo
    # pipefail` would, on a mktemp failure, trip set -e and lose the ALREADY
    # classified rung_failure BEFORE r4_hard_hold. Guard it (2>/dev/null +
    # writability + /tmp fallback). If no temp file can be staged we SKIP R3 (it
    # needs the rungs ladder on disk) but STILL HARD HOLD: the rung_failure
    # classification and the R2 evidence bundle are already established, so the
    # canary is not orphaned — losing the bug is acceptable, losing HARD HOLD is
    # not.
    rungs_file="$(mktemp "${TMPDIR:-/tmp}/r4-rungs.XXXXXX" 2>/dev/null || true)"
    if [[ -z "$rungs_file" || ! -w "$rungs_file" ]]; then
        rungs_file="$(mktemp "/tmp/r4-rungs.XXXXXX" 2>/dev/null || true)"
    fi
    if [[ -n "$rungs_file" && -w "$rungs_file" ]]; then
        printf '%s\n' "$block" >"$rungs_file"
        if [[ -x "$R4_FILE_BUG" || -f "$R4_FILE_BUG" ]]; then
            # OK: disable errexit only to capture the R3 bug-filer's exit code in
            # r3_rc; success/failure is handled explicitly below, then set -e is restored.
            set +e
            r3_out="$(KD="$KD" GB="$GB" run bash "$R4_FILE_BUG" \
                --rung "$rung" --reason "$reason" --bundle "$R4_BUNDLE_REF" \
                --mode "$rung" --cycle "$cycle" --canary "$CANARY_TASK" \
                --canary-task "$CANARY_TASK" --epic "$EPIC" \
                --rungs-file "$rungs_file" 2>&1)"
            r3_rc=$?
            set -e
            printf '%s\n' "$r3_out" | sed 's/^/    [R3] /'
            if [[ "$r3_rc" -eq 0 ]]; then
                # Match ALL R3 success verbs: `created`, `deduped`, AND
                # `created_unlinked` (R3's WARN-recovery path when a dep edge
                # didn't wire — file-next-layer-bug.sh:292; it still returns 0
                # and the bug exists). `created[a-z_]*|deduped` covers present +
                # future variants. The `|| true`s keep a no-match (e.g.
                # --dry-run, whose r3_out is a `DRY-RUN:` echo, not an R3_RESULT
                # line) from tripping set -e and aborting BEFORE r4_hard_hold —
                # the HARD-HOLD-not-reached hazard.
                R4_FILED_BUG="$(printf '%s\n' "$r3_out" \
                    | { grep -oE 'R3_RESULT=(created[a-z_]*|deduped) bug=(gd|ka)-[A-Za-z0-9]+' || true; } \
                    | { grep -oE '(gd|ka)-[A-Za-z0-9]+' || true; } | tail -n1)"
                # Surface the degraded-but-filed case so the operator (and the
                # next dedup pass, which repairs the edge) knows an edge is
                # missing.
                if grep -q 'R3_RESULT=created_unlinked' <<<"$r3_out"; then
                    echo "canary-loop-driver: R3 filed bug ${R4_FILED_BUG} but a dep edge did not wire (created_unlinked) — the next dedup pass repairs it; NOT re-filing" >&2
                fi
            else
                echo "canary-loop-driver: R3 bug filing exited ${r3_rc} (evidence bundle still captured: ${R4_BUNDLE_REF})" >&2
            fi
        else
            echo "canary-loop-driver: R3 filer not found (${R4_FILE_BUG}) — evidence at ${R4_BUNDLE_REF}, no bug filed" >&2
        fi
        rm -f "$rungs_file"
    else
        echo "canary-loop-driver: could not create a writable rungs temp file (TMPDIR='${TMPDIR:-/tmp}') — skipping R3 bug filing; classified rung_failure + R2 evidence (${R4_BUNDLE_REF}) preserved, HARD HOLD still enforced" >&2
    fi
    echo "R4_FILED_BUG=${R4_FILED_BUG}"

    R4_RESULT="rung_failure"
    r4_hard_hold "$R4_RESULT"
}

# handle_success <block> <cycle>
# Mark the canary a CLEAN cycle + open the gb decision SUCCESS + HARD HOLD.
handle_success() {
    local block="$1" cycle="$2"
    echo "R4_CYCLE=${cycle} verdict=success all_gating_rungs_ok=1"

    # Mark the canary a clean cycle (a COMMENT — R4 is harness-only and does not
    # own the canary's status; flipping it is the gardener's call after on-node
    # verification). The comment records the clean cycle + the full rung ladder.
    local canary_note
    canary_note="$(cat <<EOF
CLEAN CYCLE (auto-observed by Rig R4 canary-loop-driver, $(ts_now)) — freeze→thaw cycle ${cycle}/${MAX_CYCLES} drove a FULLY-CLEAN pass: every gating rung positively ok (${R4_REQUIRED_OK_RUNGS// /, }). No rung failed and none were left unmeasured — this is NOT a silent pass.

R1 rung ladder:
$(printf '%s\n' "$block" | { grep -E '^FT_RUNG_' || true; })

HARD HOLD enforced — no further cycles, prod/freezeThaw-flag untouched, zero orphans. A gb decision SUCCESS (go/no-go for Phase-4 default-on) has been opened for the operator.
EOF
)"
    run_quiet "$KD" comment add "$CANARY_TASK" "$canary_note" \
        || echo "canary-loop-driver: kd comment on canary ${CANARY_TASK} failed (non-fatal; decision still opened)" >&2

    # gb decision SUCCESS — genuinely divergent next-actions for the operator
    # (the epic's own children). --no-wait: R4 is an autonomous driver and MUST
    # NOT block on gb yield; it HARD HOLDs regardless of the answer. The decision
    # surfaces asynchronously.
    local prompt
    prompt="gVisor freeze→thaw canary drove a FULLY-CLEAN cycle (${cycle}/${MAX_CYCLES}) in the privileged ns — all gating rungs ok (snapshot/restore/netns/acks/work/ram), positively measured (not a silent pass). Pick the next action for the Phase-4 gVisor rollout (epic ${EPIC}):"
    if run_quiet "$GB" decision create --no-wait \
        --prompt "$prompt" \
        --option "flip_go_no_go:Proceed to Phase-4 default-on go/no-go (ka-0000001283) — take a fresh operator go/no-go on freezeThaw.enabled=true now:plan" \
        --option "perf_rebaseline_first:Run the C3 perf re-baseline (ka-0000006167) BEFORE any default-on flip:plan" \
        --option "hold:Hold at clean — bank the green cycle, keep the freezeThaw flag OFF, revisit later:report"; then
        R4_DECISION="created"
    else
        echo "canary-loop-driver: gb decision create failed (clean cycle still recorded on ${CANARY_TASK})" >&2
    fi
    echo "R4_DECISION=${R4_DECISION}"

    R4_RESULT="success"
    r4_hard_hold "$R4_RESULT"
}

# --- the bounded loop -------------------------------------------------------
echo "R4_START max_cycles=${MAX_CYCLES} canary=${CANARY_TASK} epic=${EPIC} dry_run=${DRY_RUN}"

CYCLES_RUN=0
LAST_STREAM_FILE=""
LAST_BLOCK=""

for (( cycle=1; cycle<=MAX_CYCLES; cycle++ )); do
    CYCLES_RUN="$cycle"

    # Stage the per-cycle FT_* stream. ABORT-PROOF: a BARE `stream_file=$(mktemp
    # …)` under `set -euo pipefail` would, on a mktemp failure (unwritable
    # TMPDIR, ENOSPC), trip set -e and kill the driver BEFORE it ever reaches
    # r4_hard_hold — the orphaned-canary / HARD-HOLD-not-reached state the rig
    # exists to prevent (the SAME bug class already fixed at the r4_field /
    # R3-parse sites, re-appearing here). Mirror sibling R2
    # (lib-gvisor-forensics.sh): 2>/dev/null + a writability check + a /tmp
    # fallback + an explicit terminal. If we cannot stage even one stream we
    # cannot drive a cycle, so the correct terminal is inconclusive_exhausted →
    # HARD HOLD (no R3 bug: no classified rung failure; the canary is untouched).
    stream_file="$(mktemp "${TMPDIR:-/tmp}/r4-cycle.XXXXXX" 2>/dev/null || true)"
    if [[ -z "$stream_file" || ! -w "$stream_file" ]]; then
        stream_file="$(mktemp "/tmp/r4-cycle.XXXXXX" 2>/dev/null || true)"
    fi
    if [[ -z "$stream_file" || ! -w "$stream_file" ]]; then
        echo "R4_CYCLE=${cycle} verdict=inconclusive kind=mktemp_failed — no writable temp dir (TMPDIR='${TMPDIR:-/tmp}'); cannot stage a freeze→thaw cycle stream. HARD HOLD (no orphaned canary, no bug filed)."
        R4_RESULT="inconclusive_exhausted"
        r4_hard_hold "$R4_RESULT"
        break
    fi

    # Drive ONE cycle via the injected runner. rc!=0 ⇒ infra flake (no assertion
    # evaluated) ⇒ inconclusive ⇒ retry.
    # OK: disable errexit only to capture the cycle runner's exit code in
    # runner_rc; rc!=0 is handled explicitly below as an infra-flake retry,
    # then set -e is restored.
    set +e
    "$FX_CYCLE_RUNNER" "$cycle" >"$stream_file" 2>/dev/null
    runner_rc=$?
    set -e

    if [[ "$runner_rc" -ne 0 ]]; then
        echo "R4_CYCLE=${cycle} verdict=inconclusive kind=infra_flake runner_rc=${runner_rc} — freeze→thaw layer never exercised; retrying (bounded)"
        LAST_STREAM_FILE="$stream_file"
        LAST_BLOCK=""
        continue
    fi

    # Classify via R1. rc==0 ⇒ no rung FAILED; rc!=0 ⇒ some rung failed.
    # OK: disable errexit only to capture ft_classify_rungs' exit code in
    # classify_rc; rc!=0 is handled explicitly below as a definitive rung
    # failure (HARD HOLD), then set -e is restored.
    set +e
    block="$(ft_classify_rungs "$stream_file")"
    classify_rc=$?
    set -e
    LAST_STREAM_FILE="$stream_file"
    LAST_BLOCK="$block"

    if [[ "$classify_rc" -ne 0 ]]; then
        # A rung DEFINITIVELY failed — capture, file, HARD HOLD. No retry.
        handle_rung_failure "$stream_file" "$block" "$cycle"
        break
    fi

    # No rung failed. Guard against the SILENT PASS: every REQUIRED rung must be
    # positively `ok`. A required rung that is `skip` (sentinel never emitted)
    # means the signal was not measured ⇒ inconclusive ⇒ re-drive.
    missing=""
    for req in $R4_REQUIRED_OK_RUNGS; do
        v="$(r4_field "$block" "FT_RUNG_${req}")"
        [[ "$v" == "ok" ]] || missing="${missing}${missing:+ }${req}=${v:-absent}"
    done

    if [[ -n "$missing" ]]; then
        echo "R4_CYCLE=${cycle} verdict=inconclusive kind=unmeasured_gating_rung not_ok=[${missing}] — silent-pass guard: re-driving to measure the signal (bounded)"
        continue
    fi

    # Fully clean, fully measured.
    handle_success "$block" "$cycle"
    break
done

# --- terminal: cap exhausted with no conclusive cycle -----------------------
if [[ -z "$R4_RESULT" ]]; then
    echo "R4_CYCLE=${CYCLES_RUN} verdict=inconclusive_exhausted — ${MAX_CYCLES} cycles ran without a conclusive (clean or failed) result"
    R4_RESULT="inconclusive_exhausted"
    if [[ -n "$LAST_STREAM_FILE" && -s "$LAST_STREAM_FILE" ]]; then
        # Capture the last stream so the operator has evidence of the flaky/never-
        # measured cycles. NO R3 bug: there is no classified rung FAILURE to file.
        capture_forensics "$LAST_STREAM_FILE" "${LAST_BLOCK:-}" "inconclusive_exhausted" "cap ${MAX_CYCLES} reached with no conclusive freeze->thaw cycle"
        echo "R4_FORENSIC_BUNDLE=${R4_FORENSIC_BUNDLE}"
        run_quiet "$KD" comment add "$CANARY_TASK" "INCONCLUSIVE-EXHAUSTED (Rig R4 canary-loop-driver, $(ts_now)) — ${MAX_CYCLES} freeze->thaw cycles ran without a conclusive clean-or-failed result (infra flakes and/or gating rungs never measured). Evidence: ${R4_BUNDLE_REF}. HARD HOLD enforced. No next-layer bug filed (no classified rung failure); re-drive with the harness signals (acks/work/ram) wired." \
            || echo "canary-loop-driver: kd comment on canary failed (non-fatal)" >&2
    fi
    r4_hard_hold "$R4_RESULT"
fi

# --- machine-readable terminal block ----------------------------------------
echo "R4_CYCLES_RUN=${CYCLES_RUN}"
echo "R4_RESULT=${R4_RESULT}"
echo "R4_FIRST_FAILED_RUNG=${R4_FIRST_FAILED_RUNG}"
echo "R4_FORENSIC_BUNDLE=${R4_FORENSIC_BUNDLE}"
echo "R4_FILED_BUG=${R4_FILED_BUG}"
echo "R4_DECISION=${R4_DECISION}"

# Exit 0 on a clean success; 1 on any non-clean terminal (rung failure or
# inconclusive-exhausted) so a caller can branch on the process rc as well.
[[ "$R4_RESULT" == "success" ]]
