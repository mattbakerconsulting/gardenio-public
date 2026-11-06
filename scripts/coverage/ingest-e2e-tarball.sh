#!/usr/bin/env bash
# scripts/coverage/ingest-e2e-tarball.sh — translate the drained E2E
# coverage tarball into per-component LCOV inputs for scripts/coverage/merge.sh.
#
# Input layout (per W2 §3.2; see research bundle ka-0000001408 §5.2):
#   <staging>/<binary>/covmeta.*       + covcounters.*    (Go binaries)
#   <staging>/<binary>/*.profraw                          (Rust binaries: coop, coopmux)
#   <staging>/agents/<pod>/go/*                           (agent pod Go data)
#   <staging>/agents/<pod>/coop-*.profraw                 (agent pod Rust data)
#
# Output (under --output-dir, default .coverage-raw/):
#   controller-e2e.lcov   controller module rollup
#                         (controller, beads-viewer, slack-bridge,
#                          jira-bridge, gitlab-bridge, push-bridge,
#                          trigger-engine,
#                          agents/*/go — gb cli inside agent pods)
#   kbeads-e2e.lcov       kbeads daemon
#   coop-e2e.lcov         coop + coopmux (+ agents/*/coop-*.profraw)
#
# Tools required (the script SKIPs cleanly when a tool is missing — empty
# LCOVs are emitted; downstream merge.sh tolerates empty provenance buckets):
#   go (1.20+)                 -- always required (Go covdata merge + textfmt)
#   gcov2lcov v1.1.1           -- go install github.com/jandelgado/gcov2lcov@v1.1.1
#   llvm-profdata              -- llvm tools (Rust merge)
#   cargo-llvm-cov 0.6+        -- cargo install cargo-llvm-cov --version "^0.6"
#
# Usage:
#   scripts/coverage/ingest-e2e-tarball.sh [--tarball PATH] [--output-dir DIR]
#                                          [--workspace-root DIR]
#                                          [--staging-dir DIR]
#                                          [--coop-object PATH]
#                                          [--coopmux-object PATH]
#                                          [--strict]
#
# Exit: writes the three -e2e.lcov files in the layout merge.sh discovers
# (scripts/coverage/merge.sh §"Discover inputs"), and prints a one-line
# coverage summary per component on stdout.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_TO_LCOV="${SCRIPT_DIR}/go-to-lcov.sh"

TARBALL="${COVERAGE_E2E_TARBALL:-coverage-artifacts/coverage.tar.gz}"
OUTPUT_DIR="${COVERAGE_E2E_OUTPUT_DIR:-./.coverage-raw}"
WORKSPACE_ROOT="${COVERAGE_WORKSPACE_ROOT:-$PWD}"
STAGING_DIR=""
STRICT=0

COOP_OBJECT="${COVERAGE_COOP_OBJECT:-${WORKSPACE_ROOT}/coop/target/release/coop}"
COOPMUX_OBJECT="${COVERAGE_COOPMUX_OBJECT:-${WORKSPACE_ROOT}/coop/target/release/coopmux}"

while [[ $# -gt 0 ]]; do
  case $1 in
    --tarball)        TARBALL="$2"; shift 2 ;;
    --output-dir)     OUTPUT_DIR="$2"; shift 2 ;;
    --workspace-root) WORKSPACE_ROOT="$2"; shift 2 ;;
    --staging-dir)    STAGING_DIR="$2"; shift 2 ;;
    --coop-object)    COOP_OBJECT="$2"; shift 2 ;;
    --coopmux-object) COOPMUX_OBJECT="$2"; shift 2 ;;
    --strict)         STRICT=1; shift ;;
    -h|--help)
      sed -n '2,40p' "$0" | sed 's/^# \?//'
      exit 0
      ;;
    *) echo "ingest: unknown option: $1" >&2; exit 1 ;;
  esac
done

mkdir -p "$OUTPUT_DIR"

if [[ -z "$STAGING_DIR" ]]; then
  STAGING_DIR="$(mktemp -d)"
  trap 'rm -rf "$STAGING_DIR"' EXIT
else
  mkdir -p "$STAGING_DIR"
fi

CONTROLLER_OUT="$OUTPUT_DIR/controller-e2e.lcov"
KBEADS_OUT="$OUTPUT_DIR/kbeads-e2e.lcov"
COOP_OUT="$OUTPUT_DIR/coop-e2e.lcov"

emit_empty_outputs() {
  : > "$CONTROLLER_OUT"
  : > "$KBEADS_OUT"
  : > "$COOP_OUT"
}

