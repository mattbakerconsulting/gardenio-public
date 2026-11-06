#!/usr/bin/env bash
# file-next-layer-bug_test.sh — Unit tests for file-next-layer-bug.sh (Rig R3,
# ka-0000004124).
#
# Hermetic: NO live kbeads daemon, NO canary, NO prod. The kd/gb binaries the
# script calls are resolved via $KD/$GB and shadowed here by PATH stubs that
# (a) record every invocation to $CALL_LOG for assertions and (b) serve a canned
# `kd list ... --json` reply from $KD_LIST_FIXTURE so the dedup branch is
# exercised both ways.
#
# Usage:  bash scripts/gvisor/file-next-layer-bug_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/file-next-layer-bug.sh"

PASS=0
FAIL=0
fail_test() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }
pass_test() { echo "PASS: $1"; PASS=$((PASS + 1)); }

# Build a throwaway bin dir holding `kd`/`gb` shims (jq is real). kd records args
# to $CALL_LOG. `kd list` is FAITHFUL: it serves $KD_LIST_FIXTURE but actually
# applies the --label and --project filters the SUT passes, so the dedup tests
# exercise real lookup behaviour rather than passing vacuously. Setting
# $KD_LIST_FAIL=1 makes `kd list` exit non-zero (transport-failure simulation).
# `kd create --json` returns a stable fake id.
make_shims() {
    local bin="$1"
    mkdir -p "$bin"

    cat > "${bin}/kd" <<'SHIM'
#!/usr/bin/env bash
echo "kd $*" >> "$CALL_LOG"
# Verb log: one line per invocation, binary + subcommand ONLY (never the argv
# body), so the harness-only guard can assert which binaries/verbs ran without
# tripping over prose inside the filed bug description.
echo "kd ${1:-} ${2:-}" >> "$VERB_LOG"
if [[ "${1:-}" == "list" ]]; then
    [[ -n "${KD_LIST_FAIL:-}" ]] && { echo "kd: list failed" >&2; exit 1; }
    # Parse the --label and --project filters out of argv and apply them to the
    # fixture, mirroring how the real daemon scopes the query.
    want_label="" want_project="" want_type=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --label)   want_label="${2:-}"; shift 2 ;;
            --project) want_project="${2:-}"; shift 2 ;;
            --type)    want_type="${2:-}"; shift 2 ;;
            *)         shift ;;
        esac
    done
    # Record the project actually queried so a test can assert the SUT pinned it
    # (rather than picking up an ambient BOAT_PROJECT/KD_PROJECT override).
    echo "$want_project" >> "${QUERIED_PROJECT_LOG:-/dev/null}"
    # Model the real daemon's --type filter too (only return matching-type beads).
    jq -c --arg lbl "$want_label" --arg proj "$want_project" --arg typ "$want_type" '
        [ .[]
          | select($typ == "" or (.type // "bug") == $typ)
          | select($lbl == "" or (.labels // [] | index($lbl)))
          | select($proj == "" or (.labels // [] | index("project:" + $proj)))
        ]' "${KD_LIST_FIXTURE:-/dev/null}" 2>/dev/null || echo "[]"
    exit 0
fi
if [[ "${1:-}" == "create" ]]; then
    # Honor --json by returning a parseable object with a fixed id.
    for a in "$@"; do [[ "$a" == "--json" ]] && { echo '{"id":"gd-ISJPD6HUA9"}'; exit 0; }; done
    echo "gd-ISJPD6HUA9"
    exit 0
fi
exit 0
SHIM
    chmod +x "${bin}/kd"

    cat > "${bin}/gb" <<'SHIM'
#!/usr/bin/env bash
echo "gb $*" >> "$CALL_LOG"
echo "gb ${1:-}" >> "$VERB_LOG"
exit 0
SHIM
    chmod +x "${bin}/gb"

    # kubectl / helm / runsc must NEVER be invoked by a harness-only script. Drop
    # stubs that record to the verb log AND fail, so any accidental call is loud.
    for forbidden in kubectl helm runsc; do
        cat > "${bin}/${forbidden}" <<SHIM
#!/usr/bin/env bash
echo "${forbidden} \$*" >> "\$VERB_LOG"
echo "${forbidden}: harness-only script must not call this" >&2
exit 97
SHIM
        chmod +x "${bin}/${forbidden}"
    done
}

setup() {
    TMP="$(mktemp -d)"
    BIN="${TMP}/bin"
    make_shims "$BIN"
    CALL_LOG="${TMP}/calls.log"
    : > "$CALL_LOG"
    export CALL_LOG
    VERB_LOG="${TMP}/verbs.log"
    : > "$VERB_LOG"
    export VERB_LOG
    QUERIED_PROJECT_LOG="${TMP}/queried-project.log"
    : > "$QUERIED_PROJECT_LOG"
    export QUERIED_PROJECT_LOG
    export KD_LIST_FIXTURE="${TMP}/list.json"
    echo "[]" > "$KD_LIST_FIXTURE"   # default: no existing dup
    export PATH="${BIN}:${PATH}"
    export KD="kd" GB="gb"
}
teardown() { rm -rf "$TMP"; }

# Extract the sig=... token from an R3_RESULT line in captured stdout.
sig_of() { grep -oE 'sig=[0-9a-f]+' <<<"$1" | head -n1 | cut -d= -f2; }

# ---------------------------------------------------------------------------
# T1: create path — no dup -> files a bug and emits BOTH dep edges with the
# correct direction (canary blocked-by bug; bug child-of epic).
test_create_files_bug_and_links() {
    setup
    local out
    out="$(KD=kd GB=gb bash "$SUT" \
        --rung restore_rc0 \
        --reason "thaw fail-closed rc=128 at CWD-dentry assertion" \
        --bundle gd-V04APHT5ZX 2>/dev/null)"

    grep -q '^R3_RESULT=created bug=gd-ISJPD6HUA9 ' <<<"$out" \
        || fail_test T1 "expected created result, got: $out"

    grep -q '^kd create ' "$CALL_LOG"                                    || fail_test T1 "no kd create"
    grep -q 'kd create .*--type bug'                "$CALL_LOG" >/dev/null || fail_test T1 "create not type=bug"
    # canary blocked-by bug:  kd dep add <canary-task> <bug> --type blocks
    grep -q 'kd dep add ka-0000001355 gd-ISJPD6HUA9 --type blocks' "$CALL_LOG" \
        || fail_test T1 "missing/incorrect canary-blocks edge"
    # bug child-of epic:      kd dep add <bug> <epic> --type parent-child
    grep -q 'kd dep add gd-ISJPD6HUA9 ka-0000000867 --type parent-child' "$CALL_LOG" \
        || fail_test T1 "missing/incorrect epic parent-child edge"
    grep -q 'project:gardenio' "$CALL_LOG"  || fail_test T1 "missing project label"
    grep -q 'ft-sig:'         "$CALL_LOG"  || fail_test T1 "missing signature label"
    grep -q 'ft-rung:restore_rc0' "$CALL_LOG" || fail_test T1 "missing rung label"

    [[ $FAIL -eq 0 ]] && pass_test T1
    teardown
}

# ---------------------------------------------------------------------------
# T2: dedup path — an OPEN bug with the same sig exists -> comment, do NOT create.
test_dedup_comments_not_creates() {
    setup
    local f0 out
    # First run with no dup: discover the canonical signature for this failure.
    f0="$(KD=kd GB=gb bash "$SUT" --rung netns_routable \
            --reason "default route missing in restored netns" \
            --bundle gd-QY1ZT6DWAD 2>/dev/null)"
    local sig; sig="$(sig_of "$f0")"
    [[ -n "$sig" ]] || fail_test T2 "could not derive signature"

    # Now seed the dup fixture as an OPEN bug carrying that exact sig label.
    cat > "$KD_LIST_FIXTURE" <<EOF
[{"id":"gd-NR68QNEWRH","status":"open","labels":["project:gardenio","ft-sig:${sig}","ft-rung:netns_routable"]}]
EOF
    : > "$CALL_LOG"
    # Same classified failure, only volatile VALUE tokens differ (cycle index,
    # node) — supplied via the structured flags, not smuggled into --reason — so
    # the signature is identical and the open bug is found.
    out="$(KD=kd GB=gb bash "$SUT" --rung netns_routable \
            --reason "default route missing in restored netns" \
            --cycle 3 --node ip-10-1-2-3 \
            --bundle gd-XTJZIDFJBI 2>/dev/null)"

    grep -q "^R3_RESULT=deduped bug=gd-NR68QNEWRH " <<<"$out" \
        || fail_test T2 "expected deduped result, got: $out"
    grep -q 'kd comment add gd-NR68QNEWRH' "$CALL_LOG" || fail_test T2 "no comment on existing bug"
    grep -q '^kd create '                  "$CALL_LOG" && fail_test T2 "must NOT create when dup exists"

    [[ $FAIL -eq 0 ]] && pass_test T2
    teardown
}

# ---------------------------------------------------------------------------
# T3: signature stability — same rung + same failure mode but volatile tokens
# (timestamps, node, ip, sizes, counts, hex) differ -> SAME signature.
test_signature_stable_across_volatiles() {
    setup
    local a b sa sb
    a="$(KD=kd GB=gb bash "$SUT" --rung work_intact \
          --reason "agent/lower MISSING /home/agent/workspace; lower=701MiB at 2026-06-23T05:40:35Z node ip-10-30-136-242 0xdeadbeef x193" \
          --bundle gd-F1 2>/dev/null)"
    b="$(KD=kd GB=gb bash "$SUT" --rung work_intact \
          --reason "agent/lower MISSING /home/agent/workspace; lower=512MiB at 2026-06-24T11:22:33Z node ip-10-99-1-7 0xcafef00d x42" \
          --bundle gd-F2 2>/dev/null)"
    sa="$(sig_of "$a")"; sb="$(sig_of "$b")"
    [[ -n "$sa" && "$sa" == "$sb" ]] \
        || fail_test T3 "signature drifted across volatiles: $sa vs $sb"
    [[ $FAIL -eq 0 ]] && pass_test T3
    teardown
}

# ---------------------------------------------------------------------------
# T4: signature DISCRIMINATION — different rung, or genuinely different mode,
# yields a DIFFERENT signature (no false-merge of distinct failures).
test_signature_discriminates() {
    setup
    local a b c sa sb sc
    a="$(KD=kd GB=gb bash "$SUT" --rung restore_rc0    --reason "rc=128 restore fail" --bundle gd-F 2>/dev/null)"
    b="$(KD=kd GB=gb bash "$SUT" --rung netns_routable --reason "rc=128 restore fail" --bundle gd-F 2>/dev/null)"
    c="$(KD=kd GB=gb bash "$SUT" --rung restore_rc0    --reason "cni veth attach failed" --bundle gd-F 2>/dev/null)"
    sa="$(sig_of "$a")"; sb="$(sig_of "$b")"; sc="$(sig_of "$c")"
    [[ "$sa" != "$sb" ]] || fail_test T4 "same sig for different rungs ($sa)"
    [[ "$sa" != "$sc" ]] || fail_test T4 "same sig for different reasons ($sa)"
    [[ $FAIL -eq 0 ]] && pass_test T4
    teardown
}

# ---------------------------------------------------------------------------
# T5: bundle link — a bare bead id renders to the viewer URL WITH the /beads/
# prefix (advice ka-0000003730); a full URL is passed through verbatim.
test_bundle_link_prefix() {
    setup
    KD=kd GB=gb bash "$SUT" --rung ram_intact --reason "ram drift" --bundle gd-BUND123 >/dev/null 2>&1
    grep -q '/beads/bundle/gd-BUND123' "$CALL_LOG" \
        || fail_test T5 "bare id did not render /beads/bundle/ URL"
    : > "$CALL_LOG"
    KD=kd GB=gb bash "$SUT" --rung ram_intact --reason "ram drift" \
        --bundle "https://example.test/beads/bundle/gd-X" >/dev/null 2>&1
    grep -q 'https://example.test/beads/bundle/gd-X' "$CALL_LOG" \
        || fail_test T5 "full URL not passed through"
    [[ $FAIL -eq 0 ]] && pass_test T5
    teardown
}

# ---------------------------------------------------------------------------
# T6: a CLOSED prior bug with the same sig must NOT dedup — recurrence after a
# (failed) fix files a fresh bug.
test_closed_dup_files_fresh() {
    setup
    local sig f0
    f0="$(KD=kd GB=gb bash "$SUT" --rung cni_confirmed --reason "cni result absent" --bundle gd-F 2>/dev/null)"
    sig="$(sig_of "$f0")"
    cat > "$KD_LIST_FIXTURE" <<EOF
[{"id":"gd-CLOSED01","status":"closed","labels":["project:gardenio","ft-sig:${sig}"]}]
EOF
    : > "$CALL_LOG"
    local out
    out="$(KD=kd GB=gb bash "$SUT" --rung cni_confirmed --reason "cni result absent" --bundle gd-F 2>/dev/null)"
    grep -q '^R3_RESULT=created ' <<<"$out" || fail_test T6 "closed dup should file fresh, got: $out"
    grep -q '^kd create '         "$CALL_LOG" || fail_test T6 "no create on closed-dup recurrence"
    [[ $FAIL -eq 0 ]] && pass_test T6
    teardown
}

# ---------------------------------------------------------------------------
# T7: required-arg validation — missing --bundle exits non-zero.
test_requires_bundle() {
    setup
    if KD=kd GB=gb bash "$SUT" --rung restore_rc0 --reason "x" >/dev/null 2>&1; then
        fail_test T7 "expected nonzero exit on missing --bundle"
    else
        pass_test T7
    fi
    teardown
}

# ---------------------------------------------------------------------------
# T8: --mode overrides reason as the signature basis — two different reasons
# under the same classified mode dedup together.
test_mode_drives_signature() {
    setup
    local a b sa sb
    a="$(KD=kd GB=gb bash "$SUT" --rung restore_rc0 --mode workspace-overlay-loss \
          --reason "lower missing /home" --bundle gd-F 2>/dev/null)"
    b="$(KD=kd GB=gb bash "$SUT" --rung restore_rc0 --mode workspace-overlay-loss \
          --reason "completely different wording here" --bundle gd-F 2>/dev/null)"
    sa="$(sig_of "$a")"; sb="$(sig_of "$b")"
    [[ "$sa" == "$sb" ]] || fail_test T8 "mode-based sig should match across reasons: $sa vs $sb"
    [[ $FAIL -eq 0 ]] && pass_test T8
    teardown
}

# ---------------------------------------------------------------------------
# T9: a PARTIAL prior bug (terminal-for-progression in kbeads, like closed) must
# NOT dedup — it no longer blocks the canary, so a recurrence must file fresh.
test_partial_dup_files_fresh() {
    setup
    local sig f0 out
    f0="$(KD=kd GB=gb bash "$SUT" --rung snapshot_persisted --reason "snapshot count != 1" --bundle gd-F 2>/dev/null)"
    sig="$(sig_of "$f0")"
    cat > "$KD_LIST_FIXTURE" <<EOF
[{"id":"gd-PARTIAL01","status":"partial","labels":["project:gardenio","ft-sig:${sig}"]}]
EOF
    : > "$CALL_LOG"
    out="$(KD=kd GB=gb bash "$SUT" --rung snapshot_persisted --reason "snapshot count != 1" --bundle gd-F 2>/dev/null)"
    grep -q '^R3_RESULT=created ' <<<"$out"  || fail_test T9 "partial dup should file fresh, got: $out"
    grep -q '^kd create '         "$CALL_LOG" || fail_test T9 "no create on partial-dup recurrence"
    grep -q 'kd comment add'      "$CALL_LOG" && fail_test T9 "must NOT comment on a terminal partial bug"
    [[ $FAIL -eq 0 ]] && pass_test T9
    teardown
}

# ---------------------------------------------------------------------------
# T10: a dedup-query FAILURE must fail LOUD (nonzero, no create) — never file a
# blind duplicate when it cannot tell "no dup" from "query failed".
test_query_failure_fails_loud() {
    setup
    export KD_LIST_FAIL=1
    local rc=0
    KD=kd GB=gb bash "$SUT" --rung restore_rc0 --reason "x" --bundle gd-F >/dev/null 2>&1 || rc=$?
    unset KD_LIST_FAIL
    [[ "$rc" -ne 0 ]]                     || fail_test T10 "expected nonzero exit on query failure"
    grep -q '^kd create ' "$CALL_LOG"     && fail_test T10 "must NOT file a bug when dedup query failed"
    [[ $FAIL -eq 0 ]] && pass_test T10
    teardown
}

# ---------------------------------------------------------------------------
# T11: the filed bug BODY must carry the failing rung AND the reason (the core
# contract), not just labels. Asserts against the `kd create -d <desc>` argv.
test_bug_body_carries_rung_and_reason() {
    setup
    KD=kd GB=gb bash "$SUT" --rung work_intact \
        --reason "UNIQUEREASONTOKEN canary marker absent from restored workspace" \
        --bundle gd-BUNDXYZ >/dev/null 2>&1
    grep -q 'FAILING RUNG: work_intact'        "$CALL_LOG" || fail_test T11 "body missing failing rung"
    grep -q 'UNIQUEREASONTOKEN'                "$CALL_LOG" || fail_test T11 "body missing reason text"
    grep -q '/beads/bundle/gd-BUNDXYZ'         "$CALL_LOG" || fail_test T11 "body missing bundle link"
    grep -q 'kd dep add ka-0000001355 gd-ISJPD6HUA9 --type blocks'        "$CALL_LOG" || fail_test T11 "body missing block edge"
    grep -q 'kd dep add gd-ISJPD6HUA9 ka-0000000867 --type parent-child'  "$CALL_LOG" || fail_test T11 "body missing parent edge"
    [[ $FAIL -eq 0 ]] && pass_test T11
    teardown
}

# ---------------------------------------------------------------------------
# T12: HARNESS-ONLY guard — the script must never shell out to gb/kubectl/helm
# nor read/flip a freezeThaw flag. Asserts the recorded call log over a full run.
test_harness_only_no_side_effects() {
    setup
    KD=kd GB=gb bash "$SUT" --rung restore_rc0 --reason "rc=128" --bundle gd-F >/dev/null 2>&1
    # Assert against the VERB log (binary + subcommand only), never the argv body
    # — the filed bug description legitimately contains prose like "freezeThaw"
    # and "freezethaw_node.go" which must not be mistaken for a side effect.
    grep -qE '^(gb|kubectl|helm|runsc)\b' "$VERB_LOG" \
        && fail_test T12 "harness-only violated: gb/kubectl/helm/runsc invoked"
    # Only kd verbs that mutate beads or read for dedup are allowed.
    if grep -vqE '^kd (list|create|dep|comment) ?' "$VERB_LOG"; then
        echo "  unexpected verbs:"; grep -vE '^kd (list|create|dep|comment) ?' "$VERB_LOG" | sed 's/^/    /'
        fail_test T12 "unexpected non-allowlisted verb invoked"
    fi
    [[ $FAIL -eq 0 ]] && pass_test T12
    teardown
}

# ---------------------------------------------------------------------------
# T13: a partial-edge create (one dep-add fails) still emits a machine-readable
# result for R4 (created_unlinked) instead of aborting silently under set -e.
test_partial_link_still_signals() {
    setup
    # Make the SECOND dep-add (parent-child) fail by shadowing kd with a wrapper
    # that fails only that specific call.
    cat > "${BIN}/kd" <<'SHIM'
#!/usr/bin/env bash
echo "kd $*" >> "$CALL_LOG"
if [[ "${1:-}" == "list" ]]; then echo "[]"; exit 0; fi
if [[ "${1:-}" == "create" ]]; then echo '{"id":"gd-ISJPD6HUA9"}'; exit 0; fi
if [[ "${1:-}" == "dep" && "${4:-}" == "ka-0000000867" ]]; then echo "kd: parent edge boom" >&2; exit 1; fi
exit 0
SHIM
    chmod +x "${BIN}/kd"
    local out rc=0
    out="$(KD=kd GB=gb bash "$SUT" --rung restore_rc0 --reason "rc=128" --bundle gd-F 2>/dev/null)" || rc=$?
    [[ "$rc" -eq 0 ]] || fail_test T13 "must not abort on a partial-edge failure (rc=$rc)"
    grep -q '^R3_RESULT=created_unlinked bug=gd-ISJPD6HUA9 .*parent=0' <<<"$out" \
        || fail_test T13 "expected created_unlinked signal, got: $out"
    [[ $FAIL -eq 0 ]] && pass_test T13
    teardown
}

# ---------------------------------------------------------------------------
# T14: dedup must work even when the calling agent's ambient project differs.
# The SUT pins --project gardenio, so (a) the dedup query is issued with
# project=gardenio NOT the ambient override, and (b) a prior bug labelled
# project:gardenio is still found. We assert BOTH — the query-project log proves
# the pin is load-bearing (not an inert env var).
test_dedup_ignores_ambient_project() {
    setup
    local sig f0 out
    f0="$(KD=kd GB=gb bash "$SUT" --rung cni_confirmed --reason "veth attach failed" --bundle gd-F 2>/dev/null)"
    sig="$(sig_of "$f0")"
    cat > "$KD_LIST_FIXTURE" <<EOF
[{"id":"gd-AMB01","status":"open","type":"bug","labels":["project:gardenio","ft-sig:${sig}"]}]
EOF
    : > "$CALL_LOG"; : > "$QUERIED_PROJECT_LOG"
    out="$(BOAT_PROJECT=some-other-project KD_PROJECT=some-other-project \
        KD=kd GB=gb bash "$SUT" --rung cni_confirmed --reason "veth attach failed" --bundle gd-F 2>/dev/null)"
    grep -q '^R3_RESULT=deduped bug=gd-AMB01 ' <<<"$out" \
        || fail_test T14 "dedup must survive a non-gardenio ambient project, got: $out"
    grep -q '^kd create ' "$CALL_LOG" && fail_test T14 "must NOT file fresh when a gardenio dup exists"
    # The pin is load-bearing: the query MUST have used gardenio, never the override.
    grep -qx 'gardenio' "$QUERIED_PROJECT_LOG" \
        || fail_test T14 "dedup query did not pin project=gardenio (log: $(tr '\n' ',' < "$QUERIED_PROJECT_LOG"))"
    grep -q 'some-other-project' "$QUERIED_PROJECT_LOG" \
        && fail_test T14 "dedup query leaked the ambient project override"
    [[ $FAIL -eq 0 ]] && pass_test T14
    teardown
}

# ---------------------------------------------------------------------------
# T15: dedup path must survive a transient `kd comment add` failure — it still
# re-asserts both contract edges (WARN-recovery) and emits R3_RESULT=deduped,
# rather than aborting under set -e and leaving the canary unblocked.
test_dedup_survives_comment_failure() {
    setup
    local sig f0 out rc=0
    f0="$(KD=kd GB=gb bash "$SUT" --rung restore_rc0 --reason "rc=128 restore" --bundle gd-F 2>/dev/null)"
    sig="$(sig_of "$f0")"
    # kd shim: dedup match present, but `kd comment add` fails.
    cat > "${BIN}/kd" <<SHIM
#!/usr/bin/env bash
echo "kd \$*" >> "\$CALL_LOG"
if [[ "\${1:-}" == "list" ]]; then echo '[{"id":"gd-LIVE7","status":"open","type":"bug","labels":["project:gardenio","ft-sig:${sig}"]}]'; exit 0; fi
if [[ "\${1:-}" == "comment" ]]; then echo "kd: comment boom" >&2; exit 1; fi
exit 0
SHIM
    chmod +x "${BIN}/kd"
    : > "$CALL_LOG"
    out="$(KD=kd GB=gb bash "$SUT" --rung restore_rc0 --reason "rc=128 restore" --bundle gd-F 2>/dev/null)" || rc=$?
    [[ "$rc" -eq 0 ]] || fail_test T15 "must not abort on comment-add failure (rc=$rc)"
    grep -q '^R3_RESULT=deduped bug=gd-LIVE7 ' <<<"$out" || fail_test T15 "expected deduped result, got: $out"
    grep -q 'kd dep add ka-0000001355 gd-LIVE7 --type blocks'       "$CALL_LOG" || fail_test T15 "blocks edge not re-asserted after comment failure"
    grep -q 'kd dep add gd-LIVE7 ka-0000000867 --type parent-child' "$CALL_LOG" || fail_test T15 "parent edge not re-asserted after comment failure"
    [[ $FAIL -eq 0 ]] && pass_test T15
    teardown
}

# --- shellcheck (if available) ----------------------------------------------
test_shellcheck_clean() {
    if command -v shellcheck >/dev/null 2>&1; then
        if shellcheck -S warning "$SUT" "${SCRIPT_DIR}/file-next-layer-bug_test.sh" >/dev/null 2>&1; then
            pass_test "shellcheck"
        else
            fail_test "shellcheck" "shellcheck reported issues"
            shellcheck -S warning "$SUT" "${SCRIPT_DIR}/file-next-layer-bug_test.sh" || true
        fi
    else
        echo "SKIP: shellcheck not installed"
    fi
}

echo "== file-next-layer-bug.sh tests =="
test_create_files_bug_and_links
test_dedup_comments_not_creates
test_signature_stable_across_volatiles
test_signature_discriminates
test_bundle_link_prefix
test_closed_dup_files_fresh
test_requires_bundle
test_mode_drives_signature
test_partial_dup_files_fresh
test_query_failure_fails_loud
test_bug_body_carries_rung_and_reason
test_harness_only_no_side_effects
test_partial_link_still_signals
test_dedup_ignores_ambient_project
test_dedup_survives_comment_failure
test_shellcheck_clean

echo
echo "PASS=${PASS} FAIL=${FAIL}"
[[ $FAIL -eq 0 ]]
