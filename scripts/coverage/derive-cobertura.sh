#!/usr/bin/env bash
# scripts/coverage/derive-cobertura.sh — wrap lcov_cobertura.
#
# Derives a Cobertura XML from a merged LCOV tracefile. Cobertura is
# consumed by GitLab's MR widget (artifacts:reports:coverage_report);
# LCOV remains the canonical merged artifact (research §canonical format).
#
# Usage:
#   scripts/coverage/derive-cobertura.sh --input MERGED.lcov --output coverage.xml
#                                        [--base-dir DIR]
#
# Strategy (in order, first available wins):
#   1. lcov_cobertura on PATH.
#   2. python3 -m lcov_cobertura.
#   3. pip install --user lcov_cobertura into a tempdir, then run.
#   4. Fail with a non-zero exit and an actionable message.

set -euo pipefail

INPUT=""
OUTPUT=""
BASE_DIR=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --input)    INPUT="$2"; shift 2 ;;
    --output)   OUTPUT="$2"; shift 2 ;;
    --base-dir) BASE_DIR="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,18p' "$0" | sed 's/^# \?//'
      exit 0
      ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

[[ -n "$INPUT"  ]] || { echo "missing --input"  >&2; exit 1; }
[[ -n "$OUTPUT" ]] || { echo "missing --output" >&2; exit 1; }
[[ -s "$INPUT"  ]] || {
  echo "warn: $INPUT is empty; emitting minimal Cobertura" >&2
  cat > "$OUTPUT" <<'XML'
<?xml version="1.0" ?>
<!DOCTYPE coverage SYSTEM "http://cobertura.sourceforge.net/xml/coverage-04.dtd">
<coverage line-rate="0" branch-rate="0" lines-covered="0" lines-valid="0" branches-covered="0" branches-valid="0" complexity="0" version="2.0.3" timestamp="0">
  <sources/>
  <packages/>
</coverage>
XML
  exit 0
}

BASE_ARGS=()
[[ -n "$BASE_DIR" ]] && BASE_ARGS+=(--base-dir "$BASE_DIR")

run_lcov_cobertura() {
  if command -v lcov_cobertura >/dev/null 2>&1; then
    lcov_cobertura "$INPUT" --output "$OUTPUT" "${BASE_ARGS[@]}"
    return $?
  fi
  if python3 -c 'import lcov_cobertura' >/dev/null 2>&1; then
    python3 -m lcov_cobertura "$INPUT" --output "$OUTPUT" "${BASE_ARGS[@]}"
    return $?
  fi
  return 127
}

if run_lcov_cobertura; then
  exit 0
fi

# Last resort: install into a scratch venv. Bounded by network access;
# CI images should pre-install lcov_cobertura so this branch is rare.
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

if python3 -m pip install --quiet --target "$SCRATCH" lcov_cobertura >/dev/null 2>&1; then
  PYTHONPATH="$SCRATCH" python3 -m lcov_cobertura "$INPUT" --output "$OUTPUT" "${BASE_ARGS[@]}"
  exit $?
fi

echo "derive-cobertura: lcov_cobertura not available and pip install failed" >&2
echo "  install with: pip install lcov_cobertura" >&2
exit 1
