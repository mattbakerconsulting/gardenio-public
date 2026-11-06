#!/usr/bin/env bash
set -euo pipefail

# litellm-stage3-migrate.sh — Stage-3 of the in-cluster LiteLLM migration
# (epic ka-0000000560, task ka-0000002573). Follow-on to Stage-2
# (ka-0000000158, done — 8 OAuth accounts flipped in-cluster + verified). Two
# items were deliberately left external in Stage-2 because the in-cluster proxy
# served no bedrock model group:
#
#   1. EMBEDDINGS: repoint kbeads BEADS_EMBED_URL (model bedrock-titan-embed-v2)
#      from the EXTERNAL proxy (https://litellm.app.devops.acme.example) to the
#      in-cluster proxy. The actual flip is a HELM VALUES change (set
#      litellm.embedding.enabled=true + beads.embedding.url=http://gardenio-litellm:4000
#      in deploy-values/gardenio.yaml), deployed via the release pipeline — NOT a
#      bead write. This script's role for item 1 is VERIFICATION: prove the
#      in-cluster proxy serves the Titan embed model and that /embeddings +
#      `kd embed sync` succeed against it, both BEFORE the prod values flip
#      (against a non-prod namespace) and AFTER (against prod gardenio-litellm).
#
#   2. litellm-bedrock account (ka-0000003488, model bedrock-claude-opus-4-7):
#      same SENSITIVE per-account base_url + litellm_key cutover as Stage-2, but
#      for the bedrock chat model group. Requires litellm.bedrock.enabled=true
#      in the in-cluster proxy config (chart support added in this MR) so the
#      account's model + probe_model resolve before its base_url can flip.
#
# HARD GATE (epic ka-0000000560): NEVER `flip` the live prod account (or deploy
# the prod embeddings values flip) without a green non-prod bake, a TESTED
# rollback, AND a fresh operator decision-gate. As in Stage-2 this script
# SEPARATES non-destructive prep (snapshot/seed/verify/embed-verify) from the
# gated destructive `flip`.
#
# The controller treats base_url-set + empty-litellm_key as UNUSABLE
# (accountInfoUnusable, main.go:1754 — every agent on the account 401s). So BOTH
# `flip` and `rollback` MUST write base_url AND litellm_key together, atomically.
#
# Subcommands:
#   snapshot      Capture current base_url + litellm_key for the bedrock account
#                 (and, by default, every open account as a safety net) into a
#                 rollback ledger JSON. READ-ONLY. Run FIRST.
#   seed          Mint a virtual key for the bedrock account in the IN-CLUSTER
#                 litellm DB scoped to the bedrock chat models, persist plaintext
#                 into a K8s Secret (default gardenio-litellm-stage3-vkeys).
#                 NON-DESTRUCTIVE; does NOT touch the account bead. Idempotent
#                 (--force to re-mint). Round-trips each key via /key/info.
#   verify        Re-check the seeded bedrock key resolves via /key/info AND that
#                 the proxy serves the account's model + probe_model. READ-ONLY.
#   embed-verify  Item 1 gate: POST /embeddings {model: bedrock-titan-embed-v2}
#                 against the (in-cluster) proxy and assert HTTP 200 + 1024-dim
#                 vector. READ-ONLY. Run against a non-prod namespace for the bake and
#                 against prod after the values flip.
#   flip          *** GATED — operator decision required ***. Writes
#                 base_url=http://gardenio-litellm:4000 + the seeded litellm_key
#                 onto the bedrock account bead, ATOMICALLY. Requires
#                 --i-have-operator-approval=<resolved-go-decision-id> and
#                 --account (defaults to the bedrock account).
#   rollback      Restore base_url + litellm_key from the snapshot ledger,
#                 atomically. The tested escape hatch.
#
# Usage:
#   gardenio/scripts/litellm-stage3-migrate.sh snapshot [--ledger PATH]
#   gardenio/scripts/litellm-stage3-migrate.sh seed [--account NAME|ID] [--force] [--dry-run]
#   gardenio/scripts/litellm-stage3-migrate.sh verify [--account NAME|ID]
#   gardenio/scripts/litellm-stage3-migrate.sh embed-verify
#   gardenio/scripts/litellm-stage3-migrate.sh flip --account NAME|ID \
#         --i-have-operator-approval=<decision-bead-id> [--dry-run]
#   gardenio/scripts/litellm-stage3-migrate.sh rollback [--account NAME|ID] [--ledger PATH] [--dry-run]
#
# Environment:
#   NAMESPACE          gardenio namespace (default: gardenio; use a non-prod namespace for the bake)
#   LITELLM_SVC        in-cluster service name (default: gardenio-litellm; <release>-litellm elsewhere)
#   LITELLM_PORT       proxy port (default: 4000)
#   BASE_URL           in-cluster base_url written by flip
#                      (default: http://${LITELLM_SVC}:${LITELLM_PORT})
#   EXTERNAL_URL       external proxy host (default: https://litellm.app.devops.acme.example)
#   MASTER_KEY_SECRET  K8s secret holding the litellm master key (default: litellm-master-key)
#   VKEY_SECRET        K8s secret to persist minted virtual keys (default: gardenio-litellm-stage3-vkeys)
#   LEDGER             rollback ledger path (default: ./litellm-stage3-rollback-ledger.json)
#   BEDROCK_ACCOUNT    bedrock account bead title (default: litellm-bedrock)
#   EMBED_MODEL        embedding model name to probe (default: bedrock-titan-embed-v2)

