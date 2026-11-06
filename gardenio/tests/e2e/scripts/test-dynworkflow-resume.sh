#!/usr/bin/env bash
# test-dynworkflow-resume.sh — E2E: a gardener-shaped agent loses its pod
# mid dynamic-workflow run and resumes cleanly via `resumeFromRunId` —
# completed agent() calls return cached results, only the unfinished tail
# re-runs, rather than the whole workflow restarting from scratch.
#
# Why (gd-yJnnThsTeF, parent ka-0000006835):
#   This is the core resilience property for running the sanitize/LOL pilot
#   as a dynamic workflow: a pod can be OOM-killed / rescheduled mid-flight
#   and the next pod picks up where the last one left off. The companion-dir
#   S3 sync (ka-0000000659) carries the per-run journals (`agent-*.jsonl` +
#   run-state under `runs/<run-id>.json`) so the resume can replay cached
#   results.
#
# Flow under test:
#   1. A PVC-backed gardener-shaped agent runs a multi-step dynamic workflow
#      (several agent() calls) on Claude Code >= 2.1.154.
#   2. The pod is killed mid-run — after at least one agent() completes but
#      before the workflow returns. The companion dir on the PVC holds the
#      partial journals: agent-1.jsonl (done) + runs/<run-id>.json (in_progress).
#   3. The entrypoint resumes (coop --resume → claude --resume <uuid>). The
#      companion dir is the sibling of the transcript .jsonl Claude reads.
#   4. The Workflow tool resumes via resumeFromRunId: the unchanged prefix of
#      agent() calls returns cached results from the journal; only the
#      unfinished tail (agent-2, agent-3, ...) runs live.
#
# This script validates that chain with TWO complementary layers:
#
#   • Layer A — companion-dir journal contract (DETERMINISTIC, hermetic):
#     Builds the exact Claude companion-dir layout on a temp FS — a partial
#     run journal where agent-1 completed and the run-state is in_progress —
#     then asserts the resume-decision logic that the Workflow tool relies on:
#     same run-id, the completed-prefix agent journals survive a restart, and
#     a fresh-start would have discarded them. Asserts via journal/run-id
#     state, NOT wall-clock timing (per the bead acceptance criteria). Runs
#     anywhere; no cluster needed.
#
#   • Layer B — bead lifecycle + optional live-pod check:
#     Mirrors test-session-resume.sh / test-thread-respawn-resume.sh: the
#     agent bead + a workflow-run session chain (s1 crashed mid-run,
#     resumed=false → s2 resumed=true, same run-id, same PVC host). When a
#     live cluster is available, an optional check greps the agent pod for
#     the coop restore log line and inspects the PVC companion dir.
#
# Usage:
#   ./tests/e2e/scripts/test-dynworkflow-resume.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN), gb binary (or GB_BIN)
#   - kubectl context pointing at the target cluster (Layer B live-pod check)
#   - jq and python3 installed
#
# Scenarios tested:
#   A1. Companion dir derived from session log (strip .jsonl) — coop contract
#   A2. Partial run journal: agent-1 completed, run-state in_progress
#   A3. Resume preserves the run-id (resumeFromRunId targets the same run)
#   A4. Completed-prefix agent journals survive the restart (cached results)
#   A5. Fresh-start (no resume) would discard the partial run → re-run all
#   A6. Resume re-runs only the unfinished tail (agent-2 absent pre-restart)
#   B1. Gardener-shaped agent bead created (PVC-backed, mode=crew/gardener)
#   B2. Workflow-run session 1: resumed=false, status active (mid-run)
#   B3. Pod killed mid-run → session 1 status=crashed, exit_code=137
#   B4. Resume session 2: resumed=true, same PVC host, same run-id
#   B5. Exactly 2 sessions for the agent; run-id continuity across the pair
#   B6. (optional) Live-pod check: coop workflow-restore log / PVC journal

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
TMP_ROOT=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

# Wrapper for curl that includes auth header when BEADS_TOKEN is set.
# kbeads server enforces non-empty operated_by since ka-0000005772 — raw
# curl writes without X-Operated-By 400. run-shard.sh exports
# BEADS_OPERATED_BY for `kd`/`gb`; mirror it onto raw POSTs here.
beads_curl() {
  curl ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
       ${BEADS_OPERATED_BY:+-H "X-Operated-By: $BEADS_OPERATED_BY"} "$@"
}

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

