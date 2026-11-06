#!/usr/bin/env bash
#
# govulncheck-baseline.sh — the CI / Go-Vuln reachability-diff orchestrator.
#
#   Bundle: ka-0000005053 §2.3 (security affordance). Task: ka-0000007861.
#   Pairs with: scripts/security/govulncheck-diff.py (the pure reachability-diff
#               decision function + its hermetic test govulncheck-diff_test.py).
#
# ── What it does ─────────────────────────────────────────────────────────────
# Runs `govulncheck -json ./...` on a Go module at TWO revisions and gates on the
# DIFF: it fails iff a critical/high vuln is REACHABLE on HEAD that was NOT
# reachable at the upstream-main merge-base baseline. Reachability (a call-graph
# path to the vulnerable symbol), not the raw advisory count, is the signal — the
# 2026-04-19 controller-go audit found 1 of 118 vulns reachable, so a count gate
# would be pure churn.
#
#   1. Compute the merge-base of HEAD and the target branch.
#   2. Run govulncheck at HEAD -> head report; extract reachable crit/high ids.
#   3. `git stash`/worktree the merge-base, run govulncheck there -> baseline ids.
#      (Restored afterwards; a checkout-based approach is used, not a stash, so a
#       dirty tree never blocks the scan.)
#   4. diff: NEW reachable crit/high on HEAD not in baseline -> exit 3 (gate FAIL).
#
# Fails OPEN (exit 0, WARN) on infra uncertainty (no target branch = a push/tag
# build with no diff to gate; no merge-base; govulncheck install failure) so a
# flaky scan can never red a legitimate MR — the gate only ever HARD-fails on a
# concrete NEW reachable crit/high, matching the coverage-patch precedent.
#
# ── Invocation ───────────────────────────────────────────────────────────────
#   scripts/security/govulncheck-baseline.sh <module-dir> [<target-branch>]
#
#   <module-dir>     the Go module to scan (e.g. gardenio/controller, kbeads).
#   <target-branch>  MR target branch; defaults to $TARGET_BRANCH (CI passes ""
#                    on checks.yml's advisory job). Empty -> push/tag build -> scan HEAD
#                    informationally and pass.
#
# Env knobs (for the test harness / local runs):
#   GOVULNCHECK_BIN   path to govulncheck (default: govulncheck on PATH).
#   DIFF_TOOL         path to govulncheck-diff.py (default: sibling of this file).
#   GOVULN_BASE_REF   override the baseline ref (default: merge-base w/ target).
#   SKIP_BASELINE=1   scan HEAD only, print reachable crit/high, always pass
#                     (used when no baseline revision is resolvable).

set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIFF_TOOL="${DIFF_TOOL:-$SELF_DIR/govulncheck-diff.py}"
GOVULNCHECK_BIN="${GOVULNCHECK_BIN:-govulncheck}"

log() { echo "[go-vuln] $*" >&2; }

MODULE_DIR="${1:-}"
TARGET_BRANCH="${2:-${TARGET_BRANCH:-}}"

if [ -z "$MODULE_DIR" ]; then
  log "usage: govulncheck-baseline.sh <module-dir> [<target-branch>]"
  exit 2
fi
if [ ! -d "$MODULE_DIR" ]; then
  log "module dir not found: $MODULE_DIR"
  exit 2
fi

run_govulncheck() {
  # $1 = module dir. Emits the -json report on stdout. Non-fatal: govulncheck
  # exits non-zero when it FINDS vulns, which is expected — we parse the JSON.
  # GOWORK=off scans the module standalone (the repo's go.work links controller +
  # kbeads; govulncheck rejects a -mod flag in workspace mode, and each module
  # must be scanned on its own go.mod anyway).
  ( cd "$1" && GOWORK=off "$GOVULNCHECK_BIN" -json ./... 2>/dev/null ) || true
}

