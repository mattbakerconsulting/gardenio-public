#!/bin/bash
# shellcheck shell=bash
#
# Shared helper: close any leaked claude-* config beads before a test starts.
#
# Context: config-* e2e tests create open config beads with titles like
# "claude-settings", "claude-hooks", "claude-mcp", "claude-instructions" and
# global/role/project labels. If a prior test's EXIT trap didn't fire (SIGKILL,
# `set -e` unwind, close race), those beads remain visible to gb setup claude
# in the next test and contaminate resolution. The 2026.112.1 config-* failures
# (gd-zzuzGsgOma) traced to exactly this pattern — a leaked "isolated-model"
# claude-settings bead from test-config-merge-override.sh S11 leaking into
# test-config-multi-role.sh S12.
#
# Usage (in a test script):
#   source "$(dirname "$0")/lib-config-cleanup.sh"
#   ...
#   main() {
#     setup_port_forward
#     cleanup_stale_claude_configs   # after kd is reachable
#     ...
#   }
#
# Requires KD_BIN, KD_ACTOR, and a reachable beads daemon.

# cleanup_stale_claude_configs closes any open config bead whose title is one
# of the claude-* categories below AND matches one of the following conditions:
#
# 1. created_at is older than CLEANUP_MIN_AGE_SECONDS (default 120s) — catches
#    leaked beads from prior test runs (hours/days old)
# 2. OR has a role:${CLEANUP_STALE_ROLE} label — catches beads from earlier
#    scenarios within the SAME test script that used this role name
#
# The CLEANUP_STALE_ROLE filter (when set) is the primary scavenger for
# same-script inter-scenario contamination (gd-i2qgk7Zjou). Each test should
# set CLEANUP_STALE_ROLE to a unique value (e.g. derived from PID + timestamp)
# before the FIRST scenario creates beads, then call cleanup_stale_claude_configs
# after the test's final scenario completes.
#
# The age filter remains for cross-run leaks: each e2e run gets
# its own namespace, but the beads daemon backing
# those shards is shared across all of an instance's parallel shards
# (and `--all-projects` here intentionally crosses project boundaries to catch
# leaks). An unfiltered cleanup would close sibling-shard beads mid-scenario
# and cause ResolveConfigBeads to fall through to defaultUserSettings. Genuine
# leaked beads from prior runs are hours/days old, so a 120s floor catches leaks
# while leaving in-flight shards (3–10s per scenario) untouched.
cleanup_stale_claude_configs() {
  local kd_bin="${KD_BIN:-kd}"
  local kd_actor="${KD_ACTOR:-e2e-test}"
  local min_age="${CLEANUP_MIN_AGE_SECONDS:-120}"
  local cleanup_role="${CLEANUP_STALE_ROLE:-}"

  local stale_ids
  stale_ids=$(env -u BOAT_PROJECT "$kd_bin" list --type=config --status=open \
    --limit=500 --all-projects --json 2>/dev/null | MIN_AGE="$min_age" CLEANUP_ROLE="$cleanup_role" python3 -c "
import sys, json, os
from datetime import datetime, timezone
try:
    beads = json.load(sys.stdin)
except Exception:
    sys.exit(0)
min_age = int(os.environ.get('MIN_AGE', '120'))
cleanup_role = os.environ.get('CLEANUP_ROLE', '')
now = datetime.now(timezone.utc)
for b in beads:
    title = b.get('title','')
    if title not in ('claude-settings', 'claude-hooks', 'claude-mcp', 'claude-instructions'):
        continue
    created = b.get('created_at') or b.get('createdAt') or ''
    if not created:
        continue
    try:
        # Handle both 'Z' and offset suffixes
        ts = created.replace('Z', '+00:00')
        dt = datetime.fromisoformat(ts)
    except Exception:
        continue
    age = (now - dt).total_seconds()
    # Match by age (cross-run leaks) OR by role label (same-script scenarios)
    matches_by_age = age >= min_age
    matches_by_role = cleanup_role and f'role:{cleanup_role}' in b.get('labels', [])
    if matches_by_age or matches_by_role:
        print(b['id'])
" 2>/dev/null || echo "")

  if [ -z "$stale_ids" ]; then
    return 0
  fi

  local count=0
  while IFS= read -r id; do
    [ -z "$id" ] && continue
    "$kd_bin" close "$id" --actor "$kd_actor" 2>/dev/null || true
    count=$((count + 1))
  done <<< "$stale_ids"

  if [ "$count" -gt 0 ] && type dim >/dev/null 2>&1; then
    dim "  Cleaned $count stale claude-* config bead(s)"
  fi
}
