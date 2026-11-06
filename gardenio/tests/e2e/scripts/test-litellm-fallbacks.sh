#!/usr/bin/env bash
# test-litellm-fallbacks.sh — LiteLLM router_settings.fallbacks arm E2E,
# asserted against the DEPLOYED stack (task ka-0000003835, epic ka-0000000958).
#
# WHY THIS EXISTS — the render test cannot see this regression.
# `gardenio/helm/gardenio/tests/litellm_router_alias_test.go` is a chart-render
# UNIT test over DEFAULT values. It proves the template renders the arm when
# nothing overrides it. It cannot observe the deployed ConfigMap, so any
# deploy-values/*.yaml override that drops or reshapes `fallbacks`, a stale
# deploy that predates the arm commit, or a runtime /config/update that clobbers
# router_settings renders that suite green while production has NO fallback arm.
# That failure already happened: the exact shape of open bug ka-0000002264 and
# ka-0000007458 — an agent with an exhausted key spin-loops on a 429 "monthly
# spend limit" while its pod stays Running + health-probe-green (a rate-limit
# livelock invisible to `gb news --active-only`, advice ka-0000004373). It fails
# SILENT, not loud, and burns hours of dark lane time.
#
# So this module reads the ACTUAL running router, not `helm template`:
#   GET /get/config/callbacks  ->  .router_settings.fallbacks
# LiteLLM only surfaces `fallbacks` under `router_settings` in that response
# when it was nested correctly in the loaded config; a top-level `fallbacks:`
# key is silently ignored and never appears there (same trap as
# model_group_alias, ka-0000005911). Reading the live router is therefore both
# the value AND the nesting proof.
#
# Legs (all against the DEPLOYED LiteLLM, never the rendered chart):
#   1. router_settings.fallbacks exposes an arm keyed EXACTLY
#      claude-opus-4-8-oauth whose ordered target list contains
#      claude-opus-5-5-oauth (the crew-e2e opus-4-8 -> opus-5.5 spill, ka-0000007458).
#   2. That arm is NESTED under router_settings (asserted structurally: the live
#      router surfaces it there, AND the deployed ConfigMap indents `fallbacks:`
#      under `router_settings:`), not as a silently-ignored top-level key.
#   3. Every fallback PRIMARY key and TARGET resolves to a model group that
#      actually exists in the live model_list (GET /v1/models). An arm pointing
#      at an undefined group is indistinguishable from no arm at runtime — the
#      router 400s "no healthy deployments" on that route.
#
# NATURAL MUTATION PROOF (advice ka-0000001521, born-vacuous guard). A test that
# passes before the fix proves nothing. This module distinguishes present-arm
# from missing-arm on a REAL deployed stack: run it against a namespace whose
# LiteLLM predates commit 9843c4a0f (or was deployed from values with the arm
# deleted) and leg 1 goes RED; run it against a namespace carrying the arm and
# it goes GREEN — while the sibling litellm-cost-tracking legs (health, key
# mint, spend) stay GREEN on BOTH, proving leg 1 tests the arm specifically, not
# general LiteLLM health. See the MR PROOF block for the captured run.
#
# Usage:
#   ./tests/e2e/scripts/test-litellm-fallbacks.sh
#
#   # Against a specific namespace / release (e.g. AWS POC):
#   NAMESPACE=gardenio-system RELEASE=gardenio \
#     LITELLM_MASTER_KEY=$MK \
#     ./tests/e2e/scripts/test-litellm-fallbacks.sh
#
# Env overrides:
#   NAMESPACE            target namespace (default gardenio-e2e)
#   RELEASE              Helm release name (default = NAMESPACE; resolves
#                        the *-litellm Service + *-litellm-config ConfigMap)
#   LITELLM_MASTER_KEY   supply the LiteLLM master key directly
#   MASTER_KEY_SECRET    name of the K8s secret holding the master key (key: token)
#   EXPECT_PRIMARY       fallback arm primary key to require
#                        (default claude-opus-4-8-oauth)
#   EXPECT_TARGET        target that must appear in EXPECT_PRIMARY's list
#                        (default claude-opus-5-5-oauth)
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - curl + python3 installed
#   - LiteLLM deployed in the target namespace (litellm.enabled=true)
#
# Exit 0 = every leg passed (or clean SKIP when LiteLLM is disabled).
# Non-zero = a fallbacks-arm regression on the DEPLOYED router.

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
# RELEASE is the Helm release name. In per-run e2e namespaces release name and
# namespace coincide (e.g. gardenio-e2e-0), so it defaults to NAMESPACE. On the
# AWS POC the release is "gardenio" while the namespace is "gardenio-system".
RELEASE="${RELEASE:-${NAMESPACE}}"
LITELLM_SVC="${LITELLM_SVC:-${RELEASE}-litellm}"
LITELLM_CONFIGMAP="${LITELLM_CONFIGMAP:-${RELEASE}-litellm-config}"

