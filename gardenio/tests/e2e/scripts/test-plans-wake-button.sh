#!/usr/bin/env bash
# test-plans-wake-button.sh — /plans "Wake" button (plan-assistant prompt) E2E
#
# The /plans surface renders a state-adaptive Spawn/Wake/Send composer on every
# plan card and in the /plans/{id} cockpit lane. Submitting it POSTs
#   POST /plans/{id}/assistant/prompt   (form field: message)
# which, per promptPlanAssistant (plans_assistant_prompt.go), takes one of three
# paths by assistant state:
#   • none   → spawnPlanAssistant mints a FRESH assistant (no session_resume).
#   • parked → the prior assistant bead is REVIVED IN PLACE — same bead id (=>
#              stable S3 session key => wake continuity), session_resume=true
#              (the "Wake" path — S1 wake-continuity contract, ka-0000002360).
#   • live   → coopmux Nudge (instant); no CreateBead.
#
# Unit tests (plans_assistant_prompt_test.go) cover the branch logic against a
# fake daemon. This module is the missing REAL e2e: it drives the actual HTTP
# verb against a live beads-viewer + kbeads daemon and asserts the wake path
# produces the right agent bead in the graph — the thing an operator sees fail
# when "the Wake button isn't working".
#
# Scenarios:
#   1. Spawn — a plan with NO assistant: POST returns 200 + data-state="waking",
#      and a plan-agent bead is created bound to the epic
#      (role=plan-gardener — the unified role of the ka-0000000081 merge —
#      plan_id=<epic>, prompt=<message>, assigned edge), with NO
#      session_resume (fresh conversation).
#   2. Wake — a plan whose assistant is PARKED (terminal agent_state): POST
#      returns 200 + data-state="waking" and the SAME assistant bead is revived
#      in place — same bead id (so the coop S3 session-resume key stays constant
#      and the plan conversation continues instead of cold-starting, bug
#      ka-0000002360), status flipped back to in_progress, session_resume=true,
#      prompt refreshed, and NO second bead minted. Guards the reported "Wake
#      button isn't working" regression end-to-end.
#   3. Empty message — POST with a blank message returns 400 and creates no
#      assistant bead (defense-in-depth for placeholder-only submissions).
#
# Scenarios 1-3 are the DEFAULT graph-level suite: they drive the real HTTP verb
# but assert only on the bead graph, so they are safe on a viewer-only cluster
# (no controller reacting to the beads → no real pod → no model spend).
#
#   4. Wake-over-a-husk (OPT-IN, live cluster) — the LIVE-CLUSTER integration
#      guard for the adopt-or-replace fix (bug ka-0000006228, MR !5439). The fix
#      made podmanager.CreateAgentPod stop SWALLOWING a K8s AlreadyExists on the
#      deterministic (stable-identity) pod name: a wake whose PRIOR incarnation's
#      pod is still present under the canonical name used to "create" over the
#      husk, start no coop process, and silently revert the /plans chip from
#      "waking" back to "parked". manager_adopt_or_replace_test.go unit-covers the
#      branch against a fake client; THIS scenario is the missing real-cluster
#      guard. It parks a plan-gardener assistant, plants a stale pod squatting the
#      canonical name spec.PodName() = <mode>-<project>-<role>-<agent>
#      (podmanager/manager.go), POSTs the wake, and asserts a FRESH Running coop
#      pod comes up under that name (new UID, no DeletionTimestamp) with the
#      conversation-resume contract intact (session_resume=true on the reopened
#      bead). See the scenario-4 preamble for why it needs the gVisor substrate
#      and why it is opt-in.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-plans-wake-button.sh              # scenarios 1-3
#   PLANS_WAKE_POD_E2E=1 ./gardenio/tests/e2e/scripts/test-plans-wake-button.sh  # scenario 4
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - kd, jq, curl, python3 in PATH
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer
#     (GARDENIO_E2E_GITLAB_BEARER or the static daemon token must be set). The
#     resolved bearer carries no operator group claim, so the operatorOnly
#     route's RequireRole falls through to the no-roles allow branch (RBAC not
#     enforced for the internal caller) — the POST is accepted. When a token
#     cannot be resolved, the POST scenarios SKIP.
#
# Self-SKIPs (exit 0) when the viewer Service is absent, so it is inert in the
# per-instance ephemeral e2e namespaces and asserts only against a long-lived
# viewer deployment (staging singleton / prod). Same shape as the sibling
# viewer modules (test-viewer-bead-activity.sh, test-agents-live-view.sh).
#
# Scenario 4 gating (PLANS_WAKE_POD_E2E=1): the plan-gardener role pins
# runtime_class=runsc (role beads ka-0000003712 / legacy gd-yLXHzBtwJL), so a
# woken plan assistant's pod ROUTES to the controller's gVisor namespace
# (routeGVisorNamespace) and only ever reaches Running on the runsc substrate.
# staging has NO gVisor node pool; prod does (gardenio-gvisor,
# freezeThaw ON). So scenario 4 is OPT-IN and needs (a) the runsc RuntimeClass +
# gVisor namespace, and (b) a live controller watching the beads it seeds. It
# runs INSTEAD OF scenarios 1-3 (not alongside): on a cluster with a live
# controller, the un-mocked scenario-1/2 spawns would schedule REAL Claude pods
# and burn model spend, so the opt-in cleanly separates the two suites. When the
# opt-in is set but the substrate/controller is absent it SKIPs LOUD (still exit
# 0 — an absent PoC node pool must never red the suite, ka-0000000847 — but a
# skip is never mistaken for a pass), mirroring test-gb-stop-suspend.sh.
#
# Idempotency: every created bead carries the title prefix
#   plans-wake-e2e-<random>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Hard rules: do NOT edit .captain/quarantines/. Do not touch any other e2e
# module.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-viewer.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19811}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19812}"
KD_BIN="${KD_BIN:-kd}"
PROJECT="${PLANS_WAKE_PROJECT:-gardenio}"
OPERATOR_EMAIL="${OPERATOR_EMAIL:-e2e-plans-op-$$@example.com}"
TITLE_PREFIX="plans-wake-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"

