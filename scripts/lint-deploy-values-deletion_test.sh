#!/usr/bin/env bash
# lint-deploy-values-deletion_test.sh — unit tests for lint-deploy-values-deletion.sh
#
# Hermetic: uses the DEPLOY_VALUES_DELETED env hook to inject the deleted-file
# set (no git), and feeds MR descriptions via a temp file. No daemon, no
# network. Runs entirely in the main shell so pass/fail counters are reliable.
#
# The RECURRENCE tests replay the three historical occurrences (bug
# ka-0000002710): each asserts that an MR deleting exactly the files the real
# commit deleted, WITHOUT a receipt, is RED — i.e. the guard would have fired.
#
# Usage:  bash scripts/lint-deploy-values-deletion_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUT="${SCRIPT_DIR}/lint-deploy-values-deletion.sh"

PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <deleted-list> <desc-file-or-empty>
assert_exit() {
  local expected="$1" name="$2" deleted="$3" descfile="$4"
  local out rc args=()
  [[ -n "$descfile" ]] && args=("$descfile")
  out="$(env -u CI_MERGE_REQUEST_DESCRIPTION \
             -u CI_MERGE_REQUEST_TARGET_BRANCH_NAME \
             DEPLOY_VALUES_DELETED="$deleted" \
             bash "$SUT" "${args[@]}" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

mkdesc() {
  local f; f="$(mktemp "$TMP/desc.XXXXXX")"
  # %b so literal "\n" in a test string becomes a real newline (line anchors
  # in the lint depend on it). Receipt bodies use heredocs and are unaffected.
  printf '%b' "$1" > "$f"
  echo "$f"
}

# A complete, valid receipt for a given deleted-path string.
receipt() {
  local paths="$1" stranded="$2"
  cat <<EOF
## Summary
Delete a legacy overlay.

<!-- DEPLOY-VALUES-DELETION:BEGIN -->
## Deploy-values deletion receipt
- **Deleted paths:** ${paths}
- **Stranded open beads:** ${stranded}
- **Action taken:** commented on each hit
<!-- DEPLOY-VALUES-DELETION:END -->
EOF
}

# ── No-op: MR deletes nothing under deploy-values/ ────────────────────
assert_exit 0 "no deletions -> pass (no receipt needed)" "" "$(mkdesc "no receipt here")"
assert_exit 0 "non-deploy-values deletion is filtered out -> pass" \
  "gardenio/helm/gardenio/values-test.yaml" "$(mkdesc "irrelevant")"

# ── Deletion WITHOUT receipt -> RED ───────────────────────────────────
assert_exit 1 "deletion without receipt -> fail" \
  "deploy-values/gardenio-rwx.yaml" "$(mkdesc "## Summary\nDeleting an overlay, forgot the receipt.")"

# ── RECURRENCE REPLAYS — the guard must fire on all three occurrences ─
# Occurrence 1: bdf67c368 deleted deploy-values/gardenio.yaml (stranded ka-0000001165).
assert_exit 1 "occurrence-1 (gardenio.yaml, bdf67c368) without receipt -> FIRES" \
  "deploy-values/gardenio.yaml" "$(mkdesc "## Summary\nchore: delete legacy-909 deploy-values/gardenio.yaml (dead file)")"

# Occurrence 2: d6f728709 deleted BOTH gardenio-rwx.yaml and the ephemeral tmpl
# (stranded ka-0000008192 AND ka-0000001165).
assert_exit 1 "occurrence-2 (gardenio-rwx.yaml + ephemeral tmpl, d6f728709) without receipt -> FIRES" \
  "$(printf 'deploy-values/gardenio-rwx.yaml\ndeploy-values/gardenio-rwx-ephemeral.yaml.tmpl')" \
  "$(mkdesc "## Summary\nchore(gardenio/deploy): delete legacy 909-account gardenio-rwx overlays (ka-0000001994)")"

# ── Deletion WITH a complete, matching receipt -> GREEN ───────────────
assert_exit 0 "occurrence-1 WITH receipt naming the path + strand -> pass" \
  "deploy-values/gardenio.yaml" \
  "$(mkdesc "$(receipt 'deploy-values/gardenio.yaml' 'ka-0000001165')")"

assert_exit 0 "occurrence-2 WITH receipt naming BOTH paths + strands -> pass" \
  "$(printf 'deploy-values/gardenio-rwx.yaml\ndeploy-values/gardenio-rwx-ephemeral.yaml.tmpl')" \
  "$(mkdesc "$(receipt 'deploy-values/gardenio-rwx.yaml, deploy-values/gardenio-rwx-ephemeral.yaml.tmpl' 'ka-0000008192, ka-0000001165')")"

# ── Receipt that omits one of the deleted paths -> RED ────────────────
assert_exit 1 "receipt naming only ONE of two deleted paths -> fail" \
  "$(printf 'deploy-values/gardenio-rwx.yaml\ndeploy-values/gardenio-rwx-ephemeral.yaml.tmpl')" \
  "$(mkdesc "$(receipt 'deploy-values/gardenio-rwx.yaml' 'ka-0000001165')")"

# ── Receipt present but 'Stranded open beads' still a placeholder -> RED ─
assert_exit 1 "receipt with placeholder stranded field -> fail" \
  "deploy-values/gardenio.yaml" \
  "$(mkdesc "$(receipt 'deploy-values/gardenio.yaml' '<kd-ids from the sweep>')")"

# ── Receipt present but 'Stranded open beads' empty -> RED ────────────
assert_exit 1 "receipt with empty stranded field -> fail" \
  "deploy-values/gardenio.yaml" \
  "$(mkdesc "$(receipt 'deploy-values/gardenio.yaml' '')")"

# ── Basename-only mention in receipt still counts -> GREEN ────────────
assert_exit 0 "receipt naming basename only -> pass" \
  "deploy-values/gardenio.yaml" \
  "$(mkdesc "$(receipt 'gardenio.yaml' 'none')")"

# ── Exemption off-ramp ────────────────────────────────────────────────
assert_exit 0 "exemption with real reason -> pass" \
  "deploy-values/gardenio.yaml" \
  "$(mkdesc "## Summary\nDeploy-Values-Deletion-Exempt: file was never referenced by any bead, verified manually")"

assert_exit 1 "exemption with placeholder reason -> fail" \
  "deploy-values/gardenio.yaml" \
  "$(mkdesc "## Summary\nDeploy-Values-Deletion-Exempt: <reason>")"

assert_exit 1 "exemption with empty reason -> fail" \
  "deploy-values/gardenio.yaml" \
  "$(mkdesc "## Summary\nDeploy-Values-Deletion-Exempt:")"

# ── Summary ───────────────────────────────────────────────────────────
echo
echo "lint-deploy-values-deletion_test: ${PASS} passed, ${FAIL} failed."
(( FAIL == 0 )) || exit 1