NAMESPACE="${NAMESPACE:-gardenio}"
LITELLM_SVC="${LITELLM_SVC:-gardenio-litellm}"
LITELLM_PORT="${LITELLM_PORT:-4000}"
BASE_URL="${BASE_URL:-http://${LITELLM_SVC}:${LITELLM_PORT}}"
EXTERNAL_URL="${EXTERNAL_URL:-https://litellm.app.devops.acme.example}"
MASTER_KEY_SECRET="${MASTER_KEY_SECRET:-litellm-master-key}"
VKEY_SECRET="${VKEY_SECRET:-gardenio-litellm-stage3-vkeys}"
LEDGER="${LEDGER:-./litellm-stage3-rollback-ledger.json}"
BEDROCK_ACCOUNT="${BEDROCK_ACCOUNT:-litellm-bedrock}"
EMBED_MODEL="${EMBED_MODEL:-bedrock-titan-embed-v2}"

# Bedrock CHAT model groups the in-cluster proxy serves once
# litellm.bedrock.enabled=true (must mirror values.yaml litellm.bedrock.models +
# the external proxy's bedrock-* groups). Accounts whose model is NOT in this set
# cannot migrate via this script.
#
# ACL invariant (Stage-2 lesson, MR !4312 / ka-0000004250): LiteLLM checks a
# virtual key's model ACL against the LITERAL model name BEFORE model_group_alias
# resolution, so INCLUSTER_BEDROCK_MODELS MUST be a subset of KEY_MODELS, and the
# account's `model` + `probe_model` fields MUST both appear in KEY_MODELS — else
# the flip guard passes but the account 401s at runtime (and the probe stays
# green, masking it). Here both sets are identical and the litellm-bedrock
# account uses unprefixed bedrock names (model=bedrock-claude-opus-4-7,
# probe_model=bedrock-claude-haiku-4-5), so no alias resolution is involved.
INCLUSTER_BEDROCK_MODELS='bedrock-claude-opus-4-7 bedrock-claude-haiku-4-5'
# Models requested when minting the bedrock account's virtual key.
KEY_MODELS='["bedrock-claude-opus-4-7","bedrock-claude-haiku-4-5"]'
# Expected embedding vector dimensionality (matches the pgvector column width,
# kbeads migration 005_bead_embeddings).
EMBED_DIMS=1024

ACCOUNT_FILTER=""
DRY_RUN=false
FORCE=false
APPROVAL=""

SUBCMD="${1:-}"
shift || true

