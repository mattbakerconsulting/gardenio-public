#!/usr/bin/env bash
# check-go-module-coverage_test.sh — Unit tests for check-go-module-coverage.sh
#
# Each test builds a synthetic mini-repo in a temp dir (go.work + Makefile +
# some go.mod files) and asserts the guard's exit status when pointed at it via
# REPO_ROOT, so no test depends on the real working tree.
#
# The guard's own registries (OUT_OF_WORKSPACE, IN_WORKSPACE) are compiled into
# the script, so a fixture cannot vary them. Fixtures therefore vary the things a
# contributor actually changes — the set of go.mod files, go.work, the Makefile
# and the workflows — and the tests assert the guard's reaction to each. Test 2 is the regression that matters: a
# module present in neither go.work nor the registry.
#
# The final test runs the guard against the REAL repo, so a future unregistered
# module reds immediately rather than only in synthetic fixtures.
#
# Usage:  bash scripts/check-go-module-coverage_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/check-go-module-coverage.sh"
REAL_REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# The modules the real registry knows about, and the workspace members.
REGISTERED_DIRS=(
  "gardenio/helm/gardenio/tests"
  "gardenio/tests/slackmock"
  "gardenio/grafana-plugins/gardenio-assistant-app"
  "lands/contract"
  "lands/gl"
  "scripts/heraldry-proto"
)
WORKSPACE_DIRS=(
  gardenio/clicreds
  gardenio/controller
  kbeads
  scripts/build-dictation-corpus
  scripts/wer-eval
)

