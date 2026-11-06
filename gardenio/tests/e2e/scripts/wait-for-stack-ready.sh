#!/usr/bin/env bash
# wait-for-stack-ready.sh — gate e2e on the target namespace being stable.
#
# Without this gate the e2e tests can start while the stack is still
# rolling — producing a cascade of "service not found" and port-forward
# breakage mid-run. See ka-0000005539.
#
# Runs once from `lane.sh install` (e2e.yml live-setup), before the shards
# start.
#
# Exits 0 when the stack is Ready (all Helm-managed Deployments Available,
# all StatefulSets caught up, critical services have endpoints).
# Exits 1 if TIMEOUT_S elapses first, with diagnostic context.
set -euo pipefail

NAMESPACE="${NAMESPACE:-gardenio-e2e}"
# Total wait budget for the full stack to settle. This is a SINGLE budget
# shared across three sequential phases below (Deployments-Available →
# StatefulSets → Service-endpoints). On a warm singleton namespace the whole
# thing finishes in under 2m, but on a cold-start POC ephemeral namespace the
# Deployments-Available phase alone can eat most of a 300s budget (image pull +
# DB connect + first health ping), leaving near-zero slack for the endpoints
# phase to ride out a brief readiness flap of the single-replica beads pod —
# which is exactly how e2e-setup wedged on
# "waiting for endpoints on <ns>-beads" (ka-0000005545). 420s gives every phase
# real headroom while staying short enough that a truly wedged deploy still
# fails the gate well inside the setup job timeout (cold-start-absorption
# bumps 6f31c7f, 7c18c6c).
TIMEOUT_S="${STACK_READY_TIMEOUT_S:-420}"
# Services whose endpoints the e2e port-forwards actually depend on.
# Missing or unbacked endpoints here are the exact failure we're preventing.
CRITICAL_SERVICES=(
  "${NAMESPACE}-beads"
  "${NAMESPACE}-postgres"
  "${NAMESPACE}-nats"
)
# Services that are deployed in some overlays (canonical staging) and skipped
# in others (per-instance ephemeral, per gd-3ytvdJYZQG). When absent we warn
# but do not fail — escalation tests that need slack-bridge will surface their
# own failures downstream.
OPTIONAL_SERVICES=(
  "${NAMESPACE}-slack-bridge"
)

dim()  { printf '\033[2m%s\033[0m\n' "$*"; }
red()  { printf '\033[0;31m%s\033[0m\n' "$*"; }
ok()   { printf '\033[0;32m%s\033[0m\n' "$*"; }

echo "Waiting for $NAMESPACE stack to be Ready (timeout ${TIMEOUT_S}s)..."
DEADLINE=$(($(date +%s) + TIMEOUT_S))
remaining() { echo "$((DEADLINE - $(date +%s)))"; }

# ── Deployments ────────────────────────────────────────────────────
# kubectl wait with a Helm-instance selector catches every deployment
# the release manages. If any is not Available, wait blocks until it is
# or the per-invocation timeout fires.
REM=$(remaining)
if [ "$REM" -le 0 ]; then
  red "Timed out before even starting Deployment wait"
  exit 1
fi
if ! kubectl -n "$NAMESPACE" wait --for=condition=Available \
    deployment -l "app.kubernetes.io/instance=${NAMESPACE}" \
    --timeout="${REM}s"; then
  red "One or more Deployments not Available — dumping state:"
  kubectl -n "$NAMESPACE" get deploy -l "app.kubernetes.io/instance=${NAMESPACE}" \
    -o wide 2>&1 | sed 's/^/  /'
  kubectl -n "$NAMESPACE" get pods -o wide 2>&1 | grep -vE "Running|Completed" | sed 's/^/  /' || true
  exit 1
fi
ok "  Deployments Available"

