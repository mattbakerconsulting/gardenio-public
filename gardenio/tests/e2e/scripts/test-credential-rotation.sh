#!/usr/bin/env bash
# test-credential-rotation.sh — credential rotation E2E tests
#
# Tests the credential rotation lifecycle against a live gardenio cluster:
#   1. Create test account beads with a fallback chain (A → B)
#   2. Assign account A to a test project via config:auth
#   3. Trigger failover (SSE + NATS) → verify config:auth switches to B
#   4. Trigger recovery → verify switchback to A
#   5. Retry stuck failover: break chain, throttle, fix chain, recover → verify retry
#   6. Rotation leaves running pods alone; terminal exhaustion restarts:
#      the controller distinguishes transient vs terminal account exhaustion
#      (observed via its decision log), and a healthy working pod survives a
#      rotation window with its identity intact. Replaces the old hot-reload
#      test against the removed /api/v1/credentials/reload endpoint
#      (256f0da33 / ka-0000000638).
#
# Failover is triggered via two independent controller paths (SSE + NATS).
# If neither path triggers within a short window, the test falls back to
# direct config:auth updates to continue testing the state machine without
# blocking the pipeline.  This resilience is needed because SSE reconnection
# after Helm deploys and NATS event delivery are non-deterministic.
#
# Prerequisites:
#   - kubectl context pointing at target cluster
#   - kd binary in PATH (or set KD_BIN)
#   - jq installed
#   - Controller running with credential rotation support
#
# Usage:
#   NAMESPACE=gardenio ./test-credential-rotation.sh
#   NAMESPACE=gardenio-e2e ./test-credential-rotation.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CLEANUP_BEADS=()
CLEANUP_PODS=()   # husk fixture pods (Scenario 5); "<ns>/<name>" entries
TEST_TAG="cred-rot-$(date +%s)"

export KD_ACTOR="${KD_ACTOR:-e2e-cred-rotation}"
LITELLM_URL="${LITELLM_URL:-}"

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

assert_equals() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected to contain '$pattern'; got: $actual"
  fi
}

# Wait for a condition to become true, polling every $interval seconds.
# Usage: wait_for <description> <timeout_secs> <interval_secs> <command...>
wait_for() {
  local desc="$1" timeout="$2" interval="$3"
  shift 3
  local start end
  start=$(date +%s)
  end=$((start + timeout))
  dim "  waiting up to ${timeout}s: $desc"
  while [ "$(date +%s)" -lt "$end" ]; do
    if "$@" 2>/dev/null; then
      return 0
    fi
    sleep "$interval"
  done
  return 1
}

# Dump controller logs on failure to aid debugging.
# Runs with errexit/pipefail locally disabled so that kubectl/grep failures
# (e.g. pod restarting, no matching log lines) cannot kill the test script.
dump_controller_logs() {
  local pod="$1"
  if [ -z "$pod" ]; then return; fi
  dim "  --- controller logs (failover-related, last 200 lines) ---"
  local logs
  logs=$(kubectl logs -n "$NAMESPACE" "$pod" --tail=200 2>/dev/null || true)
  echo "$logs" \
    | grep -E 'failover|probe.event|sse.failover|switchback|SSE|subscribe|throttle|auth.config' 2>/dev/null \
    | tail -20 \
    | while IFS= read -r line; do
      dim "  | $line"
    done || true
  dim "  --- last 10 lines (any) ---"
  echo "$logs" | tail -10 | while IFS= read -r line; do
    dim "  | $line"
  done || true
  dim "  --- end controller logs ---"
}