# make_fixture <dir> [extra module dirs...]
#   Writes a go.work naming WORKSPACE_DIRS, a Makefile with the real
#   targets + rollup + test-go, the CI jobs IN_WORKSPACE names, and a go.mod for
#   every workspace member, every registered module, and any extra dirs passed in.
make_fixture() {
  local dir="$1"; shift
  local d
  mkdir -p "$dir"

  {
    echo "go 1.25.12"
    echo
    echo "use ("
    for d in "${WORKSPACE_DIRS[@]}"; do echo "	./${d}"; done
    echo ")"
  } > "${dir}/go.work"

  cat > "${dir}/Makefile" <<'MK'
test-out-of-workspace: test-helm test-slackmock test-grafana-plugin test-lands-contract test-lands-gl test-heraldry-proto

test-helm:
	cd gardenio/helm/gardenio/tests && GOWORK=off go test -count=1 .

test-slackmock:
	cd gardenio/tests/slackmock && GOWORK=off go test -count=1 ./...

test-grafana-plugin:
	cd gardenio/grafana-plugins/gardenio-assistant-app && GOWORK=off go test -count=1 ./...

test-lands-contract:
	cd lands/contract && GOWORK=off go test -count=1 ./...

test-lands-gl:
	cd lands/gl && GOWORK=off go test -count=1 ./...

test-heraldry-proto:
	cd scripts/heraldry-proto && GOWORK=off go test -count=1 ./...

test-go:
	$(MAKE) -C gardenio/controller test
	$(MAKE) -C kbeads test
	cd gardenio/clicreds && go test ./...
	cd scripts/build-dictation-corpus && go test ./...
	cd scripts/wer-eval && go test ./...
MK

  mkdir -p "${dir}/.github/workflows"
  cat > "${dir}/.github/workflows/ci.yml" <<'YML'
on: [push]
jobs:
  changes:
    steps:
      - uses: dorny/paths-filter@v3
        with:
          filters: |
            clicreds:
              - 'gardenio/clicreds/**'
  go:
    strategy:
      matrix:
        include: ${{ fromJSON('[{"module":"kbeads","dir":"kbeads"},{"module":"controller","dir":"gardenio/controller"},{"module":"clicreds","dir":"gardenio/clicreds"}]') }}
    steps:
      - working-directory: ${{ matrix.dir }}
        run: go test ./...
  rust:
    steps:
      - run: cargo test
YML
  cat > "${dir}/.github/workflows/checks.yml" <<'YML'
on: [push]
jobs:
  dictation-corpus:
    steps:
      - run: cd scripts/build-dictation-corpus && GOWORK=off go vet ./... && GOWORK=off go test ./...

  wer-eval:
    steps:
      - run: cd scripts/wer-eval && GOWORK=off go vet ./... && GOWORK=off go test ./...
YML

  for d in "${WORKSPACE_DIRS[@]}" "${REGISTERED_DIRS[@]}" "$@"; do
    mkdir -p "${dir}/${d}"
    echo "module ${d}" > "${dir}/${d}/go.mod"
  done
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

# assert_fails_citing <name> <fixture_dir> <grep pattern>
assert_fails_citing() {
  local name="$1" dir="$2" pat="$3"
  local out rc
  out="$(REPO_ROOT="$dir" bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == 1 ]] && grep -qF "$pat" <<<"$out"; then
    pass "$name"
  else
    fail "$name" "expected exit 1 citing '${pat}', got ${rc}. Output: ${out}"
  fi
}

# ── 1. A fully accounted-for tree passes ─────────────────────────────────
D="${TMPDIR_T}/clean"
make_fixture "$D"
assert_exit 0 "fully accounted-for tree passes" "$D"

# ── 2. THE REGRESSION (ka-0000008045): an unregistered module ────────────
# A new module in neither go.work nor the registry is invisible to a root
# `go test ./...`. This is the case the guard exists for; it MUST fail, and it
# must name the offending directory so the fix is obvious.
D="${TMPDIR_T}/unregistered"
make_fixture "$D" "gardenio/tools/newthing"
assert_fails_citing "an unregistered out-of-workspace module is caught" "$D" \
  "gardenio/tools/newthing is a Go module that is NEITHER in go.work"

# ── 2b. …including one nested under an existing workspace member ────────
# The trap's real shape: gardenio/helm/gardenio/tests sits UNDER gardenio/, so it
# looks covered. A sub-module of a workspace member is not covered either.
D="${TMPDIR_T}/nested"
make_fixture "$D" "gardenio/controller/subtool"
assert_fails_citing "a module nested under a workspace member is caught" "$D" \
  "gardenio/controller/subtool is a Go module that is NEITHER in go.work"

# ── 3. A stale registry entry is caught ─────────────────────────────────
# The module the registry claims to cover is gone; the guard must not keep
# claiming coverage for it.
D="${TMPDIR_T}/stale"
make_fixture "$D"
rm -rf "${D}/gardenio/tests/slackmock"
assert_fails_citing "a stale registry entry is caught" "$D" \
  "gardenio/tests/slackmock/go.mod does not"

# ── 4. A missing make target is caught ──────────────────────────────────
D="${TMPDIR_T}/notarget"
make_fixture "$D"
grep -v 'test-slackmock' "${D}/Makefile" > "${D}/Makefile.new"
mv "${D}/Makefile.new" "${D}/Makefile"
assert_fails_citing "a registered target missing from the Makefile is caught" "$D" \
  "Makefile does not define"

# ── 5. A target whose recipe stopped running the module is caught ───────
# The subtle rot: the target still exists, so `make test-helm` succeeds — but it
# no longer runs the suite. Byte-for-byte the shape of the original bug.
D="${TMPDIR_T}/wrongrecipe"
make_fixture "$D"
cat > "${D}/Makefile" <<'MK'
test-out-of-workspace: test-helm test-slackmock test-grafana-plugin test-lands-contract test-lands-gl test-heraldry-proto

test-helm:
	@echo "TODO: re-enable the chart suite"

test-slackmock:
	cd gardenio/tests/slackmock && GOWORK=off go test -count=1 ./...

test-grafana-plugin:
	cd gardenio/grafana-plugins/gardenio-assistant-app && GOWORK=off go test -count=1 ./...

test-lands-contract:
	cd lands/contract && GOWORK=off go test -count=1 ./...

test-lands-gl:
	cd lands/gl && GOWORK=off go test -count=1 ./...

test-heraldry-proto:
	cd scripts/heraldry-proto && GOWORK=off go test -count=1 ./...
MK
assert_fails_citing "a target that no longer runs its module is caught" "$D" \
  "does not mention 'gardenio/helm/gardenio/tests' in its recipe"

# ── 6. A target dropped from the rollup is caught ───────────────────────
D="${TMPDIR_T}/norollup"
make_fixture "$D"
sed -i 's/^test-out-of-workspace: test-helm test-slackmock test-grafana-plugin test-lands-contract test-lands-gl test-heraldry-proto$/test-out-of-workspace: test-slackmock test-grafana-plugin test-lands-contract test-lands-gl test-heraldry-proto/' "${D}/Makefile"
assert_fails_citing "a target dropped from the rollup is caught" "$D" \
  "is not a prerequisite of 'test-out-of-workspace'"

# ── 7. A missing rollup target is caught ────────────────────────────────
D="${TMPDIR_T}/rollupgone"
make_fixture "$D"
grep -v '^test-out-of-workspace:' "${D}/Makefile" > "${D}/Makefile.new"
mv "${D}/Makefile.new" "${D}/Makefile"
assert_fails_citing "a missing rollup target is caught" "$D" \
  "has no 'test-out-of-workspace' target"

# ── 8. A registered module ALSO added to go.work is caught ──────────────
# Contradictory registration: it would be covered twice and the registry's
# claim ("kept out on purpose") would be false.
D="${TMPDIR_T}/both"
make_fixture "$D"
{
  echo "go 1.25.12"
  echo
  echo "use ("
  for d in "${WORKSPACE_DIRS[@]}" gardenio/tests/slackmock; do echo "	./${d}"; done
  echo ")"
} > "${D}/go.work"
assert_fails_citing "a module in BOTH go.work and the registry is caught" "$D" \
  "is BOTH a go.work member and registered out-of-workspace"

# ── 9. The one-line `use ./dir` go.work form is understood ──────────────
# go.work accepts both forms; misparsing the one-line form would report every
# workspace member as unregistered.
D="${TMPDIR_T}/oneline"
make_fixture "$D"
{
  echo "go 1.25.12"
  for d in "${WORKSPACE_DIRS[@]}"; do echo "use ./${d}"; done
} > "${D}/go.work"
assert_exit 0 "the one-line 'use ./dir' go.work form is parsed" "$D"

# ── 10. A vendored go.mod is not treated as a repo module ───────────────
D="${TMPDIR_T}/vendored"
make_fixture "$D"
mkdir -p "${D}/gardenio/controller/vendor/example.com/lib" "${D}/gardenio/ui/node_modules/thing"
echo "module example.com/lib" > "${D}/gardenio/controller/vendor/example.com/lib/go.mod"
echo "module thing"           > "${D}/gardenio/ui/node_modules/thing/go.mod"
assert_exit 0 "vendor/ and node_modules/ go.mod files are pruned" "$D"

# ── 11. An empty tree fails loudly rather than passing vacuously ───────
D="${TMPDIR_T}/empty"
mkdir -p "$D"
echo "go 1.25.12" > "${D}/go.work"
touch "${D}/Makefile"
assert_exit 1 "a tree with no go.mod fails loudly" "$D"

# ── 12. The REAL repo is fully accounted for ───────────────────────────
assert_exit 0 "real repo: every Go module is in go.work or has a make target" "$REAL_REPO_ROOT"

# ── 13. Git-ignored modules are pruned; untracked ones are STILL caught ──
#    `find` walks the working directory, not the repo. A nested git worktree
#    (`git worktree add .worktrees/<branch>`, with `.worktrees/` gitignored) is
#    a second full checkout on disk, so before 2026-09-23 every module in the
#    repo was found again under a path in no go.work and no make target: the
#    guard exited 1 with 8 real-looking FAILs in any tree that had one, while
#    the same commit exported clean passed. The verdict depended on the
#    developer's disk, and that cost a wiring decision.
#
#    BOTH halves matter. Pruning ignored paths could just as easily have turned
#    this into a guard that cannot fail, so the untracked-but-not-ignored case
#    is the control that proves the sensitivity survived.
if command -v git >/dev/null 2>&1; then
  D="${TMPDIR_T}/gitignored"
  make_fixture "$D"
  git -C "$D" init -q
  printf '.worktrees/\n' > "${D}/.gitignore"

  # a) ignored -> pruned
  mkdir -p "${D}/.worktrees/nested/gardenio/controller"
  echo "module nested" > "${D}/.worktrees/nested/gardenio/controller/go.mod"
  assert_exit 0 "a git-ignored nested module is pruned" "$D"

  # b) untracked but NOT ignored -> still caught. Never staged, on purpose:
  #    ls-files-based pruning would pass here, and that would be the bug.
  mkdir -p "${D}/tools/newmod"
  echo "module newmod" > "${D}/tools/newmod/go.mod"
  assert_fails_citing "an untracked, non-ignored module is still caught" "$D" \
    "tools/newmod is a Go module that is NEITHER"
else
  echo "SKIP: git not on PATH; skipping the ignored-path pruning cases"
fi

# ── 14. A go.work member no CI job is registered to test is caught ──────
#    gd-9DycTnzUo3: the guard used to print "covered by a root ./... run" for
#    every go.work member, and nothing runs one, so scripts/wer-eval's tests
#    ran nowhere while this guard said OK. Joining go.work is not coverage.
D="${TMPDIR_T}/unregistered-member"
make_fixture "$D" "scripts/newtool"
sed -i 's#^)$#\t./scripts/newtool\n)#' "${D}/go.work"
printf '\tcd scripts/newtool && go test ./...\n' >> "${D}/Makefile"
assert_fails_citing "a go.work member no CI job tests is caught" "$D" \
  "scripts/newtool is a go.work member that no CI job is registered to test"

# ── 15. A registered CI job that no longer exists is caught ─────────────
D="${TMPDIR_T}/jobgone"
make_fixture "$D"
sed -i '/^  wer-eval:$/,$d' "${D}/.github/workflows/checks.yml"
assert_fails_citing "a registered CI job missing from its workflow is caught" "$D" \
  "job 'wer-eval' in checks.yml, which defines"

# ── 16. A job that names the module only in a glob or comment is caught ──
#    clicreds' exact pre-gd-nS1qtIMUcG shape: ci.yml named gardenio/clicreds
#    in paths-filter globs and comments, so it LOOKED wired, but the `go`
#    matrix never had a clicreds arm.
D="${TMPDIR_T}/globonly"
make_fixture "$D"
sed -i 's#,{"module":"clicreds","dir":"gardenio/clicreds"}##' "${D}/.github/workflows/ci.yml"
sed -i 's#^  go:$#  go:\n    \# gardenio/clicreds is shared by both arms#' "${D}/.github/workflows/ci.yml"
assert_fails_citing "a job naming the module only in a glob or comment is caught" "$D" \
  "job 'go' in ci.yml does not name 'gardenio/clicreds'"

# ── 17. A job that names the module but runs no go test is caught ──────
D="${TMPDIR_T}/lintonly"
make_fixture "$D"
sed -i 's#cd scripts/wer-eval && .*#cd scripts/wer-eval \&\& GOWORK=off golangci-lint run ./...#' \
  "${D}/.github/workflows/checks.yml"
assert_fails_citing "a registered job that runs no go test is caught" "$D" \
  "job 'wer-eval' in checks.yml runs no 'go test'"

# ── 18. A go.work member missing from test-go is caught ────────────────
D="${TMPDIR_T}/notestgo"
make_fixture "$D"
sed -i '/cd scripts\/wer-eval && go test/d' "${D}/Makefile"
assert_fails_citing "a go.work member missing from test-go is caught" "$D" \
  "make target 'test-go' does not name 'scripts/wer-eval'"

# ── 19. A registered member that left go.work is caught ────────────────
#    Its go.mod is still there, so the module is ALSO reported as in neither
#    go.work nor OUT_OF_WORKSPACE; the stale IN_WORKSPACE entry must be named.
D="${TMPDIR_T}/leftworkspace"
make_fixture "$D"
sed -i '/scripts\/wer-eval/d' "${D}/go.work"
assert_fails_citing "an IN_WORKSPACE entry that is not a go.work member is caught" "$D" \
  "IN_WORKSPACE registers 'scripts/wer-eval', but it is not a go.work member"

echo
echo "check-go-module-coverage_test: ${PASS} passed, ${FAIL} failed"
[[ "$FAIL" -eq 0 ]]