WORK="$(mktemp -d)"
BASE_TREE=""
# Inline trap (not a cleanup function — a function called only via trap trips
# the SC2317 unreachable-code lint, and quench forbids suppressions). Removes
# the baseline worktree if one was created ($BASE_TREE is expanded at trap-fire
# time), then the scratch dir. worktree remove on an empty path is a no-op.
trap 'if [ -n "$BASE_TREE" ]; then git worktree remove --force "$BASE_TREE" >/dev/null 2>&1 || true; fi; rm -rf "$WORK"' EXIT

log "scanning HEAD: $MODULE_DIR"
run_govulncheck "$MODULE_DIR" > "$WORK/head.json"
if [ ! -s "$WORK/head.json" ]; then
  log "WARN: empty govulncheck output at HEAD (install/toolchain issue?) — failing OPEN."
  exit 0
fi

HEAD_REACHABLE="$WORK/head-reachable.txt"
python3 "$DIFF_TOOL" --mode reachable-ids --report "$WORK/head.json" > "$HEAD_REACHABLE" || {
  log "WARN: could not parse HEAD report — failing OPEN."
  exit 0
}
log "HEAD reachable critical/high ids: $(wc -l < "$HEAD_REACHABLE" | tr -d ' ')"

# ── Resolve the baseline revision ────────────────────────────────────────────
BASE_REF="${GOVULN_BASE_REF:-}"
if [ -z "$BASE_REF" ] && [ "${SKIP_BASELINE:-0}" != "1" ] && [ -n "$TARGET_BRANCH" ]; then
  if git rev-parse --git-dir >/dev/null 2>&1; then
    git fetch --no-tags --depth=200 origin "$TARGET_BRANCH" >/dev/null 2>&1 \
      || git fetch --no-tags origin "$TARGET_BRANCH" >/dev/null 2>&1 || true
    BASE_REF="$(git merge-base "origin/$TARGET_BRANCH" HEAD 2>/dev/null || echo '')"
  fi
fi

if [ "${SKIP_BASELINE:-0}" = "1" ] || [ -z "$BASE_REF" ]; then
  log "no baseline revision (target-branch='$TARGET_BRANCH', base_ref='$BASE_REF') — informational HEAD-only scan, passing."
  if [ -s "$HEAD_REACHABLE" ]; then
    log "reachable critical/high on HEAD (informational):"
    sed 's/^/  /' "$HEAD_REACHABLE" >&2
  fi
  exit 0
fi

# ── Scan the baseline in a detached worktree (never mutates the checkout) ─────
log "baseline merge-base: $BASE_REF"
BASE_TREE="$WORK/base-tree"
if ! git worktree add --detach "$BASE_TREE" "$BASE_REF" >/dev/null 2>&1; then
  log "WARN: could not create baseline worktree at $BASE_REF — failing OPEN."
  BASE_TREE=""
  exit 0
fi

run_govulncheck "$BASE_TREE/$MODULE_DIR" > "$WORK/base.json"
BASELINE_IDS="$WORK/baseline-reachable.txt"
if [ -s "$WORK/base.json" ]; then
  python3 "$DIFF_TOOL" --mode reachable-ids --report "$WORK/base.json" > "$BASELINE_IDS" 2>/dev/null || : > "$BASELINE_IDS"
else
  # Empty baseline scan (module didn't exist / didn't build at merge-base) — treat
  # as an empty reachable set so any HEAD-reachable crit/high is a NEW finding.
  log "WARN: empty baseline govulncheck output — treating baseline reachable set as empty."
  : > "$BASELINE_IDS"
fi
log "baseline reachable critical/high ids: $(wc -l < "$BASELINE_IDS" | tr -d ' ')"

# ── The gate: NEW reachable crit/high on HEAD not in baseline ────────────────
python3 "$DIFF_TOOL" --mode diff --baseline "$BASELINE_IDS" --head "$WORK/head.json"
rc=$?
if [ "$rc" = "3" ]; then
  log "GATE FAIL: this MR introduces NEW reachable critical/high vulnerabilities (see above)."
  exit 3
fi
if [ "$rc" != "0" ]; then
  log "WARN: diff tool errored (rc=$rc) — failing OPEN."
  exit 0
fi
log "GATE PASS: no NEW reachable critical/high vulnerabilities vs baseline."
exit 0
