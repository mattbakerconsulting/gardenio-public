variable "env_name" { type = string }
variable "vpc_id" { type = string }
variable "subnet_ids" { type = list(string) }
variable "server_subnet_ids" {
  type        = list(string)
  default     = null
  description = "Subnets for the server group alone; null uses subnet_ids. Lets the three etcd members span three zones while agents and CI stay in subnet_ids."
}
variable "ci_subnet_ids" {
  type        = list(string)
  default     = []
  description = "Subnets for the CI pool; empty uses subnet_ids. CI keeps public IPs (3.1 TB/day inbound on 2026-10-20 would swamp a NAT box), so these must route to the IGW (cost plan C)."
}
variable "agent_spot_subnet_ids" {
  type        = list(string)
  default     = []
  description = "Subnets for the spot agent pool; when set, the pool launches from agent_private (no public IP) and these must route through the NAT. Empty keeps today's wiring."
}
variable "edge_subnet_id" {
  type        = string
  default     = ""
  description = "Subnet for the public-edge node's ENI; empty uses subnet_ids[0]. Changing it REPLACES the edge ENI, instance and EIP association; the EIP itself is kept."
}
variable "server_public_ip" {
  type        = bool
  default     = true
  description = "Give servers a public IP. Without one the boot script publishes the private IP under the API name (cost plan C3)."
}
variable "agent_public_ip" {
  type        = bool
  default     = true
  description = "Give the agent launch template (the on-demand platform pool) a public IP; the subnets must then route through the NAT (cost plan C3)."
}
variable "vpc_cidr" {
  type        = string
  description = "VPC CIDR; the only source allowed to reach the in-cluster ziti edge router on 3022."
}
variable "zone_id" { type = string }
variable "api_fqdn" {
  type        = string
  description = "DNS name (in zone_id) that carries every server's public IP; join + kubectl endpoint."
}
variable "join_fqdn" {
  type        = string
  description = "DNS name (in zone_id) carrying every server's PRIVATE IP; nodes join through it so the SG self-rule applies."
}
variable "web_ingress_cidrs" {
  type        = list(string)
  default     = ["0.0.0.0/0"]
  description = "CIDRs allowed to reach the cluster ingress on 80/443. Defaults to the internet for module compatibility; narrow it when every real path is an overlay and the ingress only needs to answer in-VPC."
}
variable "hooks_fqdn" {
  type        = string
  default     = ""
  description = "Optional CNAME to api_fqdn for the public webhook host (trigger-engine /webhooks/ only). Empty = no record."
}
variable "admin_cidrs" {
  type        = list(string)
  description = "CIDRs allowed to reach the API (6443). Keep to the operator's IP."
}
variable "api_source_security_group_ids" {
  type        = list(string)
  default     = []
  description = "Security groups allowed to reach the API (6443) as a source. For in-VPC clients whose private IP is not stable; admin_cidrs stays the operator's IP."
}
variable "ecr_cache_repo_arns" {
  type        = list(string)
  default     = []
  description = "ECR pull-through cache repository ARNs the node role may pull and auto-create (e.g. arn:aws:ecr:<region>:<acct>:repository/ghcr/*). Empty disables the grant. Inherited by every pod via IMDS, so keep it to cache prefixes."
}
variable "ecr_read_repo_arns" {
  type        = list(string)
  default     = []
  description = "ECR repository ARNs the node role may PULL from (no create, no import, no push), e.g. arn:aws:ecr:<region>:<acct>:repository/gardenio/*. CI pushes there through GitHub OIDC (plan 2026-10-21-aws-cost-cuts). Inherited by every pod that can reach IMDS, which is why it is pull-only."
}
variable "enable_acme_dns01" {
  type        = bool
  default     = false
  description = "Grant the node role route53:GetChange on change/* so an in-cluster ACME DNS-01 solver (cert-manager with ambient credentials) can confirm challenge propagation. Read-only; the wildcard is unavoidable because GetChange cannot be scoped to a zone."
}
variable "k3s_version" { type = string }
variable "ami_id" { type = string }
variable "server_instance_types" {
  type        = list(string)
  default     = ["t3a.medium", "t3.medium"]
  description = "4 GiB candidates for the k3s servers; the first is the on-demand base (prioritized), spot is capacity-optimized across all. 2 GiB (t3a.small) was too small: k3s-server alone is 0.85-1.2 GiB RSS and the servers also host CoreDNS, traefik, metrics-server, the ARC controller/listeners and cluster-autoscaler, so on 2026-10-14 all three ran out of memory one after another during a server roll (gd-ybTutceJpn). Never go below 4 GiB; changing this edits the ASG launch-template overrides only and does NOT replace running servers (no instance_refresh, by design)."
}
variable "agent_instance_types" {
  type    = list(string)
  default = ["t3a.medium", "t3.medium"]
}
variable "server_count" {
  type    = number
  default = 3 # embedded etcd needs an odd quorum; 3 survives one spot reclaim
}
variable "server_on_demand_base" {
  type        = number
  default     = 1
  description = "How many of server_count run on-demand (the rest spot). 1 keeps one etcd member and the beads Dolt volume off the spot market; two simultaneous spot reclaims then still leave a member that never goes away, and quorum returns as soon as one replacement joins."
}
variable "agent_count" {
  type    = number
  default = 0 # phase 1: servers also run workloads
}
variable "agent_on_demand_base" {
  type        = number
  default     = 1
  description = "Size of the platform pool's fixed on-demand ASG (min = max = desired). Gardeners require an on-demand node (podmanager capacityAffinity), so this is the real ceiling on concurrent gardeners, not agent_count."
}
variable "agent_spot_min" {
  type        = number
  default     = 1
  description = "Floor of the platform pool's cluster-autoscaler-managed spot ASG. 1 keeps one warm node so a crew spawn does not wait ~3 min for a launch; 0 is allowed. The ceiling is agent_count - agent_on_demand_base."
}
variable "agent_root_volume_gb" {
  type        = number
  default     = 60
  description = "Root volume for the general/platform agent pool. Separate from root_volume_gb (servers) because the gardenio agent image is ~2.4 GB compressed / ~7 GB extracted and the 30 GB server default leaves too little room."
}
variable "root_volume_gb" {
  type    = number
  default = 30
}
variable "common_tags" {
  type    = map(string)
  default = {}
}

