#!/usr/bin/env bash
# file-next-layer-bug.sh — Rig R3 (ka-0000004124).
#
# On a gVisor freeze->thaw canary RUNG FAILURE, auto-file (or dedup-comment) the
# next-layer bug so the failure becomes a tracked, fixable work item instead of a
# lost log line. This is the helper the bounded canary driver R4 (ka-0000006231)
# invokes after R2 (ka-0000006948) has captured the forensic bundle, on the FIRST
# failing rung, before HARD HOLD.
#
# What it guarantees about the filed bug (modeled on ka-0000000357):
#   - captures the failing rung, the classified reason, and the R2 bundle link,
#   - blocks the canary task   ka-0000001355  (canary cannot go clean while open),
#   - is a child of the epic   ka-0000000867  (Phase-4 gVisor rollout),
#   - carries a stable dedup signature label  ft-sig:<hash>  and  ft-rung:<rung>.
#
# DEDUP: the signature is sha1(rung + normalized-reason). Volatile tokens
# (timestamps, node names, IPs, hex ids, byte sizes, counts) are stripped before
# hashing, so the SAME classified failure mode maps to the SAME signature across
# cycles even when the forensic details differ. Before filing, we look for an
# OPEN (not-closed) bug carrying that signature label; if one exists we COMMENT a
# recurrence note on it (with the fresh bundle) instead of duplicating. If the
# only prior match is closed (a recurrence after a fix that didn't hold), we file
# a fresh bug.
#
# HARNESS ONLY: this files/annotates beads. It never spawns a canary, never
# touches prod, and never reads or flips the freezeThaw flag. The kd/gb binaries
# are resolved via $KD / $GB so a test can shadow them on PATH (see the _test.sh).
#
# Usage:
#   file-next-layer-bug.sh --rung <name> --reason <text> --bundle <id|url> \
#       [--mode <classified-mode>] [--cycle <n>] [--node <name>] \
#       [--canary <bead-id>] [--rungs-file <path>] [--canary-task kd-...] \
#       [--epic kd-...] [--priority N] [--dry-run]
#
# Required:
#   --rung     name of the first failing rung (e.g. restore_rc0, netns_routable).
#   --reason   free-text reason string emitted alongside FT_RUNG_<rung>=fail.
#   --bundle   R2 forensic bundle: a bead id (kd-...) or a full viewer URL.
#
# Optional:
#   --mode        canonical classified failure mode from R1 (if R1 classified it).
#                 When given it is used as the signature basis instead of the
#                 normalized free-text reason — the most stable dedup key.
#   --cycle       driver cycle index (for the recurrence/comment log).
#   --node        node name where the failure landed (forensic context only).
#   --canary      the canary instance bead that observed the failure (context).
#   --rungs-file  a file containing the full FT_RUNG_* set to embed verbatim.
#   --canary-task the canary task the bug should BLOCK (default ka-0000001355).
#   --epic        the parent epic (default ka-0000000867).
#   --priority    filed-bug priority (default 1).
#   --dry-run     print the kd/gb commands that would run; change nothing.
#
# Output (last line, machine-readable for R4 to capture):
#   R3_RESULT=created bug=<bead-id> sig=<sig>
#   R3_RESULT=deduped bug=<bead-id> sig=<sig>

set -euo pipefail

KD="${KD:-kd}"
GB="${GB:-gb}"

# Canonical R3 contract targets (overridable for testability).
DEFAULT_CANARY_TASK="ka-0000001355"   # the freeze->thaw canary re-drive task
DEFAULT_EPIC="ka-0000000867"          # Phase-4 gVisor rollout epic
VIEWER_BASE="${R3_VIEWER_BASE:-https://gardenio.e2e.example.com}"
# Project that owns the filed bug AND scopes the dedup query. These MUST agree:
# the dedup `kd list` filters by project, and the filed bug is labelled with the
# same project, so a prior bug is always found regardless of the ambient
# $BOAT_PROJECT/$KD_PROJECT of the calling agent.
R3_PROJECT="${R3_PROJECT:-gardenio}"

die() { echo "file-next-layer-bug: $*" >&2; exit 2; }

