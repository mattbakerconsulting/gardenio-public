#!/usr/bin/env bash
# check-go-module-coverage.sh
#
# Guard: every Go module in the repo is either a member of the root `go.work`
# workspace AND named by `make test-go` and a CI job that tests it, or is
# registered below against a `make` target that runs its tests.
#
# WHY THIS EXISTS (bug ka-0000008045): `gardenio/helm/gardenio/tests` is its own
# module (module name `gardenio/helm/tests` — it does not match its path) and is
# absent from go.work. In workspace mode `./...` resolves only against workspace
# members, so a repo-root
#
#   go test ./...
#
# reports SUCCESS having never compiled the chart/dashboard suite — where every
# per-board Grafana assertion lives. An agent ran the controller suite green,
# pushed, and CI came back red on `test-helm` with 6 failing test functions. The
# obvious recovery move misleads too: `go build ./...` from inside the module
# dir fails with "directory prefix . does not contain modules listed in
# go.work", and `GOFLAGS=-mod=mod` errors instead of working. Only
# `GOWORK=off go test ./...` from the module dir runs it.
#
# Other modules are in the same position (gardenio/tests/slackmock,
# gardenio/grafana-plugins/gardenio-assistant-app, lands/contract, lands/gl), so the durable fix is not a
# one-off note: every out-of-workspace module gets a named make target, and this
# guard reds the build the moment a NEW one appears without one. The failure
# mode being defended against is silence — a test suite nothing runs and nobody
# knows is unrun.
#
# What is checked:
#   1. Every go.mod in the tree is in go.work OR in OUT_OF_WORKSPACE below.
#   2. No module is in BOTH (a contradictory registration).
#   3. Every OUT_OF_WORKSPACE entry names a real module dir (no stale entries).
#   4. Every registered make target exists in the root Makefile.
#   5. Each target's recipe actually references its module dir — so pointing a
#      target somewhere else, or moving the module, is caught.
#   6. Every registered target is a prerequisite of the ROLLUP target, so one
#      documented command runs all of them.
#   7. Every go.work member is in IN_WORKSPACE below, mapped to the CI job that
#      tests it, and every IN_WORKSPACE entry is a real go.work member.
#   8. That job exists in that workflow, names the module dir outside comments
#      and paths-filter globs, and runs `go test`.
#   9. The LOCAL_TARGET (`test-go`) recipe names every go.work member.
#
# WHY 7-9 EXIST (gd-9DycTnzUo3): this guard used to wave every go.work member
# through as "covered by a root ./... run" -- but no CI job and no make target
# runs a repo-root `./...`. ci.yml's `go` job tests only the dirs in its matrix,
# so scripts/wer-eval's main_test.go never ran anywhere and gardenio/clicreds'
# tests ran nowhere until gd-nS1qtIMUcG gave it a matrix arm, while this guard
# printed OK for both. Joining go.work makes a module visible to a root run that
# nobody does; it is not coverage.
#
# Usage:
#   ./scripts/check-go-module-coverage.sh          # scan the real repo
#   REPO_ROOT=/tmp/fixture ./scripts/check-go-module-coverage.sh
#
# Exit status: 0 if every module is accounted for, 1 otherwise.
#
# Companion test: scripts/check-go-module-coverage_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Modules deliberately kept OUT of go.work, each mapped to the make target that
# runs its tests. Keeping them out is a real choice: the helm suite alone is
# ~5 min, and joining the workspace would put that on every root-level build and
# test. The cost of that choice is that they are invisible to `./...`, which is
# exactly what this registry + the targets pay for.
#
# Format: "<module dir relative to repo root>:<make target>"
OUT_OF_WORKSPACE=(
  "gardenio/helm/gardenio/tests:test-helm"
  "gardenio/tests/slackmock:test-slackmock"
  "gardenio/grafana-plugins/gardenio-assistant-app:test-grafana-plugin"
  # Kept out on purpose, not for cost: the Lands contract must never import
  # kbeads or the controller (gd-EWSKPH7pa5 section 3), and a workspace member
  # could do so silently. Its own deps_test.go asserts the same.
  "lands/contract:test-lands-contract"
  # Same reason: gl is the tenant's CLI and may know only the contract and the
  # shared sign-in (gd-EWSKPH7pa5 section 3); its deps_test.go asserts it.
  "lands/gl:test-lands-gl"
  # Throwaway Phase 0 heraldry renderer prototype (gd-cAQGv3TAan). Kept out so
  # its oksvg dependency never enters the workspace build.
  "scripts/heraldry-proto:test-heraldry-proto"
)

