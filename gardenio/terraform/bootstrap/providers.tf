# Bootstrap composition.
#
# Intentionally NO `backend` block here. The first apply runs against a
# local backend so the state bucket can be created before any remote
# backend exists. After the first apply, the operator runs
# `terraform init -migrate-state -backend-config=...` to move state into
# the bucket this composition just provisioned. See README §
# "Chicken-and-egg".

provider "aws" {
  region = var.region

  default_tags {
    tags = var.common_tags
  }
}