# ── CI node pool (scale-to-zero, driven by cluster-autoscaler) ──────────
#
# THE LIST IS NOT HOMOGENEOUS AND THE HEAVY TIER FEELS IT. Every type is
# 4 vCPU, but memory splits 16 GiB (m*) vs 32 GiB (r*), and a heavy runner
# requests 1900m/12Gi. So:
#
#   m*.xlarge  ~15.2Gi allocatable -> 12Gi fits ONCE  -> heavy density 1
#   r*.xlarge  ~30.9Gi allocatable -> 12Gi fits TWICE -> heavy density 2
#
# CPU allows 2 on both (2x1900m = 3800m of 4000m). Memory is the only thing
# that decides, so the class the ASG happens to launch silently halves or
# doubles heavy capacity with no config change.
#
# MEASURED 2026-09-22/23, spot price per HEAVY SLOT (7-day mean, best of
# us-east-1a/1b, the two AZs this ASG spans; n=54-61 samples per type and
# worst case within 3% of mean, so the ranking is stable, not a market dip):
#
#   r5.xlarge   $0.0386   r7a.xlarge  $0.0481   r5a.xlarge  $0.0496
#   r6a.xlarge  $0.0579   m5.xlarge   $0.0596   r7i.xlarge  $0.0599
#   m6a.xlarge  $0.0718   m5a.xlarge  $0.0777   r6i.xlarge  $0.0780
#   m7i.xlarge  $0.0819   m7a.xlarge  $0.0881   m6i.xlarge  $0.0950
#
# Nine of the twelve cheapest-per-slot are r-class. But price-capacity-
# optimized weighs interruption risk then price PER INSTANCE -- never
# memory-per-dollar and never pods-per-node. Since no 32 GiB type costs 2x
# its 16 GiB sibling, the per-instance optimum is systematically the
# per-slot pessimum. Measured consequence, 24h to 2026-09-23 00:20Z,
# n=16 launches: 13 m-class (81%) and 3 r-class, and all 10 nodes in the
# fleet at 00:30Z were m-class. The heavy tier therefore runs at density 1
# roughly four launches out of five.
#
# DO NOT USE THE $/SLOT TABLE ABOVE AS A RANKING ON ITS OWN. It prices a
# slot-hour and says nothing about whether the slot OUTLIVES THE JOB, and
# on this pool that is the dominant term. Reclaim census 2026-09-23 ~00:55Z
# (`describe-spot-instance-requests`, code `instance-terminated-no-capacity`,
# exposure-normalised over 35.6 CI instance-hours; cross-checked against 18
# reclaim/replace pairs in 5 h of ASG scaling activities):
#
#   CI pool: 16 reclaims / 35.6 inst-h = 44.9 per 100 inst-h, MTBR 2.23 h.
#   6 of the 16 (38%) landed INSIDE one measured 15.7-minute run (940 s).
#
#   type          $/slot-h  MTBR h   n   P(finish 15.7m)  E[$ per DONE job]
#   r5.xlarge      0.0386    1.49    4        83.9%        0.01204  (1.00x)
#   m5.xlarge      0.0596    6.65    1        96.1%        0.01622  (1.35x)
#   r5a.xlarge     0.0496    0.67    3        67.7%        0.01918  (1.59x)
#   m6a.xlarge     0.0718    1.31    5        81.9%        0.02294  (1.91x)
#   m7a.xlarge     0.0881    3.72    3        93.2%        0.02473  (2.05x)
#   m5a m6i r6a r6i r7a r7i: ZERO exposure in the window -> NO MTBR AT ALL.
#   m7i: 0 reclaims in 3.3 inst-h, which is not an estimate either.
#
# Three inversions the price-only table hides:
#   1. r5a is #3 on $/slot and the THIRD-WORST buy per completed job (1.59x).
#      Its us-east-1b MTBR alone is 0.38 h -- two reclaims at 4.6 and 5.1 min.
#   2. m5 is DENSITY 1 and still beats r5a and m6a per completed job, because
#      it is the best survivor measured (MTBR 6.65 h).
#   3. m6a.xlarge -- override[0], the type CA models and a frequent PCO pick --
#      is the second-worst survivor (MTBR 1.31 h; 0.91 h in us-east-1a) and
#      1.91x the best on $/completed job.
#
# So r5 is the only type that is both cheap per slot AND a decent survivor,
# and gd-Q6iltiM1TO's option 1 ("drop r5a/r5/m5") would delete the single
# best type in the list alongside the worst. Generation, density and SURVIVAL
# are three separable axes; that option collapses all three.
#
# Caveats, because the numbers above are thin: per-type n is 1-5, half the
# list has no exposure at all, P(finish) assumes memoryless reclaims (real
# ones cluster), and 35.6 inst-h is one sample of the spot market. Treat the
# ORDERING as the finding and the absolute dollars as indicative.
# In particular this says NOTHING about r7a/r7i, added below with zero
# survival data -- when they do get picked, measure how long they LIVE, not
# just that PCO chose them.
# And do NOT answer a high reclaim rate by widening this list further; that
# is a separate, already-rejected move.
#
# r7a/r7i are here because they are the ONLY current-generation 32 GiB
# options, i.e. the only types that get heavy density 2 AND dodge the
# generation slowdown measured in gd-Q6iltiM1TO (identical jobs ran ~2.5x
# slower once the fleet drifted to 5th gen). Adding types is deliberately
# the additive half of that bead's option list: it can only widen the spot
# pool, so unlike "drop r5a/r5/m5" it does not trade throughput for
# interruption risk and is not blocked behind the AZ-span work.
#
# What adding them does NOT do: make PCO prefer them. r7a is ~$0.096/instance
# against m5's ~$0.060, so PCO will still usually pick m-class. Forcing
# density 2 needs the 16 GiB types gone (shrinks the pool -> blocked) or a
# heavy-only 32 GiB node group (the structural fix, see gd-iW6lkpTi7W).
# Do not read this list as a density fix; it is a capacity-and-generation
# widening whose density upside is opportunistic.
#
# KEEP THE LIST ORDERED m-FIRST, AND APPEND ONLY. The AWS cluster-autoscaler
# models a mixed-instances ASG from override[0] and nothing else --
# aws_wrapper.go getInstanceTypesForAsgs: `results[name] =
# asg.MixedInstancesPolicy.instanceTypesOverrides[0]`. This ASG's launch
# template deliberately sets no instance_type (verified: DescribeLaunch-
# TemplateVersions returns InstanceType None), so that [0] is the ONLY thing
# describing a CI node to CA.
#
# override[0] = m6a.xlarge, a 16 GiB type, which makes CA's template
# conservative: it plans one heavy pod per new node and is never wrong in the
# direction that strands pods. Promote a 32 GiB type to the front and CA
# would believe every new node seats two heavy pods, then leave the second
# one Pending on every m-class launch -- i.e. 81% of them.
#
# The flip side, worth knowing before anyone tunes ci_max_nodes: because CA
# is pinned to a 16 GiB template, it NEVER plans for density 2. Even when
# r-class nodes are present, the second pod only lands there if the
# scheduler bin-packs it after the fact. r-class density is opportunistic in
# the scheduler and invisible to the autoscaler.
variable "ci_instance_types" {
  type        = list(string)
  default     = ["m6a.xlarge", "m5a.xlarge", "m6i.xlarge", "m5.xlarge", "m7a.xlarge", "m7i.xlarge", "r6a.xlarge", "r6i.xlarge", "r5a.xlarge", "r5.xlarge", "r7a.xlarge", "r7i.xlarge"]
  description = "4 vCPU / 16-32 GiB spot candidates for GitHub Actions runner pods; price-capacity-optimized picks among them. Every type must fit the heavy runner pod request (1900m CPU / 12 Gi -- 3 CPU until the 3000m->1900m A/B): c6a.xlarge (8 GiB) was tried and could not schedule one. The 16 vs 32 GiB split sets heavy density 1 vs 2, and per-type spot survival (MTBR 0.67-6.65 h measured) matters as much as price; see the comment above before changing the list or its order."
}
variable "ci_max_nodes" {
  type    = number
  default = 2
}
variable "ci_root_volume_gb" {
  type    = number
  default = 100 # docker layer cache + Go/Rust build trees live on the node disk
}

