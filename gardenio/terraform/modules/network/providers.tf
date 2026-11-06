# Required providers for modules/network.
#
# No `provider` block here — provider configuration is the composition's
# responsibility (composition/<env>/providers.tf). Submodules declare
# version constraints only.
#
# No `backend` block in submodules; backends live in composition/ and
# bootstrap/. See canonical-prod-tf-module-architecture.md §2.

terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}
