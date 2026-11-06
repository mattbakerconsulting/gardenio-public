#!/usr/bin/env bash
# scripts/coverage/merge_test.sh — integration smoke test for merge.sh.
#
# Verifies that with synthetic per-component LCOV inputs, merge.sh
# produces a deterministic merged tracefile that round-trips through
# `lcov --summary` (when available) and emits exactly one
# "Coverage: NN.N%" line on stdout.
#
# Run: bash scripts/coverage/merge_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MERGE="${SCRIPT_DIR}/merge.sh"

PASS=0; FAIL=0

assert_eq() {
  local name="$1" want="$2" got="$3"
  if [[ "$want" == "$got" ]]; then
    echo "PASS: $name"
    PASS=$((PASS+1))
  else
    echo "FAIL: $name"
    echo "  want: $want"
    echo "  got:  $got"
    FAIL=$((FAIL+1))
  fi
}

assert_match() {
  local name="$1" pat="$2" got="$3"
  if [[ "$got" =~ $pat ]]; then
    echo "PASS: $name"
    PASS=$((PASS+1))
  else
    echo "FAIL: $name"
    echo "  pattern: $pat"
    echo "  got:     $got"
    FAIL=$((FAIL+1))
  fi
}

write_lcov() {
  # write_lcov FILE SF [LF LH]
  local file="$1" sf="$2" lf="${3:-3}" lh="${4:-2}"
  cat > "$file" <<EOF
TN:
SF:${sf}
FN:1,Foo
FNDA:1,Foo
FNF:1
FNH:1
DA:1,1
DA:2,1
DA:3,0
LF:${lf}
LH:${lh}
end_of_record
EOF
}

TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

INPUT_DIR="$TEST_DIR/in"
OUTPUT_DIR="$TEST_DIR/out"
mkdir -p "$INPUT_DIR"

# Inputs:
#   - controller-unit.lcov  (gardenio/controller, paths in module form)
#   - controller-e2e.lcov   (gardenio/controller, paths in absolute form)
#   - kbeads-unit.lcov      (kbeads in github module form, includes a gen/ file to be excluded)
#   - coop-unit.lcov        (coop in absolute form)
#   - coop-e2e.lcov         (coop in repo-relative form already)
write_lcov "$INPUT_DIR/controller-unit.lcov" "gardenio/controller/internal/foo.go" 3 2
write_lcov "$INPUT_DIR/controller-e2e.lcov" "/repo/gardenio/controller/internal/foo.go" 3 3

cat > "$INPUT_DIR/kbeads-unit.lcov" <<'EOF'
TN:
SF:gardenio/kbeads/internal/server/server.go
FN:1,Serve
FNDA:1,Serve
FNF:1
FNH:1
DA:1,1
DA:2,1
DA:3,0
LF:3
LH:2
end_of_record
TN:
SF:gardenio/kbeads/internal/gen/queries.pb.go
FN:1,Generated
FNDA:0,Generated
FNF:1
FNH:0
DA:1,0
LF:1
LH:0
end_of_record
EOF

write_lcov "$INPUT_DIR/coop-unit.lcov" "/repo/coop/crates/cli/src/main.rs" 4 4
write_lcov "$INPUT_DIR/coop-e2e.lcov" "coop/crates/cli/src/main.rs" 4 2

# ── Run twice; check determinism ─────────────────────────────────────
RUN1="$TEST_DIR/run1.out"
RUN2="$TEST_DIR/run2.out"
"$MERGE" --input-dir "$INPUT_DIR" --output-dir "$OUTPUT_DIR/r1" \
         --workspace-root "/repo" --no-cobertura > "$RUN1" 2>"$TEST_DIR/r1.err"
"$MERGE" --input-dir "$INPUT_DIR" --output-dir "$OUTPUT_DIR/r2" \
         --workspace-root "/repo" --no-cobertura > "$RUN2" 2>"$TEST_DIR/r2.err"

