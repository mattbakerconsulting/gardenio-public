#!/usr/bin/env bash
# publish-microvm-context.sh — S6 (ka-0000008012): publish the Gardenio-owned
# MicroVM agent-context zip to the in-account artifact bucket so terraform's
# awscc_lambda_microvm_image can register the image FROM Gardenio' own account
# (123456789012), not the retired Network-Fabric copy.
#
# WHAT IT DOES
#   1. Pulls the published context artifact image
#        <registry>/gardenio/microvm-agent-context:<calver>
#      and extracts /context/context.zip — the exact build context (Dockerfile +
#      artifacts/) the Lambda MicroVM image build consumes.
#   2. Uploads it to
#        s3://acme-gardenio-<env>-microvm-agent-<acct>-<region>/microvm-images/<calver>/context.zip
#      which is the ONLY prefix the build role can s3:GetObject, and exactly the
#      URI pinned by var.microvm_context_artifact_uri in prod-real.tfvars.
#   3. Prints the s3:// URI to paste into var.microvm_context_artifact_uri (already
#      pinned in prod-real.tfvars for the prod default) and the next apply step.
#
# WHY OPERATOR-GATED
#   The locked-down gardenio-prod-agent SA cannot s3:PutObject to the artifact
#   bucket. Run this from the operator/AKIA path (advice ka-0000007102) whose
#   sts:GetCallerIdentity resolves to acct 123456789012, then run the -targeted
#   terraform apply of awscc_lambda_microvm_image.gardenio_microvm_agent.
#
# USAGE
#   gardenio/scripts/microvm/publish-microvm-context.sh <calver> [--env prod] \
#       [--account 123456789012] [--region us-east-1] [--dry-run]
#
#   <calver>   promoted microvm-agent-context tag to publish, e.g. 2026.200.32
#
# REQUIREMENTS: crane (or skopeo), aws CLI, unzip/tar. Registry auth: either a
#   docker login to registry.gitlab.com, or CRANE auth via the dockerconfigjson
#   PAT in SM gardenio/aws-poc/gitlab-registry-pat (advice ka-0000007102).

set -euo pipefail

REGISTRY="${REGISTRY:-registry.example.com/gardenio}"
ENV_NAME="prod"
ACCOUNT="123456789012"
REGION="us-east-1"
DRY_RUN=0

CALVER="${1:-}"
if [[ -z "$CALVER" || "$CALVER" == -* ]]; then
  echo "ERROR: first argument must be the context calver (e.g. 2026.200.32)" >&2
  echo "usage: $0 <calver> [--env prod] [--account 123456789012] [--region us-east-1] [--dry-run]" >&2
  exit 2
fi
shift

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env)     ENV_NAME="$2"; shift 2 ;;
    --account) ACCOUNT="$2"; shift 2 ;;
    --region)  REGION="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "ERROR: unknown arg: $1" >&2; exit 2 ;;
  esac
done

BUCKET="acme-gardenio-${ENV_NAME}-microvm-agent-${ACCOUNT}-${REGION}"
KEY="microvm-images/${CALVER}/context.zip"
S3_URI="s3://${BUCKET}/${KEY}"
SRC_IMAGE="${REGISTRY}/microvm-agent-context:${CALVER}"

echo "== S6 publish-microvm-context =="
echo "  source image : ${SRC_IMAGE}"
echo "  dest bucket  : ${BUCKET} (acct ${ACCOUNT}, ${REGION})"
echo "  dest key     : ${KEY}"
echo "  s3 uri       : ${S3_URI}"
echo

# --- Identity guard: refuse to write into the wrong account ------------------
CALLER_ACCT="$(aws sts get-caller-identity --query Account --output text 2>/dev/null || echo '?')"
if [[ "$CALLER_ACCT" != "$ACCOUNT" ]]; then
  echo "ERROR: sts:GetCallerIdentity account=${CALLER_ACCT} != target ${ACCOUNT}." >&2
  echo "       Use the operator/AKIA path (advice ka-0000007102) so the caller resolves" >&2
  echo "       to ${ACCOUNT} before publishing. Refusing to write cross-account." >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- Extract context/context.zip from the published artifact image -----------
echo "-- extracting context/context.zip from ${SRC_IMAGE} --"
if command -v crane >/dev/null 2>&1; then
  crane export "$SRC_IMAGE" - | tar -xO context/context.zip > "$WORK/context.zip"
elif command -v skopeo >/dev/null 2>&1; then
  skopeo copy "docker://${SRC_IMAGE}" "dir:${WORK}/oci" >/dev/null
  # skopeo writes raw layers; unpack the single scratch layer to find the file.
  for layer in "$WORK"/oci/*; do
    if tar -xf "$layer" -C "$WORK" context/context.zip 2>/dev/null; then
      break
    fi
  done
  if [[ -f "$WORK/context/context.zip" ]]; then
    mv "$WORK/context/context.zip" "$WORK/context.zip"
  fi
else
  echo "ERROR: need crane or skopeo to extract the artifact image." >&2
  exit 1
fi

if [[ ! -s "$WORK/context.zip" ]]; then
  echo "ERROR: extracted context.zip is empty/missing — wrong tag or artifact layout?" >&2
  exit 1
fi

SHA="$(sha256sum "$WORK/context.zip" | cut -d' ' -f1)"
SIZE="$(stat -c%s "$WORK/context.zip")"
echo "   extracted context.zip: ${SIZE} bytes, sha256:${SHA}"

# Sanity: the zip must carry the Dockerfile the AWS build consumes.
if command -v unzip >/dev/null 2>&1; then
  # Do not use grep -q here: with pipefail, its early exit can SIGPIPE unzip
  # and make a valid archive look like a failed pipeline.
  if ! unzip -Z1 "$WORK/context.zip" | grep -Fx "Dockerfile" >/dev/null; then
    echo "ERROR: context.zip does not contain a Dockerfile — refusing to publish." >&2
    exit 1
  fi
fi

if [[ "$DRY_RUN" == "1" ]]; then
  echo
  echo "[dry-run] would upload: ${S3_URI}"
  echo "[dry-run] set (already pinned in prod-real.tfvars):"
  echo "          microvm_context_artifact_uri = \"${S3_URI}\""
  exit 0
fi

# --- Upload (SSE-S3 AES256, matching the bucket default) ----------------------
echo "-- uploading to ${S3_URI} --"
aws s3 cp "$WORK/context.zip" "$S3_URI" \
  --region "$REGION" \
  --sse AES256 \
  --only-show-errors
echo "   uploaded."

echo
echo "== DONE =="
echo "  Pinned URI (already in prod-real.tfvars): ${S3_URI}"
echo "  Next: register the image with a -targeted apply from composition/canonical-prod:"
echo "    terraform apply -target=awscc_lambda_microvm_image.gardenio_microvm_agent"
echo "  Then read back the assigned version:"
echo "    terraform output -json microvm_plane   # image_registered=true, image_arn=…"
