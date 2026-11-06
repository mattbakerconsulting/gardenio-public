#!/usr/bin/env bash
# scripts/coverage/merge.sh — orchestrate cross-component LCOV merge.
#
# Inputs (per W3 §8 + §8.6 update on ka-0000007391):
#   - controller-unit.lcov     (gardenio/controller, go test -coverprofile)
#   - controller-e2e.lcov      (gardenio/controller, GOCOVERDIR + textfmt)
#   - kbeads-unit.lcov         (kbeads, go test -coverprofile)
#   - coop-unit.lcov           (coop, cargo llvm-cov --lcov)
#   - coop-e2e.lcov            (coop, grcov -t lcov)
#
# Outputs (in --output-dir, default: ./coverage):
#   - coverage.unit.lcov       (all unit-source LCOVs, normalized + merged)
#   - coverage.e2e.lcov        (all e2e-source LCOVs, normalized + merged)
#   - coverage.merged.lcov     (everything, normalized + merged)
#   - coverage.cobertura.xml   (derived from merged via lcov_cobertura)
#   - stdout: exactly one line "Coverage: NN.N%" for the W4 regex.
#
# Inputs are discovered in --input-dir matching one of:
#   {component}-{provenance}.lcov
#   {component}/{provenance}.lcov
# where provenance ∈ {unit,e2e}. Missing inputs are tolerated; an empty
# provenance bucket simply yields an empty merged file in that bucket.
#
# Usage:
#   scripts/coverage/merge.sh [--input-dir DIR] [--output-dir DIR]
#                             [--workspace-root DIR] [--no-cobertura]
#                             [--lcov-bin PATH] [--strict]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

INPUT_DIR="${COVERAGE_INPUT_DIR:-./coverage-raw}"
OUTPUT_DIR="${COVERAGE_OUTPUT_DIR:-./coverage}"
WORKSPACE_ROOT="${COVERAGE_WORKSPACE_ROOT:-$PWD}"
DO_COBERTURA=1
STRICT=0
LCOV_BIN="${LCOV_BIN:-lcov}"

while [[ $# -gt 0 ]]; do
  case $1 in
    --input-dir)       INPUT_DIR="$2"; shift 2 ;;
    --output-dir)      OUTPUT_DIR="$2"; shift 2 ;;
    --workspace-root)  WORKSPACE_ROOT="$2"; shift 2 ;;
    --no-cobertura)    DO_COBERTURA=0; shift ;;
    --lcov-bin)        LCOV_BIN="$2"; shift 2 ;;
    --strict)          STRICT=1; shift ;;
    -h|--help)
      sed -n '2,30p' "$0" | sed 's/^# \?//'
      exit 0
      ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

mkdir -p "$OUTPUT_DIR"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

NORMALIZER="${SCRIPT_DIR}/normalize-paths.py"
if [[ ! -x "$NORMALIZER" ]]; then
  chmod +x "$NORMALIZER" 2>/dev/null || true
fi

# ── Discover inputs ──────────────────────────────────────────────────
COMPONENTS=(controller kbeads coop)
PROVENANCES=(unit e2e)

declare -a UNIT_INPUTS=()
declare -a E2E_INPUTS=()

discover_input() {
  local component="$1" provenance="$2"
  local candidates=(
    "${INPUT_DIR}/${component}-${provenance}.lcov"
    "${INPUT_DIR}/${component}/${provenance}.lcov"
    "${INPUT_DIR}/${component}_${provenance}.lcov"
  )
  for c in "${candidates[@]}"; do
    if [[ -s "$c" ]]; then
      printf '%s\n' "$c"
      return 0
    fi
  done
  return 1
}

for c in "${COMPONENTS[@]}"; do
  for p in "${PROVENANCES[@]}"; do
    if found="$(discover_input "$c" "$p")"; then
      echo "found: $c $p -> $found" >&2
      if [[ "$p" == "unit" ]]; then
        UNIT_INPUTS+=("$c|$found")
      else
        E2E_INPUTS+=("$c|$found")
      fi
    else
      echo "missing: $c $p (skipping)" >&2
    fi
  done
done

