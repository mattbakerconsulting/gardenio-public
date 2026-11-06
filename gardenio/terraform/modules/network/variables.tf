# Inputs for modules/network.
#
# Variable names track `docs/research/canonical-prod-module-io-contract.md`
# §3.2 (networking). Types are concrete; resource bodies are deferred.

variable "env_name" {
  description = "Short environment identifier (e.g. \"canonical-prod\", \"poc\")."
  type        = string
}

variable "region" {
  description = "AWS region. Single-region module today."
  type        = string
}

variable "vpc_cidr" {
  description = "Top-level CIDR for the VPC. Subnets carved by this module."
  type        = string

  validation {
    condition = can(cidrhost(var.vpc_cidr, 0)) && (
      tonumber(split("/", var.vpc_cidr)[1]) >= 16 &&
      tonumber(split("/", var.vpc_cidr)[1]) <= 20
    )
    error_message = "vpc_cidr must be a valid IPv4 CIDR with mask /16-/20."
  }
}

variable "additional_cidr_blocks" {
  description = "Optional additional CIDR blocks attached to the VPC (e.g. 100.64.0.0/16 carrier-grade NAT)."
  type        = list(string)
  default     = []
}

variable "az_count" {
  description = "Number of AZs to span (2-6)."
  type        = number
  default     = 3

  validation {
    condition     = var.az_count >= 2 && var.az_count <= 6
    error_message = "az_count must be between 2 and 6."
  }
}

variable "availability_zones" {
  description = "Explicit AZ list. Empty = derive first az_count from data.aws_availability_zones."
  type        = list(string)
  default     = []
}

variable "nat_mode" {
  description = <<-EOT
    Egress topology for the private-app tier:
      single   — one managed NAT Gateway (1 EIP), all AZs route to it.
      per-az   — one managed NAT Gateway per AZ (az_count EIPs), HA.
      instance — fck-nat / NAT instance(s) on t4g.nano with an auto-assigned
                 public IPv4 (0 EIP). See nat_instance.tf + nat_instance_per_az.
                 Recommended substrate per docs/research/canonical-prod-nat-elimination.md.
      none     — no default route; private-app has no internet egress (breaks the
                 ~78% IPv4-only public-HTTPS workload — use only with a full
                 endpoint/proxy alternative path).
  EOT
  type        = string
  default     = "single"

  validation {
    condition     = contains(["single", "per-az", "instance", "none"], var.nat_mode)
    error_message = "nat_mode must be one of: single, per-az, instance, none."
  }
}

variable "nat_instance_per_az" {
  description = "When nat_mode=instance: false (default) = one shared NAT box (cost-parity with single); true = one box per AZ (per-AZ fault isolation). Either way 0 EIPs (auto-assigned public IPv4)."
  type        = bool
  default     = false
}

variable "nat_instance_type" {
  description = <<-EOT
    Instance type for the NAT box (nat_mode=instance). t4g.nano (~$3.07/mo
    on-demand, ~32 Mbps sustained baseline, ~5 Gbps burst) suits light egress
    (~600 GB/mo of trickle traffic), BUT a burstable box drains its NETWORK
    burst-credit budget under sustained bulk egress and then wedges ALL egress at
    baseline for hours — the 2026-07-12 outage failure mode (ka-0000007968). Size
    UP (t4g.small/medium/large raise the baseline 4x/8x/16x) for any workload with
    bulk NAT-bound pulls, or move to a fixed-throughput family for a hard
    no-burst-cliff guarantee.
  EOT
  type        = string
  default     = "t4g.nano"
}

variable "nat_instance_arch" {
  description = "CPU arch for the NAT-instance AL2023 AMI SSM lookup: arm64 (t4g, default) or x86_64 (t3)."
  type        = string
  default     = "arm64"

  validation {
    condition     = contains(["arm64", "x86_64"], var.nat_instance_arch)
    error_message = "nat_instance_arch must be arm64 or x86_64."
  }
}

