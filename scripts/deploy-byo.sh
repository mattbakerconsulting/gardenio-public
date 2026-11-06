#!/usr/bin/env bash
# deploy-byo.sh — deploy gardenio to a "bring-your-own" Kubernetes cluster.
#
# This is the PUBLIC, ACME-free deploy path. Unlike the old ACME EKS deploy
# pipeline (which ran `aws eks update-kubeconfig`, pulled secrets from AWS
# Secrets Manager, and wired Keycloak/forwardauth/Slack/LiteLLM), this
# script targets WHATEVER cluster your current kubeconfig/context points at and
# deploys the self-contained deploy-values/gardenio-byo.yaml profile — no AWS, no
# Keycloak, no ExternalSecrets, no Slack, no LiteLLM, no ACME DNS/TLS.
#
# Prerequisites (you provide):
#   * A reachable Kubernetes cluster + a kubeconfig context. We do NOT call
#     `aws eks update-kubeconfig`; set KUBECONFIG / KUBE_CONTEXT yourself.
#   * helm 3 and kubectl on PATH.
#   * If your images live in a private registry, a pull secret referenced via
#     --set imagePullSecrets[0].name=<secret> (or pre-edit the values profile).
#
# Usage:
#   scripts/deploy-byo.sh [--namespace NS] [--release NAME] [--context CTX]
#                         [--chart DIR] [--values FILE] [--extra-values FILE]...
#                         [--no-smoke] [--image-registry REG] [--app-version TAG]
#                         [--slack-channel C0…]
#
# To slack-enable the box, layer the slack overlay and pass the channel:
#   scripts/deploy-byo.sh --values deploy-values/gardenio-byo.yaml \
#     --extra-values gardenio/helm/gardenio/values-inbox.yaml \
#     --extra-values deploy-values/gardenio-inbox.yaml \
#     --extra-values deploy-values/gardenio-inbox-slack.yaml \
#     --slack-channel C0XXXXXXXXX
# (Pre-create the gardenio-slack-bridge-creds secret first — see
#  gardenio/docs/inbox-slack-enable-runbook.md.)
#
# Environment overrides (flags win over env):
#   NAMESPACE        target namespace            (default: gardenio)
#   RELEASE          helm release name           (default: gardenio)
#   KUBE_CONTEXT     kubeconfig context          (default: current-context)
#   CHART_DIR        chart path                  (default: gardenio/helm/gardenio)
#   VALUES_FILE      values overlay              (default: deploy-values/gardenio-byo.yaml)
#   IMAGE_REGISTRY   global.imageRegistry        (default: chart default)
#   APP_VERSION      image tag / Chart.appVersion(default: chart default)
#   SLACK_CHANNEL    slackBridge.slack.channel   (default: unset; overlay value)
#   GITLAB_TOKEN     GitLab PAT/deploy token (read_repository) for the agent
#                    init-clone of a private GitLab repo. Seeds the
#                    `${RELEASE}-gitlab-token` Secret (key `token`); placeholder
#                    when unset. The project bead must reference it — see the
#                    gitlab-token block below. (default: unset)
#   RUN_SMOKE        1=run smoke after deploy     (default: 1)
#
# The script pre-creates the three core secrets the BYO profile needs
# (externalSecrets is OFF) ONLY if they do not already exist, generating random
# values with `openssl rand`. Pre-create them yourself first if you want to
# control the values (e.g. share the beads token with a CLI client).
set -euo pipefail

NAMESPACE="${NAMESPACE:-gardenio}"
RELEASE="${RELEASE:-gardenio}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
CHART_DIR="${CHART_DIR:-gardenio/helm/gardenio}"
VALUES_FILE="${VALUES_FILE:-deploy-values/gardenio-byo.yaml}"
IMAGE_REGISTRY="${IMAGE_REGISTRY:-}"
APP_VERSION="${APP_VERSION:-}"
RUN_SMOKE="${RUN_SMOKE:-1}"
# Per-box Slack channel id (C0…). When set, it is passed to helm as
# `--set slackBridge.slack.channel=…` so a slack-enabled box gets its channel
# without hand-editing the deploy-values/gardenio-inbox-slack.yaml placeholder —
# keeping the overlay reusable across boxes. Enablement itself still comes from
# layering that overlay via --extra-values; this only injects the channel.
SLACK_CHANNEL="${SLACK_CHANNEL:-}"
# Additional values overlays layered ON TOP of --values (helm precedence: later
# wins). Repeatable. Used e.g. by scripts/bootstrap-ec2.sh --gvisor to add
# deploy-values/gardenio-gvisor-inbox.yaml without replacing the base profile.
EXTRA_VALUES=()