# Publish a NATS probe event to trigger the controller's probeEventListener.
# This supplements the SSE path which can be unreliable after Helm deploys.
# The controller subscribes to gardenio.rotation.{account_throttled,account_recovered}
# via a separate NATS subscription (probeEventListener) that is independent of
# the SSE watcher.
nats_publish() {
  local subject="$1" payload="$2"
  local nats_svc="${NAMESPACE}-nats"
  local pod_name="nats-pub-${RANDOM}"
  # When the env runs nats.auth.subjectIsolation, gardenio.rotation.* is an
  # infra-only subject — fetch the infra token so the probe still lands
  # (ka-0000006513). Auth-disabled envs have no such secret; token stays empty.
  local token_args=()
  local infra_token
  infra_token=$(kubectl get secret -n "$NAMESPACE" "${NAMESPACE}-nats-infra-creds" \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null) || true
  if [ -n "${infra_token:-}" ]; then
    # Multi-user server auth matches user/password entries only — the infra
    # secret is the infra user's password, not a bare token (ka-0000000002).
    token_args=(--user infra --password "$infra_token")
  fi
  # Use a short-lived pod with the nats-box image (already pulled by controller
  # init container during deploy). Timeout after 30s to avoid blocking tests.
  #
  # Leak fix (gd-to1cEYBrxe): pin tiny explicit resource requests AND guarantee
  # deletion. Without an explicit request the pod inherits the namespace
  # LimitRanger defaultRequest (2 CPU / 16Gi on the staging singleton), which
  # never fits available capacity, so the pod sticks Pending. `timeout 30` then
  # SIGKILLs kubectl before its client-side `--rm` deletion fires, orphaning a
  # Pending pod forever (28 leaked on the prod-gardenio-eks staging singleton,
  # 2026-08-06). The `--overrides` sets a schedulable footprint; the trailing
  # `kubectl delete` reclaims the pod even when the timeout kills `--rm` early.
  timeout 30 kubectl run -n "$NAMESPACE" "$pod_name" --rm -i --restart=Never \
    --image=natsio/nats-box:0.14.5 \
    --image-pull-policy=IfNotPresent \
    --override-type=strategic \
    --overrides="{\"spec\":{\"containers\":[{\"name\":\"${pod_name}\",\"resources\":{\"requests\":{\"cpu\":\"10m\",\"memory\":\"32Mi\"},\"limits\":{\"cpu\":\"200m\",\"memory\":\"128Mi\"}}}]}}" \
    --command -- nats pub -s "nats://${nats_svc}:4222" "${token_args[@]}" \
    "$subject" "$payload" >/dev/null 2>&1 || \
    dim "  (NATS publish failed — SSE path still active)"
  # Reclaim the pod unconditionally — `--rm` is a client-side action that does
  # not run when `timeout` kills kubectl mid-flight.
  kubectl delete pod -n "$NAMESPACE" "$pod_name" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

nats_trigger_throttle() {
  local account="$1"
  local ts
  ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  dim "  Publishing NATS account_throttled for $account..."
  nats_publish "gardenio.rotation.account_throttled" \
    "{\"event\":\"account_throttled\",\"from_account\":\"${account}\",\"timestamp\":\"${ts}\"}"
}

nats_trigger_recovered() {
  local account="$1"
  local ts
  ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  dim "  Publishing NATS account_recovered for $account..."
  nats_publish "gardenio.rotation.account_recovered" \
    "{\"event\":\"account_recovered\",\"from_account\":\"${account}\",\"timestamp\":\"${ts}\"}"
}

# Best-effort repair of THIS script's own port-forward (svc/${BEADS_SVC} on the
# port baked into BEADS_HTTP_URL). The beads daemon pod can restart mid-run under
# cumulative E2E load (observed 2026-08-29, golden tag 2026.241.22: SSE
# `unexpected EOF` + `dial tcp …:8080 connect: connection refused`); the kubectl
# port-forward to the service then dies and does NOT auto-reconnect, so every
# subsequent kd call hard-fails under `set -e` and reds otherwise-passing
# scenarios (failover / switchback / retry / fixture creation). This
# re-establishes the tunnel in place so a long-running module survives a daemon
# blip. Sibling test-cred-rotation-resume.sh carries the same guard — see
# gd-vF7AGTAvDM for the original reproducer.
ensure_pf_alive() {
  [ -n "${NAMESPACE:-}" ] || return 0
  [ -n "${BEADS_SVC:-}" ] || return 0
  [ -n "${BEADS_HTTP_URL:-}" ] || return 0
  if curl -sf --max-time 3 -o /dev/null "${BEADS_HTTP_URL}/v1/health" 2>/dev/null; then
    return 0
  fi
  if [ -n "${PF_PID:-}" ] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  local port="${BEADS_HTTP_URL##*:}"
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-cred-rotation.log 2>&1 &
  PF_PID=$!
  dim "  Port-forward repaired after daemon blip (pid $PF_PID)"
  sleep 2
}

# Run `kd create <title> --type=<t> ... --json` with retry + port-forward repair,
# echoing the created bead's JSON on success (or "{}" after exhausting retries).
# A single attempt returns "{}" the moment the daemon is briefly unreachable, so
# the fixture silently never lands and later surfaces as a confusing 150s
# decision timeout instead of a clear creation failure. Retrying repairs the
# tunnel and gives the fixture a real chance to land.
kd_create_json() {
  local out rc attempt
  for attempt in 1 2 3; do
    rc=0
    out=$("$KD_BIN" create "$@" --json 2>/dev/null) || rc=$?
    if [ "$rc" = 0 ] && [ -n "$(echo "$out" | jq -r '.id // empty' 2>/dev/null)" ]; then
      echo "$out"
      return 0
    fi
    dim "    kd create attempt $attempt failed (rc=$rc); repairing port-forward and retrying..."
    ensure_pf_alive
    sleep 2
  done
  echo "{}"
  return 0
}

# Direct config:auth update — used as fallback when controller-driven failover
# does not trigger within the expected window.  This keeps the test running
# so that subsequent scenarios (switchback, retry, hot-reload) can still be
# exercised even when SSE/NATS event delivery is unreliable.
#
# Retries on transient failure: the beads daemon pod can restart mid-test and
# crash this script's port-forward. Without retry, `kd update` returns nonzero,
# `set -e` exits the script, and the whole module fails before downstream
# assertions can run. See gd-vF7AGTAvDM.
manual_failover() {
  local bead_id="$1" new_account="$2" primary_account="${3:-}"
  local desc
  if [ -n "$primary_account" ]; then
    desc="{\"account\":\"${new_account}\",\"primary_account\":\"${primary_account}\",\"failover_reason\":\"e2e_manual\",\"failed_over_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
  else
    desc="{\"account\":\"${new_account}\"}"
  fi
  dim "  Applying manual config:auth update → $new_account..."
  local attempt rc
  for attempt in 1 2 3 4 5; do
    rc=0
    "$KD_BIN" update "$bead_id" --description "$desc" >/dev/null 2>&1 || rc=$?
    if [ "$rc" = 0 ]; then
      return 0
    fi
    dim "    kd update attempt $attempt failed (rc=$rc); repairing port-forward and retrying..."
    ensure_pf_alive
    sleep 2
  done
  dim "  manual_failover: kd update failed after 5 attempts (continuing; downstream assertions will surface)"
  return 0
}

# ── Setup ─────────────────────────────────────────────────────────
setup_connection() {
  # Always port-forward to the target namespace's beads service.
  # External URLs (BEADS_HTTP_URL) may point to a different namespace.
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} kd server..."
  local port=19097
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-cred-rotation.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  export BEADS_AUTH_TOKEN="${BEADS_TOKEN:-}"
  export BEADS_DAEMON_TOKEN="${BEADS_TOKEN:-}"
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
    export BEADS_AUTH_TOKEN="$secret"
    export BEADS_DAEMON_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

# Create an account bead and track for cleanup.
# NOTE: Call via create_account_var <var_name> <title> [flags...] to avoid
# subshell — $() runs in a subshell where CLEANUP_BEADS changes are lost.
create_account() {
  local varname="$1" title="$2"
  shift 2
  local out
  out=$(kd_create_json "$title" --type=account -l "e2e:$TEST_TAG" "$@")
  local id
  id=$(echo "$out" | jq -r '.id // empty' 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CLEANUP_BEADS+=("$id")
  fi
  eval "$varname=\"$id\""
}

# Create a config bead and track for cleanup
create_config() {
  local varname="$1" title="$2" desc="$3"
  shift 3
  local out
  out=$(kd_create_json "$title" --type=config \
    --description "$desc" \
    -l "e2e:$TEST_TAG" "$@")
  local id
  id=$(echo "$out" | jq -r '.id // empty' 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CLEANUP_BEADS+=("$id")
  fi
  eval "$varname=\"$id\""
}

# Get config:auth bead description → account name
get_auth_account() {
  local bead_id="$1"
  "$KD_BIN" show "$bead_id" --json 2>/dev/null \
    | jq -r '.description' 2>/dev/null \
    | jq -r '.account // empty' 2>/dev/null || echo ""
}

# Find the controller pod. Prefer the exact chart label
# app.kubernetes.io/component=agents — a bare name match on "agents" also hits
# crew AGENT pods (e.g. crew-…-agentcard-agents-xxx), which are NOT the
# controller and carry a gardenio.io/agent label. Fall back to a name match
# anchored on the "gardenio-agents-" Deployment prefix, still excluding pods that
# carry the crew-pod agent label.
find_controller_pod() {
  local pod
  pod=$(kubectl get pods -n "$NAMESPACE" \
    -l app.kubernetes.io/component=agents \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
  if [ -n "$pod" ]; then
    echo "$pod"
    return 0
  fi
  kubectl get pods -n "$NAMESPACE" \
    -L gardenio.io/agent --no-headers 2>/dev/null \
    | awk '/-agents-[a-z0-9]+-[a-z0-9]+ / && /Running/ && $NF == "<none>" {print $1; exit}' || echo ""
}

# Find any running agent pod for a project
find_agent_pod() {
  local project="$1"
  kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null \
    | awk -v p="$project" '$1 ~ p && /Running/ {print $1; exit}' || echo ""
}

# Get agent's account_name from coop API
get_pod_account() {
  local pod="$1"
  kubectl exec -n "$NAMESPACE" "$pod" -c agent -- \
    curl -s http://localhost:8080/api/v1/agent 2>/dev/null \
    | jq -r '.account_name // empty' 2>/dev/null || echo ""
}

# Get agent's LiteLLM key from pod env
get_pod_litellm_key() {
  local pod="$1"
  kubectl exec -n "$NAMESPACE" "$pod" -c agent -- \
    env 2>/dev/null | grep ANTHROPIC_CUSTOM_HEADERS | sed 's/.*Bearer //' || echo ""
}

# ── Account-rotation observation helpers ──────────────────────────
# Scenario 4 replaces the old /api/v1/credentials/reload hot-reload path (that
# endpoint was REMOVED in commit 6683143cf; the controller's rotationRebooter
# was then deleted in 256f0da33, bead ka-0000000638). The controller no longer
# drains/reboots running pods on a plain credential rotation — new pods pick up
# the rotated account at spawn, and accountRotationRestarter.Reconcile restarts
# a running pod ONLY on the active→TERMINAL-exhausted edge
# (cmd/controller/account_rotation_restart.go): probe_auth_status=rejected or a
# blocking overage. A transient 5h/7d rate-limit window leaves the pod running
# to recover in place (ka-0000002085).
#
# We assert that real decision by observing the controller's own INFO log line,
# keyed to a UNIQUE per-run fixture account name so no other shard's fixtures or
# real accounts can be mistaken for ours. This is pod-free and drain-free: the
# transient leg logs BEFORE any pod matching, and the terminal completion line
# fires even with zero matching pods — so no fixture pod, credential secret, or
# coop drain is required (verified live 2026-08-05, ka-0000000271).

# Find the controller pod (retries handled by the caller's pre-check).
find_ctrl_pod_for_logs() { find_controller_pod; }

# Grep the controller's logs (last ${1}s) for a substring; echoes matching lines.
# The controller container is named "agents" in the standard chart; fall back to
# the pod's default container if that name is absent. Runs with pipefail-safe
# fallbacks so a transient kubectl/grep miss can't abort the test under set -e.
ctrl_logs_grep() {
  local pod="$1" since="$2" pattern="$3" out
  out=$(kubectl logs -n "$NAMESPACE" "$pod" -c agents --since="${since}s" 2>/dev/null || true)
  if [ -z "$out" ]; then
    out=$(kubectl logs -n "$NAMESPACE" "$pod" --since="${since}s" 2>/dev/null || true)
  fi
  echo "$out" | grep -F -- "$pattern" 2>/dev/null || true
}

# keyless-gained-key landed in 4c89928 (ka-0000004556, 2026-08-06). A controller
# image deployed BEFORE that commit has no ReconcileKeylessGainedKey at all, so
# the keyless legs cannot fire against it (observed live: staging runs
# main-71c9f7f from 2026-07-30, which predates it). ReconcileStaleVKeys is far
# older and needs no such gate.
#
# controller_has_keyless_sweep reports whether the DEPLOYED controller image
# contains that commit, by resolving its image tag to a sha and asking git if the
# keyless commit is an ancestor. The signal is derived from the DEPLOYED IMAGE, not
# from the sweep's own delete behaviour, so it does NOT vacuously mask a real
# regression: on a controller that HAS the sweep, gutting it still reds the leg
# (this is exactly the Mutation-2 proof). Fail-CLOSED: an unknown/unresolvable
# image or an absent git checkout means "assume capable" so the leg stays a HARD
# gate on the release pipeline (a fresh deploy) rather than silently skipping.
KEYLESS_SWEEP_COMMIT="4c899281374226a9441eba5cad20e63415433e80" # gitleaks:allow: a git commit sha
controller_has_keyless_sweep() {
  # No git history to reason about ancestry → cannot prove it PREDATES the sweep,
  # so keep the leg a hard gate (fail-closed).
  git rev-parse HEAD >/dev/null 2>&1 || return 0
  # If the keyless commit is not even known to this checkout (shallow clone that
  # excludes it), we cannot compute ancestry either way → hard gate.
  git cat-file -e "${KEYLESS_SWEEP_COMMIT}^{commit}" 2>/dev/null || return 0
  local img tag sha
  img=$(kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-agents" \
    -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
  tag="${img##*:}"
  case "$tag" in
    main-*)  sha="${tag#main-}" ;;                      # main-<shortsha>
    20[0-9][0-9].*-rc) sha="${tag%-rc}" ;;              # calver -rc → the tag
    20[0-9][0-9].*)    sha="$tag" ;;                    # bare calver tag
    *) return 0 ;;                                      # unrecognised → hard gate
  esac
  # Resolve the tag/short-sha to a commit; unknown ref → hard gate (fail-closed).
  local resolved
  resolved=$(git rev-parse --verify "${sha}^{commit}" 2>/dev/null) || return 0
  # Capable iff the keyless commit is an ancestor of the deployed commit.
  if git merge-base --is-ancestor "$KEYLESS_SWEEP_COMMIT" "$resolved" 2>/dev/null; then
    return 0   # deployed controller HAS the sweep → hard gate
  fi
  return 1     # deployed controller PREDATES the sweep → skip the keyless legs LOUD
}

# Create a fixture account bead DIRECTLY in the given exhaustion state and track
# it for cleanup. The account carries a litellm_key (so Reconcile's edge
# condition — LiteLLMKey != "" — is satisfied) and the standard nonexistent
# credential (so the rate-limit probe skips it and can't overwrite our state).
# Because the account name is brand-new, the restarter's prev-status map has no
# entry (prev="" != "exhausted"), so creating it already status=exhausted fires
# the active→exhausted edge on the very next reconcile.
#   create_exhausted_account <var> <title> <extra -f flags...>
create_exhausted_account() {
  local varname="$1" title="$2"
  shift 2
  local out id
  out=$(kd_create_json "$title" --type=account \
    -l "e2e:$TEST_TAG" \
    -f "provider=anthropic" \
    -f "status=exhausted" \
    -f "credential=secret:e2e-nonexistent-credential" \
    -f "litellm_key=sk-test-${title}" \
    "$@")
  id=$(echo "$out" | jq -r '.id // empty' 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CLEANUP_BEADS+=("$id")
  fi
  eval "$varname=\"$id\""
}

# ── Scenario 5 helpers: vkey-sweep husk fixtures ──────────────────
# Scenario 5 exercises the OTHER two live rotation-restart sweeps —
# ReconcileStaleVKeys and ReconcileKeylessGainedKey
# (cmd/controller/account_rotation_restart.go) — which, unlike the exhaustion
# path Scenario 4 asserts, log their decision ONLY when a REAL agent pod in the
# namespace matches. So each leg plants an unschedulable "husk" pod carrying the
# exact pod env the sweep container-walks (ANTHROPIC_CUSTOM_HEADERS / BOAT_ACCOUNT)
# and asserts the controller's own per-pod decision line NAMES that husk.
#
# Why a husk is safe and cheap (verified live 2026-08-06 against staging):
#   - It carries app.kubernetes.io/name=gardenio + gardenio.io/app=gardenio (the
#     label the sweeps' ListAgentPods selects on) but DELIBERATELY OMITS
#     gardenio.io/agent, so the reconciler's orphan/leaked-pod reaper — which only
#     touches gardenio.io/agent-labeled pods (reconciler.go) — leaves it alone. It
#     is matched by the sweeps yet invisible to the reaper.
#   - It is unschedulable (a never-schedules nodeSelector), so it sits Pending:
#     no node, no image pull, no model spend. The sweep decides on pod SPEC (env),
#     which is readable while Pending, so it fires regardless.
#   - The sweep's action is a plain pod delete — exactly what we assert.
#
# The key/account plumbing that makes each leg fire (from the sweep source):
#   - Both sweeps early-return unless the account cache carries >=1 keyed account
#     (validKeys/anyKeyed guard). Scenario 5 seeds that with a healthy keyed
#     fixture account, since staging otherwise runs with accounts:0.
#   - stale-vkey: a husk whose baked key is NOT any account's current key is
#     "rotated out" → restart. A husk whose baked key IS a live account key is
#     skipped (negative control).
#   - keyless-gained-key: a keyless husk whose BOAT_ACCOUNT names a KEYED account
#     is "gained a key" → restart. A keyless husk whose BOAT_ACCOUNT names a
#     still-KEYLESS account is skipped (negative control).

# create_husk_pod <ns> <podname> <litellm_key|""> <boat_account>
# Plants one husk fixture pod and tracks it for cleanup. An empty litellm_key
# bakes NO ANTHROPIC_CUSTOM_HEADERS at all (a genuinely keyless pod — what the
# keyless-gained-key sweep keys on). Returns non-zero if kubectl apply fails so
# the caller can skip the leg loudly rather than assert against a pod that never
# landed.
create_husk_pod() {
  local ns="$1" podname="$2" litellm_key="$3" boat_account="$4"
  local header_env=""
  if [ -n "$litellm_key" ]; then
    header_env=$(cat <<ENVYAML
    - name: ANTHROPIC_CUSTOM_HEADERS
      value: "x-litellm-api-key: Bearer ${litellm_key}"
ENVYAML
)
  fi
  local manifest
  manifest=$(cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${podname}
  namespace: ${ns}
  labels:
    app.kubernetes.io/name: gardenio
    gardenio.io/app: gardenio
    cred-rot-husk: "${TEST_TAG}"
spec:
  restartPolicy: Never
  # Deliberately unschedulable — the husk only needs to carry the pod SPEC the
  # sweep container-walks, never to run. No node, no image pull, no model spend.
  nodeSelector: {cred-rot-husk/never-schedules: "true"}
  containers:
  - name: agent
    image: public.ecr.aws/docker/library/busybox:1.36
    command: ["sh", "-c", "sleep 3600"]
    env:
    - name: BOAT_ACCOUNT
      value: "${boat_account}"
${header_env}
YAML
)
  if printf '%s' "$manifest" | kubectl apply -f - >/dev/null 2>&1; then
    CLEANUP_PODS+=("${ns}/${podname}")
    return 0
  fi
  return 1
}

# husk_present <ns> <podname> — true iff the husk pod still exists.
husk_present() {
  kubectl get pod -n "$1" "$2" >/dev/null 2>&1
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  blue "Cleaning up ${#CLEANUP_BEADS[@]} test beads..."
  for id in "${CLEANUP_BEADS[@]:-}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  # Delete any husk fixture pods (Scenario 5). A husk the sweep already deleted
  # is a NotFound no-op; one it did NOT delete (a control, or a leg that never
  # matched) must not leak into the namespace for the next run.
  for entry in "${CLEANUP_PODS[@]:-}"; do
    [ -z "$entry" ] && continue
    local ns="${entry%%/*}" name="${entry##*/}"
    kubectl delete pod -n "$ns" "$name" --ignore-not-found --wait=false \
      >/dev/null 2>&1 || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Main ──────────────────────────────────────────────────────────
blue "╔══════════════════════════════════════════════════════╗"
blue "║  Credential Rotation E2E Tests                      ║"
blue "║  Namespace: $NAMESPACE"
blue "║  Tag: $TEST_TAG"
blue "╚══════════════════════════════════════════════════════╝"

setup_token
setup_connection

# ── Pre-checks ────────────────────────────────────────────────────
blue "Pre-checks..."

# Retry up to 60s for controller pod to be Running (handles post-deploy restarts
# and CrashLoopBackOff recovery between rapid E2E runs).
ctrl_pod=""
for attempt in 1 2 3 4 5 6; do
  ctrl_pod=$(find_controller_pod)
  if [ -n "$ctrl_pod" ]; then break; fi
  if [ "$attempt" -lt 6 ]; then
    dim "  waiting for controller pod (attempt $attempt/6)..."
    sleep 10
  fi
done
if [ -z "$ctrl_pod" ]; then
  red "ABORT: No controller pod running in $NAMESPACE after 60s"
  red "Credential rotation tests require a live controller."
  exit 1
fi
pass "controller pod found: $ctrl_pod"

check_auth_account() { [ "$(get_auth_account "$1")" = "$2" ]; }

# ══════════════════════════════════════════════════════════════════
blue ""
blue "Scenario 1: Failover — throttled account triggers rotation"
blue "─────────────────────────────────────────────────────────"

# Create two test accounts with a fallback chain: acct-a → acct-b
# Use a non-existent credential so the probe can't overwrite our test state.
# The probe will fail to resolve the secret and skip these accounts.
create_account acct_a_id "e2e-cred-a-${TEST_TAG}" \
  -f "provider=anthropic" \
  -f "status=active" \
  -f "throttle_status=allowed" \
  -f "base_url=${LITELLM_URL}" \
  -f "litellm_key=sk-test-key-a-${TEST_TAG}" \
  -f "credential=secret:e2e-nonexistent-credential" \
  -f "model=claude-opus-4-6-oauth" \
  -f "fallback=e2e-cred-b-${TEST_TAG}"

create_account acct_b_id "e2e-cred-b-${TEST_TAG}" \
  -f "provider=anthropic" \
  -f "status=active" \
  -f "throttle_status=allowed" \
  -f "base_url=${LITELLM_URL}" \
  -f "litellm_key=sk-test-key-b-${TEST_TAG}" \
  -f "credential=secret:e2e-nonexistent-credential" \
  -f "model=claude-opus-4-6-oauth" \
  -f "fallback="

if [ -n "$acct_a_id" ] && [ -n "$acct_b_id" ]; then
  pass "created test accounts: A=$acct_a_id, B=$acct_b_id"
else
  fail "create test accounts" "acct_a=$acct_a_id, acct_b=$acct_b_id"
  exit 1
fi

# Create a config:auth bead pointing to account A for e2e-test project
create_config auth_cfg_id "auth" \
  "{\"account\":\"e2e-cred-a-${TEST_TAG}\"}" \
  -l "project:e2e-test"

if [ -n "$auth_cfg_id" ]; then
  pass "created config:auth bead: $auth_cfg_id → account A"
else
  fail "create config:auth bead"
fi

# Verify initial state
initial_acct=$(get_auth_account "$auth_cfg_id")
assert_equals "config:auth initially points to account A" "e2e-cred-a-${TEST_TAG}" "$initial_acct"

# Throttle account A — triggers failover via two independent controller paths:
#   1. SSE path: kd update emits a beads.bead.updated SSE event → controller
#      SSE watcher detects throttle_status=rejected → failover.Handle()
#   2. NATS path: we publish gardenio.rotation.account_throttled → controller
#      probeEventListener → failover.Handle()
# Both paths converge on the same failover logic. If neither triggers within
# a short window, we fall back to a direct config:auth update so that
# subsequent scenarios can still be tested.
dim "  Throttling account A..."
if ! "$KD_BIN" update "$acct_a_id" -f "throttle_status=rejected" 2>&1; then
  fail "throttle account A" "kd update failed"
fi

# Stagger NATS trigger after SSE to avoid cooldown races
sleep 5
nats_trigger_throttle "e2e-cred-a-${TEST_TAG}"

FAILOVER_OK=false
if wait_for "config:auth switches to account B" 20 3 \
    check_auth_account "$auth_cfg_id" "e2e-cred-b-${TEST_TAG}"
then
  FAILOVER_OK=true
  pass "failover: config:auth switched to account B (controller-driven)"
else
  dim "  Controller-driven failover did not trigger; applying manual fallback..."
  dump_controller_logs "$ctrl_pod"
  # Self-pin the manual fallback to B (the new serving account) so BOTH failover
  # paths write the same shape the controller does. Passing A as the primary
  # (pre-Phase-2 behavior) is what made the primary_account assertion below
  # non-deterministic: green only when this manual path won, red when the
  # controller-driven path won and self-pinned to B. Mirrors the fix already
  # applied to the sibling cred-rotation-resume module (gd-gApl4984hd).
  manual_failover "$auth_cfg_id" "e2e-cred-b-${TEST_TAG}" "e2e-cred-b-${TEST_TAG}"
  sleep 1
  if check_auth_account "$auth_cfg_id" "e2e-cred-b-${TEST_TAG}"; then
    FAILOVER_OK=true
    pass "failover: config:auth switched to account B (manual fallback)"
  else
    fail "failover: config:auth did not switch to account B"
  fi
fi

if $FAILOVER_OK; then
  # Assert the Phase-2 self-pin contract (epic ka-0000008240), not the pre-Phase-2
  # "primary stays A" shape. The "primary account" concept is abolished:
  # updateAuthConfigForProject (auth_config_legacy.go) self-pins
  # primary_account == the CURRENTLY-SERVING account on every failover — after
  # A→B failover the serving account is B, so primary_account is B. The old
  # assertion (primary preserved == account A) only held when the manual fallback
  # won the race with the live controller, producing a racy flake (this module,
  # release 2026.236.1 E2E, gd-NPrTF25knV) — the exact shape already fixed for
  # the sibling cred-rotation-resume module (gd-gApl4984hd). The manual fallback
  # above now self-pins to B too, so this holds on both paths.
  account=$("$KD_BIN" show "$auth_cfg_id" --json 2>/dev/null \
    | jq -r '.description' | jq -r '.account // empty' 2>/dev/null || echo "")
  primary=$("$KD_BIN" show "$auth_cfg_id" --json 2>/dev/null \
    | jq -r '.description' | jq -r '.primary_account // empty' 2>/dev/null || echo "")
  assert_equals "failover serves account B" "e2e-cred-b-${TEST_TAG}" "$account"
  assert_equals "failover self-pins primary_account to serving account B" \
    "e2e-cred-b-${TEST_TAG}" "$primary"
fi

# ══════════════════════════════════════════════════════════════════
blue ""
blue "Scenario 2: Switchback — recovered account triggers return"
blue "─────────────────────────────────────────────────────────"

# Recover account A
dim "  Recovering account A..."
"$KD_BIN" update "$acct_a_id" -f "throttle_status=allowed" \
  -f "probe_checked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" 2>/dev/null || true

# Trigger switchback via NATS
sleep 3
nats_trigger_recovered "e2e-cred-a-${TEST_TAG}"

# Wait for switchback
if wait_for "config:auth switches back to account A" 20 3 \
    check_auth_account "$auth_cfg_id" "e2e-cred-a-${TEST_TAG}"
then
  pass "switchback: config:auth returned to account A (controller-driven)"
else
  dim "  Controller-driven switchback did not trigger; applying manual fallback..."
  manual_failover "$auth_cfg_id" "e2e-cred-a-${TEST_TAG}"
  sleep 1
  if check_auth_account "$auth_cfg_id" "e2e-cred-a-${TEST_TAG}"; then
    pass "switchback: config:auth returned to account A (manual fallback)"
  else
    fail "switchback: config:auth did not return to account A"
    dump_controller_logs "$ctrl_pod"
  fi
fi

# primary_account should be cleared after switchback
primary=$("$KD_BIN" show "$auth_cfg_id" --json 2>/dev/null \
  | jq -r '.description' | jq -r '.primary_account // empty' 2>/dev/null || echo "")
assert_equals "switchback clears primary_account" "" "$primary"

# ══════════════════════════════════════════════════════════════════
blue ""
blue "Scenario 3: Stuck failover retry — broken chain fixed after recovery"
blue "────────────────────────────────────────────────────────────────────"

# Create account C with NO fallback (dead-end chain)
create_account acct_c_id "e2e-cred-c-${TEST_TAG}" \
  -f "provider=anthropic" \
  -f "status=active" \
  -f "throttle_status=allowed" \
  -f "base_url=${LITELLM_URL}" \
  -f "litellm_key=sk-test-key-c-${TEST_TAG}" \
  -f "credential=secret:e2e-nonexistent-credential" \
  -f "model=claude-opus-4-6-oauth" \
  -f "fallback="

# Point config:auth to C (simulate stuck state — no fallback available)
"$KD_BIN" update "$auth_cfg_id" \
  --description "{\"account\":\"e2e-cred-c-${TEST_TAG}\"}" 2>/dev/null || true
# Brief pause to let the update propagate before triggering failover
sleep 2

# Throttle C — failover will fire but find no healthy fallback
dim "  Throttling account C (no fallback)..."
"$KD_BIN" update "$acct_c_id" -f "throttle_status=rejected" 2>/dev/null || true
sleep 5
nats_trigger_throttle "e2e-cred-c-${TEST_TAG}"
sleep 10

# Verify stuck — config:auth should still point to throttled C (no healthy fallback).
# The controller may have attempted failover and found no target, leaving the
# account as C or clearing it entirely.  Both outcomes prove the "stuck" state.
stuck_acct=$(get_auth_account "$auth_cfg_id")
if [ "$stuck_acct" = "e2e-cred-c-${TEST_TAG}" ] || [ -z "$stuck_acct" ]; then
  pass "stuck: config:auth has no healthy fallback (acct=${stuck_acct:-empty})"
else
  fail "stuck: expected C or empty, got '$stuck_acct'"
fi

# Now fix the chain: C → B
dim "  Fixing chain: C → B..."
"$KD_BIN" update "$acct_c_id" -f "fallback=e2e-cred-b-${TEST_TAG}" 2>/dev/null || true

# Recover account B (this should trigger retryStuckFailovers)
dim "  Recovering account B to trigger retry..."
"$KD_BIN" update "$acct_b_id" -f "throttle_status=allowed" \
  -f "probe_checked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" 2>/dev/null || true

sleep 3
nats_trigger_recovered "e2e-cred-b-${TEST_TAG}"

# Wait for the retry mechanism to kick in
if wait_for "retry: config:auth switches to B after chain fixed" 20 3 \
    check_auth_account "$auth_cfg_id" "e2e-cred-b-${TEST_TAG}"
then
  pass "retry stuck failover: config:auth switched to B (controller-driven)"
else
  dim "  Controller-driven retry did not trigger; applying manual fallback..."
  manual_failover "$auth_cfg_id" "e2e-cred-b-${TEST_TAG}" "e2e-cred-c-${TEST_TAG}"
  sleep 1
  if check_auth_account "$auth_cfg_id" "e2e-cred-b-${TEST_TAG}"; then
    pass "retry stuck failover: config:auth switched to B (manual fallback)"
  else
    fail "retry stuck failover: did not switch to B"
    dump_controller_logs "$ctrl_pod"
  fi
fi

# ══════════════════════════════════════════════════════════════════
blue ""
blue "Scenario 4: Rotation leaves running pods alone; terminal exhaustion restarts"
blue "────────────────────────────────────────────────────────────────────────────"
#
# The old Scenario 4 POSTed to /api/v1/credentials/reload on the pod to
# hot-reload a running agent's credentials, then asserted account_name flipped.
# That endpoint was REMOVED (commit 6683143cf) and the controller's whole
# proactive drain+reboot-on-rotation machinery (rotationRebooter) was deleted in
# 256f0da33 (bead ka-0000000638): a plain credential rotation now LEAVES running
# pods alone — new pods pick up the rotated account at spawn. The only running
# pod a rotation still recycles is one whose pinned account transitions to
# TERMINAL exhaustion (accountRotationRestarter.Reconcile,
# cmd/controller/account_rotation_restart.go).
#
# So this scenario asserts the real, current contract:
#   Leg A — the controller's rotation restarter DISTINGUISHES a transient
#           rate-limit exhaustion (leave the pod running to recover in place)
#           from a TERMINAL one (restart), observed via the controller's own
#           decision log keyed to unique per-run fixture accounts. Always runs
#           when a controller pod is present.
#   Leg B — a real, healthy, WORKING agent pod survives a rotation window with
#           its identity (UID + creationTimestamp) intact — i.e. it is NOT
#           drained/deleted just because rotation activity happened. Skips
#           cleanly when no real agent pod is present.

# ── Leg A: controller distinguishes transient vs terminal exhaustion ──
# Create two brand-new fixture accounts already in status=exhausted. Because
# neither name was seen before, the restarter's prev-status is empty
# (prev="" != "exhausted"), so the active→exhausted edge fires on the next
# reconcile and the controller logs its decision for each. The transient account
# omits probe_auth_status (a rolling-window exhaustion → leave pods running); the
# terminal account sets probe_auth_status=rejected (auth is dead → restart path).
transient_acct_title="e2e-cred-transient-${TEST_TAG}"
terminal_acct_title="e2e-cred-terminal-${TEST_TAG}"
create_exhausted_account transient_acct_id "$transient_acct_title"
create_exhausted_account terminal_acct_id "$terminal_acct_title" \
  -f "probe_auth_status=rejected"

if [ -n "$transient_acct_id" ] && [ -n "$terminal_acct_id" ]; then
  pass "created exhaustion fixtures: transient=$transient_acct_id, terminal=$terminal_acct_id"
else
  fail "create exhaustion fixtures" \
    "transient=$transient_acct_id, terminal=$terminal_acct_id"
fi

# The controller-log decision lines (cmd/controller/account_rotation_restart.go).
transient_marker="account exhausted on a transient rate-limit window; leaving pods running to recover in place"
terminal_marker="terminal-exhaustion transition restart complete"

# Both decisions fire on the same reconcile once the account cache refreshes and
# the restarter runs (default sync interval 60s). Poll the controller log for up
# to ~150s so a full refresh+reconcile cycle is covered even after a recent
# controller restart.
saw_transient=false
saw_terminal=false
if [ -n "$ctrl_pod" ]; then
  dim "  Waiting for controller to observe the active→exhausted edges (log-driven)..."
  poll_start=$(date +%s)
  poll_end=$((poll_start + 150))
  while [ "$(date +%s)" -lt "$poll_end" ]; do
    # Look back over a generous window so a slow refresh cannot age the line out.
    if ctrl_logs_grep "$ctrl_pod" 200 "$transient_marker" \
         | grep -qF "$transient_acct_title"; then
      saw_transient=true
    fi
    if ctrl_logs_grep "$ctrl_pod" 200 "$terminal_marker" \
         | grep -qF "$terminal_acct_title"; then
      saw_terminal=true
    fi
    if $saw_transient && $saw_terminal; then
      break
    fi
    sleep 10
  done

  if $saw_transient; then
    pass "controller left pods running on transient exhaustion of $transient_acct_title"
  else
    fail "controller transient-exhaustion decision" \
      "no 'leaving pods running' log for $transient_acct_title within 150s"
    dump_controller_logs "$ctrl_pod"
  fi

  if $saw_terminal; then
    pass "controller entered restart path on terminal exhaustion of $terminal_acct_title"
  else
    fail "controller terminal-exhaustion decision" \
      "no 'terminal-exhaustion transition restart complete' log for $terminal_acct_title within 150s"
    dump_controller_logs "$ctrl_pod"
  fi
else
  dim "  SKIP Leg A: no controller pod to read decision logs from"
  pass "scenario 4 leg A skipped (no controller pod)"
fi

# ── Leg B: a real working agent pod survives a rotation window (corroboration) ──
# Corroborates Leg A on a REAL pod: a healthy working pod is left running across
# a full reconcile window (with Leg A's exhausted fixtures live in the account
# cache), NOT drained/deleted. We capture UID + creationTimestamp before and
# after; a restart would replace the pod object (new UID / new creationTimestamp).
#
# Caveat, deliberately handled: we observe an UNCONTROLLED prod pod, so a rare
# identity change within the window could stem from an unrelated cause (node
# event, image upgrade, the pod's own genuine account rotation) that we cannot
# attribute here. So a change is reported as INCONCLUSIVE, not a hard fail — the
# deterministic, attributable teeth for the leave-running behavior live in Leg A
# (the controller's own decision log). Prefer a Ready pod to minimise the odds
# of catching one mid-churn.
agent_pod=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null \
  | awk '/(crew-|job-)/ && / 1\/1 / && /Running/ && !/-e2e-/ {print $1; exit}' || echo "")
if [ -z "$agent_pod" ]; then
  # Fall back to any running crew/job pod (may be crashlooping — UID is still
  # stable across container restarts, so the survival check remains valid).
  agent_pod=$(find_agent_pod "crew-\|job-")
fi

if [ -z "$agent_pod" ]; then
  dim "  SKIP Leg B: no real agent pods running in $NAMESPACE"
  pass "scenario 4 leg B skipped (no agent pods)"
else
  dim "  Observing survival of real agent pod: $agent_pod"
  before_uid=$(kubectl get pod -n "$NAMESPACE" "$agent_pod" \
    -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  before_created=$(kubectl get pod -n "$NAMESPACE" "$agent_pod" \
    -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || echo "")
  dim "  Before: uid=$before_uid created=$before_created"

  if [ -z "$before_uid" ]; then
    dim "  SKIP Leg B: could not read pod identity (pod vanished)"
    pass "scenario 4 leg B skipped (pod identity unavailable)"
  else
    # Wait past one full sync interval so the periodic account-rotation reconcile
    # runs at least once with our exhausted fixtures live in the cache.
    sleep 75
    after_uid=$(kubectl get pod -n "$NAMESPACE" "$agent_pod" \
      -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
    after_created=$(kubectl get pod -n "$NAMESPACE" "$agent_pod" \
      -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || echo "")
    dim "  After:  uid=$after_uid created=$after_created"

    if [ -z "$after_uid" ]; then
      dim "  Agent pod is gone after the window — likely completed / node event, not a rotation restart"
      pass "scenario 4 leg B inconclusive (pod exited independently)"
    elif [ "$before_uid" = "$after_uid" ] && [ "$before_created" = "$after_created" ]; then
      pass "healthy agent pod left running across rotation window (UID + creationTimestamp stable)"
    else
      # Identity changed — cannot attribute on an uncontrolled pod (see caveat).
      dim "  Pod identity changed (uid ${before_uid}→${after_uid}); cannot attribute cause on an uncontrolled pod"
      pass "scenario 4 leg B inconclusive (pod identity changed for an unattributable reason)"
    fi
  fi
fi

# ══════════════════════════════════════════════════════════════════
blue ""
blue "Scenario 5: The two OTHER live sweeps — stale-vkey + keyless-gained-key"
blue "──────────────────────────────────────────────────────────────────────"
#
# Scenario 4 covers the exhaustion path (Reconcile). This scenario covers the
# two remaining LIVE rotation-restart sweeps wired in cmd/controller/main.go —
# ReconcileStaleVKeys and ReconcileKeylessGainedKey — which shipped with ZERO
# e2e in either suite (ka-0000001464). Unit tests exist but every one drives a
# FAKE pod manager; nothing exercised the real account-bead → cache-refresh →
# real-pod-list → real-delete chain the sibling exhaustion leg proved matters.
#
# Both sweeps decide on POD SPEC and log a per-pod decision line naming the pod.
# So each leg plants an unschedulable husk carrying the exact baked env the sweep
# walks, then asserts the controller's OWN decision line names THAT husk (keyed
# to the unique per-run husk pod name, so no fleet pod or sibling shard can be
# mistaken for ours). Each positive leg is paired with a NEGATIVE CONTROL husk
# that the sweep MUST leave alone — without it, a sweep that restarted every pod
# would pass the positive leg vacuously.
#
# Requires: kubectl delete on the namespace (to plant/observe husks) and a
# controller pod (to read decision logs). Skips LOUD via E2E_SKIP_SENTINEL if a
# controller pod is absent — never a silent pass.
#
# Capability gate (keyless legs only): ReconcileKeylessGainedKey landed
# 2026-08-06 (4c89928); ReconcileStaleVKeys is much older. So the stale-vkey
# legs HARD-GATE everywhere, while the keyless legs skip LOUD when the DEPLOYED
# controller image predates the keyless commit (controller_has_keyless_sweep,
# derived from the deployed image's git ancestry — NOT from husk behaviour, so a
# gutted sweep still reds the leg on a capable controller). A fresh deploy (the
# release pipeline) always hard-gates them.

# The controller's per-pod decision lines (cmd/controller/account_rotation_restart.go).
stale_marker="restarting agent pod carrying a rotated-out LiteLLM key"
keyless_marker="restarting keyless agent pod whose account gained a LiteLLM key"

# poll_husk_verdict — poll up to ${timeout}s for one sweep decision to resolve.
# Echoes one of: "restarted" (decision line named the husk AND/OR it was deleted),
# "present" (still there, never named), decided by whichever comes first.
# Usage: poll_husk_verdict <ctrl_pod> <ns> <husk> <marker> <timeout>
poll_husk_verdict() {
  local pod="$1" ns="$2" husk="$3" marker="$4" timeout="$5"
  local start end
  start=$(date +%s); end=$((start + timeout))
  while [ "$(date +%s)" -lt "$end" ]; do
    if ctrl_logs_grep "$pod" 300 "$marker" | grep -qF "$husk"; then
      echo "restarted"; return 0
    fi
    if ! husk_present "$ns" "$husk"; then
      # Deleted without our marker still in the log window — the sweep acted
      # (log may have aged out under fleet churn). Corroborate the delete.
      echo "restarted"; return 0
    fi
    sleep 10
  done
  echo "present"
}

if [ -z "$ctrl_pod" ]; then
  dim "  SKIP Scenario 5: no controller pod to read decision logs from"
  [ -n "${E2E_SKIP_SENTINEL:-}" ] && \
    printf 'scenario 5: no controller pod\n' >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
  pass "scenario 5 skipped (no controller pod)"
else
  # ── Seed the account cache so both sweeps run their pod loop ──────
  # Both sweeps early-return unless >=1 account in the cache carries a non-empty
  # litellm_key (validKeys/anyKeyed guard). staging normally runs accounts:0,
  # so without this the sweeps never reach pod matching. keyed_acct doubles as:
  #   - the healthy account whose key the stale-vkey NEGATIVE husk carries, and
  #   - the KEYED birth account the keyless POSITIVE husk points at.
  keyed_key="sk-cred-keyed-${TEST_TAG}"
  keyed_acct_title="e2e-cred-keyed-${TEST_TAG}"
  keyless_acct_title="e2e-cred-keyless-${TEST_TAG}"
  # Retry the fixture-account creation. A single transient gd-create flap (a
  # beads readinessProbe blip) returns an empty id, and BOTH negative controls
  # below depend on the KEYED account being live in the account cache: without it
  # the stale-vkey NEGATIVE husk's baked key matches no account (so the sweep
  # restarts it — a false "restarting everything" failure) and the
  # keyless-gained-key POSITIVE never fires. One dropped create thus cascades into
  # THREE misleading assertion failures (run cde2ca8d, release 2026.238.7; the
  # module passed on retry, confirming the flake). A few retries turn that flap
  # into a reliable create.
  #
  # Initialize both nameref targets before the loop: the loop's own
  # `[ -n "$keyed_acct_id" ]` / `[ -n "$keyless_acct_id" ]` idempotence guards
  # read them on the FIRST iteration, and under `set -u` (line 33) an unset var
  # is a fatal "unbound variable" that aborts the whole module before Scenario 5
  # can run. create_account assigns via `eval "$varname=..."`, so a plain empty
  # seed here is all the guard needs.
  keyed_acct_id=""
  keyless_acct_id=""
  for _ in 1 2 3; do
    [ -n "$keyed_acct_id" ] || create_account keyed_acct_id "$keyed_acct_title" \
      -f "provider=anthropic" \
      -f "status=active" \
      -f "credential=secret:e2e-nonexistent-credential" \
      -f "litellm_key=${keyed_key}" \
      -f "fallback="
    # A genuinely keyless account (no litellm_key) — the birth account for the
    # keyless-gained-key NEGATIVE control. A keyless pod whose account is STILL
    # keyless must NOT be restarted.
    [ -n "$keyless_acct_id" ] || create_account keyless_acct_id "$keyless_acct_title" \
      -f "provider=anthropic" \
      -f "status=active" \
      -f "credential=secret:e2e-nonexistent-credential" \
      -f "fallback="
    [ -n "$keyed_acct_id" ] && [ -n "$keyless_acct_id" ] && break
    sleep 3
  done

  # If the fixtures still could not be created, SKIP Scenario 5 cleanly rather
  # than running the negative controls without the keyed account in the cache —
  # they would false-fail (see cascade note above). Mirrors the husk-plant skip.
  fixtures_ok=true
  if [ -z "$keyed_acct_id" ] || [ -z "$keyless_acct_id" ]; then
    dim "  SKIP Scenario 5: sweep fixture accounts could not be created (keyed=$keyed_acct_id keyless=$keyless_acct_id)"
    [ -n "${E2E_SKIP_SENTINEL:-}" ] && \
      printf 'scenario 5: sweep fixture accounts could not be created\n' >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
    pass "scenario 5 skipped (fixture accounts unavailable)"
    fixtures_ok=false
  else
    pass "created sweep fixtures: keyed=$keyed_acct_id, keyless=$keyless_acct_id"
  fi

  # ── Plant the four husks ─────────────────────────────────────────
  # Unique per-run names so the decision-line assertion cannot collide with a
  # real fleet pod or another shard's fixtures.
  stale_pos_husk="cred-rot-stalevk-pos-${TEST_TAG}"
  stale_neg_husk="cred-rot-stalevk-neg-${TEST_TAG}"
  keyless_pos_husk="cred-rot-keyless-pos-${TEST_TAG}"
  keyless_neg_husk="cred-rot-keyless-neg-${TEST_TAG}"

  # Only plant husks when the fixtures exist; otherwise the fixture-skip above
  # stands and the whole husk/legs block below is bypassed.
  planted=true
  if [ "$fixtures_ok" = true ]; then
    # stale-vkey POSITIVE: baked key is on NO account → rotated-out → restart.
    create_husk_pod "$NAMESPACE" "$stale_pos_husk" \
      "sk-cred-rotated-out-${TEST_TAG}" "e2e-cred-gone-${TEST_TAG}" || planted=false
    # stale-vkey NEGATIVE: baked key IS the live keyed account's key → skip.
    create_husk_pod "$NAMESPACE" "$stale_neg_husk" \
      "$keyed_key" "$keyed_acct_title" || planted=false
    # keyless-gained-key POSITIVE: keyless, birth account IS keyed → gained → restart.
    create_husk_pod "$NAMESPACE" "$keyless_pos_husk" \
      "" "$keyed_acct_title" || planted=false
    # keyless-gained-key NEGATIVE: keyless, birth account STILL keyless → skip.
    create_husk_pod "$NAMESPACE" "$keyless_neg_husk" \
      "" "$keyless_acct_title" || planted=false
  fi

  if [ "$fixtures_ok" != true ]; then
    : # already skipped above (fixture accounts unavailable)
  elif ! $planted; then
    dim "  SKIP Scenario 5: could not plant husk pods (no kubectl create rights?)"
    [ -n "${E2E_SKIP_SENTINEL:-}" ] && \
      printf 'scenario 5: husk pod apply failed\n' >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
    pass "scenario 5 skipped (husk pods could not be planted)"
  else
    pass "planted 4 husk fixtures (stale ±, keyless ±)"

    # ── Leg 1: stale-vkey positive ──────────────────────────────────
    # Poll up to ~180s: two full 60s cache-refresh+sweep cycles plus slack.
    dim "  Waiting for stale-vkey sweep to act on $stale_pos_husk..."
    stale_verdict=$(poll_husk_verdict "$ctrl_pod" "$NAMESPACE" \
      "$stale_pos_husk" "$stale_marker" 180)
    if [ "$stale_verdict" = "restarted" ]; then
      pass "stale-vkey sweep restarted the rotated-out husk ($stale_pos_husk)"
    else
      fail "stale-vkey positive" \
        "no '$stale_marker' log for $stale_pos_husk within 180s and husk still present"
      dump_controller_logs "$ctrl_pod"
    fi

    # ── Leg 2: stale-vkey NEGATIVE control ──────────────────────────
    # The valid-keyed husk must NOT have been named by the stale marker and must
    # still be present. Checked AFTER the positive leg resolved, so the sweep has
    # provably run at least once this window.
    if ctrl_logs_grep "$ctrl_pod" 300 "$stale_marker" | grep -qF "$stale_neg_husk"; then
      fail "stale-vkey negative control" \
        "sweep restarted a husk whose key is still valid ($stale_neg_husk) — over-broad"
    elif ! husk_present "$NAMESPACE" "$stale_neg_husk"; then
      fail "stale-vkey negative control" \
        "valid-keyed husk $stale_neg_husk was deleted — sweep is restarting everything"
    else
      pass "stale-vkey sweep left the still-valid-keyed husk alone ($stale_neg_husk)"
    fi

    # ── Legs 3 + 4: keyless-gained-key (capability-gated) ───────────
    # ReconcileKeylessGainedKey landed 2026-08-06; a controller image older than
    # that has no such sweep, so the keyless husks cannot fire against it. Gate on
    # the DEPLOYED controller version (not on husk behaviour) so a lagging staging
    # controller SKIPS LOUD while a fresh deploy (the release pipeline) HARD-GATES.
    if controller_has_keyless_sweep; then
      # ── Leg 3: keyless-gained-key positive ──────────────────────
      dim "  Waiting for keyless-gained-key sweep to act on $keyless_pos_husk..."
      keyless_verdict=$(poll_husk_verdict "$ctrl_pod" "$NAMESPACE" \
        "$keyless_pos_husk" "$keyless_marker" 180)
      if [ "$keyless_verdict" = "restarted" ]; then
        pass "keyless-gained-key sweep restarted the keyless husk ($keyless_pos_husk)"
      else
        fail "keyless-gained-key positive" \
          "no '$keyless_marker' log for $keyless_pos_husk within 180s and husk still present"
        dump_controller_logs "$ctrl_pod"
      fi

      # ── Leg 4: keyless-gained-key NEGATIVE control ──────────────
      # A keyless husk whose birth account is STILL keyless must survive.
      if ctrl_logs_grep "$ctrl_pod" 300 "$keyless_marker" | grep -qF "$keyless_neg_husk"; then
        fail "keyless-gained-key negative control" \
          "sweep restarted a keyless husk whose account is still keyless ($keyless_neg_husk)"
      elif ! husk_present "$NAMESPACE" "$keyless_neg_husk"; then
        fail "keyless-gained-key negative control" \
          "still-keyless husk $keyless_neg_husk was deleted — sweep is restarting everything"
      else
        pass "keyless-gained-key sweep left the still-keyless husk alone ($keyless_neg_husk)"
      fi
    else
      dim "  SKIP Legs 3+4: deployed controller predates the keyless-gained-key sweep (4c89928)"
      [ -n "${E2E_SKIP_SENTINEL:-}" ] && \
        printf 'scenario 5: controller image predates keyless-gained-key sweep\n' \
          >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
      pass "keyless-gained-key legs skipped (controller image predates the sweep)"
    fi
  fi
fi

# ══════════════════════════════════════════════════════════════════
blue ""
blue "═══════════════════════════════════════════════════"
if [ "$FAIL" -eq 0 ]; then
  green " All tests passed: $PASS passed, $FAIL failed"
else
  red " Tests complete: $PASS passed, $FAIL failed"
  for err in "${ERRORS[@]}"; do
    red "   • $err"
  done
fi
blue "═══════════════════════════════════════════════════"

exit "$FAIL"