# The arm this module exists to guard (crew-e2e opus-4-8 -> opus-5.5 spill). Both
# are chart defaults (litellm.fallbacks in values.yaml); overridable so an
# overlay that legitimately reshapes the default can re-point the assertion.
EXPECT_PRIMARY="${EXPECT_PRIMARY:-claude-opus-4-8-oauth}"
EXPECT_TARGET="${EXPECT_TARGET:-claude-opus-5-5-oauth}"

# A distinct local port so a concurrent litellm-cost-tracking port-forward in
# the same shard never collides (that module uses 19094).
LITELLM_PORT="${LITELLM_PORT:-19099}"
LITELLM_URL=""
MASTER_KEY=""
PF_LITELLM_PID=""

PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

# ── Cleanup ───────────────────────────────────────────────────────
# shellcheck disable=SC2317  # invoked indirectly via `trap ... EXIT`
cleanup() {
  if [ -n "$PF_LITELLM_PID" ]; then
    kill "$PF_LITELLM_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Self-skip when LiteLLM is not deployed ───────────────────────
# The bundled litellm pod can be disabled via helm (litellm.enabled=false) in
# environments that use an external LiteLLM URL. When no litellm pod exists,
# skip cleanly (exit 0) so this module does not fail the suite — mirrors
# litellm-cost-tracking's guard. Flipping litellm.enabled back on re-arms it.
skip_if_litellm_disabled() {
  local pod_names
  pod_names=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
    -o name 2>/dev/null || true)
  if [ -z "$pod_names" ]; then
    blue "═══════════════════════════════════════════════════"
    blue " litellm-fallbacks E2E tests"
    blue "═══════════════════════════════════════════════════"
    echo
    green "SKIP: No LiteLLM pod found in namespace '${NAMESPACE}'"
    dim "  The bundled LiteLLM proxy is disabled (litellm.enabled=false)."
    dim "  This test runs automatically when litellm.enabled is flipped back on."
    blue "═══════════════════════════════════════════════════"
    if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "no LiteLLM pod in $NAMESPACE (litellm.enabled=false)" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
    exit 0
  fi
}

# ── Setup: wait for LiteLLM pod Ready ────────────────────────────
# Pod phase=Running does NOT mean the readinessProbe passed; Services only route
# to Ready pods, so port-forwarding before Ready yields HTTP 000. LiteLLM's
# Prisma schema sync on first boot can take 60–120s. Includes the recycle
# self-heal litellm-cost-tracking uses (ka-0000007858 / gd-Ie2mByMaht): a fresh
# ephemeral litellm can wedge at readiness 503 for 10–15m but ALWAYS clears when
# the Deployment respawns it, so delete-once-and-re-wait rather than aborting.
LITELLM_READY_TIMEOUT="${LITELLM_READY_TIMEOUT:-330s}"

_litellm_wait_once() {
  kubectl -n "$NAMESPACE" wait --for=condition=Ready pod \
    -l app.kubernetes.io/component=litellm --timeout="$LITELLM_READY_TIMEOUT" \
    >/tmp/gd-wait-litellm-fb.log 2>&1
}

_dump_litellm_diag() {
  dim "  --- pod status ---"
  kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
    -o wide 2>&1 | sed 's/^/    /' || true
  dim "  --- last 40 lines of pod logs ---"
  kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/component=litellm \
    --tail=40 --all-containers=true 2>&1 | sed 's/^/    /' || true
}

