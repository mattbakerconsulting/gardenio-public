#!/usr/bin/env bash
# lint-trust-dossier_test.sh — Unit tests for lint-trust-dossier.sh
#
# Each test writes a synthetic MR description to a temp file and asserts the
# lint's exit status. No external services. Runs entirely in the main shell
# (no pipes into the assert helper) so pass/fail counters are reliable.
#
# Usage:  bash scripts/lint-trust-dossier_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/lint-trust-dossier.sh"
TEMPLATE="${SCRIPT_DIR}/../.gitlab/merge_request_templates/trust-dossier.md"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <file> [env-assignments...]
assert_exit() {
  local expected="$1" name="$2" file="$3"; shift 3
  local out rc
  out="$(env -u DOSSIER_MR -u CI_MERGE_REQUEST_DESCRIPTION "$@" \
        bash "$SCRIPT_UNDER_TEST" "$file" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# Write a description to a fresh temp file and echo its path.
mkdesc() {
  local f
  f="$(mktemp "${TMPDIR_T}/desc.XXXXXX")"
  cat >"$f"
  echo "$f"
}

# A complete, valid dossier.
complete_dossier() {
  cat <<'EOF'
<!-- DOSSIER:BEGIN -->

## Summary

Adds the foo helper to the bar module.

## Why

The bar module recomputed baz on every call, which showed up as an N+1 in the
gardener hot path; the foo helper memoizes it.

## Alternatives considered

Considered a package-level cache map, rejected because it is not goroutine-safe
without a lock that would reintroduce the contention we are removing.

## Risk assessment

Blast radius is one function; rollback is a straight revert. Worst case a stale
memo returns an old baz, guarded by the existing invalidate-on-write path.

## Evidence

- Standalone build: upstream-HEAD run 12345 green
- /code-review: gd-review1, no findings
- /security-review: clean, gitleaks green

## Provenance

- Dark MR: dark!1234
- Dark bead: gd-abcd1234
- Peeled from: gd-efgh5678

<!-- DOSSIER:END -->

## What this is NOT

Does not touch the auth middleware.

---
Operator: Matthew Baker (oidc:matthew.baker@acme.example)
Bead: ka-0000005561
EOF
}

# ── Tests ───────────────────────────────────────────────────────────

# 1. Complete dossier passes.
f="$(complete_dossier | mkdesc)"
assert_exit 0 "complete dossier passes" "$f"

# 2. Non-dossier MR (no markers, DOSSIER_MR unset) is a no-op pass.
f="$(printf '## Summary\n\nAn ordinary MR with no dossier markers.\n' | mkdesc)"
assert_exit 0 "ordinary MR without markers is skipped" "$f"

# 3. Missing a required section (heading removed) fails.
f="$(complete_dossier | grep -v '^## Risk assessment' | mkdesc)"
assert_exit 1 "missing Risk assessment heading fails" "$f"

# 4. Why byte-equal to Summary fails.
f="$(cat <<'EOF' | mkdesc
<!-- DOSSIER:BEGIN -->
## Summary

Adds the foo helper.

## Why

Adds the foo helper.

## Alternatives considered

Considered a cache map, rejected for lock contention.

## Risk assessment

One function; straight revert.

## Evidence

- Standalone build: run 1 green

## Provenance

- Dark bead: gd-abcd1234

<!-- DOSSIER:END -->
EOF
)"
assert_exit 1 "Why byte-equal to Summary fails" "$f"

# 5. Empty Why (placeholder only) fails.
f="$(cat <<'EOF' | mkdesc
<!-- DOSSIER:BEGIN -->
## Summary

Adds the foo helper.

## Why

_The problem this solves and why it was requested — distinct from Summary._

## Alternatives considered

Considered a cache map, rejected for lock contention.

## Risk assessment

One function; straight revert.

## Evidence

- Standalone build: run 1 green

## Provenance

- Dark bead: gd-abcd1234

<!-- DOSSIER:END -->
EOF
)"
assert_exit 1 "placeholder Why fails" "$f"

# 6. The unfilled template itself fails (all placeholders).
if [[ -f "$TEMPLATE" ]]; then
  assert_exit 1 "unfilled template fails" "$TEMPLATE"
else
  fail "unfilled template fails" "template not found at $TEMPLATE"
fi

# 7. DOSSIER_MR=1 without markers fails (opted in but block absent).
f="$(printf '## Summary\n\nNo markers here.\n' | mkdesc)"
assert_exit 1 "DOSSIER_MR=1 without markers fails" "$f" DOSSIER_MR=1

# 8. Prose mention of the token (backtick-quoted) does NOT opt in.
# shellcheck disable=SC2016  # literal backticks are intentional test content
f="$(printf 'This MR documents the `DOSSIER:BEGIN` marker convention.\n' | mkdesc)"
assert_exit 0 "prose mention of marker token is not opted in" "$f"

# 9. Valid Trickle-Size-Exception trailer passes.
f="$(complete_dossier | sed '/^Bead:/a Trickle-Size-Exception: generated protobuf bump pushed net LOC to 520; all mechanical.' | mkdesc)"
assert_exit 0 "valid Trickle-Size-Exception trailer passes" "$f"

# 10. Bare Trickle-Size-Exception trailer (no reason) fails.
f="$(complete_dossier | sed '/^Bead:/a Trickle-Size-Exception:' | mkdesc)"
assert_exit 1 "bare Trickle-Size-Exception trailer fails" "$f"

# 11. Valid Multi-Concern-Ack trailer passes.
f="$(complete_dossier | sed '/^Bead:/a Multi-Concern-Ack: touches both the helper and its one caller; splitting would leave a dead reference.' | mkdesc)"
assert_exit 0 "valid Multi-Concern-Ack trailer passes" "$f"

# 12. stdin ('-') path works.
out="$(env -u DOSSIER_MR -u CI_MERGE_REQUEST_DESCRIPTION \
      bash "$SCRIPT_UNDER_TEST" - < "$(complete_dossier | mkdesc)" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "0" ]]; then pass "stdin path works"; else fail "stdin path works" "rc=$rc out=$out"; fi

# 13. CI_MERGE_REQUEST_DESCRIPTION env path works.
out="$(env -u DOSSIER_MR CI_MERGE_REQUEST_DESCRIPTION="$(complete_dossier)" \
      bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "0" ]]; then pass "CI env path works"; else fail "CI env path works" "rc=$rc out=$out"; fi

# ── Summary ─────────────────────────────────────────────────────────
echo "----------------------------------------"
echo "lint-trust-dossier_test: ${PASS} passed, ${FAIL} failed"
(( FAIL == 0 )) || exit 1
