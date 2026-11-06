#!/usr/bin/env bash
# check-deploy-values-dup-keys_test.sh — Unit tests for
# check-deploy-values-dup-keys.sh
#
# Each test writes a synthetic overlay to a temp file and asserts the guard's
# exit status. No external services. Runs entirely in the main shell so the
# pass/fail counters are reliable.
#
# Requires yamllint on PATH or importable via `python3 -m yamllint`
# (CI installs it with `pip install yamllint`). If yamllint is unavailable the
# test SKIPS with exit 0 so a dev box without it does not red the suite — CI
# guarantees yamllint is present.
#
# Usage:  bash scripts/check-deploy-values-dup-keys_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/check-deploy-values-dup-keys.sh"

# Skip cleanly if yamllint is not available (dev convenience). NB: the
# Makefile lint target ALSO
# invokes the live guard script directly after this test — that invocation will
# loudly FAIL on a yamllint-less machine (via the script's TOOL-MISSING / rc=127
# path), so a missing yamllint still reds `make lint`. This test just exits 0
# rather than reporting fixture failures from an absent tool.
if ! command -v yamllint >/dev/null 2>&1 && \
   ! { command -v python3 >/dev/null 2>&1 && python3 -m yamllint --version >/dev/null 2>&1; }; then
  echo "SKIP: yamllint not installed; the live guard (also run by make lint) will fail loud"
  echo "      Install with: pip install --break-system-packages yamllint"
  exit 0
fi

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <file>
assert_exit() {
  local expected="$1" name="$2" file="$3"
  local out rc
  out="$(bash "$SCRIPT_UNDER_TEST" "$file" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

mkfile() {
  local f
  f="$(mktemp "${TMPDIR_T}/ov.XXXXXX.yaml")"
  cat >"$f"
  echo "$f"
}

# 1. A clean overlay (no duplicate keys) passes.
clean="$(mkfile <<'EOF'
grafana:
  llm:
    enabled: true
    mcp:
      toolsets:
        dashboard: true
        alerting: false
EOF
)"
assert_exit 0 "clean overlay passes" "$clean"

# 2. A duplicate top-level key fails LOUDLY.
dup_top="$(mkfile <<'EOF'
grafana:
  enabled: true
grafana:
  enabled: false
EOF
)"
assert_exit 1 "duplicate top-level key fails" "$dup_top"

# 3. The C7 footgun shape: a duplicate nested key in the toolset allow-list
#    (last-key-wins would flip alerting:false -> true silently). Must fail.
dup_toolset="$(mkfile <<'EOF'
grafana:
  llm:
    mcp:
      toolsets:
        alerting: false
        dashboard: true
        alerting: true
EOF
)"
assert_exit 1 "duplicate nested toolset key (C7 footgun) fails" "$dup_toolset"

# 4. The REAL live overlays must be duplicate-key-free right now (regression
#    guard — if a future merge introduces a dup into a tracked overlay this
#    reds immediately, not just synthetic fixtures).
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
for live in "${REPO_ROOT}"/deploy-values/*.yaml; do
  [[ -f "$live" ]] || continue
  assert_exit 0 "live $(basename "$live") is dup-key-free" "$live"
done

echo ""
echo "PASS=${PASS} FAIL=${FAIL}"
[[ "$FAIL" -eq 0 ]]
