#!/usr/bin/env bash
set -euo pipefail

# litellm-grafana-key.sh — Phase-0 of the OSS Grafana chat frontend (Path C,
# epic ka-0000002011, task ka-0000005911, plan ka-0000005715). Mints a DEDICATED
# LiteLLM virtual key (NOT the master key) scoped to the grafana-llm-app
# OpenAI-style model ids, and proves the acceptance: a /v1/chat/completions call
# with the scoped key + an aliased OpenAI model id returns a Bedrock completion.
#
# The durable wiring (the model_group_alias map) ships via the chart:
# litellm.modelGroupAlias rendered under router_settings.model_group_alias in
# templates/litellm/configmap.yaml. The chart default is empty, and the
# non-prod overlay that set it was deleted with RWX. This script is the
# operator-run tool that mints the scoped KEY (which is not chart material —
# it is a per-deployment secret the grafana-llm-app config consumes) and the
# acceptance harness.
#
# ─── EMPIRICAL INVARIANTS (verified live against prod gardenio-litellm v1.84.8) ───
#  1. LiteLLM only honors model_group_alias under `router_settings`. A top-level
#     block is silently ignored. `alias-seed` POSTs to router_settings via
#     /config/update; the chart renders it there too.
#  2. The virtual-key model ACL is checked against the LITERAL request model name
#     BEFORE alias resolution. A key scoped only to the Bedrock groups 401s
#     (key_model_access_denied) on `gpt-4o-mini`. So the key is scoped to the
#     OPENAI ALIAS NAMES (gpt-4o, gpt-4, gpt-4o-mini, gpt-3.5-turbo) — once the
#     alias is in router_settings, the resolved Bedrock target is NOT required in
#     the key ACL (least-privilege: alias names only).
#  3. STORE_MODEL_IN_DB=True: /config/update persists into the LiteLLM DB, and
#     the controller's startup alias-sync (litellm_aliases.go) UNION-merges the
#     claude-* set into router_settings.model_group_alias. So router_settings is
#     a shared, DB-persisted, multi-writer object. `alias-snapshot` captures the
#     EXACT current router_settings JSON and `alias-restore` POSTs it back
#     verbatim (compare-and-restore, NOT key-subtraction) so an acceptance run
#     against a live proxy is byte-reversible. Run the acceptance in a window
#     where the controller is not restarting (its alias-sync is startup-only).
#
# ─── SUBCOMMANDS ───
#   seed          Mint an EPHEMERAL virtual key scoped to the grafana OpenAI
#                 alias names. Prints the key on stdout (capture it; it is NOT
#                 persisted into a K8s Secret — the durable consumer lives in a
#                 later non-prod phase on a different cluster). Round-trips via
#                 /key/info. Idempotent on key_alias (delete + re-mint with
#                 --force).
#   alias-snapshot  Capture the EXACT current router_settings JSON to a ledger
#                 file (default ./litellm-grafana-router-snapshot.json). READ-ONLY.
#                 Run FIRST when you intend to mutate a live/shared proxy.
#   alias-seed    POST the grafana OpenAI->Bedrock aliases into
#                 router_settings.model_group_alias via /config/update, MERGING
#                 with whatever is already there (preserves claude-*). For a
#                 prod/shared proxy, run alias-snapshot first.
#   verify        ACCEPTANCE: POST /v1/chat/completions with a given scoped key
#                 + an aliased OpenAI model id; assert HTTP 200 and a non-empty
#                 assistant message (a Bedrock completion). READ-ONLY (no config
#                 mutation). Pass --key <sk-…> and optionally --model <id>.
#   alias-restore Restore router_settings to the EXACT snapshot captured by
#                 alias-snapshot. The reversible escape hatch.
#   key-delete    Delete a minted key (--key <sk-…>). Use after an acceptance run
#                 that should leave the proxy byte-unchanged.
#
# ─── USAGE ───
#   gardenio/scripts/litellm-grafana-key.sh seed [--force]
#   gardenio/scripts/litellm-grafana-key.sh alias-snapshot [--ledger PATH]
#   gardenio/scripts/litellm-grafana-key.sh alias-seed [--dry-run]
#   gardenio/scripts/litellm-grafana-key.sh verify --key sk-… [--model gpt-4o-mini]
#   gardenio/scripts/litellm-grafana-key.sh alias-restore [--ledger PATH] [--dry-run]
#   gardenio/scripts/litellm-grafana-key.sh key-delete --key sk-…
#
# ─── ENVIRONMENT ───
#   NAMESPACE          gardenio namespace (default: gardenio; a non-prod namespace for the bake)
#   LITELLM_SVC        in-cluster service name (default: gardenio-litellm)
#   LITELLM_PORT       proxy port (default: 4000)
#   MASTER_KEY_SECRET  K8s secret holding the master key (default: litellm-master-key)
#   KEY_ALIAS          virtual-key alias (default: grafana-llm-app)
#   KEY_BUDGET         max_budget on the minted key (default: null = unlimited)
#   LEDGER             router snapshot ledger path
#                      (default: ./litellm-grafana-router-snapshot.json)

