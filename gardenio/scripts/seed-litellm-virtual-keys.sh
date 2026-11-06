#!/usr/bin/env bash
set -euo pipefail

# seed-litellm-virtual-keys.sh — Seed per-account-bead LiteLLM virtual keys
# for the AWS POC (epic ka-0000004609, tier h4 task ka-0000002922).
#
# For each open account bead in the local kbeads daemon, this script:
#   1. POSTs /key/generate on the in-cluster LiteLLM proxy via kubectl exec
#      (no port-forward needed — runs inside the gardenio-litellm pod). The
#      request is driven by python3, which the official litellm image ships
#      (curl is NOT present), with secrets passed over stdin rather than argv.
#   2. PATCHes the account bead with base_url + litellm_key via `kd update`.
#
# Idempotent: accounts that already have a non-empty litellm_key are skipped
# unless --force is passed. Re-running after partial completion is safe.
#
# Operator-driven: never invoked autonomously by agents. Smoke-testing of the
# resulting wiring is owned by the operator per epic ka-0000004609 hard rules.
#
# Usage:
#   gardenio/scripts/seed-litellm-virtual-keys.sh                # seed all open accounts
#   gardenio/scripts/seed-litellm-virtual-keys.sh --account foo  # seed one
#   gardenio/scripts/seed-litellm-virtual-keys.sh --dry-run      # show plan, no writes
#   gardenio/scripts/seed-litellm-virtual-keys.sh --force        # overwrite existing keys
#
# Environment:
#   NAMESPACE       gardenio namespace (default: gardenio)
#   LITELLM_SVC     in-cluster service name (default: gardenio-litellm)
#   LITELLM_PORT    proxy port (default: 4000)
#   BASE_URL        value to write to each account bead's base_url
#                   (default: http://${LITELLM_SVC}:${LITELLM_PORT})
#   MASTER_KEY_SECRET name of the K8s secret holding the LiteLLM master key
#                   (default: gardenio-litellm-master-key)
#   MASTER_KEY_KEY  key within the secret (default: master-key; fallback: token)

NAMESPACE="${NAMESPACE:-gardenio}"
LITELLM_SVC="${LITELLM_SVC:-gardenio-litellm}"
LITELLM_PORT="${LITELLM_PORT:-4000}"
BASE_URL="${BASE_URL:-http://${LITELLM_SVC}:${LITELLM_PORT}}"
MASTER_KEY_SECRET="${MASTER_KEY_SECRET:-gardenio-litellm-master-key}"
MASTER_KEY_KEY="${MASTER_KEY_KEY:-master-key}"

ACCOUNT_FILTER=""
DRY_RUN=false
FORCE=false

for arg in "$@"; do
  case "$arg" in
    --account=*) ACCOUNT_FILTER="${arg#*=}" ;;
    --account)
      shift
      ACCOUNT_FILTER="${1:-}"
      ;;
    --dry-run) DRY_RUN=true ;;
    --force) FORCE=true ;;
    -h|--help)
      sed -n '5,30p' "$0"
      exit 0
      ;;
    *) ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need kubectl
need kd
need jq

echo "=== Seed LiteLLM virtual keys for account beads ==="
echo "Namespace:    ${NAMESPACE}"
echo "LiteLLM URL:  ${BASE_URL}"
echo "Dry-run:      ${DRY_RUN}"
echo "Force:        ${FORCE}"
[ -n "${ACCOUNT_FILTER}" ] && echo "Filter:       account=${ACCOUNT_FILTER}"
echo ""

# Resolve master key from the in-cluster Secret. Try the requested key first,
# then fall back to the legacy "token" key shape.
fetch_master_key() {
  local k
  for k in "${MASTER_KEY_KEY}" token master-key; do
    if kubectl -n "${NAMESPACE}" get secret "${MASTER_KEY_SECRET}" \
        -o jsonpath="{.data.${k}}" 2>/dev/null | grep -q .; then
      kubectl -n "${NAMESPACE}" get secret "${MASTER_KEY_SECRET}" \
        -o jsonpath="{.data.${k}}" | base64 -d
      return 0
    fi
  done
  return 1
}

MASTER_KEY="$(fetch_master_key)" || {
  echo "ERROR: cannot read master key from secret ${MASTER_KEY_SECRET} in ${NAMESPACE}" >&2
  exit 1
}

# Resolve a pod to exec into (avoids needing port-forward from the
# operator's workstation). Picks the first Ready litellm pod.
LITELLM_POD="$(kubectl -n "${NAMESPACE}" get pods \
  -l app.kubernetes.io/component=litellm \
  -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' \
  | awk '{print $1}')"
if [ -z "${LITELLM_POD}" ]; then
  echo "ERROR: no Running litellm pod found in ${NAMESPACE} (label app.kubernetes.io/component=litellm)" >&2
  exit 1
fi
echo "Using litellm pod: ${LITELLM_POD}"
echo ""

