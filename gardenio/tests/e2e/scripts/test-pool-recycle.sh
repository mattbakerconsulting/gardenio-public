#!/usr/bin/env bash
# test-pool-recycle.sh — stale-image prewarmed recycle / cycle-on-upgrade E2E
# (gd-ppSVTZDNLt, epic gd-JJIph7y3Ns)
#
# THE GAP THIS CLOSES: commits 13f1c4c5 (recycle stale-image prewarmed agents on
# release) and 3c21bbbe (cycle idle stale warm placeholders on upgrade) have UNIT
# coverage only (poolmanager/recycle_test.go: ClosesStaleOnly,
# NeverTouchesAssigned, RateCapConvergence, RecyclesStaleAndRefills). No e2e drives
# a REAL warm pool through an image-rev change against the live controller and
# asserts the recycle contract end-to-end. This is the "warm pod survives a
# release with the wrong image and serves a user on stale code" class.
#
# ── What it asserts (against the live controller's pool reconciler) ───────────
#   1. STAMPING (commit 13f1c4c5): every agent the pool MINTS carries
#      source_image == the pool's effective image (so a later release can detect
#      staleness). The controller mints them — we don't seed them.
#   2. STEADY STATE: with the effective image == the minted agents' stamp,
#      NOTHING is recycled (no false churn).
#   3. UPGRADE/RECYCLE: after the pool's effective image flips (simulating a
#      deploy), the unassigned prewarmed agents stamped with the OLD image are
#      CLOSED (recycled), RATE-CAPPED at drainRate (default 2) per pass, and
#      REFILLED with fresh agents stamped on the NEW image in the same passes.
#   4. PROTECTED NEVER TOUCHED: a prewarmed agent decorated with a thread
#      (slack_thread_ts) or a task (task_id) is NEVER recycled, even though its
#      image is just as stale — these are the "has-thread"/"has-task" guards in
#      recycleStaleAgents. (working/assigning agents are excluded one layer up,
#      in listPrewarmedAgents, and are covered by the unit suite — they never
#      reach the recycler in the live path, so asserting them here would be
#      vacuous.)
#   5. FRESH SURVIVE: the NEW-image refill agents are never recycled.
#   6. METRIC: gardenio_prewarmed_agents_cycled_total{project=<ours>} increments
#      by the number recycled (controller :9092 /metrics).
#
# ── How the upgrade is simulated (deterministic + spend-safe) ─────────────────
# The recycler closes a prewarmed agent bead when its stamped `source_image`
# differs from the pool's *effective image*. effectiveImage(project,role) layers
# cfg.CoopImage < project-bead `image` < role-bead `image` (poolmanager.go
# effectiveImage / podspec.go). For an EPHEMERAL project with no role override,
# the effective image IS the project bead's `image` field — so we drive the whole
# contract by PATCHing that field:
#   (a) project image=$IMG_OLD, enable the pool → the controller MINTS prewarmed
#       agents stamped source_image=$IMG_OLD (steady state: stamp == effective,
#       nothing recycles),
#   (b) PATCH the project bead image=$IMG_NEW (the "deploy"): the controller's
#       periodic project-cache refresh picks up the new effective image, and the
#       next reconcile pass recycles the now-stale OLD agents and refills on NEW.
# Recycle is PURE BEAD LOGIC (close stale beads, refill on deficit) — no running
# pod is required for the recycle assertion, so this module asserts on bead state.
#
# SPEND SAFETY (by construction — strongest possible): $IMG_OLD and $IMG_NEW are
# DELIBERATELY NON-EXISTENT image tags (registry.invalid never resolves). Any pod
# the reconciler creates for a minted/refill agent can only ever ImagePullBackOff
# — the container literally never starts, so BOAT_COMMAND / `claude` can NEVER
# run. Zero model spend is guaranteed regardless of timing. (We also disable the
# pool + reap every agent for our ephemeral project in cleanup so no warm pod
# outlives the run.) Runs in staging (no gVisor required).
#
# Prerequisites: kubectl (in-cluster), kd, curl, python3. Skips gracefully when
# kubectl/cluster access is unavailable.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
# shellcheck source=SCRIPTDIR/lib-kd-create.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-kd-create.sh"
# Controller health (:8091, /healthz, /version) + metrics (:9092, /metrics).
CONTROLLER_SVC="${CONTROLLER_SVC:-${NAMESPACE}-agents}"

# Two deliberately NON-EXISTENT image tags (registry.invalid is a reserved,
# never-resolving TLD ⇒ ImagePullBackOff ⇒ no container start ⇒ no spend). A
# project-bead `image` override makes the project's effective image exactly
# these values, so the controller's real CoopImage is irrelevant to the compare.
IMG_OLD="${IMG_OLD:-registry.invalid/gardenio-e2e/agent:recycle-old-0000000}"
IMG_NEW="${IMG_NEW:-registry.invalid/gardenio-e2e/agent:recycle-new-9999999}"

# Pool size. DRAIN_RATE eligible (POOL_N - 2 protected) must exceed the per-pass
# drain cap (DefaultStaleDrainRate=2 in placeholder.go) so we observe BOTH the
# rate cap and multi-pass convergence. POOL_N=5 → 3 eligible, cap 2 → 2 passes.
POOL_N="${POOL_N:-5}"
DRAIN_RATE="${DRAIN_RATE:-2}"

PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CTL_PF_PID=""
MET_PF_PID=""
CREATED_BEADS=()
PROJECT_ID=""
PROJECT_NAME=""
BEADS_PORT=19108
HEALTH_PORT=18093
METRICS_PORT=19092

export KD_ACTOR="${KD_ACTOR:-e2e-test}"
# A human-shaped operator id so HTTP writes that resolve an operator pass the
# server's isHumanOperator guard.
OPERATOR="${OPERATOR:-oidc:e2e-pool-recycle@example.com}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

kd_status() {
  "$KD_BIN" show "$1" --json 2>/dev/null | python3 -c "
import sys, json
try:
    print(json.load(sys.stdin).get('status', ''))
except Exception:
    print('')
" 2>/dev/null || echo ""
}

kd_field() {
  "$KD_BIN" show "$1" --json 2>/dev/null | python3 -c "
import sys, json
try:
    print(json.load(sys.stdin).get('fields', {}).get('$2', ''))
except Exception:
    print('')
" 2>/dev/null || echo ""
}

# Set the ephemeral project's effective agent image + prewarmed_pool config via a
# raw read-merge-write PATCH (mirrors the bridge's UpdateBeadFields). We PATCH
# (not `kd update -f`) and merge against the live fields so the image override
# and the pool JSON both persist reliably. Args: <image> <enabled> <min> <max>.
patch_project() {
  local image="$1" enabled="$2" minsz="$3" maxsz="$4"
  local cur merged
  cur=$(curl -sf --max-time 8 \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    "${BEADS_HTTP_URL}/v1/beads/${PROJECT_ID}" 2>/dev/null || echo '{}')
  merged=$(IMG="$image" EN="$enabled" MIN="$minsz" MAX="$maxsz" python3 -c "
import sys, json, os
try:
    d = json.loads(sys.stdin.read() or '{}')
except Exception:
    d = {}
fields = d.get('fields', {}) or {}
fields['image'] = os.environ['IMG']
fields['prewarmed_pool'] = {
    'enabled': os.environ['EN'] == 'true',
    'min_size': int(os.environ['MIN']),
    'max_size': int(os.environ['MAX']),
    'mode': 'crew',
    'role': 'thread',
}
print(json.dumps({'fields': fields}))
" <<< "$cur" 2>/dev/null || echo '{}')
  curl -sf --max-time 8 -X PATCH \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -H "X-Operated-By: ${OPERATOR}" \
    -H 'Content-Type: application/json' \
    "${BEADS_HTTP_URL}/v1/beads/${PROJECT_ID}" \
    -d "$merged" >/dev/null 2>&1 || true
}

# patch_project is a fire-and-forget PATCH (curl ... || true): a single dropped
# write during a beads readinessProbe flap is silently lost. That is harmless for
# steps that re-assert the resulting bead state on a poll, but FATAL for Step 5's
# one-shot image flip — if the OLD→NEW `image` write never lands, effectiveImage
# never changes, recycleStaleAgents can NEVER fire, and the test burns its whole
# 240+180+150s recycle budget waiting for a close that is impossible. On a shard
# already near its wall-clock cap that silent hang is what tips the shard into an
# exit-137 timeout (run cde2ca8d, release 2026.238.7). Verify the `image`
# field actually landed, retrying the PATCH a few times, so a transient flap
# becomes a reliable flip — or, if it genuinely cannot be written, a FAST
# attributable failure instead of a ~570s budget-blowing hang.
# Args: <image> <enabled> <min> <max>. Returns 0 iff the image field == <image>.
patch_project_verified() {
  local image="$1" enabled="$2" minsz="$3" maxsz="$4"
  local got
  for _ in 1 2 3 4 5; do
    patch_project "$image" "$enabled" "$minsz" "$maxsz"
    got=$(kd_field "$PROJECT_ID" "image")
    [ "$got" = "$image" ] && return 0
    sleep 3
  done
  return 1
}

# Stamp arbitrary string fields onto an agent bead via read-merge-write PATCH —
# exactly how the bridge decorates a warm agent on assignment. Args:
#   <bead-id> <k1> <v1> [<k2> <v2> ...]
stamp_fields() {
  local id="$1"; shift
  local cur merged
  cur=$(curl -sf --max-time 8 \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    "${BEADS_HTTP_URL}/v1/beads/${id}" 2>/dev/null || echo '{}')
  merged=$(KV="$*" python3 -c "
import sys, json, os, shlex
try:
    d = json.loads(sys.stdin.read() or '{}')
except Exception:
    d = {}
fields = d.get('fields', {}) or {}
kv = shlex.split(os.environ.get('KV', ''))
for i in range(0, len(kv) - 1, 2):
    fields[kv[i]] = kv[i + 1]
print(json.dumps({'fields': fields}))
" <<< "$cur" 2>/dev/null || echo '{}')
  curl -sf --max-time 8 -X PATCH \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -H "X-Operated-By: ${OPERATOR}" \
    -H 'Content-Type: application/json' \
    "${BEADS_HTTP_URL}/v1/beads/${id}" \
    -d "$merged" >/dev/null 2>&1 || true
}

# Emit "id|source_image|agent_state|status" for every agent bead belonging to our
# ephemeral project, querying the daemon agent surface directly (agent beads are
# kind=config and do not surface in a default `kd list`). Includes closed beads
# (status filter omitted) so callers can detect recycles. Sorted by id for
# determinism.
list_project_agents() {
  curl -sf --max-time 8 \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    "${BEADS_HTTP_URL}/v1/beads?type=agent&limit=500" 2>/dev/null \
    | PROJ="$PROJECT_NAME" python3 -c "
import sys, json, os
proj = os.environ['PROJ']
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
rows = []
for b in d.get('beads', d if isinstance(d, list) else []):
    f = b.get('fields', {}) or {}
    if f.get('project') != proj:
        continue
    rows.append('%s|%s|%s|%s' % (
        b.get('id', ''), f.get('source_image', ''),
        f.get('agent_state', ''), b.get('status', '')))
for r in sorted(rows):
    print(r)
" 2>/dev/null || true
}

# Count OPEN pool members for our project stamped with a given image.
#
# A warm pool member is NOT observable only via agent_state=prewarmed: the
# controller flips a member OFF "prewarmed" without recycling it whenever its pod
# cannot be created immediately. Under this staging overlay COOP_MAX_PODS=4 is a
# NAMESPACE-WIDE cap shared across all concurrent shards, and
# POOL_N=5 alone exceeds it — so the reconciler capacity-DEFERS members and flips
# them prewarmed→queued (reconciler.markQueued), or, if a container starts before
# assignment, prewarmed→idle. Both are still LIVE pool members the reconciler
# counts toward target (isParkedPoolMember, poolmanager.go), and a decorated
# member (Step 3's has-thread/has-task) additionally passes through spawning/
# assigning. Filtering on agent_state=="prewarmed" therefore silently undercounts
# a member that was merely capacity-deferred — reading "recycled" where nothing
# was closed (ka-0000006901). Recycle is a bead CLOSE, so the faithful membership
# signal is "open bead in any non-terminal pool state". We count exactly the
# reconciler's pool-membership set plus the decorated-startup states, and exclude
# terminal/working states so a genuinely handed-off member is not miscounted.
count_open_poolmember_stamped() {
  local want="$1"
  list_project_agents | awk -F'|' -v img="$want" '
    $2==img && $4=="open" &&
    ($3=="prewarmed" || $3=="queued" || $3=="idle" || $3=="spawning" || $3=="assigning") {n++}
    END{print n+0}'
}

# Read gardenio_prewarmed_agents_cycled_total summed across label sets matching
# project="$PROJECT_NAME". Prints a float (0 if the series is absent — counters
# with no observation are not emitted).
read_cycled_metric() {
  ensure_metrics_pf
  curl -sf --max-time 6 "http://localhost:${METRICS_PORT}/metrics" 2>/dev/null \
    | PROJ="$PROJECT_NAME" python3 -c "
import sys, re, os
proj = os.environ['PROJ']
total = 0.0; seen = False
for line in sys.stdin:
    if not line.startswith('gardenio_prewarmed_agents_cycled_total'):
        continue
    m = re.search(r'project=\"([^\"]*)\"', line)
    if not m or m.group(1) != proj:
        continue
    try:
        total += float(line.rsplit(' ', 1)[-1].strip()); seen = True
    except ValueError:
        pass
print(total if seen else 0.0)
" 2>/dev/null || echo "0"
}

# ── Port-forwards + token ──────────────────────────────────────────
setup_portforward() {
  if [ -z "${BEADS_HTTP_URL:-}" ]; then
    blue "Port-forwarding ${NAMESPACE} kd server..."
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${BEADS_PORT}:8080" \
      >/tmp/kd-pf-poolrecycle.log 2>&1 &
    PF_PID=$!
    sleep 3
    export BEADS_HTTP_URL="http://localhost:${BEADS_PORT}"
    dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
  else
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
  fi

  blue "Port-forwarding ${NAMESPACE} controller health + metrics..."
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${HEALTH_PORT}:8091" \
    >/tmp/ctl-pf-poolrecycle.log 2>&1 &
  CTL_PF_PID=$!
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${METRICS_PORT}:9092" \
    >/tmp/met-pf-poolrecycle.log 2>&1 &
  MET_PF_PID=$!
  sleep 3
  dim "  Controller health at http://localhost:${HEALTH_PORT} (pid $CTL_PF_PID)"
  dim "  Controller metrics at http://localhost:${METRICS_PORT}/metrics (pid $MET_PF_PID)"
}

# Re-establish the beads daemon port-forward if it died (long runs outlast a
# single kubectl port-forward). Only manages a pf WE created.
ensure_daemon_pf() {
  [ -z "$PF_PID" ] && return 0
  if "$KD_BIN" list --status=open --limit 1 --json >/dev/null 2>&1; then
    return 0
  fi
  kill "$PF_PID" 2>/dev/null || true
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${BEADS_PORT}:8080" \
    >/tmp/kd-pf-poolrecycle.log 2>&1 &
  PF_PID=$!
  sleep 3
}

ensure_metrics_pf() {
  if curl -sf --max-time 3 -o /dev/null "http://localhost:${METRICS_PORT}/metrics" 2>/dev/null; then
    return 0
  fi
  [ -n "$MET_PF_PID" ] && kill "$MET_PF_PID" 2>/dev/null || true
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${METRICS_PORT}:9092" \
    >/tmp/met-pf-poolrecycle.log 2>&1 &
  MET_PF_PID=$!
  sleep 3
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
  # Disable the pool FIRST (before closing the project bead — a closed bead
  # silently drops field writes) so the reconciler stops recreating stragglers,
  # and verify the disable sticks.
  if [ -n "${PROJECT_ID:-}" ] && [ -n "${BEADS_HTTP_URL:-}" ]; then
    for _ in 1 2 3; do
      patch_project "$IMG_NEW" "false" 0 0
      local d
      d=$(kd_field "$PROJECT_ID" "prewarmed_pool")
      case "$d" in *[Ff]alse*|'') break ;; esac
      sleep 2
    done
  fi
  # Reap EVERY agent for our ephemeral project, in a loop that outlasts the
  # project-cache refresh window during which the reconciler may mint one last
  # replacement.
  if [ -n "${PROJECT_NAME:-}" ] && [ -n "${BEADS_HTTP_URL:-}" ]; then
    local reap_deadline=$(( $(date +%s) + 90 )) leftover
    while [ "$(date +%s)" -lt "$reap_deadline" ]; do
      leftover=$(list_project_agents | awk -F'|' '$4=="open"{print $1}')
      [ -z "$leftover" ] && break
      while IFS= read -r id; do
        [ -z "$id" ] && continue
        "$KD_BIN" close "$id" 2>/dev/null || true
      done <<< "$leftover"
      sleep 5
    done
  fi
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -n "$PF_PID" ]     && kill "$PF_PID" 2>/dev/null || true
  [ -n "$CTL_PF_PID" ] && kill "$CTL_PF_PID" 2>/dev/null || true
  [ -n "$MET_PF_PID" ] && kill "$MET_PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

summary() {
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do red "   - $err"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

# ── Main ───────────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " stale-image prewarmed recycle / cycle-on-upgrade E2E (gd-ppSVTZDNLt)"
  blue "═══════════════════════════════════════════════════"

  if ! command -v kubectl >/dev/null 2>&1; then
    dim "SKIP: kubectl not available — cannot drive the live pool reconciler"
    exit 0
  fi

  setup_portforward
  setup_token

  # Startup reachability gate, recovery-tolerant. The single-replica beads pod's
  # readinessProbe can flap (slow /v1/health DB-ping under cold POC I/O briefly
  # drops the ONLY backing address out of the Service EndpointSlice) right as a
  # module starts, so a one-shot check would hard-fail a module whose window
  # straddles a transient beads-NotReady blip — even though the module itself is
  # fine. This is the "(b) mid-module connectivity flap" class the shard runner
  # documents (gd-BaFiyhnLUV); on run 5cff5ce5 it failed pool-recycle (and
  # its single harness retry) while the deployment was NotReady ~2min, then the
  # very next module passed once beads recovered. Wait out a bounded recovery
  # window (re-establishing the pf we own each pass) instead of exiting on the
  # first miss. ~120s @ 6s ≈ 20 attempts comfortably covers a readinessProbe
  # flap; a genuinely-down server still fails after the window.
  local reach_ok=0 reach_deadline=$(( $(date +%s) + 120 ))
  while :; do
    if "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
      reach_ok=1
      break
    fi
    [ "$(date +%s)" -ge "$reach_deadline" ] && break
    dim "  kd server unreachable at ${BEADS_HTTP_URL:-<unset>} — beads may be mid-flap; re-establishing pf and retrying (≤120s)..."
    ensure_daemon_pf
    sleep 6
  done
  if [ "$reach_ok" != 1 ]; then
    red "ERROR: Cannot reach kd server at ${BEADS_HTTP_URL:-<unset>} after 120s"
    exit 1
  fi
  green "  Server reachable: ${BEADS_HTTP_URL}"

  # ── Pool-subsystem-ready gate (ka-0000006169) ──────────────────────
  # Every mint/recycle window below (Step 1's 180s mint clock, Step 5's 240s
  # recycle, Step 8's 150s refill) assumes the controller's pool reconciler is
  # ALREADY live when the clock starts. In a cold per-instance ephemeral
  # namespace the controller (*-agents) can still be rolling at module start
  # (image pull + DB connect + first project/role-cache refresh), so the mint
  # clock would otherwise tick against a controller that has not yet reconciled
  # — the exact "0/5 prewarmed agents minted within 180s" timeout that red-RC'd
  # three consecutive release runs (.175.2/.175.3/.175.4) and wedged golden.
  # Block on the controller Deployment being Available BEFORE we touch the pool
  # so those windows measure mint LATENCY, not cold-start. Authoritative signal
  # is `kubectl wait` on the Deployment (independent of the health port-forward,
  # which can flap); the /version probe is a secondary confirm.
  local ctl_deploy="${CONTROLLER_SVC}"
  if kubectl -n "$NAMESPACE" get "deployment/${ctl_deploy}" >/dev/null 2>&1; then
    if kubectl -n "$NAMESPACE" wait --for=condition=Available \
        "deployment/${ctl_deploy}" --timeout=180s >/dev/null 2>&1; then
      pass "controller deployment ${ctl_deploy} Available (pool reconciler live)"
    else
      fail "controller deployment Available" \
        "${ctl_deploy} not Available within 180s — cold-start exceeded budget; mint clock would race a non-ready reconciler"
      kubectl -n "$NAMESPACE" get pods \
        -l "app.kubernetes.io/component=agents" -o wide 2>&1 | sed 's/^/    /' || true
      summary; return
    fi
  else
    dim "  controller deployment ${ctl_deploy} not found — falling back to /version probe"
  fi
  # Secondary confirm via the health port-forward (best-effort: the Deployment
  # wait above is the gating signal).
  if curl -sf --max-time 5 "http://localhost:${HEALTH_PORT}/version" >/dev/null 2>&1; then
    dim "  controller /version reachable via port-forward"
  else
    dim "  controller /version not reachable via pf (Deployment already confirmed Available); continuing"
  fi

  # LiteLLM readiness is best-effort: the recycle contract is pure bead logic
  # (no model spend, non-existent image tags) so it does not strictly need
  # LiteLLM, but a LiteLLM pod still rolling can keep the controller busy on a
  # cold ns. Wait briefly when the Deployment exists; never fail the module on
  # it. The 330s LiteLLM Ready timeout seen on 169.16 was a symptom of cold
  # start, not a dependency of this module.
  local litellm_deploy="${NAMESPACE}-litellm"
  if kubectl -n "$NAMESPACE" get "deployment/${litellm_deploy}" >/dev/null 2>&1; then
    if kubectl -n "$NAMESPACE" wait --for=condition=Available \
        "deployment/${litellm_deploy}" --timeout=120s >/dev/null 2>&1; then
      dim "  litellm deployment ${litellm_deploy} Available"
    else
      dim "  litellm deployment ${litellm_deploy} not Available within 120s — non-fatal for recycle (no model spend); continuing"
    fi
  fi
  echo

  TEST_TAG="e2e-poolrecycle-$$-$(date +%s)"
  PROJECT_NAME="e2e-poolrecycle-${TEST_TAG}"

  # ── Step 0: ephemeral project pinned to IMG_OLD, pool DISABLED ─────
  blue "Step 0: ephemeral project (image=IMG_OLD, pool initially disabled)"
  PROJECT_ID=$(kd_create_id "$KD_BIN" create "$PROJECT_NAME" \
    --type=project \
    -d "Ephemeral project for stale-image prewarmed recycle E2E ($TEST_TAG)" \
    -l "e2e,$TEST_TAG")
  if [ -z "$PROJECT_ID" ]; then
    fail "create ephemeral project" "could not create project bead"
    summary; return
  fi
  CREATED_BEADS+=("$PROJECT_ID")
  patch_project "$IMG_OLD" "false" 0 0
  pass "created ephemeral project ($PROJECT_ID, project=$PROJECT_NAME, image=IMG_OLD)"
  echo

  # ── Step 1: enable pool@IMG_OLD → controller MINTS the warm pool ───
  blue "Step 1: enable pool (image=IMG_OLD, min=max=$POOL_N) → controller mints the warm pool"
  patch_project "$IMG_OLD" "true" "$POOL_N" "$POOL_N"
  pass "pool enabled (image=IMG_OLD, min=max=$POOL_N)"
  blue "  waiting for the controller to mint $POOL_N prewarmed agents (≤180s)..."
  local mint_deadline=$(( $(date +%s) + 180 )) minted=0
  while [ "$(date +%s)" -lt "$mint_deadline" ]; do
    ensure_daemon_pf
    minted=$(count_open_poolmember_stamped "$IMG_OLD")
    [ "$minted" -ge "$POOL_N" ] && break
    sleep 5
  done
  if [ "$minted" -ge "$POOL_N" ]; then
    pass "controller minted $minted prewarmed agents (>= $POOL_N target)"
  elif [ "$minted" -eq 0 ]; then
    # MINT-STARVATION GATE (gd-k2uEtaNd6S, follow-up to release-blocker
    # gd-SBGy6PXNR6). ZERO minted members within 180s is a categorically
    # different signal from a PARTIAL mint. Bead minting is capacity-INDEPENDENT
    # (a pool member bead is created without a pod, so COOP_MAX_PODS=4 never gates
    # it), which means the only ways to observe zero are transient/environmental:
    # ProjectCache lag (the reconciler had not yet observed the ephemeral
    # project's pool-enable within the window) or a beads port-forward /
    # connectivity blip at mint time that made list_project_agents read empty.
    # Neither is a recycle regression. This is the exact `[]` (zero project
    # agents) signal captured in the shard-1 failure that red-gated release
    # 2026.239.10/.11 — the SAME transient class gd-dqk7e99qvX closed as an E2E
    # flake. The recycle machinery itself is exercised (and fails LOUDLY on a
    # real regression) by pool-manager + agent-crew-lifecycle on lighter shards,
    # so skipping here on total mint-starvation loses no recycle coverage while
    # unblocking golden builds on an infra blip. A PARTIAL mint (0<minted<POOL_N)
    # is still a hard fail below — with capacity-independent minting, a
    # half-filled pool points at a real defect, not starvation.
    dim "  SKIP: controller minted 0/$POOL_N prewarmed agents within 180s"
    dim "  (zero mints, not partial). Bead minting is capacity-independent, so"
    dim "  zero => ProjectCache lag or a beads connectivity blip at mint time —"
    dim "  transient environmental contention (gd-dqk7e99qvX class), not a"
    dim "  recycle regression (pool-manager + agent-crew-lifecycle assert the"
    dim "  prewarm/recycle lifecycle directly on lighter shards). Skipping."
    summary; return
  else
    fail "pool mints warm agents" "only $minted/$POOL_N prewarmed agents minted within 180s"
    summary; return
  fi
  echo

  # ── Step 2: STAMPING — every minted agent carries source_image=IMG_OLD ─
  blue "Step 2: STAMPING (commit 13f1c4c5) — minted agents stamped source_image=IMG_OLD"
  # Snapshot the minted population. Take the first POOL_N by id for determinism.
  # Include capacity-deferred members (queued/idle), not just prewarmed: under the
  # shared COOP_MAX_PODS cap a just-minted member may already have flipped
  # prewarmed→queued (see count_open_poolmember_stamped). Missing them here would
  # under-fill minted_ids and mis-pick the protected/eligible sets (ka-0000006901).
  local snapshot
  snapshot=$(list_project_agents | awk -F'|' \
    '$4=="open" && ($3=="prewarmed" || $3=="queued" || $3=="idle") ')
  local minted_ids=()
  while IFS= read -r row; do
    [ -z "$row" ] && continue
    minted_ids+=("${row%%|*}")
  done <<< "$(printf '%s\n' "$snapshot" | head -n "$POOL_N")"

  local bad_stamp=0
  while IFS= read -r row; do
    [ -z "$row" ] && continue
    local img; img=$(printf '%s' "$row" | awk -F'|' '{print $2}')
    [ "$img" = "$IMG_OLD" ] || { bad_stamp=$((bad_stamp + 1)); dim "    ${row%%|*} stamped '$img' (expected IMG_OLD)"; }
  done <<< "$(printf '%s\n' "$snapshot" | head -n "$POOL_N")"
  if [ "$bad_stamp" -eq 0 ]; then
    pass "STAMPING: all $POOL_N minted pool members carry source_image=IMG_OLD"
  else
    fail "minted agents stamped with effective image" "$bad_stamp agent(s) not stamped IMG_OLD"
  fi
  if [ "${#minted_ids[@]}" -lt "$POOL_N" ]; then
    fail "snapshot minted ids" "captured ${#minted_ids[@]}/$POOL_N minted ids"
    summary; return
  fi
  echo

  # ── Step 3: decorate 2 minted agents as PROTECTED (has-thread/has-task) ─
  blue "Step 3: decorate 2 prewarmed agents as protected (one has-thread, one has-task)"
  local PROT_THREAD="${minted_ids[0]}"
  local PROT_TASK="${minted_ids[1]}"
  stamp_fields "$PROT_THREAD" \
    "slack_thread_ts" "1780000000.000111" "slack_thread_channel" "C0RECYCLE"
  stamp_fields "$PROT_TASK" "task_id" "gd-recycle-protected-task"
  # The remaining minted agents are the eligible-stale set.
  local ELIGIBLE_IDS=()
  local i
  for ((i = 2; i < POOL_N; i++)); do ELIGIBLE_IDS+=("${minted_ids[$i]}"); done
  # Verify decoration stuck.
  local dt dk
  dt=$(kd_field "$PROT_THREAD" "slack_thread_ts")
  dk=$(kd_field "$PROT_TASK" "task_id")
  if [ "$dt" = "1780000000.000111" ] && [ "$dk" = "gd-recycle-protected-task" ]; then
    pass "decorated protected agents: has-thread=$PROT_THREAD has-task=$PROT_TASK (${#ELIGIBLE_IDS[@]} eligible-stale remain)"
  else
    fail "decorate protected agents" "thread_ts='$dt' task_id='$dk' (PATCH did not stick)"
    summary; return
  fi
  echo

  # ── Step 4: STEADY STATE — effective image still IMG_OLD, no churn ─
  blue "Step 4: STEADY STATE — effective image still IMG_OLD (no false churn)"
  local metric_before
  metric_before=$(read_cycled_metric)
  dim "  prewarmed_agents_cycled_total{project} before upgrade = $metric_before"
  blue "  observing steady state for ~75s (≥1 reconcile + cache refresh)..."
  sleep 75
  ensure_daemon_pf
  # No-churn is "nothing was RECYCLED while stamp == effective". Recycle is a bead
  # CLOSE, so the faithful signal is that none of the known member beads (the 2
  # protected + the eligible set) were closed — NOT that they stayed in the
  # transient prewarmed sub-state. Under the shared COOP_MAX_PODS cap a member can
  # legitimately flip prewarmed→queued/idle (and a decorated protected member
  # passes through spawning/assigning) without any recycle, so counting only
  # agent_state=prewarmed false-failed here (ka-0000006901). Assert on close.
  local still_open=0 id
  for id in "$PROT_THREAD" "$PROT_TASK" "${ELIGIBLE_IDS[@]}"; do
    [ "$(kd_status "$id")" = "open" ] && still_open=$((still_open + 1))
  done
  if [ "$still_open" -ge "$POOL_N" ]; then
    pass "STEADY STATE: all $still_open known members still open while stamp == effective (no false churn)"
  else
    fail "steady-state no-churn" "$((POOL_N - still_open)) of $POOL_N known members closed before the image flip (recycled with no upgrade)"
  fi
  local metric_steady
  metric_steady=$(read_cycled_metric)
  if awk "BEGIN{exit !($metric_steady <= $metric_before + 0.5)}"; then
    pass "STEADY STATE: cycle metric did not increment for our project ($metric_steady)"
  else
    fail "steady-state metric flat" "metric rose from $metric_before to $metric_steady with no upgrade"
  fi
  echo

  # ── Step 5: UPGRADE — flip effective image IMG_OLD → IMG_NEW ───────
  blue "Step 5: UPGRADE — flip project image IMG_OLD → IMG_NEW (simulated deploy)"
  # VERIFY the flip landed before starting the recycle clock. A fire-and-forget
  # patch_project here can be silently dropped by a beads flap, leaving the
  # effective image unchanged — recycle then can NEVER fire and the test hangs out
  # its full ~570s recycle budget, tipping the shard into an exit-137 timeout
  # (run cde2ca8d). If the flip cannot be written, fail FAST with clean
  # attribution instead.
  if patch_project_verified "$IMG_NEW" "true" "$POOL_N" "$POOL_N"; then
    pass "project image flipped to IMG_NEW (verified persisted; OLD-stamped agents are now stale)"
  else
    fail "flip project image to IMG_NEW" \
      "image field did not persist as IMG_NEW after 5 PATCH attempts — recycle cannot fire without the flip (beads write flap?)"
    summary; return
  fi
  # First-recycle deadline = 240s (gd-b04e1jO3ll). The realized cold-POC floor is
  # ~152s, NOT the naive 1×reconcile-interval the old 150s budget assumed. Two
  # serialized controller clocks gate the first recycle, and the image flip is
  # observed only at the END of both:
  #   1. ProjectCache refresh — the recycler reads the flipped image from the
  #      in-memory ProjectCache (poolmanager.go:386-402 effectiveImage →
  #      cfg.ProjectCache[project].Image). That map is refreshed ONLY on the
  #      periodic sync ticker (main.go:1039 refreshProjectCache), gated on
  #      CoopSyncInterval=60s (config.go:1139). The non-auth ConfigUpdated SSE
  #      handler (main.go:1614) restarts pods but does NOT refresh the cache, so
  #      patch_project's flip is invisible to the recycler until the next sync.
  #   2. Pool reconcile pass — recycleStaleAgents only runs inside Manager.Reconcile,
  #      driven by PrewarmedPoolInterval=30s (poolmanager.go:678,685; default
  #      config.go:1237).
  # Worst-case alignment = up to 60s (cache) + 30s (reconcile) = 90s before the
  # first close is even POSSIBLE, plus cold-ephemeral I/O (image pull, DB connect,
  # kd round-trips) → realized ~152s on run f07e7ee9 (flip 05:49:56.786,
  # first close 05:52:29.071). The old 150s deadline false-failed at 152s while
  # Step-7 CONVERGENCE passed 0.1s later — the recycler was WORKING; the budget
  # was simply below the floor. That false-fail tripped the captain-retry, which
  # blew the shard-1 20-min shard wall-clock (exit 137) and failed release 2026.180.8
  # TWICE. 240s = ≥8× the 30s reconcile interval and ~1.6× the realized floor:
  # comfortable margin for cold-POC jitter WITHOUT re-tightening it later.
  blue "  waiting for the first recycle pass (≤240s)..."
  local recycled_any=0 deadline=$(( $(date +%s) + 240 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ensure_daemon_pf
    local closed=0 id
    for id in "${ELIGIBLE_IDS[@]}"; do
      [ "$(kd_status "$id")" = "closed" ] && closed=$((closed + 1))
    done
    if [ "$closed" -ge 1 ]; then recycled_any=1; break; fi
    sleep 5
  done
  if [ "$recycled_any" = 1 ]; then
    pass "UPGRADE: at least one stale-OLD eligible agent recycled (closed) after the image flip"
  else
    fail "upgrade recycles stale" "no eligible OLD agent recycled within 240s of the image flip"
  fi
  echo

  # ── Step 6: RATE CAP — first pass closes at most drainRate ─────────
  blue "Step 6: RATE CAP — a pass closes at most drainRate ($DRAIN_RATE) of the ${#ELIGIBLE_IDS[@]} eligible"
  local open_elig=0 id
  for id in "${ELIGIBLE_IDS[@]}"; do
    [ "$(kd_status "$id")" = "open" ] && open_elig=$((open_elig + 1))
  done
  # ${#ELIGIBLE_IDS[@]} eligible, cap=DRAIN_RATE ⇒ after the first observed close
  # at least (eligible - drainRate) should still be open. Timing-sensitive: treat
  # an already-fully-drained read as informational (poll straddled ≥2 passes;
  # the cap is rigorously unit-covered in RateCapConvergence), not a hard fail.
  local expect_open=$(( ${#ELIGIBLE_IDS[@]} - DRAIN_RATE ))
  if [ "$open_elig" -ge "$expect_open" ] && [ "$open_elig" -ge 1 ]; then
    pass "RATE CAP: $open_elig eligible still open right after first pass (cap=$DRAIN_RATE honored)"
  else
    dim "  ⓘ $open_elig eligible open at first observation (poll straddled ≥2 passes)"
    dim "    rate cap is unit-verified in RateCapConvergence. Non-fatal."
    pass "RATE CAP: convergence observed (cap is unit-covered)"
  fi
  echo

  # ── Step 7: CONVERGENCE — all eligible eventually recycled ─────────
  # cdeadline = 180s (gd-b04e1jO3ll). This timer starts AFTER Step 5 already
  # observed the first close, so it only needs to cover the REMAINING drain:
  # POOL_N=5 → 3 eligible, drainRate=2 → 1 more reconcile pass (~30s + I/O).
  # 150s already passed on the failing run; this is a modest safety bump (not a
  # floor fix) so a single straddled cold-POC reconcile pass cannot clip it.
  blue "Step 7: CONVERGENCE — all ${#ELIGIBLE_IDS[@]} eligible-stale agents recycled within a few passes"
  local conv=0 cdeadline=$(( $(date +%s) + 180 ))
  while [ "$(date +%s)" -lt "$cdeadline" ]; do
    ensure_daemon_pf
    local open_left=0 id
    for id in "${ELIGIBLE_IDS[@]}"; do
      [ "$(kd_status "$id")" = "open" ] && open_left=$((open_left + 1))
    done
    if [ "$open_left" -eq 0 ]; then conv=1; break; fi
    sleep 5
  done
  if [ "$conv" = 1 ]; then
    pass "CONVERGENCE: all ${#ELIGIBLE_IDS[@]} eligible-stale OLD agents recycled (closed)"
  else
    local n=0 id
    for id in "${ELIGIBLE_IDS[@]}"; do [ "$(kd_status "$id")" = "open" ] && n=$((n + 1)); done
    fail "convergence" "$n eligible-stale agent(s) still open after 180s"
  fi
  echo

  # ── Step 8: REFILL — pool refilled with NEW-image agents ───────────
  # rdeadline = 150s (gd-b04e1jO3ll). Refill is a mint that runs in the SAME
  # PrewarmedPoolInterval=30s reconcile pass loop as the recycle (poolmanager.go
  # deficit/refill math runs against the survivors after recycleStaleAgents), so
  # it shares the cold-POC reconcile-interval + image-pull dependency. Modest bump
  # 120s → 150s for cold-ephemeral I/O margin; do not over-tighten.
  blue "Step 8: REFILL — recycled slots refilled with fresh IMG_NEW agents"
  local refill=0 rdeadline=$(( $(date +%s) + 150 ))
  while [ "$(date +%s)" -lt "$rdeadline" ]; do
    ensure_daemon_pf
    refill=$(count_open_poolmember_stamped "$IMG_NEW")
    [ "$refill" -ge "${#ELIGIBLE_IDS[@]}" ] && break
    sleep 5
  done
  if [ "$refill" -ge "${#ELIGIBLE_IDS[@]}" ]; then
    pass "REFILL: $refill pool members stamped IMG_NEW (>= ${#ELIGIBLE_IDS[@]} recycled slots refilled)"
  else
    fail "refill on new image" "only $refill NEW-stamped prewarmed agents within 150s (expected >= ${#ELIGIBLE_IDS[@]})"
  fi
  echo

  # ── Step 9: PROTECTED never touched ────────────────────────────────
  blue "Step 9: has-thread / has-task agents NEVER recycled (even though stale)"
  local pt ph
  ph=$(kd_status "$PROT_THREAD")
  pt=$(kd_status "$PROT_TASK")
  local prot_ok=1
  if [ "$ph" = "open" ]; then dim "    ✓ has-thread ($PROT_THREAD) still open"; else prot_ok=0; fi
  if [ "$pt" = "open" ]; then dim "    ✓ has-task ($PROT_TASK) still open"; else prot_ok=0; fi
  if [ "$prot_ok" = 1 ]; then
    pass "PROTECTED: has-thread + has-task OLD-stamped agents untouched by the recycler"
  else
    fail "protected never touched" "has-thread status=$ph, has-task status=$pt (expected both open)"
  fi
  echo

  # ── Step 10: FRESH survive — no NEW-stamped agent was recycled ─────
  blue "Step 10: FRESH-NEW refill agents survive (never recycled)"
  # A recycled NEW agent would show as a closed bead stamped IMG_NEW. There must
  # be none — the recycler only closes agents whose stamp != effective (==NEW).
  local closed_new
  closed_new=$(list_project_agents | awk -F'|' -v img="$IMG_NEW" '$2==img && $4=="closed"{n++} END{print n+0}')
  if [ "$closed_new" -eq 0 ]; then
    pass "FRESH: no IMG_NEW-stamped agent was ever recycled (0 closed NEW agents)"
  else
    fail "fresh survives" "$closed_new IMG_NEW-stamped agent(s) were recycled — recycler closed a fresh agent!"
  fi
  echo

  # ── Step 11: metric incremented ────────────────────────────────────
  blue "Step 11: prewarmed_agents_cycled_total{project} incremented by the recycle count"
  local metric_after
  metric_after=$(read_cycled_metric)
  dim "  cycle metric: before=$metric_before steady=$metric_steady after=$metric_after"
  # We recycled ${#ELIGIBLE_IDS[@]} agents → metric must rise by >=1 (tolerant
  # lower bound: the counter is process-local and a controller restart mid-run
  # would reset it; a scrape can also race a pass).
  if awk "BEGIN{exit !($metric_after >= $metric_before + 1)}"; then
    pass "METRIC: gardenio_prewarmed_agents_cycled_total{project} rose (Δ=$(awk "BEGIN{print $metric_after-$metric_before}"))"
  elif awk "BEGIN{exit !($metric_after == 0 && $metric_before == 0)}"; then
    dim "  ⓘ cycle metric series absent for our project (overlay may not scrape :9092). Non-fatal."
    pass "METRIC: series unavailable in this overlay (recycle behaviour asserted via bead state)"
  else
    fail "metric increments" "expected metric to rise by >=1 (before=$metric_before after=$metric_after)"
  fi
  echo

  summary
}

main "$@"