for arg in "$@"; do
  case "$arg" in
    --account=*) ACCOUNT_FILTER="${arg#*=}" ;;
    --account) NEXT_IS_ACCOUNT=1 ;;
    --ledger=*) LEDGER="${arg#*=}" ;;
    --i-have-operator-approval=*) APPROVAL="${arg#*=}" ;;
    --dry-run) DRY_RUN=true ;;
    --force) FORCE=true ;;
    -h|--help) sed -n '5,82p' "$0"; exit 0 ;;
    *)
      if [ "${NEXT_IS_ACCOUNT:-0}" = "1" ]; then ACCOUNT_FILTER="$arg"; NEXT_IS_ACCOUNT=0; fi
      ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need kubectl
need kd
need jq

litellm_pod() {
  kubectl -n "${NAMESPACE}" get pods -l app.kubernetes.io/component=litellm \
    -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' | awk '{print $1}'
}

fetch_master_key() {
  local k v
  for k in master-key token; do
    v="$(kubectl -n "${NAMESPACE}" get secret "${MASTER_KEY_SECRET}" \
          -o jsonpath="{.data.${k}}" 2>/dev/null || true)"
    if [ -n "${v}" ]; then printf '%s' "${v}" | base64 -d; return 0; fi
  done
  return 1
}

# POST/GET against the in-cluster proxy from inside the litellm pod (no
# port-forward). Args: METHOD PATH [JSON_BODY]. Secrets travel over stdin, never
# argv. Prints the raw JSON response; non-zero exit on HTTP/transport error.
proxy_call() {
  local method="$1" path="$2" body="${3:-}"
  printf '%s\n%s\n%s\n%s\n%s\n' "${LITELLM_PORT}" "${MASTER_KEY}" "${method}" "${path}" "${body}" \
    | kubectl -n "${NAMESPACE}" exec -i "${LITELLM_POD}" -c litellm -- python3 -c '
import sys, urllib.request, urllib.error
port=sys.stdin.readline().strip(); mk=sys.stdin.readline().strip()
method=sys.stdin.readline().strip(); path=sys.stdin.readline().strip()
body=sys.stdin.readline().rstrip("\n")
data=body.encode() if body else None
req=urllib.request.Request("http://127.0.0.1:"+port+path, data=data, method=method,
    headers={"Authorization":"Bearer "+mk,"Content-Type":"application/json"})
try:
    sys.stdout.write(urllib.request.urlopen(req,timeout=60).read().decode())
except urllib.error.HTTPError as e:
    sys.stderr.write("HTTP %d: %s\n" % (e.code, e.read().decode("utf-8","replace"))); sys.exit(1)
'
}

vkey_for() {
  local id="$1"
  kubectl -n "${NAMESPACE}" get secret "${VKEY_SECRET}" \
    -o jsonpath="{.data.${id}}" 2>/dev/null | { read -r b64 || true; [ -n "${b64:-}" ] && printf '%s' "$b64" | base64 -d; } || true
}

store_vkey() {
  local id="$1" key="$2"
  if ! kubectl -n "${NAMESPACE}" get secret "${VKEY_SECRET}" >/dev/null 2>&1; then
    kubectl -n "${NAMESPACE}" create secret generic "${VKEY_SECRET}" >/dev/null
  fi
  local b64; b64="$(printf '%s' "${key}" | base64 -w0)"
  kubectl -n "${NAMESPACE}" patch secret "${VKEY_SECRET}" --type merge \
    -p "{\"data\":{\"${id}\":\"${b64}\"}}" >/dev/null
}

