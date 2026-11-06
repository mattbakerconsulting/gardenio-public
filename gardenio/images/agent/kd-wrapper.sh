#!/bin/bash
# kd wrapper — intercepts the EX_USAGE-family sentinel exit code 64 emitted
# by the kbeads CLI when a write is refused because no human operator could
# be attributed to the request. On match, the wrapper invokes
# `gb operator-refusal report` so the refusal is loudly surfaced via Slack
# (gb squawk) and captured durably as a type=bug bead under the agent's
# spawn lineage, then exits non-zero so the cycle is marked failed.
#
# Contract (see ka-0000006731):
#   - real binary lives at /usr/local/bin/kd-bin
#   - wrapper at /usr/local/bin/kd
#   - exit code 64 from kd-bin → parse JSON stderr payload, call gb,
#     re-emit the stderr payload, exit with code 64 propagated up
#
# Bypass paths:
#   - KD_WRAPPER_BYPASS=1                → skip wrapping entirely
#   - argv[0] basename ends with "-bin"  → direct invocation of real binary
#
# All other behavior is pass-through: the wrapper does not alter argv,
# stdin, stdout, environment, or working directory.

set -u

KD_REAL_BIN="${KD_REAL_BIN:-/usr/local/bin/kd-bin}"
GB_BIN="${GB_BIN:-/usr/local/bin/gb}"

if [ "${KD_WRAPPER_BYPASS:-0}" = "1" ] || [ ! -x "$KD_REAL_BIN" ]; then
    # No real binary or bypass requested — exec whatever is reasonable.
    if [ -x "$KD_REAL_BIN" ]; then
        exec "$KD_REAL_BIN" "$@"
    fi
    echo "kd-wrapper: real kd binary not found at $KD_REAL_BIN" >&2
    exit 127
fi

# Capture stderr so we can both echo it back AND parse the refusal JSON
# payload. We intentionally do NOT capture stdout — most kd invocations
# print results there and callers must see them streamed in real time.
TMP_STDERR=$(mktemp -t kd-wrapper-stderr.XXXXXX)
trap 'rm -f "$TMP_STDERR"' EXIT

# Run the real binary. Tee stderr so the user still sees it as it streams.
"$KD_REAL_BIN" "$@" 2> >(tee "$TMP_STDERR" >&2)
rc=$?

if [ "$rc" -ne 64 ]; then
    exit "$rc"
fi

# Exit code 64 (ExitCodeOperatorRefusal) — the kd CLI emits a single-line
# JSON object on stderr with the shape:
#   {"error":"operator_refusal","resolver_source":"X","message":"...","command":"kd ..."}
#
# Pull the last JSON line out of stderr; fall back to defaults if absent.
RESOLVER_SOURCE="unknown"
COMMAND_SUMMARY="kd"
if [ -s "$TMP_STDERR" ]; then
    JSON_LINE=$(grep -F '"error":"operator_refusal"' "$TMP_STDERR" | tail -n1)
    if [ -n "$JSON_LINE" ]; then
        # Cheap, dependency-free extraction. jq isn't guaranteed on every
        # host; sed handles the two fields we need without shelling to a
        # parser. Keys are stable per the kd CLI contract.
        rs=$(printf '%s' "$JSON_LINE" | sed -n 's/.*"resolver_source":"\([^"]*\)".*/\1/p')
        cs=$(printf '%s' "$JSON_LINE" | sed -n 's/.*"command":"\([^"]*\)".*/\1/p')
        if [ -n "$rs" ]; then RESOLVER_SOURCE="$rs"; fi
        if [ -n "$cs" ]; then COMMAND_SUMMARY="$cs"; fi
    fi
fi

# Best-effort call to gb operator-refusal report. We never block the exit
# on this — if gb is missing or daemon-unreachable, the wrapper still
# propagates exit code 64 so the calling cycle fails loudly.
if [ -x "$GB_BIN" ]; then
    "$GB_BIN" operator-refusal report \
        --source "$RESOLVER_SOURCE" \
        --command "$COMMAND_SUMMARY" \
        --message "intercepted by kd-wrapper after exit 64" \
        >&2 || true
else
    echo "kd-wrapper: gb binary not found at $GB_BIN; cannot file refusal report (source=$RESOLVER_SOURCE)" >&2
fi

exit 64