# POST /key/generate inside the litellm pod. The official litellm image
# (ghcr.io/berriai/litellm:v1.82.0-stable) ships python3 but NOT curl, so we
# drive the request with python3's urllib by default and only fall back to curl
# where it happens to exist (older/custom images). Both speak to 127.0.0.1
# inside the pod, so no port-forward from the operator's workstation is needed.
generate_key() {
  local alias="$1"
  local body
  body=$(printf '{"key_alias":"%s","models":["claude-opus-5-5-oauth","claude-opus-5-oauth","claude-opus-4-8-oauth","claude-opus-4-7-oauth","claude-opus-4-6-oauth","claude-sonnet-5-5-oauth","claude-sonnet-5-oauth","claude-sonnet-4-6-oauth","claude-fable-5-1-oauth","claude-fable-5-oauth","anthropic/claude-haiku-4-5"],"max_budget":null}' "${alias}")
  # Feed the port, master key, and request body to the pod over STDIN as three
  # newline-delimited lines — never argv (argv is world-readable via /proc on
  # the pod). The in-pod python3 helper reads them, POSTs to 127.0.0.1, prints
  # the raw JSON response on stdout, and exits non-zero on any HTTP/transport
  # error so the caller's `if ! resp=...` branch fires. The master key and
  # body are single-line JSON, so newline-delimited framing is unambiguous.
  # The python source is intentionally single-quoted so it runs inside the pod
  # verbatim — no host-side expansion is wanted (and shellcheck agrees: it reads
  # no host variables, so there is nothing for SC2016 to flag).
  printf '%s\n%s\n%s\n' "${LITELLM_PORT}" "${MASTER_KEY}" "${body}" \
    | kubectl -n "${NAMESPACE}" exec -i "${LITELLM_POD}" -c litellm -- \
        python3 -c '
import sys, urllib.request, urllib.error
port = sys.stdin.readline().strip()
mk = sys.stdin.readline().strip()
body = sys.stdin.readline().rstrip("\n").encode()
req = urllib.request.Request(
    "http://127.0.0.1:" + port + "/key/generate",
    data=body, method="POST",
    headers={"Authorization": "Bearer " + mk, "Content-Type": "application/json"},
)
try:
    sys.stdout.write(urllib.request.urlopen(req, timeout=30).read().decode())
except urllib.error.HTTPError as e:
    sys.stderr.write("HTTP %d: %s\n" % (e.code, e.read().decode("utf-8", "replace")))
    sys.exit(1)
'
}

# Walk account beads.
mapfile -t ACCOUNTS < <(kd list --type=account --status=open --json \
  | jq -r '.[] | "\(.id)\t\(.title // "")\t\((.fields // {}) | (.litellm_key // ""))"')

if [ "${#ACCOUNTS[@]}" -eq 0 ]; then
  echo "No open account beads found."
  exit 0
fi

SEEDED=0
SKIPPED=0
FAILED=0

for row in "${ACCOUNTS[@]}"; do
  id="$(printf '%s' "${row}" | awk -F '\t' '{print $1}')"
  title="$(printf '%s' "${row}" | awk -F '\t' '{print $2}')"
  existing="$(printf '%s' "${row}" | awk -F '\t' '{print $3}')"

  if [ -n "${ACCOUNT_FILTER}" ] && [ "${title}" != "${ACCOUNT_FILTER}" ] && [ "${id}" != "${ACCOUNT_FILTER}" ]; then
    continue
  fi

  if [ -n "${existing}" ] && [ "${FORCE}" != "true" ]; then
    echo "skip   ${id} (${title}) — litellm_key already set; pass --force to overwrite"
    SKIPPED=$((SKIPPED + 1))
    continue
  fi

  alias="account-${title:-${id}}"
  if [ "${DRY_RUN}" = "true" ]; then
    echo "dryrun ${id} (${title}) — would POST /key/generate alias=${alias} and set base_url=${BASE_URL}"
    continue
  fi

  echo "seed   ${id} (${title}) — POST /key/generate alias=${alias}"
  if ! resp="$(generate_key "${alias}")"; then
    echo "       FAILED to call /key/generate" >&2
    FAILED=$((FAILED + 1))
    continue
  fi
  vkey="$(printf '%s' "${resp}" | jq -r '.key // empty')"
  if [ -z "${vkey}" ]; then
    echo "       FAILED — no .key in response: ${resp}" >&2
    FAILED=$((FAILED + 1))
    continue
  fi

  if ! kd update "${id}" -f "base_url=${BASE_URL}" -f "litellm_key=${vkey}" >/dev/null; then
    echo "       FAILED to update bead ${id}" >&2
    FAILED=$((FAILED + 1))
    continue
  fi
  echo "       OK — wrote base_url + litellm_key"
  SEEDED=$((SEEDED + 1))
done

echo ""
echo "=== Summary ==="
echo "seeded:  ${SEEDED}"
echo "skipped: ${SKIPPED}"
echo "failed:  ${FAILED}"

[ "${FAILED}" -eq 0 ]
