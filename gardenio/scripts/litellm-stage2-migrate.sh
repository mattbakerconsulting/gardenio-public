#!/usr/bin/env bash
set -euo pipefail

# litellm-stage2-migrate.sh — Stage-2 of the in-cluster LiteLLM migration
# (epic ka-0000000560, task ka-0000000158). This is the SENSITIVE, operator-gated
# cutover of LIVE prod account beads from the EXTERNAL LiteLLM proxy
# (https://litellm.app.devops.acme.example) to the in-cluster proxy
# (http://gardenio-litellm:4000).
#
# Stage-1 (ka-0000004799) stood the in-cluster proxy up in prod and pointed the
# GLOBAL controller LITELLM_URL at it, but live traffic is UNCHANGED because each
# account bead's per-account base_url takes precedence (podspec.go:970-977, fix
# ka-0000003221). Stage-2 flips the per-account base_url + litellm_key.
#
# HARD GATE (epic ka-0000000560): NEVER `flip` a live prod account without a green
# non-prod bake, a TESTED rollback, AND a fresh operator decision-gate. This
# script deliberately SEPARATES the non-destructive prep (`seed`, `verify`,
# `snapshot`) from the gated, destructive `flip` so prep can run freely while the
# flip stays behind the gate.
#
# The controller treats base_url-set + empty-litellm_key as UNUSABLE
# (accountInfoUnusable, main.go:1754 — every agent 401s). So BOTH `flip` and
# `rollback` MUST write base_url AND litellm_key together, atomically, per bead.
#
# Subcommands:
#   snapshot   Capture current base_url + litellm_key for every open account bead
#              into a rollback ledger JSON. READ-ONLY on cluster + beads. Run this
#              FIRST so rollback can restore exact prior values.
#   seed       Mint a per-account virtual key in the IN-CLUSTER litellm DB and
#              persist the plaintext key into a K8s Secret (default
#              gardenio-litellm-stage2-vkeys, one data key per account-bead-id).
#              NON-DESTRUCTIVE: adds keys to the litellm DB + a Secret; does NOT
#              touch any account bead, so live traffic (still external) is
#              unaffected. Idempotent (skips accounts already seeded; --force to
#              re-mint). Verifies each minted key round-trips via /key/info.
#   verify     Re-check that every seeded key still resolves via /key/info on the
#              in-cluster proxy. READ-ONLY.
#   flip       *** GATED — operator decision required ***. For each named account,
#              writes base_url=http://gardenio-litellm:4000 + the seeded litellm_key
#              onto the bead, ATOMICALLY. Requires --i-have-operator-approval=<id>
#              naming the resolved `go` decision bead, else it refuses to run.
#              Staged: pass --account to flip ONE bead; re-run per account.
#   rollback   Restore base_url + litellm_key from the snapshot ledger, atomically.
#              The tested escape hatch. Pass --account to roll back ONE bead.
#
# Scope note: the in-cluster proxy serves only the OAuth model groups
# (claude-opus-4-{6,7,8}-oauth, claude-sonnet-4-6-oauth, anthropic/claude-haiku-4-5).
# It does NOT serve bedrock models, so the `litellm-bedrock` account
# (model bedrock-claude-opus-4-7) MUST stay external this stage and is skipped by
# `seed`/`flip` unless explicitly named.
#
# Usage:
#   gardenio/scripts/litellm-stage2-migrate.sh snapshot [--ledger PATH]
#   gardenio/scripts/litellm-stage2-migrate.sh seed [--account NAME|ID] [--force] [--dry-run]
#   gardenio/scripts/litellm-stage2-migrate.sh verify
#   gardenio/scripts/litellm-stage2-migrate.sh flip  --account NAME|ID \
#         --i-have-operator-approval=<decision-bead-id> [--dry-run]
#   gardenio/scripts/litellm-stage2-migrate.sh rollback [--account NAME|ID] [--ledger PATH] [--dry-run]
#
# Environment:
#   NAMESPACE          gardenio namespace (default: gardenio)
#   LITELLM_SVC        in-cluster service name (default: gardenio-litellm)
#   LITELLM_PORT       proxy port (default: 4000)
#   BASE_URL           in-cluster base_url written by flip
#                      (default: http://${LITELLM_SVC}:${LITELLM_PORT})
#   EXTERNAL_URL       external proxy host (default: https://litellm.app.devops.acme.example)
#   MASTER_KEY_SECRET  K8s secret holding the litellm master key (default: litellm-master-key)
#   VKEY_SECRET        K8s secret to persist minted virtual keys (default: gardenio-litellm-stage2-vkeys)
#   LEDGER             rollback ledger path (default: ./litellm-stage2-rollback-ledger.json)

