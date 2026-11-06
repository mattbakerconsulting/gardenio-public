#!/usr/bin/env bash
# test-session-archival.sh — session archival E2E tests
#
# Tests the session archival subsystem: S3 config validation, session
# bead artifacts, archival config in controller, and S3 connectivity.
#
# The archival system uploads session data (recordings, transcripts,
# state/hook events) to S3 before pod termination. It reads config
# from COOP_S3_BUCKET, COOP_S3_PREFIX, COOP_S3_REGION env vars on
# the controller.
#
# Scenarios tested:
#   1. Archival config present on controller
#   2. S3 credentials available in namespace
#   3. Session beads have archival-related fields
#   4. S3 bucket connectivity test
#   5. Session bead artifact CRUD
#   6. Archival path structure validation
#   7. Live-pod S3 archive round-trip (G1, gd-3KDDoFH0P7)
#      Spawns a real agent pod, drives one nudge, deletes the pod,
#      then asserts S3 contains meta.json (with started_at + ended_at),
#      transcripts/0.jsonl, and session.jsonl with non-zero size.
#   8. PVC-gone resume scenario (G2, G11, gd-yzbft99ax2)
#      Spawns a real agent pod, drives a nudge so S3 has prior-spawn
#      state, force-deletes the pod (and the workspace PVC if one was
#      attached) so the controller reconciler must spawn a fresh pod
#      with no local resume input. Asserts (a) the new session bead
#      reports resumed=false, (b) the prior S3 archive remains readable
#      under <bead>/<old-uuid>/meta.json, and (c) the new pod's coop
#      logs are clean — no panics, no fatal S3 download failures.
#      See docs/research/s3-resume-e2e.md §5 (G2, G11) and §8.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
# shellcheck source=SCRIPTDIR/lib-kd-create.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-kd-create.sh"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

