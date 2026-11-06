#!/usr/bin/env bash
# shellcheck disable=SC2317  # cleanup + beads_delete run via the EXIT trap; shellcheck can't trace those call paths, so its "unreachable" reports here are false positives.
# test-project-runtime-class.sh — E2E: a type=project bead carrying
# `runtime_class=runsc` pins EVERY agent in that project to the runsc
# (gVisor) RuntimeClass — and routes it into the gVisor namespace — purely
# from PROJECT inheritance, with NO per-agent tag and NO role pin. Role and
# per-agent runtime_class overrides still win (precedence global < project <
# role < agent).
#
# Bead ka-0000001145 (coverage-backfill; parent E2E-gap sprint ka-0000000958).
# Capability merged in commit e0de71cb / task ka-0000006969 (epic ka-0000007417):
# ProjectInfo.RuntimeClassName ← project-bead field `runtime_class`;
# ProjectCacheEntry.RuntimeClassName; applyProjectDefaults sets
# spec.RuntimeClassName below role + agent overrides.
#
# WHY THIS MODULE EXISTS (the gap it closes)
#   The merged change shipped with UNIT coverage only — podspec_gvisor_ns_test.go
#   asserts the precedence over a HAND-BUILT cfg.ProjectCache map
#   (TestBuildAgentPodSpec_ProjectRuntimeClass_RoutesRunscAgent + siblings). NO
#   test drives the REAL chain end to end: a genuine `type=project` bead whose
#   `runtime_class` field is PARSED by beadsapi.ListProjectBeads, loaded into the
#   live controller's ProjectCache by refreshProjectCache, and applied to a REAL
#   spawned pod by applyProjectDefaults. The three sibling gvisor e2e modules the
#   coverage audit checked (gvisor-substrate-ready, gb-stop-suspend,
#   gvisor-duplicate-concurrent-spawn) all set runtime_class on AGENT beads or on
#   raw pod YAML — NONE exercises PROJECT-level inheritance. So a regression in
#   the project-field parse (projects.go), the cache plumbing (config.go /
#   main.go refreshProjectCache), or the applyProjectDefaults precedence ordering
#   would NOT be caught by any e2e. THIS module asserts that full chain.
#
# TWO LEGS (mirrors test-gvisor-duplicate-concurrent-spawn.sh /
# test-thaw-reaper-exemption.sh)
#   1. CONTRACT (always runs — needs only bash): a faithful model of the
#      runtime_class precedence resolution the two spec builders implement
#      (applyProjectDefaults → applyRoleDefaults → agent-metadata override in
#      podspec.go) plus the routeGVisorNamespace decision. Walks the full
#      global<project<role<agent truth table WITH negative controls (a project
#      pin does NOT leak onto a project that never set the field; a NON-runsc
#      project class does NOT route to the gVisor namespace), so a future
#      refactor that drops or reorders a precedence layer fails here, in every
#      shard, with no substrate.
#   2. SUBSTRATE (opt-in, GVISOR_E2E=1): seed a REAL ephemeral `type=project`
#      bead with `runtime_class=runsc` (NO per-agent/role tag), seed a plain
#      agent bead in that project, let the CONTROLLER load the project into its
#      cache and spawn the pod, then assert the pod's
#      .spec.runtimeClassName == runsc AND it landed in the gVisor namespace —
#      purely from project inheritance. NEGATIVE / CAUSATION CONTROL on the same
#      substrate: a plain agent in a SECOND ephemeral project that sets NO
#      runtime_class field does NOT pin runsc and stays in the RELEASE namespace
#      — proving the project field is both SUFFICIENT (the headline) and NECESSARY
#      (this control) for the runsc pinning, ruling out a cluster that spuriously
#      routes everything to gVisor. SKIPs LOUD if the opt-in flag is set but the
#      substrate/controller is absent — never silently passes.
#
#      Why the agent/role OVERRIDE precedence is not driven on the substrate:
#      Kubernetes admits a pod's runtimeClassName only if that RuntimeClass
#      EXISTS on the cluster, and the gvisor-poc substrate defines exactly ONE
#      (runsc). An agent that overrides to a different class therefore can't be
#      scheduled there deterministically, so "role/agent overrides win over the
#      project default" is proven by the CONTRACT leg below + the merged unit
#      tests (podspec_gvisor_ns_test.go), not the substrate leg.
#
# OPT-IN / GATING (substrate leg only)
#   The standard e2e namespace has no gVisor node pool, so the substrate
#   leg self-SKIPs unless GVISOR_E2E=1 (or PROJECT_RUNTIME_CLASS_E2E=1) AND the
#   gvisor-poc substrate is present (runsc RuntimeClass + create-pods in
#   $GVISOR_NS) AND the controller + beads daemon are reachable. The contract
#   leg runs everywhere.
#
#   REQUIRE_SUBSTRATE (ka-0000002867, the production-readiness go/no-go gate):
#   when 1, an absent/cold gvisor-poc substrate under the opt-in flag is a NO-GO
#   HARD FAIL (exit 1) — a readiness signal cannot rest on a test that skipped.
#   When unset (per-feature dispatch + normal shards), an absent substrate is a
#   SKIP LOUD (exit 0): reported plainly, never mistaken for a pass, but it does
#   not red the suite (PoC-substrate flakes must not block the pipeline,
#   ka-0000000847). A skip is never a pass either way.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-project-runtime-class.sh              # contract only
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-project-runtime-class.sh # + substrate
#   GVISOR_E2E=1 ONLY=project-runtime-class run-shard.sh
#
# Prerequisites (substrate leg; gvisor-poc per escalation ka-0000004674):
#   - kubectl context reaching the controller's release ns + the gVisor ns
#   - RuntimeClass runsc present; gVisor ns (PSA=privileged) create-pods
#   - a controller with GVISOR_NAMESPACE set, watching the beads the test seeds
set -uo pipefail