NAMESPACE="${NAMESPACE:-gardenio}"
LITELLM_SVC="${LITELLM_SVC:-gardenio-litellm}"
LITELLM_PORT="${LITELLM_PORT:-4000}"
MASTER_KEY_SECRET="${MASTER_KEY_SECRET:-litellm-master-key}"
KEY_ALIAS="${KEY_ALIAS:-grafana-llm-app}"
KEY_BUDGET="${KEY_BUDGET:-null}"
LEDGER="${LEDGER:-./litellm-grafana-router-snapshot.json}"

# grafana-llm-app OpenAI-provider model ids -> in-cluster Bedrock chat groups.
# Defaults: Large->gpt-4o, Base->gpt-4o-mini (grafana-llm-app docs); gpt-4 /
# gpt-3.5-turbo included for admin-customized mappings. MUST mirror the
# litellm.modelGroupAlias gpt-* entries of whichever overlay sets them.
GRAFANA_ALIASES_JSON='{
  "gpt-4o": "bedrock-claude-opus-4-7",
  "gpt-4": "bedrock-claude-opus-4-7",
  "gpt-4o-mini": "bedrock-claude-haiku-4-5",
  "gpt-3.5-turbo": "bedrock-claude-haiku-4-5"
}'
# The key ACL is the alias NAMES only (invariant 2). Keep in sync with the keys
# of GRAFANA_ALIASES_JSON.
KEY_MODELS_JSON='["gpt-4o","gpt-4","gpt-4o-mini","gpt-3.5-turbo"]'
# Default model id the acceptance `verify` exercises.
DEFAULT_VERIFY_MODEL="gpt-4o-mini"

DRY_RUN=false
FORCE=false
ARG_KEY=""
ARG_MODEL=""

SUBCMD="${1:-}"
shift || true
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=true ;;
    --force) FORCE=true ;;
    --ledger) LEDGER="$2"; shift ;;
    --ledger=*) LEDGER="${1#*=}" ;;
    --key) ARG_KEY="$2"; shift ;;
    --key=*) ARG_KEY="${1#*=}" ;;
    --model) ARG_MODEL="$2"; shift ;;
    --model=*) ARG_MODEL="${1#*=}" ;;
    -h|--help) sed -n '5,75p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
  shift
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need kubectl
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
# port-forward). Args: BEARER METHOD PATH [JSON_BODY]. Secrets travel over
# stdin, never argv. Prints the raw JSON response; non-zero exit on HTTP error.
proxy_call() {
  local bearer="$1" method="$2" path="$3" body="${4:-}"
  printf '%s\n%s\n%s\n%s\n%s\n' "${LITELLM_PORT}" "${bearer}" "${method}" "${path}" "${body}" \
    | kubectl -n "${NAMESPACE}" exec -i "${LITELLM_POD}" -c litellm -- python3 -c '
import sys, urllib.request, urllib.error
port=sys.stdin.readline().strip(); key=sys.stdin.readline().strip()
method=sys.stdin.readline().strip(); path=sys.stdin.readline().strip()
body=sys.stdin.readline().rstrip("\n")
data=body.encode() if body else None
req=urllib.request.Request("http://127.0.0.1:"+port+path, data=data, method=method,
    headers={"Authorization":"Bearer "+key,"Content-Type":"application/json"})
try:
    sys.stdout.write(urllib.request.urlopen(req,timeout=60).read().decode())
except urllib.error.HTTPError as e:
    sys.stderr.write("HTTP %d: %s\n" % (e.code, e.read().decode("utf-8","replace"))); sys.exit(1)
'
}

