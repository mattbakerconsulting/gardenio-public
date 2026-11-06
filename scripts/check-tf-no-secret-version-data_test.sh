#!/usr/bin/env bash
# check-tf-no-secret-version-data_test.sh
#
# Companion test for scripts/check-tf-no-secret-version-data.sh. Hermetic:
# every case is a fixture under mktemp; the live tree is checked by the
# `terraform` job in checks.yml, not here.
#
# POSITIVE controls (must pass): a clean root, and a root whose only mention
# of the data source is a comment. Without them a guard that always refused
# would pass its own tests.
# NEGATIVE controls (must refuse): the data source in a root, the same in a
# nested module, a missing root, a root with no .tf files.
#
# Run: bash scripts/check-tf-no-secret-version-data_test.sh

set -uo pipefail

GUARD="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-tf-no-secret-version-data.sh"
[ -f "$GUARD" ] || { echo "FAIL: guard not found at $GUARD" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
expect() { # expect pass|fail <name> <roots>
  local want="$1" name="$2" roots="$3" got
  if TF_SECRET_SCAN_ROOTS="$roots" bash "$GUARD" >/dev/null 2>&1; then got=pass; else got=fail; fi
  if [ "$got" = "$want" ]; then
    echo "PASS: $name"
  else
    echo "FAIL: $name (want $want, got $got)" >&2
    failures=$((failures + 1))
  fi
}

mkdir -p "$WORK/clean"
cat >"$WORK/clean/main.tf" <<'EOF'
resource "aws_secretsmanager_secret" "x" {
  name = "x"
}
EOF
expect pass "clean root passes" "$WORK/clean"

mkdir -p "$WORK/comment"
cat >"$WORK/comment/main.tf" <<'EOF'
# Do not read the value with `data "aws_secretsmanager_secret_version"`:
# it lands in state.
resource "aws_secretsmanager_secret" "x" {
  name = "x"
}
EOF
expect pass "a comment naming the data source passes" "$WORK/comment"

mkdir -p "$WORK/planted"
cp "$WORK/clean/main.tf" "$WORK/planted/main.tf"
cat >>"$WORK/planted/main.tf" <<'EOF'
data "aws_secretsmanager_secret_version" "x" {
  secret_id = aws_secretsmanager_secret.x.id
}
EOF
expect fail "a planted data block refuses" "$WORK/planted"

mkdir -p "$WORK/nested/composition" "$WORK/nested/modules/m"
cp "$WORK/clean/main.tf" "$WORK/nested/composition/main.tf"
printf '  data  "aws_secretsmanager_secret_version" "y" {\n  secret_id = "y"\n}\n' \
  >"$WORK/nested/modules/m/secrets.tf"
expect fail "an indented block in a nested module refuses" "$WORK/nested"
expect fail "the bad root refuses when listed after a clean one" "$WORK/clean $WORK/planted"

mkdir -p "$WORK/cached/.terraform/modules/vendor"
cp "$WORK/clean/main.tf" "$WORK/cached/main.tf"
cp "$WORK/planted/main.tf" "$WORK/cached/.terraform/modules/vendor/main.tf"
expect pass "a .terraform cache is skipped" "$WORK/cached"

expect fail "a missing root refuses" "$WORK/does-not-exist"
mkdir -p "$WORK/empty"
expect fail "a root with no .tf files refuses" "$WORK/empty"

if [ "$failures" -gt 0 ]; then
  echo "check-tf-no-secret-version-data_test: $failures case(s) failed" >&2
  exit 1
fi
echo "check-tf-no-secret-version-data_test: all cases passed"