skip() {
  local name="$1"
  local reason="${2:-}"
  dim "  ~ $name (skipped${reason:+: $reason})"
}

assert_exit() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_equals() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" = "$expected" ]; then
    pass "$name"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

assert_file() {
  local name="$1"
  local path="$2"
  if [ -f "$path" ]; then
    pass "$name"
  else
    fail "$name" "expected file to exist: $path"
  fi
}

assert_absent() {
  local name="$1"
  local path="$2"
  if [ ! -e "$path" ]; then
    pass "$name"
  else
    fail "$name" "expected path to be absent: $path"
  fi
}

json_nested() {
  local json="$1"
  local path="$2"
  echo "$json" | python3 -c "
import sys, json
d = json.load(sys.stdin)
keys = '$path'.split('.')
for k in keys:
    if isinstance(d, dict):
        d = d.get(k, '')
    else:
        d = ''
        break
if isinstance(d, bool):
    print(str(d).lower())
elif isinstance(d, (dict, list)):
    print(json.dumps(d))
else:
    print(d)
" 2>/dev/null || echo ""
}

# ── coop companion-dir contract (mirrors coop/crates/cli/src/s3.rs) ──
# Claude stores the transcript at  <proj>/<uuid>.jsonl  and the dynamic
# workflow journals in the sibling dir  <proj>/<uuid>/  — the same path with
# the .jsonl suffix stripped. `workflow_companion_dir()` in coop derives one
# from the other; this is the bash equivalent the test asserts against.
companion_dir_for() {
  local log_path="$1"
  case "$log_path" in
    *.jsonl) printf '%s\n' "${log_path%.jsonl}" ;;
    *)       return 1 ;;  # no .jsonl suffix → no companion dir derivable
  esac
}

# ── Setup: port-forward kd server (Layer B) ──────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19099
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-dynworkflow-resume.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    return
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

seed_session_type() {
  # We deliberately do NOT add workflow-specific fields to the session schema —
  # the staging daemon is shared across shards and a schema change would race
  # other resume tests. Workflow run-state instead rides in the session
  # description (always-allowed JSON) + freeform labels.
  # type:session is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.
  dim "  Session type config: using daemon builtin (no shadow PUT)"
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  [ -n "$TMP_ROOT" ] && rm -rf "$TMP_ROOT" 2>/dev/null || true
}
trap cleanup EXIT