# ── StatefulSets ───────────────────────────────────────────────────
# kubectl wait has no Ready condition for StatefulSets, so use
# `rollout status` per SS. Quiet unless it has to report progress.
for ss in postgres nats vmsingle; do
  FULL="${NAMESPACE}-${ss}"
  if ! kubectl -n "$NAMESPACE" get statefulset "$FULL" >/dev/null 2>&1; then
    dim "  (statefulset $FULL not present — skipping)"
    continue
  fi
  REM=$(remaining)
  if [ "$REM" -le 0 ]; then
    red "Timed out before $FULL rollout check"
    exit 1
  fi
  if ! kubectl -n "$NAMESPACE" rollout status "statefulset/${FULL}" \
      --timeout="${REM}s" >/dev/null; then
    red "StatefulSet $FULL not ready"
    kubectl -n "$NAMESPACE" describe "statefulset/${FULL}" 2>&1 | tail -20 | sed 's/^/  /'
    exit 1
  fi
  ok "  StatefulSet $FULL Ready"
done

# ── Service endpoints ──────────────────────────────────────────────
# A Deployment can be Available while its Service endpoints are still
# empty (readiness probe not yet reconciled into EndpointSlice), and
# that's exactly the race mode that bit us on ka-0000005539. Poll
# until each critical service has at least one backing address.
for svc in "${CRITICAL_SERVICES[@]}"; do
  if ! kubectl -n "$NAMESPACE" get svc "$svc" >/dev/null 2>&1; then
    red "Critical service $svc missing from namespace"
    exit 1
  fi
  while :; do
    REM=$(remaining)
    if [ "$REM" -le 0 ]; then
      red "Timed out waiting for endpoints on $svc"
      kubectl -n "$NAMESPACE" get endpoints "$svc" -o yaml 2>&1 | sed 's/^/  /' || true
      # The endpoints YAML alone only shows the pod sitting under
      # notReadyAddresses — it doesn't say WHY readiness is failing. Dump the
      # backing pods' Ready conditions, restart counts, and recent probe
      # events so the next recurrence is diagnosable from the log without a
      # live cluster (ka-0000005545). Best-effort: never let diagnostics fail
      # the gate beyond the timeout we already hit.
      LABELSEL=$(kubectl -n "$NAMESPACE" get svc "$svc" -o go-template='{{range $k,$v := .spec.selector}}{{$k}}={{$v}},{{end}}' 2>/dev/null | sed 's/,$//' || true)
      if [ -n "$LABELSEL" ]; then
        red "  Backing pods for $svc (selector: $LABELSEL):"
        kubectl -n "$NAMESPACE" get pods -l "$LABELSEL" \
          -o wide 2>&1 | sed 's/^/    /' || true
        kubectl -n "$NAMESPACE" get pods -l "$LABELSEL" \
          -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}Ready={range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\trestarts="}{range .status.containerStatuses[*]}{.restartCount}{" "}{end}{"\n"}{end}' 2>&1 | sed 's/^/    /' || true
        red "  Recent events (last 15):"
        kubectl -n "$NAMESPACE" get events \
          --sort-by=.lastTimestamp 2>&1 | tail -15 | sed 's/^/    /' || true
      fi
      exit 1
    fi
    ADDRS=$(kubectl -n "$NAMESPACE" get endpoints "$svc" \
      -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || echo "")
    if [ -n "$ADDRS" ]; then
      ok "  Service $svc has endpoints"
      break
    fi
    sleep 2
  done
done

# ── Optional service endpoints ─────────────────────────────────────
# Services present in some overlays but skipped in others. Wait for
# endpoints when the service exists; warn-and-skip when it does not.
for svc in "${OPTIONAL_SERVICES[@]}"; do
  if ! kubectl -n "$NAMESPACE" get svc "$svc" >/dev/null 2>&1; then
    dim "  (optional service $svc not present — skipping)"
    continue
  fi
  while :; do
    REM=$(remaining)
    if [ "$REM" -le 0 ]; then
      red "Timed out waiting for endpoints on optional $svc"
      kubectl -n "$NAMESPACE" get endpoints "$svc" -o yaml 2>&1 | sed 's/^/  /' || true
      exit 1
    fi
    ADDRS=$(kubectl -n "$NAMESPACE" get endpoints "$svc" \
      -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || echo "")
    if [ -n "$ADDRS" ]; then
      ok "  Optional service $svc has endpoints"
      break
    fi
    sleep 2
  done
done

ok "Stack Ready. Proceeding with e2e."
