#!/usr/bin/env bash
# scripts/coverage/ingest-e2e-tarball_test.sh — fixture-based test for
# scripts/coverage/ingest-e2e-tarball.sh.
#
# Builds a tiny Go binary with `go build -cover`, runs it to produce real
# covmeta/covcounters files, packages them into a coverage tarball mirroring
# the W2 §3.2 layout, and verifies:
#
#   1. SKIPPED sentinel path emits three empty LCOVs and exits 0.
#   2. Missing tarball path emits three empty LCOVs and exits 0 (no --strict).
#   3. A real Go cover tarball produces non-empty controller-e2e.lcov and
#      kbeads-e2e.lcov when gcov2lcov is on PATH (skipped otherwise).
#   4. Per-component summary lines appear on stdout.
#   5. coop-e2e.lcov is created (may be empty when llvm tools are absent).
#
# Run: bash scripts/coverage/ingest-e2e-tarball_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INGEST="${SCRIPT_DIR}/ingest-e2e-tarball.sh"

PASS=0; FAIL=0

assert_eq() {
  local name="$1" want="$2" got="$3"
  if [[ "$want" == "$got" ]]; then
    echo "PASS: $name"; PASS=$((PASS+1))
  else
    echo "FAIL: $name"; echo "  want: $want"; echo "  got:  $got"
    FAIL=$((FAIL+1))
  fi
}

assert_match() {
  local name="$1" pat="$2" got="$3"
  if [[ "$got" =~ $pat ]]; then
    echo "PASS: $name"; PASS=$((PASS+1))
  else
    echo "FAIL: $name"; echo "  pattern: $pat"; echo "  got:     $got"
    FAIL=$((FAIL+1))
  fi
}

assert_file_exists() {
  local name="$1" path="$2"
  if [[ -e "$path" ]]; then
    echo "PASS: $name"; PASS=$((PASS+1))
  else
    echo "FAIL: $name ($path missing)"; FAIL=$((FAIL+1))
  fi
}

TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

# ── Case 1: SKIPPED sentinel ────────────────────────────────────────
SKIP_DIR="$TEST_DIR/skip"
SKIP_OUT="$TEST_DIR/skip-out"
mkdir -p "$SKIP_DIR" "$SKIP_OUT"
: > "$SKIP_DIR/SKIPPED"
# Touch a real (non-empty) tarball path; the SKIPPED sentinel in the same
# directory should short-circuit before we look at the tarball contents.
: > "$SKIP_DIR/coverage.tar.gz"
echo "junk" > "$SKIP_DIR/coverage.tar.gz"

set +e
"$INGEST" \
  --tarball     "$SKIP_DIR/coverage.tar.gz" \
  --output-dir  "$SKIP_OUT" \
  > "$TEST_DIR/skip.out" 2> "$TEST_DIR/skip.err"
EC=$?
set -e
assert_eq "SKIPPED sentinel: exit 0" 0 "$EC"
for c in controller-e2e.lcov kbeads-e2e.lcov coop-e2e.lcov; do
  assert_file_exists "SKIPPED: $c emitted" "$SKIP_OUT/$c"
  if [[ -s "$SKIP_OUT/$c" ]]; then
    echo "FAIL: SKIPPED: $c should be empty"; FAIL=$((FAIL+1))
  else
    echo "PASS: SKIPPED: $c empty"; PASS=$((PASS+1))
  fi
done

# ── Case 2: Missing tarball, no --strict ────────────────────────────
MISS_OUT="$TEST_DIR/miss-out"
mkdir -p "$MISS_OUT"
set +e
"$INGEST" \
  --tarball     "$TEST_DIR/nope/missing.tar.gz" \
  --output-dir  "$MISS_OUT" \
  > "$TEST_DIR/miss.out" 2> "$TEST_DIR/miss.err"
EC=$?
set -e
assert_eq "missing tarball: exit 0 (graceful)" 0 "$EC"
for c in controller-e2e.lcov kbeads-e2e.lcov coop-e2e.lcov; do
  assert_file_exists "missing: $c emitted" "$MISS_OUT/$c"
done

# ── Case 3: Missing tarball with --strict ──────────────────────────
set +e
"$INGEST" \
  --tarball     "$TEST_DIR/nope/missing.tar.gz" \
  --output-dir  "$TEST_DIR/strict-out" \
  --strict \
  > "$TEST_DIR/strict.out" 2> "$TEST_DIR/strict.err"
EC=$?
set -e
if [[ "$EC" -ne 0 ]]; then
  echo "PASS: --strict missing tarball: non-zero exit ($EC)"; PASS=$((PASS+1))
else
  echo "FAIL: --strict missing tarball should have failed"; FAIL=$((FAIL+1))
fi

# ── Case 4: Real fixture tarball with a Go cover binary ─────────────
# Build a tiny Go module with -cover, run it to produce covmeta/covcounters,
# then package into a tarball matching the drained-PVC layout.
FIX="$TEST_DIR/fixture"
mkdir -p "$FIX"
cat > "$FIX/go.mod" <<'EOF'
module ingestfixture

go 1.20
EOF
cat > "$FIX/main.go" <<'EOF'
package main

import "fmt"

func add(a, b int) int { return a + b }

func mul(a, b int) int {
    if a == 0 {
        return 0
    }
    return a * b
}

func main() {
    fmt.Println(add(2, 3))
    fmt.Println(mul(4, 5))
}
EOF