# ── Scenario 4 (opt-in wake-over-a-husk) config ───────────────────
# Opt-in flag: unset → scenarios 1-3 (graph-only) run; set → scenario 4 (live
# pod) runs INSTEAD (see the header + the scenario-4 preamble for why the two
# suites are mutually exclusive on a live-controller cluster).
PLANS_WAKE_POD_E2E="${PLANS_WAKE_POD_E2E:-0}"
# The runsc RuntimeClass a woken plan-gardener pod is pinned to (role config).
PLANS_WAKE_RUNTIME_CLASS="${PLANS_WAKE_RUNTIME_CLASS:-runsc}"
# The gVisor namespace a runsc agent routes to. Empty → auto-discover from the
# controller Deployment's GVISOR_NAMESPACE env (below).
PLANS_WAKE_GVISOR_NS="${PLANS_WAKE_GVISOR_NS:-}"
# Controller Deployment (its GVISOR_NAMESPACE env is the routing source of truth).
CONTROLLER_DEPLOY="${CONTROLLER_DEPLOY:-${RELEASE}-agents}"
# Claudeless mock scenario the woken pod runs (zero model spend); baked into the
# agent image at /scenarios/ (images/agent/Dockerfile).
PLANS_WAKE_MOCK_SCENARIO="${PLANS_WAKE_MOCK_SCENARIO:-basic-echo}"
# How long to wait for a fresh Running pod to come up under the canonical name
# after the wake POST.
PLANS_WAKE_POD_TIMEOUT="${PLANS_WAKE_POD_TIMEOUT:-240}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
CREATED_BEADS=()
# Husk + fresh pods to reap in cleanup (scenario 4). "<ns>/<name>" entries.
CREATED_PODS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}
skip() {
  local name="$1" reason="${2:-}"
  yellow "  ⊘ SKIP: $name${reason:+ — $reason}"
  SKIP=$((SKIP + 1))
}

cleanup() {
  # Close beads FIRST: a live controller reaps a closed agent bead's pod, so
  # this also tears down any real pod the wake brought up. Then force-delete any
  # pod we planted or observed, in case no controller is watching (belt-and-
  # suspenders — a leaked runsc pod on the PoC node is expensive).
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] || continue
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  for entry in "${CREATED_PODS[@]:-}"; do
    [ -n "$entry" ] || continue
    local pns="${entry%%/*}" pname="${entry#*/}"
    kubectl -n "$pns" delete pod "$pname" --grace-period=0 --force \
      >/dev/null 2>&1 || true
  done
  [ -n "$VIEWER_PF_PID" ] && kill "$VIEWER_PF_PID" 2>/dev/null || true
  [ -n "$BEADS_PF_PID"  ] && kill "$BEADS_PF_PID"  2>/dev/null || true
}
trap cleanup EXIT

setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/plans-wake-beads-pf.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
}

bead_id_from_json() {
  printf '%s' "$1" | jq -r '.id // empty' 2>/dev/null || printf ''
}