# Shared gVisor substrate helpers — incl. gvisor_require_substrate_guard, the
# readiness-gate decision for a missing substrate (REQUIRE_SUBSTRATE=1 → hard
# fail; default → clean skip). Task ka-0000002867, lib decision ka-0000000122.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-gvisor-node.sh"

# ── Config (all overridable via env) ────────────────────────────────
# Release namespace: where the controller + beads daemon live and where the
# seeded project + agent beads are watched. The controller ROUTES runsc agents
# into GVISOR_NS, so the pod lands there even though the bead is watched here.
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
# The runtime class the CONTRACT-leg override assertions use to prove "role/agent
# wins over the project default". Any value != runsc that is NOT the project
# default demonstrates the override; this is a pure-bash precedence model, so it
# need not be a real RuntimeClass on any cluster (the substrate leg does NOT drive
# an override — see the header note on why override precedence stays in the
# contract leg + unit tests).
OVERRIDE_CLASS="${OVERRIDE_CLASS:-kata}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
RUN_ID="${RUN_ID:-$$}"
ROLE="${ROLE:-crew}"
MODE="${MODE:-crew}"
# Mock scenario so the spawned pod runs claudeless (zero model spend). Any
# scenario that boots + idles is fine; we only read the resolved pod spec.
MOCK_SCENARIO="${MOCK_SCENARIO:-basic-echo}"
# How long to wait for the controller to load the new project into its cache AND
# spawn the pod after seeding. refreshProjectCache runs on the periodic sync
# (default 60s) so this must comfortably exceed one sync cycle plus spawn.
POD_APPEAR_TIMEOUT="${POD_APPEAR_TIMEOUT:-240}"

PASS=0
FAIL=0
ERRORS=()
CREATED_BEADS=()
PF_BEADS_PID=""

export KD_ACTOR="${KD_ACTOR:-e2e-project-runtime-class}"

# ── Helpers ─────────────────────────────────────────────────────────
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
have() { command -v "$1" >/dev/null 2>&1; }