setup_proxy() {
  LITELLM_POD="$(litellm_pod)"
  [ -n "${LITELLM_POD}" ] || { echo "no Running litellm pod in ${NAMESPACE}" >&2; exit 1; }
  MASTER_KEY="$(fetch_master_key)" || { echo "cannot read master key from ${MASTER_KEY_SECRET}" >&2; exit 1; }
}

cmd_seed() {
  setup_proxy
  # All diagnostics go to stderr; the minted key plaintext is the ONLY thing on
  # stdout (so `KEY=$(... seed)` captures exactly the key).
  echo "=== seed: mint EPHEMERAL grafana virtual key (scoped, NOT master) ===" >&2
  echo "pod=${LITELLM_POD}  ns=${NAMESPACE}  alias=${KEY_ALIAS}  models=${KEY_MODELS_JSON}  force=${FORCE}" >&2
  if [ "${FORCE}" = "true" ]; then
    # Best-effort delete of prior key(s) with this alias (key_alias must be
    # unique across all keys). /key/list returns the hashed token; /key/delete
    # accepts that hash in key_aliases OR the hash in keys — use key_aliases.
    proxy_call "${MASTER_KEY}" POST /key/delete \
      "$(jq -nc --arg a "${KEY_ALIAS}" '{key_aliases:[$a]}')" >/dev/null 2>&1 || true
  fi
  local body resp vkey
  body="$(jq -nc --arg a "${KEY_ALIAS}" --argjson m "${KEY_MODELS_JSON}" --argjson b "${KEY_BUDGET}" \
            '{key_alias:$a,models:$m,max_budget:$b}')"
  if [ "${DRY_RUN}" = "true" ]; then
    echo "dryrun — would POST /key/generate ${body}" >&2; return 0
  fi
  resp="$(proxy_call "${MASTER_KEY}" POST /key/generate "${body}")" \
    || { echo "FAILED /key/generate" >&2; exit 1; }
  vkey="$(printf '%s' "${resp}" | jq -r '.key // empty')"
  [ -n "${vkey}" ] || { echo "FAILED — no .key in response: ${resp}" >&2; exit 1; }
  proxy_call "${MASTER_KEY}" GET "/key/info?key=${vkey}" >/dev/null \
    || { echo "FAILED — minted key did not round-trip via /key/info" >&2; exit 1; }
  echo "OK — minted + verified scoped key (alias=${KEY_ALIAS}). Plaintext key on the next line:" >&2
  printf '%s\n' "${vkey}"
}

cmd_alias_snapshot() {
  setup_proxy
  echo "=== alias-snapshot: capture EXACT router_settings -> ${LEDGER} ==="
  local resp
  resp="$(proxy_call "${MASTER_KEY}" GET /get/config/callbacks)" \
    || { echo "FAILED to fetch router config" >&2; exit 1; }
  printf '%s' "${resp}" | jq '.router_settings // {}' > "${LEDGER}"
  echo "captured router_settings ($(jq -c '.model_group_alias // {} | keys' "${LEDGER}")) -> ${LEDGER}"
  echo "alias-restore will POST this EXACT object back. Keep it for the duration of the acceptance run."
}