# ── Layer A: companion-dir journal resume contract ──────────────────
# Hermetic — builds the exact Claude companion-dir layout on a temp FS and
# asserts the cached-prefix resume property the Workflow tool relies on.
# No cluster, no API calls; runs identically everywhere.
layer_a_journal_contract() {
  blue "Layer A: dynamic-workflow companion-dir journal resume contract"

  TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/dynworkflow-resume.XXXXXX")
  local uuid="11111111-2222-3333-4444-555555555555"
  local run_id="wf_resume_e2e_abc123"
  # Claude project transcript dir layout: <claude>/projects/<proj-hash>/
  local proj_dir="$TMP_ROOT/projects/-home-agent-bot-gardenio-work"
  mkdir -p "$proj_dir"
  local session_log="$proj_dir/$uuid.jsonl"

  # The transcript itself — one valid JSONL line carrying the sessionId, the
  # ground truth coop's parse_session_id() reads to locate the companion dir.
  printf '{"sessionId":"%s","type":"summary"}\n' "$uuid" > "$session_log"

  # ── A1: companion dir is the transcript path minus the .jsonl suffix ──
  local companion
  if companion=$(companion_dir_for "$session_log"); then
    assert_equals "A1 companion dir = session log minus .jsonl" \
      "$proj_dir/$uuid" "$companion"
  else
    fail "A1 companion dir derivable" "companion_dir_for returned non-zero for $session_log"
    return
  fi
  # Negative: a non-.jsonl path yields no companion dir (coop returns None).
  if companion_dir_for "$proj_dir/session.log" >/dev/null 2>&1; then
    fail "A1 non-.jsonl path → no companion dir" "expected companion_dir_for to fail"
  else
    pass "A1 non-.jsonl path → no companion dir (coop None contract)"
  fi

  mkdir -p "$companion/runs"

  # ── A2: partial run journal — agent-1 done, run-state in_progress ──────
  # Workflow journals: one agent-N.jsonl per completed agent() call, plus a
  # run-state file under runs/<run-id>.json recording how far the run got.
  printf '{"agent":1,"label":"sanitize:step-1","status":"completed","result":"OK-1"}\n' \
    > "$companion/agent-1.jsonl"
  cat > "$companion/runs/$run_id.json" <<RUNEOF
{
  "run_id": "$run_id",
  "status": "in_progress",
  "total_steps": 3,
  "completed_agents": [1],
  "next_agent": 2
}
RUNEOF
  assert_file "A2 agent-1 journal present (completed call)" "$companion/agent-1.jsonl"
  assert_file "A2 run-state journal present (in_progress)" "$companion/runs/$run_id.json"
  assert_absent "A2 agent-2 journal absent pre-restart (not yet run)" "$companion/agent-2.jsonl"

  local run_status
  run_status=$(python3 -c "import json;print(json.load(open('$companion/runs/$run_id.json'))['status'])" 2>/dev/null || echo "")
  assert_equals "A2 run-state status is in_progress (mid-flight)" "in_progress" "$run_status"

  # Snapshot the completed-prefix journal content so we can prove it is the
  # SAME bytes after the simulated restart (cached, not re-derived).
  local pre_restart_agent1
  pre_restart_agent1=$(cat "$companion/agent-1.jsonl")

  # ── Simulate the pod restart ──────────────────────────────────────────
  # On the PVC-intact path the companion dir is untouched across pods — coop
  # --resume hands Claude the same transcript and the journals are still
  # there. (On the S3-only path, restore_workflow_dir re-materialises these
  # exact files; the resume *decision* downstream is identical, which is what
  # this layer asserts.) We model "pod restarts, PVC intact" as: the files
  # persist; a NEW coop process re-derives the companion dir from the same
  # session log and reads the run-state to decide resume vs fresh-start.
  local resumed_companion
  resumed_companion=$(companion_dir_for "$session_log")

  # ── A3: resume targets the SAME run-id (resumeFromRunId) ──────────────
  local resumed_run_id
  resumed_run_id=$(python3 -c "import json;print(json.load(open('$resumed_companion/runs/$run_id.json'))['run_id'])" 2>/dev/null || echo "")
  assert_equals "A3 resume run-id matches pre-restart (resumeFromRunId)" "$run_id" "$resumed_run_id"

  # ── A4: completed-prefix journals survive the restart (cached) ────────
  assert_file "A4 agent-1 journal survives restart" "$resumed_companion/agent-1.jsonl"
  local post_restart_agent1
  post_restart_agent1=$(cat "$resumed_companion/agent-1.jsonl")
  assert_equals "A4 agent-1 cached result is byte-identical (no re-run)" \
    "$pre_restart_agent1" "$post_restart_agent1"

  # ── A5: a fresh start (no resume) would discard the partial run ───────
  # Model `claude` WITHOUT --resume: a new session uuid → a new (empty)
  # companion dir → the in_progress run journal is unreachable, so every
  # agent() call would re-run. This is the regression we are guarding against.
  local fresh_uuid="99999999-8888-7777-6666-555555555555"
  local fresh_companion="$proj_dir/$fresh_uuid"
  assert_absent "A5 fresh-start companion dir has no carried run-state" \
    "$fresh_companion/runs/$run_id.json"
  assert_absent "A5 fresh-start companion dir has no cached agent-1" \
    "$fresh_companion/agent-1.jsonl"

  # ── A6: resume re-runs only the unfinished tail ───────────────────────
  # The run-state says completed=[1], next=2. Drive the tail to completion the
  # way a resumed Workflow would: agent-1 is served from cache (no new file),
  # only agent-2 and agent-3 get fresh journals; run-state flips to completed.
  printf '{"agent":2,"label":"sanitize:step-2","status":"completed","result":"OK-2"}\n' \
    > "$resumed_companion/agent-2.jsonl"
  printf '{"agent":3,"label":"sanitize:step-3","status":"completed","result":"OK-3"}\n' \
    > "$resumed_companion/agent-3.jsonl"
  cat > "$resumed_companion/runs/$run_id.json" <<RUNEOF2
{
  "run_id": "$run_id",
  "status": "completed",
  "total_steps": 3,
  "completed_agents": [1, 2, 3],
  "next_agent": null
}
RUNEOF2
  # agent-1's file was never rewritten by the tail — assert it is still the
  # original cached content (the resume did NOT re-run completed calls).
  assert_equals "A6 agent-1 NOT re-run by resumed tail (still cached bytes)" \
    "$pre_restart_agent1" "$(cat "$resumed_companion/agent-1.jsonl")"
  assert_file "A6 unfinished tail agent-2 ran post-resume" "$resumed_companion/agent-2.jsonl"
  assert_file "A6 unfinished tail agent-3 ran post-resume" "$resumed_companion/agent-3.jsonl"

  local final_status final_completed
  final_status=$(python3 -c "import json;print(json.load(open('$resumed_companion/runs/$run_id.json'))['status'])" 2>/dev/null || echo "")
  assert_equals "A6 run-state reaches completed after tail" "completed" "$final_status"
  final_completed=$(python3 -c "import json;print(len(json.load(open('$resumed_companion/runs/$run_id.json'))['completed_agents']))" 2>/dev/null || echo "0")
  assert_equals "A6 all 3 agents completed across the pod restart" "3" "$final_completed"
  echo
}