# ── Resolve tarball ─────────────────────────────────────────────────
TARBALL_DIR="$(dirname "$TARBALL")"
if [[ -f "$TARBALL_DIR/SKIPPED" ]]; then
  echo "ingest: SKIPPED sentinel at $TARBALL_DIR/SKIPPED — coverage opt-in off, emitting empty LCOVs" >&2
  emit_empty_outputs
  exit 0
fi

if [[ ! -s "$TARBALL" ]]; then
  if [[ "$STRICT" -eq 1 ]]; then
    echo "ingest: tarball missing or empty ($TARBALL); --strict set, failing" >&2
    exit 1
  fi
  echo "ingest: tarball missing or empty ($TARBALL) — emitting empty LCOVs" >&2
  emit_empty_outputs
  exit 0
fi

echo "ingest: extracting $TARBALL → $STAGING_DIR" >&2
tar -C "$STAGING_DIR" -xzf "$TARBALL"

MERGED_DIR="$STAGING_DIR/.merged"
RAW_DIR="$STAGING_DIR/.raw"
mkdir -p "$MERGED_DIR" "$RAW_DIR"

# ── Tool availability ───────────────────────────────────────────────
HAVE_GCOV2LCOV=0
HAVE_LLVM_PROFDATA=0
HAVE_CARGO_LLVM_COV=0
command -v gcov2lcov     >/dev/null 2>&1 && HAVE_GCOV2LCOV=1
command -v llvm-profdata >/dev/null 2>&1 && HAVE_LLVM_PROFDATA=1
command -v cargo-llvm-cov >/dev/null 2>&1 && HAVE_CARGO_LLVM_COV=1

if [[ "$HAVE_GCOV2LCOV" -ne 1 ]]; then
  echo "ingest: gcov2lcov missing — Go LCOV emission will be skipped (install: go install github.com/jandelgado/gcov2lcov@v1.1.1)" >&2
  [[ "$STRICT" -eq 1 ]] && { echo "ingest: --strict set, failing" >&2; exit 1; }
fi

# ── Go path: per-binary covdata merge → textfmt → LCOV ──────────────
# Module ownership (see ka-0000006608 §5.2 option 5a):
CONTROLLER_BINS=(
  controller
  beads-viewer
  slack-bridge
  jira-bridge
  gitlab-bridge
  push-bridge
  trigger-engine
)
KBEADS_BINS=(kbeads)
RUST_BINS=(coop coopmux)

declare -a CONTROLLER_LCOVS=()
declare -a KBEADS_LCOVS=()

go_dir_has_data() {
  local d="$1"
  [[ -d "$d" ]] || return 1
  compgen -G "$d/covmeta.*" >/dev/null && return 0
  compgen -G "$d/covcounters.*" >/dev/null && return 0
  return 1
}

go_process_dir() {
  # $1 = input dir with covmeta.* + covcounters.*
  # $2 = label (used for intermediate filenames)
  # echoes the path to the produced LCOV file on success.
  local indir="$1" name="$2"
  go_dir_has_data "$indir" || return 1
  local merged="$MERGED_DIR/$name"
  mkdir -p "$merged"
  if ! go tool covdata merge -i="$indir" -o="$merged" 2>/dev/null; then
    echo "ingest: covdata merge failed for $name (skipping)" >&2
    return 1
  fi
  local raw_out="$RAW_DIR/$name.out"
  if ! go tool covdata textfmt -i="$merged" -o="$raw_out" 2>/dev/null; then
    echo "ingest: covdata textfmt failed for $name (skipping)" >&2
    return 1
  fi
  local lcov="$RAW_DIR/$name.lcov"
  if [[ "$HAVE_GCOV2LCOV" -eq 1 ]]; then
    if ! "$GO_TO_LCOV" "$raw_out" "$lcov" 2>/dev/null; then
      echo "ingest: go-to-lcov failed for $name (skipping)" >&2
      return 1
    fi
  else
    return 1
  fi
  printf '%s\n' "$lcov"
}

for bin in "${CONTROLLER_BINS[@]}"; do
  if path="$(go_process_dir "$STAGING_DIR/$bin" "$bin")"; then
    CONTROLLER_LCOVS+=("$path")
  fi
done
for bin in "${KBEADS_BINS[@]}"; do
  if path="$(go_process_dir "$STAGING_DIR/$bin" "$bin")"; then
    KBEADS_LCOVS+=("$path")
  fi
done

