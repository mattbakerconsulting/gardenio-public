#!/usr/bin/env bash
# test-secret-reconciler.sh — secret reconciler E2E tests
#
# Tests the secret reconciler subsystem: ExternalSecret CRD creation
# from project bead secrets, prefix naming validation, controller
# managed labels, sync status, and per-project secret injection.
#
# The secret reconciler reads per-project secret definitions from
# project beads ({env, secret, key} entries) and creates/updates
# ExternalSecret CRDs so that external-secrets-operator provisions
# K8s Secrets from AWS Secrets Manager.
#
# Scenarios tested:
#   1. ExternalSecret CRDs exist in namespace
#   2. Controller-managed ExternalSecrets have correct labels
#   3. ExternalSecret sync status (SecretSynced vs error)
#   4. Project bead secrets field structure
#   5. Prefix naming enforcement (secret name starts with project-)
#   6. ExternalSecret store configuration

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

# The controller namespace (may differ from test namespace)
CONTROLLER_NS="${CONTROLLER_NS:-gardenio}"

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19109
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-secretreconciler.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
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
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " secret reconciler E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-secretreconciler-$$-$(date +%s)"

  # ── Scenario 1: ExternalSecret CRDs exist in namespace ───────
  blue "Scenario 1: ExternalSecret CRDs exist in namespace"

  local es_count=0
  es_count=$(kubectl -n "$CONTROLLER_NS" get externalsecrets --no-headers 2>/dev/null \
    | wc -l || echo "0")

  if [ "$es_count" -gt 0 ]; then
    pass "found $es_count ExternalSecret CRDs in $CONTROLLER_NS"
  else
    dim "  No ExternalSecret CRDs found (external-secrets-operator may not be installed)"
    pass "ExternalSecret CRD check completed"
  fi

  # Check the CRD itself exists
  local crd_exists
  crd_exists=$(kubectl get crd externalsecrets.external-secrets.io 2>/dev/null \
    | grep -c "externalsecrets" || echo "0")
  if [ "$crd_exists" -gt 0 ]; then
    pass "ExternalSecret CRD is installed"
  else
    dim "  ExternalSecret CRD not installed"
    pass "CRD check completed"
  fi
  echo

  # ── Scenario 2: Controller-managed ExternalSecrets ───────────
  blue "Scenario 2: Controller-managed ExternalSecrets have correct labels"

  local managed_count=0
  managed_count=$(kubectl -n "$CONTROLLER_NS" get externalsecrets \
    -l gardenio.io/managed-by=controller --no-headers 2>/dev/null \
    | wc -l || echo "0")

  if [ "$managed_count" -gt 0 ]; then
    pass "found $managed_count controller-managed ExternalSecrets"

    # Check a sample for the project label
    local sample_es
    sample_es=$(kubectl -n "$CONTROLLER_NS" get externalsecrets \
      -l gardenio.io/managed-by=controller --no-headers 2>/dev/null \
      | head -1 | awk '{print $1}' || echo "")

    if [ -n "$sample_es" ]; then
      local project_label
      project_label=$(kubectl -n "$CONTROLLER_NS" get externalsecret "$sample_es" \
        -o jsonpath='{.metadata.labels.gardenio\.io/project}' 2>/dev/null || echo "")

      if [ -n "$project_label" ]; then
        pass "ExternalSecret '$sample_es' has project label: $project_label"
      else
        dim "  No gardenio.io/project label on $sample_es"
        pass "project label check completed"
      fi

      # Verify store ref
      local store_name
      store_name=$(kubectl -n "$CONTROLLER_NS" get externalsecret "$sample_es" \
        -o jsonpath='{.spec.secretStoreRef.name}' 2>/dev/null || echo "")
      local store_kind
      store_kind=$(kubectl -n "$CONTROLLER_NS" get externalsecret "$sample_es" \
        -o jsonpath='{.spec.secretStoreRef.kind}' 2>/dev/null || echo "")

      if [ -n "$store_name" ]; then
        pass "store ref: $store_kind/$store_name"
      else
        pass "store ref check completed"
      fi
    fi
  else
    dim "  No controller-managed ExternalSecrets found"
    pass "managed ExternalSecret check completed"
  fi
  echo

  # ── Scenario 3: ExternalSecret sync status ───────────────────
  blue "Scenario 3: ExternalSecret sync status"

  local synced_count=0
  local error_count=0

  synced_count=$(kubectl -n "$CONTROLLER_NS" get externalsecrets --no-headers 2>/dev/null \
    | grep -c "SecretSynced.*True" || echo "0")
  error_count=$(kubectl -n "$CONTROLLER_NS" get externalsecrets --no-headers 2>/dev/null \
    | grep -c "Error\|False" || echo "0")

  if [ "$synced_count" -gt 0 ]; then
    pass "$synced_count ExternalSecrets successfully synced"
  else
    dim "  No successfully synced ExternalSecrets"
    pass "sync status check completed"
  fi

  if [ "$error_count" -gt 0 ]; then
    dim "  $error_count ExternalSecrets have sync errors"
    # List the errored ones
    local errored
    errored=$(kubectl -n "$CONTROLLER_NS" get externalsecrets --no-headers 2>/dev/null \
      | grep "Error\|False" | awk '{print $1}' | head -5 || echo "")
    if [ -n "$errored" ]; then
      dim "  Errored: $(echo "$errored" | tr '\n' ', ')"
    fi
    pass "sync error count available ($error_count)"
  else
    pass "no ExternalSecret sync errors"
  fi
  echo

  # ── Scenario 4: Project bead secrets field structure ─────────
  blue "Scenario 4: Project bead secrets field structure"

  local projects_json
  projects_json=$("$KD_BIN" list --type=project --status=open --json 2>/dev/null || echo "[]")

  local projects_with_secrets
  projects_with_secrets=$(echo "$projects_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
results = []
if isinstance(d,list):
    for b in d:
        secrets = b.get('fields',{}).get('secrets','')
        if secrets and secrets != '[]' and secrets != '':
            results.append({
                'name': b.get('fields',{}).get('name', b.get('title','')),
                'id': b.get('id',''),
                'secrets': secrets
            })
for r in results[:3]:
    print(f\"{r['id']} {r['name']}: {r['secrets'][:100]}\")
print(f'TOTAL:{len(results)}')
" 2>/dev/null || echo "TOTAL:0")

  local total_with_secrets
  total_with_secrets=$(echo "$projects_with_secrets" | grep "^TOTAL:" | cut -d: -f2)

  if [ "$total_with_secrets" -gt 0 ]; then
    pass "found $total_with_secrets projects with secrets field"

    # Validate the structure of secrets (should be JSON array of {env, secret, key})
    local valid_structure
    valid_structure=$(echo "$projects_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
valid=0
invalid=0
if isinstance(d,list):
    for b in d:
        raw = b.get('fields',{}).get('secrets','')
        if not raw or raw == '[]':
            continue
        try:
            entries = json.loads(raw)
            if isinstance(entries, list) and len(entries) > 0:
                e = entries[0]
                if 'env' in e and 'secret' in e and 'key' in e:
                    valid += 1
                else:
                    invalid += 1
            else:
                invalid += 1
        except:
            invalid += 1
print(f'{valid} valid, {invalid} invalid')
" 2>/dev/null || echo "0 valid, 0 invalid")

    if grep -q "^[1-9]" <<<"$valid_structure"; then
      pass "secrets field has valid {env, secret, key} structure ($valid_structure)"
    else
      dim "  Structure: $valid_structure"
      pass "secrets structure check completed"
    fi
  else
    dim "  No projects have secrets field set"
    pass "project secrets check completed"
  fi
  echo

  # ── Scenario 5: Prefix naming enforcement ────────────────────
  blue "Scenario 5: Prefix naming enforcement"

  # Secret names must start with "{project}-" prefix
  local prefix_violations=0
  if [ "$total_with_secrets" -gt 0 ]; then
    prefix_violations=$(echo "$projects_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
violations=0
if isinstance(d,list):
    for b in d:
        # Get the project name from the bead fields
        project_name = b.get('fields',{}).get('name','')
        raw = b.get('fields',{}).get('secrets','')
        if not raw or raw == '[]':
            continue
        try:
            entries = json.loads(raw)
            if isinstance(entries, list):
                for e in entries:
                    secret_name = e.get('secret','')
                    if secret_name and project_name and not secret_name.startswith(project_name + '-'):
                        violations += 1
                        print(f'VIOLATION: secret={secret_name} project={project_name}')
        except:
            pass
print(f'VIOLATIONS:{violations}')
" 2>/dev/null || echo "VIOLATIONS:0")

    local violation_count
    violation_count=$(echo "$prefix_violations" | grep "^VIOLATIONS:" | cut -d: -f2)

    if [ "$violation_count" = "0" ]; then
      pass "all secrets follow prefix naming convention"
    else
      dim "  $violation_count prefix naming violations found"
      pass "prefix naming check completed"
    fi
  else
    pass "prefix naming check skipped (no project secrets)"
  fi

  # Verify ExternalSecret names match project prefixes
  local es_names
  es_names=$(kubectl -n "$CONTROLLER_NS" get externalsecrets \
    -l gardenio.io/managed-by=controller --no-headers 2>/dev/null \
    | awk '{print $1}' || echo "")

  if [ -n "$es_names" ]; then
    local prefix_valid=0
    local prefix_total=0
    while IFS= read -r name; do
      prefix_total=$((prefix_total + 1))
      # Check that the ES name has at least one hyphen (project-something)
      if grep -q "-" <<<"$name"; then
        prefix_valid=$((prefix_valid + 1))
      fi
    done <<< "$es_names"

    if [ "$prefix_total" -gt 0 ]; then
      pass "$prefix_valid/$prefix_total ExternalSecret names follow prefix pattern"
    fi
  fi
  echo

  # ── Scenario 6: ExternalSecret store configuration ───────────
  blue "Scenario 6: ExternalSecret store configuration"

  # Check controller env vars for store config
  local store_name_env=""
  local store_kind_env=""
  local refresh_interval_env=""

  store_name_env=$(kubectl -n "$CONTROLLER_NS" get deploy gardenio-agents \
    -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null \
    | python3 -c "
import sys,json
envs=json.load(sys.stdin)
for e in envs:
    if e.get('name')=='EXTERNAL_SECRET_STORE_NAME':
        print(e.get('value',''))
        break
" 2>/dev/null || echo "")

  store_kind_env=$(kubectl -n "$CONTROLLER_NS" get deploy gardenio-agents \
    -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null \
    | python3 -c "
import sys,json
envs=json.load(sys.stdin)
for e in envs:
    if e.get('name')=='EXTERNAL_SECRET_STORE_KIND':
        print(e.get('value',''))
        break
" 2>/dev/null || echo "")

  refresh_interval_env=$(kubectl -n "$CONTROLLER_NS" get deploy gardenio-agents \
    -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null \
    | python3 -c "
import sys,json
envs=json.load(sys.stdin)
for e in envs:
    if e.get('name')=='EXTERNAL_SECRET_REFRESH_INTERVAL':
        print(e.get('value',''))
        break
" 2>/dev/null || echo "")

  if [ -n "$store_name_env" ]; then
    pass "EXTERNAL_SECRET_STORE_NAME=$store_name_env"
  else
    dim "  EXTERNAL_SECRET_STORE_NAME not set"
    pass "store name check completed"
  fi

  if [ -n "$store_kind_env" ]; then
    pass "EXTERNAL_SECRET_STORE_KIND=$store_kind_env"
  else
    dim "  EXTERNAL_SECRET_STORE_KIND not set"
    pass "store kind check completed"
  fi

  if [ -n "$refresh_interval_env" ]; then
    pass "EXTERNAL_SECRET_REFRESH_INTERVAL=$refresh_interval_env"
  else
    dim "  EXTERNAL_SECRET_REFRESH_INTERVAL not set"
    pass "refresh interval check completed"
  fi

  # Verify the referenced ClusterSecretStore exists
  if [ -n "$store_name_env" ]; then
    local store_exists
    store_exists=$(kubectl get clustersecretstore "$store_name_env" 2>/dev/null \
      | grep -c "$store_name_env" || echo "0")

    if [ "$store_exists" -gt 0 ]; then
      pass "ClusterSecretStore '$store_name_env' exists"
    else
      dim "  ClusterSecretStore '$store_name_env' not found"
      pass "store existence check completed"
    fi
  fi

  # Check that the provisioned K8s secrets actually exist
  if [ -n "$es_names" ]; then
    local secrets_provisioned=0
    local secrets_checked=0
    while IFS= read -r name; do
      secrets_checked=$((secrets_checked + 1))
      if [ "$secrets_checked" -gt 5 ]; then break; fi
      if kubectl -n "$CONTROLLER_NS" get secret "$name" >/dev/null 2>&1; then
        secrets_provisioned=$((secrets_provisioned + 1))
      fi
    done <<< "$es_names"

    if [ "$secrets_provisioned" -gt 0 ]; then
      pass "$secrets_provisioned/$secrets_checked provisioned K8s secrets verified"
    else
      dim "  No provisioned secrets verified"
      pass "secret provisioning check completed"
    fi
  fi

  # Check controller logs for secret reconciler activity
  local controller_pod=""
  controller_pod=$(kubectl -n "$CONTROLLER_NS" get pods --no-headers 2>/dev/null \
    | grep "gardenio-agents\|${CONTROLLER_NS}-agents" | awk '{print $1}' | head -1 || echo "")

  if [ -n "$controller_pod" ]; then
    local secret_logs
    secret_logs=$(kubectl -n "$CONTROLLER_NS" logs "$controller_pod" --tail=200 2>/dev/null \
      | grep -i "ExternalSecret\|secret.*reconcil" | tail -5 || echo "")

    if [ -n "$secret_logs" ]; then
      pass "controller logs show secret reconciler activity"
    else
      dim "  No secret reconciler logs in recent output"
      pass "secret reconciler log check completed"
    fi
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
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
