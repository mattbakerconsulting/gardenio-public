variable "domain" {
  type        = string
  description = "The SES sending domain (lands.clonk.tv). Must sit inside the zone named by zone_id."

  validation {
    condition     = can(regex("^([a-z0-9-]+\\.)+[a-z]{2,}$", var.domain))
    error_message = "domain must be a lowercase DNS name such as lands.clonk.tv."
  }
}

variable "zone_id" {
  type        = string
  description = "Route 53 public hosted zone that holds the DKIM, MAIL FROM and DMARC records."
}

variable "mail_from_subdomain" {
  type        = string
  default     = "bounce"
  description = "Label of the custom MAIL FROM domain, giving <mail_from_subdomain>.<domain>. It must not be a name that already receives mail."
}

variable "dmarc_policy" {
  type        = string
  default     = "quarantine"
  description = "DMARC p= value."

  validation {
    condition     = contains(["none", "quarantine", "reject"], var.dmarc_policy)
    error_message = "dmarc_policy must be none, quarantine or reject."
  }
}

variable "dmarc_rua" {
  type        = string
  default     = ""
  description = "Mailbox for DMARC aggregate reports (rua=mailto:...). Empty omits rua, so no reports are sent. The owner picks this mailbox (gd-o4fbw9qUJc owner item b); set it in the untracked terraform.tfvars, not in git."
}

variable "smtp_user_name" {
  type        = string
  description = "Name of the IAM user whose access key becomes the SES SMTP credential."
}

variable "smtp_from_address" {
  type        = string
  description = "The ONLY From address the SMTP user may send as. Must be on domain."

  validation {
    condition     = can(regex("^[^@\\s]+@[^@\\s]+$", var.smtp_from_address))
    error_message = "smtp_from_address must be a single email address."
  }
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags applied to every taggable resource in the module."
}