wait_for_litellm_ready() {
  blue "Waiting for LiteLLM pod Ready in ${NAMESPACE} (timeout ${LITELLM_READY_TIMEOUT})..."
  if _litellm_wait_once; then
    dim "  LiteLLM pod is Ready"
    return 0
  fi
  red "  LiteLLM pod not Ready within ${LITELLM_READY_TIMEOUT}; recycling stuck pod and retrying once (cold-start self-heal, gd-Ie2mByMaht)"
  _dump_litellm_diag
  local stuck
  stuck=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
  if [ -n "$stuck" ]; then
    dim "  Deleting not-ready litellm pod(s): $stuck"
    echo "$stuck" | tr ' ' '\n' | xargs -r -n1 -I{} \
      kubectl -n "$NAMESPACE" delete "pod/{}" --ignore-not-found --wait=false 2>/dev/null || true
    sleep 10
  fi
  blue "  Re-waiting for fresh LiteLLM pod Ready (timeout ${LITELLM_READY_TIMEOUT})..."
  if _litellm_wait_once; then
    dim "  LiteLLM pod is Ready after recycle"
    return 0
  fi
  red "  ERROR: LiteLLM pod did not become Ready within ${LITELLM_READY_TIMEOUT} (after one recycle)"
  _dump_litellm_diag
  return 1
}

# ── Setup: port-forward LiteLLM ─────────────────────────────────
setup_litellm_portforward() {
  blue "Port-forwarding ${NAMESPACE} LiteLLM (svc/${LITELLM_SVC})..."
  kubectl -n "$NAMESPACE" port-forward "svc/${LITELLM_SVC}" "${LITELLM_PORT}:4000" \
    >/tmp/kd-pf-litellm-fb.log 2>&1 &
  PF_LITELLM_PID=$!
  sleep 3
  LITELLM_URL="http://localhost:${LITELLM_PORT}"
  dim "  LiteLLM at $LITELLM_URL (pid $PF_LITELLM_PID)"
}