NAMESPACE="${NAMESPACE:-gardenio}"
LITELLM_SVC="${LITELLM_SVC:-gardenio-litellm}"
LITELLM_PORT="${LITELLM_PORT:-4000}"
BASE_URL="${BASE_URL:-http://${LITELLM_SVC}:${LITELLM_PORT}}"
EXTERNAL_URL="${EXTERNAL_URL:-https://litellm.app.devops.acme.example}"
MASTER_KEY_SECRET="${MASTER_KEY_SECRET:-litellm-master-key}"
VKEY_SECRET="${VKEY_SECRET:-gardenio-litellm-stage2-vkeys}"
LEDGER="${LEDGER:-./litellm-stage2-rollback-ledger.json}"

# Model groups the in-cluster proxy serves. Accounts whose model is NOT in this
# set (e.g. bedrock) cannot migrate this stage and are skipped by seed/flip.
#
# This set MUST be a subset of KEY_MODELS below: the minted virtual key only
# grants access to KEY_MODELS, and LiteLLM enforces that key ACL against the
# literal requested model name BEFORE model_group_alias resolution. If an
# account's `model` field is in INCLUSTER_MODELS but NOT in KEY_MODELS, it
# passes model_served()/the flip guard yet every agent request 401s with
# key_model_access_denied (ka-0000004250: first-mate-haiku had bare
# `claude-haiku-4-5`, whose key only granted the prefixed
# `anthropic/claude-haiku-4-5`). Keep the haiku entry prefixed-only.
#
# claude-opus-5-oauth was added when Opus 5 shipped as a served model group and
# became the `model` on every OAuth account bead — but BOTH lists here were left
# on the 4-8-era set. That is the ka-0000004250 failure above, recurred: all six
# OAuth account keys granted 4-8/4-7/4-6/sonnet/fable/haiku and none granted
# opus-5, so every agent running the account default 401'd on its first
# inference turn ("key not allowed to access model … Tried to access
# claude-opus-5-oauth"). coop classifies that as ErrorCategory::Unauthorized →
# marks the session a zombie → /livez 503 → the kubelet restarts the container
# → the fresh session 401s again, a crash-loop that never recovers because a
# restart cannot widen a key ACL. Whenever a new model group becomes an account
# `model` or `probe_model`, it MUST be added to both lists in the same change.
INCLUSTER_MODELS='claude-opus-5-5-oauth claude-opus-5-oauth claude-opus-4-8-oauth claude-opus-4-7-oauth claude-opus-4-6-oauth claude-sonnet-5-5-oauth claude-sonnet-5-oauth claude-sonnet-4-6-oauth claude-fable-5-1-oauth claude-fable-5-oauth anthropic/claude-haiku-4-5'
# Models requested when minting a virtual key (broad OAuth set; covers every
# OAuth account model). Bedrock is deliberately excluded.
KEY_MODELS='["claude-opus-5-5-oauth","claude-opus-5-oauth","claude-opus-4-8-oauth","claude-opus-4-7-oauth","claude-opus-4-6-oauth","claude-sonnet-5-5-oauth","claude-sonnet-5-oauth","claude-sonnet-4-6-oauth","claude-fable-5-1-oauth","claude-fable-5-oauth","anthropic/claude-haiku-4-5"]'

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
    -h|--help) sed -n '5,80p' "$0"; exit 0 ;;
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
  # Try master-key, then token (the in-cluster Secret carries both, equal).
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
    sys.stdout.write(urllib.request.urlopen(req,timeout=30).read().decode())
