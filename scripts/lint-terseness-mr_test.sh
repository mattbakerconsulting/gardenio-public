#!/usr/bin/env bash
# lint-terseness-mr_test.sh — Unit tests for lint-terseness-mr.sh
#
# Each test writes a synthetic MR description (and, where needed, a synthetic
# unified diff) to temp files and asserts the lint's exit status. No external
# services. Runs entirely in the main shell so pass/fail counters are reliable.
#
# Usage:  bash scripts/lint-terseness-mr_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/lint-terseness-mr.sh"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <desc-file> [env-assignments...]
# Runs the lint on <desc-file> with a clean CI env and compares exit status.
assert_exit() {
  local expected="$1" name="$2" file="$3"; shift 3
  local out rc
  out="$(env -u CI_MERGE_REQUEST_DESCRIPTION -u CI_MERGE_REQUEST_DIFF_BASE_SHA \
          -u TRICKLE_MR -u TERSENESS_DIFF_FILE "$@" \
          bash "$SCRIPT_UNDER_TEST" "$file" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# Write content to a fresh temp file and echo its path.
mkfile() {
  local f
  f="$(mktemp "${TMPDIR_T}/f.XXXXXX")"
  cat >"$f"
  echo "$f"
}

# A valid PROVENANCE block that opts an MR into the trickle gate.
provenance_block() {
  cat <<'EOF'
<!-- PROVENANCE:BEGIN -->
## Provenance

- **Source dark commits/MRs:** dark!1234, abc1234
- **Bead IDs:** ka-0000007571
- **Affordance results:** /code-review: pass · tests: pass
- **Dwell time in dark:** 6 days, 3 dark MRs
- **Beads-viewer link:** https://gardenio.e2e.example.com/beads/ka-0000007571
<!-- PROVENANCE:END -->
EOF
}

# A small, well-formed diff (~6 changed lines): passes the diff-size gate.
small_diff() {
  cat <<'EOF'
diff --git a/foo.go b/foo.go
index 1111111..2222222 100644
--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,3 @@
-old line one
-old line two
+new line one
+new line two
+new line three
EOF
}

# A diff whose changed-line count exceeds a given number.
big_diff() {
  local n="$1" i
  echo "diff --git a/big.txt b/big.txt"
  echo "--- a/big.txt"
  echo "+++ b/big.txt"
  echo "@@ -1,0 +1,${n} @@"
  for (( i = 0; i < n; i++ )); do
    echo "+added content line number ${i} with some text"
  done
}

# ── Off-ramp: non-trickle MRs are a no-op pass ────────────────────────
f="$(printf 'Just an ordinary MR description with no provenance markers.\n' | mkfile)"
assert_exit 0 "ordinary MR (no markers) is a no-op pass" "$f"

# A prose mention of the token (not the real marker) must NOT opt in.
f="$(printf 'We follow the PROVENANCE:BEGIN convention in trickle MRs.\n' | mkfile)"
assert_exit 0 "prose mention of PROVENANCE:BEGIN does not opt in" "$f"

# ── Trickle MR, small diff, terse prose → pass ────────────────────────
diff_f="$(small_diff | mkfile)"
f="$(
  { echo "## Summary"; echo; echo "Peels the foo fix to real main."; echo; provenance_block; } | mkfile
)"
assert_exit 0 "trickle + small diff + terse prose passes" "$f" \
  TERSENESS_DIFF_FILE="$diff_f"

# ── TRICKLE_MR=1 opts in even without markers; still needs a diff ─────
f="$(printf '## Summary\n\nTerse.\n' | mkfile)"
assert_exit 0 "TRICKLE_MR=1 with small diff passes" "$f" \
  TRICKLE_MR=1 TERSENESS_DIFF_FILE="$diff_f"

# ── Diff-size hard block (#6) ─────────────────────────────────────────
big_f="$(big_diff 600 | mkfile)"
assert_exit 1 "diff over hard limit (600 > 500) blocks" "$f" \
  TRICKLE_MR=1 TERSENESS_DIFF_FILE="$big_f"