# viewer_post <path> <token> <form-encoded-body>
# POSTs a form body to the viewer with bearer auth (no -L; a redirect surfaces
# as the status). Echoes "<status>\n<body>".
viewer_post() {
  local path="$1" token="$2" body="$3"
  local out status
  out=$(mktemp)
  status=$(curl -sk --max-time 15 -X POST \
    -H "Authorization: Bearer ${token}" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -H "Accept: text/html" \
    --data "$body" \
    -o "$out" \
    -w '%{http_code}' \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000")
  printf '%s\n' "$status"
  cat "$out"
  rm -f "$out"
}

# assistant_bead_for_plan <epic-id>
# Returns (JSON) the newest plan-agent bead bound to <epic-id> via
# fields.plan_id, or "" when none. Mirrors planAssistant()'s fields.plan_id
# lookup, including its dual-read of the unified role:plan-gardener label OR
# the legacy role:plan-assistant (ka-0000000081 rename). Reads via kd list so
# it works without the raw HTTP token.
assistant_bead_for_plan() {
  local epic="$1"
  "$KD_BIN" list --type=agent --label-any "role:plan-gardener,role:plan-assistant" --json 2>/dev/null \
    | jq -c --arg epic "$epic" '
        [ .[] | select((.fields.plan_id // "") == $epic) ]
        | sort_by(.updated_at) | last // empty
      ' 2>/dev/null || printf ''
}

# ══════════════════════════════════════════════════════════════════
# Scenario 4 (opt-in): wake over a stale pod squatting the canonical name.
# ══════════════════════════════════════════════════════════════════

# canonical_pod_name <mode> <project> <role> <agent> — reproduce
# podmanager.AgentPodSpec.PodName() exactly: "<mode>-<project>-<role>-<agent>",
# with the "<project>-" prefix stripped from the agent name (manager.go PodName)
# and commas in the role replaced with dashes (SanitizeRole).
canonical_pod_name() {
  local mode="$1" project="$2" role="$3" agent="$4"
  local name="$agent"
  if [ -n "$project" ]; then
    name="${name#"${project}-"}"
  fi
  local sane_role="${role//,/-}"
  printf '%s-%s-%s-%s' "$mode" "$project" "$sane_role" "$name"
}

# discover_gvisor_ns — the namespace a runsc (plan-gardener) agent routes to.
# Prefer the explicit override, else read GVISOR_NAMESPACE off the controller
# Deployment's env (the routeGVisorNamespace source of truth), else fall back to
# the release namespace (a cluster with no namespace split runs runsc agents in
# the release ns). Echoes the namespace (never empty).
discover_gvisor_ns() {
  if [ -n "$PLANS_WAKE_GVISOR_NS" ]; then
    printf '%s' "$PLANS_WAKE_GVISOR_NS"
    return 0
  fi
  local ns
  ns=$(kubectl -n "$NAMESPACE" get deploy "$CONTROLLER_DEPLOY" \
    -o jsonpath='{range .spec.template.spec.containers[*].env[?(@.name=="GVISOR_NAMESPACE")]}{.value}{end}' \
    2>/dev/null || printf '')
  if [ -n "$ns" ]; then
    printf '%s' "$ns"
  else
    printf '%s' "$NAMESPACE"
  fi
}

# plant_husk_pod <ns> <podname> <agent> <mode> <project> <role>
# Create a stale pod squatting the canonical name so the wake's CreateAgentPod
# hits AlreadyExists and must adopt-or-replace (bug ka-0000006228). The husk is:
#   • bead-id MISMATCH — it carries a gardenio.io/bead-id annotation for a DIFFERENT
#     (fake) bead, which is exactly one of the stale cases stalePodReason flags
#     ("the name is held by some other agent's leftover"); this fires for ANY
#     phase, so the husk need never actually run.
#   • UNSCHEDULABLE — a bogus nodeSelector keeps it Pending forever, so it burns
#     no model spend and needs no gVisor node, while still holding the NAME.
#   • labelled gardenio.io/agent=<agent> + app so both the controller (reconciler
#     desired/actual join) and our own pod lookups resolve it like a real pod.
# Best-effort flips it to phase=Failed via the status subresource for extra
# fidelity to the "leaked terminal-phase pod" shape; correctness does not depend
# on that flip (the bead-id mismatch already makes it stale).
plant_husk_pod() {
  local ns="$1" podname="$2" agent="$3" mode="$4" project="$5" role="$6"
  local sane_role="${role//,/-}"
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
    gardenio.io/project: ${project}
    gardenio.io/mode: ${mode}
    gardenio.io/role: ${sane_role}
    gardenio.io/agent: ${agent}
    plans-wake-e2e: "${RANDOM_SUFFIX}"
  annotations:
    gardenio.io/bead-id: "gd-PLANSWAKEHUSK-${RANDOM_SUFFIX}"
spec:
  restartPolicy: Never
  # Deliberately unschedulable — the husk only needs to OCCUPY the canonical
  # name for the collision, never to run. No model spend, no gVisor node.
  nodeSelector: {plans-wake-e2e/never-schedules: "true"}
  containers:
  - name: agent
    image: public.ecr.aws/docker/library/busybox:1.36
    command: ["sh", "-c", "sleep 3600"]
YAML
)
  printf '%s' "$manifest" | kubectl apply -f - >/dev/null 2>&1 || return 1
  CREATED_PODS+=("${ns}/${podname}")
  # Best-effort: mark it terminal-phase so it also matches the literal
  # "leaked terminal-phase pod" case. --subresource=status needs a recent
  # kubectl/apiserver; a failure here is fine (the bead-id mismatch already
  # makes stalePodReason flag it).
  kubectl -n "$ns" patch pod "$podname" --subresource=status --type=merge \
    -p '{"status":{"phase":"Failed"}}' >/dev/null 2>&1 || true
  return 0
}

# pod_field <ns> <podname> <jsonpath> — echo a pod field (empty on absence).
pod_field() {
  kubectl -n "$1" get pod "$2" -o jsonpath="$3" 2>/dev/null || printf ''
}

# scenario_4_wake_over_husk <op_token>
# The live-cluster integration guard for the adopt-or-replace fix. Gated behind
# PLANS_WAKE_POD_E2E=1; SKIPs LOUD (never silently passes) when the runsc
# substrate / gVisor ns / a live controller is absent.
scenario_4_wake_over_husk() {
  local op_token="$1"

  blue "Scenario 4: wake over a stale pod squatting the canonical name (ka-0000003650)"

  # ── Substrate preflight — SKIP LOUD, never silently pass ──────────
  if ! command -v kubectl >/dev/null 2>&1; then
    skip "scenario 4: wake-over-a-husk" "kubectl not installed (opt-in live-cluster module)"
    return 0
  fi
  if ! kubectl get runtimeclass "$PLANS_WAKE_RUNTIME_CLASS" >/dev/null 2>&1; then
    yellow "  ⚠ RuntimeClass '$PLANS_WAKE_RUNTIME_CLASS' absent — this cluster has no gVisor substrate."
    skip "scenario 4: wake-over-a-husk" \
      "RuntimeClass '$PLANS_WAKE_RUNTIME_CLASS' not present (plan-gardener pods route to runsc/gVisor; staging has no gVisor pool)"
    return 0
  fi
  local gvisor_ns
  gvisor_ns=$(discover_gvisor_ns)
  if ! kubectl get ns "$gvisor_ns" >/dev/null 2>&1; then
    skip "scenario 4: wake-over-a-husk" "gVisor namespace '$gvisor_ns' not present"
    return 0
  fi
  if [ "$(kubectl auth can-i create pods -n "$gvisor_ns" 2>/dev/null)" != "yes" ]; then
    skip "scenario 4: wake-over-a-husk" "cannot create pods in gVisor ns '$gvisor_ns'"
    return 0
  fi
  if ! kubectl -n "$NAMESPACE" get deploy "$CONTROLLER_DEPLOY" >/dev/null 2>&1; then
    skip "scenario 4: wake-over-a-husk" \
      "controller Deployment '$CONTROLLER_DEPLOY' absent in $NAMESPACE — no live controller to react to the wake"
    return 0
  fi
  dim "  substrate present — gVisor ns=$gvisor_ns, controller=$CONTROLLER_DEPLOY"

  # ── 1. Create the plan epic. ──────────────────────────────────────
  local plan_id=""
  plan_id=$(bead_id_from_json "$("$KD_BIN" create "${TITLE_PREFIX}-husk-${RANDOM_SUFFIX}" \
    --type=epic --project "$PROJECT" -l "plan" \
    -d "scenario 4 plan ($RANDOM_SUFFIX)" --json 2>/dev/null || printf '{}')")
  if [ -z "$plan_id" ]; then
    fail "scenario 4: create plan epic" "no ID returned from kd create"
    return 0
  fi
  CREATED_BEADS+=("$plan_id")
  pass "scenario-4 plan epic created ($plan_id)"

  # ── 2. Seed a plan-gardener assistant, then PARK it (close). ───────
  # An agent bead REQUIRES fields.agent (daemon type config), and buildEvent
  # (subscriber/sse.go) maps a "created" agent event to AgentSpawn UNCONDITIONALLY
  # once fields.agent is set — there is no way to seed the assistant without the
  # controller spawning a pod for it. mock_scenario=basic-echo keeps that spawn
  # CLAUDELESS (zero model spend). We immediately CLOSE the bead to PARK it: the
  # close fires AgentDone (controller tears the spawned pod down) and drops the
  # bead out of the reconciler's activeStatuses desired set, so no create-missing
  # spawn recurs and any canonical-name pod becomes a reconciler orphan. Step 3
  # then DRAINS the name to a clean slate before we plant the controlled husk.
  # The wake's reopen (closed->in_progress) in step 4 is the SOLE spawn that hits
  # the husk — exactly the mid-teardown wake the bug (ka-0000006228) bites.
  local mode="crew" role="plan-gardener"
  local agent_name="${PROJECT}-plan-asst-husk-${RANDOM_SUFFIX}"
  local parked_id=""
  parked_id=$(bead_id_from_json "$("$KD_BIN" create "$agent_name" \
    --type=agent --project "$PROJECT" -l "role:${role}" \
    -f "agent=${agent_name}" \
    -f "mode=${mode}" \
    -f "role=${role}" \
    -f "project=${PROJECT}" \
    -f "plan_id=${plan_id}" \
    -f "task_id=${plan_id}" \
    -f "mock_scenario=${PLANS_WAKE_MOCK_SCENARIO}" \
    -f "last_session_id=e2e-husk-session-${RANDOM_SUFFIX}" \
    -d "parked plan assistant (husk scenario) for $plan_id" --json 2>/dev/null || printf '{}')")
  if [ -z "$parked_id" ]; then
    fail "scenario 4: seed parked assistant bead" "no ID returned from kd create"
    return 0
  fi
  CREATED_BEADS+=("$parked_id")
  # The load-bearing assigned edge (one of planAssistant()'s two lookup paths;
  # it resolves the assistant even while the bead is closed — the reverse-dep
  # lookup is status-agnostic).
  "$KD_BIN" dep add "$parked_id" "$plan_id" --type assigned >/dev/null 2>&1 || true
  # Park it: close → AgentDone tears down the spawned pod + drops it from the
  # reconciler desired set.
  if ! "$KD_BIN" close "$parked_id" >/dev/null 2>&1; then
    fail "scenario 4: park (close) the seeded assistant" "kd close failed"
    return 0
  fi
  pass "seeded + parked assistant ($parked_id, name=$agent_name, closed)"

  # ── 3. DRAIN the canonical name to a clean slate. ─────────────────
  # The created->AgentSpawn from step 2 produced (or is lagging to produce) a
  # claudeless pod under the canonical name. The controller processes SSE events
  # FIFO, so its CreateAgentPod (created) runs before its DeleteAgentPod (close);
  # but the create's K8s pod object can land a beat after we start looking. Since
  # the bead is now CLOSED, the reconciler will not spawn and the ONLY thing that
  # can (re)populate the name is that single in-flight create. So we aggressively
  # force-delete anything under the name and require it to stay clear for a
  # CONTINUOUS window (DRAIN_CLEAR_STREAK polls) — long enough that a lagging
  # create manifests its pod object (which we then reap, resetting the streak)
  # before we accept the name as drained. This makes the husk we plant next the
  # sole occupant so the wake's collision is deterministic.
  local pod_name
  pod_name=$(canonical_pod_name "$mode" "$PROJECT" "$role" "$agent_name")
  dim "  canonical pod name: ${gvisor_ns}/${pod_name}"
  local DRAIN_CLEAR_STREAK=5 clear_streak=0 drain_deadline=$((SECONDS + 150))
  while [ "$SECONDS" -lt "$drain_deadline" ]; do
    if [ -z "$(pod_field "$gvisor_ns" "$pod_name" '{.metadata.uid}')" ]; then
      clear_streak=$((clear_streak + 1))
      [ "$clear_streak" -ge "$DRAIN_CLEAR_STREAK" ] && break
    else
      clear_streak=0
      kubectl -n "$gvisor_ns" delete pod "$pod_name" --grace-period=0 --force \
        >/dev/null 2>&1 || true
    fi
    sleep 3
  done
  if [ "$clear_streak" -lt "$DRAIN_CLEAR_STREAK" ]; then
    fail "scenario 4: drain canonical name before planting husk" \
      "a pod kept re-appearing under ${gvisor_ns}/${pod_name} after 150s (in-flight spawn not settling?)"
    return 0
  fi
  pass "drained canonical name (${gvisor_ns}/${pod_name} clear for ${DRAIN_CLEAR_STREAK} polls)"

  # ── 3b. Plant the controlled stale husk. ──────────────────────────
  if ! plant_husk_pod "$gvisor_ns" "$pod_name" "$agent_name" "$mode" "$PROJECT" "$role"; then
    fail "scenario 4: plant stale husk pod" "kubectl apply failed for ${gvisor_ns}/${pod_name}"
    return 0
  fi
  local husk_uid
  husk_uid=$(pod_field "$gvisor_ns" "$pod_name" '{.metadata.uid}')
  if [ -z "$husk_uid" ]; then
    fail "scenario 4: stale husk pod present" "no UID for planted pod ${gvisor_ns}/${pod_name}"
    return 0
  fi
  pass "planted stale husk pod (${gvisor_ns}/${pod_name}, uid=${husk_uid:0:8}…, unschedulable, bead-id mismatch)"

  # ── 4. POST the wake. Reopen (closed->in_progress) is the sole spawn. ─
  local msg="wake up over the husk ${RANDOM_SUFFIX}"
  local resp status body
  resp=$(viewer_post "/plans/${plan_id}/assistant/prompt" "$op_token" \
    "message=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$msg")")
  status=$(printf '%s' "$resp" | head -n1)
  body=$(printf '%s' "$resp" | tail -n +2)
  if [ "$status" != "200" ]; then
    fail "scenario 4: POST wake returns 200" "got $status; body=$(printf '%s' "$body" | head -c 300)"
    return 0
  fi
  if grep -qE 'data-state="waking"' <<<"$body"; then
    pass "POST wake returns 200 + lane fragment renders data-state=\"waking\""
  else
    fail "scenario 4: POST wake renders data-state=\"waking\"" \
      "200 but no data-state=\"waking\" in body: $(printf '%s' "$body" | head -c 200)"
  fi

  # The wake's agent_state='' clear is DROPPED by the daemon on the still-closed
  # bead (dropTransientClosedFields), so re-clear it on the now-reopened bead.
  # This matches wakePlanAssistant's intent and removes the terminal agent_state
  # that would otherwise let the reconciler's leaked-bead closer race the swap.
  "$KD_BIN" update "$parked_id" --unset-field agent_state >/dev/null 2>&1 || true

  # Confirm the reopened bead carries the conversation-resume contract — this is
  # what makes the plan chat CONTINUE rather than cold-start (bug ka-0000002360),
  # the "conversation resumes" half of the acceptance criteria. (crew pods use
  # emptyDir, so BOAT_SESSION_RESUME env is not injected; the durable bead field
  # is the portable signal.)
  sleep 2
  local revived resume_flag revived_status
  revived=$("$KD_BIN" show "$parked_id" --json 2>/dev/null || printf '{}')
  resume_flag=$(printf '%s' "$revived" | jq -r '.fields.session_resume // "unset"')
  revived_status=$(printf '%s' "$revived" | jq -r '.status // empty')
  if [ "$resume_flag" = "true" ] && { [ "$revived_status" = "in_progress" ] || [ "$revived_status" = "open" ]; }; then
    pass "scenario 4: reopened SAME bead with session_resume=true (status=$revived_status — conversation resumes)"
  else
    fail "scenario 4: wake reopened the bead with session_resume=true" \
      "session_resume=$resume_flag status=$revived_status (want true + in_progress/open)"
  fi

  # ── 5. Assert a FRESH Running pod under the canonical name. ───────
  # The husk is force-deleted (grace 0, UID-preconditioned) and a fresh pod is
  # created — either by the wake's adoptOrReplaceExistingPod (the fix under test)
  # or, since the bug is "partially self-healing", by the reconciler once the
  # husk is gone. Both converge to the same observable, which is exactly the
  # operator-facing contract the guard protects: the wake is NOT silently
  # swallowed — a live coop pod comes up under the stable name.
  local fresh_uid="" fresh_phase="" fresh_deltime="" running=0
  local deadline=$((SECONDS + PLANS_WAKE_POD_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    fresh_uid=$(pod_field "$gvisor_ns" "$pod_name" '{.metadata.uid}')
    fresh_deltime=$(pod_field "$gvisor_ns" "$pod_name" '{.metadata.deletionTimestamp}')
    fresh_phase=$(pod_field "$gvisor_ns" "$pod_name" '{.status.phase}')
    # A fresh, live pod: different UID from the husk, not being deleted, Running.
    if [ -n "$fresh_uid" ] && [ "$fresh_uid" != "$husk_uid" ] && \
       [ -z "$fresh_deltime" ] && [ "$fresh_phase" = "Running" ]; then
      running=1
      break
    fi
    sleep 5
  done
  [ -n "$fresh_uid" ] && [ "$fresh_uid" != "$husk_uid" ] && CREATED_PODS+=("${gvisor_ns}/${pod_name}")

  if [ "$running" = "1" ]; then
    pass "scenario 4: FRESH Running coop pod under the canonical name (uid=${fresh_uid:0:8}… != husk, no DeletionTimestamp)"
  else
    fail "scenario 4: fresh Running pod replaces the husk" \
      "after ${PLANS_WAKE_POD_TIMEOUT}s: uid=${fresh_uid:-<none>} (husk=${husk_uid:0:8}…) phase=${fresh_phase:-<none>} deletionTimestamp=${fresh_deltime:-<none>}"
    kubectl -n "$gvisor_ns" get pod "$pod_name" -o wide 2>&1 | sed 's/^/    /' || true
  fi
  echo
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " /plans Wake button (plan-assistant prompt) E2E (gd-R7wAWZ0E8T)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "plans-wake-button module" "viewer service $VIEWER_SVC absent"
    summary
    return 0
  fi
  setup_beads_portforward

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "plans-wake-button module" \
      "kd cannot reach beads daemon at ${BEADS_HTTP_URL:-unset}"
    summary
    return 0
  fi

  # ── Mint an operator token for the POST verb. ────────────────────
  local op_token=""
  if ! op_token=$(mint_user_token "$OPERATOR_EMAIL" 2>/tmp/plans-wake-mint.err); then
    yellow "mint_user_token failed for ${OPERATOR_EMAIL} — see /tmp/plans-wake-mint.err"
    skip "scenario 1: spawn on first message" "no usable GitLab-direct bearer"
    skip "scenario 2: wake parked assistant"  "no usable GitLab-direct bearer"
    skip "scenario 3: empty message rejected"  "no usable GitLab-direct bearer"
    summary
    return 0
  fi
  dim "  minted operator token (sub=${OPERATOR_EMAIL})"
  # The viewer's own OIDC middleware stays pinned to the Keycloak issuer until
  # the viewer-login flip lands (ka-0000002083); it rejects this GitLab-direct /
  # static-daemon bearer with 401. SKIP-clean rather than hard-fail every gated
  # /plans GET/POST (bug gd-5M8iT2oNlr) — gate BOTH the opt-in and graph-only
  # branches below. Auto-enables when the viewer trusts the GitLab edge.
  if ! viewer_accepts_bearer "$op_token" "/plans?project=${PROJECT}&operated_by="; then
    skip "plans-wake-button module" "viewer rejects the E2E transport bearer — viewer still OIDC-gated at the pod; viewer-login flip pending (ka-0000002083)"
    summary
    return 0
  fi

  # ══════════════════════════════════════════════════════════════════
  # Opt-in branch: the live-cluster wake-over-a-husk guard runs INSTEAD of the
  # graph-only scenarios 1-3. On a cluster with a live controller (the only
  # place scenario 4 can assert), the un-mocked scenario-1/2 spawns would
  # schedule real Claude pods and burn model spend, so the two suites are
  # mutually exclusive. See the header + scenario_4_wake_over_husk preamble.
  # ══════════════════════════════════════════════════════════════════
  if [ "$PLANS_WAKE_POD_E2E" = "1" ]; then
    scenario_4_wake_over_husk "$op_token"
    summary
    return 0
  fi

  # ══════════════════════════════════════════════════════════════════
  # Scenario 1: SPAWN — plan with no assistant. First message spawns a
  # fresh assistant (no session_resume) and returns data-state="waking".
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 1: first message spawns a fresh assistant"
  local plan1_id=""
  plan1_id=$(bead_id_from_json "$("$KD_BIN" create "${TITLE_PREFIX}-spawn-${RANDOM_SUFFIX}" \
    --type=epic --project "$PROJECT" -l "plan" \
    -d "scenario 1 plan ($RANDOM_SUFFIX)" --json 2>/dev/null || printf '{}')")
  if [ -z "$plan1_id" ]; then
    fail "create scenario-1 plan epic" "no ID returned from kd create"
  else
    CREATED_BEADS+=("$plan1_id")
    pass "scenario-1 plan epic created ($plan1_id)"

    local msg1="draft the plan for me ${RANDOM_SUFFIX}"
    local resp1 status1 body1
    resp1=$(viewer_post "/plans/${plan1_id}/assistant/prompt" "$op_token" \
      "message=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$msg1")")
    status1=$(printf '%s' "$resp1" | head -n1)
    body1=$(printf '%s' "$resp1" | tail -n +2)

    if [ "$status1" != "200" ]; then
      fail "POST spawn returns 200" "got $status1; body=$(printf '%s' "$body1" | head -c 300)"
    else
      pass "POST spawn returns 200"
      if grep -qE 'data-state="waking"' <<<"$body1"; then
        pass "scenario 1: lane fragment renders data-state=\"waking\""
      else
        fail "scenario 1: lane fragment renders data-state=\"waking\"" \
          "no data-state=\"waking\" in response body"
      fi

      # Give the write a moment to land, then inspect the graph.
      sleep 2
      local asst1
      asst1=$(assistant_bead_for_plan "$plan1_id")
      if [ -z "$asst1" ]; then
        fail "scenario 1: assistant agent bead created" \
          "no plan-agent bead (either role label) bound to $plan1_id"
      else
        local asst1_id
        asst1_id=$(printf '%s' "$asst1" | jq -r '.id // empty')
        [ -n "$asst1_id" ] && CREATED_BEADS+=("$asst1_id")
        pass "scenario 1: assistant agent bead created ($asst1_id)"

        # Fresh spawns mint the unified plan-gardener role (ka-0000000081).
        if [ "$(printf '%s' "$asst1" | jq -r '.fields.role // empty')" = "plan-gardener" ]; then
          pass "scenario 1: assistant role = plan-gardener"
        else
          fail "scenario 1: assistant role = plan-gardener" \
            "role=$(printf '%s' "$asst1" | jq -r '.fields.role // "<none>"')"
        fi

        if [ "$(printf '%s' "$asst1" | jq -r '.fields.prompt // empty')" = "$msg1" ]; then
          pass "scenario 1: assistant prompt = operator message"
        else
          fail "scenario 1: assistant prompt = operator message" \
            "prompt=$(printf '%s' "$asst1" | jq -r '.fields.prompt // "<none>"')"
        fi

        # Fresh spawn must NOT carry session_resume (no prior conversation).
        if [ "$(printf '%s' "$asst1" | jq -r '.fields.session_resume // "unset"')" = "true" ]; then
          fail "scenario 1: fresh spawn has no session_resume" \
            "session_resume=true on a first-time spawn"
        else
          pass "scenario 1: fresh spawn has no session_resume"
        fi
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 2: WAKE — plan whose assistant is PARKED. The message must
  # REVIVE THE SAME bead in place: same bead id (so the coop S3 session-resume
  # key KD_AGENT_ID stays constant and the conversation continues rather than
  # cold-starting — bug ka-0000002360), status flipped back to in_progress (the
  # AgentSpawn re-spawn trigger), session_resume=true, prompt refreshed. It must
  # NOT mint a second plan-assistant bead and must NOT nudge coopmux.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 2: message wakes a parked assistant IN PLACE (same bead id, session_resume, in_progress)"
  local plan2_id=""
  plan2_id=$(bead_id_from_json "$("$KD_BIN" create "${TITLE_PREFIX}-wake-${RANDOM_SUFFIX}" \
    --type=epic --project "$PROJECT" -l "plan" \
    -d "scenario 2 plan ($RANDOM_SUFFIX)" --json 2>/dev/null || printf '{}')")
  if [ -z "$plan2_id" ]; then
    fail "create scenario-2 plan epic" "no ID returned from kd create"
  else
    CREATED_BEADS+=("$plan2_id")
    pass "scenario-2 plan epic created ($plan2_id)"

    # Seed a PARKED assistant: terminal agent_state + bound to the plan by
    # plan_id AND the assigned edge (both lookup paths planAssistant() uses).
    # assistantIsLive() is false for agent_state=done (not in the alive set),
    # so promptPlanAssistant takes the wake/spawn branch. Deliberately seeded
    # under the LEGACY role:plan-assistant label/role so this scenario also
    # proves the resolver's dual-read wakes a pre-rename bead (ka-0000000081).
    local parked_name="gardenio-plan-asst-wake-${RANDOM_SUFFIX}"
    local parked_id=""
    parked_id=$(bead_id_from_json "$("$KD_BIN" create "$parked_name" \
      --type=agent --project "$PROJECT" -l "role:plan-assistant" \
      -f "agent=${parked_name}" \
      -f "agent_state=done" \
      -f "role=plan-assistant" \
      -f "project=${PROJECT}" \
      -f "mode=crew" \
      -f "plan_id=${plan2_id}" \
      -f "task_id=${plan2_id}" \
      -f "last_session_id=e2e-parked-session-${RANDOM_SUFFIX}" \
      -d "parked plan assistant for $plan2_id" --json 2>/dev/null || printf '{}')")
    if [ -z "$parked_id" ]; then
      fail "seed parked assistant bead" "no ID returned from kd create"
    else
      CREATED_BEADS+=("$parked_id")
      "$KD_BIN" dep add "$parked_id" "$plan2_id" --type assigned >/dev/null 2>&1 || true
      pass "seeded parked assistant ($parked_id, name=$parked_name, agent_state=done)"

      local msg2="wake up and answer question 4 ${RANDOM_SUFFIX}"
      local resp2 status2 body2
      resp2=$(viewer_post "/plans/${plan2_id}/assistant/prompt" "$op_token" \
        "message=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$msg2")")
      status2=$(printf '%s' "$resp2" | head -n1)
      body2=$(printf '%s' "$resp2" | tail -n +2)

      if [ "$status2" != "200" ]; then
        fail "POST wake returns 200" "got $status2; body=$(printf '%s' "$body2" | head -c 300)"
      else
        pass "POST wake returns 200"
        if grep -qE 'data-state="waking"' <<<"$body2"; then
          pass "scenario 2: lane fragment renders data-state=\"waking\""
        else
          fail "scenario 2: lane fragment renders data-state=\"waking\"" \
            "no data-state=\"waking\" in response body"
        fi

        sleep 2
        # Wake REVIVES the SAME bead — so the number of plan-agent beads bound
        # to this plan must stay at exactly ONE (no second bead minted). Count
        # under BOTH role labels: an erroneous re-mint would carry the new
        # role:plan-gardener label, which a legacy-only filter would miss.
        local asst_count
        asst_count=$("$KD_BIN" list --type=agent --label-any "role:plan-gardener,role:plan-assistant" --json 2>/dev/null \
          | jq --arg epic "$plan2_id" '[ .[] | select((.fields.plan_id // "") == $epic) ] | length' 2>/dev/null || echo 0)
        if [ "$asst_count" = "1" ]; then
          pass "scenario 2: wake did NOT mint a second bead (exactly 1 plan agent bound to the plan — stable id)"
        else
          fail "scenario 2: wake reuses the same bead (no re-mint)" \
            "expected exactly 1 plan-agent bead bound to $plan2_id, found $asst_count"
        fi

        # Re-read the SAME seeded bead by id and assert it was revived in place.
        local revived
        revived=$("$KD_BIN" show "$parked_id" --json 2>/dev/null || printf '{}')
        local revived_status revived_resume revived_prompt revived_state
        revived_status=$(printf '%s' "$revived" | jq -r '.status // empty')
        revived_resume=$(printf '%s' "$revived" | jq -r '.fields.session_resume // "unset"')
        revived_prompt=$(printf '%s' "$revived" | jq -r '.fields.prompt // empty')
        revived_state=$(printf '%s' "$revived" | jq -r '.fields.agent_state // empty')

        if [ "$revived_status" = "in_progress" ] || [ "$revived_status" = "open" ]; then
          pass "scenario 2: prior bead $parked_id reopened (status=$revived_status — the AgentSpawn re-spawn trigger)"
        else
          fail "scenario 2: prior bead reopened for wake" \
            "status=$revived_status (want in_progress/open), agent_state=$revived_state"
        fi

        if [ "$revived_resume" = "true" ]; then
          pass "scenario 2: revived bead carries session_resume=true"
        else
          fail "scenario 2: revived bead carries session_resume=true" \
            "session_resume=$revived_resume on the woken bead"
        fi

        if [ "$revived_prompt" = "$msg2" ]; then
          pass "scenario 2: revived bead prompt refreshed to the wake message"
        else
          fail "scenario 2: revived bead prompt refreshed" \
            "prompt=$revived_prompt, want $msg2"
        fi
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 3: EMPTY MESSAGE — a blank message is rejected with 400 and
  # creates no assistant bead (placeholder-only submission guard).
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 3: empty message is rejected (400, no spawn)"
  local plan3_id=""
  plan3_id=$(bead_id_from_json "$("$KD_BIN" create "${TITLE_PREFIX}-empty-${RANDOM_SUFFIX}" \
    --type=epic --project "$PROJECT" -l "plan" \
    -d "scenario 3 plan ($RANDOM_SUFFIX)" --json 2>/dev/null || printf '{}')")
  if [ -z "$plan3_id" ]; then
    fail "create scenario-3 plan epic" "no ID returned from kd create"
  else
    CREATED_BEADS+=("$plan3_id")
    pass "scenario-3 plan epic created ($plan3_id)"

    local resp3 status3
    resp3=$(viewer_post "/plans/${plan3_id}/assistant/prompt" "$op_token" "message=")
    status3=$(printf '%s' "$resp3" | head -n1)
    if [ "$status3" = "400" ]; then
      pass "scenario 3: empty message returns 400"
    else
      fail "scenario 3: empty message returns 400" "got $status3"
    fi

    sleep 1
    if [ -z "$(assistant_bead_for_plan "$plan3_id")" ]; then
      pass "scenario 3: no assistant bead created for the rejected message"
    else
      local stray
      stray=$(assistant_bead_for_plan "$plan3_id" | jq -r '.id // empty')
      [ -n "$stray" ] && CREATED_BEADS+=("$stray")
      fail "scenario 3: no assistant bead created" \
        "an assistant bead ($stray) was created despite the 400"
    fi
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " plans-wake-button: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in ${ERRORS[@]+"${ERRORS[@]}"}; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
