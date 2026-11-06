#!/usr/bin/env bash
# test-dark-jira-fence-pod.sh — dark-pod Jira write-token fence, POD leg E2E
# (task ka-0000000981; split out of ka-0000007263 / MR !6612, coverage-backfill
# epic ka-0000000958; fence bead ka-0000005242, R7 epic ka-0000004670).
#
# ── What this asserts (the POD leg, distinct from the NETWORK leg) ────────────
#
# The sibling test-dark-jira-fence.sh asserts the NETWORK leg: that the deployed
# dark-agent-egress NetworkPolicy denies a dark pod egress to Atlassian. This
# module asserts the POD leg: that scrubDarkForbiddenEnv (podspec.go) actually
# strips the privileged Jira WRITE token JIRA_API_TOKEN out of a DEPLOYED dark
# agent pod's env — the unguarded plaintext-env path R7 flagged (project/role
# EnvVars + EnvOverrides have NO name filtering, unlike the prefix-gated secrets
# path). That layer had UNIT coverage only (podspec_dark_jira_test.go); nothing
# asserted it on a real controller-spawned pod until here.
#
# ── Why the OBVIOUS test is VACUOUS (the whole reason this module is careful) ──
#
# The naive version — spawn a dark pod, assert JIRA_API_TOKEN is absent — proves
# NOTHING on staging: jiraBridge.enabled=false, there is NO jira secret in
# the namespace, and no project/role bead injects JIRA_API_TOKEN. The assertion
# passes even if scrubDarkForbiddenEnv is DELETED, because the token was never
# injected in the first place. Adding it would manufacture a false green that
# looks like coverage. (Verified live 2026-08-05: 0 JIRA_API_TOKEN env
# occurrences across staging pods, no jira secret.)
#
# ── What an HONEST version requires (all four legs) ───────────────────────────
#
# We build the token-injection rig the vacuity trap needs, then a NEGATIVE
# CONTROL that proves the token WAS injected:
#
#   1. Seed an ephemeral type=project bead whose `env` field carries a DUMMY
#      JIRA_API_TOKEN — the exact unguarded plaintext path (project EnvVars).
#      Wait for the controller's ProjectCache to pick it up (applyProjectDefaults
#      keys on ProjectCache[project], so an uncached project injects nothing).
#   2. DARK leg: seed a dark agent bead (labels dark:true + monorepo-dark, per
#      beadsapi.IsDark) against that project, let the controller spawn its pod,
#      and assert JIRA_API_TOKEN is ABSENT from the pod env.
#   3. CONTROL leg (NOT optional): seed a NON-dark agent against the SAME project,
#      spawn it, and assert JIRA_API_TOKEN is PRESENT (== the dummy). Without this
#      the DARK leg cannot distinguish "the scrub worked" from "the token was
#      never injected" — precisely the vacuity above. The two legs run against
#      the SAME project bead, so the ONLY difference is the dark marker.
#   4. LINCHPIN leg: assert the dark pod carries GARDENIO_DARK=true (the in-pod
#      client-layer marker gb reads to build a READ-ONLY Jira client) AND the
#      gardenio.io/dark pod label (the podSelector the network fence keys on — the
#      join between the pod and network legs). JIRA_EMAIL (harmless read-only
#      identity) must SURVIVE on the dark pod; only the write token is fenced.
#
# Proven on the live substrate (2026-08-05, bead ka-0000000981): on a FENCED
# namespace (2026.217.x, dark-agent-egress present) the dark pod is scrubbed and
# the control keeps the token; running the SAME rig on the UNFENCED persistent
# staging (pre-fence controller) left the token IN the dark pod — i.e. this
# module has TEETH: it fails if scrubDarkForbiddenEnv is absent.
#
# ── Cost / why OPT-IN + SKIP-loud (never a false green) ───────────────────────
#
# Two real agent-pod spawns plus ephemeral config-bead setup/teardown. The
# in-shard suite spawns NO real agent pods (0 in staging); the only precedent
# for controller-spawn pod-env assertions is test-gb-stop-suspend.sh, run
# out-of-band behind GVISOR_E2E. We follow the same discipline: gated behind
# DARK_FENCE_POD_E2E=1, SKIP LOUD when unset so it can never manufacture a false
# green in the default suite. When the flag IS set but the fence is not deployed
# (a namespace older than 2026.217.x has NEITHER dark-agent-egress policy) we
# SKIP LOUD too — the module cannot assert a control the deployment does not have
# — rather than red the suite.
#
# Precedents reused: test-pool-assign-attribution.sh (ephemeral project ->
# wait_for_agent_pod -> pod_env helper -> hardened trap cleanup; mock_scenario
# SPEND-SAFETY so pods run claudeless at ZERO model spend), test-gb-stop-suspend.sh
# (controller-spawn wait + kubectl jsonpath env assertion).
#
# Usage:
#   DARK_FENCE_POD_E2E=1 ./gardenio/tests/e2e/scripts/test-dark-jira-fence-pod.sh
#   DARK_FENCE_POD_E2E=1 NAMESPACE=gardenio-e2e-0 KEEP_BEADS=1 ./...
set -uo pipefail

