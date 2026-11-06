#!/usr/bin/env bash
# seed-litellm-account.sh — seed an in-box account bead pointing at the
# in-cluster LiteLLM proxy (epic ka-0000007994).
#
# After `values-inbox.yaml` enables the bundled LiteLLM (litellm.enabled:true)
# and deploy-byo.sh seeds the master-key Secret, this script wires the spawn
# path: it creates a type=account bead whose base_url is the in-cluster LiteLLM
# Service and whose litellm_key is the master key from the seeded Secret. The
# controller's applyAccountConfig then bakes ANTHROPIC_BASE_URL + ANTHROPIC_MODEL
# + ANTHROPIC_CUSTOM_HEADERS onto every agent pod for the labelled project, so
# `gb spawn` resolves a model end-to-end against the in-box proxy.
#
# Requires: kubectl (against the box, e.g. via SSH tunnel) + kd pointed at the
# in-box beads daemon. Run from a context that can reach both.
#
# Usage:
#   scripts/inbox/seed-litellm-account.sh [--namespace NS] [--release NAME]
#       [--project PROJECT] [--model MODEL] [--account-title TITLE]
#
# Env overrides (flags win): NAMESPACE, RELEASE, PROJECT, MODEL, ACCOUNT_TITLE.
set -euo pipefail

NAMESPACE="${NAMESPACE:-gardenio}"
RELEASE="${RELEASE:-gardenio}"
PROJECT="${PROJECT:-gardenio}"
MODEL="${MODEL:-claude-opus-5-5}"
ACCOUNT_TITLE="${ACCOUNT_TITLE:-claude-inbox-litellm-${PROJECT}}"
# OAUTH_SECRET names the K8s secret holding the Claude Code OAuth token (key
# "token"). applyAccountConfig injects it as CLAUDE_CODE_OAUTH_TOKEN via the
# account's `credential=secret:<name>` field. WITHOUT this, the agent gets a
# base_url but no upstream credential and every model call 401s — the agent
# boots and then goes silent (ka-0000000385).
OAUTH_SECRET="${OAUTH_SECRET:-}"
# OPERATED_BY satisfies the beads server-side operator-resolution guard on
# create (otherwise: "operator could not be resolved"). Override per box.
OPERATED_BY="${OPERATED_BY:-oidc:matthew.baker@acme.example}"

while [ $# -gt 0 ]; do
  case "$1" in
    --namespace) NAMESPACE="$2"; shift 2;;
    --release) RELEASE="$2"; shift 2;;
    --project) PROJECT="$2"; shift 2;;
    --model) MODEL="$2"; shift 2;;
    --account-title) ACCOUNT_TITLE="$2"; shift 2;;
    --oauth-secret) OAUTH_SECRET="$2"; shift 2;;
    --operated-by) OPERATED_BY="$2"; shift 2;;
    -h|--help) sed -n '2,30p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

for bin in kubectl kd; do
  command -v "$bin" >/dev/null 2>&1 || { echo "ERROR: '$bin' not found on PATH" >&2; exit 1; }
done

mk_secret="${RELEASE}-litellm-master-key"
base_url="http://${RELEASE}-litellm.${NAMESPACE}.svc.cluster.local:4000"

echo "=== Reading litellm master key from secret/${mk_secret} (ns/${NAMESPACE}) ==="
litellm_key="$(kubectl -n "$NAMESPACE" get secret "$mk_secret" -o jsonpath='{.data.token}' | base64 -d)"
[ -n "$litellm_key" ] || { echo "ERROR: secret/${mk_secret} has empty 'token' key" >&2; exit 1; }

# Auto-discover the Claude OAuth secret if not pinned: first secret matching
# "<release>-claude-oauth-*" in the namespace.
if [ -z "$OAUTH_SECRET" ]; then
  OAUTH_SECRET="$(kubectl -n "$NAMESPACE" get secrets -o name 2>/dev/null \
    | sed 's#^secret/##' | grep -m1 "^${RELEASE}-claude-oauth-" || true)"
fi

# Title is a POSITIONAL arg in `kd create <title>`, NOT --title. provider is a
# REQUIRED field on type=account (server rejects create without it). credential
# wires the OAuth token. --operated-by satisfies the operator guard.
cred_args=()
if [ -n "$OAUTH_SECRET" ]; then
  echo "    credential=secret:${OAUTH_SECRET}"
  cred_args=(--field "credential=secret:${OAUTH_SECRET}")
else
  echo "    WARNING: no <release>-claude-oauth-* secret found; account will have"
  echo "             no OAuth credential and agents will fail to authenticate."
  echo "             Pass --oauth-secret <name> once the token secret exists."
fi

echo "=== Creating account bead '${ACCOUNT_TITLE}' (project:${PROJECT}, model=${MODEL}) ==="
echo "    base_url=${base_url}"
kd create "$ACCOUNT_TITLE" --type account \
  --operated-by "$OPERATED_BY" \
  --field "provider=anthropic" \
  --field "base_url=${base_url}" \
  --field "model=${MODEL}" \
  --field "litellm_key=${litellm_key}" \
  "${cred_args[@]}" \
  --label "project:${PROJECT}"

echo ""
echo "=== Done. The controller's applyAccountConfig will bake ANTHROPIC_BASE_URL/"
echo "    ANTHROPIC_MODEL/ANTHROPIC_CUSTOM_HEADERS/CLAUDE_CODE_OAUTH_TOKEN onto"
echo "    agent pods for project:${PROJECT}. Spawn with: gb spawn --project ${PROJECT} ..."
