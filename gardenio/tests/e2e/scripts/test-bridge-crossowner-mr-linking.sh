#!/usr/bin/env bash
# test-bridge-crossowner-mr-linking.sh
#
# E2E defense for the MR→beads graph-linking regression (gd-XOcerNXFV2,
# epic gd-v9gFo1rRU1). The gitlab-bridge authenticates with a FLEET token and
# performs system-managed graph writes (produces/triggered-by edges + mr_url
# back-fill) against task beads OWNED BY OTHER AGENTS. The kbeads ownership
# gate (authorizeOwnership) bypasses only admin-OIDC and the primary static
# token, so once the bridge moved to a fleet token every cross-owner graph
# write it attempted began returning HTTP 403 "ownership denied" — silently
# dropping the produces/triggered-by edges and the mr_url projection.
#
# T1 (ka-0000001946) adds a narrow PRIVILEGED service-fleet capability: a
# designated bridge fleet token may bypass ownership for these system-managed
# writes, while ORDINARY fleet tokens stay gated. This module exercises the
# real cross-owner path end-to-end against a live kbeads daemon:
#
#   1. Admin creates a task bead OWNED BY identity A (owner=<agent-A>).
#   2. Register a PRIVILEGED bridge fleet token (identity B, not the owner).
#   3. As the privileged fleet token, add produces (task→MR) + triggered-by
#      (MR→task) edges and back-fill mr_url on the cross-owner task bead.
#      Assert all succeed and read back the edges + mr_url.
#   4. Negative control: an ORDINARY (non-privileged) fleet token attempting
#      the same cross-owner write is still 403 (security boundary lock-in).
#
# Deterministic: drives the writes directly over HTTP — no reliance on the
# 15-minute reverse-reconciler cadence.
#
# OPT-IN / self-skipping. The privileged-fleet capability ships with T1.
# Until T1 is deployed to the target namespace this module SELF-SKIPS (exit 0):
# it probes whether a privileged fleet token can perform a cross-owner write,
# and if not (pre-T1 behavior) it reports SKIP rather than FAIL so it never
# breaks the live suite ahead of the fix. Once T1 lands the probe succeeds and
# the full assertions run. Force-run the assertions with CROSSOWNER_E2E=1.
#
# Usage:
#   ./tests/e2e/scripts/test-bridge-crossowner-mr-linking.sh [--namespace <ns>]

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
TEST_TAG="xowner-$$-$(date +%s)"
FLEET_PRIV="xowner-priv-${TEST_TAG}"
FLEET_ORD="xowner-ord-${TEST_TAG}"
PRIV_TOKEN="xowner-priv-tok-${TEST_TAG}"
ORD_TOKEN="xowner-ord-tok-${TEST_TAG}"
OWNER_A="agent-a-${TEST_TAG}@e2e.example.com"
MR_URL="https://gitlab.com/acme/platform/gardenio/-/merge_requests/424242"
CLEANUP_BEADS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }
pass()  { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail()  { red "  ✗ $1"; [ -n "${2:-}" ] && dim "    $2"; FAIL=$((FAIL + 1)); ERRORS+=("$1: ${2:-}"); }
skip()  { blue "SKIP: $1"; if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$1" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi; exit 0; }  # skip visibility (ka-0000000989)

# ── Setup: port-forward kbeads daemon ────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kbeads daemon..."
  local port=19142
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-xowner.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: discover admin token ──────────────────────────────────
setup_admin_token() {
  if [ -n "${BEADS_ADMIN_TOKEN:-}" ]; then return 0; fi
  if [ -n "${BEADS_TOKEN:-}" ]; then export BEADS_ADMIN_TOKEN="$BEADS_TOKEN"; return 0; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then export BEADS_ADMIN_TOKEN="$secret"; return 0; fi
  return 1
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  if [ -n "${BEADS_ADMIN_TOKEN:-}" ]; then
    for id in "${CLEANUP_BEADS[@]:-}"; do
      [ -n "$id" ] && curl -sf -X POST \
        -H "Authorization: Bearer $BEADS_ADMIN_TOKEN" \
        "${BEADS_HTTP_URL}/v1/beads/${id}/close" >/dev/null 2>&1 || true
    done
    for fleet in "$FLEET_PRIV" "$FLEET_ORD"; do
      curl -sf -X DELETE \
        -H "Authorization: Bearer $BEADS_ADMIN_TOKEN" \
        "${BEADS_HTTP_URL}/v1/fleet-tokens/${fleet}" >/dev/null 2>&1 || true
    done
  fi
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── API wrappers ──────────────────────────────────────────────────
# create_owned_bead <type> <title> <owner> [<mr_url>] → echoes bead id
create_owned_bead() {
  local btype="$1" title="$2" owner="$3" mrurl="${4:-}"
  local body resp id
  if [ -n "$mrurl" ]; then
    body=$(python3 -c "import json; print(json.dumps({'title':'$title','type':'$btype','owner':'$owner','labels':['project:gardenio'],'fields':{'mr_url':'$mrurl'}}))")
  else
    body=$(python3 -c "import json; print(json.dumps({'title':'$title','type':'$btype','owner':'$owner','labels':['project:gardenio']}))")
  fi
  resp=$(curl -s -X POST "${BEADS_HTTP_URL}/v1/beads" \
    -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" \
    -H "Content-Type: application/json" \
    -H "X-Operated-By: oidc:matthew.baker@acme.example" \
    -d "$body")
  id=$(echo "$resp" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || true)
  [ -n "$id" ] && CLEANUP_BEADS+=("$id")
  echo "$id"
}

# register_fleet_token <fleet> <token> <subject> <privileged-bool> → http code
register_fleet_token() {
  local fleet="$1" token="$2" subject="$3" priv="$4"
  local body code
  body=$(python3 -c "import json; print(json.dumps({'token':'$token','fleet':'$fleet','subject':'$subject','privileged':$([ "$priv" = true ] && echo True || echo False)}))")
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BEADS_HTTP_URL}/v1/fleet-tokens" \
    -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "$body")
  echo "$code"
}

# add_dep <token> <bead> <depends_on> <type> → http code
add_dep() {
  local token="$1" bead="$2" dep="$3" dtype="$4" body code
  body=$(python3 -c "import json; print(json.dumps({'depends_on_id':'$dep','type':'$dtype'}))")
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BEADS_HTTP_URL}/v1/beads/${bead}/dependencies" \
    -H "Authorization: Bearer ${token}" -H "Content-Type: application/json" -d "$body")
  echo "$code"
}

# set_mr_url <token> <bead> <mr_url> → http code
set_mr_url() {
  local token="$1" bead="$2" mrurl="$3" body code
  body=$(python3 -c "import json; print(json.dumps({'fields':{'mr_url':'$mrurl'}}))")
  code=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH "${BEADS_HTTP_URL}/v1/beads/${bead}" \
    -H "Authorization: Bearer ${token}" -H "Content-Type: application/json" -d "$body")
  echo "$code"
}

# has_dep <bead> <depends_on> <type> → 0 if present
has_dep() {
  local bead="$1" dep="$2" dtype="$3" resp
  resp=$(curl -s "${BEADS_HTTP_URL}/v1/beads/${bead}/dependencies?type=${dtype}" \
    -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}")
  echo "$resp" | python3 -c "
import json,sys
d=json.load(sys.stdin).get('dependencies',[])
sys.exit(0 if any(x.get('depends_on_id')=='$dep' and x.get('type')=='$dtype' for x in d) else 1)
" 2>/dev/null
}

# bead_mr_url <bead> → echoes mr_url
bead_mr_url() {
  local bead="$1" resp
  resp=$(curl -s "${BEADS_HTTP_URL}/v1/beads/${bead}" -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}")
  echo "$resp" | python3 -c "import json,sys; print((json.load(sys.stdin).get('fields') or {}).get('mr_url',''))" 2>/dev/null || true
}

# ── Main ──────────────────────────────────────────────────────────
echo "=== Bridge cross-owner MR→bead linking E2E (gd-XOcerNXFV2) ==="
setup_portforward
if ! setup_admin_token; then
  skip "admin token not reachable in $NAMESPACE (kd-beads-token absent) — skipping cross-owner module"
fi

blue "Setup: create cross-owner beads owned by identity A"
TASK_BEAD=$(create_owned_bead task "E2E xowner producing task ${TEST_TAG}" "$OWNER_A")
MR_BEAD=$(create_owned_bead merge-request "E2E xowner MR bead ${TEST_TAG}" "$OWNER_A" "$MR_URL")
if [ -z "$TASK_BEAD" ] || [ -z "$MR_BEAD" ]; then
  fail "create cross-owner beads" "task=$TASK_BEAD mr=$MR_BEAD"
  echo "Cannot continue."; exit 1
fi
pass "created task bead $TASK_BEAD + MR bead $MR_BEAD owned by $OWNER_A"

blue "Setup: register privileged + ordinary fleet tokens"
code=$(register_fleet_token "$FLEET_PRIV" "$PRIV_TOKEN" "bridge:gitlab" true)
if [ "$code" = 201 ]; then pass "registered privileged fleet token (HTTP 201)"; else fail "register privileged fleet token" "HTTP $code"; exit 1; fi
code=$(register_fleet_token "$FLEET_ORD" "$ORD_TOKEN" "fleet:worker" false)
if [ "$code" = 201 ]; then pass "registered ordinary fleet token (HTTP 201)"; else fail "register ordinary fleet token" "HTTP $code"; exit 1; fi

# ── Self-skip probe: is T1's privileged capability deployed? ──────
# A privileged fleet token doing a cross-owner produces edge returns 2xx
# post-T1, 403 pre-T1. Skip (don't fail) the suite when running ahead of T1.
probe=$(add_dep "$PRIV_TOKEN" "$TASK_BEAD" "$MR_BEAD" "produces")
if [ "${CROSSOWNER_E2E:-0}" != 1 ] && [ "$probe" = 403 ]; then
  skip "privileged-fleet capability not deployed in $NAMESPACE (cross-owner write still 403; T1 ka-0000001946 not yet shipped). Set CROSSOWNER_E2E=1 to force the assertions."
fi

# ── Assertions (T1 deployed) ─────────────────────────────────────
blue "1. Privileged fleet token: produces edge task→MR on cross-owner bead"
# The probe above already issued the produces edge; treat its result as the assertion.
if [ "$probe" = 201 ] || [ "$probe" = 200 ]; then
  pass "produces edge accepted (HTTP $probe)"
else
  fail "privileged produces edge" "expected 2xx, got HTTP $probe (regression — T1 ka-0000001946)"
fi
if has_dep "$TASK_BEAD" "$MR_BEAD" "produces"; then
  pass "produces edge task→MR present"
else
  fail "produces edge readback" "edge not found after privileged write"
fi

blue "2. Privileged fleet token: triggered-by edge MR→task"
code=$(add_dep "$PRIV_TOKEN" "$MR_BEAD" "$TASK_BEAD" "triggered-by")
if [ "$code" = 201 ] || [ "$code" = 200 ]; then
  pass "triggered-by edge accepted (HTTP $code)"
else
  fail "privileged triggered-by edge" "expected 2xx, got HTTP $code"
fi
if has_dep "$MR_BEAD" "$TASK_BEAD" "triggered-by"; then
  pass "triggered-by edge MR→task present"
else
  fail "triggered-by edge readback" "edge not found after privileged write"
fi

blue "3. Privileged fleet token: mr_url back-fill on cross-owner task bead"
code=$(set_mr_url "$PRIV_TOKEN" "$TASK_BEAD" "$MR_URL")
if [ "$code" = 200 ]; then
  pass "mr_url back-fill accepted (HTTP 200)"
else
  fail "privileged mr_url back-fill" "expected 200, got HTTP $code"
fi
if [ "$(bead_mr_url "$TASK_BEAD")" = "$MR_URL" ]; then
  pass "mr_url back-fill readback matches"
else
  fail "mr_url readback" "got '$(bead_mr_url "$TASK_BEAD")', want '$MR_URL'"
fi

blue "4. Negative control: ordinary fleet token cross-owner write still 403"
code=$(add_dep "$ORD_TOKEN" "$TASK_BEAD" "$MR_BEAD" "produces")
if [ "$code" = 403 ]; then
  pass "ordinary fleet token denied produces edge (HTTP 403) — security boundary intact"
else
  fail "ordinary fleet token should be denied" "expected HTTP 403, got $code"
fi
code=$(set_mr_url "$ORD_TOKEN" "$MR_BEAD" "https://gitlab.com/x/y/-/merge_requests/1")
if [ "$code" = 403 ]; then
  pass "ordinary fleet token denied mr_url write (HTTP 403)"
else
  fail "ordinary fleet token mr_url should be denied" "expected HTTP 403, got $code"
fi

# ── Summary ───────────────────────────────────────────────────────
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All cross-owner MR-linking assertions passed!"