# --- arg parse --------------------------------------------------------------
RUNG="" REASON="" MODE="" BUNDLE="" CYCLE="" NODE="" CANARY="" RUNGS_FILE=""
CANARY_TASK="$DEFAULT_CANARY_TASK" EPIC="$DEFAULT_EPIC" PRIORITY="1" DRY_RUN=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --rung)        RUNG="${2:-}"; shift 2 ;;
        --reason)      REASON="${2:-}"; shift 2 ;;
        --mode)        MODE="${2:-}"; shift 2 ;;
        --bundle)      BUNDLE="${2:-}"; shift 2 ;;
        --cycle)       CYCLE="${2:-}"; shift 2 ;;
        --node)        NODE="${2:-}"; shift 2 ;;
        --canary)      CANARY="${2:-}"; shift 2 ;;
        --rungs-file)  RUNGS_FILE="${2:-}"; shift 2 ;;
        --canary-task) CANARY_TASK="${2:-}"; shift 2 ;;
        --epic)        EPIC="${2:-}"; shift 2 ;;
        --priority)    PRIORITY="${2:-}"; shift 2 ;;
        --dry-run)     DRY_RUN=1; shift ;;
        -h|--help)     sed -n '2,60p' "$0"; exit 0 ;;
        *)             die "unknown arg: $1" ;;
    esac
done

[[ -n "$RUNG"   ]] || die "missing required --rung"
[[ -n "$REASON" ]] || die "missing required --reason"
[[ -n "$BUNDLE" ]] || die "missing required --bundle"

# --- reason normalization ---------------------------------------------------
# Collapse a forensic reason to its stable classifier shape so the dedup
# signature survives across cycles. Order matters: specific patterns first,
# bare-number stripping last (it would otherwise eat the digits in IPs/sizes).
normalize_reason() {
    printf '%s' "$1" \
    | tr '[:upper:]' '[:lower:]' \
    | sed -E \
        -e 's/[0-9]{4}-[0-9]{2}-[0-9]{2}[t ][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?z?/<ts>/g' \
        -e 's/\bip-[0-9]+-[0-9]+-[0-9]+-[0-9]+\b/<node>/g' \
        -e 's/\b([0-9]{1,3}\.){3}[0-9]{1,3}\b/<ip>/g' \
        -e 's/\b[0-9]+(\.[0-9]+)?[kmgt]i?b\b/<size>/g' \
        -e 's/\b0x[0-9a-f]+\b/<hex>/g' \
        -e 's/\b[0-9a-f]{8,64}\b/<hex>/g' \
        -e 's/\bx[0-9]+\b/x<n>/g' \
        -e 's/\b[0-9]+\b/<n>/g' \
        -e 's/[[:space:]]+/ /g' \
        -e 's/^[[:space:][:punct:]]+//' \
        -e 's/[[:space:][:punct:]]+$//'
}

# The signature basis: a caller-supplied classified --mode is the most stable
# key; otherwise fall back to the normalized free-text reason.
if [[ -n "$MODE" ]]; then
    SIG_BASIS="$(printf '%s' "$MODE" | tr '[:upper:]' '[:lower:]' | sed -E 's/[[:space:]]+/ /g; s/^ //; s/ $//')"
else
    SIG_BASIS="$(normalize_reason "$REASON")"
fi

SIG="$(printf '%s\n%s' "$RUNG" "$SIG_BASIS" | sha1sum | cut -c1-12)"
SIG_LABEL="ft-sig:${SIG}"
RUNG_LABEL="ft-rung:${RUNG}"

# --- bundle link rendering --------------------------------------------------
# Accept a bare bead id or a full URL. A bead id renders to the canonical viewer
# bundle URL — note the mandatory /beads/ path prefix (advice ka-0000003730).
render_bundle_link() {
    case "$1" in
        http://*|https://*) printf '%s' "$1" ;;
        kd-*)               printf '%s/beads/bundle/%s' "$VIEWER_BASE" "$1" ;;
        *)                  printf '%s' "$1" ;;   # opaque ref; surface as-is
    esac
}
BUNDLE_LINK="$(render_bundle_link "$BUNDLE")"

# --- optional embedded rung ladder -----------------------------------------
RUNGS_BLOCK=""
if [[ -n "$RUNGS_FILE" ]]; then
    [[ -f "$RUNGS_FILE" ]] || die "--rungs-file not found: $RUNGS_FILE"
    RUNGS_BLOCK="$(cat "$RUNGS_FILE")"
fi

