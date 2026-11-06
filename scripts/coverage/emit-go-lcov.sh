#!/usr/bin/env bash
# emit-go-lcov.sh — convert per-module Go cover profiles to LCOV.
#
# Reads:
#   .coverage/unit/gardenio-controller.out
#   .coverage/unit/kbeads.out
#
# Writes:
#   .coverage/unit/gardenio-controller.lcov
#   .coverage/unit/kbeads.lcov
#
# Run `make coverage-go` first to populate the repo-rooted .coverage/unit/
# directory from the per-module profiles. The W3 merge job (ka-0000007391)
# consumes the LCOV outputs as drop-in inputs.
#
# See docs/research/coverage-2a-w1-go-emission.md.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
HELPER="$ROOT/scripts/coverage/go-to-lcov.sh"
COVERAGE_ROOT="${COVERAGE_ROOT:-$ROOT/.coverage/unit}"

for module in gardenio-controller kbeads; do
  in="$COVERAGE_ROOT/$module.out"
  out="$COVERAGE_ROOT/$module.lcov"
  if [[ ! -f "$in" ]]; then
    echo "error: missing input profile $in — run 'make coverage-go' first" >&2
    exit 1
  fi
  "$HELPER" "$in" "$out"
  echo "wrote $out"
done
