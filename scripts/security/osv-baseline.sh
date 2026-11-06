#!/usr/bin/env bash
#
# osv-baseline.sh — the CI / OSV-Scan NEW-vs-baseline orchestrator.
#
#   Bundle: ka-0000005053 §2.3 (security affordance). Task: ka-0000007861.
#   Pairs with: scripts/security/osv-diff.py (pure NEW-vs-baseline decision +
#               hermetic test osv-diff_test.py).
#
# Runs `osv-scanner scan` over every bun.lock surface at HEAD and at the
# upstream-main merge-base, and gates on the DIFF: fails iff an OSV vulnerability
# id is present on HEAD that was NOT present at the baseline. The repo's bun.lock
# files already carry pre-existing advisories, so a raw-count gate would red every
# MR — NEW-vs-baseline is the "did THIS MR make it worse" signal, mirroring the
# Go-Vuln reachability gate.
#
# Fails OPEN (exit 0, WARN) on infra uncertainty (no target branch, no merge-base,
# scanner install failure) so a flaky scan never reds a legitimate MR — the gate
# only ever HARD-fails on a concrete NEW vuln id.
#
# Invocation:  osv-baseline.sh [<target-branch>]
#   <target-branch>  MR target branch; defaults to $TARGET_BRANCH. Empty -> a
#                    push/tag build with no diff to gate -> informational
#                    HEAD-only scan, always pass.
#
# Env knobs (test harness / local):
#   OSV_SCANNER_BIN   path to osv-scanner (default: osv-scanner on PATH).
#   DIFF_TOOL         path to osv-diff.py (default: sibling of this file).
#   SKIP_BASELINE=1   scan HEAD only, print ids, always pass.

set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIFF_TOOL="${DIFF_TOOL:-$SELF_DIR/osv-diff.py}"
OSV_SCANNER_BIN="${OSV_SCANNER_BIN:-osv-scanner}"

log() { echo "[osv-scan] $*" >&2; }

TARGET_BRANCH="${1:-${TARGET_BRANCH:-}}"

WORK="$(mktemp -d)"
BASE_TREE=""
# Inline trap (not a function — SC2317 fires on trap-only functions and quench
# forbids suppressions). Removes the baseline worktree if one was created, then
# the scratch dir; $BASE_TREE is expanded at trap-fire time.
trap 'if [ -n "$BASE_TREE" ]; then git worktree remove --force "$BASE_TREE" >/dev/null 2>&1 || true; fi; rm -rf "$WORK"' EXIT

# scan_tree <root> <out.json> — scan every bun.lock under <root> and merge the
# per-lockfile OSV ids into a single ids file (via the diff tool's ids mode).
scan_tree() {
  local root="$1" out="$2" lock rep
  : > "$out"
  while IFS= read -r lock; do
    rep="$WORK/rep.json"
    # osv-scanner exits 1 when it finds vulns (expected) — capture the JSON.
    "$OSV_SCANNER_BIN" scan --lockfile "$lock" --format json > "$rep" 2>/dev/null || true
    if [ -s "$rep" ]; then
      python3 "$DIFF_TOOL" --mode ids --report "$rep" >> "$out" 2>/dev/null || true
    fi
  done < <(find "$root" -name bun.lock -not -path '*/node_modules/*' -not -path '*/.git/*')
  # Dedupe.
  LC_ALL=C sort -u "$out" -o "$out"
}

log "scanning HEAD bun.lock surfaces"
HEAD_IDS="$WORK/head-ids.txt"
scan_tree "." "$HEAD_IDS"
log "HEAD OSV vuln ids: $(wc -l < "$HEAD_IDS" | tr -d ' ')"

# Resolve baseline revision.
BASE_REF=""
if [ "${SKIP_BASELINE:-0}" != "1" ] && [ -n "$TARGET_BRANCH" ] && git rev-parse --git-dir >/dev/null 2>&1; then
  git fetch --no-tags --depth=200 origin "$TARGET_BRANCH" >/dev/null 2>&1 \
    || git fetch --no-tags origin "$TARGET_BRANCH" >/dev/null 2>&1 || true
  BASE_REF="$(git merge-base "origin/$TARGET_BRANCH" HEAD 2>/dev/null || echo '')"
fi

if [ "${SKIP_BASELINE:-0}" = "1" ] || [ -z "$BASE_REF" ]; then
  log "no baseline revision (target-branch='$TARGET_BRANCH') — informational HEAD-only scan, passing."
  if [ -s "$HEAD_IDS" ]; then
    log "OSV vuln ids on HEAD (informational):"
    sed 's/^/  /' "$HEAD_IDS" >&2
  fi
  exit 0
fi

log "baseline merge-base: $BASE_REF"
BASE_TREE="$WORK/base-tree"
if ! git worktree add --detach "$BASE_TREE" "$BASE_REF" >/dev/null 2>&1; then
  log "WARN: could not create baseline worktree at $BASE_REF — failing OPEN."
  BASE_TREE=""
  exit 0
fi
BASELINE_IDS="$WORK/baseline-ids.txt"
scan_tree "$BASE_TREE" "$BASELINE_IDS"
log "baseline OSV vuln ids: $(wc -l < "$BASELINE_IDS" | tr -d ' ')"

# The gate: NEW OSV ids on HEAD not in baseline.
python3 "$DIFF_TOOL" --mode diff --baseline "$BASELINE_IDS" --head <(
  # Re-emit HEAD ids as a minimal osv report the diff tool can read: the diff
  # tool's --head expects a report, but we already have the ids — wrap them.
  python3 - "$HEAD_IDS" <<'PY'
import json, sys
ids = [l.strip() for l in open(sys.argv[1]) if l.strip()]
print(json.dumps({"results":[{"source":{"path":"head"},"packages":[
    {"package":{"name":"agg"},"vulnerabilities":[{"id":i} for i in ids]}]}]}))
PY
)
rc=$?
if [ "$rc" = "3" ]; then
  log "GATE FAIL: this MR introduces NEW OSV vulnerabilities (see above)."
  exit 3
fi
if [ "$rc" != "0" ]; then
  log "WARN: diff tool errored (rc=$rc) — failing OPEN."
  exit 0
fi
log "GATE PASS: no NEW OSV vulnerabilities vs baseline."
exit 0