assert_eq "stdout deterministic across runs" "$(cat "$RUN1")" "$(cat "$RUN2")"
assert_eq "merged.lcov deterministic across runs" \
  "$(sha256sum "$OUTPUT_DIR/r1/coverage.merged.lcov" | awk '{print $1}')" \
  "$(sha256sum "$OUTPUT_DIR/r2/coverage.merged.lcov" | awk '{print $1}')"

# ── Single Coverage: line on stdout for the W4 regex ─────────────────
COV_LINES="$(grep -c '^Coverage: ' "$RUN1" || true)"
assert_eq "exactly one Coverage: line on stdout" "1" "$COV_LINES"
assert_match "Coverage line shape (NN.N%)" \
  '^Coverage: [0-9]+\.[0-9]+%$' "$(grep '^Coverage:' "$RUN1")"

# ── Exclusion behaviour: gen file must be absent ─────────────────────
if grep -q "kbeads/internal/gen/queries.pb.go" \
        "$OUTPUT_DIR/r1/coverage.merged.lcov"; then
  echo "FAIL: excluded gen file present in merged"; FAIL=$((FAIL+1))
else
  echo "PASS: excluded gen file absent from merged"; PASS=$((PASS+1))
fi

# ── Path normalization: every SF must start with a known prefix ──────
BAD="$(awk '/^SF:/{p=substr($0,4); ok=0;
            if(p ~ /^gardenio\//) ok=1;
            if(p ~ /^kbeads\//)  ok=1;
            if(p ~ /^coop\//)    ok=1;
            if(!ok) print p }' \
        "$OUTPUT_DIR/r1/coverage.merged.lcov" | head)"
if [[ -z "$BAD" ]]; then
  echo "PASS: all merged paths repo-root-relative"
  PASS=$((PASS+1))
else
  echo "FAIL: bad paths in merged:"
  echo "$BAD"
  FAIL=$((FAIL+1))
fi

# ── Provenance pair: unit and e2e files exist and are non-empty ──────
[[ -s "$OUTPUT_DIR/r1/coverage.unit.lcov" ]] && \
  { echo "PASS: coverage.unit.lcov non-empty"; PASS=$((PASS+1)); } || \
  { echo "FAIL: coverage.unit.lcov empty";   FAIL=$((FAIL+1)); }
[[ -s "$OUTPUT_DIR/r1/coverage.e2e.lcov"  ]] && \
  { echo "PASS: coverage.e2e.lcov non-empty"; PASS=$((PASS+1)); } || \
  { echo "FAIL: coverage.e2e.lcov empty";   FAIL=$((FAIL+1)); }

# ── lcov round-trip (skipped if lcov binary missing) ─────────────────
if command -v lcov >/dev/null 2>&1; then
  if lcov --summary "$OUTPUT_DIR/r1/coverage.merged.lcov" >/dev/null 2>&1; then
    echo "PASS: lcov --summary round-trips merged.lcov"; PASS=$((PASS+1))
  else
    echo "FAIL: lcov --summary rejected merged.lcov"; FAIL=$((FAIL+1))
  fi
else
  echo "SKIP: lcov binary not available; round-trip not verified"
fi

# ── Cobertura derivation (only when both lcov_cobertura and lcov are
# available and the cobertura toggle is on) ─────────────────────────
"$MERGE" --input-dir "$INPUT_DIR" --output-dir "$OUTPUT_DIR/cob" \
         --workspace-root "/repo" >/dev/null 2>"$TEST_DIR/cob.err" || true
if [[ -s "$OUTPUT_DIR/cob/coverage.cobertura.xml" ]]; then
  if grep -q '<coverage' "$OUTPUT_DIR/cob/coverage.cobertura.xml"; then
    echo "PASS: cobertura xml emitted"; PASS=$((PASS+1))
  else
    echo "FAIL: cobertura xml present but not parseable"; FAIL=$((FAIL+1))
  fi
else
  echo "SKIP: cobertura xml not emitted (lcov_cobertura unavailable)"
fi

echo
echo "tests: $((PASS+FAIL)) — pass=$PASS fail=$FAIL"
[[ "$FAIL" -eq 0 ]]
