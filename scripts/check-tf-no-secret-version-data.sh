#!/usr/bin/env bash
# check-tf-no-secret-version-data.sh
#
# Refuse any `data "aws_secretsmanager_secret_version"` block under the
# terraform tree. Reading a secret through that data source writes the
# decrypted value into terraform.tfstate in plain text (ka-0000002892).
# Secrets are written out of band (put-secret-value) and read at runtime.
#
# This is INV-4 of the edge-auth module's verify-secret-guard.sh, re-homed
# here when that module was deleted (PR #1681). The old guard scanned only
# composition/ and the edge-auth module; this one scans every root and module,
# since any module the composition calls lands in the same state file.
#
# Only a real HCL block counts: the match is anchored at the start of a line,
# so a comment that names the data source (to explain why it is not used)
# passes. .terraform/ provider and module caches are skipped.
#
# CONFIG (env):
#   TF_SECRET_SCAN_ROOTS  space-separated dirs to scan
#                         (default: <repo>/gardenio/terraform).
#
# Run:  bash scripts/check-tf-no-secret-version-data.sh
# Test: bash scripts/check-tf-no-secret-version-data_test.sh
# Exit: 0 clean; 1 a forbidden block, a missing root, or a root with no .tf
#       files (a typo must not read as clean).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
read -ra ROOTS <<<"${TF_SECRET_SCAN_ROOTS:-$REPO_ROOT/gardenio/terraform}"

fail() { echo "FAIL: $*" >&2; exit 1; }

hits=""
for root in "${ROOTS[@]}"; do
  [ -d "$root" ] || fail "scan root '$root' does not exist"
  n="$(find "$root" -name .terraform -prune -o -type f -name '*.tf' -print | wc -l)"
  [ "$n" -gt 0 ] || fail "scan root '$root' holds no .tf files"
  h="$(grep -REl --include='*.tf' --exclude-dir=.terraform \
        '^[[:space:]]*data[[:space:]]+"aws_secretsmanager_secret_version"' "$root" || true)"
  [ -z "$h" ] || hits="${hits}${h}"$'\n'
done

if [ -n "$hits" ]; then
  fail "data \"aws_secretsmanager_secret_version\" puts the decrypted secret in terraform state (ka-0000002892); remove it from:"$'\n'"$hits"
fi
echo "check-tf-no-secret-version-data: OK (${ROOTS[*]})"