# A 300-line diff warns but passes (200 < 300 <= 500).
warn_f="$(big_diff 300 | mkfile)"
assert_exit 0 "diff in warn band (300) passes" "$f" \
  TRICKLE_MR=1 TERSENESS_DIFF_FILE="$warn_f"

# Env override lowers the hard limit so the warn-band diff now blocks.
assert_exit 1 "TERSENESS_MAX_DIFF_LINES override blocks 300" "$f" \
  TRICKLE_MR=1 TERSENESS_DIFF_FILE="$warn_f" TERSENESS_MAX_DIFF_LINES=250

# ── Verbosity ratio (#7): a rationale wall over a tiny diff blocks ────
wall="$(
  {
    echo "## Summary"; echo
    # ~2000 chars of defensive rationale prose (well over the 1200 floor).
    for i in $(seq 1 40); do
      echo "This paragraph re-derives the correctness of the change in exhaustive detail, restating why each line is safe and defensible, which is exactly the rationale-wall verbosity the terseness gate exists to stop."
    done
    echo; provenance_block
  } | mkfile
)"
assert_exit 1 "rationale wall over tiny diff blocks on ratio" "$wall" \
  TERSENESS_DIFF_FILE="$diff_f"

# ── Prose floor guards terse small MRs (R5 U4) ────────────────────────
# A short summary over a 1-line diff has a high ratio but tiny prose → OK.
tiny_diff="$(
  { echo "diff --git a/x b/x"; echo "--- a/x"; echo "+++ b/x"; echo "@@ -1 +1 @@"; echo "+x"; } | mkfile
)"
f="$(
  { echo "## Summary"; echo; echo "Fixes the typo."; echo; provenance_block; } | mkfile
)"
assert_exit 0 "terse short prose over tiny diff passes (below floor)" "$f" \
  TERSENESS_DIFF_FILE="$tiny_diff"

# ── Provenance is excluded from the prose count ───────────────────────
# The same rationale wall, but placed INSIDE the provenance block, is excluded
# from the prose count and must therefore pass the ratio check. We build a
# provenance block padded with a long affordance value.
prov_padded="$(
  {
    echo "## Summary"; echo; echo "Terse one-liner."; echo
    echo "<!-- PROVENANCE:BEGIN -->"
    echo "## Provenance"
    echo "- **Source dark commits/MRs:** dark!1234"
    echo "- **Bead IDs:** ka-0000007571"
    printf -- "- **Affordance results:** "
    for i in $(seq 1 40); do printf 'code-review pass and tests pass and security review pass and more detail here, '; done
    echo
    echo "- **Dwell time in dark:** 6 days"
    echo "- **Beads-viewer link:** https://example/beads/ka-0000007571"
    echo "<!-- PROVENANCE:END -->"
  } | mkfile
)"
assert_exit 0 "long PROVENANCE block is excluded from prose (passes ratio)" "$prov_padded" \
  TERSENESS_DIFF_FILE="$tiny_diff"

# ── Exemption off-ramp ────────────────────────────────────────────────
exempt="$(
  { echo "## Summary"; echo; echo "Terse."; echo "Terseness-Exempt: atomic generated file, cannot split"; echo; provenance_block; } | mkfile
)"
assert_exit 0 "Terseness-Exempt with a reason bypasses the gate" "$exempt" \
  TERSENESS_DIFF_FILE="$big_f"

# An exemption line with no reason is rejected.
exempt_bad="$(
  { echo "## Summary"; echo; echo "Terse."; echo "Terseness-Exempt: <reason>"; echo; provenance_block; } | mkfile
)"
assert_exit 1 "Terseness-Exempt with placeholder reason is rejected" "$exempt_bad" \
  TERSENESS_DIFF_FILE="$big_f"

# ── Missing diff source on a trickle MR → warn + pass (infra off-ramp) ─
f="$(
  { echo "## Summary"; echo; echo "Terse."; echo; provenance_block; } | mkfile
)"
assert_exit 0 "trickle MR with no resolvable diff warns and passes" "$f"

# ── Summary ───────────────────────────────────────────────────────────
echo
echo "── lint-terseness-mr_test: ${PASS} passed, ${FAIL} failed ──"
(( FAIL == 0 ))
