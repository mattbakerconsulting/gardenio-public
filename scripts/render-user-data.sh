#!/usr/bin/env bash
# render-user-data.sh — substitute @@PLACEHOLDER@@ tokens in the in-box
# #cloud-config user-data template, emitting a ready-to-launch user-data file on
# stdout. Phase 0, epic ka-0000007690 (task ka-0000003206).
#
# Usage:
#   scripts/render-user-data.sh [--scenario A|B|C] [--repo-url URL] [--repo-ref REF]
#     [--repo-token TOKEN] [--repo-token-user USER]
#     [--namespace NS] [--values FILE] [--extra-values "F1 F2"] [--slack-channel C0…]
#     [--gvisor 0|1] [--harden 0|1] [--operator-user USER] [--k3s-version VER]
#     [--app-version TAG] [--offline-assets DIR] [--template FILE] > user-data.yaml
#
# gardenio is a PRIVATE repo. A fresh box clones it in cloud-init, so unless the
# repo is already staged on the box (AMI-baked / air-gap bundle under
# /opt/gardenio/repo), you MUST supply a read-only clone credential — either
#   GARDENIO_REPO_TOKEN=glpat-xxxx scripts/render-user-data.sh ...   (preferred)
# or  scripts/render-user-data.sh --repo-token glpat-xxxx ...
# The token is baked into the rendered user-data; treat that file as a secret and
# delete it after launch. See runbook gardenio-in-a-box-install.md §A.0.
#
# --app-version pins the image tag (Chart.appVersion) the chart renders to a
# KNOWN-PUBLISHED calver, so a fresh box never references an image the registry
# does not have yet (the chart's appVersion can outpace the newest promoted
# image — ka-0000001967). Use the latest promoted non-rc calver that passed E2E;
# see gardenio/docs/gardenio-in-a-box-install.md "Pinning a known-good image tag".
#
# The PUBLIC-SAFE one-shot install (Phase 5 capstone) layers the two in-box
# overlays and enables host hardening:
#   scripts/render-user-data.sh --scenario A --harden 1 \
#     --extra-values "gardenio/helm/gardenio/values-inbox.yaml deploy-values/gardenio-inbox.yaml" \
#     > user-data.yaml
#
# To slack-enable the box reproducibly (Socket Mode, no inbound surface), ALSO
# layer the slack overlay and pass the channel id — the channel is injected via
# deploy-byo --slack-channel so the overlay carries no per-box value, and the
# gardenio-slack-bridge-creds secret must be pre-created on the box (see
# gardenio/docs/inbox-slack-enable-runbook.md):
#   scripts/render-user-data.sh --scenario A --harden 1 \
#     --extra-values "gardenio/helm/gardenio/values-inbox.yaml deploy-values/gardenio-inbox.yaml deploy-values/gardenio-inbox-slack.yaml" \
#     --slack-channel C0XXXXXXXXX \
#     > user-data.yaml
# See gardenio/docs/gardenio-in-a-box-install.md and gardenio/docs/inbox-overlay-layering.md.
#
# Then:
#   aws ec2 run-instances --instance-type <scenario instance> \
#     --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=<gp3>,VolumeType=gp3}' \
#     --user-data file://user-data.yaml ...
set -euo pipefail

SCENARIO="A"
REPO_URL="https://github.com/mattbakerconsulting/gardenio.git"
REPO_REF="main"
# Read-only deploy-token / PAT used to clone the PRIVATE gardenio repo on the box.
# Prefer the GARDENIO_REPO_TOKEN env var over --repo-token so the secret never lands
# in `ps`/shell history. Empty = anonymous clone (public mirror / AMI-baked / air-gap
# repo already staged under /opt/gardenio/repo — see runbook A.0).
REPO_TOKEN="${GARDENIO_REPO_TOKEN:-}"
REPO_TOKEN_USER="oauth2"
NAMESPACE="gardenio"
VALUES_FILE="deploy-values/gardenio-byo.yaml"
EXTRA_VALUES=""
# Per-box Slack channel id (C0…). Set this together with layering
# deploy-values/gardenio-inbox-slack.yaml into --extra-values to slack-enable the
# box; it threads through to deploy-byo --slack-channel so the reusable overlay
# needs no hand-edited channel placeholder. Empty = slack off / overlay default.
SLACK_CHANNEL=""
GVISOR="0"
K3S_VERSION=""
APP_VERSION=""
HARDEN="0"
OPERATOR_USER="gardenio-op"
# Offline-assets dir the tarboat tarball unpacks to. When the tarball stages the
# gardenio subset under <OFFLINE_ASSETS>/repo (Tarboat T1/G1.2, ka-0000008214),
# cloud-init links it to /opt/gardenio/repo and skips the private clone entirely —
# the true air-gap path. Also passed to bootstrap-ec2.sh --offline-assets.
OFFLINE_ASSETS="/opt/gardenio/tarboat"
self_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$self_root/scripts/cloud-config/gardenio-inbox-user-data.yaml.tmpl"