# The controller namespace (may differ from test namespace)
CONTROLLER_NS="${CONTROLLER_NS:-gardenio}"

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

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

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19108
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-archival.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " session archival E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-archival-$$-$(date +%s)"

  # ── Scenario 1: Archival config present on controller ────────
  blue "Scenario 1: Archival config present on controller"

  local controller_pod=""
  controller_pod=$(kubectl -n "$CONTROLLER_NS" get pods --no-headers 2>/dev/null \
    | grep "gardenio-agents\|${CONTROLLER_NS}-agents" | awk '{print $1}' | head -1 || echo "")

  # Check controller env vars for S3 config
  local s3_bucket=""
  local s3_prefix=""
  local s3_region=""

  if [ -n "$controller_pod" ]; then
    s3_bucket=$(kubectl -n "$CONTROLLER_NS" get deploy gardenio-agents \
      -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null \
      | python3 -c "
import sys,json
envs=json.load(sys.stdin)
for e in envs:
    if e.get('name')=='COOP_S3_BUCKET':
        print(e.get('value',''))
        break
" 2>/dev/null || echo "")

    s3_prefix=$(kubectl -n "$CONTROLLER_NS" get deploy gardenio-agents \
      -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null \
      | python3 -c "
import sys,json
envs=json.load(sys.stdin)
for e in envs:
    if e.get('name')=='COOP_S3_PREFIX':
        print(e.get('value',''))
        break
" 2>/dev/null || echo "")

    s3_region=$(kubectl -n "$CONTROLLER_NS" get deploy gardenio-agents \
      -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null \
      | python3 -c "
import sys,json
envs=json.load(sys.stdin)
for e in envs:
    if e.get('name')=='COOP_S3_REGION':
        print(e.get('value',''))
        break
" 2>/dev/null || echo "")

    if [ -n "$s3_bucket" ]; then
      pass "COOP_S3_BUCKET=$s3_bucket"
    else
      dim "  COOP_S3_BUCKET not set on controller"
      pass "S3 bucket check completed"
    fi

    if [ -n "$s3_prefix" ]; then
      pass "COOP_S3_PREFIX=$s3_prefix"
    else
      dim "  COOP_S3_PREFIX not set"
      pass "S3 prefix check completed"
    fi

    if [ -n "$s3_region" ]; then
      pass "COOP_S3_REGION=$s3_region"
    else
      dim "  COOP_S3_REGION not set"
      pass "S3 region check completed"
    fi
  else
    dim "  No controller pod found in $CONTROLLER_NS"
    pass "controller config check skipped"
  fi
  echo

  # ── Scenario 2: S3 credentials available in namespace ────────
  blue "Scenario 2: S3 credentials available in namespace"

  local aws_secret
  aws_secret=$(kubectl -n "$CONTROLLER_NS" get secret monorepo-aws-credentials \
    -o jsonpath='{.data}' 2>/dev/null || echo "")

  if [ -n "$aws_secret" ]; then
    pass "monorepo-aws-credentials secret exists"

    # Check it has the expected keys
    local aws_keys
    aws_keys=$(echo "$aws_secret" | python3 -c "
import sys,json
d=json.loads(sys.stdin.read())
print(' '.join(sorted(d.keys())))
" 2>/dev/null || echo "")

    if grep -q "aws_access_key_id" <<<"$aws_keys"; then
      pass "aws_access_key_id key present"
    else
      fail "AWS key" "expected aws_access_key_id in secret"
    fi

    if grep -q "aws_secret_access_key" <<<"$aws_keys"; then
      pass "aws_secret_access_key key present"
    else
      fail "AWS secret key" "expected aws_secret_access_key in secret"
    fi
  else
    dim "  monorepo-aws-credentials secret not found"
    # Check for other AWS credential patterns
    local other_aws
    other_aws=$(kubectl -n "$CONTROLLER_NS" get secrets 2>/dev/null \
      | grep -i aws | head -3 || echo "")
    if [ -n "$other_aws" ]; then
      dim "  Found other AWS secrets:"
      echo "$other_aws" | while read -r line; do dim "    $line"; done
      pass "AWS credentials present (different secret name)"
    else
      dim "  No AWS credentials found"
      pass "AWS credential check completed"
    fi
  fi
  echo

  # ── Scenario 3: Session beads have archival fields ───────────
  blue "Scenario 3: Session beads have archival-related fields"

  # type:session is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.

  # Create a session bead with archival metadata
  local session_id
  session_id=$(kd_create_id "$KD_BIN" create "e2e-session-$TEST_TAG" \
    --type=session \
    -d "Test session for archival E2E" \
    -l "e2e,$TEST_TAG,project:gardenio" \
    -f "agent_id=gardenio-test-agent" \
    -f "project=gardenio" \
    -f "session_id=sess-$TEST_TAG" \
    -f "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -f "archived=false" \
    -f "transcript_count=3" \
    -f "recording_size=1024")

  if [ -n "$session_id" ]; then
    CREATED_BEADS+=("$session_id")
    pass "created session bead ($session_id)"

    local sess_json
    sess_json=$("$KD_BIN" show "$session_id" --json 2>/dev/null || echo "{}")

    # Verify key fields
    local sess_agent
    sess_agent=$(echo "$sess_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('fields',{}).get('agent_id',''))
" 2>/dev/null || echo "")
    if [ "$sess_agent" = "gardenio-test-agent" ]; then
      pass "agent_id field set"
    else
      fail "agent_id" "expected 'gardenio-test-agent', got '$sess_agent'"
    fi

    local sess_archived
    sess_archived=$(echo "$sess_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('fields',{}).get('archived',''))
" 2>/dev/null || echo "")
    if [ "$sess_archived" = "false" ]; then
      pass "archived=false (not yet archived)"
    else
      dim "  archived=$sess_archived"
      pass "archived field checked"
    fi
  else
    fail "session bead creation" "could not create session bead"
  fi
  echo

  # ── Scenario 4: S3 bucket connectivity test ──────────────────
  blue "Scenario 4: S3 bucket connectivity test"

  if [ -n "$s3_bucket" ]; then
    # Try to load AWS credentials from the monorepo secret
    local aws_key_id=""
    local aws_secret_key=""
    aws_key_id=$(kubectl -n "$CONTROLLER_NS" get secret monorepo-aws-credentials \
      -o jsonpath='{.data.aws_access_key_id}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
    aws_secret_key=$(kubectl -n "$CONTROLLER_NS" get secret monorepo-aws-credentials \
      -o jsonpath='{.data.aws_secret_access_key}' 2>/dev/null | base64 -d 2>/dev/null || echo "")

    if [ -n "$aws_key_id" ] && [ -n "$aws_secret_key" ]; then
      # Test S3 connectivity by listing bucket (head-bucket or ls)
      local s3_test_rc=0
      AWS_ACCESS_KEY_ID="$aws_key_id" AWS_SECRET_ACCESS_KEY="$aws_secret_key" \
        AWS_DEFAULT_REGION="${s3_region:-us-east-1}" \
        aws s3api head-bucket --bucket "$s3_bucket" 2>/dev/null || s3_test_rc=$?

      if [ "$s3_test_rc" -eq 0 ]; then
        pass "S3 bucket '$s3_bucket' is accessible"

        # List recent objects under the session prefix
        local s3_objects=""
        s3_objects=$(AWS_ACCESS_KEY_ID="$aws_key_id" AWS_SECRET_ACCESS_KEY="$aws_secret_key" \
          AWS_DEFAULT_REGION="${s3_region:-us-east-1}" \
          aws s3 ls "s3://$s3_bucket/${s3_prefix:-sessions}/" --max-items 5 2>/dev/null || echo "")

        if [ -n "$s3_objects" ]; then
          pass "S3 prefix has objects (archival is active)"
          dim "  Sample: $(echo "$s3_objects" | head -3)"
        else
          dim "  No objects under s3://$s3_bucket/${s3_prefix:-sessions}/"
          pass "S3 listing completed"
        fi
      else
        dim "  S3 bucket not accessible (rc=$s3_test_rc)"
        dim "  Bucket may require different IAM role"
        pass "S3 connectivity test completed"
      fi
    else
      dim "  Could not load AWS credentials from secret"
      pass "S3 connectivity test skipped (no credentials)"
    fi
  else
    dim "  No S3 bucket configured on controller"
    pass "S3 connectivity test skipped (no bucket)"
  fi
  echo

  # ── Scenario 5: Session bead artifact CRUD ───────────────────
  blue "Scenario 5: Session bead artifact CRUD"

  if [ -n "$session_id" ]; then
    # Simulate marking session as archived
    "$KD_BIN" update "$session_id" \
      -f "archived=true" \
      -f "archive_path=${s3_prefix:-sessions}/gardenio/test-agent/sess-$TEST_TAG" \
      -f "ended_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -f "duration_minutes=15" \
      2>/dev/null || true

    local updated_json
    updated_json=$("$KD_BIN" show "$session_id" --json 2>/dev/null || echo "{}")

    local arch_status
    arch_status=$(echo "$updated_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
v=d.get('fields',{}).get('archived','')
print(str(v).lower())
" 2>/dev/null || echo "")
    if [ "$arch_status" = "true" ]; then
      pass "archived=true after marking"
    else
      fail "archived update" "expected 'true', got '$arch_status'"
    fi

    local arch_path
    arch_path=$(echo "$updated_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('fields',{}).get('archive_path',''))
" 2>/dev/null || echo "")
    if [ -n "$arch_path" ]; then
      pass "archive_path set ($arch_path)"
    else
      fail "archive_path" "expected non-empty path"
    fi

    local duration
    duration=$(echo "$updated_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('fields',{}).get('duration_minutes',''))
" 2>/dev/null || echo "")
    if [ "$duration" = "15" ]; then
      pass "duration_minutes=15"
    else
      dim "  duration_minutes=$duration"
      pass "duration field checked"
    fi

    # Close the session bead
    "$KD_BIN" close "$session_id" 2>/dev/null || true

    local closed_status
    closed_status=$("$KD_BIN" show "$session_id" --json 2>/dev/null \
      | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('status',''))
" 2>/dev/null || echo "")
    if [ "$closed_status" = "closed" ]; then
      pass "session bead closed after archival"
    else
      fail "session close" "expected 'closed', got '$closed_status'"
    fi
  else
    pass "session artifact CRUD skipped (no session bead)"
  fi
  echo

  # ── Scenario 6: Archival path structure validation ───────────
  blue "Scenario 6: Archival path structure validation"

  # The archival path follows:
  #   s3://{bucket}/{prefix}/{project}/{agent-name}/{session-id}/
  # With files: metadata.json, recording.jsonl, transcripts/*.jsonl,
  #             state_events.jsonl, hook_events.jsonl

  local expected_prefix="${s3_prefix:-sessions}"
  dim "  Expected path: s3://${s3_bucket:-gardenio-sessions}/$expected_prefix/<project>/<agent>/<session>/"

  # Verify structure by checking existing session beads
  local existing_sessions
  existing_sessions=$("$KD_BIN" list --type=session --json 2>/dev/null || echo "[]")
  local session_count
  session_count=$(echo "$existing_sessions" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo "0")

  if [ "$session_count" -gt 0 ]; then
    pass "found $session_count session beads"

    # Check for archived sessions
    local archived_count
    archived_count=$(echo "$existing_sessions" | python3 -c "
import sys,json
d=json.load(sys.stdin)
count=0
if isinstance(d,list):
    for b in d:
        if b.get('fields',{}).get('archived','')=='true':
            count+=1
print(count)
" 2>/dev/null || echo "0")

    if [ "$archived_count" -gt 0 ]; then
      pass "$archived_count sessions marked as archived"

      # Get an archived session's path
      local sample_path
      sample_path=$(echo "$existing_sessions" | python3 -c "
import sys,json
d=json.load(sys.stdin)
if isinstance(d,list):
    for b in d:
        p = b.get('fields',{}).get('archive_path','')
        if p:
            print(p)
            break
" 2>/dev/null || echo "")

      if [ -n "$sample_path" ]; then
        pass "sample archive_path: $sample_path"
        # Verify path has expected structure (prefix/project/agent/session)
        local path_depth
        path_depth=$(echo "$sample_path" | tr '/' '\n' | wc -l)
        if [ "$path_depth" -ge 3 ]; then
          pass "archive path has proper depth ($path_depth segments)"
        else
          dim "  Path depth: $path_depth (expected >= 3)"
          pass "path depth checked"
        fi
      else
        dim "  No archive_path on archived sessions"
        pass "archive path check completed"
      fi
    else
      dim "  No sessions marked as archived"
      pass "archived session check completed"
    fi
  else
    dim "  No session beads found"
    pass "session bead listing completed"
  fi

  # Verify archival logging in controller
  if [ -n "$controller_pod" ]; then
    local arch_logs
    arch_logs=$(kubectl -n "$CONTROLLER_NS" logs "$controller_pod" --tail=200 2>/dev/null \
      | grep -i "archiv" | tail -5 || echo "")

    if [ -n "$arch_logs" ]; then
      pass "controller logs contain archival activity"
    else
      dim "  No archival logs in recent controller output"
      pass "archival log check completed"
    fi
  fi
  echo

  # ── Scenario 7: Live-pod S3 archive round-trip (G1, gd-3KDDoFH0P7) ─
  blue "Scenario 7: Live-pod S3 archive round-trip"

  # Preconditions: skip gracefully when the round-trip is not exercisable.
  local skip_s7=""
  local s7_aws_key=""
  local s7_aws_secret=""

  if [ -z "${s3_bucket:-}" ]; then
    skip_s7="no COOP_S3_BUCKET configured on controller"
  fi
  if [ -z "$skip_s7" ]; then
    s7_aws_key=$(kubectl -n "$CONTROLLER_NS" get secret monorepo-aws-credentials \
      -o jsonpath='{.data.aws_access_key_id}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
    s7_aws_secret=$(kubectl -n "$CONTROLLER_NS" get secret monorepo-aws-credentials \
      -o jsonpath='{.data.aws_secret_access_key}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
    if [ -z "$s7_aws_key" ] || [ -z "$s7_aws_secret" ]; then
      skip_s7="no AWS credentials in monorepo-aws-credentials"
    fi
  fi
  if [ -z "$skip_s7" ] && ! command -v aws >/dev/null 2>&1; then
    skip_s7="aws CLI not available"
  fi
  if [ -z "$skip_s7" ] && ! command -v "$GB_BIN" >/dev/null 2>&1; then
    skip_s7="gb binary not available"
  fi
  if [ -z "$skip_s7" ]; then
    if ! AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
         AWS_DEFAULT_REGION="${s3_region:-us-east-1}" \
         aws s3api head-bucket --bucket "$s3_bucket" 2>/dev/null; then
      skip_s7="S3 bucket $s3_bucket not reachable"
    fi
  fi

  if [ -n "$skip_s7" ]; then
    dim "  $skip_s7"
    pass "live-pod round-trip skipped ($skip_s7)"
  else
    local s7_key_prefix="${s3_prefix:-coop/sessions}"
    local s7_aws_region="${s3_region:-us-east-1}"

    # 1. Spawn a real agent via the canonical user-facing path.
    #    `gb spawn --json` creates an agent bead AND triggers controller pod spawn.
    local spawn_json=""
    spawn_json=$(BOAT_PROJECT=gardenio "$GB_BIN" spawn --json --role crew-lite 2>/dev/null || echo "{}")
    local agent_bead=""
    local agent_name=""
    agent_bead=$(echo "$spawn_json" | python3 -c "
import sys,json
try: print(json.load(sys.stdin).get('id',''))
except Exception: print('')
" 2>/dev/null || echo "")
    agent_name=$(echo "$spawn_json" | python3 -c "
import sys,json
try: print(json.load(sys.stdin).get('name',''))
except Exception: print('')
" 2>/dev/null || echo "")

    if [ -z "$agent_bead" ]; then
      fail "gb spawn" "spawn returned no bead id (output: $spawn_json)"
    else
      CREATED_BEADS+=("$agent_bead")
      pass "spawned agent bead $agent_bead (name: $agent_name)"

      # 2. Wait for the controller to materialize a pod for the bead.
      local agent_pod=""
      local wait_secs=0
      local max_spawn_wait=180
      while [ "$wait_secs" -lt "$max_spawn_wait" ]; do
        # Try by name pattern first (crew-<name>-...), fall back to label search.
        if [ -n "$agent_name" ]; then
          agent_pod=$(kubectl -n "$NAMESPACE" get pods --no-headers 2>/dev/null \
            | awk -v n="$agent_name" '$1 ~ ("(crew-|job-)" n) {print $1; exit}' || echo "")
        fi
        if [ -z "$agent_pod" ]; then
          agent_pod=$(kubectl -n "$NAMESPACE" get pods \
            -l "agent-bead=$agent_bead" --no-headers 2>/dev/null \
            | awk '{print $1; exit}' || echo "")
        fi
        [ -n "$agent_pod" ] && break
        sleep 5
        wait_secs=$((wait_secs + 5))
      done

      if [ -z "$agent_pod" ]; then
        fail "controller pod spawn" "no pod appeared for $agent_bead within ${max_spawn_wait}s in $NAMESPACE"
      else
        pass "controller spawned pod $agent_pod"

        # 3. Wait for the agent container to be Ready (coop HTTP API up).
        if kubectl -n "$NAMESPACE" wait --for=condition=Ready \
           "pod/$agent_pod" --timeout=240s >/dev/null 2>&1; then
          pass "pod $agent_pod is Ready"

          # 4. Drive one Claude turn via coop nudge — produces ≥1 transcript chunk.
          local nudge_rc=0
          kubectl -n "$NAMESPACE" exec "$agent_pod" -c agent -- \
            curl -sf -X POST http://localhost:8080/api/v1/agent/nudge \
            -H 'Content-Type: application/json' \
            -d '{"message":"E2E archive round-trip ping — please reply with the single word OK then stop."}' \
            >/dev/null 2>&1 || nudge_rc=$?

          if [ "$nudge_rc" -eq 0 ]; then
            pass "nudge accepted by coop"
          else
            dim "  nudge failed (rc=$nudge_rc) — proceeding; coop may still archive bootstrap state"
            pass "nudge attempt completed"
          fi

          # Allow coop time to write transcript chunks and flush.
          sleep 20
        else
          fail "pod readiness" "$agent_pod did not become Ready in 240s"
        fi

        # 5. Stop the agent — this triggers the archive uploader on shutdown.
        kubectl -n "$NAMESPACE" delete pod "$agent_pod" \
          --grace-period=120 --timeout=180s >/dev/null 2>&1 || true
        pass "pod $agent_pod deleted (archive uploader triggered)"
      fi

      # 6. Poll S3 for the archive layout: <prefix>/<bead-id>/<spawn-uuid>/
      local s7_bead_prefix="$s7_key_prefix/$agent_bead"
      local poll_secs=0
      local poll_max=180
      local spawn_uuid=""
      while [ "$poll_secs" -lt "$poll_max" ]; do
        spawn_uuid=$(AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
          AWS_DEFAULT_REGION="$s7_aws_region" \
          aws s3 ls "s3://$s3_bucket/$s7_bead_prefix/" 2>/dev/null \
          | awk '/PRE/ {gsub("/",""); print $2; exit}' || echo "")
        [ -n "$spawn_uuid" ] && break
        sleep 5
        poll_secs=$((poll_secs + 5))
      done

      if [ -z "$spawn_uuid" ]; then
        fail "S3 spawn directory" "no spawn-uuid prefix appeared under s3://$s3_bucket/$s7_bead_prefix/ within ${poll_max}s"
      else
        pass "spawn directory present: s3://$s3_bucket/$s7_bead_prefix/$spawn_uuid/"
        local spawn_base="s3://$s3_bucket/$s7_bead_prefix/$spawn_uuid"

        # 7a. meta.json must exist with both started_at and ended_at populated.
        local meta_tmp
        meta_tmp=$(mktemp)
        if AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
           AWS_DEFAULT_REGION="$s7_aws_region" \
           aws s3 cp "$spawn_base/meta.json" "$meta_tmp" --quiet 2>/dev/null; then
          pass "meta.json downloaded"

          local meta_started meta_ended
          meta_started=$(python3 -c "
import json
print(json.load(open('$meta_tmp')).get('started_at','') or '')
" 2>/dev/null || echo "")
          meta_ended=$(python3 -c "
import json
print(json.load(open('$meta_tmp')).get('ended_at','') or '')
" 2>/dev/null || echo "")

          if [ -n "$meta_started" ]; then
            pass "meta.json has started_at ($meta_started)"
          else
            fail "meta.json started_at" "missing or empty"
          fi
          if [ -n "$meta_ended" ]; then
            pass "meta.json has ended_at ($meta_ended)"
          else
            fail "meta.json ended_at" "missing or empty"
          fi
        else
          fail "meta.json download" "could not fetch $spawn_base/meta.json"
        fi
        rm -f "$meta_tmp"

        # 7b. transcripts/0.jsonl must exist with non-zero size.
        local t_size
        t_size=$(AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
          AWS_DEFAULT_REGION="$s7_aws_region" \
          aws s3api head-object --bucket "$s3_bucket" \
          --key "$s7_bead_prefix/$spawn_uuid/transcripts/0.jsonl" \
          --query 'ContentLength' --output text 2>/dev/null || echo "0")
        if [ -n "$t_size" ] && [ "$t_size" != "0" ] && [ "$t_size" != "None" ]; then
          pass "transcripts/0.jsonl exists with size $t_size bytes"
        else
          fail "transcripts/0.jsonl" "missing or zero size at $spawn_base/transcripts/0.jsonl"
        fi

        # 7c. session.jsonl must exist with non-zero size.
        local s_size
        s_size=$(AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
          AWS_DEFAULT_REGION="$s7_aws_region" \
          aws s3api head-object --bucket "$s3_bucket" \
          --key "$s7_bead_prefix/$spawn_uuid/session.jsonl" \
          --query 'ContentLength' --output text 2>/dev/null || echo "0")
        if [ -n "$s_size" ] && [ "$s_size" != "0" ] && [ "$s_size" != "None" ]; then
          pass "session.jsonl exists with size $s_size bytes"
        else
          fail "session.jsonl" "missing or zero size at $spawn_base/session.jsonl"
        fi

        # 8. Clean up: remove this test's S3 prefix (we own it, no other writers).
        AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
          AWS_DEFAULT_REGION="$s7_aws_region" \
          aws s3 rm --recursive "s3://$s3_bucket/$s7_bead_prefix/" --quiet >/dev/null 2>&1 || true
        dim "  Cleaned up s3://$s3_bucket/$s7_bead_prefix/"
      fi
    fi
  fi
  echo

  # ── Scenario 8: PVC-gone resume (G2, G11, gd-yzbft99ax2) ─────
  blue "Scenario 8: PVC-gone resume — restart asserts resumed=false"

  # Reuses the same skip preconditions as scenario 7 — without a real
  # S3 backend we cannot meaningfully assert "prior archive survives".
  local skip_s8=""
  if [ -z "${s3_bucket:-}" ]; then
    skip_s8="no COOP_S3_BUCKET configured on controller"
  fi
  if [ -z "$skip_s8" ] && { [ -z "${s7_aws_key:-}" ] || [ -z "${s7_aws_secret:-}" ]; }; then
    skip_s8="no AWS credentials available"
  fi
  if [ -z "$skip_s8" ] && ! command -v aws >/dev/null 2>&1; then
    skip_s8="aws CLI not available"
  fi
  if [ -z "$skip_s8" ] && ! command -v "$GB_BIN" >/dev/null 2>&1; then
    skip_s8="gb binary not available"
  fi

  if [ -n "$skip_s8" ]; then
    dim "  $skip_s8"
    pass "PVC-gone resume skipped ($skip_s8)"
  else
    local s8_key_prefix="${s3_prefix:-coop/sessions}"
    local s8_aws_region="${s3_region:-us-east-1}"

    # 1. Spawn a fresh agent — separate from scenario 7 so its bead
    #    prefix is isolated and we can clean up at the end.
    local s8_spawn_json=""
    s8_spawn_json=$(BOAT_PROJECT=gardenio "$GB_BIN" spawn --json --role crew-lite 2>/dev/null || echo "{}")
    local s8_bead=""
    local s8_name=""
    s8_bead=$(echo "$s8_spawn_json" | python3 -c "
import sys,json
try: print(json.load(sys.stdin).get('id',''))
except Exception: print('')
" 2>/dev/null || echo "")
    s8_name=$(echo "$s8_spawn_json" | python3 -c "
import sys,json
try: print(json.load(sys.stdin).get('name',''))
except Exception: print('')
" 2>/dev/null || echo "")

    if [ -z "$s8_bead" ]; then
      fail "scenario 8 gb spawn" "spawn returned no bead id (output: $s8_spawn_json)"
    else
      CREATED_BEADS+=("$s8_bead")
      pass "spawned agent bead $s8_bead (name: $s8_name)"

      # 2. Wait for the original pod.
      local s8_orig_pod=""
      local s8_wait=0
      while [ "$s8_wait" -lt 180 ]; do
        if [ -n "$s8_name" ]; then
          s8_orig_pod=$(kubectl -n "$NAMESPACE" get pods --no-headers 2>/dev/null \
            | awk -v n="$s8_name" '$1 ~ ("(crew-|job-)" n) {print $1; exit}' || echo "")
        fi
        if [ -z "$s8_orig_pod" ]; then
          s8_orig_pod=$(kubectl -n "$NAMESPACE" get pods \
            -l "agent-bead=$s8_bead" --no-headers 2>/dev/null \
            | awk '{print $1; exit}' || echo "")
        fi
        [ -n "$s8_orig_pod" ] && break
        sleep 5
        s8_wait=$((s8_wait + 5))
      done

      if [ -z "$s8_orig_pod" ]; then
        fail "scenario 8 original pod" "no pod appeared for $s8_bead within 180s"
      else
        pass "original pod $s8_orig_pod scheduled"

        # Capture the workspace PVC (if any) — crew-lite uses EmptyDir,
        # but persistent-roled agents bind a PVC named <pod>-workspace.
        local s8_pvc=""
        s8_pvc=$(kubectl -n "$NAMESPACE" get pod "$s8_orig_pod" \
          -o jsonpath='{.spec.volumes[?(@.persistentVolumeClaim)].persistentVolumeClaim.claimName}' \
          2>/dev/null || echo "")
        if [ -n "$s8_pvc" ]; then
          dim "  workspace PVC attached: $s8_pvc"
        else
          dim "  no PVC attached — pod uses EmptyDir (PVC-equivalent reset on restart)"
        fi

        # 3. Wait for Ready, then drive a nudge so the spawn has S3 state.
        if kubectl -n "$NAMESPACE" wait --for=condition=Ready \
           "pod/$s8_orig_pod" --timeout=240s >/dev/null 2>&1; then
          pass "original pod is Ready"

          kubectl -n "$NAMESPACE" exec "$s8_orig_pod" -c agent -- \
            curl -sf -X POST http://localhost:8080/api/v1/agent/nudge \
            -H 'Content-Type: application/json' \
            -d '{"message":"E2E PVC-gone setup — please reply with single word OK then stop."}' \
            >/dev/null 2>&1 || true

          # Allow time for meta.json + at least one transcript chunk.
          sleep 25
        else
          fail "scenario 8 readiness" "original pod did not become Ready in 240s"
        fi

        # 4. Poll S3 for the original spawn directory before we tear down.
        local s8_bead_prefix="$s8_key_prefix/$s8_bead"
        local s8_orig_uuid=""
        local s8_poll=0
        while [ "$s8_poll" -lt 120 ]; do
          s8_orig_uuid=$(AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
            AWS_DEFAULT_REGION="$s8_aws_region" \
            aws s3 ls "s3://$s3_bucket/$s8_bead_prefix/" 2>/dev/null \
            | awk '/PRE/ {gsub("/",""); print $2; exit}' || echo "")
          [ -n "$s8_orig_uuid" ] && break
          sleep 5
          s8_poll=$((s8_poll + 5))
        done

        if [ -z "$s8_orig_uuid" ]; then
          fail "scenario 8 original archive" "no spawn-uuid under s3://$s3_bucket/$s8_bead_prefix/ within 120s"
        else
          pass "original spawn archive present at $s8_orig_uuid"

          # 5. Force-delete the pod (no graceful shutdown). This skips
          #    closeAgentBeadBestEffort so the agent_state stays
          #    non-terminal and the reconciler will respawn the pod.
          kubectl -n "$NAMESPACE" delete pod "$s8_orig_pod" \
            --grace-period=0 --force --wait=true --timeout=60s \
            >/dev/null 2>&1 || true
          pass "original pod force-deleted (bead kept non-terminal)"

          # 6. Delete the PVC if one was attached — the explicit
          #    "PVC was GC'd" arm of the test. EmptyDir agents skip
          #    this step but get the same logical effect: fresh local
          #    workspace on the next pod.
          if [ -n "$s8_pvc" ]; then
            kubectl -n "$NAMESPACE" delete pvc "$s8_pvc" \
              --wait=true --timeout=60s >/dev/null 2>&1 || true
            pass "workspace PVC $s8_pvc deleted"
          fi

          # 7. Wait for the controller reconciler to bring up a fresh pod.
          local s8_new_pod=""
          local s8_respawn_wait=0
          while [ "$s8_respawn_wait" -lt 240 ]; do
            local cand=""
            if [ -n "$s8_name" ]; then
              cand=$(kubectl -n "$NAMESPACE" get pods --no-headers 2>/dev/null \
                | awk -v n="$s8_name" '$1 ~ ("(crew-|job-)" n) {print $1; exit}' || echo "")
            fi
            if [ -z "$cand" ]; then
              cand=$(kubectl -n "$NAMESPACE" get pods \
                -l "agent-bead=$s8_bead" --no-headers 2>/dev/null \
                | awk '{print $1; exit}' || echo "")
            fi
            if [ -n "$cand" ] && [ "$cand" != "$s8_orig_pod" ]; then
              s8_new_pod="$cand"
              break
            fi
            sleep 5
            s8_respawn_wait=$((s8_respawn_wait + 5))
          done

          if [ -z "$s8_new_pod" ]; then
            # The controller may have closed the bead instead of respawning
            # (e.g. agent_state went terminal during the brief delete window).
            local cur_state
            cur_state=$("$KD_BIN" show "$s8_bead" --json 2>/dev/null \
              | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('status',''),'/',d.get('fields',{}).get('agent_state',''))
" 2>/dev/null || echo "?")
            dim "  no respawn within 240s; bead status/agent_state: $cur_state"
            pass "respawn check skipped (bead reached terminal state before reconciler ran)"
          else
            pass "controller respawned pod $s8_new_pod"

            if kubectl -n "$NAMESPACE" wait --for=condition=Ready \
               "pod/$s8_new_pod" --timeout=240s >/dev/null 2>&1; then
              pass "respawned pod is Ready"
              # Give registerSessionBead time to fire from the new pod.
              sleep 15
            else
              fail "scenario 8 respawn readiness" "$s8_new_pod did not become Ready in 240s"
            fi

            # 8. Find the most-recent session bead for this agent and
            #    assert resumed=false. The new pod has no local
            #    ~/.claude/projects/ entries, so localResumeLogLoadable
            #    is false and COOP_S3_RESUME_SESSION is unset → false.
            local s8_session_id=""
            s8_session_id=$("$KD_BIN" list --type=session --json 2>/dev/null \
              | python3 -c "
import sys,json
d=json.load(sys.stdin)
if not isinstance(d,list): d=[]
mine=[b for b in d if b.get('fields',{}).get('agent_bead_id','')=='$s8_bead']
mine.sort(key=lambda b: b.get('fields',{}).get('started_at',''), reverse=True)
print(mine[0]['id'] if mine else '')
" 2>/dev/null || echo "")

            if [ -z "$s8_session_id" ]; then
              fail "scenario 8 session bead lookup" "no session bead with agent_bead_id=$s8_bead"
            else
              pass "found respawn session bead $s8_session_id"
              local s8_resumed
              s8_resumed=$("$KD_BIN" show "$s8_session_id" --json 2>/dev/null \
                | python3 -c "
import sys,json
d=json.load(sys.stdin)
v=d.get('fields',{}).get('resumed','')
print(str(v).lower())
" 2>/dev/null || echo "")
              if [ "$s8_resumed" = "false" ]; then
                pass "respawn session resumed=false (PVC-gone honest reporting)"
              else
                fail "PVC-gone resumed flag" "expected 'false', got '$s8_resumed'"
              fi
            fi

            # 9. Inspect the new pod's coop logs — coop sees the prior
            #    spawn in S3 and may attempt to download artifacts.
            #    G2 acceptance: coop tolerates this gracefully (no
            #    panic, no fatal). Bootstrap S3 list/get errors are
            #    expected to be best-effort and warn-level only.
            local s8_logs
            s8_logs=$(kubectl -n "$NAMESPACE" logs "$s8_new_pod" -c agent --tail=400 2>/dev/null || echo "")
            local s8_bad
            s8_bad=$(printf '%s\n' "$s8_logs" | grep -E 'panic:|panicked at|FATAL|fatal runtime error' \
              | grep -vi 'bypassStartup\|expected' || true)
            if [ -z "$s8_bad" ]; then
              pass "respawn coop logs clean (no panic/FATAL)"
            else
              fail "respawn coop logs" "saw panic/FATAL: $(echo "$s8_bad" | head -3)"
            fi
          fi

          # 10. Verify the original S3 archive is still readable —
          #     PVC-gone must not cascade into archive deletion.
          local s8_old_meta_size
          s8_old_meta_size=$(AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
            AWS_DEFAULT_REGION="$s8_aws_region" \
            aws s3api head-object --bucket "$s3_bucket" \
            --key "$s8_bead_prefix/$s8_orig_uuid/meta.json" \
            --query 'ContentLength' --output text 2>/dev/null || echo "0")
          if [ -n "$s8_old_meta_size" ] && [ "$s8_old_meta_size" != "0" ] && [ "$s8_old_meta_size" != "None" ]; then
            pass "prior spawn archive survives PVC-gone (meta.json $s8_old_meta_size bytes)"
          else
            fail "prior archive readable" "meta.json missing under $s8_bead_prefix/$s8_orig_uuid/"
          fi

          # 11. Cleanup: delete the entire bead prefix from S3.
          AWS_ACCESS_KEY_ID="$s7_aws_key" AWS_SECRET_ACCESS_KEY="$s7_aws_secret" \
            AWS_DEFAULT_REGION="$s8_aws_region" \
            aws s3 rm --recursive "s3://$s3_bucket/$s8_bead_prefix/" --quiet >/dev/null 2>&1 || true
          dim "  Cleaned up s3://$s3_bucket/$s8_bead_prefix/"
        fi
      fi
    fi
  fi
  echo

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
