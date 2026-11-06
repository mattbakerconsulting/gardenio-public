#!/bin/bash
# terraform-providers.sh — pre-cache terraform providers for gardenio agent image.
#
# Downloads the terraform providers used by the site-deployment stack into a
# shared plugin cache directory. This avoids slow provider downloads on every
# `terraform init` / `terragrunt init` at runtime.
#
# Uses TF_PLUGIN_CACHE_DIR (terraform's built-in shared cache). Any subsequent
# `terraform init` that needs these providers will find them already cached,
# regardless of the module path.
#
# Used by the agent Dockerfile.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/versions.env"

CACHE_DIR="${TF_PLUGIN_CACHE_DIR:-/opt/terraform-plugin-cache}"
mkdir -p "$CACHE_DIR"

# Terraform binary must be available (installed by clis.sh).
TERRAFORM="${TERRAFORM_BIN:-terraform}"
if ! command -v "$TERRAFORM" &>/dev/null; then
  echo "ERROR: terraform not found in PATH" >&2
  exit 1
fi

# Create a temporary terraform config that declares the providers used by
# site-deployment. Running `terraform init` with TF_PLUGIN_CACHE_DIR set
# downloads providers into the shared cache.
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

cat > "$WORKDIR/providers.tf" <<EOF
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "${TF_PROVIDER_AWS_VERSION}"
    }
    local = {
      source  = "hashicorp/local"
      version = "${TF_PROVIDER_LOCAL_VERSION}"
    }
    keycloak = {
      source  = "mrparkers/keycloak"
      version = "${TF_PROVIDER_KEYCLOAK_VERSION}"
    }
    random = {
      source  = "hashicorp/random"
      version = "${TF_PROVIDER_RANDOM_VERSION}"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "${TF_PROVIDER_ARCHIVE_VERSION}"
    }
    external = {
      source  = "hashicorp/external"
      version = "${TF_PROVIDER_EXTERNAL_VERSION}"
    }
    postgresql = {
      source  = "cyrilgdn/postgresql"
      version = "${TF_PROVIDER_POSTGRESQL_VERSION}"
    }
  }
}
EOF

echo "Pre-caching terraform providers into ${CACHE_DIR}..."
echo "  hashicorp/aws         ${TF_PROVIDER_AWS_VERSION}"
echo "  hashicorp/local       ${TF_PROVIDER_LOCAL_VERSION}"
echo "  mrparkers/keycloak    ${TF_PROVIDER_KEYCLOAK_VERSION}"
echo "  hashicorp/random      ${TF_PROVIDER_RANDOM_VERSION}"
echo "  hashicorp/archive     ${TF_PROVIDER_ARCHIVE_VERSION}"
echo "  hashicorp/external    ${TF_PROVIDER_EXTERNAL_VERSION}"
echo "  cyrilgdn/postgresql   ${TF_PROVIDER_POSTGRESQL_VERSION}"

cd "$WORKDIR"
TF_PLUGIN_CACHE_DIR="$CACHE_DIR" "$TERRAFORM" init -backend=false -input=false

echo "Provider cache populated:"
find "$CACHE_DIR" -name "terraform-provider-*" -type f | while read -r p; do
  echo "  $(basename "$p") ($(du -h "$p" | cut -f1))"
done

echo "Terraform provider pre-cache complete."
