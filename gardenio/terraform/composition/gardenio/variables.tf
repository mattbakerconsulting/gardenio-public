variable "region" {
  type    = string
  default = "us-east-1"
}
variable "aws_profile" {
  type    = string
  default = "gardenio"
}
variable "env_name" {
  type    = string
  default = "gardenio"
}
variable "monthly_budget_usd" {
  type = number
  # 100 -> 150 on 2026-09-17 for the HQ host (hq-on-aws.md §4); 150 -> 250 on
  # 2026-09-18 (user-approved) when the gardenio platform pool landed alongside
  # the Gas City k8s dogfood. Revisit in a week.
  default = 250
}
variable "admin_cidrs" {
  type        = list(string)
  description = "Operator CIDRs allowed to reach the k3s API on 6443. Empty (the default) opens no public 6443 rule: kubectl goes through an SSM port-forward instead (scripts/k3s/kubectl-ssm.sh). The live value is set in the untracked terraform.tfvars."
  default     = []
}
variable "k3s_version" {
  type    = string
  default = "v1.36.4+k3s1"
}
variable "hq_instance_type" {
  type        = string
  default     = "m6a.large"
  description = "Gas City HQ host size. Change + apply to resize (terraform stops/starts it); refuse while agent sessions run (scripts/hq status)."
}

# Components of the gardenio platform that ship as our own container images.
# Each becomes its own ECR repository at gardenio/<component>, so the tag
# stays a plain version and each component gets its own lifecycle policy and
# scan results. Adding a component here is the only change needed to publish
# it; third-party images (NATS, Postgres, the VictoriaMetrics stack, Grafana)
# are pulled from their upstream registries and are deliberately absent.
# Every image in .github/images.json must be listed, enabled or not: ECR does
# not create a repo on push here and the CI release role has no
# ecr:CreateRepository, so an image without a repo fails its push (a narrowed
# dispatch still builds a disabled image). scripts/check-image-registry.py
# (ecr_repos) fails CI if the two lists drift. Each also gets a
# gardenio/cache/<component> build-cache repo (ci_ecr.tf). An empty repo costs
# nothing.
variable "gardenio_image_components" {
  type = list(string)
  default = [
    "kbeads", "controller", "agent", "coop-claude", "coopmux", "beads-viewer", "slack-bridge", "trigger-engine", "k3s-audit-shipper", "grafana-bridge", "ziti-ctrl-backup",
    "landgate",
    "landing", # the clonk.tv front door (gd-gQ8hZLd4AQ)
    # "enabled": false in images.json (gd-OQSDvm7cHz)
    "alertmanager-bridge", "bridge-log-shipper", "claudemore", "edge-auth", "gitlab-bridge", "idp-log-shipper", "inviteservice", "jira-bridge", "push-bridge",
    "roles-server", "scif-portal", "traefik-log-shipper", "wl-bridge", "ava-log-shipper", "core-log-shipper", "github-bridge", "repro-cockpit",
  ]
  description = "Component names published as gardenio/<component> ECR repositories."
}


variable "edge_probe_alert_email" {
  type        = string
  description = "Email subscribed to the edge-probe SNS topic. No default: set it in the operator's local terraform.tfvars (untracked), never in git."
}

variable "lands_support_forward_to" {
  type        = string
  default     = ""
  description = "Mailbox that mail to support@lands.clonk.tv is forwarded to. Empty uses edge_probe_alert_email. Set it in the operator's local terraform.tfvars (untracked), never in git."
}

variable "ses_lands_dmarc_rua" {
  type        = string
  default     = ""
  description = "Mailbox for lands.clonk.tv DMARC aggregate reports. Empty publishes DMARC without rua. Set it in the operator's local terraform.tfvars (untracked), never in git."
}