# Emit "id<TAB>title<TAB>base_url<TAB>has_key<TAB>model<TAB>probe_model" for open accounts.
list_accounts() {
  kd list --type=account --status=open --json \
    | jq -r '.[] | [.id, (.title // ""), ((.fields // {}).base_url // ""),
                    (if ((.fields // {}).litellm_key // "")=="" then "no" else "yes" end),
                    ((.fields // {}).model // ""),
                    ((.fields // {}).probe_model // "")] | @tsv'
}

model_served() {
  local m="$1"; [ -z "$m" ] && return 0
  for s in ${INCLUSTER_BEDROCK_MODELS}; do [ "$m" = "$s" ] && return 0; done; return 1
}

# Default the account filter to the bedrock account when none was given (this
# stage's primary subject).
default_filter() { [ -z "${ACCOUNT_FILTER}" ] && ACCOUNT_FILTER="${BEDROCK_ACCOUNT}"; return 0; }

matches_filter() {
  local id="$1" title="$2"
  [ -z "${ACCOUNT_FILTER}" ] && return 0
  [ "${ACCOUNT_FILTER}" = "${id}" ] || [ "${ACCOUNT_FILTER}" = "${title}" ]
}

cmd_snapshot() {
  echo "=== snapshot: capturing rollback ledger -> ${LEDGER} ==="
  # Capture ALL open accounts (safety net) regardless of model; rollback only
  # touches the ones matching --account.
  kd list --type=account --status=open --json \
    | jq '[.[] | {id, title, base_url: ((.fields//{}).base_url // ""),
                  litellm_key: ((.fields//{}).litellm_key // ""),
                  model: ((.fields//{}).model // ""),
                  probe_model: ((.fields//{}).probe_model // ""),
                  provider: ((.fields//{}).provider // ""),
                  credential: ((.fields//{}).credential // "")}]' > "${LEDGER}"
  local n; n="$(jq 'length' "${LEDGER}")"
  echo "captured ${n} account beads (base_url + litellm_key per bead)."
  echo "rollback restores EXACTLY these values. Keep this file safe; it contains live keys."
}

cmd_seed() {
  default_filter
  LITELLM_POD="$(litellm_pod)"; [ -n "${LITELLM_POD}" ] || { echo "no Running litellm pod in ${NAMESPACE}" >&2; exit 1; }
  MASTER_KEY="$(fetch_master_key)" || { echo "cannot read master key from ${MASTER_KEY_SECRET}" >&2; exit 1; }
  echo "=== seed: mint bedrock virtual key in IN-CLUSTER proxy (non-destructive) ==="
  echo "pod=${LITELLM_POD}  ns=${NAMESPACE}  vkey-secret=${VKEY_SECRET}  filter=${ACCOUNT_FILTER}  dry-run=${DRY_RUN}  force=${FORCE}"
  local seeded=0 skipped=0 failed=0
  while IFS=$'\t' read -r id title _burl _haskey model _probe; do
    matches_filter "${id}" "${title}" || continue
    if ! model_served "${model}"; then
      echo "skip   ${id} (${title}) — model '${model}' NOT in in-cluster bedrock set [${INCLUSTER_BEDROCK_MODELS}]"
      skipped=$((skipped+1)); continue
    fi
    local existing; existing="$(vkey_for "${id}")"
    if [ -n "${existing}" ] && [ "${FORCE}" != "true" ]; then
      echo "skip   ${id} (${title}) — already seeded in ${VKEY_SECRET} (--force to re-mint)"
      skipped=$((skipped+1)); continue
    fi
    local alias="account-${title:-$id}"
    if [ "${DRY_RUN}" = "true" ]; then
      echo "dryrun ${id} (${title}) — would POST /key/generate alias=${alias} models=${KEY_MODELS}"
      continue
    fi
    echo "seed   ${id} (${title}) — POST /key/generate alias=${alias}"
    local body resp vkey
    body="$(jq -nc --arg a "${alias}" --argjson m "${KEY_MODELS}" '{key_alias:$a,models:$m,max_budget:null}')"
    if ! resp="$(proxy_call POST /key/generate "${body}")"; then
      echo "       FAILED /key/generate" >&2; failed=$((failed+1)); continue
    fi
    vkey="$(printf '%s' "${resp}" | jq -r '.key // empty')"
    [ -n "${vkey}" ] || { echo "       FAILED — no .key: ${resp}" >&2; failed=$((failed+1)); continue; }
    if ! proxy_call GET "/key/info?key=${vkey}" >/dev/null; then
      echo "       FAILED — minted key did not round-trip via /key/info" >&2; failed=$((failed+1)); continue
    fi
    store_vkey "${id}" "${vkey}"
    echo "       OK — minted + verified + stored in ${VKEY_SECRET}[${id}]"
    seeded=$((seeded+1))
  done < <(list_accounts)
  echo ""
  echo "=== seed summary === seeded:${seeded} skipped:${skipped} failed:${failed}"
  [ "${failed}" -eq 0 ]
}

cmd_verify() {
  default_filter
  LITELLM_POD="$(litellm_pod)"; [ -n "${LITELLM_POD}" ] || { echo "no Running litellm pod" >&2; exit 1; }
  MASTER_KEY="$(fetch_master_key)" || { echo "cannot read master key" >&2; exit 1; }
  echo "=== verify: seeded key round-trips + proxy serves model/probe_model ==="
  # Snapshot the proxy's served model list once.
  local served; served="$(proxy_call GET /v1/models 2>/dev/null | jq -r '.data[].id' 2>/dev/null || true)"
  local ok=0 miss=0 bad=0
  while IFS=$'\t' read -r id title _burl _haskey model probe; do
    matches_filter "${id}" "${title}" || continue
    model_served "${model}" || continue
    local vkey; vkey="$(vkey_for "${id}")"
    if [ -z "${vkey}" ]; then echo "MISS  ${id} (${title}) — not seeded"; miss=$((miss+1)); continue; fi
    if ! proxy_call GET "/key/info?key=${vkey}" >/dev/null 2>&1; then
      echo "BAD   ${id} (${title}) — seeded key does NOT resolve" >&2; bad=$((bad+1)); continue
    fi
    local m_ok=yes p_ok=yes
    if ! grep -qx "${model}" <<<"${served}"; then m_ok=no; fi
    if [ -n "${probe}" ] && ! grep -qx "${probe}" <<<"${served}"; then p_ok=no; fi
    if [ "${m_ok}" = "yes" ] && [ "${p_ok}" = "yes" ]; then
      echo "OK    ${id} (${title}) — key resolves; proxy serves model=${model} probe=${probe:-<none>}"; ok=$((ok+1))
    else
      echo "BAD   ${id} (${title}) — proxy missing model(${m_ok})/probe(${p_ok}); enable litellm.bedrock" >&2; bad=$((bad+1))
    fi
  done < <(list_accounts)
  echo ""
  echo "=== verify summary === ok:${ok} missing:${miss} bad:${bad}"
  [ "${bad}" -eq 0 ] && [ "${ok}" -ge 1 ]
}

cmd_embed_verify() {
  LITELLM_POD="$(litellm_pod)"; [ -n "${LITELLM_POD}" ] || { echo "no Running litellm pod in ${NAMESPACE}" >&2; exit 1; }
  MASTER_KEY="$(fetch_master_key)" || { echo "cannot read master key" >&2; exit 1; }
  echo "=== embed-verify: POST /embeddings model=${EMBED_MODEL} against ${NAMESPACE}/${LITELLM_POD} ==="
  local body resp dims
  body="$(jq -nc --arg m "${EMBED_MODEL}" '{model:$m,input:"gardenio stage-3 embedding probe"}')"
  if ! resp="$(proxy_call POST /embeddings "${body}")"; then
    echo "FAIL — /embeddings returned an error (model not served? credentials? enable litellm.embedding)" >&2
    exit 1
  fi
  dims="$(printf '%s' "${resp}" | jq -r '.data[0].embedding | length' 2>/dev/null || echo 0)"
  echo "embedding vector dims=${dims} (expected ${EMBED_DIMS})"
  if [ "${dims}" != "${EMBED_DIMS}" ]; then
    echo "FAIL — dim mismatch; pgvector column is ${EMBED_DIMS}-wide. Do NOT flip BEADS_EMBED_URL." >&2
    exit 1
  fi
  echo "OK — in-cluster proxy serves ${EMBED_MODEL} at ${EMBED_DIMS} dims. Embeddings cutover is safe for ${NAMESPACE}."
}

cmd_flip() {
  default_filter
  if [ -z "${APPROVAL}" ]; then
    echo "REFUSING: flip is operator-gated. Re-run with --i-have-operator-approval=<resolved-go-decision-bead-id>." >&2
    echo "This must name a 'gb decision' bead resolved 'go' for THIS stage (epic ka-0000000560 hard rule)." >&2
    exit 2
  fi
  LITELLM_POD="$(litellm_pod)"; MASTER_KEY="$(fetch_master_key)" || true
  echo "=== flip (GATED, approval=${APPROVAL}): repoint account ${ACCOUNT_FILTER} -> in-cluster ==="
  local hit=0
  while IFS=$'\t' read -r id title _burl _haskey model _probe; do
    matches_filter "${id}" "${title}" || continue
    hit=1
    if ! model_served "${model}"; then
      echo "REFUSING ${id} (${title}) — model '${model}' not served in-cluster" >&2; exit 2
    fi
    local vkey; vkey="$(vkey_for "${id}")"
    [ -n "${vkey}" ] || { echo "REFUSING ${id} — no seeded key in ${VKEY_SECRET}; run 'seed' first" >&2; exit 2; }
    if [ "${DRY_RUN}" = "true" ]; then
      echo "dryrun ${id} (${title}) — would set base_url=${BASE_URL} + seeded litellm_key (atomic)"
      continue
    fi
    echo "flip   ${id} (${title}) — set base_url=${BASE_URL} + litellm_key (atomic)"
    kd update "${id}" -f "base_url=${BASE_URL}" -f "litellm_key=${vkey}"
    echo "       OK — bead repointed in-cluster. Watch spend_sync + agent 401s for ~5-10min."
  done < <(list_accounts)
  [ "${hit}" = "1" ] || { echo "no matching open account for '${ACCOUNT_FILTER}'" >&2; exit 1; }
}

cmd_rollback() {
  default_filter
  [ -f "${LEDGER}" ] || { echo "no ledger at ${LEDGER}; run 'snapshot' (ideally before flip)" >&2; exit 1; }
  echo "=== rollback: restore base_url + litellm_key from ${LEDGER} (atomic per bead) ==="
  local n; n="$(jq 'length' "${LEDGER}")"
  local hit=0
  for i in $(seq 0 $((n-1))); do
    local id title burl key
    id="$(jq -r ".[$i].id" "${LEDGER}")"
    title="$(jq -r ".[$i].title" "${LEDGER}")"
    burl="$(jq -r ".[$i].base_url" "${LEDGER}")"
    key="$(jq -r ".[$i].litellm_key" "${LEDGER}")"
    matches_filter "${id}" "${title}" || continue
    hit=1
    if [ "${DRY_RUN}" = "true" ]; then
      echo "dryrun ${id} (${title}) — would restore base_url=${burl} + prior litellm_key"
      continue
    fi
    echo "restore ${id} (${title}) — base_url=${burl} + prior litellm_key (atomic)"
    kd update "${id}" -f "base_url=${burl}" -f "litellm_key=${key}"
  done
  [ "${hit}" = "1" ] || { echo "no matching ledger entry for '${ACCOUNT_FILTER}'" >&2; exit 1; }
  echo "rollback complete. Controller credential-sync propagates to running agents within ~60s."
}

case "${SUBCMD}" in
  snapshot)     cmd_snapshot ;;
  seed)         cmd_seed ;;
  verify)       cmd_verify ;;
  embed-verify) cmd_embed_verify ;;
  flip)         cmd_flip ;;
  rollback)     cmd_rollback ;;
  ""|-h|--help) sed -n '5,82p' "$0" ;;
  *) echo "unknown subcommand: ${SUBCMD}" >&2; sed -n '36,62p' "$0" >&2; exit 2 ;;
esac
