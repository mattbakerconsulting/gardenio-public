#!/usr/bin/env bash
# lint-trickle-mr_test.sh — Unit tests for lint-trickle-mr.sh
#
# Each test writes a synthetic MR description to a temp file and asserts the
# lint's exit status. No external services. Runs entirely in the main shell
# (no pipes into the assert helper) so pass/fail counters are reliable.
#
# Usage:  bash scripts/lint-trickle-mr_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/lint-trickle-mr.sh"
TEMPLATE="${SCRIPT_DIR}/../.gitlab/merge_request_templates/trickle.md"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <file> [env-assignments...]
# Runs the lint on <file> and compares exit status to <expected>.
assert_exit() {
  local expected="$1" name="$2" file="$3"; shift 3
  local out rc
  out="$(env -u TRICKLE_MR -u CI_MERGE_REQUEST_DESCRIPTION "$@" \
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

# A complete, valid provenance block.
complete_block() {
  cat <<'EOF'
## Summary

Adds the foo helper.

<!-- PROVENANCE:BEGIN -->
## Provenance

- **Source dark commits/MRs:** dark!1234, abc1234
- **Bead IDs:** ka-0000003961
- **Affordance results:** /code-review: pass, /security-review: pass, tests: pass
- **Dwell time in dark:** 6 days, 3 dark MRs
- **Beads-viewer link:** https://beads-viewer.e2e.example.com/bead/ka-0000003961
<!-- PROVENANCE:END -->

## What this is NOT

Does not touch auth.
EOF
}

# ── Tests ───────────────────────────────────────────────────────────

# 1. Complete provenance block passes.
f="$(complete_block | mkdesc)"
assert_exit 0 "complete provenance block passes" "$f"

# 2. Non-trickle MR (no markers, TRICKLE_MR unset) is a no-op pass.
f="$(printf '## Summary\n\nAn ordinary MR with no provenance markers.\n' | mkdesc)"
assert_exit 0 "ordinary MR without markers is skipped" "$f"

# 3. Missing a required field fails.
f="$(complete_block | grep -v 'Dwell time' | mkdesc)"
assert_exit 1 "missing dwell-time field fails" "$f"

# 4. Placeholder value (template not filled in) fails.
assert_exit 1 "unfilled template fails" "$TEMPLATE"

# 5. Empty-value field fails.
f="$(mkdesc <<'EOF'
<!-- PROVENANCE:BEGIN -->
- **Source dark commits/MRs:** dark!1
- **Bead IDs:** gd-x
- **Affordance results:** pass
- **Dwell time in dark:**
- **Beads-viewer link:** https://x/y
<!-- PROVENANCE:END -->
EOF
)"
assert_exit 1 "empty field value fails" "$f"

# 6. Placeholder ("e.g. ...") value fails.
f="$(mkdesc <<'EOF'
<!-- PROVENANCE:BEGIN -->
- **Source dark commits/MRs:** e.g. dark!1234
- **Bead IDs:** gd-x
- **Affordance results:** pass
- **Dwell time in dark:** 2 days
- **Beads-viewer link:** https://x/y
<!-- PROVENANCE:END -->
EOF
)"
assert_exit 1 "e.g. placeholder value fails" "$f"

# 7. TRICKLE_MR=1 with no markers fails (explicit opt-in but block missing).
f="$(printf 'no provenance here\n' | mkdesc)"
assert_exit 1 "TRICKLE_MR=1 without block fails" "$f" TRICKLE_MR=1

# 8. CI_MERGE_REQUEST_DESCRIPTION env path works for a complete block.
out="$(CI_MERGE_REQUEST_DESCRIPTION="$(complete_block)" bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "0" ]]; then pass "reads CI_MERGE_REQUEST_DESCRIPTION env"; else
  fail "reads CI_MERGE_REQUEST_DESCRIPTION env" "expected 0, got ${rc}: ${out}"; fi

# 9. Comment-only value (key present, value is just an HTML comment) fails.
f="$(mkdesc <<'EOF'
<!-- PROVENANCE:BEGIN -->
- **Source dark commits/MRs:** <!-- dark!1234 -->
- **Bead IDs:** gd-x
- **Affordance results:** pass
- **Dwell time in dark:** 2 days
- **Beads-viewer link:** https://x/y
<!-- PROVENANCE:END -->
EOF
)"
assert_exit 1 "comment-only value fails" "$f"

# 10. An MR that merely *mentions* the marker token in prose/backticks (e.g.
#     the MR that introduces this very template) is NOT a trickle MR and must
#     be skipped — only real `<!-- PROVENANCE:BEGIN -->` HTML-comment markers
#     opt an MR into the check. Regression guard for the self-gating bug.
f="$(mkdesc <<'EOF'
## Summary

Adds the trickle template with machine-checkable `PROVENANCE:BEGIN` /
`PROVENANCE:END` markers and a lint enforcing the provenance block.

This ordinary feature MR talks about PROVENANCE:BEGIN and PROVENANCE:END
but carries no real provenance block, so it must not be gated.
EOF
)"
assert_exit 0 "prose mention of marker token is not a trickle MR" "$f"

# 11. Real markers present without a body still gate (markers are the trigger).
f="$(printf '<!-- PROVENANCE:BEGIN -->\n<!-- PROVENANCE:END -->\n' | mkdesc)"
assert_exit 1 "real markers with empty block fail" "$f"

# ── Summary ─────────────────────────────────────────────────────────
echo "----"
echo "lint-trickle-mr_test: ${PASS} passed, ${FAIL} failed"
(( FAIL == 0 ))