# ── Config (all overridable via env) ────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
# Chart resources are named <release>-<component>; release == namespace in the
# per-run e2e namespaces (test-dark-jira-fence.sh invariant). The
# dark-agent-egress NetworkPolicy is the deployed-fence signal we gate on.
RELEASE="${RELEASE:-${NAMESPACE}}"
DARK_NP="${RELEASE}-dark-agent-egress"
# Mock scenario so both spawned pods run claudeless (ZERO model spend), not real
# Claude — the pod env is frozen at create, so this is the ONLY spend-safe path
# (mirrors test-pool-assign-attribution / test-gb-stop-suspend).
MOCK_SCENARIO="${MOCK_SCENARIO:-basic-echo}"
# The DUMMY write token we inject via the ephemeral project's plaintext env. Must
# be obviously fake — it is a probe value, never a real credential.
DUMMY_TOKEN="${DUMMY_TOKEN:-dummy-dark-fence-probe-token-DO-NOT-USE}"
DUMMY_EMAIL="${DUMMY_EMAIL:-e2e-darkfence@example.com}"
# ProjectCache refresh lag: applyProjectDefaults keys on ProjectCache[project],
# refreshed on the controller's sync interval (default 60s). We poll the log for
# the refresh AND fall back to a fixed dwell, so an uncached project never makes
# the injection silently no-op (which would fool the CONTROL leg into a red).
CACHE_WAIT="${CACHE_WAIT:-150}"
# How long to wait for the controller to spawn each pod (Running, not Ready — a
# claudeless mock pod is Running but its readiness gate never flips).
POD_WAIT="${POD_WAIT:-240}"
RUN_ID="${RUN_ID:-$$}"

export KD_ACTOR="${KD_ACTOR:-e2e-dark-jira-fence-pod}"
# oidc-shape operator so daemon writes pass the human-operator sanitizer.
ASSIGNING_HUMAN="${BEADS_OPERATED_BY:-oidc:e2e-dark-jira-fence-pod@example.com}"

PASS=0
FAIL=0
ERRORS=()
CREATED_BEADS=()
PROJECT_ID=""
PF_PID=""

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

# skip_all exits 0 with a LOUD banner. A skip must NEVER be silently read as a
# pass for a SECURITY control — the banner spells out what was NOT asserted.
# tag "off" = opt-in unset (inert, expected in standard shards); tag "loud" =
# opt-in SET but the fence/substrate is absent (the gated run expected to assert).
skip_all() {
  local why="$1" tag="${2:-loud}"
  if [ "$tag" = "off" ]; then
    blue "SKIP dark-jira-fence-pod: $why"
    dim "  Opt-in module: set DARK_FENCE_POD_E2E=1 and run against a namespace whose"
    dim "  deployment carries the dark-Jira fence (2026.217.x+, dark-agent-egress NP)."
    if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
    exit 0
  fi
  red "════════════════════════════════════════════════════"
  red " SKIP dark-jira-fence-pod (LOUD): $why"
  red " This run did NOT assert the dark-pod Jira write-token fence's POD leg"
  red " (ka-0000005242): that scrubDarkForbiddenEnv strips JIRA_API_TOKEN from a"
  red " DEPLOYED dark agent pod while a non-dark control pod keeps it. A green"
  red " suite here proves NOTHING about the pod-layer scrub."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# beads_get <id> — print bead JSON (empty on failure).
beads_get() {
  curl -sf --max-time 10 "${BEADS_HTTP_URL}/v1/beads/$1" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} 2>/dev/null || echo ""
}

