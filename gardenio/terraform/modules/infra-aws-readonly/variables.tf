variable "env_name" {
  type        = string
  description = "Environment name; prefixes every resource (gardenio)."
}

variable "subjects" {
  type        = list(string)
  default     = ["gardenio/gardenio-infra-reader"]
  description = <<-EOT
    The in-cluster identities that get read-only AWS access, as "<namespace>/<serviceaccount>".
    One IAM user is created per entry, named "<env_name>-<serviceaccount>-aws", so each
    identity has its own key that can be rotated or deactivated alone. A static key is not
    bound to the ServiceAccount by AWS: the binding is that only the role bead whose pods run
    as that ServiceAccount lists the key's Secret (see README). P6 later appends
    "gardenio/gardenio-infra-admin".
  EOT

  validation {
    condition     = length(var.subjects) > 0 && alltrue([for s in var.subjects : can(regex("^[a-z0-9-]+/[a-z0-9-]+$", s))])
    error_message = "subjects must be a non-empty list of \"<namespace>/<serviceaccount>\" strings (lowercase, digits, hyphens)."
  }

  validation {
    condition     = length(distinct([for s in var.subjects : try(split("/", s)[1], s)])) == length(var.subjects)
    error_message = "ServiceAccount names must be unique across subjects (they name the IAM users)."
  }
}

variable "readable_log_groups" {
  type = list(string)
  default = [
    "/aws/lambda/gardenio-edge-probe",
    "/aws/lambda/gardenio-uptime-probe",
    "/aws/lambda/gardenio-support-forward",
  ]
  description = <<-EOT
    CloudWatch Logs groups whose content (GetLogEvents, FilterLogEvents) the subjects may
    read. Log content is denied on every other group. Name only groups whose lines carry no
    secret: the support forwarder logs a disposition list, never a message.
  EOT

  validation {
    condition     = alltrue([for g in var.readable_log_groups : can(regex("^/[A-Za-z0-9_./#-]+$", g)) && !strcontains(g, "*")])
    error_message = "readable_log_groups must be exact log group names (no wildcards)."
  }
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags applied to every resource in the module."
}