while [ $# -gt 0 ]; do
  case "$1" in
    --namespace) NAMESPACE="$2"; shift 2;;
    --release) RELEASE="$2"; shift 2;;
    --context) KUBE_CONTEXT="$2"; shift 2;;
    --chart) CHART_DIR="$2"; shift 2;;
    --values) VALUES_FILE="$2"; shift 2;;
    --extra-values) EXTRA_VALUES+=("$2"); shift 2;;
    --image-registry) IMAGE_REGISTRY="$2"; shift 2;;
    --app-version) APP_VERSION="$2"; shift 2;;
    --slack-channel) SLACK_CHANNEL="$2"; shift 2;;
    --no-smoke) RUN_SMOKE=0; shift;;
    -h|--help) sed -n '2,40p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

# Resolve repo root so relative defaults work from any cwd.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
case "$CHART_DIR" in /*) :;; *) CHART_DIR="$repo_root/$CHART_DIR";; esac
case "$VALUES_FILE" in /*) :;; *) VALUES_FILE="$repo_root/$VALUES_FILE";; esac
# Resolve + assemble the --values argument vector (base + any extra overlays).
VALUES_ARGS=(--values "$VALUES_FILE")
for ev in "${EXTRA_VALUES[@]:-}"; do
  [ -n "$ev" ] || continue
  case "$ev" in /*) :;; *) ev="$repo_root/$ev";; esac
  [ -f "$ev" ] || { echo "ERROR: extra values file not found: $ev" >&2; exit 1; }
  VALUES_ARGS+=(--values "$ev")
done

KCTL=(kubectl)
HELM=(helm)
if [ -n "$KUBE_CONTEXT" ]; then
  KCTL+=(--context "$KUBE_CONTEXT")
  HELM+=(--kube-context "$KUBE_CONTEXT")
fi

for bin in kubectl helm openssl; do
  command -v "$bin" >/dev/null 2>&1 || { echo "ERROR: '$bin' not found on PATH" >&2; exit 1; }
done
[ -f "$VALUES_FILE" ] || { echo "ERROR: values file not found: $VALUES_FILE" >&2; exit 1; }
[ -d "$CHART_DIR" ]  || { echo "ERROR: chart dir not found: $CHART_DIR" >&2; exit 1; }

echo "=== Target context: $("${KCTL[@]}" config current-context 2>/dev/null || echo '<unset>') ==="
"${KCTL[@]}" cluster-info >/dev/null 2>&1 || {
  echo "ERROR: cannot reach the cluster. Set KUBECONFIG/--context to a working cluster." >&2
  exit 1
}

echo "=== Ensuring namespace $NAMESPACE ==="
"${KCTL[@]}" create namespace "$NAMESPACE" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -

# ── Pre-create the three core secrets (externalSecrets is OFF in BYO) ───────
# postgres password secret name follows the chart helper: <release>-postgres-password
#
# stamp_helm_ownership <secret-name> — label + annotate a pre-created Secret so
# `helm upgrade --install` will ADOPT it instead of aborting the whole release
# with "invalid ownership metadata ... annotation validation error". The in-box
# chart TEMPLATES some of these Secrets itself (e.g. postgres/credentials.yaml
# owns <release>-postgres-password whenever postgres.enabled &&
# !externalSecrets.enabled — exactly the BYO/in-box profile). A bare
# `kubectl create secret` carries none of Helm's ownership metadata, so Helm 3
# refuses to take it over; the release aborts and NO workloads install. That is
# the two-pass BYO deploy failure behind the tarboat login/beads_data
# regressions (ka-0000003256 / ka-0000007542): cloud-init's first deploy attempt
# left an orphan bare postgres-password Secret, and the remediation re-run could
# not adopt it → zero pods → Keycloak/beads never came up.
#
# Idempotent (--overwrite makes re-runs a no-op) and safe for Secrets the chart
# does NOT render — Helm never attempts to adopt those, so the metadata is inert.
stamp_helm_ownership() {
  local name="$1"
  "${KCTL[@]}" -n "$NAMESPACE" label secret "$name" \
    app.kubernetes.io/managed-by=Helm --overwrite >/dev/null
  "${KCTL[@]}" -n "$NAMESPACE" annotate secret "$name" \
    "meta.helm.sh/release-name=$RELEASE" \
    "meta.helm.sh/release-namespace=$NAMESPACE" --overwrite >/dev/null
}
ensure_secret() {
  local name="$1" key="$2" value="$3"
  if "${KCTL[@]}" -n "$NAMESPACE" get secret "$name" >/dev/null 2>&1; then
    echo "  secret/$name already exists — leaving value as-is (stamping for Helm adoption)"
  else
    "${KCTL[@]}" -n "$NAMESPACE" create secret generic "$name" --from-literal="$key=$value"
    echo "  secret/$name created (key=$key)"
  fi
  # Stamp on BOTH branches: a Secret left bare by a prior (failed) deploy attempt
  # must be made adoptable now, or this run's `helm upgrade --install` re-aborts.
  stamp_helm_ownership "$name"
}
echo "=== Ensuring core secrets ==="
ensure_secret "${RELEASE}-postgres-password" password "$(openssl rand -hex 24)"
ensure_secret "kd-beads-token"               token    "$(openssl rand -hex 24)"
ensure_secret "coop-credential-key"          key      "$(openssl rand -hex 32)"

# ── In-box LiteLLM secrets (epic ka-0000007994) ─────────────────────────────
# The bundled LiteLLM proxy (litellm.enabled:true in values-inbox.yaml) reads
# two Secrets. ExternalSecrets is OFF in-box so neither is materialized by ESO —
# seed them here, create-if-missing, exactly like the core secrets above.
#
# 1. Master/salt key Secret. Name matches the gardenio.litellm.masterKeySecretName
#    helper fallback ("<release>-litellm-master-key") so the litellm Deployment
#    secretKeyRef (key `token` → LITELLM_MASTER_KEY) and the controller's
#    alias-sync admin key (same Secret) resolve with no extra wiring. `master-key`
#    and `salt-key` mirror the prod-style ExternalSecret shape; `token` is the
#    alias the Deployment actually reads. master-key == token (one value, two
#    keys) so a virtual-key issued against the master key authenticates. The
#    master key is ALSO the value an in-box account bead's litellm_key should
#    carry (see gardenio/docs/gardenio-in-a-box-install.md).
litellm_master="sk-$(openssl rand -hex 24)"
litellm_mk_secret="${RELEASE}-litellm-master-key"
if "${KCTL[@]}" -n "$NAMESPACE" get secret "$litellm_mk_secret" >/dev/null 2>&1; then
  echo "  secret/$litellm_mk_secret already exists — leaving as-is"
else
  "${KCTL[@]}" -n "$NAMESPACE" create secret generic "$litellm_mk_secret" \
    --from-literal="master-key=$litellm_master" \
    --from-literal="salt-key=sk-$(openssl rand -hex 24)" \
    --from-literal="token=$litellm_master"
  echo "  secret/$litellm_mk_secret created (keys=master-key,salt-key,token)"
fi
stamp_helm_ownership "$litellm_mk_secret"

# 2. Upstream provider-key Secret. The in-box LiteLLM holds a REAL provider API
#    key (sk-ant-api…, sent as x-api-key) rather than forwarding the agent OAuth
#    token, because a fresh EC2 egress IP is OAuth-IP-throttled (ka-0000004450).
#    The operator supplies the real key via the ANTHROPIC_API_KEY env at deploy
#    time; absent that, a placeholder is seeded so the pod starts and every
#    wiring step is provable up to the (operator-gated) provider call. NEVER
#    hardcode a key in a tracked file — this only reads the deploy-time env.
litellm_provider_secret="${RELEASE}-litellm-provider-key"
if "${KCTL[@]}" -n "$NAMESPACE" get secret "$litellm_provider_secret" >/dev/null 2>&1; then
  echo "  secret/$litellm_provider_secret already exists — leaving as-is"
else
  provider_key="${ANTHROPIC_API_KEY:-PLACEHOLDER-set-real-sk-ant-api-key-and-recreate-secret}"
  "${KCTL[@]}" -n "$NAMESPACE" create secret generic "$litellm_provider_secret" \
    --from-literal="anthropic-api-key=$provider_key"
  if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    echo "  secret/$litellm_provider_secret created (key=anthropic-api-key, from ANTHROPIC_API_KEY env)"
  else
    echo "  secret/$litellm_provider_secret created (key=anthropic-api-key, PLACEHOLDER — set ANTHROPIC_API_KEY and recreate for live inference)"
  fi
fi
stamp_helm_ownership "$litellm_provider_secret"

# In-box object store (ka-0000004955): MinIO root credentials. Seeded
# create-if-missing like the others — harmless (unreferenced) when objectStore
# is off, and ready when an in-box overlay flips it on. Two keys in one Secret:
# accesskey/secretkey, surfaced to coop/controller as AWS_ACCESS_KEY_ID/SECRET.
minio_secret="${RELEASE}-minio-credentials"
if "${KCTL[@]}" -n "$NAMESPACE" get secret "$minio_secret" >/dev/null 2>&1; then
  echo "  secret/$minio_secret already exists — leaving as-is"
else
  "${KCTL[@]}" -n "$NAMESPACE" create secret generic "$minio_secret" \
    --from-literal="accesskey=gardenio" \
    --from-literal="secretkey=$(openssl rand -hex 24)"
  echo "  secret/$minio_secret created (keys=accesskey,secretkey)"
fi
stamp_helm_ownership "$minio_secret"

# Least-privilege MinIO user for AGENT pods (gd-urSiLLwPIz): the controller
# projects this Secret onto every agent pod (COOP_S3_CREDENTIALS_SECRET), so it
# must never be the root pair above. The chart's bootstrap Job turns it into a
# MinIO user whose policy covers only the coop sessions prefix, and fails the
# install/upgrade if it is missing — so seed it here, create-if-missing.
minio_agent_secret="${RELEASE}-minio-agent-credentials"
if "${KCTL[@]}" -n "$NAMESPACE" get secret "$minio_agent_secret" >/dev/null 2>&1; then
  echo "  secret/$minio_agent_secret already exists — leaving as-is"
else
  # Via 0600 files, not --from-literal, so the key is never in kubectl's argv.
  agent_key_dir="$(umask 077 && mktemp -d)"
  printf gardenio-coop-agent > "$agent_key_dir/accesskey"
  openssl rand -hex 24 | tr -d '\n' > "$agent_key_dir/secretkey"
  "${KCTL[@]}" -n "$NAMESPACE" create secret generic "$minio_agent_secret" \
    --from-file=accesskey="$agent_key_dir/accesskey" \
    --from-file=secretkey="$agent_key_dir/secretkey"
  rm -rf "$agent_key_dir"
  echo "  secret/$minio_agent_secret created (keys=accesskey,secretkey)"
fi
stamp_helm_ownership "$minio_agent_secret"

# ── Agent GitLab clone token (ka-0000006162 / ka-0000006337) ─────────────────
# When a project bead's git_url points at a PRIVATE GitLab repo (e.g. the
# gardenio-project box cloning github.com/mattbakerconsulting/gardenio.git), the
# agent init-clone needs a credential or it dies with "fatal: could not read
# Username for https://gitlab.com". Per podspec.go, init-clone gets a GitLab
# cred ONLY when the project bead declares a secret named `<project>-*` exposing
# env GITLAB_TOKEN — the controller then writes
# `https://oauth2:$GITLAB_TOKEN@gitlab.com` into .git-credentials.
#
# Seed that Secret here, create-if-missing, exactly like the LiteLLM
# provider-key above: read a deploy-time GITLAB_TOKEN env and place it under key
# `token`; absent that, a placeholder so the wiring is provable up to the
# (operator-gated) clone. NEVER hardcode a token in a tracked file. The Secret
# name carries the project prefix the podspec.go guard requires (RELEASE
# defaults to the project name "gardenio" → "gardenio-gitlab-token").
#
# The matching project-bead secrets[] entry
#   {env: GITLAB_TOKEN, key: token, secret: ${RELEASE}-gitlab-token}
# is NOT created by this script (project beads live in the beads DB, not helm) —
# see gardenio/docs/byo-cluster.md / the in-box install runbook for that step.
gitlab_secret="${RELEASE}-gitlab-token"
if "${KCTL[@]}" -n "$NAMESPACE" get secret "$gitlab_secret" >/dev/null 2>&1; then
  echo "  secret/$gitlab_secret already exists — leaving as-is"
else
  gitlab_token="${GITLAB_TOKEN:-PLACEHOLDER-set-real-gitlab-read_repository-token-and-recreate-secret}"
  "${KCTL[@]}" -n "$NAMESPACE" create secret generic "$gitlab_secret" \
    --from-literal="token=$gitlab_token"
  if [ -n "${GITLAB_TOKEN:-}" ]; then
    echo "  secret/$gitlab_secret created (key=token, from GITLAB_TOKEN env)"
  else
    echo "  secret/$gitlab_secret created (key=token, PLACEHOLDER — set GITLAB_TOKEN and recreate for private-repo clones)"
  fi
fi
stamp_helm_ownership "$gitlab_secret"

# ── Deploy ──────────────────────────────────────────────────────────────────
HELM_SET=()
[ -n "$IMAGE_REGISTRY" ] && HELM_SET+=(--set "global.imageRegistry=$IMAGE_REGISTRY")
# Inject the per-box Slack channel id without editing the reusable slack overlay.
[ -n "$SLACK_CHANNEL" ] && HELM_SET+=(--set "slackBridge.slack.channel=$SLACK_CHANNEL")

# Image tags default to Chart.appVersion. To pin a specific release tag without
# mutating the tracked Chart.yaml, render with a temporary chart copy whose
# appVersion is rewritten (a `sed` on appVersion).
if [ -n "$APP_VERSION" ]; then
  echo "=== Pinning image tag (appVersion) to $APP_VERSION ==="
  TMP_CHART="$(mktemp -d)"
  cp -r "$CHART_DIR/." "$TMP_CHART/"
  sed -i "s/^appVersion:.*/appVersion: $APP_VERSION/" "$TMP_CHART/Chart.yaml"
  CHART_DIR="$TMP_CHART"
  trap 'rm -rf "$TMP_CHART"' EXIT
