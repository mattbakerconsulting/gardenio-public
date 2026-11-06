#!/usr/bin/env bash
# go-to-lcov.sh — convert a Go `-coverprofile` text file to LCOV via gcov2lcov.
#
# Usage: go-to-lcov.sh <input.out> <output.lcov>
#
# gcov2lcov v1.1.1 (Apache-2.0, jandelgado, 2024) handles atomic-mode profiles
# correctly — see W1 §5.2 of docs/research/coverage-tooling-per-language.md.
# Install via: go install github.com/jandelgado/gcov2lcov@v1.1.1
#
# Path normalization (SF: prefix rewriting across modules) is NOT this script's
# job — the W3 merge step (docs/research/coverage-2a-w3-merge.md) is responsible
# for that, since it has to be coordinated across all components and reconciled
# with the repo-rooted scheme.

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <input.out> <output.lcov>" >&2
  exit 2
fi

input="$1"
output="$2"

if [[ ! -f "$input" ]]; then
  echo "error: input profile not found: $input" >&2
  exit 1
fi

if ! command -v gcov2lcov >/dev/null 2>&1; then
  echo "error: gcov2lcov not on PATH — install with:" >&2
  echo "  go install github.com/jandelgado/gcov2lcov@v1.1.1" >&2
  exit 1
fi

mkdir -p "$(dirname "$output")"
gcov2lcov -infile "$input" -outfile "$output"