except urllib.error.HTTPError as e:
    sys.stderr.write("HTTP %d: %s\n" % (e.code, e.read().decode("utf-8","replace"))); sys.exit(1)
'
}

# Read minted key for an account id out of the vkey Secret (empty if absent).
vkey_for() {
  local id="$1"
  kubectl -n "${NAMESPACE}" get secret "${VKEY_SECRET}" \
    -o jsonpath="{.data.${id}}" 2>/dev/null | { read -r b64 || true; [ -n "${b64:-}" ] && printf '%s' "$b64" | base64 -d; } || true
}

# Persist a minted key for an account id into the vkey Secret (create-or-patch).
store_vkey() {
  local id="$1" key="$2"
  if ! kubectl -n "${NAMESPACE}" get secret "${VKEY_SECRET}" >/dev/null 2>&1; then
    kubectl -n "${NAMESPACE}" create secret generic "${VKEY_SECRET}" >/dev/null
  fi
  local b64; b64="$(printf '%s' "${key}" | base64 -w0)"
  kubectl -n "${NAMESPACE}" patch secret "${VKEY_SECRET}" --type merge \
    -p "{\"data\":{\"${id}\":\"${b64}\"}}" >/dev/null
}

# Emit "id<TAB>title<TAB>base_url<TAB>has_key<TAB>model" for open account beads.
list_accounts() {
  kd list --type=account --status=open --json \
    | jq -r '.[] | [.id, (.title // ""), ((.fields // {}).base_url // ""),
                    (if ((.fields // {}).litellm_key // "")=="" then "no" else "yes" end),
                    ((.fields // {}).model // "")] | @tsv'
}

model_served() {
  local m="$1"; for s in ${INCLUSTER_MODELS}; do [ "$m" = "$s" ] && return 0; done; return 1
}

matches_filter() {
  local id="$1" title="$2"
  [ -z "${ACCOUNT_FILTER}" ] && return 0
  [ "${ACCOUNT_FILTER}" = "${id}" ] || [ "${ACCOUNT_FILTER}" = "${title}" ]
}

cmd_snapshot() {
  echo "=== snapshot: capturing rollback ledger -> ${LEDGER} ==="
  kd list --type=account --status=open --json \
    | jq '[.[] | {id, title, base_url: ((.fields//{}).base_url // ""),
                  litellm_key: ((.fields//{}).litellm_key // ""),
                  model: ((.fields//{}).model // ""),
                  provider: ((.fields//{}).provider // ""),
                  credential: ((.fields//{}).credential // "")}]' > "${LEDGER}"
  local n; n="$(jq 'length' "${LEDGER}")"
  echo "captured ${n} account beads (base_url + litellm_key per bead)."
  echo "rollback restores EXACTLY these values. Keep this file safe; it contains live keys."
}

cmd_seed() {
  LITELLM_POD="$(litellm_pod)"; [ -n "${LITELLM_POD}" ] || { echo "no Running litellm pod in ${NAMESPACE}" >&2; exit 1; }
  MASTER_KEY="$(fetch_master_key)" || { echo "cannot read master key from ${MASTER_KEY_SECRET}" >&2; exit 1; }
  echo "=== seed: mint per-account virtual keys in IN-CLUSTER proxy (non-destructive) ==="
  echo "pod=${LITELLM_POD}  vkey-secret=${VKEY_SECRET}  dry-run=${DRY_RUN}  force=${FORCE}"
  local seeded=0 skipped=0 failed=0
  while IFS=$'\t' read -r id title _burl _haskey model; do
    matches_filter "${id}" "${title}" || continue
    if ! model_served "${model}"; then
      echo "skip   ${id} (${title}) — model '${model}' NOT served in-cluster (bedrock); stays external"
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
    # round-trip verify via /key/info (proves salt decrypt)
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
  LITELLM_POD="$(litellm_pod)"; [ -n "${LITELLM_POD}" ] || { echo "no Running litellm pod" >&2; exit 1; }
  MASTER_KEY="$(fetch_master_key)" || { echo "cannot read master key" >&2; exit 1; }
  echo "=== verify: re-check seeded keys round-trip via /key/info ==="
  local ok=0 miss=0 bad=0
  while IFS=$'\t' read -r id title _burl _haskey model; do
    matches_filter "${id}" "${title}" || continue
    model_served "${model}" || continue
    local vkey; vkey="$(vkey_for "${id}")"
    if [ -z "${vkey}" ]; then echo "MISS  ${id} (${title}) — not seeded"; miss=$((miss+1)); continue; fi
    if proxy_call GET "/key/info?key=${vkey}" >/dev/null 2>&1; then
      echo "OK    ${id} (${title}) — key resolves in-cluster"; ok=$((ok+1))
    else
      echo "BAD   ${id} (${title}) — seeded key does NOT resolve" >&2; bad=$((bad+1))
    fi
  done < <(list_accounts)
  echo ""
  echo "=== verify summary === ok:${ok} missing:${miss} bad:${bad}"
  [ "${bad}" -eq 0 ]
}

cmd_flip() {
  # GATED. Refuse without an explicit operator-approval decision id.
  if [ -z "${APPROVAL}" ]; then
    echo "REFUSING: flip is operator-gated. Re-run with --i-have-operator-approval=<resolved-go-decision-bead-id>." >&2
    echo "This must name a 'gb decision' bead resolved 'go' for THIS stage (epic ka-0000000560 hard rule)." >&2
    exit 2
  fi
  if [ -z "${ACCOUNT_FILTER}" ]; then
    echo "REFUSING: flip requires --account NAME|ID (staged, one account at a time)." >&2
    exit 2
  fi
  LITELLM_POD="$(litellm_pod)"; MASTER_KEY="$(fetch_master_key)" || true
  echo "=== flip (GATED, approval=${APPROVAL}): repoint account ${ACCOUNT_FILTER} -> in-cluster ==="
  local hit=0
  while IFS=$'\t' read -r id title _burl _haskey model; do
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
  [ -f "${LEDGER}" ] || { echo "no ledger at ${LEDGER}; run 'snapshot' (ideally before flip)" >&2; exit 1; }
  echo "=== rollback: restore base_url + litellm_key from ${LEDGER} (atomic per bead) ==="
  local n; n="$(jq 'length' "${LEDGER}")"
  for i in $(seq 0 $((n-1))); do
    local id title burl key
    id="$(jq -r ".[$i].id" "${LEDGER}")"
    title="$(jq -r ".[$i].title" "${LEDGER}")"
    burl="$(jq -r ".[$i].base_url" "${LEDGER}")"
    key="$(jq -r ".[$i].litellm_key" "${LEDGER}")"
    matches_filter "${id}" "${title}" || continue
    if [ "${DRY_RUN}" = "true" ]; then
      echo "dryrun ${id} (${title}) — would restore base_url=${burl} + prior litellm_key"
      continue
    fi
    echo "restore ${id} (${title}) — base_url=${burl} + prior litellm_key (atomic)"
    kd update "${id}" -f "base_url=${burl}" -f "litellm_key=${key}"
  done
  echo "rollback complete. Controller credential-sync propagates to running agents within ~60s."
}

case "${SUBCMD}" in
  snapshot) cmd_snapshot ;;
  seed)     cmd_seed ;;
  verify)   cmd_verify ;;
  flip)     cmd_flip ;;
  rollback) cmd_rollback ;;
  ""|-h|--help) sed -n '5,80p' "$0" ;;
  *) echo "unknown subcommand: ${SUBCMD}" >&2; sed -n '36,60p' "$0" >&2; exit 2 ;;
esac