fi

echo "=== helm lint ==="
"${HELM[@]}" lint "$CHART_DIR" "${VALUES_ARGS[@]}"

echo "=== helm upgrade --install $RELEASE -> ns/$NAMESPACE ==="
"${HELM[@]}" upgrade --install "$RELEASE" "$CHART_DIR" \
  --namespace "$NAMESPACE" \
  "${VALUES_ARGS[@]}" \
  "${HELM_SET[@]}" \
  --wait --timeout 10m

echo ""
echo "--- Pods ---";        "${KCTL[@]}" get pods -n "$NAMESPACE"
echo "--- Services ---";    "${KCTL[@]}" get svc  -n "$NAMESPACE"

if [ "$RUN_SMOKE" = "1" ]; then
  echo ""
  echo "=== Running BYO smoke check ==="
  RELEASE="$RELEASE" NAMESPACE="$NAMESPACE" KUBE_CONTEXT="$KUBE_CONTEXT" \
    "$repo_root/scripts/smoke-byo.sh"
fi

echo ""
echo "=== gardenio (BYO) deployed to ns/$NAMESPACE, release=$RELEASE ==="
echo "Reach the beads daemon with:"
echo "  kubectl -n $NAMESPACE port-forward svc/${RELEASE}-beads 8080:8080"
echo "Then point kd at it: export BEADS_HTTP_ADDR=http://localhost:8080"