ts_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# --- dedup: find a LIVE bug carrying this signature -------------------------
# Query by the signature label (project-pinned so the scope matches the label on
# the filed bug, regardless of the caller's ambient $BOAT_PROJECT), then keep
# only bugs in a NON-TERMINAL status. Both "closed" and "partial" are terminal
# for progression in kbeads (a partial bug satisfies its blocking deps just like
# closed), so a recurrence after either should file a FRESH bug, not comment on
# the dead one. Echoes the first live match id, or "" when there is genuinely no
# live dup. Returns non-zero ONLY on a query/transport failure so the caller can
# fail loud instead of mis-filing a duplicate.
find_live_dup() {
    local raw ids
    # Capture separately so a kd/transport failure is distinguishable from an
    # empty (no-dup) result. `kd list --json` prints "[]" on a clean empty set.
    if ! raw="$("$KD" list --type bug --project "$R3_PROJECT" \
                    --label "$SIG_LABEL" --limit 50 --json 2>/dev/null)"; then
        return 3
    fi
    [[ -n "$raw" ]] || { echo ""; return 0; }
    # jq selects only LIVE (non-terminal) matches. Use `first(.[] | ...)` so jq
    # itself emits at most one id — avoids a `| head` pipe whose early close
    # would SIGPIPE jq and, under pipefail, masquerade as a query failure. A jq
    # parse error (malformed response) surfaces as a non-zero return -> fail loud.
    ids="$(printf '%s' "$raw" \
        | jq -r 'first(.[]? | select((.status // "") as $s | $s != "closed" and $s != "partial") | .id) // empty')" \
        || return 3
    printf '%s' "$ids"
}

run() {
    if [[ "$DRY_RUN" -eq 1 ]]; then
        printf 'DRY-RUN:'; printf ' %q' "$@"; printf '\n'
        return 0
    fi
    "$@"
}

# --- recurrence path: comment on the existing open bug ----------------------
comment_on_dup() {
    local bug="$1"
    local body
    body=$(cat <<EOF
RECURRENCE (auto-filed by Rig R3, $(ts_now)) — same (rung + reason) signature, deduped against this open bug instead of filing a new one.

- failing rung: ${RUNG}
- reason: ${REASON}
$( [[ -n "$MODE" ]]  && echo "- classified mode: ${MODE}" )
$( [[ -n "$CYCLE" ]] && echo "- driver cycle: ${CYCLE}" )
$( [[ -n "$NODE" ]]  && echo "- node: ${NODE}" )
$( [[ -n "$CANARY" ]] && echo "- canary instance: ${CANARY}" )
- fresh forensic bundle (R2): ${BUNDLE_LINK}
- dedup signature: ${SIG}

This failure mode is still live. HARD HOLD respected — no canary spawned, prod/freezeThaw-flag untouched.
EOF
)
    # Tolerate a transient comment-add failure: the load-bearing part of the
    # dedup path is RE-ASSERTING the contract edges (so a prior 'created_unlinked'
    # bug whose edges never landed gets repaired) and emitting a result for R4.
    # A failed comment must NOT abort under set -e before that happens.
    local comment_ok=1
    run "$KD" comment add "$bug" -m "$body" || comment_ok=0
    # Idempotently (re)assert the two contract edges. A pre-existing edge makes
    # `kd dep add` a no-op/duplicate error; we tolerate that so the dedup path
    # also REPAIRS a prior 'created_unlinked' bug whose edges never landed — the
    # WARN recovery contract.
    run "$KD" dep add "$CANARY_TASK" "$bug" --type blocks || true
    run "$KD" dep add "$bug" "$EPIC" --type parent-child || true
    [[ "$comment_ok" -eq 1 ]] || \
        echo "R3_WARN: comment add on ${bug} failed; edges re-asserted anyway" >&2
    echo "R3_RESULT=deduped bug=${bug} sig=${SIG}"
}