# Read one env var from the `agent` container of a pod by agent label.
pod_env() {
  local agent_name="$1" var="$2"
  local pod
  pod=$(kubectl -n "$NAMESPACE" get pod -l "gardenio.io/agent=${agent_name}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [ -z "$pod" ] && { echo ""; return; }
  # jsonpath over the spec env (frozen at create) — robust to the pod not being
  # Ready (a claudeless standby pod never becomes Ready, but its spec is stable).
  kubectl -n "$NAMESPACE" get pod "$pod" \
    -o jsonpath="{range .spec.containers[?(@.name==\"agent\")].env[?(@.name==\"$var\")]}{.value}{end}" \
    2>/dev/null || echo ""
}

# Read a pod label by agent label.
pod_label() {
  local agent_name="$1" label="$2"
  local pod
  pod=$(kubectl -n "$NAMESPACE" get pod -l "gardenio.io/agent=${agent_name}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [ -z "$pod" ] && { echo ""; return; }
  kubectl -n "$NAMESPACE" get pod "$pod" \
    -o jsonpath="{.metadata.labels.${label//./\\.}}" 2>/dev/null || echo ""
}

# Wait for a pod whose `agent` container is RUNNING (NOT Ready) for the agent.
# A claudeless standby pod is Running but never Ready. Prints "1" on success.
wait_for_agent_pod_running() {
  local agent_name="$1" timeout="${2:-$POD_WAIT}"
  local deadline=$(( $(date +%s) + timeout )) started=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    started=$(kubectl -n "$NAMESPACE" get pod -l "gardenio.io/agent=${agent_name}" \
      -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="agent")].state.running.startedAt}' \
      2>/dev/null || true)
    [ -n "$started" ] && { echo "1"; return 0; }
    sleep 5
  done
  echo ""
  return 1
}