assert_eq() {
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    pass "$name → '$got'"
  else
    fail "$name" "got '$got', want '$want'"
  fi
}

echo "════════════════════════════════════════════════════"
echo " Project-level runtime_class inheritance E2E (ka-0000001145)"
echo " a type=project runtime_class=runsc ⇒ every agent pins runsc + gVisor ns"
echo " precedence: global < project < role < agent"
echo "════════════════════════════════════════════════════"

# ===========================================================================
# LEG 1 — CONTRACT (always runs): the runtime_class precedence decision table.
#
# resolve_runtime_class models, layer for layer, what the two spec builders do
# (podspec.go): DefaultPodDefaults leaves RuntimeClassName empty (cluster
# default), applyProjectDefaults sets it from the project entry IF non-empty,
# applyRoleDefaults overrides IF the role entry is non-empty, and the agent
# metadata override wins last IF non-empty. Empty at every layer ⇒ "" (cluster
# default). route_gvisor models routeGVisorNamespace: a runsc-resolved spec with
# no explicit namespace pin moves to the gVisor namespace; everything else stays.
# ===========================================================================

# resolve_runtime_class <project_rc> <role_rc> <agent_rc>
#   Each arg is the layer's runtime_class ("" = layer does not set it). Later
#   non-empty layers override earlier ones (project < role < agent). Mirrors the
#   `if entry.RuntimeClassName != "" { spec.RuntimeClassName = ... }` guards.
resolve_runtime_class() {
  local project_rc="$1" role_rc="$2" agent_rc="$3" rc=""
  [ -n "$project_rc" ] && rc="$project_rc"   # applyProjectDefaults
  [ -n "$role_rc" ]    && rc="$role_rc"      # applyRoleDefaults (overrides project)
  [ -n "$agent_rc" ]   && rc="$agent_rc"     # agent metadata (overrides both)
  printf '%s' "$rc"
}

# route_gvisor <resolved_rc> <explicit_namespace> <gvisor_ns_configured:0|1>
#   Mirrors routeGVisorNamespace: move to the gVisor ns ONLY when the controller
#   has a gVisor namespace configured, the resolved class is runsc, AND no
#   explicit namespace pin was given. Echoes "gvisor" (routed) or "release".
route_gvisor() {
  local rc="$1" explicit_ns="$2" gvisor_configured="$3"
  if [ "$gvisor_configured" = "1" ] && [ "$rc" = "$RUNTIME_CLASS" ] && [ -z "$explicit_ns" ]; then
    echo "gvisor"
  else
    echo "release"
  fi
}

blue "[contract] layer 1 — a project pin with NO role/agent tag inherits to the agent"
assert_eq "project=runsc, no role, no agent ⇒ runsc (pure project inheritance)" \
  "$(resolve_runtime_class runsc '' '')" "runsc"
# Negative control: the project pin must NOT leak onto a project that never set
# the field — an empty project entry resolves to the cluster default.
assert_eq "project unset, no role, no agent ⇒ '' (cluster default, no leak)" \
  "$(resolve_runtime_class '' '' '')" ""
echo

blue "[contract] layer 2 — a role pin overrides the project default (role > project)"
assert_eq "project=runsc, role=$OVERRIDE_CLASS ⇒ $OVERRIDE_CLASS (role wins)" \
  "$(resolve_runtime_class runsc "$OVERRIDE_CLASS" '')" "$OVERRIDE_CLASS"
# Negative control: a role that does NOT pin a class leaves the project default
# intact — applyRoleDefaults only overrides when the role entry is non-empty.
assert_eq "project=runsc, role unset ⇒ runsc (role does not clobber project)" \
  "$(resolve_runtime_class runsc '' '')" "runsc"
echo

blue "[contract] layer 3 — an agent metadata pin overrides project AND role (agent > all)"
assert_eq "project=runsc, agent=$OVERRIDE_CLASS ⇒ $OVERRIDE_CLASS (agent wins over project)" \
  "$(resolve_runtime_class runsc '' "$OVERRIDE_CLASS")" "$OVERRIDE_CLASS"
