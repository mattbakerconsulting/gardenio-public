#!/usr/bin/env bash
# check-golangci-pin_test.sh — Unit tests for check-golangci-pin.sh
#
# Each test builds a synthetic mini-repo in a temp dir (versions.env +
# .github/workflows/checks.yml) and asserts the guard's exit status when
# pointed at it via REPO_ROOT, so no test depends on the real working tree.
#
# The final test runs the guard against the REAL repo — a regression guard so a
# future drifted pin reds immediately, not just synthetic fixtures.
#
# Usage:  bash scripts/check-golangci-pin_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/check-golangci-pin.sh"
REAL_REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# make_fixture <dir> <versions_pin> <checks_yml_body>
#   Builds a mini-repo. <checks_yml_body> is written verbatim as
#   .github/workflows/checks.yml.
make_fixture() {
  local dir="$1" vpin="$2" ci_body="$3"
  mkdir -p "${dir}/gardenio/images/agent/install" "${dir}/.github/workflows"
  printf 'export GO_FULL_VERSION=1.25.9\nexport GOLANGCI_LINT_VERSION=%s\n' "$vpin" \
    > "${dir}/gardenio/images/agent/install/versions.env"
  printf '%s\n' "$ci_body" > "${dir}/.github/workflows/checks.yml"
}

# The shape checks.yml has today: the pin is a step `env:` literal and the
# install line interpolates it. Fixture bodies are single-quoted ON PURPOSE:
# they are inert DATA written to a temp file, and `${GOLANGCI_LINT_VERSION}` /
# `$(go env GOPATH)` must survive verbatim as the literal text the guard parses.
# env_ci_yml <version> — that shape, pinned at <version>. The quoted heredoc
# keeps every `$` literal; only the pin line is printed with expansion.
env_ci_yml() {
  printf '      - name: golangci-lint (pinned)\n        env:\n          GOLANGCI_LINT_VERSION: %s\n' "$1"
  cat <<'EOF'
        run: |
          GOBIN="$BIN" go install \
            "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v${GOLANGCI_LINT_VERSION}"
EOF
}

# assert_exit <expected> <name> <fixture_dir>
assert_exit() {
  local expected="$1" name="$2" dir="$3"
  local out rc
  out="$(REPO_ROOT="$dir" bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# ── 1. Agreeing pins pass ────────────────────────────────────────────────
D="${TMPDIR_T}/agree"
make_fixture "$D" 2.13.2 "$(env_ci_yml 2.13.2)"
assert_exit 0 "agreeing pins pass" "$D"

# ── 2. The live gap this retarget closed: image 2.11.2 vs CI 2.13.2 ─────
D="${TMPDIR_T}/env-drift"
make_fixture "$D" 2.11.2 "$(env_ci_yml 2.13.2)"
assert_exit 1 "workflow env pin drifted from versions.env is caught" "$D"

# ── 3. THE REGRESSION (ka-0000000665): the exact pre-fix drift ───────────
# versions.env said 1.64.8 while CI installed v2.11.2 through the upstream
# installer, which puts the version in a DETACHED shell argument far from the
# tool name. A name-anchored regex (`golangci-lint[^ ]*@v2.11.2`) silently
# skips it.
D="${TMPDIR_T}/detached-arg"
# shellcheck disable=SC2016  # literal fixture text; see note above
make_fixture "$D" 1.64.8 '      - run: |
          curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.11.2'
assert_exit 1 "pre-fix drift (image 1.64.8 vs CI v2.11.2, detached argument) is caught" "$D"

# ── 4. A literal `go install …@vX` pin is checked too ───────────────────
D="${TMPDIR_T}/go-install"
make_fixture "$D" 2.13.2 '      - run: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1'
assert_exit 1 "drifted go-install literal is caught (patch-level)" "$D"

# ── 5. Bumping versions.env alone (forgetting CI) is caught ─────────────
# The realistic future failure: someone bumps the image pin and moves on.
D="${TMPDIR_T}/forgot-ci"
make_fixture "$D" 2.14.0 "$(env_ci_yml 2.13.2)"
assert_exit 1 "bumping versions.env without checks.yml is caught" "$D"

# ── 6. A listed file whose pin disappeared fails loudly ─────────────────
D="${TMPDIR_T}/no-pin"
make_fixture "$D" 2.13.2 '      - run: echo no pins here'
assert_exit 1 "a listed file with no pin fails rather than passing vacuously" "$D"

# ── 7. A comment naming a version is NOT a pin ──────────────────────────
D="${TMPDIR_T}/comment"
make_fixture "$D" 2.13.2 "$(env_ci_yml 2.13.2)"
cat >> "${D}/.github/workflows/checks.yml" <<'EOF'
      # Historical note: this used to be golangci-lint@v1.64.8 before the v2 bump.
EOF
assert_exit 0 "a version named in a comment is not treated as a pin" "$D"

# ── 8. Missing versions.env fails loudly rather than passing vacuously ──
D="${TMPDIR_T}/novers"
mkdir -p "${D}/.github/workflows"
assert_exit 1 "missing versions.env fails loudly" "$D"

# ── 9. The REAL repo is consistent ──────────────────────────────────────
assert_exit 0 "real repo pins agree" "$REAL_REPO_ROOT"

echo
echo "check-golangci-pin_test: ${PASS} passed, ${FAIL} failed"
[[ "$FAIL" -eq 0 ]]