# Seed an agent bead via the beads HTTP API. Dark iff $2=1 (labels dark:true +
# monorepo-dark, per beadsapi.IsDark). mock_scenario keeps the pod claudeless.
# X-No-Auto-Inherit:1 mirrors how the pool reconciler seeds real agents. Echoes
# the bead id (empty on failure).
seed_agent() {
  local name="$1" dark="$2" body labels
  if [ "$dark" = "1" ]; then
    labels="['e2e','dark-jira-pod-${RUN_ID}','project:${PROJECT_NAME}','dark:true','monorepo-dark']"
  else
    labels="['e2e','dark-jira-pod-${RUN_ID}','project:${PROJECT_NAME}']"
  fi
  body=$(LABELS="$labels" python3 -c "
import json, os
print(json.dumps({
  'type': 'agent',
  'title': '$name',
  'labels': json.loads(os.environ['LABELS'].replace(\"'\", '\"')),
  'fields': {
    'agent': '$name',
    'agent_state': 'spawning',
    'mode': 'crew',
    'role': 'crew',
    'project': '$PROJECT_NAME',
    'mock_scenario': '$MOCK_SCENARIO',
  },
}))")
  curl -sf --max-time 10 -X POST "${BEADS_HTTP_URL}/v1/beads" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -H "X-Operated-By: ${ASSIGNING_HUMAN}" \
    -H 'X-No-Auto-Inherit: 1' \
    -H 'Content-Type: application/json' \
    -d "$body" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
    || echo ""
}

# ── Cleanup (LOAD-BEARING) ──────────────────────────────────────────
# We seed a project bead that injects a fake JIRA_API_TOKEN and spawn two real
# pods. A hardened trap MUST remove the ephemeral project + both agent beads even
# on failure/interrupt so no JIRA_API_TOKEN-bearing config bead is left behind in
# the fleet and no orphan pod survives (closing/deleting the agent bead makes the
# reconciler tear its pod down on the next sweep). Best-effort; never fail on it.
cleanup() {
  blue "── cleanup ──"
  if [ "${KEEP_BEADS:-0}" = "1" ]; then
    dim "  KEEP_BEADS=1 — leaving ${#CREATED_BEADS[@]} bead(s) (incl. the token-bearing project) for debug"
  else
    for id in "${CREATED_BEADS[@]:-}"; do
      [ -n "$id" ] || continue
      curl -sf --max-time 10 -X DELETE "${BEADS_HTTP_URL}/v1/beads/$id" \
        ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1 \
        || "$KD_BIN" close "$id" 2>/dev/null || true
    done
    # Belt-and-suspenders: the token-bearing project MUST NOT survive even if it
    # never made it into CREATED_BEADS (e.g. a mid-create interrupt).
    if [ -n "$PROJECT_ID" ]; then
      curl -sf --max-time 10 -X DELETE "${BEADS_HTTP_URL}/v1/beads/$PROJECT_ID" \
        ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1 || true
    fi
    dim "  removed ephemeral project + agent beads (pods reaped by the reconciler)"
  fi
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "════════════════════════════════════════════════════"
echo " dark-pod Jira write-token fence — POD leg E2E (ka-0000000981)"
echo " token-inject project -> DARK pod (scrubbed) + NON-DARK control (kept)"
echo " namespace=$NAMESPACE  release=$RELEASE"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + preconditions (SKIP, never silently) ───────────
if [ "${DARK_FENCE_POD_E2E:-0}" != "1" ]; then
  skip_all "DARK_FENCE_POD_E2E != 1 (opt-in module — two real pod spawns)" off
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed"
command -v curl    >/dev/null 2>&1 || skip_all "curl not installed"
command -v python3 >/dev/null 2>&1 || skip_all "python3 not installed"
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || skip_all "namespace '$NAMESPACE' not present"
[ "$(kubectl auth can-i create pods -n "$NAMESPACE" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in '$NAMESPACE'"

# The POD scrub and the NETWORK fence ship together (2026.217.x). We gate on the
# dark-agent-egress NetworkPolicy: a namespace without it predates the fence, so
# scrubDarkForbiddenEnv is not deployed and the DARK leg would legitimately fail
# (the token would survive) — a skip, not a red. Present ⇒ the scrub is deployed
# and any surviving token is a HARD regression.
if ! kubectl -n "$NAMESPACE" get networkpolicy "$DARK_NP" >/dev/null 2>&1; then
  skip_all "'$DARK_NP' absent in '$NAMESPACE' — deployment predates the dark-Jira fence (shipped 2026.217.x)"
fi
blue "Fence deployed ($DARK_NP present) — driving the token-injection rig."

# ── 1. Reach the beads daemon ───────────────────────────────────────
blue "[1/5] reaching the beads daemon"
if [ -z "${BEADS_TOKEN:-}" ]; then
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
  [ -n "$secret" ] && export BEADS_TOKEN="$secret"
fi
if [ -z "${BEADS_HTTP_URL:-}" ]; then
  PORT="${BEADS_PORT:-19390}"
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${PORT}:8080" \
    >/tmp/dark-jira-pod-pf.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${PORT}"
fi
if ! curl -sf --max-time 10 "${BEADS_HTTP_URL}/v1/health" >/dev/null 2>&1; then
  skip_all "beads daemon $BEADS_HTTP_URL unreachable (port-forward to svc/$BEADS_SVC failed)"
fi
pass "beads daemon reachable ($BEADS_HTTP_URL)"

# latest_cached_projects — the `projects` count from the controller's most-recent
# "refreshed project cache" log line (0 if none yet). The controller logs this
# integer on every ProjectCache refresh; it is a direct, clock-skew-proof signal
# of how many projects the cache currently holds. Single-replica controller, so
# `deploy/` logs are unambiguous.
CTRL_DEPLOY="${NAMESPACE}-agents"
latest_cached_projects() {
  kubectl -n "$NAMESPACE" logs "deploy/${CTRL_DEPLOY}" --tail=600 2>/dev/null \
    | grep "refreshed project cache" | tail -1 \
    | python3 -c "
import sys, re
line = sys.stdin.read()
m = re.search(r'\"projects\":\s*(\d+)', line)
print(m.group(1) if m else '0')
" 2>/dev/null || echo "0"
}

# ── 2. Seed the ephemeral token-injection project ───────────────────
blue "[2/5] seeding an ephemeral project that injects a DUMMY JIRA_API_TOKEN (plaintext env)"
# Baseline the cached-project count BEFORE the create so we can wait for a refresh
# that STRICTLY includes our new project (count > baseline) — not merely for "some
# refresh happened", which races a refresh that started before our create landed.
BASELINE_PROJECTS=$(latest_cached_projects)
dim "  cached projects before create: $BASELINE_PROJECTS"
PROJECT_NAME="e2e-darkjira-${RUN_ID}-$(date +%s)"
PROJ_BODY=$(TOK="$DUMMY_TOKEN" EM="$DUMMY_EMAIL" PN="$PROJECT_NAME" RID="$RUN_ID" python3 -c "
import json, os
env = [
  {'name': 'JIRA_API_TOKEN', 'value': os.environ['TOK']},
  {'name': 'JIRA_EMAIL',     'value': os.environ['EM']},
]
print(json.dumps({
  'type': 'project',
  'title': os.environ['PN'],
  'labels': ['e2e', 'dark-jira-pod-' + os.environ['RID']],
  'fields': {'env': json.dumps(env)},
}))")
PROJECT_ID=$(curl -sf --max-time 10 -X POST "${BEADS_HTTP_URL}/v1/beads" \
  ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
  -H "X-Operated-By: ${ASSIGNING_HUMAN}" \
  -H 'Content-Type: application/json' \
  -d "$PROJ_BODY" 2>/dev/null \
  | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
if [ -z "$PROJECT_ID" ]; then
  fail "seed ephemeral project" "beads POST returned no id"
  echo "════════ $PASS passed, $FAIL failed ════════"; exit 1
fi
CREATED_BEADS+=("$PROJECT_ID")
pass "seeded ephemeral project ($PROJECT_ID, project=$PROJECT_NAME) with plaintext JIRA_API_TOKEN"

# The controller only injects EnvVars for a project it has in ProjectCache
# (applyProjectDefaults keys on ProjectCache[project]). We MUST wait for a refresh
# that INCLUDES our project before the pods spawn — an uncached project injects
# nothing, silently vacating BOTH legs (this bit the first live run: counting
# refreshes in a rolling window broke early on a pre-create refresh). Wait for the
# controller's own logged `projects` count to exceed the pre-create baseline; that
# integer can only rise once our project is in the cache. Clock-skew-proof (it uses
# the controller's count, not wall time). The CONTROL leg is the ultimate backstop
# if this heuristic is ever wrong — it fails LOUD when the token was not injected.
blue "  waiting up to ${CACHE_WAIT}s for the controller ProjectCache to include the project (baseline=$BASELINE_PROJECTS)"
CACHE_DEADLINE=$(( $(date +%s) + CACHE_WAIT ))
CACHE_OK=0
while [ "$(date +%s)" -lt "$CACHE_DEADLINE" ]; do
  cur=$(latest_cached_projects)
  if [ "${cur:-0}" -gt "${BASELINE_PROJECTS:-0}" ] 2>/dev/null; then
    CACHE_OK=1
    dim "  cached projects now $cur (> baseline $BASELINE_PROJECTS) — our project is cached"
    # One extra refresh interval so the reconciler that spawns pods is provably
    # reading the post-refresh cache, not mid-write.
    sleep 5
    break
  fi
  sleep 10
done
if [ "$CACHE_OK" = "1" ]; then
  pass "ProjectCache now includes the ephemeral project (EnvVars injectable)"
else
  dim "  could not confirm the project entered ProjectCache within ${CACHE_WAIT}s"
  dim "  (proceeding — the CONTROL leg is the backstop and fails LOUD if injection did not happen)"
fi

# ── 3. Spawn the DARK pod + the NON-DARK control against the SAME project ──
blue "[3/5] spawning a DARK pod and a NON-DARK control against the same project"
DARK_AGENT="djp-${RUN_ID}-dark"
CTRL_AGENT="djp-${RUN_ID}-ctrl"
DARK_ID=$(seed_agent "$DARK_AGENT" 1)
CTRL_ID=$(seed_agent "$CTRL_AGENT" 0)
if [ -z "$DARK_ID" ] || [ -z "$CTRL_ID" ]; then
  fail "seed agent beads" "dark='$DARK_ID' ctrl='$CTRL_ID' (beads POST returned no id)"
  echo "════════ $PASS passed, $FAIL failed ════════"; exit 1
fi
CREATED_BEADS+=("$DARK_ID" "$CTRL_ID")
pass "seeded DARK agent ($DARK_ID) + NON-DARK control ($CTRL_ID), mock=$MOCK_SCENARIO (claudeless)"

# Confirm the daemon actually stored the dark markers (IsDark keys on them) — a
# dropped label would make the "dark" pod non-dark and the DARK leg vacuously
# pass (token absent because never dark-injected... no: token WOULD be present).
DARK_LABELS=$(beads_get "$DARK_ID" | python3 -c "import sys,json;print(','.join(json.load(sys.stdin).get('labels',[])))" 2>/dev/null || echo "")
case ",$DARK_LABELS," in
  *,dark:true,*|*,monorepo-dark,*) pass "dark agent bead carries a beadsapi.IsDark marker ($DARK_LABELS)" ;;
  *) fail "dark agent bead carries a dark marker" "labels='$DARK_LABELS' — IsDark would read it as NON-dark, voiding the DARK leg" ;;
esac

blue "  waiting up to ${POD_WAIT}s for BOTH pods to reach Running (claudeless standby)"
DARK_RUN=$(wait_for_agent_pod_running "$DARK_AGENT" "$POD_WAIT" || true)
CTRL_RUN=$(wait_for_agent_pod_running "$CTRL_AGENT" "$POD_WAIT" || true)
if [ "$DARK_RUN" = "1" ]; then
  pass "controller spawned the DARK pod (Running)"
else
  fail "DARK pod Running" "no Running pod for gardenio.io/agent=$DARK_AGENT within ${POD_WAIT}s"
fi
if [ "$CTRL_RUN" = "1" ]; then
  pass "controller spawned the NON-DARK control pod (Running)"
else
  fail "control pod Running" "no Running pod for gardenio.io/agent=$CTRL_AGENT within ${POD_WAIT}s"
fi
# Both pods are required — the CONTROL leg is what makes the DARK leg non-vacuous.
if [ "$DARK_RUN" != "1" ] || [ "$CTRL_RUN" != "1" ]; then
  fail "both pods present" "the negative control requires BOTH pods; cannot assert the fence without it"
  echo "════════ $PASS passed, $FAIL failed ════════"
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi

# ── 4. THE ASSERTIONS ───────────────────────────────────────────────
blue "[4/5] asserting the fence: dark pod SCRUBBED, control pod KEPT the token"

# CONTROL leg FIRST — it proves the token WAS injected. If the control pod does
# NOT carry the token, the injection rig is broken (uncached project, wrong env
# shape) and the DARK leg's "absent" would be VACUOUS. So a failing control is a
# hard stop, spelled out as such.
CTRL_TOKEN=$(pod_env "$CTRL_AGENT" "JIRA_API_TOKEN")
if [ "$CTRL_TOKEN" = "$DUMMY_TOKEN" ]; then
  pass "CONTROL: non-dark pod carries JIRA_API_TOKEN=<dummy> (injection rig works → DARK leg is non-vacuous)"
else
  fail "CONTROL: non-dark pod carries the injected token" \
    "got '${CTRL_TOKEN:-<absent>}', want the dummy — the token was NOT injected, so the DARK leg's 'absent' proves NOTHING (vacuity trap). Check ProjectCache refresh + the project 'env' field shape."
fi

# DARK leg — the token must be SCRUBBED from the dark pod.
DARK_TOKEN=$(pod_env "$DARK_AGENT" "JIRA_API_TOKEN")
if [ -z "$DARK_TOKEN" ]; then
  pass "DARK: dark pod has NO JIRA_API_TOKEN (scrubDarkForbiddenEnv fenced the plaintext write token)"
else
  fail "DARK: JIRA_API_TOKEN absent from the dark pod" \
    "SECURITY REGRESSION: dark pod carries JIRA_API_TOKEN='${DARK_TOKEN}' — the plaintext-env scrub did NOT fire; the privileged Jira write token reached a dark pod"
fi

# Harmless read-only identity SURVIVES on the dark pod (only the write token is
# fenced — a dark agent may still know which Jira exists).
DARK_EMAIL=$(pod_env "$DARK_AGENT" "JIRA_EMAIL")
if [ "$DARK_EMAIL" = "$DUMMY_EMAIL" ]; then
  pass "DARK: JIRA_EMAIL survives on the dark pod (only the write token is fenced, not the read-only identity)"
else
  fail "DARK: JIRA_EMAIL survives" "got '${DARK_EMAIL:-<absent>}', want '$DUMMY_EMAIL' — the scrub is over-broad (it must fence ONLY JIRA_API_TOKEN)"
fi

# ── 5. LINCHPIN leg — the markers joining the pod + network fences ──
blue "[5/5] linchpin: GARDENIO_DARK=true env + gardenio.io/dark label on the dark pod"
# GARDENIO_DARK: the in-pod signal gb reads to build a READ-ONLY Jira client
# (client-layer leg). Absent ⇒ a dark agent could build a WRITE client.
DARK_MARK=$(pod_env "$DARK_AGENT" "GARDENIO_DARK")
if [ "$DARK_MARK" = "true" ]; then
  pass "DARK: GARDENIO_DARK=true present (gb builds a read-only Jira client — client-layer leg)"
else
  fail "DARK: GARDENIO_DARK=true present" "got '${DARK_MARK:-<absent>}' — the client-layer read-only marker did not reach the dark pod"
fi
# gardenio.io/dark label: the podSelector the dark-agent-egress NetworkPolicy keys
# on — the JOIN between the pod leg and the network leg. Without it the network
# fence would not select this pod.
DARK_LABEL=$(pod_label "$DARK_AGENT" "gardenio.io/dark")
if [ "$DARK_LABEL" = "true" ]; then
  pass "DARK: pod label gardenio.io/dark=true present (the dark-agent-egress podSelector — joins pod+network legs)"
else
  fail "DARK: pod label gardenio.io/dark=true" "got '${DARK_LABEL:-<absent>}' — the dark-egress NetworkPolicy would NOT select this pod"
fi

# NEGATIVE CONTROL on the markers: the non-dark pod carries NEITHER, proving the
# fence is dark-scoped, not a global change.
CTRL_MARK=$(pod_env "$CTRL_AGENT" "GARDENIO_DARK")
CTRL_LABEL=$(pod_label "$CTRL_AGENT" "gardenio.io/dark")
if [ -z "$CTRL_MARK" ] && [ -z "$CTRL_LABEL" ]; then
  pass "CONTROL: non-dark pod carries neither GARDENIO_DARK nor gardenio.io/dark (fence is dark-scoped, not global)"
else
  fail "CONTROL: non-dark pod is unmarked" "GARDENIO_DARK='${CTRL_MARK}' label='${CTRL_LABEL}' — the dark markers leaked onto a non-dark pod"
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed — the dark-pod Jira write-token POD scrub holds end-to-end"
  echo "════════════════════════════════════════════════════"
  exit 0
fi
red " ✗ $FAIL/$total assertions failed"
for e in "${ERRORS[@]}"; do red "   - $e"; done
echo "════════════════════════════════════════════════════"
exit 1
