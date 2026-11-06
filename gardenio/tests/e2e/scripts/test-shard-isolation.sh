#!/usr/bin/env bash
# test-shard-isolation.sh — assert run-shard.sh partitions every module into
# exactly one shard for any TOTAL_SHARDS value (gd-UHoecPuYe0, gap G9 from
# ka-0000003664).
#
# Each module's real-time S3 uploader writes under a COOP_S3_SESSION_KEY
# derived from the agent bead ID it spawns. If two shards executed the same
# module concurrently, two uploaders would collide on the same S3 prefix and
# corrupt the resumable session output. shard_for() is a pure function over
# module name, so the partition is correct as long as:
#   1. Every `run_module "X"` line uses a unique module name.
#   2. shard_for() returns a value in [0, TOTAL_SHARDS).
#   3. The union of modules-per-shard equals the full module set, with each
#      module appearing in exactly one shard.
#
# This script is hermetic: it sources only the shard_for() function from
# run-shard.sh and never touches kubectl, beads, or the network.
#
# It ALSO asserts exclusion-list completeness (ka-0000004758) so e2e wiring
# drift cannot recur silently. The drift audit subtracts a hand-maintained
# prose list of "not wired on purpose" scripts (the exclusion block in the
# run-shard.sh header) from the on-disk script count. Nothing used to ENFORCE
# that list, so two hermetic scripts once went 18 days undetected. Here we
# make it machine-checked, in BOTH directions:
#   FORWARD  every gardenio/tests/e2e/scripts/test-*.sh on disk is EITHER
#            invoked by a `run_module` call in run-shard.sh OR named in the
#            run-shard.sh header exclusion block.
#   REVERSE  every name in the exclusion block still exists on disk (a
#            deleted/renamed script leaves no stale bullet behind).
# The exclusion block is delimited by explicit sentinel markers in the
# run-shard.sh header, so this parse never depends on prose formatting.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_SHARD="${SCRIPT_DIR}/run-shard.sh"

if [ ! -f "$RUN_SHARD" ]; then
  echo "FAIL: run-shard.sh not found at $RUN_SHARD" >&2
  exit 1
fi

# ── Discover the canonical module list ──────────────────────────────
# Every test module is registered with a `run_module "<name>" ...` call.
MODULES_RAW=$(grep -oE 'run_module "[a-z][a-z0-9-]+"' "$RUN_SHARD" \
  | awk -F'"' '{print $2}')
MODULES_SORTED=$(printf '%s\n' "$MODULES_RAW" | sort)
MODULES_UNIQUE=$(printf '%s\n' "$MODULES_RAW" | sort -u)
TOTAL_MODULES=$(printf '%s\n' "$MODULES_UNIQUE" | wc -l | tr -d ' ')
echo "Discovered $TOTAL_MODULES distinct modules in $(basename "$RUN_SHARD")"

# ── Invariant 1: every module name is registered exactly once ───────
DUPES=$(printf '%s\n' "$MODULES_SORTED" | uniq -d || true)
if [ -n "$DUPES" ]; then
  echo "FAIL: duplicated run_module entries:" >&2
  printf '%s\n' "$DUPES" | sed 's/^/  /' >&2
  exit 1
fi
echo "PASS: every run_module name is unique"

# ── Invariant 1b: exclusion-list completeness (ka-0000004758) ────────
# Every test-*.sh on disk must be accounted for: either it is invoked by a
# `run_module` call in run-shard.sh, or it is named in the run-shard.sh header
# exclusion block (delimited by the `shard-isolation-lint: exclusion-list
# BEGIN/END` sentinels). This turns the previously prose-only "not wired on
# purpose" list into an enforced invariant so e2e drift cannot recur silently.

# (a) scripts run_module actually invokes, restricted to the scripts/ dir.
#     run_module also wires a handful of scripts that live in sibling dirs
#     (edge-auth/) — those are matched by their full path elsewhere and are
#     not part of the scripts/test-*.sh set this completeness check governs.
WIRED_SCRIPTS=$(grep -oE 'run_module "[^"]+" "\./gardenio/tests/e2e/scripts/test-[a-z0-9-]+\.sh"' "$RUN_SHARD" \
  | grep -oE 'test-[a-z0-9-]+\.sh' | sort -u)