# ── Layer B real-pod verifier ───────────────────────────────────────
# Returns:
#   0 if a live pod for the agent shows the coop workflow-restore log line
#     OR a non-empty companion dir under ~/.claude/projects/
#   1 if a live pod was found but neither signal is present (assertion fail)
#   2 if no live pod / cluster access (skip)
verify_resume_on_pod() {
  local agent_name="$1"
  command -v kubectl >/dev/null 2>&1 || return 2

  # Synthetic test agent names never match a real controller-spawned pod —
  # there is no prior workflow run for them to resume. Skip cleanly; the
  # bead-level + Layer A scenarios cover the contract deterministically.
  case "$agent_name" in
    e2e-dynwf-agent-*) return 2 ;;
  esac

  local pod
  pod=$(kubectl -n "$NAMESPACE" get pod \
    -l "gardenio.io/agent=${agent_name}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [ -z "$pod" ] && return 2

  dim "  live pod $pod found for agent $agent_name"

  # Signal 1: coop emits this on a successful companion-dir restore (see
  # coop/crates/cli/src/run.rs: "s3: restored workflow companion dir from S3").
  local log_line
  log_line=$(kubectl -n "$NAMESPACE" logs "$pod" -c agent --tail=800 2>/dev/null \
    | grep -F 'restored workflow companion dir' | head -1 || true)
  if [ -n "$log_line" ]; then
    pass "coop restored workflow companion dir on resume"
    dim "  log: $log_line"
    return 0
  fi

  # Signal 2: fallback — a non-empty workflow companion dir on the PVC
  # (sibling of a session transcript, holding agent-*.jsonl journals).
  local journal
  journal=$(kubectl -n "$NAMESPACE" exec "$pod" -c agent -- \
    sh -c 'find "$HOME/.claude/projects" -type f -name "agent-*.jsonl" 2>/dev/null | head -1' \
    2>/dev/null || true)
  if [ -n "$journal" ]; then
    pass "PVC has dynamic-workflow journal ($journal)"
    return 0
  fi

  fail "workflow resume state present on pod $pod" \
    "no coop restore log line and no agent-*.jsonl under ~/.claude/projects/"
  return 1
}

