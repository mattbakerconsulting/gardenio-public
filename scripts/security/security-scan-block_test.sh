#!/usr/bin/env bash
#
# security-scan-block_test.sh — hermetic offline test for the Security-Scan:
# trailer generator (bundle ka-0000005053 §2.3; task ka-0000007861).
#
# Drives the REAL security-scan-block.sh CLI against a throwaway lock fixture —
# only bash + sed + coreutils, no network, no scanners. Asserts the block is
# well-formed, records lock versions, round-trips through --verify, and that
# --verify catches a version drift.

set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

FAILS=0
ok()   { echo "  ok   $1"; }
fail() { echo "  FAIL $1  ${2:-}"; FAILS=$((FAILS+1)); }

# assert_contains <name> <haystack> <needle>
assert_contains() {
  case "$2" in
    *"$3"*) ok "$1" ;;
    *)      fail "$1" "missing: $3" ;;
  esac
}

# assert_not_contains <name> <haystack> <needle>
assert_not_contains() {
  case "$2" in
    *"$3"*) fail "$1" "unexpected: $3" ;;
    *)      ok "$1" ;;
  esac
}

# assert_rc <name> <expected> <actual>
assert_rc() {
  if [ "$2" = "$3" ]; then ok "$1"; else fail "$1" "want rc $2, got $3"; fi
}

# ── Fixture: a throwaway lock ────────────────────────────────────────────────
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cat > "$WORK/security-tools.lock" <<'EOF'
# fixture lock
GITLEAKS_VERSION=8.21.2
GOVULNCHECK_VERSION=1.1.4
OSV_SCANNER_VERSION=1.9.2
CARGO_AUDIT_VERSION=0.21.2
CARGO_DENY_VERSION=0.16.4
EOF

GEN="$SELF_DIR/security-scan-block.sh"

echo "test_emit_well_formed"
OUT="$("$GEN" --lock "$WORK/security-tools.lock" --sha abc1234 \
  --result gitleaks=clean --result govulncheck=0-reachable \
  --result osv-scanner=clean --result cargo-audit=clean --result cargo-deny=pass)"
assert_contains "starts with Security-Scan:" "$OUT" "Security-Scan: abc1234"
assert_contains "gitleaks pinned version" "$OUT" "gitleaks=8.21.2=clean"
assert_contains "govulncheck reachable result" "$OUT" "govulncheck=1.1.4=0-reachable"
assert_contains "osv-scanner clean" "$OUT" "osv-scanner=1.9.2=clean"
assert_contains "cargo-audit clean" "$OUT" "cargo-audit=0.21.2=clean"
assert_contains "cargo-deny pass" "$OUT" "cargo-deny=0.16.4=pass"
assert_not_contains "no review tuple when omitted" "$OUT" "security-review="

echo "test_emit_with_review"
OUT_REVIEW="$("$GEN" --lock "$WORK/security-tools.lock" --sha abc1234 \
  --result gitleaks=clean --result govulncheck=0-reachable \
  --result osv-scanner=clean --result cargo-audit=clean --result cargo-deny=pass \
  --review high=0-findings)"
assert_contains "review tuple present" "$OUT_REVIEW" "security-review=high=0-findings"

echo "test_missing_result_defaults_unknown"
# A scanner with no --result must record =unknown (fail-loud; the gate FAILs on it).
OUT_PARTIAL="$("$GEN" --lock "$WORK/security-tools.lock" --sha abc1234 --result gitleaks=clean)"
assert_contains "omitted scanner -> unknown" "$OUT_PARTIAL" "cargo-deny=0.16.4=unknown"

echo "test_verify_roundtrip_ok"
printf '%s\n' "$OUT" | "$GEN" --lock "$WORK/security-tools.lock" --verify >/dev/null 2>&1
assert_rc "clean block verifies" 0 "$?"

echo "test_verify_catches_drift"
# Hand-craft a block claiming a stale govulncheck version -> verify must reject.
DRIFT="Security-Scan: abc1234 gitleaks=8.21.2=clean govulncheck=9.9.9=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass"
printf '%s\n' "$DRIFT" | "$GEN" --lock "$WORK/security-tools.lock" --verify >/dev/null 2>&1
assert_rc "drifted block rejected" 6 "$?"

echo "test_missing_sha_usage_error"
"$GEN" --lock "$WORK/security-tools.lock" --result gitleaks=clean >/dev/null 2>&1
assert_rc "missing --sha -> usage" 2 "$?"

echo "test_unreadable_lock"
"$GEN" --lock "$WORK/nope.lock" --sha abc >/dev/null 2>&1
assert_rc "unreadable lock" 3 "$?"

echo
if [ "$FAILS" -ne 0 ]; then
  echo "FAILED ($FAILS)"
  exit 1
fi
echo "all security-scan-block tests passed"