# Agent pod Go data → controller component (gb cli is in controller module)
if [[ -d "$STAGING_DIR/agents" ]]; then
  for agent_dir in "$STAGING_DIR/agents"/*/; do
    [[ -d "$agent_dir" ]] || continue
    pod="$(basename "$agent_dir")"
    if [[ -d "$agent_dir/go" ]]; then
      if path="$(go_process_dir "$agent_dir/go" "agent-${pod}-go")"; then
        CONTROLLER_LCOVS+=("$path")
      fi
    fi
  done
fi

# ── Rust path: collect profraw → llvm-profdata → cargo-llvm-cov ─────
declare -a COOP_PROFRAWS=()
collect_profraws() {
  # Append all .profraw paths under $1 to COOP_PROFRAWS.
  local root="$1"
  [[ -d "$root" ]] || return 0
  while IFS= read -r -d '' f; do
    COOP_PROFRAWS+=("$f")
  done < <(find "$root" -type f -name '*.profraw' -print0)
}

for bin in "${RUST_BINS[@]}"; do
  collect_profraws "$STAGING_DIR/$bin"
done
collect_profraws "$STAGING_DIR/agents"

COOP_LCOV=""
if [[ "${#COOP_PROFRAWS[@]}" -gt 0 ]]; then
  if [[ "$HAVE_LLVM_PROFDATA" -ne 1 || "$HAVE_CARGO_LLVM_COV" -ne 1 ]]; then
    echo "ingest: Rust tools missing (llvm-profdata=$HAVE_LLVM_PROFDATA cargo-llvm-cov=$HAVE_CARGO_LLVM_COV) — skipping coop LCOV" >&2
    [[ "$STRICT" -eq 1 ]] && { echo "ingest: --strict set, failing" >&2; exit 1; }
  else
    PROFDATA="$MERGED_DIR/coop.profdata"
    if ! llvm-profdata merge -sparse "${COOP_PROFRAWS[@]}" -o "$PROFDATA" 2>/dev/null; then
      echo "ingest: llvm-profdata merge failed (skipping coop LCOV)" >&2
    else
      report_args=(report --instr-profile="$PROFDATA" --lcov)
      if [[ -e "$COOP_OBJECT" ]]; then
        report_args+=(--object "$COOP_OBJECT")
      else
        echo "ingest: coop object not found at $COOP_OBJECT" >&2
      fi
      if [[ -e "$COOPMUX_OBJECT" ]]; then
        report_args+=(--object "$COOPMUX_OBJECT")
      else
        echo "ingest: coopmux object not found at $COOPMUX_OBJECT" >&2
      fi
      candidate="$RAW_DIR/coop.lcov"
      if cargo-llvm-cov "${report_args[@]}" --output-path "$candidate" 2>/dev/null && [[ -s "$candidate" ]]; then
        COOP_LCOV="$candidate"
      else
        echo "ingest: cargo-llvm-cov report failed (skipping coop LCOV)" >&2
      fi
    fi
  fi
fi

# ── Emit per-component LCOVs ────────────────────────────────────────
emit_component() {
  local out="$1"; shift
  if [[ "$#" -eq 0 ]]; then
    : > "$out"
    return 0
  fi
  cat "$@" > "$out"
}

emit_component "$CONTROLLER_OUT" "${CONTROLLER_LCOVS[@]+"${CONTROLLER_LCOVS[@]}"}"
emit_component "$KBEADS_OUT"     "${KBEADS_LCOVS[@]+"${KBEADS_LCOVS[@]}"}"
if [[ -n "$COOP_LCOV" && -s "$COOP_LCOV" ]]; then
  cp "$COOP_LCOV" "$COOP_OUT"
else
  : > "$COOP_OUT"
fi

# ── Per-component one-line summary ──────────────────────────────────
summarize() {
  local name="$1" file="$2"
  if [[ ! -s "$file" ]]; then
    printf 'ingest: %-10s -> %s (empty)\n' "$name" "$file"
    return
  fi
  local LH LF
  read -r LH LF <<<"$(awk '
    /^LH:/ { lh += substr($0,4)+0 }
    /^LF:/ { lf += substr($0,4)+0 }
    END    { printf "%d %d", lh, lf }
  ' "$file")"
  local pct="0.0"
  if [[ "${LF:-0}" -gt 0 ]]; then
    pct="$(awk -v lh="$LH" -v lf="$LF" 'BEGIN { printf "%.1f", (lh/lf)*100 }')"
  fi
  printf 'ingest: %-10s -> %s (lines=%s/%s, %s%%)\n' "$name" "$file" "$LH" "$LF" "$pct"
}

summarize "controller" "$CONTROLLER_OUT"
summarize "kbeads"     "$KBEADS_OUT"
summarize "coop"       "$COOP_OUT"