while [ $# -gt 0 ]; do
  case "$1" in
    --scenario) SCENARIO="$2"; shift 2;;
    --repo-url) REPO_URL="$2"; shift 2;;
    --repo-ref) REPO_REF="$2"; shift 2;;
    --repo-token) REPO_TOKEN="$2"; shift 2;;
    --repo-token-user) REPO_TOKEN_USER="$2"; shift 2;;
    --namespace) NAMESPACE="$2"; shift 2;;
    --values) VALUES_FILE="$2"; shift 2;;
    --extra-values) EXTRA_VALUES="$2"; shift 2;;
    --slack-channel) SLACK_CHANNEL="$2"; shift 2;;
    --gvisor) GVISOR="$2"; shift 2;;
    --k3s-version) K3S_VERSION="$2"; shift 2;;
    --app-version) APP_VERSION="$2"; shift 2;;
    --harden) HARDEN="$2"; shift 2;;
    --operator-user) OPERATOR_USER="$2"; shift 2;;
    --offline-assets) OFFLINE_ASSETS="$2"; shift 2;;
    --template) TEMPLATE="$2"; shift 2;;
    -h|--help) sed -n '2,20p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

[ -f "$TEMPLATE" ] || { echo "template not found: $TEMPLATE" >&2; exit 1; }
case "$(printf '%s' "$SCENARIO" | tr '[:lower:]' '[:upper:]')" in
  A|B|C) SCENARIO="$(printf '%s' "$SCENARIO" | tr '[:lower:]' '[:upper:]')";;
  *) echo "invalid --scenario '$SCENARIO' (expected A|B|C)" >&2; exit 2;;
esac

# Use awk for substitution so URL slashes/ampersands don't trip sed.
awk \
  -v scenario="$SCENARIO" -v repo_url="$REPO_URL" -v repo_ref="$REPO_REF" \
  -v repo_token="$REPO_TOKEN" -v repo_token_user="$REPO_TOKEN_USER" \
  -v namespace="$NAMESPACE" -v values_file="$VALUES_FILE" \
  -v extra_values="$EXTRA_VALUES" \
  -v slack_channel="$SLACK_CHANNEL" \
  -v gvisor="$GVISOR" -v k3s_version="$K3S_VERSION" \
  -v app_version="$APP_VERSION" \
  -v harden="$HARDEN" -v operator_user="$OPERATOR_USER" \
  -v offline_assets="$OFFLINE_ASSETS" '
{
  gsub(/@@SCENARIO@@/, scenario)
  gsub(/@@REPO_URL@@/, repo_url)
  gsub(/@@REPO_REF@@/, repo_ref)
  gsub(/@@REPO_TOKEN@@/, repo_token)
  gsub(/@@REPO_TOKEN_USER@@/, repo_token_user)
  gsub(/@@NAMESPACE@@/, namespace)
  gsub(/@@VALUES_FILE@@/, values_file)
  gsub(/@@EXTRA_VALUES@@/, extra_values)
  gsub(/@@SLACK_CHANNEL@@/, slack_channel)
  gsub(/@@GVISOR@@/, gvisor)
  gsub(/@@K3S_VERSION@@/, k3s_version)
  gsub(/@@APP_VERSION@@/, app_version)
  gsub(/@@HARDEN@@/, harden)
  gsub(/@@OPERATOR_USER@@/, operator_user)
  gsub(/@@OFFLINE_ASSETS@@/, offline_assets)
  print
}' "$TEMPLATE"
