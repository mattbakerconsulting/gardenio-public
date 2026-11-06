#!/usr/bin/env bash
# test-hermetic-set-lint.sh — fail if any module in run-shard.sh's
# HERMETIC_MODULES has acquired a live-cluster dependency.
#
# The hermetic subset exists so `HERMETIC_ONLY=1 run-shard.sh` can run in a
# GitHub Actions runner with no cluster, no beads daemon and no port-forwards.
# That property is not enforced by anything at runtime: a module that starts
# shelling out to kubectl will simply fail, and the natural fix under time
# pressure is to drop it from the set -- which is how coverage disappears
# quietly. This lint makes the regression loud at the point it is introduced.
#
# Hermetic in this context means: no kubectl, no kd, no gb, no helm, no
# terraform, and no reference to the port-forward ports 19090/19091. A module
# MAY build and run Go, start httptest servers, bind 127.0.0.1 and use python3.
#
# Runs in <1s and needs nothing installed.
#
# NOT WIRED YET. Nothing invokes this script as of this commit: there is no
# `shell-hermetic` job in .github/workflows/e2e.yml (that string appears only
# as a value in the ALL matrix list), and `HERMETIC_ONLY` is referenced by no
# workflow. T6 of the e2e-optional-ci-jobs plan adds the job that runs it, as
# an explicit `run:` step before run-shard.sh.
#
# Stated explicitly because a guard that documents itself as wired is exactly
# how this repo's two dead guards survived (gd-pqTjuJANoq, and the
# internal-strings audit removed in d52a305c): you grep, you find the guard,
# you read its header, and you believe it runs. If you are reading this after
# T6 landed, delete this block rather than leaving it stale.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_SHARD="${SCRIPT_DIR}/run-shard.sh"

if [ ! -f "$RUN_SHARD" ]; then
  echo "FAIL: run-shard.sh not found at $RUN_SHARD" >&2
  exit 1
fi

# The forbidden tools, as word-boundary regexes. `go` is deliberately absent.
FORBIDDEN='(^|[^[:alnum:]_-])(kubectl|helm|terraform)([^[:alnum:]_-]|$)|\$\{?(KD_BIN|GB_BIN)\b|(^|[^[:alnum:]_.-])(kd|gb)[[:space:]]|localhost:1909[01]'

# `|| true` is load-bearing, on this and the run_module lookup below. This
# script sets `pipefail`, so a failing grep makes the whole pipeline exit 1,
# and `set -e` then aborts at the ASSIGNMENT -- before the guard that explains
# what went wrong. Without it the missing-variable path below is dead code and
# the lint exits 1 silently, which is the opposite of "make the regression
# loud".
HERMETIC_MODULES="$(
  grep -m1 '^HERMETIC_MODULES=' "$RUN_SHARD" \
    | sed 's/^HERMETIC_MODULES="//; s/"$//' || true
)"

if [ -z "$HERMETIC_MODULES" ]; then
  echo "FAIL: could not read HERMETIC_MODULES from $RUN_SHARD" >&2
  exit 1
fi

rc=0
count=0
IFS=',' read -ra MODULES <<< "$HERMETIC_MODULES"
for mod in "${MODULES[@]}"; do
  mod="$(echo "$mod" | xargs)"
  [ -n "$mod" ] || continue
  count=$((count + 1))

  # Resolve the module name to its script via the registry line in run-shard.sh.
  # `|| true` for the same reason as above: without it a typo'd or renamed
  # module aborts the lint here, so the "has no run_module entry" message
  # never prints AND every module after it in the list goes unchecked.
  script_rel="$(
    grep -m1 "run_module \"${mod}\" " "$RUN_SHARD" \
      | sed 's/.*run_module "[^"]*" "\([^"]*\)".*/\1/' || true
  )"
  if [ -z "$script_rel" ]; then
    echo "FAIL: ${mod} is in HERMETIC_MODULES but has no run_module entry" >&2
    rc=1
    continue
  fi

  script_abs="${SCRIPT_DIR}/../../../../${script_rel#./}"
  if [ ! -f "$script_abs" ]; then
    echo "FAIL: ${mod} -> ${script_rel} does not exist" >&2
    rc=1
    continue
  fi

  # Strip comments and blank lines before matching: a module may legitimately
  # *mention* kubectl in a rationale comment (several do).
  hits="$(sed 's/#.*//' "$script_abs" | grep -nE "$FORBIDDEN" || true)"
  if [ -n "$hits" ]; then
    echo "FAIL: ${mod} (${script_rel}) is no longer hermetic:" >&2
    echo "$hits" | head -5 | sed 's/^/    /' >&2
    rc=1
  fi
done

if [ "$count" -eq 0 ]; then
  echo "FAIL: HERMETIC_MODULES parsed to zero modules" >&2
  exit 1
fi

if [ "$rc" -eq 0 ]; then
  echo "PASS: all ${count} modules in HERMETIC_MODULES are hermetic"
else
  echo "" >&2
  echo "A module in HERMETIC_MODULES acquired a cluster dependency." >&2
  echo "Fix the module, or remove it from HERMETIC_MODULES *and* say so on a" >&2
  echo "bead -- silently shrinking the set is the failure this lint exists" >&2
  echo "to prevent." >&2
fi
exit "$rc"