cmd_alias_seed() {
  setup_proxy
  echo "=== alias-seed: merge grafana OpenAI->Bedrock aliases into router_settings.model_group_alias ==="
  local current merged
  current="$(proxy_call "${MASTER_KEY}" GET /get/config/callbacks \
              | jq '.router_settings.model_group_alias // {}')" \
    || { echo "FAILED to fetch current aliases" >&2; exit 1; }
  merged="$(jq -nc --argjson cur "${current}" --argjson add "${GRAFANA_ALIASES_JSON}" '$cur + $add')"
  echo "current aliases: $(printf '%s' "${current}" | jq -c 'keys')"
  echo "merged aliases:  $(printf '%s' "${merged}" | jq -c 'keys')"
  if [ "${DRY_RUN}" = "true" ]; then
    echo "dryrun — would POST /config/update router_settings.model_group_alias=${merged}"; return 0
  fi
  local body; body="$(jq -nc --argjson m "${merged}" '{router_settings:{model_group_alias:$m}}')"
  proxy_call "${MASTER_KEY}" POST /config/update "${body}" >/dev/null \
    || { echo "FAILED /config/update" >&2; exit 1; }
  echo "OK — grafana aliases active in router_settings."
}

cmd_verify() {
  setup_proxy
  [ -n "${ARG_KEY}" ] || { echo "verify requires --key <sk-…> (the scoped key from seed)" >&2; exit 2; }
  local model="${ARG_MODEL:-$DEFAULT_VERIFY_MODEL}"
  echo "=== verify (ACCEPTANCE): /v1/chat/completions key=${ARG_KEY:0:10}… model=${model} ==="
  local body resp content
  body="$(jq -nc --arg m "${model}" \
            '{model:$m,max_tokens:16,messages:[{role:"user",content:"Reply with the single word: pong"}]}')"
  resp="$(proxy_call "${ARG_KEY}" POST /v1/chat/completions "${body}")" \
    || { echo "FAILED — scoped key + aliased model did NOT return a completion" >&2; exit 1; }
  content="$(printf '%s' "${resp}" | jq -r '.choices[0].message.content // empty')"
  local routed; routed="$(printf '%s' "${resp}" | jq -r '.model // empty')"
  [ -n "${content}" ] || { echo "FAILED — empty completion content: ${resp}" >&2; exit 1; }
  echo "OK — Bedrock completion via scoped key + aliased model:"
  echo "     requested model: ${model}  | routed model: ${routed}"
  echo "     content: ${content}"
}

cmd_alias_restore() {
  setup_proxy
  [ -f "${LEDGER}" ] || { echo "no snapshot ledger at ${LEDGER} (run alias-snapshot first)" >&2; exit 1; }
  echo "=== alias-restore: POST EXACT snapshot router_settings from ${LEDGER} ==="
  local snap; snap="$(cat "${LEDGER}")"
  if [ "${DRY_RUN}" = "true" ]; then
    echo "dryrun — would POST /config/update router_settings=$(printf '%s' "${snap}" | jq -c .)"; return 0
  fi
  local body; body="$(jq -nc --argjson rs "${snap}" '{router_settings:$rs}')"
  proxy_call "${MASTER_KEY}" POST /config/update "${body}" >/dev/null \
    || { echo "FAILED /config/update (restore)" >&2; exit 1; }
  echo "OK — router_settings restored to the captured snapshot."
}

cmd_key_delete() {
  setup_proxy
  [ -n "${ARG_KEY}" ] || { echo "key-delete requires --key <sk-…>" >&2; exit 2; }
  echo "=== key-delete: ${ARG_KEY:0:10}… ==="
  proxy_call "${MASTER_KEY}" POST /key/delete "{\"keys\":[\"${ARG_KEY}\"]}" >/dev/null \
    || { echo "FAILED /key/delete" >&2; exit 1; }
  echo "OK — key deleted."
}

case "${SUBCMD}" in
  seed)            cmd_seed ;;
  alias-snapshot)  cmd_alias_snapshot ;;
  alias-seed)      cmd_alias_seed ;;
  verify)          cmd_verify ;;
  alias-restore)   cmd_alias_restore ;;
  key-delete)      cmd_key_delete ;;
  ""|-h|--help)    sed -n '5,75p' "$0" ;;
  *) echo "unknown subcommand: ${SUBCMD}" >&2; sed -n '38,55p' "$0" >&2; exit 2 ;;
esac
