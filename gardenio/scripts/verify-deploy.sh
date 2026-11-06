#!/usr/bin/env bash
set -euo pipefail

# verify-deploy.sh — Post-deploy verification for gardenio releases.
#
# Checks pod health, image tags, service endpoints, and files bug beads
# for any failures when BEADS_AUTH_TOKEN is set.
#
# Usage:
#   scripts/verify-deploy.sh                              # verify gardenio namespace
#   scripts/verify-deploy.sh --namespace gardenio-e2e      # custom namespace
#   scripts/verify-deploy.sh --version 2026.61.5          # check specific version
#   scripts/verify-deploy.sh --dry-run                    # check only, no bug filing

NAMESPACE="${NAMESPACE:-gardenio}"
VERSION=""
DRY_RUN=false
BEADS_HTTP_URL="${BEADS_HTTP_URL:-}"
BEADS_AUTH_TOKEN="${BEADS_AUTH_TOKEN:-}"

for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --version=*) VERSION="${arg#*=}" ;;
    --dry-run) DRY_RUN=true ;;
    *) echo "Unknown flag: $arg"; exit 1 ;;
  esac
done

FAILURES=()
WARNINGS=()

pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1"; FAILURES+=("$1"); }
warn() { echo "  WARN: $1"; WARNINGS+=("$1"); }

echo "=== Gardenio Post-Deploy Verification ==="
echo "Namespace: ${NAMESPACE}"
[ -n "$VERSION" ] && echo "Expected version: ${VERSION}"
echo ""

# --- 1. Pod status ---
echo "--- Pod Status ---"
PODS=$(kubectl get pods -n "$NAMESPACE" -o json 2>/dev/null)
if [ $? -ne 0 ] || [ -z "$PODS" ]; then
  fail "Cannot list pods in namespace ${NAMESPACE}"
else
  TOTAL=$(echo "$PODS" | jq '.items | length')
  RUNNING=$(echo "$PODS" | jq '[.items[] | select(.status.phase == "Running")] | length')
  NOT_RUNNING=$(echo "$PODS" | jq -r '.items[] | select(.status.phase != "Running") | "\(.metadata.name): \(.status.phase)"')

  if [ "$RUNNING" -eq "$TOTAL" ] && [ "$TOTAL" -gt 0 ]; then
    pass "All ${TOTAL} pods running"
  elif [ "$TOTAL" -eq 0 ]; then
    fail "No pods found in namespace ${NAMESPACE}"
  else
    fail "${RUNNING}/${TOTAL} pods running"
    echo "$NOT_RUNNING" | while read -r line; do
      echo "    - $line"
    done
  fi

  # Check for crashloops
  CRASHLOOPS=$(echo "$PODS" | jq -r '.items[] | .status.containerStatuses[]? | select(.restartCount > 3) | "\(.name): \(.restartCount) restarts"')
  if [ -n "$CRASHLOOPS" ]; then
    fail "Containers with excessive restarts"
    echo "$CRASHLOOPS" | while read -r line; do
      echo "    - $line"
    done
  else
    pass "No crashloops detected"
  fi
fi
echo ""

# --- 2. Image tag verification ---
if [ -n "$VERSION" ]; then
  echo "--- Image Tags ---"
  IMAGES=$(kubectl get pods -n "$NAMESPACE" -o json 2>/dev/null | \
    jq -r '.items[].spec.containers[].image' | sort -u)

  GARDENIO_IMAGES=$(echo "$IMAGES" | grep "/gardenio/" || true)
  if [ -n "$GARDENIO_IMAGES" ]; then
    WRONG_TAG=""
    while IFS= read -r img; do
      TAG=$(echo "$img" | sed 's/.*://')
      if [ "$TAG" = "$VERSION" ] || [ "$TAG" = "latest" ]; then
        pass "Image: ${img}"
      else
        warn "Image ${img} — expected tag ${VERSION} or latest"
        WRONG_TAG="$WRONG_TAG\n    - $img"
      fi
    done <<< "$GARDENIO_IMAGES"
    if [ -n "$WRONG_TAG" ]; then
      warn "Some images have unexpected tags (may be expected for external images)"
    fi
  else
    warn "No gardenio images (/gardenio/ repo path) found"
  fi
  echo ""
fi