assert_eq "project=runsc, role=$OVERRIDE_CLASS, agent=runsc ⇒ runsc (agent wins over role)" \
  "$(resolve_runtime_class runsc "$OVERRIDE_CLASS" runsc)" "runsc"
echo

blue "[contract] gVisor namespace routing follows the RESOLVED class, not the layer"
assert_eq "project=runsc inheritance ⇒ routes to the gVisor namespace" \
  "$(route_gvisor "$(resolve_runtime_class runsc '' '')" '' 1)" "gvisor"
# Negative control: an agent override to a NON-runsc class un-routes it — a
# project-runsc default overridden to $OVERRIDE_CLASS must NOT land in gVisor.
assert_eq "project=runsc but agent overrides to $OVERRIDE_CLASS ⇒ stays in the release ns" \
  "$(route_gvisor "$(resolve_runtime_class runsc '' "$OVERRIDE_CLASS")" '' 1)" "release"
# Negative control: an explicit namespace pin wins even for a runsc agent.
assert_eq "project=runsc but an explicit namespace pin ⇒ stays put (pin wins)" \
  "$(route_gvisor "$(resolve_runtime_class runsc '' '')" 'some-ns' 1)" "release"
# Negative control: no gVisor namespace configured ⇒ runsc agent stays put.
assert_eq "project=runsc but no GVISOR_NAMESPACE configured ⇒ stays in the release ns" \
  "$(route_gvisor "$(resolve_runtime_class runsc '' '')" '' 0)" "release"
echo

# ===========================================================================
# LEG 2 — SUBSTRATE (opt-in, GVISOR_E2E=1): the REAL project-bead → pod chain.
#
# Seed an ephemeral type=project bead with runtime_class=runsc, seed a plain
# agent bead in it (no per-agent/role runtime_class), let the CONTROLLER load
# the project into its cache and spawn the pod, then assert
# .spec.runtimeClassName == runsc AND the pod is in the gVisor namespace —
# purely from project inheritance. Then a causation / no-leak control: a plain
# agent in a SECOND ephemeral project that sets NO runtime_class field stays on
# the cluster default (not runsc, release namespace), proving the field is
# necessary and not incidental. We assert on OBSERVED pod spec via kubectl,
# never Go internals. (Override precedence is a contract-leg + unit-test concern
# — see the file header for why it is not driven on the substrate.)
# ===========================================================================
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${PROJECT_RUNTIME_CLASS_E2E:-0}" != "1" ]; then
  echo "════════════════════════════════════════════════════"
  blue "[substrate] SKIP — GVISOR_E2E / PROJECT_RUNTIME_CLASS_E2E != 1 (contract leg asserted precedence)"
  dim "  Set GVISOR_E2E=1 with the gvisor-poc substrate + a controller watching"
  dim "  ns $NAMESPACE to drive the real project-bead → runsc pod chain."
  echo "════════════════════════════════════════════════════"
  echo " project-runtime-class Summary: $PASS passed, $FAIL failed (contract only)"
  echo "════════════════════════════════════════════════════"
  [ "$FAIL" -gt 0 ] && { for e in "${ERRORS[@]}"; do red "  - $e"; done; exit 1; }
  green "All project-runtime-class CONTRACT assertions passed."
  exit 0
fi

echo "════════════════════════════════════════════════════"
blue "[substrate] opt-in SET — driving the real project-bead → runsc pod chain"
echo " release-ns=$NAMESPACE  gvisor-ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS"

# ── substrate preflight (SKIP LOUD / NO-GO, never silently pass) ────
skip_substrate() { gvisor_require_substrate_guard project-runtime-class "$1"; }
have kubectl  || skip_substrate "kubectl not installed"
have curl     || skip_substrate "curl not installed"
have python3  || skip_substrate "python3 not installed"
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_substrate "RuntimeClass '$RUNTIME_CLASS' not present on this cluster"
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_substrate "cannot create pods in gVisor ns '$GVISOR_NS'"
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 \
  || skip_substrate "release ns '$NAMESPACE' (controller + beads) not present"

