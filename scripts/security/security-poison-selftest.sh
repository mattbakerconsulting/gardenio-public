#!/usr/bin/env bash
#
# security-poison-selftest.sh — the recurring "are the scanners actually wired?"
# self-test (bundle ka-0000005053 §2.3; task ka-0000007861).
#
# The gardener-poison-module pattern applied to the security bar: plant a KNOWN
# bad input, assert the gate goes RED. A scanner that silently stopped firing
# (wrong config, disabled rule, un-invoked binary, a reachability regression)
# is the worst failure mode — it green-washes every MR. This self-test converts
# that into a LOUD build failure.
#
# Legs:
#   G1  gitleaks flags the planted fake AWS credential (proves CI / Gitleaks
#       actually detects secrets — not just that the binary exists).
#   G2  the govulncheck reachability-diff flips RED on a planted NEW reachable
#       critical vuln vs an empty baseline (proves CI / Go-Vuln gates).
#   G3  an imported-but-NOT-called critical vuln does NOT gate (proves the
#       reachability thesis holds — the whole point of the Go-Vuln gate is that
#       it fires on reachable, not merely present, vulns).
#
# G2/G3 are hermetic (pure python + committed JSON fixtures; no Go toolchain,
# no network). G1 needs the `gitleaks` binary; if it is absent the leg SKIPS
# cleanly (exit 0 for that leg) so local runs without gitleaks stay green — CI
# installs gitleaks (gitleaks-bin task) so the leg is exercised there. Set
# POISON_REQUIRE_GITLEAKS=1 to turn an absent gitleaks into a hard failure
# (used by the CI task, which guarantees the binary is present).

set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POISON_DIR="$SELF_DIR/poison"
DIFF_TOOL="$SELF_DIR/govulncheck-diff.py"
OSV_DIFF_TOOL="$SELF_DIR/osv-diff.py"

FAILS=0
pass() { echo "  PASS $1"; }
fail() { echo "  FAIL $1  ${2:-}"; FAILS=$((FAILS+1)); }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ── G1: gitleaks flags the planted fake credential ───────────────────────────
echo "G1: gitleaks detects the planted fake AWS credential"
if ! command -v gitleaks >/dev/null 2>&1; then
  if [ "${POISON_REQUIRE_GITLEAKS:-0}" = "1" ]; then
    fail "G1" "gitleaks not on PATH but POISON_REQUIRE_GITLEAKS=1"
  else
    echo "  SKIP G1 — gitleaks not installed (set POISON_REQUIRE_GITLEAKS=1 to require)"
  fi
else
  # Copy the fixture into a throwaway dir so the scan is isolated from the repo's
  # .gitleaks.toml poison allowlist — we WANT the planted key to fire here.
  cp "$POISON_DIR/fake-aws-key.txt" "$WORK/planted.txt"
  gitleaks detect --no-banner --no-git --source "$WORK" \
    --report-format json --report-path "$WORK/gitleaks.json" --exit-code=1 >/dev/null 2>&1
  gl_rc=$?
  n=$(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1]))))' "$WORK/gitleaks.json" 2>/dev/null || echo 0)
  if [ "$gl_rc" = "1" ] && [ "$n" -ge 1 ]; then
    pass "G1 (gitleaks flagged $n finding(s), exit $gl_rc)"
  else
    fail "G1" "gitleaks did NOT flag the planted credential (rc=$gl_rc, findings=$n) — the secret scanner is not firing!"
  fi
fi

# ── G2: reachability-diff flips RED on a planted NEW reachable critical ───────
echo "G2: Go-Vuln reachability-diff gates a NEW reachable critical vuln"
: > "$WORK/empty-baseline.txt"
python3 "$DIFF_TOOL" --mode diff --baseline "$WORK/empty-baseline.txt" \
  --head "$POISON_DIR/govulncheck-reachable.json" >/dev/null 2>&1
g2_rc=$?
if [ "$g2_rc" = "3" ]; then
  pass "G2 (gate returned exit 3 = blocked, as designed)"
else
  fail "G2" "reachability-diff did NOT gate a planted reachable critical (rc=$g2_rc) — the Go-Vuln gate is not firing!"
fi

# ── G3: imported-but-not-called critical does NOT gate ────────────────────────
echo "G3: an imported-but-not-called critical does NOT gate (reachability thesis)"
python3 "$DIFF_TOOL" --mode diff --baseline "$WORK/empty-baseline.txt" \
  --head "$POISON_DIR/govulncheck-imported-only.json" >/dev/null 2>&1
g3_rc=$?
if [ "$g3_rc" = "0" ]; then
  pass "G3 (imported-only critical correctly non-gating, exit 0)"
else
  fail "G3" "imported-but-not-called critical INCORRECTLY gated (rc=$g3_rc) — reachability logic regressed!"
fi

# ── G4: OSV NEW-vs-baseline diff flips RED on a planted new vuln ──────────────
echo "G4: OSV-Scan diff gates a planted NEW vuln id vs an empty baseline"
python3 "$OSV_DIFF_TOOL" --mode diff --baseline "$WORK/empty-baseline.txt" \
  --head "$POISON_DIR/osv-report.json" >/dev/null 2>&1
g4_rc=$?
if [ "$g4_rc" = "3" ]; then
  pass "G4 (OSV diff returned exit 3 = blocked, as designed)"
else
  fail "G4" "osv-diff did NOT gate a planted NEW vuln (rc=$g4_rc) — the OSV-Scan gate is not firing!"
fi

echo
if [ "$FAILS" -ne 0 ]; then
  echo "SECURITY-POISON SELF-TEST FAILED ($FAILS leg(s)) — a security scanner is not wired correctly."
  exit 1
fi
echo "security-poison self-test passed — all scanners are wired and firing."