variable "nat_instance_use_spot" {
  description = "When nat_mode=instance: run the NAT box(es) on Spot capacity. Safe because the ASG-of-1 + route-reclaim self-heal treats a Spot interruption as just another relaunch. Default false (on-demand) for steady egress; flip on to shave the already-tiny instance cost further."
  type        = bool
  default     = false
}

variable "nat_instance_cpu_credits" {
  description = <<-EOT
    CPU-credit mode for a burstable (T-family) NAT box: "unlimited" (default) or
    "standard". Only emitted onto the launch template when nat_instance_type is a
    T family; ignored for fixed-throughput families (m/c/…) which have no credit
    concept. "unlimited" removes the CPU-credit cliff — the box never throttles to
    baseline CPU when packet-forwarding load spikes, at the cost of a small
    per-vCPU-hour surcharge only while sustained above baseline (near-zero for a
    NAT box). This is the CPU half of the burst-credit hardening after the
    2026-07-12 egress-saturation outage (ka-0000007968). NOTE: T-instance NETWORK
    burst credits are a SEPARATE, non-configurable budget — they cannot be set to
    unlimited, so bulk-egress hardening comes from a larger nat_instance_type
    (higher sustained baseline bandwidth), not from this knob.
  EOT
  type        = string
  default     = "unlimited"

  validation {
    condition     = contains(["standard", "unlimited"], var.nat_instance_cpu_credits)
    error_message = "nat_instance_cpu_credits must be standard or unlimited."
  }
}

variable "nat_instance_alarm_sns_topic_arns" {
  description = "SNS topic ARNs notified when a NAT-instance ASG has no in-service box (egress-down). Empty = alarm with no action (still visible in CloudWatch)."
  type        = list(string)
  default     = []
}

variable "vpc_endpoint_services" {
  description = <<-EOT
    Set of AWS services to expose via VPC endpoints. "s3"/"dynamodb" become free
    Gateway endpoints (0 EIP, 0 hourly); everything else becomes an Interface
    endpoint (~$7.30/mo per service per AZ). Widened default (ADR
    docs/research/canonical-prod-nat-elimination.md §3a) takes the AWS control-plane
    data path (IRSA/STS, Secrets Manager, ECR, KMS, Logs, EKS) off the public-egress
    chokepoint, so a NAT/instance outage no longer also kills IRSA + secret fetch +
    image pulls. The only gardenio AWS dep with NO PrivateLink is Route53 (stays public).
    Trim to a leaner subset + fewer AZs in cost-pressed compositions (see POC).
  EOT
  type        = set(string)
  default = [
    "s3",
    "ec2",
    "ec2messages",
    "ssm",
    "ssmmessages",
    "ecr.api",
    "ecr.dkr",
    "sts",
    "secretsmanager",
    "logs",
    "kms",
    "eks",
    "autoscaling",
    "sqs",
    "elasticloadbalancing",
  ]
}

variable "enable_private_dns_for_endpoints" {
  description = "Enable private DNS for VPC interface endpoints."
  type        = bool
  default     = true
}

variable "common_tags" {
  description = "Tags applied to every taggable resource. Merged with module-derived tags."
  type        = map(string)
  default     = {}
}

variable "extra_public_subnets" {
  description = "Extra public subnets outside the per-AZ set, keyed by availability zone, each the cidrsubnet(vpc_cidr, 4, N) index it takes. They join the public route table only, so the counted subnets' CIDRs never shift."
  type        = map(number)
  default     = {}
}

variable "edge_subnets" {
  description = "AZ -> CIDR of small public subnets (IGW route) for what must keep a public IP: the NAT box, the public-edge node, the CI pool (cost plan 2026-10-21 C2). The NAT box is placed here when set; with nat_instance_per_az every NAT AZ needs one."
  type        = map(string)
  default     = {}
}

variable "nat_routed_public_azs" {
  description = "AZs whose PUBLIC subnet (public or public_extra) is re-associated to a private-app route table, i.e. egresses through the NAT, and stops handing out public IPs on launch. Flip one AZ at a time. An AZ with no private-app table of its own (an extra_public_subnets AZ) uses private_app[0]."
  type        = set(string)
  default     = []
}
