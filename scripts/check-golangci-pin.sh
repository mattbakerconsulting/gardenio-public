#!/usr/bin/env bash
# check-golangci-pin.sh
#
# Guard: every golangci-lint version pin in the repo must agree with
# GOLANGCI_LINT_VERSION in gardenio/images/agent/install/versions.env.
#
# WHY THIS EXISTS (ka-0000000665): the agent image pinned golangci-lint 1.64.8
# in versions.env while CI hard-coded a `v2.11.2` install and both
# .golangci.yml files declare `version: "2"`. The two pins drifted silently
# because nothing tied them together. The result was worse than "different
# linters run": the v1 binary shipped in the agent image cannot even LOAD a
# v2-schema config —
#
#   Error: can't load config: the Go language version (go1.24) used to build
#   golangci-lint is lower than the targeted Go version (1.25.12)
#
# — so `make lint` was unrunnable inside an agent pod and CI/Lint was the only
# place the Go lint gate ever executed. Agents could not reproduce a CI lint
# failure locally, which is how a lint red survives to the MR.
#
# The CI pins live in .github/workflows/checks.yml (the `golangci-lint
# (pinned)` step's GOLANGCI_LINT_VERSION env) and ci.yml (the light job's lint
# step). A workflow cannot source versions.env before `env:` is evaluated, so
# the literals stay, and this guard fails loudly if one drifts from
# versions.env. Until 2026-11 the guard scanned
# only the deleted pre-Actions pipeline files, so the live CI pin went
# unguarded (gardenio/helm/ci/README.md recorded the 2.11.2 vs 2.13.2 gap).
#
# Usage:
#   ./scripts/check-golangci-pin.sh                # scan the real repo
#   REPO_ROOT=/tmp/fixture ./scripts/check-golangci-pin.sh
#
# Exit status: 0 if every pin agrees, 1 otherwise.
#
# Companion test: scripts/check-golangci-pin_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

VERSIONS_ENV="${REPO_ROOT}/gardenio/images/agent/install/versions.env"

if [[ ! -f "$VERSIONS_ENV" ]]; then
  echo "check-golangci-pin: missing ${VERSIONS_ENV}" >&2
  exit 1
fi

# The one source of truth. Read by grep rather than `source` so the guard never
# executes the pin file.
EXPECTED="$(sed -n 's/^export GOLANGCI_LINT_VERSION=//p' "$VERSIONS_ENV" | head -1)"
if [[ -z "$EXPECTED" ]]; then
  echo "check-golangci-pin: GOLANGCI_LINT_VERSION not set in ${VERSIONS_ENV}" >&2
  exit 1
fi

# Files that carry a literal golangci-lint version pin and CANNOT source
# versions.env (a workflow `env:` value, a Dockerfile ARG, …). Paths are
# repo-relative; a listed file that does not exist is skipped, so this guard
# survives a file being deleted or renamed without reding the build.
PINNED_FILES=(
  ".github/workflows/checks.yml"
  ".github/workflows/ci.yml"
)

FAILURES=0

# A pin is ANY semver-shaped token on a line that mentions golangci-lint. It is
# deliberately NOT anchored to the tool name: the upstream installer's own
# documented form puts the version in a DETACHED shell argument —
#
#   curl … /golangci-lint/HEAD/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.11.2
#
# — which is exactly the line this repo had, and exactly the drift this guard
# exists to catch. A name-anchored pattern (`golangci-lint@v2.11.2`) silently
# skips it. Other real shapes covered by the same rule:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.2
#   golangci-lint-2.11.2-linux-amd64.tar.gz
#   GOLANGCI_LINT_VERSION: 2.13.2        (a workflow env pin; matched case-blind)
# The leading `v` is optional because the tarball naming omits it.
LINE_RE='golangci[-_]lint'
VER_RE='v?([0-9]+\.[0-9]+\.[0-9]+)'

# emit_pins FILE — print `lineno:version` for every semver on a non-comment
# line mentioning golangci-lint. A line may carry more than one; all are checked
# so a half-done bump cannot hide behind a correct neighbour.
emit_pins() {
  local f="$1"
  grep -inE "$LINE_RE" "$f" 2>/dev/null | while IFS= read -r line; do
    local lineno="${line%%:*}" text="${line#*:}" rest
    # A version named in a comment is documentation, not a pin.
    [[ "$text" =~ ^[[:space:]]*# ]] && continue
    rest="$text"
    while [[ "$rest" =~ $VER_RE ]]; do
      echo "${lineno}:${BASH_REMATCH[1]}"
      rest="${rest#*"${BASH_REMATCH[0]}"}"
    done
  done
}

for rel in "${PINNED_FILES[@]}"; do
  f="${REPO_ROOT}/${rel}"
  [[ -f "$f" ]] || continue

  found_any=0
  while IFS= read -r pin; do
    lineno="${pin%%:*}"
    got="${pin#*:}"
    found_any=1
    if [[ "$got" != "$EXPECTED" ]]; then
      echo "FAIL: ${rel}:${lineno} pins golangci-lint ${got}, but"
      echo "      gardenio/images/agent/install/versions.env pins ${EXPECTED}."
      echo "      A drifted pin means agents run a different linter than CI —"
      echo "      and across the v1/v2 boundary the local binary cannot even"
      echo "      load .golangci.yml. Bump both together."
      FAILURES=$((FAILURES + 1))
    else
      echo "  OK: ${rel}:${lineno} -> ${got}"
    fi
  done < <(emit_pins "$f")

  if [[ "$found_any" == 0 ]]; then
    echo "FAIL: ${rel} is listed as carrying a golangci-lint pin, but no pin was"
    echo "      found. If the pin moved to versions.env sourcing, drop the file"
    echo "      from PINNED_FILES in $(basename "$0")."
    FAILURES=$((FAILURES + 1))
  fi
done

if [[ "$FAILURES" -gt 0 ]]; then
  echo
  echo "check-golangci-pin: ${FAILURES} drifted pin(s). Expected ${EXPECTED}."
  exit 1
fi

echo "check-golangci-pin: all golangci-lint pins agree (${EXPECTED})."