# --- file path: create the next-layer bug -----------------------------------
file_bug() {
    local short_reason title desc new_bug
    short_reason="$(printf '%s' "$REASON" | head -c 120 | tr '\n' ' ')"
    title="gVisor freeze->thaw canary rung failure: ${RUNG} — ${short_reason}"

    desc=$(cat <<EOF
AUTO-FILED by Rig R3 (ka-0000004124) on a gVisor freeze->thaw canary rung failure, before HARD HOLD. This is the next-layer bug for the FIRST failing rung of the canary cycle.

FAILING RUNG: ${RUNG}$( [[ -n "$MODE" ]] && echo " (classified mode: ${MODE})" )
REASON: ${REASON}

SYMPTOM: the bounded canary self-loop driver (R4, ka-0000006231) drove a freeze->thaw cycle; rung ${RUNG} reported FT_RUNG_${RUNG}=fail. Per the R1 ladder, the first failing rung is the classified failure mode — subsequent rungs are not evaluated.
$( [[ -n "$CYCLE" ]] && echo "DRIVER CYCLE: ${CYCLE}" )
$( [[ -n "$NODE" ]]  && echo "NODE: ${NODE}" )
$( [[ -n "$CANARY" ]] && echo "CANARY INSTANCE: ${CANARY} (task ${CANARY_TASK})" )

EVIDENCE: R2 forensic bundle (ip addr/route, /proc/net/route, runsc debug + restore stderr, /var/lib/cni/results, Pod B phase+events, controller log tail, full FT_RUNG_* set): ${BUNDLE_LINK}
$( [[ -n "$RUNGS_BLOCK" ]] && printf 'RUNG LADDER (FT_RUNG_* set at failure):\n%s\n' "$RUNGS_BLOCK" )
IMPACT: blocks the freeze->thaw canary task ${CANARY_TASK} — the canary cannot be declared clean while this rung fails — and therefore gates the Phase-4 gVisor rollout go/no-go under epic ${EPIC}.

FIX DIRECTIONS (engineer/gardener to triage): localize the failure to the named rung's stage in freezethaw_node.go / freezethaw_rehydrate.go using the bundle, identify whether it is freeze-side (snapshot/relocation) or thaw-side (restore/netns/cni/rehydrate-ack), and ship the fix via a GATED release. Do NOT touch the freezeThaw flag.

DEDUP: filed under signature (rung + normalized-reason). Recurrences of this exact mode comment on this bug instead of duplicating.
FT_AUTOFILE_SIG: ${SIG}
FT_FAILING_RUNG: ${RUNG}

HARD HOLD respected: no canary spawned by R3, prod/freezeThaw-flag/step-3 UNTOUCHED. Harness-filed.
EOF
)

    if [[ "$DRY_RUN" -eq 1 ]]; then
        run "$KD" create "$title" --type bug --priority "$PRIORITY" --no-parent \
            --project "$R3_PROJECT" \
            -l "project:${R3_PROJECT}" -l "$SIG_LABEL" -l "$RUNG_LABEL" -l gvisor-canary -l needs-intake \
            -d "$desc"
        echo "DRY-RUN: kd dep add ${CANARY_TASK} <new-bug> --type blocks"
        echo "DRY-RUN: kd dep add <new-bug> ${EPIC} --type parent-child"
        echo "R3_RESULT=created bug=<dry-run> sig=${SIG}"
        return 0
    fi

    new_bug="$("$KD" create "$title" --type bug --priority "$PRIORITY" --no-parent \
        --project "$R3_PROJECT" \
        -l "project:${R3_PROJECT}" -l "$SIG_LABEL" -l "$RUNG_LABEL" -l gvisor-canary -l needs-intake \
        -d "$desc" --json | jq -r '.id')"
    [[ -n "$new_bug" && "$new_bug" != "null" ]] || die "kd create did not return a bead id"

    # Wire the two contract edges. Do NOT let a transient dep-add failure abort
    # under `set -e` before we emit a result line — the bug already exists, so
    # R4 must learn its id either way. We attempt both edges, then report
    # 'created' only if BOTH landed, else 'created_unlinked' so R4 (or a human)
    # can complete the wiring instead of re-filing a duplicate (the ft-sig label
    # is already on the bug, so the next dedup pass will find it).
    local blocks_ok=0 parent_ok=0
    # The bug BLOCKS the canary: canary depends-on bug (blocks) so the canary is
    # blocked-until-fixed. (kd dep add <blocked> <blocker> --type blocks.)
    if "$KD" dep add "$CANARY_TASK" "$new_bug" --type blocks; then blocks_ok=1; fi
    # The bug is a CHILD of the Phase-4 epic.
    if "$KD" dep add "$new_bug" "$EPIC" --type parent-child; then parent_ok=1; fi

    if [[ "$blocks_ok" -eq 1 && "$parent_ok" -eq 1 ]]; then
        echo "R3_RESULT=created bug=${new_bug} sig=${SIG}"
    else
        echo "R3_WARN: bug ${new_bug} filed but edges incomplete (blocks=${blocks_ok} parent=${parent_ok}); rerun completes wiring via dedup" >&2
        echo "R3_RESULT=created_unlinked bug=${new_bug} sig=${SIG} blocks=${blocks_ok} parent=${parent_ok}"
    fi
}

# --- main -------------------------------------------------------------------
# Distinguish a dedup query FAILURE (return 3) from a genuine empty result.
# On failure we fail loud rather than silently filing a duplicate: a transient
# daemon/auth blip must not defeat dedup, since the canary self-loop re-drives
# the SAME failure and would otherwise spew one bug per cycle.
if EXISTING="$(find_live_dup)"; then
    if [[ -n "$EXISTING" ]]; then
        comment_on_dup "$EXISTING"
    else
        file_bug
    fi
else
    die "dedup query failed (kd list); refusing to file blind to avoid duplicates — retry when the daemon is reachable"
fi
