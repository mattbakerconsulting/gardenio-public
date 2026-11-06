#!/usr/bin/env bash
# smoke-byo.sh — minimal, ACME-free health check for a BYO-cluster gardenio.
#
# The full E2E suite (gardenio/tests/e2e) assumes the ACME keycloak/forwardauth/
# litellm/slack stack is live and is gated OUT for the public BYO path. This
# script is the BYO replacement: it verifies the core stack an external user
# actually deployed (deploy-values/gardenio-byo.yaml) is healthy, with NO AWS,
# NO Keycloak, NO Slack, NO ingress, NO ACME DNS.
#
# Checks:
#   1. The beads Deployment and postgres/nats StatefulSets are Ready.
#   2. The beads daemon answers GET /v1/health over a port-forward.
#   3. A bead CRUD round-trip succeeds (create -> read -> close) using the
#      bearer token from the kd-beads-token Secret.
#
# Exit 0 = healthy. Exit 1 = something is wrong (with diagnostics).
#
# Environment:
#   RELEASE        helm release name      (default: gardenio)
#   NAMESPACE      target namespace       (default: gardenio)
#   KUBE_CONTEXT   kubeconfig context     (default: current-context)
#   SMOKE_TIMEOUT  rollout wait seconds   (default: 300)
#   SKIP_CRUD      1 = health-only, skip the CRUD round-trip (default: 0)
set -euo pipefail

RELEASE="${RELEASE:-gardenio}"
NAMESPACE="${NAMESPACE:-gardenio}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
SMOKE_TIMEOUT="${SMOKE_TIMEOUT:-300}"
SKIP_CRUD="${SKIP_CRUD:-0}"

KCTL=(kubectl -n "$NAMESPACE")
[ -n "$KUBE_CONTEXT" ] && KCTL+=(--context "$KUBE_CONTEXT")

red()  { printf '\033[0;31m%s\033[0m\n' "$*"; }
ok()   { printf '\033[0;32m%s\033[0m\n' "$*"; }
info() { printf '\033[2m%s\033[0m\n' "$*"; }

fail() {
  red "SMOKE FAILED: $*"
  echo "--- Pods ---";   "${KCTL[@]}" get pods -o wide 2>/dev/null || true
  echo "--- Events ---"; "${KCTL[@]}" get events --sort-by=.lastTimestamp 2>/dev/null | tail -30 || true
  exit 1
}

# ── 1. Workload readiness ───────────────────────────────────────────────────
echo "=== [1/3] Waiting for core workloads (timeout ${SMOKE_TIMEOUT}s) ==="
"${KCTL[@]}" rollout status "deploy/${RELEASE}-beads" --timeout="${SMOKE_TIMEOUT}s" \
  || fail "beads deployment did not become Ready"
for sts in postgres nats; do
  if "${KCTL[@]}" get "statefulset/${RELEASE}-${sts}" >/dev/null 2>&1; then
    "${KCTL[@]}" rollout status "statefulset/${RELEASE}-${sts}" --timeout="${SMOKE_TIMEOUT}s" \
      || fail "${sts} statefulset did not become Ready"
  else
    info "  statefulset/${RELEASE}-${sts} not present (disabled in values) — skipping"
  fi
done
ok "Core workloads Ready"

# ── 2. Health endpoint over a port-forward ──────────────────────────────────
echo "=== [2/3] Probing beads /v1/health ==="
LPORT=18080
"${KCTL[@]}" port-forward "svc/${RELEASE}-beads" "${LPORT}:8080" >/tmp/byo-pf.log 2>&1 &
PF_PID=$!
cleanup() { kill "$PF_PID" >/dev/null 2>&1 || true; }
trap cleanup EXIT

BASE="http://127.0.0.1:${LPORT}"
HEALTHY=0
for _ in $(seq 1 30); do
  if curl -sf --max-time 3 -o /dev/null "${BASE}/v1/health" 2>/dev/null; then
    HEALTHY=1; break
  fi
  sleep 2
done
[ "$HEALTHY" = "1" ] || { cat /tmp/byo-pf.log 2>/dev/null || true; fail "beads /v1/health never returned 200"; }
ok "beads /v1/health OK"

if [ "$SKIP_CRUD" = "1" ]; then
  ok "SMOKE PASSED (health-only; SKIP_CRUD=1)"
  exit 0
fi

# ── 3. Bead CRUD round-trip ─────────────────────────────────────────────────
echo "=== [3/3] Bead CRUD round-trip ==="
TOKEN="$("${KCTL[@]}" get secret kd-beads-token -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[ -n "$TOKEN" ] || fail "could not read kd-beads-token secret (key 'token')"
AUTH=(-H "Authorization: Bearer ${TOKEN}")

CREATE_BODY='{"title":"byo-smoke probe","type":"task","priority":2}'
CREATE_RESP="$(curl -sf --max-time 10 "${AUTH[@]}" -H 'Content-Type: application/json' \
  -X POST "${BASE}/v1/beads" -d "$CREATE_BODY" 2>/dev/null)" \
  || fail "create bead (POST /v1/beads) failed — check daemon auth/token"
# Parse the TOP-LEVEL .id. The create response is the bead object; a regex over
# the whole body could grab a nested id (label/dep/comment) whose field happens
# to serialize first. `|| true` keeps the no-match case from tripping `set -e`
# so the friendly diagnostic below runs.
if command -v jq >/dev/null 2>&1; then
  BEAD_ID="$(printf '%s' "$CREATE_RESP" | jq -r '.id // empty' 2>/dev/null || true)"
else
  # jq absent: fall back to the first top-level "id" (objects render id early).
  BEAD_ID="$(printf '%s' "$CREATE_RESP" | grep -oE '"id"[ ]*:[ ]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/' || true)"
fi
[ -n "$BEAD_ID" ] || fail "create returned no bead id; body=$CREATE_RESP"
info "  created $BEAD_ID"

curl -sf --max-time 10 "${AUTH[@]}" -o /dev/null "${BASE}/v1/beads/${BEAD_ID}" \
  || fail "read bead (GET /v1/beads/$BEAD_ID) failed"
info "  read $BEAD_ID"

curl -sf --max-time 10 "${AUTH[@]}" -H 'Content-Type: application/json' \
  -X PATCH "${BASE}/v1/beads/${BEAD_ID}" -d '{"status":"closed"}' -o /dev/null \
  || fail "close bead (PATCH /v1/beads/$BEAD_ID) failed"
info "  closed $BEAD_ID"

ok "SMOKE PASSED — core gardenio stack is healthy on this BYO cluster"
