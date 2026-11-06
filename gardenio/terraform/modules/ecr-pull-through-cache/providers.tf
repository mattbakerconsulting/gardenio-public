# Required providers for modules/ecr-pull-through-cache.
#
# No `provider` block here — provider configuration is the composition's
# responsibility (composition/<env>/providers.tf). Submodules declare
# version constraints only. No `backend` block in submodules.

terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}
