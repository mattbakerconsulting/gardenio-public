variable "domain" {
  type        = string
  description = "The SES domain that receives mail (lands.clonk.tv). Its identity must be verified (module ses-identity) and it must sit in the zone named by zone_id."

  validation {
    condition     = can(regex("^([a-z0-9-]+\\.)+[a-z]{2,}$", var.domain))
    error_message = "domain must be a lowercase DNS name such as lands.clonk.tv."
  }
}

variable "zone_id" {
  type        = string
  description = "Route 53 public hosted zone that holds the domain's MX record."
}

variable "identity_arn" {
  type        = string
  description = "ARN of the SES domain identity the forwarder sends as (module.ses_lands.identity_arn)."
}

variable "recipient_local_part" {
  type        = string
  default     = "support"
  description = "Local part of the one address received: <recipient_local_part>@<domain>."

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9._-]*$", var.recipient_local_part))
    error_message = "recipient_local_part must be a plain lowercase local part such as support."
  }
}

variable "forward_to" {
  type        = string
  description = "The operator's mailbox the mail is forwarded to. Set it in the untracked terraform.tfvars, never in git. While SES is in the sandbox it must be a verified identity."

  validation {
    condition     = can(regex("^[^@\\s]+@[^@\\s]+$", var.forward_to))
    error_message = "forward_to must be a single email address."
  }
}

variable "retention_days" {
  type        = number
  default     = 30
  description = "Days the raw received mail is kept in S3 (the Privacy Policy covers mail sent to support@)."

  validation {
    condition     = var.retention_days >= 1 && var.retention_days <= 365
    error_message = "retention_days must be between 1 and 365."
  }
}

variable "alarm_topic_arn" {
  type        = string
  default     = ""
  description = "SNS topic the forwarder's error alarm notifies (module.edge_probes.topic_arn). Empty: no alarm."
}

variable "rule_set_name" {
  type        = string
  default     = "gardenio-inbound"
  description = "Name of the receipt rule set. SES has ONE active rule set per region, and applying this makes it the active one."
}

variable "log_retention_days" {
  type    = number
  default = 30
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags applied to every taggable resource in the module."
}