# --- 3. Beads daemon health ---
echo "--- Beads Daemon ---"
BEADS_SVC="${NAMESPACE}-beads"
BEADS_EP=$(kubectl get svc -n "$NAMESPACE" "$BEADS_SVC" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
if [ -n "$BEADS_EP" ]; then
  pass "Beads service ${BEADS_SVC} exists (${BEADS_EP})"

  # Port-forward briefly to check health
  kubectl port-forward -n "$NAMESPACE" "svc/${BEADS_SVC}" 19999:8080 &>/dev/null &
  PF_PID=$!
  sleep 2

  HEALTH=$(curl -sf http://localhost:19999/ 2>/dev/null || echo "")
  if [ -n "$HEALTH" ]; then
    pass "Beads daemon responds on HTTP"
  else
    fail "Beads daemon not responding on HTTP"
  fi

  kill "$PF_PID" 2>/dev/null || true
  wait "$PF_PID" 2>/dev/null || true
else
  fail "Beads service ${BEADS_SVC} not found"
fi
echo ""

# --- 4. Slack bridge socket ---
echo "--- Slack Bridge ---"
SLACK_POD=$(kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/component=slack-bridge" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -n "$SLACK_POD" ]; then
  SLACK_READY=$(kubectl get pod -n "$NAMESPACE" "$SLACK_POD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  if [ "$SLACK_READY" = "True" ]; then
    pass "Slack bridge pod ready (${SLACK_POD})"
  else
    fail "Slack bridge pod not ready (${SLACK_POD})"
  fi
else
  warn "No slack-bridge pod found (may be expected in some namespaces)"
fi
echo ""

# --- 5. Controller ---
echo "--- Controller ---"
# The controller Deployment (gardenio-agents) carries component=agents, not
# component=controller — see gardenio.agents.selectorLabels in _helpers.tpl.
# A controller selector here matches zero pods and yields a false "missing"
# (ka-0000002077 / ka-0000002133).
CTRL_POD=$(kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/component=agents" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -n "$CTRL_POD" ]; then
  CTRL_READY=$(kubectl get pod -n "$NAMESPACE" "$CTRL_POD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  if [ "$CTRL_READY" = "True" ]; then
    pass "Controller pod ready (${CTRL_POD})"
  else
    fail "Controller pod not ready (${CTRL_POD})"
  fi
else
  warn "No controller pod found"
fi
echo ""

# --- Summary ---
echo "=== Summary ==="
echo "  Failures: ${#FAILURES[@]}"
echo "  Warnings: ${#WARNINGS[@]}"

if [ ${#FAILURES[@]} -gt 0 ]; then
  echo ""
  echo "Failures:"
  for f in "${FAILURES[@]}"; do
    echo "  - $f"
  done
fi

# --- File bug bead for failures ---
if [ ${#FAILURES[@]} -gt 0 ] && [ -n "$BEADS_AUTH_TOKEN" ] && [ -n "$BEADS_HTTP_URL" ] && ! $DRY_RUN; then
  echo ""
  echo "Filing bug bead for ${#FAILURES[@]} failures..."
  FAILURE_LIST=$(printf '\\n- %s' "${FAILURES[@]}")
  VERSION_LABEL=""
  [ -n "$VERSION" ] && VERSION_LABEL=", \"release:${VERSION}\""

  # "release-pipeline" is the POLICY label; "release:<version>" is provenance.
  # The operator's always-P0 rule (decision ka-0000000928) lives in the
  # gardener-bugfix / gardener-release-owner prompts and matches only the WORD
  # labels (release-pipeline / -pipeline-broken / -regression / -blocker), so a
  # bare "release:<version>" never triggers the escalation (ka-0000003811).
  # priority 1 is the floor — the gardener raises it to P0 on its next cycle.
  BODY=$(cat <<JSONEOF
{
  "title": "Post-deploy verification failures${VERSION:+ in ${VERSION}}",
  "type": "bug",
  "priority": 1,
  "description": "Post-deploy verification failed in namespace ${NAMESPACE}.\\n\\nFailures:${FAILURE_LIST}",
  "labels": ["project:gardenio", "release-pipeline"${VERSION_LABEL}]
}
JSONEOF
)

  curl -sf -X POST "${BEADS_HTTP_URL}/api/v1/beads" \
    -H "Authorization: Bearer ${BEADS_AUTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "$BODY" && echo "Bug bead filed" || echo "Warning: failed to create bug bead"
fi

# Exit with failure code if any checks failed
[ ${#FAILURES[@]} -eq 0 ]
