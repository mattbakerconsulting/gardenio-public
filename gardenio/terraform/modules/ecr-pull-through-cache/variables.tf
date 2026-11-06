# Inputs for modules/ecr-pull-through-cache.

variable "env_name" {
  description = "Short environment identifier (e.g. \"canonical-prod\", \"poc\")."
  type        = string
}

variable "enabled" {
  description = "Master toggle. false (default) provisions nothing — opt in per composition once the upstream credential secrets can be populated out-of-band."
  type        = bool
  default     = false
}

variable "kms_key_arn" {
  description = "Customer-managed KMS key ARN for the upstream credential secrets. Required when enabled and any upstream requires a credential."
  type        = string
  default     = null
}

variable "recovery_window_days" {
  description = "Days before a deleted upstream-credential secret is destroyed (0 disables, otherwise 7-30)."
  type        = number
  default     = 30

  validation {
    condition     = var.recovery_window_days == 0 || (var.recovery_window_days >= 7 && var.recovery_window_days <= 30)
    error_message = "recovery_window_days must be 0 or 7-30."
  }
}

variable "upstreams" {
  description = <<-EOT
    Upstream registries to mirror into ECR via pull-through cache, keyed by a
    short logical name (used in resource + secret names). Each value:
      - ecr_repository_prefix : the ECR prefix the cache populates (e.g. "ghcr",
        "docker-hub"). A node pulls "<account>.dkr.ecr.<region>.amazonaws.com/
        <prefix>/<upstream-path>:<tag>" to fetch-and-cache.
      - upstream_registry_url  : the upstream registry endpoint
        (GHCR = "ghcr.io", Docker Hub = "registry-1.docker.io").
      - requires_credential    : whether AWS requires a credential secret for
        this upstream. Both GHCR and Docker Hub do (true).

    Default mirrors GHCR + Docker Hub — the two registries the ADR's edge matrix
    (P3/P6/P14) flags as the highest-leverage NAT-byte image-pull reductions.
  EOT
  type = map(object({
    ecr_repository_prefix = string
    upstream_registry_url = string
    requires_credential   = optional(bool, true)
    # Override the generated secret suffix. The full name is always
    # "ecr-pullthroughcache/<suffix>" (AWS mandates that prefix); the suffix
    # defaults to "<env_name>-<key>". Set it only to adopt a secret that already
    # exists under a different name -- renaming forces replacement, and a cache
    # rule's credential_arn cannot be repointed without recreating the rule.
    secret_name_suffix = optional(string)
  }))
  default = {
    ghcr = {
      ecr_repository_prefix = "ghcr"
      upstream_registry_url = "ghcr.io"
      requires_credential   = true
    }
    docker-hub = {
      ecr_repository_prefix = "docker-hub"
      upstream_registry_url = "registry-1.docker.io"
      requires_credential   = true
    }
  }
}

variable "common_tags" {
  description = "Tags applied to every taggable resource. Merged with module-derived tags."
  type        = map(string)
  default     = {}
}

variable "untagged_expiry_days" {
  description = "When > 0, register an ECR repository creation template per upstream so every cache repo auto-created by a first pull inherits a lifecycle policy expiring untagged images after this many days. 0 disables. In a pull-through cache an untagged image is a superseded copy (the tag moved upstream), so it is pure storage cost."
  type        = number
  default     = 0

  validation {
    condition     = var.untagged_expiry_days >= 0
    error_message = "untagged_expiry_days must be 0 or positive."
  }
}

variable "create_node_policy" {
  description = "Create the managed IAM policy granting node roles the pull + first-pull-create verbs. Set false when the consuming cluster module already carries those statements inline on its node role, to avoid two overlapping grants."
  type        = bool
  default     = true
}