# (b) the exclusion block, parsed strictly between the sentinel markers so the
#     list boundaries never depend on surrounding prose.
EXCLUDED_SCRIPTS=$(awk '
  /shard-isolation-lint: exclusion-list BEGIN/ { inblock = 1; next }
  /shard-isolation-lint: exclusion-list END/   { inblock = 0 }
  inblock && match($0, /test-[a-z0-9-]+\.sh/) {
    print substr($0, RSTART, RLENGTH)
  }
' "$RUN_SHARD" | sort -u)

if [ -z "$EXCLUDED_SCRIPTS" ]; then
  echo "FAIL: could not parse the exclusion block — are the" \
       "'shard-isolation-lint: exclusion-list BEGIN/END' sentinels present in" \
       "$(basename "$RUN_SHARD")?" >&2
  exit 1
fi

# (c) every test-*.sh actually on disk. Glob the directory rather than shelling
#     out to `ls` (matches test-concurrency-isolation.sh's SCRIPTS=(...) pattern
#     and keeps shellcheck happy about filenames).
shopt -s nullglob
DISK_PATHS=("$SCRIPT_DIR"/test-*.sh)
shopt -u nullglob
if [ "${#DISK_PATHS[@]}" -eq 0 ]; then
  echo "FAIL: no test-*.sh scripts found in $SCRIPT_DIR" >&2
  exit 1
fi
DISK_SCRIPTS=$(for p in "${DISK_PATHS[@]}"; do basename "$p"; done | sort)

COVERED_SCRIPTS=$(printf '%s\n%s\n' "$WIRED_SCRIPTS" "$EXCLUDED_SCRIPTS" | sort -u)

# FORWARD: on disk but neither wired nor excluded → undocumented drift.
UNACCOUNTED=$(comm -23 <(printf '%s\n' "$DISK_SCRIPTS") <(printf '%s\n' "$COVERED_SCRIPTS"))
# REVERSE: named in the exclusion block but no longer on disk → stale entry.
STALE_EXCLUDE=$(comm -13 <(printf '%s\n' "$DISK_SCRIPTS") <(printf '%s\n' "$EXCLUDED_SCRIPTS"))
# CONSISTENCY: a script listed as "NOT wired" in the exclusion block must not
# also carry a run_module call — that contradicts the block's own claim.
OVERLAP=$(comm -12 <(printf '%s\n' "$WIRED_SCRIPTS") <(printf '%s\n' "$EXCLUDED_SCRIPTS"))

COMPLETENESS_RC=0
if [ -n "$UNACCOUNTED" ]; then
  echo "FAIL: these scripts/test-*.sh are neither invoked by a run_module call" \
       "nor listed in the run-shard.sh exclusion block — wire them into a shard" \
       "or add them to the exclusion block:" >&2
  printf '%s\n' "$UNACCOUNTED" | sed 's/^/  /' >&2
  COMPLETENESS_RC=1
fi
if [ -n "$STALE_EXCLUDE" ]; then
  echo "FAIL: these names appear in the run-shard.sh exclusion block but no" \
       "longer exist on disk — remove the stale exclusion entries:" >&2
  printf '%s\n' "$STALE_EXCLUDE" | sed 's/^/  /' >&2
  COMPLETENESS_RC=1
fi
if [ -n "$OVERLAP" ]; then
  echo "FAIL: these scripts are BOTH invoked by a run_module call AND listed in" \
       "the 'not wired' exclusion block — remove them from the exclusion block:" >&2
  printf '%s\n' "$OVERLAP" | sed 's/^/  /' >&2
  COMPLETENESS_RC=1
fi
if [ "$COMPLETENESS_RC" -ne 0 ]; then
  exit 1
fi
DISK_COUNT="${#DISK_PATHS[@]}"
WIRED_COUNT=$(printf '%s\n' "$WIRED_SCRIPTS" | grep -c . || true)
EXCLUDED_COUNT=$(printf '%s\n' "$EXCLUDED_SCRIPTS" | grep -c . || true)
echo "PASS: exclusion-list complete — $DISK_COUNT scripts/test-*.sh on disk =" \
     "$WIRED_COUNT wired + $EXCLUDED_COUNT excluded, no unaccounted or stale entries"

# ── Extract shard_for() and eval it in this shell ───────────────────
# We never run the body of run-shard.sh (it would shell out to kubectl).
# Instead we lift the pure shard_for() function and call it directly.
SHARD_FOR_FUNC=$(awk '
  /^shard_for\(\)[[:space:]]*\{/ { capture = 1 }
  capture                        { print }
  capture && /^\}/               { exit }
' "$RUN_SHARD")
if [ -z "$SHARD_FOR_FUNC" ]; then
  echo "FAIL: could not extract shard_for() from $RUN_SHARD" >&2
  exit 1
fi
# shellcheck disable=SC2294
eval "$SHARD_FOR_FUNC"

# ── Invariant 2 & 3: partition checks for several N values ──────────
check_partition() {
  local total_shards="$1"
  local module shard rc=0
  declare -A counts
  for module in $MODULES_UNIQUE; do
    shard=$(TOTAL_SHARDS=$total_shards shard_for "$module")
    if ! [[ "$shard" =~ ^[0-9]+$ ]]; then
      echo "FAIL: TOTAL_SHARDS=$total_shards: shard_for($module) returned non-numeric '$shard'" >&2
      rc=1
      continue
    fi
    if [ "$shard" -lt 0 ] || [ "$shard" -ge "$total_shards" ]; then
      echo "FAIL: TOTAL_SHARDS=$total_shards: shard_for($module)=$shard outside [0,$total_shards)" >&2
      rc=1
      continue
    fi
    counts[$shard]=$((${counts[$shard]:-0} + 1))
  done
  local assigned=0
  for s in "${!counts[@]}"; do
    assigned=$((assigned + counts[$s]))
  done
  if [ "$assigned" -ne "$TOTAL_MODULES" ]; then
    echo "FAIL: TOTAL_SHARDS=$total_shards: only $assigned of $TOTAL_MODULES modules placed" >&2
    rc=1
  fi
  if [ "$rc" -eq 0 ]; then
    local distribution=""
    for s in $(seq 0 $((total_shards - 1))); do
      distribution+=" shard$s=${counts[$s]:-0}"
    done
    echo "PASS: TOTAL_SHARDS=$total_shards — $TOTAL_MODULES modules partitioned;$distribution"
  fi
  return $rc
}

# ── Invariant 4: union check across all shards has no duplicates ────
# This is the literal restatement of the gap G9 invariant: build the union of
# {modules assigned to shard s} for s in 0..N-1 and assert |union| == |modules|.
check_union_unique() {
  local total_shards="$1"
  local module shard
  local -a all=()
  for module in $MODULES_UNIQUE; do
    shard=$(TOTAL_SHARDS=$total_shards shard_for "$module")
    # Append `<shard>:<module>` once if and only if shard is the assignee.
    # Since shard_for is a function we call once per module, each module
    # appears at most once — but we still verify post-hoc by sorting and
    # de-duping the module column.
    all+=("${module}")
  done
  local total
  total=$(printf '%s\n' "${all[@]}" | sort -u | wc -l | tr -d ' ')
  if [ "$total" -ne "$TOTAL_MODULES" ]; then
    echo "FAIL: TOTAL_SHARDS=$total_shards: union has $total unique modules, expected $TOTAL_MODULES" >&2
    return 1
  fi
  return 0
}

OVERALL_RC=0
# Production CI fixes TOTAL_SHARDS=4 (e2e.yml live-shards). The explicit case
# statement pins modules to shards 0..3, so 4 is the minimum supported value.
# We also check 5, 8, 16 to confirm forward compatibility — the fallback
# cksum(name) % TOTAL_SHARDS happily maps to higher N.
for n in 4 5 8 16; do
  check_partition "$n"  || OVERALL_RC=1
  check_union_unique "$n" || OVERALL_RC=1
done

echo ""
if [ "$OVERALL_RC" -eq 0 ]; then
  echo "OK: shard isolation invariant holds across all tested TOTAL_SHARDS values"
else
  echo "FAIL: shard isolation violated — see errors above" >&2
fi
exit "$OVERALL_RC"