# ── Normalize each input independently ───────────────────────────────
normalize_one() {
  local component="$1" src="$2" dst="$3"
  local args=(
    --input "$src"
    --output "$dst"
    --workspace-root "$WORKSPACE_ROOT"
  )
  [[ "$STRICT" -eq 1 ]] && args+=(--strict)
  python3 "$NORMALIZER" "${args[@]}"
}

declare -a UNIT_NORM=()
declare -a E2E_NORM=()

for entry in "${UNIT_INPUTS[@]}"; do
  c="${entry%%|*}"; src="${entry#*|}"
  dst="${WORK}/${c}-unit.norm.lcov"
  normalize_one "$c" "$src" "$dst"
  UNIT_NORM+=("$dst")
done

for entry in "${E2E_INPUTS[@]}"; do
  c="${entry%%|*}"; src="${entry#*|}"
  dst="${WORK}/${c}-e2e.norm.lcov"
  normalize_one "$c" "$src" "$dst"
  E2E_NORM+=("$dst")
done

# ── Merge with `lcov -a` if available; otherwise concat (lcov-aware
# tools sum DA records on read either way) ──────────────────────────
merge_lcov() {
  local out="$1"; shift
  local -a inputs=()
  for f in "$@"; do
    [[ -n "$f" ]] && inputs+=("$f")
  done
  if [[ ${#inputs[@]} -eq 0 ]]; then
    : > "$out"
    return 0
  fi
  if command -v "$LCOV_BIN" >/dev/null 2>&1; then
    local args=()
    for f in "${inputs[@]}"; do args+=("-a" "$f"); done
    "$LCOV_BIN" "${args[@]}" -o "$out" >/dev/null 2>&1 || {
      echo "lcov merge failed; falling back to concat" >&2
      cat "${inputs[@]}" > "$out"
    }
  else
    # Fallback: concat. Downstream tools that read multi-tracefile LCOV
    # (lcov_cobertura, codecov, sonar) sum DA records correctly.
    cat "${inputs[@]}" > "$out"
  fi
}

UNIT_OUT="${OUTPUT_DIR}/coverage.unit.lcov"
E2E_OUT="${OUTPUT_DIR}/coverage.e2e.lcov"
MERGED_OUT="${OUTPUT_DIR}/coverage.merged.lcov"

merge_lcov "$UNIT_OUT" "${UNIT_NORM[@]+"${UNIT_NORM[@]}"}"
merge_lcov "$E2E_OUT"  "${E2E_NORM[@]+"${E2E_NORM[@]}"}"
merge_lcov "$MERGED_OUT" \
  "${UNIT_NORM[@]+"${UNIT_NORM[@]}"}" \
  "${E2E_NORM[@]+"${E2E_NORM[@]}"}"

# ── Derive Cobertura ─────────────────────────────────────────────────
COBERTURA_OUT="${OUTPUT_DIR}/coverage.cobertura.xml"
if [[ "$DO_COBERTURA" -eq 1 ]]; then
  if "${SCRIPT_DIR}/derive-cobertura.sh" \
        --input  "$MERGED_OUT" \
        --output "$COBERTURA_OUT" >&2; then
    :
  else
    echo "cobertura derivation failed (continuing)" >&2
  fi
fi

# ── Compute single-line coverage % ───────────────────────────────────
# We compute LH/LF directly from the merged LCOV. This avoids depending
# on `lcov --summary` parsing for the W4 regex line, and matches the
# numerator/denominator semantics that lcov_cobertura uses.
read -r LH LF <<<"$(awk '
  /^LH:/ { lh += substr($0,4)+0 }
  /^LF:/ { lf += substr($0,4)+0 }
  END { printf "%d %d", lh, lf }
' "$MERGED_OUT")"

if [[ "${LF:-0}" -gt 0 ]]; then
  PCT="$(awk -v lh="$LH" -v lf="$LF" 'BEGIN { printf "%.1f", (lh/lf)*100 }')"
else
  PCT="0.0"
fi

# Exactly one regex-anchored line on stdout for the W4 'coverage:' regex.
printf 'Coverage: %s%%\n' "$PCT"
