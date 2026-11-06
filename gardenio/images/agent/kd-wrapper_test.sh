#!/bin/bash
# Unit tests for kd-wrapper.sh.
#
# Verifies (see ka-0000006731):
#   1. Exit code 0 from kd-bin → wrapper exits 0
#   2. Exit code 1 from kd-bin → wrapper exits 1 (no gb call)
#   3. Exit code 64 from kd-bin (with JSON stderr payload) → wrapper:
#        - parses resolver_source + command
#        - invokes gb operator-refusal report with those values
#        - exits 64
#   4. KD_WRAPPER_BYPASS=1 → wrapper exec's kd-bin directly, no interception
#   5. Missing kd-bin → wrapper exits 127 with clear stderr

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WRAPPER="$SCRIPT_DIR/kd-wrapper.sh"

if [ ! -f "$WRAPPER" ]; then
    echo "FAIL: wrapper not found at $WRAPPER" >&2
    exit 1
fi

FAILURES=0
TESTS=0

setup_sandbox() {
    SANDBOX=$(mktemp -d -t gd-wrapper-test.XXXXXX)
    GB_CALL_LOG="$SANDBOX/gb-calls.log"

    cat > "$SANDBOX/kd-bin" <<'EOF'
#!/bin/bash
case "${1:-}" in
    --simulate-ok)        exit 0 ;;
    --simulate-generic)   echo "kd: some error" >&2; exit 1 ;;
    --simulate-refusal)
        echo "kd: operator could not be resolved" >&2
        echo '{"error":"operator_refusal","resolver_source":"fallback_agent_only","message":"operator could not be resolved","command":"kd comment add"}' >&2
        exit 64 ;;
    --simulate-refusal-noargs)
        echo '{"error":"operator_refusal","resolver_source":"header_missing","message":"operator could not be resolved","command":"kd"}' >&2
        exit 64 ;;
    --print-args)         echo "ARGS:$*" ;;
    *) echo "unrecognized: $*" >&2; exit 2 ;;
esac
EOF
    chmod +x "$SANDBOX/kd-bin"

    cat > "$SANDBOX/gb" <<EOF
#!/bin/bash
echo "gb-call:\$*" >> "$GB_CALL_LOG"
exit 1
EOF
    chmod +x "$SANDBOX/gb"

    export KD_REAL_BIN="$SANDBOX/kd-bin"
    export GB_BIN="$SANDBOX/gb"
    export KD_WRAPPER_BYPASS=0
}

teardown_sandbox() {
    rm -rf "$SANDBOX"
    unset KD_REAL_BIN GB_BIN KD_WRAPPER_BYPASS
}

assert() {
    local desc="$1"
    local actual="$2"
    local expected="$3"
    TESTS=$((TESTS+1))
    if [ "$actual" = "$expected" ]; then
        echo "  PASS: $desc"
    else
        echo "  FAIL: $desc"
        echo "    expected: $expected"
        echo "    actual:   $actual"
        FAILURES=$((FAILURES+1))
    fi
}

assert_contains() {
    local desc="$1"
    local haystack="$2"
    local needle="$3"
    TESTS=$((TESTS+1))
    if grep -qF -e "$needle" <<<"$haystack"; then
        echo "  PASS: $desc"
    else
        echo "  FAIL: $desc"
        echo "    haystack: $haystack"
        echo "    needle:   $needle"
        FAILURES=$((FAILURES+1))
    fi
}

count_lines() {
    if [ -f "$1" ]; then
        wc -l < "$1" | tr -d ' '
    else
        echo "0"
    fi
}

# ─── Test 1: ok exit ────────────────────────────────────────────────
echo "Test 1: passthrough on exit 0"
setup_sandbox
bash "$WRAPPER" --simulate-ok
assert "exit code 0 propagated" "$?" "0"
assert "no gb call" "$(count_lines "$GB_CALL_LOG")" "0"
teardown_sandbox

# ─── Test 2: generic error ─────────────────────────────────────────
echo "Test 2: passthrough on exit 1"
setup_sandbox
bash "$WRAPPER" --simulate-generic 2>/dev/null
assert "exit code 1 propagated" "$?" "1"
assert "no gb call" "$(count_lines "$GB_CALL_LOG")" "0"
teardown_sandbox

# ─── Test 3: refusal interception ──────────────────────────────────
echo "Test 3: exit 64 → gb operator-refusal report"
setup_sandbox
STDERR_OUT=$(bash "$WRAPPER" --simulate-refusal 2>&1 >/dev/null)
rc=$?
assert "exit code 64 propagated" "$rc" "64"
GB_CALLS=$(cat "$GB_CALL_LOG" 2>/dev/null || true)
assert_contains "gb invoked with 'operator-refusal report'" "$GB_CALLS" "operator-refusal report"
assert_contains "gb invoked with --source=fallback_agent_only" "$GB_CALLS" "--source fallback_agent_only"
assert_contains "gb invoked with --command=kd comment add" "$GB_CALLS" "--command kd comment add"
assert_contains "stderr still surfaces JSON payload" "$STDERR_OUT" '"error":"operator_refusal"'
teardown_sandbox

# ─── Test 4: refusal interception (header_missing variant) ────────
echo "Test 4: exit 64 with header_missing source"
setup_sandbox
bash "$WRAPPER" --simulate-refusal-noargs 2>/dev/null
assert "exit code 64 propagated" "$?" "64"
GB_CALLS=$(cat "$GB_CALL_LOG" 2>/dev/null || true)
assert_contains "gb invoked with --source=header_missing" "$GB_CALLS" "--source header_missing"
teardown_sandbox

# ─── Test 5: KD_WRAPPER_BYPASS=1 ───────────────────────────────────
echo "Test 5: KD_WRAPPER_BYPASS=1 skips interception"
setup_sandbox
export KD_WRAPPER_BYPASS=1
bash "$WRAPPER" --simulate-refusal 2>/dev/null
assert "exit code 64 still propagated" "$?" "64"
assert "no gb call when bypassed" "$(count_lines "$GB_CALL_LOG")" "0"
teardown_sandbox

# ─── Test 6: missing kd-bin ────────────────────────────────────────
echo "Test 6: missing kd-bin → exit 127"
setup_sandbox
export KD_REAL_BIN="$SANDBOX/does-not-exist"
STDERR_OUT=$(bash "$WRAPPER" --simulate-ok 2>&1 >/dev/null)
assert "exit code 127" "$?" "127"
assert_contains "stderr explains missing binary" "$STDERR_OUT" "real kd binary not found"
teardown_sandbox

# ─── Test 7: argv passthrough ──────────────────────────────────────
echo "Test 7: argv preserved on success path"
setup_sandbox
OUT=$(bash "$WRAPPER" --print-args foo bar baz)
assert "argv preserved" "$OUT" "ARGS:--print-args foo bar baz"
teardown_sandbox

# ─── Summary ────────────────────────────────────────────────────────
echo ""
echo "Tests run: $TESTS, failures: $FAILURES"
if [ "$FAILURES" -gt 0 ]; then
    exit 1
fi
exit 0