# The single target that runs every out-of-workspace module's tests.
ROLLUP="test-out-of-workspace"

# Every go.work member, mapped to the CI job that runs its tests.
#
# Format: "<module dir relative to repo root>:<file in .github/workflows>:<job id>"
IN_WORKSPACE=(
  "gardenio/clicreds:ci.yml:go"
  "gardenio/controller:ci.yml:go"
  "kbeads:ci.yml:go"
  "scripts/build-dictation-corpus:checks.yml:dictation-corpus"
  "scripts/wer-eval:checks.yml:wer-eval"
)

# The make target that runs every go.work member's tests locally (`make test`).
LOCAL_TARGET="test-go"

GO_WORK="${REPO_ROOT}/go.work"
MAKEFILE="${REPO_ROOT}/Makefile"
WORKFLOWS="${REPO_ROOT}/.github/workflows"

if [[ ! -f "$GO_WORK" ]]; then
  echo "check-go-module-coverage: missing ${GO_WORK}" >&2
  exit 1
fi
if [[ ! -f "$MAKEFILE" ]]; then
  echo "check-go-module-coverage: missing ${MAKEFILE}" >&2
  exit 1
fi

FAILURES=0

# ── go.work membership ───────────────────────────────────────────────────
# Parsed textually rather than via `go work edit -json` so the guard runs
# against synthetic fixtures with no Go toolchain and no real modules. Handles
# both the block form (`use (` … `)`) and the one-line form (`use ./dir`).
# Emits repo-relative dirs with any `./` prefix and trailing slash stripped.
workspace_dirs() {
  awk '
    /^[[:space:]]*use[[:space:]]*\(/ { inblock = 1; next }
    inblock && /^[[:space:]]*\)/     { inblock = 0; next }
    {
      line = $0
      sub(/\/\/.*/, "", line)                       # strip comments
      if (inblock) {
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", line)
        if (line != "") print line
      } else if (line ~ /^[[:space:]]*use[[:space:]]+[^(]/) {
        sub(/^[[:space:]]*use[[:space:]]+/, "", line)
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", line)
        if (line != "") print line
      }
    }
  ' "$GO_WORK" | sed -e 's#^\./##' -e 's#/$##'
}

mapfile -t WORKSPACE < <(workspace_dirs)

# ── every module in the tree ─────────────────────────────────────────────
# vendor/, node_modules/ and .git/ are pruned: a vendored go.mod is not a module
# this repo tests.
#
# Git-IGNORED paths are pruned too, and that is load-bearing rather than tidy.
# `find` walks the working directory, which is not the repo: a nested git
# worktree (`git worktree add .worktrees/<branch>`, and `.worktrees/` is in
# .gitignore) is a SECOND FULL CHECKOUT on disk, so every module in the repo is
# found a second time under a path that is in no go.work and no make target.
# Measured 2026-09-23: with one such worktree present this guard reported
#   FAIL: .worktrees/gvisor-k3s/gardenio/controller is a Go module that is ...
# eight times and exited 1, while the SAME commit exported clean
# (`git archive HEAD | tar -x`) passed with "all 8 Go module(s) accounted for".
# CI checks out a clean tree and never saw it, so the guard's verdict depended
# on whether the person running it happened to have a worktree, a nested clone
# or any other ignored Go module on disk -- and the failure names real module
# paths, so it reads as a genuine finding. That cost a wiring decision: the
# guard was recorded as "reds on main, cannot simply be wired" when it is green.
#
# `git check-ignore` rather than `git ls-files`: an ignored path is not a module
# this repo tests, but an untracked NEW module is exactly what the guard must
# still catch, and ls-files would make it invisible until staged. When REPO_ROOT
# is not a git repo -- the synthetic-fixture mode in the companion test -- there
# is nothing to ignore and every candidate is kept.
all_module_dirs() {
  local found
  found="$(find "$REPO_ROOT" \
      \( -name vendor -o -name node_modules -o -name .git \) -prune -o \
      -name go.mod -print 2>/dev/null)"
  [[ -z "$found" ]] && return 0

  if git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    # check-ignore exits 1 when NOTHING is ignored, which is the healthy case,
    # so its status is discarded. -z on both sides keeps odd paths intact.
    local ignored
    ignored="$(printf '%s\n' "$found" \
      | git -C "$REPO_ROOT" check-ignore --stdin 2>/dev/null || true)"
    if [[ -n "$ignored" ]]; then
      found="$(printf '%s\n' "$found" | grep -Fxv -f <(printf '%s\n' "$ignored") || true)"
    fi
  fi

  printf '%s\n' "$found" \
    | sed -e "s#^${REPO_ROOT}/##" -e 's#/go\.mod$##' -e 's#^go\.mod$#.#' \
    | sed '/^$/d' \
    | sort
}

mapfile -t MODULES < <(all_module_dirs)

if [[ "${#MODULES[@]}" -eq 0 ]]; then
  echo "check-go-module-coverage: found no go.mod under ${REPO_ROOT} — refusing to"
  echo "      pass vacuously. Is REPO_ROOT pointing at the right tree?"
  exit 1
fi

# registered_target <dir> — echo the make target for <dir>, empty if unregistered.
# Returns 0 either way ON PURPOSE: an unregistered dir is a normal answer, not a
# command failure, and a non-zero return here would abort the caller's
# `target="$(registered_target ...)"` assignment under `set -e`.
registered_target() {
  local dir="$1" entry
  for entry in "${OUT_OF_WORKSPACE[@]}"; do
    [[ "${entry%%:*}" == "$dir" ]] && { echo "${entry#*:}"; return 0; }
  done
  return 0
}

# ci_registration <dir> — echo "<workflow>:<job>" for <dir>, empty if
# unregistered. Returns 0 either way, for the same reason as registered_target.
ci_registration() {
  local dir="$1" entry
  for entry in "${IN_WORKSPACE[@]}"; do
    [[ "${entry%%:*}" == "$dir" ]] && { echo "${entry#*:}"; return 0; }
  done
  return 0
}

in_workspace() {
  local dir="$1" w
  for w in "${WORKSPACE[@]}"; do
    [[ "$w" == "$dir" ]] && return 0
  done
  return 1
}

# make_recipe <target> — print the recipe body of <target> from the Makefile.
# A recipe is the tab-indented run of lines following `^<target>:`; it ends at
# the first line that is neither tab-indented nor blank nor a comment.
make_recipe() {
  local target="$1"
  awk -v t="$target" '
    $0 ~ "^" t ":" { inrecipe = 1; next }
    inrecipe {
      if ($0 ~ /^\t/) { print; next }
      if ($0 ~ /^[[:space:]]*$/ || $0 ~ /^[[:space:]]*#/) next
      inrecipe = 0
    }
  ' "$MAKEFILE"
}

# workflow_job <file> <job> — print the non-comment lines of job <job> in
# workflow <file>: from `  <job>:` under `jobs:` to the next two-space-indented
# key or the next top-level key. Parsed textually for the same reason as
# go.work: the fixtures have no YAML tooling.
workflow_job() {
  local file="$1" job="$2"
  awk -v j="$job" '
    /^jobs:/                            { injobs = 1; next }
    injobs && /^[^[:space:]#]/          { injobs = 0; inblock = 0 }
    injobs && $0 ~ "^  " j ":[[:space:]]*(#.*)?$" { inblock = 1; next }
    injobs && inblock && /^  [^[:space:]#]/ { inblock = 0 }
    inblock && !/^[[:space:]]*#/        { print }
  ' "$file"
}

# names_dir <dir> — true if stdin names <dir> as a whole path: `cd <dir> &&`,
# `"dir":"<dir>"`, `-C <dir> test`. A paths-filter glob (`'<dir>/**'`) does NOT
# count -- that only decides WHEN a job runs, and before gd-nS1qtIMUcG it was
# the only place ci.yml named gardenio/clicreds.
names_dir() {
  local re
  re="$(printf '%s' "$1" | sed 's/[.]/\\./g')"
  grep -qE "(^|[^A-Za-z0-9_./-])${re}([^A-Za-z0-9_./-]|\$)"
}

# make_prereqs <target> — print the prerequisites declared on `^<target>:`.
make_prereqs() {
  local target="$1"
  sed -n "s/^${target}:[[:space:]]*//p" "$MAKEFILE" | head -1
}

# ── 1 + 2. Every module is accounted for, exactly once ───────────────────
for dir in "${MODULES[@]}"; do
  target="$(registered_target "$dir")"

  if in_workspace "$dir"; then
    if [[ -n "$target" ]]; then
      echo "FAIL: ${dir} is BOTH a go.work member and registered out-of-workspace"
      echo "      against '${target}'. Pick one: a workspace member is already"
      echo "      covered by a root-level ./... run. Drop it from"
      echo "      OUT_OF_WORKSPACE in $(basename "$0")."
      FAILURES=$((FAILURES + 1))
      continue
    fi
    ci="$(ci_registration "$dir")"
    if [[ -z "$ci" ]]; then
      echo "FAIL: ${dir} is a go.work member that no CI job is registered to test."
      echo "      Being in go.work only makes it visible to a repo-root './...',"
      echo "      and nothing runs one (gd-9DycTnzUo3). Give it a job or a matrix"
      echo "      arm that runs 'cd ${dir} && go test ./...', name it in the"
      echo "      '${LOCAL_TARGET}' recipe, and register it in IN_WORKSPACE in"
      echo "      $(basename "$0")."
      FAILURES=$((FAILURES + 1))
    else
      echo "  OK: ${dir} (go.work member — tested by ${ci%%:*} job '${ci#*:}')"
    fi
    continue
  fi

  if [[ -z "$target" ]]; then
    echo "FAIL: ${dir} is a Go module that is NEITHER in go.work NOR registered"
    echo "      against a make target. A repo-root 'go test ./...' does not"
    echo "      compile it, so its tests silently never run and a green local"
    echo "      run means nothing (bug ka-0000008045). Fix it one of two ways:"
    echo "        - add ./${dir} to go.work, if it is cheap enough to join every"
    echo "          root-level build and test; or"
    echo "        - add a 'make test-<name>' target that runs"
    echo "          'cd ${dir} && GOWORK=off go test ./...', make it a"
    echo "          prerequisite of '${ROLLUP}', and register it in"
    echo "          OUT_OF_WORKSPACE in $(basename "$0")."
    FAILURES=$((FAILURES + 1))
    continue
  fi

  echo "  OK: ${dir} (out of go.work — covered by 'make ${target}')"
done

# ── 3, 4, 5, 6. Registry entries are live and wired ──────────────────────
mapfile -t ROLLUP_PREREQS < <(make_prereqs "$ROLLUP" | tr ' ' '\n' | grep -v '^$' || true)

if [[ "${#ROLLUP_PREREQS[@]}" -eq 0 ]]; then
  echo "FAIL: the Makefile has no '${ROLLUP}' target (or it has no prerequisites)."
  echo "      That rollup is the one command an agent can run to cover every"
  echo "      module outside go.work — without it each target is discoverable"
  echo "      only by reading the Makefile."
  FAILURES=$((FAILURES + 1))
fi

for entry in "${OUT_OF_WORKSPACE[@]}"; do
  dir="${entry%%:*}"
  target="${entry#*:}"

  # 3. no stale entries
  if [[ ! -f "${REPO_ROOT}/${dir}/go.mod" ]]; then
    echo "FAIL: OUT_OF_WORKSPACE registers '${dir}', but ${dir}/go.mod does not"
    echo "      exist. If the module moved or was deleted, update the registry in"
    echo "      $(basename "$0") — a stale entry makes this guard claim coverage"
    echo "      for a module that is not there."
    FAILURES=$((FAILURES + 1))
    continue
  fi

  # 4. the target exists
  if ! grep -qE "^${target}:" "$MAKEFILE"; then
    echo "FAIL: OUT_OF_WORKSPACE maps ${dir} to make target '${target}', which the"
    echo "      Makefile does not define. 'make ${target}' would exit 2, so the"
    echo "      module's tests are unrunnable by the documented route."
    FAILURES=$((FAILURES + 1))
    continue
  fi

  # 5. the recipe actually points at that module dir
  if ! make_recipe "$target" | grep -qF "$dir"; then
    echo "FAIL: make target '${target}' does not mention '${dir}' in its recipe."
    echo "      The registry claims that target covers that module; if the recipe"
    echo "      no longer runs the module's tests, the claim is false and the"
    echo "      suite is silently unrun again."
    FAILURES=$((FAILURES + 1))
  fi

  # 6. the target is reachable from the rollup
  found=0
  for p in "${ROLLUP_PREREQS[@]}"; do
    [[ "$p" == "$target" ]] && found=1
  done
  if [[ "$found" == 0 ]]; then
    echo "FAIL: make target '${target}' is not a prerequisite of '${ROLLUP}'."
    echo "      Add it, so 'make ${ROLLUP}' really does run every module that a"
    echo "      root './...' run skips."
    FAILURES=$((FAILURES + 1))
  fi
done

# ── 7, 8, 9. go.work members are tested by CI and by test-go ────────────
LOCAL_RECIPE="$(make_recipe "$LOCAL_TARGET")"
if ! grep -qE "^${LOCAL_TARGET}:" "$MAKEFILE"; then
  echo "FAIL: the Makefile has no '${LOCAL_TARGET}' target. It is how 'make test'"
  echo "      runs the go.work members' tests locally."
  FAILURES=$((FAILURES + 1))
fi

for entry in "${IN_WORKSPACE[@]}"; do
  dir="${entry%%:*}"
  rest="${entry#*:}"
  wf="${rest%%:*}"
  job="${rest#*:}"

  # 7. no stale entries
  if ! in_workspace "$dir"; then
    echo "FAIL: IN_WORKSPACE registers '${dir}', but it is not a go.work member."
    echo "      If it left the workspace, register it in OUT_OF_WORKSPACE"
    echo "      instead; if it was deleted, drop the entry from $(basename "$0")."
    FAILURES=$((FAILURES + 1))
    continue
  fi

  # 9. test-go names it
  if [[ -n "$LOCAL_RECIPE" ]] && ! names_dir "$dir" <<<"$LOCAL_RECIPE"; then
    echo "FAIL: make target '${LOCAL_TARGET}' does not name '${dir}' in its recipe,"
    echo "      so 'make test' never runs its tests. Add"
    echo "      'cd ${dir} && go test ./...' to it."
    FAILURES=$((FAILURES + 1))
  fi

  # 8. the job exists, names the module, and tests it
  if [[ ! -f "${WORKFLOWS}/${wf}" ]]; then
    echo "FAIL: IN_WORKSPACE maps ${dir} to .github/workflows/${wf}, which does"
    echo "      not exist."
    FAILURES=$((FAILURES + 1))
    continue
  fi
  block="$(workflow_job "${WORKFLOWS}/${wf}" "$job")"
  if [[ -z "$block" ]]; then
    echo "FAIL: IN_WORKSPACE maps ${dir} to job '${job}' in ${wf}, which defines"
    echo "      no such job. Nothing in CI runs that module's tests."
    FAILURES=$((FAILURES + 1))
    continue
  fi
  if ! names_dir "$dir" <<<"$block"; then
    echo "FAIL: job '${job}' in ${wf} does not name '${dir}' outside comments"
    echo "      and paths-filter globs (as 'cd ${dir}' or a matrix"
    echo "      '\"dir\":\"${dir}\"'). The registry claims that job tests the"
    echo "      module; if it no longer does, its tests are silently unrun."
    FAILURES=$((FAILURES + 1))
  fi
  if ! grep -qE 'go test' <<<"$block"; then
    echo "FAIL: job '${job}' in ${wf} runs no 'go test', so it cannot be the"
    echo "      job that tests ${dir}."
    FAILURES=$((FAILURES + 1))
  fi
done

if [[ "$FAILURES" -gt 0 ]]; then
  echo
  echo "check-go-module-coverage: ${FAILURES} failure(s)."
  exit 1
fi

echo "check-go-module-coverage: all ${#MODULES[@]} Go module(s) accounted for" \
     "(${#WORKSPACE[@]} in go.work, tested by CI and 'make ${LOCAL_TARGET}';" \
     "${#OUT_OF_WORKSPACE[@]} covered by 'make ${ROLLUP}')."