# We need a build per binary slot (controller, kbeads). They can be the SAME
# source — `go tool covdata merge` treats them as distinct build-ids if built
# in separate invocations, which mirrors production: each service binary has
# its own meta hash.
BIN_DIR="$FIX/bin"
mkdir -p "$BIN_DIR"
(
  cd "$FIX"
  go build -cover -covermode=atomic -coverpkg=./... -o "$BIN_DIR/controller-bin" .
  # Force a distinct build-id for the kbeads slot: build with -trimpath so we
  # get a slightly different binary, then run that one too.
  go build -cover -covermode=atomic -coverpkg=./... -trimpath -o "$BIN_DIR/kbeads-bin" .
) >/dev/null 2>&1 || {
  echo "FAIL: fixture Go build failed (cannot run E2E fixture test)" >&2
  echo "tests: $((PASS+FAIL)) — pass=$PASS fail=$FAIL"
  exit 1
}

# Stage matches W2 §3.2: <staging>/<binary>/{covmeta,covcounters}.*
STAGE="$FIX/stage"
mkdir -p "$STAGE/controller" "$STAGE/beads-viewer" "$STAGE/kbeads"
GOCOVERDIR="$STAGE/controller" "$BIN_DIR/controller-bin" >/dev/null
GOCOVERDIR="$STAGE/beads-viewer" "$BIN_DIR/controller-bin" >/dev/null
GOCOVERDIR="$STAGE/kbeads" "$BIN_DIR/kbeads-bin" >/dev/null

# Confirm covmeta/covcounters landed (sanity).
for d in controller beads-viewer kbeads; do
  if ! compgen -G "$STAGE/$d/covmeta.*" >/dev/null; then
    echo "FAIL: fixture: no covmeta.* in $STAGE/$d"; FAIL=$((FAIL+1))
  fi
done

# Add a fake Rust profraw (won't process without tools, but exercises the
# Rust collection path) under coop/ and agents/<pod>/.
mkdir -p "$STAGE/coop" "$STAGE/agents/agent-1/go" "$STAGE/agents/agent-1"
echo "fake" > "$STAGE/coop/coop-1-2.profraw"
echo "fake" > "$STAGE/agents/agent-1/coop-x.profraw"
# Also drop agent Go data (copy the controller covmeta/covcounters in).
cp "$STAGE/controller/"* "$STAGE/agents/agent-1/go/"

TARBALL="$FIX/coverage.tar.gz"
(cd "$STAGE" && tar -czf "$TARBALL" .)

REAL_OUT="$TEST_DIR/real-out"
mkdir -p "$REAL_OUT"
# gcov2lcov resolves Go package paths relative to CWD's go.mod when present;
# run the ingest from inside the fixture so the fake "ingestfixture" module
# is discoverable and LCOV records carry real DA: lines.
set +e
(
  cd "$FIX"
  "$INGEST" \
    --tarball     "$TARBALL" \
    --output-dir  "$REAL_OUT" \
    --workspace-root "$FIX"
) > "$TEST_DIR/real.out" 2> "$TEST_DIR/real.err"
EC=$?
set -e
assert_eq "real fixture: exit 0" 0 "$EC"

for c in controller-e2e.lcov kbeads-e2e.lcov coop-e2e.lcov; do
  assert_file_exists "real: $c emitted" "$REAL_OUT/$c"
done

# Summary line per component on stdout (regardless of gcov2lcov availability).
for label in controller kbeads coop; do
  if grep -q "^ingest: ${label} " "$TEST_DIR/real.out"; then
    echo "PASS: summary line for $label"; PASS=$((PASS+1))
  else
    echo "FAIL: summary line for $label missing"
    echo "--- real.out ---"; cat "$TEST_DIR/real.out"
    echo "--- real.err ---"; cat "$TEST_DIR/real.err"
    FAIL=$((FAIL+1))
  fi
done

# When gcov2lcov is on PATH, controller-e2e.lcov and kbeads-e2e.lcov must be
# non-empty (real coverage emitted). Otherwise this assertion is skipped.
if command -v gcov2lcov >/dev/null 2>&1; then
  for c in controller-e2e.lcov kbeads-e2e.lcov; do
    if [[ -s "$REAL_OUT/$c" ]]; then
      echo "PASS: real: $c non-empty (gcov2lcov available)"; PASS=$((PASS+1))
    else
      echo "FAIL: real: $c empty despite gcov2lcov on PATH"
      echo "--- real.err ---"; cat "$TEST_DIR/real.err"
      FAIL=$((FAIL+1))
    fi
  done
  # Quick LCOV shape check: SF: lines must reference our fixture module path.
  if grep -q "^SF:" "$REAL_OUT/controller-e2e.lcov"; then
    echo "PASS: controller-e2e.lcov has SF: records"; PASS=$((PASS+1))
  else
    echo "FAIL: controller-e2e.lcov missing SF: records"; FAIL=$((FAIL+1))
  fi
  # controller-e2e.lcov should be a concatenation of two binary LCOVs
  # (controller + beads-viewer), so it should have multiple end_of_record markers.
  EOR_COUNT=$(grep -c "^end_of_record" "$REAL_OUT/controller-e2e.lcov" || true)
  if [[ "$EOR_COUNT" -ge 2 ]]; then
    echo "PASS: controller-e2e.lcov has ≥2 end_of_record markers (rollup OK, got $EOR_COUNT)"
    PASS=$((PASS+1))
  else
    echo "FAIL: controller-e2e.lcov has only $EOR_COUNT end_of_record markers (expected ≥2)"
    FAIL=$((FAIL+1))
  fi
else
  echo "SKIP: gcov2lcov not on PATH — non-empty LCOV assertions skipped"
fi

echo
echo "tests: $((PASS+FAIL)) — pass=$PASS fail=$FAIL"
[[ "$FAIL" -eq 0 ]]