# ── Backups written by pods through the node instance role (no OIDC/IRSA on k3s) ─
variable "backup_bucket_arn" {
  type        = string
  default     = ""
  description = "S3 bucket ARN the nodes may write backups to (helm/beads dolt-backup CronJob). Empty = no grant."
}
variable "extra_backup_prefixes" {
  type        = list(string)
  default     = []
  description = "Additional key prefixes under backup_bucket_arn the nodes may list/get/put/delete in, alongside backup_prefix. Additive rather than a list-ified backup_prefix so existing callers are untouched. NOTE this role is inherited by every pod that can reach IMDS, so each prefix added here is readable AND writable by the whole cluster."
}
variable "backup_prefix" {
  type        = string
  default     = "k3s-dolt"
  description = "Key prefix under backup_bucket_arn the nodes may list/put/delete in."
}

variable "edge_node_enabled" {
  type        = bool
  default     = false
  description = "Create the dedicated on-demand edge node (ENI, EIP, own SG) that hosts the public-edge nginx and, optionally, the public ziti router."
}
variable "ziti_public_router_enabled" {
  type        = bool
  default     = false
  description = "World-open 3022 on the edge SG and the ziti_edge_fqdn record, for router-public-1. Requires edge_node_enabled."
  validation {
    condition     = !var.ziti_public_router_enabled || var.edge_node_enabled
    error_message = "ziti_public_router_enabled requires edge_node_enabled: the public router lives on the edge node and its 3022 rule and DNS record hang off that node's SG and EIP."
  }
}
variable "ziti_router_vpc_enabled" {
  type        = bool
  default     = false
  description = "VPC-only 3022 on the nodes SG for router-aws-1."
}
variable "ziti_edge_instance_type" {
  type        = string
  default     = "t3a.small"
  description = "The edge node runs a k3s agent plus one ziti router; 2 GiB is enough headroom for both and the DaemonSets that tolerate every taint."
}
variable "ziti_edge_fqdn" {
  type        = string
  default     = "ziti.clonk.tv"
  description = "A record pointed at the edge node's EIP; router-public-1 advertises this name."
}

variable "public_edge_fqdns" {
  type        = list(string)
  default     = []
  description = "Hostnames served publicly on 443 by the public-edge nginx on the ziti edge node (its own SG and EIP). Non-empty opens 443 to 0.0.0.0/0 on the ziti_edge SG only and adds A records to the EIP."
}