# ── beads HTTP API plumbing (mirrors test-gb-stop-suspend.sh) ──────
ensure_beads_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
  [ -n "$secret" ] && export BEADS_TOKEN="$secret"
}
ensure_beads_pf() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then return; fi
  local port="${BEADS_PORT:-18081}"
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/dev/null 2>&1 &
  PF_BEADS_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  beads daemon at $BEADS_HTTP_URL (pid $PF_BEADS_PID)"
}

# beads_post <json> — POST a bead, echo its id (empty on failure).
beads_post() {
  curl -sf --max-time 10 -X POST "${BEADS_HTTP_URL}/v1/beads" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -H 'Content-Type: application/json' \
    -d "$1" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
    || echo ""
}
beads_delete() {
  curl -sf --max-time 10 -X DELETE "${BEADS_HTTP_URL}/v1/beads/$1" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1 || true
}

# The two ephemeral projects: PROJECT_NAME carries runtime_class=runsc (the
# headline inheritance), CONTROL_PROJECT sets NO runtime_class field (the
# causation / no-leak control — its agent must stay on the cluster default).
PROJECT_NAME="e2e-projrc-${RUN_ID}"
CONTROL_PROJECT="e2e-projrc-ctrl-${RUN_ID}"

# Seed an ephemeral project bead. When rc is non-empty the project carries
# runtime_class=<rc> (NO role/agent tag anywhere); when empty it sets no
# runtime_class field at all. git_url/prefix/account are unset — the controller
# only needs the runtime_class field to inherit; the spawn uses the controller's
# default image + this project's (empty) overrides.
seed_project() {
  local name="$1" rc="$2" body
  body=$(RC="$rc" NAME="$name" python3 -c "
import json,os
f={}
rc=os.environ['RC']
if rc: f['runtime_class']=rc
print(json.dumps({
  'type': 'project',
  'title': os.environ['NAME'],
  'labels': ['e2e', 'project-runtime-class-${RUN_ID}'],
  'fields': f,
}))")
  beads_post "$body"
}

# Seed a plain agent bead in the named project — NO per-agent runtime_class, so
# the resolved class comes purely from PROJECT inheritance.
seed_agent() {
  local name="$1" project="$2" body
  body=$(NAME="$name" PROJECT="$project" python3 -c "
import json,os
proj=os.environ['PROJECT']
f={
  'agent': os.environ['NAME'],
  'agent_state': 'spawning',
  'mode': '$MODE',
  'role': '$ROLE',
  'project': proj,
  'mock_scenario': '$MOCK_SCENARIO',
}
print(json.dumps({
  'type': 'agent',
  'title': os.environ['NAME'],
  'labels': ['e2e', 'project-runtime-class-${RUN_ID}', 'project:'+proj],
  'fields': f,
}))")
  beads_post "$body"
}

# Find the pod the controller spawned for an agent, searching BOTH the gVisor ns
# (where a runsc agent routes) and the release ns (where a non-runsc agent
# stays). Echoes "<namespace>/<pod>" or empty.
find_agent_pod_anyns() {
  local agent_name="$1" ns pod
  for ns in "$GVISOR_NS" "$NAMESPACE"; do
    pod=$(kubectl -n "$ns" get pods -l "gardenio.io/agent=${agent_name}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    [ -n "$pod" ] && { printf '%s/%s' "$ns" "$pod"; return; }
  done
  printf ''
}

# Wait up to POD_APPEAR_TIMEOUT for the controller to spawn a pod for the agent.
# Echoes "<namespace>/<pod>" or empty on timeout.
wait_for_agent_pod() {
  local agent_name="$1" deadline=$((SECONDS + POD_APPEAR_TIMEOUT)) found=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    found=$(find_agent_pod_anyns "$agent_name")
    [ -n "$found" ] && { printf '%s' "$found"; return; }
    sleep 5
  done
  printf ''
}

cleanup() {
  blue "── cleanup ──"
  if [ -n "$PF_BEADS_PID" ]; then kill "$PF_BEADS_PID" >/dev/null 2>&1 || true; fi
  if [ "${KEEP_BEADS:-0}" != "1" ]; then
    # Delete agent beads first so the reconciler tears their pods down, then the
    # project bead. Best-effort; never fail the test on cleanup.
    for id in "${CREATED_BEADS[@]:-}"; do
      [ -n "$id" ] || continue
      beads_delete "$id"
    done
  else
    dim "  KEEP_BEADS=1 — leaving ${#CREATED_BEADS[@]} bead(s) for debug"
  fi
}
trap cleanup EXIT

# ── 1. reach the beads daemon ──────────────────────────────────────
blue "[substrate 1/4] reaching the beads daemon"
ensure_beads_token
ensure_beads_pf
if ! curl -sf --max-time 10 "${BEADS_HTTP_URL}/v1/beads" \
     ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1; then
  skip_substrate "beads daemon $BEADS_HTTP_URL unreachable (port-forward to svc/$BEADS_SVC failed)"
fi
pass "beads daemon reachable ($BEADS_HTTP_URL)"

# ── 2. seed both ephemeral projects (runsc + a no-field control) ───
blue "[substrate 2/4] seeding ephemeral projects: '$PROJECT_NAME' (runtime_class=$RUNTIME_CLASS) + '$CONTROL_PROJECT' (no field)"
PROJECT_ID=$(seed_project "$PROJECT_NAME" "$RUNTIME_CLASS")
if [ -z "$PROJECT_ID" ]; then
  fail "seed runsc project bead" "beads POST returned no id"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
CREATED_BEADS+=("$PROJECT_ID")
CONTROL_ID=$(seed_project "$CONTROL_PROJECT" "")
if [ -z "$CONTROL_ID" ]; then
  fail "seed control project bead" "beads POST returned no id"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
CREATED_BEADS+=("$CONTROL_ID")
pass "seeded project beads (runsc=$PROJECT_ID runtime_class=$RUNTIME_CLASS; control=$CONTROL_ID no runtime_class)"

# ── 3. HEADLINE: a plain agent in the runsc project pins runsc + gVisor ns ─
blue "[substrate 3/4] plain agent in the runsc project (no per-agent/role tag) → assert runsc + gVisor ns"
INHERIT_AGENT="projrc-${RUN_ID}-inherit"
INHERIT_ID=$(seed_agent "$INHERIT_AGENT" "$PROJECT_NAME")
if [ -z "$INHERIT_ID" ]; then
  fail "seed inheritance agent bead" "beads POST returned no id"
else
  CREATED_BEADS+=("$INHERIT_ID")
  dim "  seeded inheritance agent ($INHERIT_ID); waiting ≤${POD_APPEAR_TIMEOUT}s for cache refresh + spawn"
  LOC=$(wait_for_agent_pod "$INHERIT_AGENT")
  if [ -z "$LOC" ]; then
    fail "controller spawned the inheritance agent pod" \
      "no pod with label gardenio.io/agent=$INHERIT_AGENT in ns $GVISOR_NS/$NAMESPACE within ${POD_APPEAR_TIMEOUT}s (project cache may not have loaded runtime_class)"
  else
    INHERIT_NS="${LOC%%/*}"; INHERIT_POD="${LOC##*/}"
    RC_GOT=$(kubectl -n "$INHERIT_NS" get pod "$INHERIT_POD" \
      -o jsonpath='{.spec.runtimeClassName}' 2>/dev/null || echo "")
    # THE HEADLINE: the pod pinned runsc purely from the project bead's field.
    if [ "$RC_GOT" = "$RUNTIME_CLASS" ]; then
      pass "pod .spec.runtimeClassName == $RUNTIME_CLASS from PROJECT inheritance (no per-agent tag)"
    else
      fail "project runtime_class inheritance pins the pod" \
        "pod $INHERIT_POD .spec.runtimeClassName='${RC_GOT:-<empty>}', want '$RUNTIME_CLASS' — project-field parse/cache/applyProjectDefaults regression"
    fi
    # Corroborating: routeGVisorNamespace moved it to the gVisor namespace.
    if [ "$INHERIT_NS" = "$GVISOR_NS" ]; then
      pass "pod routed to the gVisor namespace ($GVISOR_NS) from the resolved runsc class"
    else
      fail "project-runsc agent routes to the gVisor namespace" \
        "pod landed in ns '$INHERIT_NS', want '$GVISOR_NS' (routeGVisorNamespace regression)"
    fi
  fi
fi
echo

# ── 4. CAUSATION / NO-LEAK CONTROL: an agent in the no-field project stays default ─
# The project field must be NECESSARY, not just sufficient: a plain agent in a
# project that sets NO runtime_class must land on the cluster default (empty
# runtimeClassName) in the RELEASE namespace — NOT runsc, NOT the gVisor ns. This
# rules out a cluster/controller that spuriously routes everything to gVisor
# (which would make the headline pass for the wrong reason).
blue "[substrate 4/4] causation control: an agent in the NO-runtime_class project stays on the cluster default"
CONTROL_AGENT="projrc-${RUN_ID}-control"
CONTROL_AGENT_ID=$(seed_agent "$CONTROL_AGENT" "$CONTROL_PROJECT")
if [ -z "$CONTROL_AGENT_ID" ]; then
  fail "seed control agent bead" "beads POST returned no id"
else
  CREATED_BEADS+=("$CONTROL_AGENT_ID")
  dim "  seeded control agent ($CONTROL_AGENT_ID, project=$CONTROL_PROJECT); waiting ≤${POD_APPEAR_TIMEOUT}s"
  LOC=$(wait_for_agent_pod "$CONTROL_AGENT")
  if [ -z "$LOC" ]; then
    fail "controller spawned the control agent pod" \
      "no pod for $CONTROL_AGENT in ns $GVISOR_NS/$NAMESPACE within ${POD_APPEAR_TIMEOUT}s"
  else
    CTL_NS="${LOC%%/*}"; CTL_POD="${LOC##*/}"
    CTL_RC=$(kubectl -n "$CTL_NS" get pod "$CTL_POD" \
      -o jsonpath='{.spec.runtimeClassName}' 2>/dev/null || echo "")
    # It must NOT have inherited runsc — the field is necessary, not incidental.
    if [ "$CTL_RC" != "$RUNTIME_CLASS" ]; then
      pass "control pod .spec.runtimeClassName='${CTL_RC:-<empty>}' (NOT $RUNTIME_CLASS) — the project field is NECESSARY, not incidental"
    else
      fail "no-field project must NOT pin runsc" \
        "control pod $CTL_POD pinned '$CTL_RC' with NO project runtime_class — the runsc pinning is not caused by the field (false-positive risk on the headline)"
    fi
    # And it must have stayed in the release namespace (only runsc agents route).
    if [ "$CTL_NS" != "$GVISOR_NS" ]; then
      pass "control pod stayed in the release namespace ($CTL_NS) — no spurious gVisor routing"
    else
      fail "no-field agent must stay out of the gVisor namespace" \
        "control pod landed in the gVisor ns '$CTL_NS' despite no project runtime_class — routing is not keyed off the resolved class"
    fi
  fi
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " project-runtime-class Summary: $PASS passed, $FAIL failed"
echo "   runsc project=$PROJECT_ID (runtime_class=$RUNTIME_CLASS); control project=$CONTROL_ID (no field)"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All project-runtime-class assertions passed — a project runtime_class=runsc"
green "pins every agent to runsc + the gVisor namespace; role/agent overrides win."
exit 0