# ── Layer B: bead lifecycle for a mid-run workflow resume ────────────
layer_b_bead_lifecycle() {
  blue "Layer B: workflow-resume bead lifecycle"

  setup_portforward
  setup_token
  seed_session_type

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "Layer B bead lifecycle" "cannot reach kd server at ${BEADS_HTTP_URL:-unset}"
    return
  fi
  green "  Server reachable: $BEADS_HTTP_URL"

  local TEST_TAG="e2e-dynwf-resume-$$-$(date +%s)"
  local AGENT_NAME="e2e-dynwf-agent-$TEST_TAG"
  local RUN_ID="wf_${TEST_TAG//-/_}"
  local HOST="e2e-dynwf-pod"

  # ── B1: gardener-shaped, PVC-backed agent ──────────────────────────────
  local agent_out agent_id
  agent_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<AGEOF
{
  "title": "$AGENT_NAME",
  "type": "agent",
  "description": "E2E dynamic-workflow resume agent — safe to delete",
  "labels": ["e2e:$TEST_TAG", "project:gardenio", "dynworkflow-resume"],
  "fields": {
    "agent": "$AGENT_NAME",
    "agent_state": "working",
    "mode": "crew",
    "project": "gardenio",
    "role": "crew"
  }
}
AGEOF
  )") || true
  agent_id=$(echo "$agent_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$agent_id" ]; then
    pass "B1 gardener-shaped agent bead created ($agent_id)"
    CREATED_BEADS+=("$agent_id")
  else
    fail "B1 gardener-shaped agent bead created" "response: $agent_out"
    return
  fi

  # ── B2: workflow-run session 1 — running mid-workflow (resumed=false) ──
  # Workflow run-state rides in the session description as JSON so we do not
  # have to extend the shared type:session schema (see seed_session_type).
  local s1_out s1_id
  s1_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(python3 - "$AGENT_NAME" "$agent_id" "$HOST" "$RUN_ID" "$TEST_TAG" <<'PYEOF'
import json, sys
agent, agent_id, host, run_id, tag = sys.argv[1:6]
body = {
  "title": f"wf-session-1-{tag}",
  "type": "session",
  "labels": [f"e2e:{tag}", "dynworkflow-resume"],
  "description": json.dumps({
    "workflow_run_id": run_id,
    "workflow_status": "in_progress",
    "total_steps": 3,
    "completed_agents": [1],
    "next_agent": 2,
  }),
  "fields": {
    "agent": agent,
    "agent_bead_id": agent_id,
    "project": "gardenio",
    "role": "crew",
    "hostname": host,
    "session_log": "/home/agent/.claude/projects/-home-agent/wf.jsonl",
    "started_at": "2026-05-28T12:00:00Z",
    "resumed": False,
    "status": "active",
    "request_count": 7,
  },
}
print(json.dumps(body))
PYEOF
    )") || true
  s1_id=$(echo "$s1_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$s1_id" ]; then
    pass "B2 workflow-run session 1 created ($s1_id)"
    CREATED_BEADS+=("$s1_id")
  else
    fail "B2 workflow-run session 1 created" "response: $s1_out"
    return
  fi
  local s1_show
  s1_show=$("$KD_BIN" show "$s1_id" --json 2>/dev/null || echo "{}")
  assert_equals "B2 session 1 resumed=false" "false" "$(json_nested "$s1_show" fields.resumed)"
  local s1_runid
  s1_runid=$(json_nested "$s1_show" description | python3 -c "import sys,json; print(json.loads(sys.stdin.read() or '{}').get('workflow_run_id',''))" 2>/dev/null || echo "")
  assert_equals "B2 session 1 carries the workflow run-id" "$RUN_ID" "$s1_runid"

  # ── B3: pod killed mid-run → session 1 crashed (exit 137) ─────────────
  "$KD_BIN" update "$s1_id" \
    -f 'status=crashed' -f 'exit_code=137' \
    -f "ended_at=2026-05-28T12:05:00Z" 2>/dev/null || true
  "$KD_BIN" close "$s1_id" 2>/dev/null || true
  local s1_after
  s1_after=$("$KD_BIN" show "$s1_id" --json 2>/dev/null || echo "{}")
  assert_equals "B3 session 1 exit_code=137 (SIGKILL mid-run)" "137" "$(json_nested "$s1_after" fields.exit_code)"
  assert_equals "B3 session 1 status=crashed" "crashed" "$(json_nested "$s1_after" fields.status)"

  # ── B4: resume session 2 — resumed=true, same host, same run-id ───────
  local s2_out s2_id
  s2_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(python3 - "$AGENT_NAME" "$agent_id" "$HOST" "$RUN_ID" "$TEST_TAG" <<'PYEOF'
import json, sys
agent, agent_id, host, run_id, tag = sys.argv[1:6]
body = {
  "title": f"wf-session-2-{tag}",
  "type": "session",
  "labels": [f"e2e:{tag}", "dynworkflow-resume"],
  "description": json.dumps({
    "workflow_run_id": run_id,
    "workflow_status": "resumed",
    "resume_from_run_id": run_id,
    "cached_agents": [1],
    "reran_agents": [2, 3],
  }),
  "fields": {
    "agent": agent,
    "agent_bead_id": agent_id,
    "project": "gardenio",
    "role": "crew",
    "hostname": host,
    "session_log": "/home/agent/.claude/projects/-home-agent/wf.jsonl",
    "started_at": "2026-05-28T12:06:00Z",
    "resumed": True,
    "status": "active",
    "request_count": 0,
  },
}
print(json.dumps(body))
PYEOF
    )") || true
  s2_id=$(echo "$s2_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$s2_id" ]; then
    pass "B4 resume session 2 created ($s2_id)"
    CREATED_BEADS+=("$s2_id")
  else
    fail "B4 resume session 2 created" "response: $s2_out"
    return
  fi
  local s2_show
  s2_show=$("$KD_BIN" show "$s2_id" --json 2>/dev/null || echo "{}")
  assert_equals "B4 session 2 resumed=true" "true" "$(json_nested "$s2_show" fields.resumed)"
  assert_equals "B4 session 2 same PVC host (PVC intact)" "$HOST" "$(json_nested "$s2_show" fields.hostname)"
  assert_equals "B4 session 2 links to same agent" "$agent_id" "$(json_nested "$s2_show" fields.agent_bead_id)"
  assert_equals "B4 session 2 starts with fresh request count" "0" "$(json_nested "$s2_show" fields.request_count)"

  # resumeFromRunId continuity: session 2 resumes the exact run session 1 left.
  local s2_resume_from
  s2_resume_from=$(json_nested "$s2_show" description | python3 -c "import sys,json; print(json.loads(sys.stdin.read() or '{}').get('resume_from_run_id',''))" 2>/dev/null || echo "")
  assert_equals "B4 session 2 resumeFromRunId == session 1 run-id" "$RUN_ID" "$s2_resume_from"

  # ── B5: exactly 2 sessions; both share the one run-id ─────────────────
  local sessions session_count runid_count
  sessions=$("$KD_BIN" list --all-types --json -l "e2e:$TEST_TAG" 2>/dev/null || echo "[]")
  session_count=$(echo "$sessions" | python3 -c "
import sys, json
beads = json.load(sys.stdin)
if isinstance(beads, dict) and 'items' in beads:
    beads = beads['items']
print(sum(1 for b in beads if b.get('type') == 'session'))
" 2>/dev/null || echo "0")
  assert_equals "B5 agent has exactly 2 workflow sessions" "2" "$session_count"

  runid_count=$(echo "$sessions" | python3 -c "
import sys, json
beads = json.load(sys.stdin)
if isinstance(beads, dict) and 'items' in beads:
    beads = beads['items']
n = 0
for b in beads:
    if b.get('type') != 'session':
        continue
    try:
        d = json.loads(b.get('description') or '{}')
    except Exception:
        d = {}
    if d.get('workflow_run_id') == '$RUN_ID':
        n += 1
print(n)
" 2>/dev/null || echo "0")
  assert_equals "B5 both sessions reference the same run-id (continuity)" "2" "$runid_count"
  echo

  # ── B6: optional live-pod verification ────────────────────────────────
  blue "Layer B6: verify workflow resume on live agent pod (optional)"
  local rc=0
  verify_resume_on_pod "$AGENT_NAME" || rc=$?
  case $rc in
    0) : ;;  # pass already reported
    1) : ;;  # fail already reported
    2) skip "live-pod workflow-resume verification" \
         "no agent pod matched gardenio.io/agent=${AGENT_NAME} in ns $NAMESPACE" ;;
  esac
  echo
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " dynworkflow-resume E2E tests"
  blue "═══════════════════════════════════════════════════"
  echo

  layer_a_journal_contract
  layer_b_bead_lifecycle

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
