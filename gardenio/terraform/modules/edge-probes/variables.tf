variable "alert_email" {
  type        = string
  description = "Address subscribed to the alert topic. Alerts go straight to email, never through Slack (which runs in the cluster this prober checks). Kept out of git: set in the operator's local terraform.tfvars."

  validation {
    condition     = can(regex("^[^@\\s]+@[^@\\s]+$", var.alert_email))
    error_message = "alert_email must be an email address."
  }
}

variable "zone_id" {
  type        = string
  description = "Route53 hosted zone id the prober enumerates (route53:ListResourceRecordSets is scoped to this one zone)."
}

variable "vpc_id" {
  type        = string
  description = "Cluster VPC id whose security groups and instances the prober audits (E7)."
}

variable "schedule_expression" {
  type        = string
  default     = "rate(15 minutes)"
  description = "EventBridge schedule for the prober. The alarms' periods assume 15 minutes."
}

variable "log_retention_days" {
  type        = number
  default     = 30
  description = "CloudWatch Logs retention for the Lambda log group."
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags applied to every resource in the module."
}

variable "uptime_schedule_expression" {
  type        = string
  default     = "rate(1 minute)"
  description = "EventBridge schedule for the availability prober (uptime.tf). The down alarms assume one run a minute."
}