# ── Setup: get LiteLLM master key ────────────────────────────────
# The chart names the secret "litellm-master-key" when ExternalSecrets are
# enabled and "${RELEASE}-litellm-master-key" otherwise; try the non-prefixed
# name first (matches the ESO-managed secret the Deployment references).
setup_master_key() {
  if [ -n "${LITELLM_MASTER_KEY:-}" ]; then
    MASTER_KEY="$LITELLM_MASTER_KEY"
    dim "  Master key loaded from LITELLM_MASTER_KEY env"
    return
  fi
  local candidates=()
  [ -n "${MASTER_KEY_SECRET:-}" ] && candidates+=("$MASTER_KEY_SECRET")
  candidates+=("litellm-master-key" "${NAMESPACE}-litellm-master-key" "gardenio-litellm-master-key")
  for secret_name in "${candidates[@]}"; do
    MASTER_KEY=$(kubectl -n "$NAMESPACE" get secret "$secret_name" \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    if [ -n "$MASTER_KEY" ]; then
      dim "  Master key loaded from secret $secret_name"
      return
    fi
  done
  red "  ERROR: Cannot load LiteLLM master key from any of: ${candidates[*]}"
  red "  Set LITELLM_MASTER_KEY or MASTER_KEY_SECRET to override."
  exit 1
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  skip_if_litellm_disabled

  blue "═══════════════════════════════════════════════════"
  blue " litellm-fallbacks E2E tests"
  blue "  namespace=$NAMESPACE  expect arm: $EXPECT_PRIMARY -> $EXPECT_TARGET"
  blue "═══════════════════════════════════════════════════"

  if ! wait_for_litellm_ready; then
    red "ABORT: LiteLLM not Ready; see diagnostics above"
    exit 1
  fi
  setup_litellm_portforward
  setup_master_key

  # Pull the live router config ONCE and project many times (advice
  # ka-0000002706 fetch-once/project-many).
  blue "Fetching live router config: GET /get/config/callbacks"
  local callbacks_out callbacks_rc
  callbacks_out=$(curl -s "${LITELLM_URL}/get/config/callbacks" \
    -H "Authorization: Bearer ${MASTER_KEY}" 2>/dev/null) && callbacks_rc=0 || callbacks_rc=$?
  if [ "$callbacks_rc" -ne 0 ] || [ -z "$callbacks_out" ]; then
    red "ABORT: could not fetch /get/config/callbacks (curl rc=$callbacks_rc)"
    exit 1
  fi

  # The live model_list (routable model_name / group ids) for target resolution.
  blue "Fetching live model_list: GET /v1/models"
  local models_out models_rc
  models_out=$(curl -s "${LITELLM_URL}/v1/models" \
    -H "Authorization: Bearer ${MASTER_KEY}" 2>/dev/null) && models_rc=0 || models_rc=$?
  if [ "$models_rc" -ne 0 ] || [ -z "$models_out" ]; then
    red "ABORT: could not fetch /v1/models (curl rc=$models_rc)"
    exit 1
  fi
  echo

  # ── Leg 1: the opus-4-8 -> opus-5 arm is present on the LIVE router ──
  # Parse .router_settings.fallbacks (List[Dict[str,List[str]]]) and require an
  # arm keyed EXACTLY $EXPECT_PRIMARY whose ordered list contains $EXPECT_TARGET.
  blue "Leg 1: router_settings.fallbacks has $EXPECT_PRIMARY -> [...$EXPECT_TARGET...]"
  local leg1
  leg1=$(EXPECT_PRIMARY="$EXPECT_PRIMARY" EXPECT_TARGET="$EXPECT_TARGET" \
    python3 - "$callbacks_out" <<'PY'
import json, os, sys
try:
    d = json.loads(sys.argv[1])
except Exception as e:
    print(f"UNPARSEABLE: {e}")
    sys.exit(0)
rs = d.get("router_settings")
if not isinstance(rs, dict):
    print("NO_ROUTER_SETTINGS")
    sys.exit(0)
fb = rs.get("fallbacks")
if fb is None:
    print("FALLBACKS_ABSENT")  # not surfaced => not nested under router_settings
    sys.exit(0)
if not isinstance(fb, list):
    print(f"FALLBACKS_NOT_LIST:{type(fb).__name__}")
    sys.exit(0)
primary = os.environ["EXPECT_PRIMARY"]
target = os.environ["EXPECT_TARGET"]
arm = None
for entry in fb:
    if isinstance(entry, dict) and primary in entry:
        arm = entry[primary]
        break
if arm is None:
    keys = sorted({k for e in fb if isinstance(e, dict) for k in e})
    print(f"ARM_MISSING present_primaries={keys}")
    sys.exit(0)
if not isinstance(arm, list):
    print(f"ARM_NOT_LIST:{type(arm).__name__}")
    sys.exit(0)
if target not in arm:
    print(f"TARGET_MISSING arm={arm}")
    sys.exit(0)
print(f"OK arm={arm}")
PY
)
  if [[ "$leg1" == OK* ]]; then
    pass "live router carries $EXPECT_PRIMARY -> $EXPECT_TARGET (${leg1#OK })"
  else
    fail "live router carries $EXPECT_PRIMARY -> $EXPECT_TARGET" "$leg1"
  fi
  echo

  # ── Leg 2: the arm is NESTED under router_settings, not top-level ──
  # Two independent proofs (advice ka-0000005924 measure-two-ways):
  #   (a) RUNTIME: the live router surfaced fallbacks UNDER router_settings.
  #       A top-level `fallbacks:` in the config is silently dropped and never
  #       appears here (ka-0000005911), so its presence under router_settings IS
  #       the runtime nesting proof.
  #   (b) STRUCTURAL: the deployed ConfigMap indents `fallbacks:` under
  #       `router_settings:`. Dependency-free indent parse (no pyyaml in the CI
  #       image) — catches a chart regression that emits a top-level key even
  #       before the router reloads.
  blue "Leg 2: fallbacks is nested under router_settings (not a top-level key)"
  local leg2a
  leg2a=$(python3 - "$callbacks_out" <<'PY'
import json, sys
try:
    d = json.loads(sys.argv[1])
except Exception as e:
    print(f"UNPARSEABLE: {e}")
    sys.exit(0)
rs = d.get("router_settings")
under = isinstance(rs, dict) and isinstance(rs.get("fallbacks"), list) and len(rs["fallbacks"]) > 0
top = isinstance(d.get("fallbacks"), list) and len(d.get("fallbacks")) > 0
if under:
    print("OK")
elif top:
    print("TOP_LEVEL_ONLY: fallbacks present at response top level but NOT under router_settings — LiteLLM ignores it")
else:
    print("NEITHER: no non-empty fallbacks under router_settings")
PY
)
  if [[ "$leg2a" == OK* ]]; then
    pass "runtime: live router surfaced fallbacks under router_settings"
  else
    fail "runtime: live router surfaced fallbacks under router_settings" "$leg2a"
  fi

  # (b) structural ConfigMap parse — best-effort; only asserts when the
  # ConfigMap is readable (RBAC/name may differ on some stacks).
  local cfg_yaml
  cfg_yaml=$(kubectl -n "$NAMESPACE" get configmap "$LITELLM_CONFIGMAP" \
    -o jsonpath='{.data.config\.yaml}' 2>/dev/null || true)
  if [ -z "$cfg_yaml" ]; then
    dim "  (ConfigMap $LITELLM_CONFIGMAP not readable — skipping structural indent check; runtime proof above stands)"
  else
    local leg2b
    leg2b=$(python3 - "$cfg_yaml" <<'PY'
import sys
lines = sys.argv[1].split("\n")
# Find the indent of `router_settings:` and of the FIRST `fallbacks:` key.
# A correctly-nested fallbacks sits at a strictly-greater indent than
# router_settings AND appears before the next 0-indent sibling after it.
rs_indent = None
in_router = False
result = "FALLBACKS_KEY_NOT_FOUND"
for line in lines:
    if not line.strip() or line.lstrip().startswith("#"):
        continue
    indent = len(line) - len(line.lstrip(" "))
    stripped = line.strip()
    key = stripped.split(":", 1)[0] if ":" in stripped else ""
    if indent == 0:
        in_router = (key == "router_settings")
        rs_indent = 0 if in_router else None
        continue
    if key == "fallbacks":
        if in_router and rs_indent is not None and indent > rs_indent:
            result = "OK"
        else:
            result = f"NOT_NESTED indent={indent} in_router={in_router}"
        break
print(result)
PY
)
    if [[ "$leg2b" == OK* ]]; then
      pass "structural: deployed ConfigMap indents fallbacks under router_settings"
    else
      fail "structural: deployed ConfigMap indents fallbacks under router_settings" "$leg2b"
    fi
  fi
  echo

  # ── Leg 3: every fallback PRIMARY and TARGET resolves to a live group ──
  # An arm whose primary or target is not a defined model_name in the live
  # model_list is inert (router 400s "no healthy deployments") — indistinguishable
  # from no arm at runtime. Cross-check the parsed fallbacks against /v1/models.
  blue "Leg 3: every fallback primary + target resolves to a live model group"
  local leg3
  leg3=$(python3 - "$callbacks_out" "$models_out" <<'PY'
import json, sys
try:
    cb = json.loads(sys.argv[1])
    mods = json.loads(sys.argv[2])
except Exception as e:
    print(f"UNPARSEABLE: {e}")
    sys.exit(0)
groups = {m.get("id") for m in mods.get("data", []) if isinstance(m, dict)}
if not groups:
    print("NO_MODEL_LIST: /v1/models returned no ids")
    sys.exit(0)
fb = cb.get("router_settings", {}).get("fallbacks")
if not isinstance(fb, list) or not fb:
    print("NO_FALLBACKS_TO_CHECK")
    sys.exit(0)
dangling = []
checked = 0
for entry in fb:
    if not isinstance(entry, dict):
        continue
    for primary, targets in entry.items():
        checked += 1
        if primary not in groups:
            dangling.append(f"primary '{primary}' not in model_list")
        if not isinstance(targets, list):
            dangling.append(f"targets of '{primary}' not a list")
            continue
        for t in targets:
            if t not in groups:
                dangling.append(f"target '{t}' (of '{primary}') not in model_list")
if dangling:
    print("DANGLING: " + "; ".join(dangling))
else:
    print(f"OK checked {checked} arm(s), all primaries+targets resolve")
PY
)
  if [[ "$leg3" == OK* ]]; then
    pass "no dangling fallback groups (${leg3#OK })"
  else
    fail "no dangling fallback groups" "$leg3"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────
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
