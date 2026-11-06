# k3s on spot: 3 servers with embedded etcd in 2 AZs, relaunched by an ASG on
# reclaim, joined through a Route53 name the nodes keep up to date themselves.
# No NAT, no ELB, no EKS -- this is the ~$25/month cluster.

locals {
  name = "${var.env_name}-k3s"
  tags = merge(var.common_tags, { "gardenio:component" = "k3s" })
}

# ── Cluster secret ──────────────────────────────────────────────────────
resource "random_password" "token" {
  length  = 48
  special = false
}

resource "aws_ssm_parameter" "token" {
  name  = "/${var.env_name}/k3s/token"
  type  = "SecureString"
  value = random_password.token.result
  tags  = local.tags
}

# Written by the first server once; read by the laptop. Content managed by
# user-data, so ignore it here.
resource "aws_ssm_parameter" "kubeconfig" {
  name  = "/${var.env_name}/k3s/kubeconfig"
  type  = "SecureString"
  value = "pending"
  tags  = local.tags
  lifecycle { ignore_changes = [value] }
}

# ── Join / API endpoint ─────────────────────────────────────────────────
# Terraform owns the record's existence; the servers own its content (each
# adds its public IP on boot and removes it on spot notice). Failure mode
# accepted: a reclaimed server that never got the 2-minute notice leaves a
# stale IP for up to TTL; Go's dialer tries every A record, so joins and
# kubectl still succeed via the live ones.
resource "aws_route53_record" "join" {
  zone_id = var.zone_id
  name    = var.join_fqdn
  type    = "A"
  ttl     = 30
  records = ["192.0.2.2"]
  lifecycle { ignore_changes = [records, ttl] }
}

resource "aws_route53_record" "api" {
  zone_id = var.zone_id
  name    = var.api_fqdn
  type    = "A"
  ttl     = 30
  records = ["192.0.2.1"] # RFC 5737 placeholder until the first server boots
  lifecycle { ignore_changes = [records, ttl] }
}

# Public webhook host (decision gd-Bz9rkZg5Q0). A CNAME to api_fqdn so it
# follows the servers' public IPs, which the servers keep current themselves
# (no EIPs; see "api" above). Traefik serves only Host(hooks) &&
# PathPrefix(/webhooks/) on it; reachability is limited by web_ingress_cidrs.
resource "aws_route53_record" "hooks" {
  count   = var.hooks_fqdn == "" ? 0 : 1
  zone_id = var.zone_id
  name    = var.hooks_fqdn
  type    = "CNAME"
  ttl     = 300
  records = [var.api_fqdn]
}

# ── Security group ──────────────────────────────────────────────────────
resource "aws_security_group" "nodes" {
  name        = local.name
  description = "k3s nodes"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { Name = local.name })
}

resource "aws_vpc_security_group_ingress_rule" "self" {
  security_group_id            = aws_security_group.nodes.id
  referenced_security_group_id = aws_security_group.nodes.id
  ip_protocol                  = "-1"
  description                  = "intra-cluster (etcd, kubelet, flannel)"
}

resource "aws_vpc_security_group_ingress_rule" "api" {
  for_each          = toset(var.admin_cidrs)
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = each.value
  from_port         = 6443
  to_port           = 6443
  ip_protocol       = "tcp"
  description       = "kube-apiserver from operator"
}

# The HQ host runs `gc` with the k8s session runtime, which drives the cluster
# through client-go: pod create/exec/delete against the API, not through the
# ziti overlay. Its SG has no ingress of its own, so it is referenced as a
# source here rather than by CIDR -- the host's private IP changes across
# stop/start, and admin_cidrs is documented as the operator's IP alone.
resource "aws_vpc_security_group_ingress_rule" "api_from_sg" {
  for_each                     = toset(var.api_source_security_group_ids)
  security_group_id            = aws_security_group.nodes.id
  referenced_security_group_id = each.value
  from_port                    = 6443
  to_port                      = 6443
  ip_protocol                  = "tcp"
  description                  = "kube-apiserver from in-VPC client"
}

# Cluster ingress. NOT 0.0.0.0/0 here: Traefik answers on every node's PUBLIC
# IP, so a world-open :443 means any IngressRoute is served to the internet the
# moment it exists -- no DNS record and no further SG change required, because
# a caller only has to set the Host header. That is the wrong default when the
# intended path is the ziti overlay and one of the services behind Traefik
# (viewer-authproxy) has no authentication of its own.
resource "aws_vpc_security_group_ingress_rule" "web" {
  for_each = {
    for pair in setproduct(keys({ http = 80, https = 443 }), var.web_ingress_cidrs) :
    "${pair[0]}-${pair[1]}" => { port = { http = 80, https = 443 }[pair[0]], cidr = pair[1], name = pair[0] }
  }
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = each.value.cidr
  from_port         = each.value.port
  to_port           = each.value.port
  ip_protocol       = "tcp"
  description       = "ingress ${each.value.name} from ${each.value.cidr}"
}

# The in-cluster OpenZiti edge router (`router-aws-1`, helm/ziti) runs with
# hostNetwork on the on-demand server and listens on 3022. It is dialled by
# the HQ host's tunneler and by pods in this VPC only; the Vultr side never
# dials in (the router config is `--private`, so it has no link listener and
# dials out to the public router instead). Deliberately no public ingress.
resource "aws_vpc_security_group_ingress_rule" "ziti_router" {
  count             = var.ziti_router_vpc_enabled ? 1 : 0
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = var.vpc_cidr
  from_port         = 3022
  to_port           = 3022
  ip_protocol       = "tcp"
  description       = "ziti edge router (router-aws-1) from the VPC only"
}

# The rule predates the flag, so it was unconditional and is live; the moved
# block keeps it from being replaced when it gains a count index.
moved {
  from = aws_vpc_security_group_ingress_rule.ziti_router
  to   = aws_vpc_security_group_ingress_rule.ziti_router[0]
}

# The public edge router (`router-public-1`, helm/ziti) is NOT covered by the
# shared nodes SG: its world-open 3022 lives on its own SG, attached only to
# the dedicated edge node below (operator ruling gd-II5iO7Nbbf, separate-sg).
# SG rules are a union across every SG on an ENI, so a 0.0.0.0/0 rule here
# would also world-open router-aws-1 and every other node.

resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

# ── Instance role: SSM Session Manager, the two parameters, the DNS record ─
data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name               = local.name
  assume_role_policy = data.aws_iam_policy_document.assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

data "aws_iam_policy_document" "node" {
  statement {
    sid       = "ClusterParams"
    actions   = ["ssm:GetParameter", "ssm:PutParameter"]
    resources = [aws_ssm_parameter.token.arn, aws_ssm_parameter.kubeconfig.arn, "arn:aws:ssm:*:*:parameter/${var.env_name}/k3s/*"]
  }
  statement {
    sid       = "JoinRecord"
    actions   = ["route53:ChangeResourceRecordSets", "route53:ListResourceRecordSets"]
    resources = ["arn:aws:route53:::hostedzone/${var.zone_id}"]
  }

  # ACME DNS-01 (cert-manager with --issuer-ambient-credentials, so the solver
  # picks this role up over IMDS rather than a credential in a Secret). Writing
  # the _acme-challenge TXT record is already covered by JoinRecord above; what
  # is NOT is GetChange, which the solver polls to confirm propagation.
  #
  # The wildcard is forced by AWS, not chosen: GetChange takes a `change/*`
  # resource, a different type from `hostedzone/*`, and change ids are opaque
  # and allocated per request, so there is no zone-scoped form of this grant.
  # It is read-only and tells the caller only whether a change has propagated;
  # ids cannot be enumerated, so holding it without already holding an id
  # reveals nothing. Strictly less powerful than the record WRITE this role
  # already has on the zone.
  dynamic "statement" {
    for_each = var.enable_acme_dns01 ? [1] : []
    content {
      sid       = "AcmeDns01GetChange"
      actions   = ["route53:GetChange"]
      resources = ["arn:aws:route53:::change/*"]
    }
  }
  statement {
    sid       = "Peers"
    actions   = ["ec2:DescribeInstances", "autoscaling:DescribeAutoScalingGroups"]
    resources = ["*"]
  }
  # cluster-autoscaler runs on the servers with this instance role (IMDS hop
  # limit 2 lets the pod reach it). Scale actions are limited to the two ASGs
  # it owns: CI and the platform pool's spot half. The on-demand half is not
  # listed on purpose; nothing automated resizes it.
  statement {
    sid = "AutoscalerRead"
    actions = [
      "autoscaling:DescribeAutoScalingInstances", "autoscaling:DescribeLaunchConfigurations",
      "autoscaling:DescribeScalingActivities", "autoscaling:DescribeTags",
      "ec2:DescribeInstanceTypes", "ec2:DescribeLaunchTemplateVersions", "ec2:DescribeImages",
      "ec2:GetInstanceTypesFromInstanceRequirements",
    ]
    resources = ["*"]
  }
  # By ARN pattern, not aws_autoscaling_group.*.arn: an ASG depends on its
  # launch template, so a reference here put both templates in the closure of
  # -target=module.k3s.aws_iam_role_policy.node and a targeted policy apply
  # rolled the SSM AMI drift onto them (gd-VAyugfDaI5). The * is the ASG's
  # generated id; the name is unique per account and region.
  statement {
    sid     = "AutoscalerScale"
    actions = ["autoscaling:SetDesiredCapacity", "autoscaling:TerminateInstanceInAutoScalingGroup"]
    resources = [
      for asg in ["${local.name}-ci", "${local.name}-agents-spot"] :
      "arn:aws:autoscaling:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:autoScalingGroup:*:autoScalingGroupName/${asg}"
    ]
  }
  # The beads Dolt backup CronJob (helm/beads) runs `aws s3 sync --delete`
  # from a pod; k3s has no OIDC provider so the pod inherits this role via
  # IMDS (hop limit 2). Scoped to one prefix; delete is a versioned delete
  # marker in that bucket, no DeleteObjectVersion.
  dynamic "statement" {
    for_each = var.backup_bucket_arn == "" ? [] : [1]
    content {
      sid       = "BackupBucketList"
      actions   = ["s3:ListBucket"]
      resources = [var.backup_bucket_arn]
      condition {
        test     = "StringLike"
        variable = "s3:prefix"
        values = flatten([
          for p in concat([var.backup_prefix], var.extra_backup_prefixes) :
          ["${p}/*", p, "${p}/"]
        ])
      }
    }
  }
  dynamic "statement" {
    for_each = var.backup_bucket_arn == "" ? [] : [1]
    content {
      sid = "BackupObjects"
      # GetObjectVersion, not just GetObject: reading a PINNED version
      # (`aws s3api get-object --version-id`) is a different action and
      # GetObject does not imply it. The pinning is deliberate — the ziti
      # controller restores from S3 on every start, so "whatever is newest"
      # would turn a write to this prefix into a boot. ListBucketVersions is
      # NOT granted: the restore is handed the version id, it never enumerates.
      actions = ["s3:PutObject", "s3:GetObject", "s3:GetObjectVersion", "s3:DeleteObject", "s3:AbortMultipartUpload"]
      resources = [
        for p in concat([var.backup_prefix], var.extra_backup_prefixes) :
        "${var.backup_bucket_arn}/${p}/*"
      ]
    }
  }

  # ECR pull-through cache. k3s ships no AWS credential provider and this
  # cluster has no OIDC/IRSA, so the kubelet pulls with the NODE role via IMDS
  # -- which means every pod that can reach IMDS inherits these verbs too.
  # That is why this is opt-in and scoped to the cache repo prefixes rather
  # than granted account-wide. The six actions mirror
  # modules/ecr-pull-through-cache/main.tf exactly: the first pull of an image
  # has to CreateRepository and BatchImportUpstreamImage, so the read-only
  # managed policy is not sufficient. GetAuthorizationToken has no resource
  # scoping available and must be "*".
  dynamic "statement" {
    for_each = length(var.ecr_cache_repo_arns) + length(var.ecr_read_repo_arns) == 0 ? [] : [1]
    content {
      sid       = "EcrAuth"
      actions   = ["ecr:GetAuthorizationToken"]
      resources = ["*"]
    }
  }
  dynamic "statement" {
    for_each = length(var.ecr_cache_repo_arns) == 0 ? [] : [1]
    content {
      sid = "EcrPullThroughCache"
      actions = [
        "ecr:GetDownloadUrlForLayer",
        "ecr:BatchGetImage",
        "ecr:BatchCheckLayerAvailability",
        "ecr:BatchImportUpstreamImage",
        "ecr:CreateRepository",
        "ecr:TagResource",
      ]
      resources = var.ecr_cache_repo_arns
    }
  }
  # Images CI pushes to ECR itself (gardenio/*, plan 2026-10-21-aws-cost-cuts
  # Task A2). Pull-only: the same IMDS inheritance applies, and nothing in the
  # cluster has any business creating, importing or pushing there.
  dynamic "statement" {
    for_each = length(var.ecr_read_repo_arns) == 0 ? [] : [1]
    content {
      sid = "EcrReadGardenio"
      actions = [
        "ecr:GetDownloadUrlForLayer",
        "ecr:BatchGetImage",
        "ecr:BatchCheckLayerAvailability",
      ]
      resources = var.ecr_read_repo_arns
    }
  }
}

resource "aws_iam_role_policy" "node" {
  name   = "${local.name}-node"
  role   = aws_iam_role.node.id
  policy = data.aws_iam_policy_document.node.json
}

resource "aws_iam_instance_profile" "node" {
  name = local.name
  role = aws_iam_role.node.name
  tags = local.tags
}

# ── Launch templates ────────────────────────────────────────────────────
locals {
  userdata_vars = {
    env_name    = var.env_name
    k3s_version = var.k3s_version
    api_fqdn    = var.api_fqdn
    join_fqdn   = var.join_fqdn
    zone_id     = var.zone_id
    token_param = aws_ssm_parameter.token.name
    kube_param  = aws_ssm_parameter.kubeconfig.name
    init_param  = "/${var.env_name}/k3s/initialized"
    extra_args  = ""
  }
}

# user_data is gzipped: with the audit policy the server script is ~18 KB raw,
# over the 16 KB EC2 limit. cloud-init unzips it transparently (hq does the same).
resource "aws_launch_template" "server" {
  name_prefix   = "${local.name}-server-"
  image_id      = var.ami_id
  user_data     = base64gzip(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "server" })))
  ebs_optimized = true
  iam_instance_profile { name = aws_iam_instance_profile.node.name }
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2 # hostNetwork pods / NTH may read IMDS
  }
  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size           = var.root_volume_gb
      volume_type           = "gp3"
      delete_on_termination = true
      encrypted             = true
    }
  }
  network_interfaces {
    associate_public_ip_address = var.server_public_ip
    delete_on_termination       = true
    security_groups             = [aws_security_group.nodes.id]
  }
  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.tags, { Name = "${local.name}-server", "k3s:role" = "server" })
  }
  tag_specifications {
    resource_type = "volume"
    tags          = local.tags
  }
  tags = local.tags
}

# The general (non-CI) pool. It carries the gardenio platform: the chart's
# controller/kbeads/beads-viewer/coopmux plus the agent pods the controller
# spawns. Those spawned pods already tolerate gardenio.agent=true:NoSchedule
# (podmanager/defaults.go), so tainting here keeps everything else off the
# pool without any extra configuration on the agent side.
locals {
  # kubelet memory reserve (gd-sDztRMaAag, 2026-10-25): without it Allocatable
  # == Capacity and the pods cgroup can take every page; k3s has no memory
  # eviction path (see node.sh.tftpl), so the kernel OOM-killer ran on the
  # whole node and ip-10-90-2-170 stopped heartbeating. With the reserve the
  # kubepods cgroup is capped at Capacity minus 640Mi and the OOM lands inside
  # it. CPU is NOT reserved: ip-10-90-30-213's platform pods already request
  # 1920m of 2000m, so a CPU reserve would leave a restarted postgres or
  # kbeads (node-local PVs) unschedulable. Comma-separated values pass
  # through k3s as one token, like eviction-hard. Live pair updated by hand
  # (k3s-agent unit) the same day; new nodes get it from user_data.
  gardenio_extra_args = "--node-label gardenio.io/role=platform --node-taint gardenio.agent=true:NoSchedule --kubelet-arg=kube-reserved=memory=384Mi --kubelet-arg=system-reserved=memory=256Mi"
  # gzipped like the server's (cloud-init unzips): plain base64 was within
  # ~800 bytes of the 16 KB EC2 limit (cost plan C3).
  agent_user_data = base64gzip(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "agent", extra_args = local.gardenio_extra_args })))
}

resource "aws_launch_template" "agent" {
  name_prefix   = "${local.name}-agent-"
  image_id      = var.ami_id
  user_data     = local.agent_user_data
  ebs_optimized = true
  iam_instance_profile { name = aws_iam_instance_profile.node.name }
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }
  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size           = var.agent_root_volume_gb
      volume_type           = "gp3"
      delete_on_termination = true
      encrypted             = true
    }
  }
  network_interfaces {
    associate_public_ip_address = var.agent_public_ip
    delete_on_termination       = true
    security_groups             = [aws_security_group.nodes.id]
  }
  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.tags, { Name = "${local.name}-agent", "k3s:role" = "agent" })
  }
  tags = local.tags
}

# The spot pool's template once agent_spot_subnet_ids moves it to private
# subnets (cost plan C3): the same node, never a public IP. A separate
# template so the on-demand half keeps its own until it moves too.
resource "aws_launch_template" "agent_private" {
  count         = length(var.agent_spot_subnet_ids) > 0 ? 1 : 0
  name_prefix   = "${local.name}-agent-private-"
  image_id      = var.ami_id
  user_data     = local.agent_user_data
  ebs_optimized = true
  iam_instance_profile { name = aws_iam_instance_profile.node.name }
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }
  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size           = var.agent_root_volume_gb
      volume_type           = "gp3"
      delete_on_termination = true
      encrypted             = true
    }
  }
  network_interfaces {
    associate_public_ip_address = false
    delete_on_termination       = true
    security_groups             = [aws_security_group.nodes.id]
  }
  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.tags, { Name = "${local.name}-agent-spot", "k3s:role" = "agent" })
  }
  tags = local.tags
}

# ── Auto Scaling: servers = on-demand base + spot, capacity-optimized across both AZs ─
# The base on-demand server(s) are launched with the first override type
# ("prioritized"), so keep the cheapest type first in server_instance_types.
resource "aws_autoscaling_group" "servers" {
  name                      = "${local.name}-servers"
  min_size                  = var.server_count
  max_size                  = var.server_count
  desired_capacity          = var.server_count
  vpc_zone_identifier       = var.server_subnet_ids != null ? var.server_subnet_ids : var.subnet_ids
  health_check_type         = "EC2"
  health_check_grace_period = 300
  capacity_rebalance        = true # relaunch when AWS signals elevated reclaim risk

  # Never terminate a healthy etcd member just to even out AZs. On 2026-10-01
  # at 09:31Z, during spot churn, AZRebalance terminated a live server and etcd
  # lost quorum until a cluster-reset. The servers span two zones, so three
  # always split 2/1 and rebalancing cannot spread them further. Suspended live
  # by CLI that day; this matches it.
  suspended_processes = ["AZRebalance"]

  mixed_instances_policy {
    instances_distribution {
      on_demand_base_capacity                  = var.server_on_demand_base
      on_demand_percentage_above_base_capacity = 0
      on_demand_allocation_strategy            = "prioritized"
      spot_allocation_strategy                 = "capacity-optimized"
    }
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.server.id
        version            = "$Latest"
      }
      dynamic "override" {
        for_each = var.server_instance_types
        content { instance_type = override.value }
      }
    }
  }

  dynamic "tag" {
    for_each = merge(local.tags, { Name = "${local.name}-server" })
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = true
    }
  }
  lifecycle { ignore_changes = [desired_capacity] }
}

# ── platform pool, on-demand half ───────────────────────────────────────
# Split 2026-10-11 (gd-AEm6o8aWkF). This group is FIXED at
# agent_on_demand_base on-demand nodes: gardeners and thread agents require
# on-demand (podmanager capacityAffinity), Postgres/NATS/beads sit on
# node-local PVs here, and cluster-autoscaler never touches it (no discovery
# tags). The name stays "${local.name}-agents" because renaming an ASG
# replaces it, which would terminate the three nodes carrying the stateful
# pods. Spot capacity lives in "agents_spot" below, which the autoscaler
# owns. Raise agent_on_demand_base (by CLI first, see composition) when a
# gardener sits Pending on "Insufficient cpu"; never expect spot to take it.
resource "aws_autoscaling_group" "agents" {
  name                      = "${local.name}-agents"
  min_size                  = var.agent_on_demand_base
  max_size                  = var.agent_on_demand_base
  desired_capacity          = var.agent_on_demand_base
  vpc_zone_identifier       = var.subnet_ids
  health_check_type         = "EC2"
  health_check_grace_period = 300
  capacity_rebalance        = true
  # Same reasoning as the CI pool below: never let the ASG kill a healthy
  # node just to even out AZs. Agent pods are unowned bare pods (a node death
  # loses the crew's session), and platform Deployments land here too. On
  # 2026-09-23 1a spot was unfulfillable 16:45-16:58Z, the pool skewed 2/4,
  # and when 1a capacity returned AZRebalance terminated i-0123456789abcdef0 at
  # 17:06:40Z -- the node gardenio-beads had just rolled onto -- for a ~4 min
  # fleet-wide beads outage. capacity_rebalance is a separate call, left as is.
  suspended_processes = ["AZRebalance"]

  mixed_instances_policy {
    instances_distribution {
      # One on-demand node always present: local-path PVs are node-local and
      # die with the node, so anything stateful on this pool (Postgres) needs
      # a node that is not on the spot market. Same reasoning as
      # server_on_demand_base=1 for the beads Dolt volume. It is also what
      # satisfies the controller's REQUIRED on-demand affinity for
      # interactive agent roles -- with 0 here those pods never schedule.
      on_demand_base_capacity = var.agent_on_demand_base
      # 100, not 0: this group is on-demand only now. Anything above the base
      # (there should be nothing; min = max) must also be on-demand so a
      # transient over-capacity never launches a spot node here.
      on_demand_percentage_above_base_capacity = 100
      spot_allocation_strategy                 = "capacity-optimized"
    }
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.agent.id
        version            = "$Latest"
      }
      dynamic "override" {
        for_each = var.agent_instance_types
        content { instance_type = override.value }
      }
    }
  }

  dynamic "tag" {
    for_each = merge(local.tags, { Name = "${local.name}-agent" })
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = true
    }
  }
  lifecycle { ignore_changes = [desired_capacity] }
}

# ── platform pool, spot half (cluster-autoscaler managed) ───────────────
# Same launch template, labels and taint as the on-demand half; the only
# differences are spot-only capacity and the autoscaler discovery tags. The
# node-template tags describe a node BEFORE it exists so scale-from-min works
# and so the autoscaler knows a node here can never satisfy an on-demand
# affinity (karpenter.sh/capacity-type=spot): a Pending gardener therefore
# stays Pending and raises the on-demand question instead of a useless spot
# launch (the 2026-09 failure in gd-cTobdw0kAL). Crews are bare pods, so the
# autoscaler's scale-down simulation refuses any node with a live crew; the
# platform Deployments on these nodes are evictable and move. Measured
# 2026-10-11: eight pinned nodes at 6-17% CPU on six of them, ~$4.6/day for
# the pool; this group lets the five spot nodes go away when crews are idle.
resource "aws_autoscaling_group" "agents_spot" {
  name                      = "${local.name}-agents-spot"
  min_size                  = var.agent_spot_min
  max_size                  = max(var.agent_count - var.agent_on_demand_base, var.agent_spot_min)
  desired_capacity          = var.agent_spot_min
  vpc_zone_identifier       = length(var.agent_spot_subnet_ids) > 0 ? var.agent_spot_subnet_ids : var.subnet_ids
  health_check_type         = "EC2"
  health_check_grace_period = 300
  # Same as the on-demand half: a crew's session dies with its node, so only
  # the autoscaler (scale-down) and a real spot reclaim may end one.
  capacity_rebalance  = true
  suspended_processes = ["AZRebalance"]

  metrics_granularity = "1Minute"
  enabled_metrics = [
    "GroupMinSize",
    "GroupMaxSize",
    "GroupDesiredCapacity",
    "GroupInServiceInstances",
    "GroupPendingInstances",
    "GroupStandbyInstances",
    "GroupTerminatingInstances",
    "GroupTotalInstances",
  ]

  mixed_instances_policy {
    instances_distribution {
      on_demand_base_capacity                  = 0
      on_demand_percentage_above_base_capacity = 0
      spot_allocation_strategy                 = "capacity-optimized"
    }
    launch_template {
      launch_template_specification {
        launch_template_id = length(var.agent_spot_subnet_ids) > 0 ? aws_launch_template.agent_private[0].id : aws_launch_template.agent.id
        version            = "$Latest"
      }
      # override[0] is the type the autoscaler models for this group.
      dynamic "override" {
        for_each = var.agent_instance_types
        content { instance_type = override.value }
      }
    }
  }

  dynamic "tag" {
    for_each = merge(local.tags, {
      Name                                                                       = "${local.name}-agent-spot"
      "k8s.io/cluster-autoscaler/enabled"                                        = "true"
      "k8s.io/cluster-autoscaler/${var.env_name}"                                = "owned"
      "k8s.io/cluster-autoscaler/node-template/label/gardenio.io/role"           = "platform"
      "k8s.io/cluster-autoscaler/node-template/label/gardenio.io/lifecycle"      = "spot"
      "k8s.io/cluster-autoscaler/node-template/label/karpenter.sh/capacity-type" = "spot"
      "k8s.io/cluster-autoscaler/node-template/taint/gardenio.agent"             = "true:NoSchedule"
    })
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = true
    }
  }
  lifecycle { ignore_changes = [desired_capacity] }
}

# ── CI pool: 0..N spot agents for GitHub Actions runner pods ────────────
# Tainted so only runner pods land here; labelled so runner pods land only
# here. cluster-autoscaler (auto-discovery tags below) scales it from zero on
# a pending runner pod and back to zero ~10 min after the job. The
# node-template tags tell the autoscaler what a node will look like before
# one exists, which is what makes scale-from-zero work.
locals {
  ci_extra_args = "--node-label gardenio.io/role=ci --node-taint ci=true:NoSchedule"
}

resource "aws_launch_template" "ci" {
  name_prefix   = "${local.name}-ci-"
  image_id      = var.ami_id
  user_data     = base64gzip(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "agent", extra_args = local.ci_extra_args })))
  ebs_optimized = true
  iam_instance_profile { name = aws_iam_instance_profile.node.name }
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }
  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size           = var.ci_root_volume_gb
      volume_type           = "gp3"
      delete_on_termination = true
      encrypted             = true
    }
  }
  network_interfaces {
    associate_public_ip_address = true
    delete_on_termination       = true
    security_groups             = [aws_security_group.nodes.id]
  }
  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.tags, { Name = "${local.name}-ci", "k3s:role" = "agent", "gardenio:pool" = "ci" })
  }
  tag_specifications {
    resource_type = "volume"
    tags          = local.tags
  }
  tags = local.tags
}

resource "aws_autoscaling_group" "ci" {
  name                      = "${local.name}-ci"
  min_size                  = 0
  max_size                  = var.ci_max_nodes
  desired_capacity          = 0
  vpc_zone_identifier       = length(var.ci_subnet_ids) > 0 ? var.ci_subnet_ids : var.subnet_ids
  health_check_type         = "EC2"
  health_check_grace_period = 300
  # A CI node is only ever busy or about to be removed; never let the ASG
  # terminate one on its own initiative. AZRebalance killed a node mid-job
  # on the first run ("launched to balance zones ... more than desired"),
  # and capacity rebalance would do the same on a mere risk signal. Only the
  # autoscaler (scale-down) and a real 2-minute spot notice may end a node.
  #
  # Now measured, 2026-09-22 (AWS/Events TriggeredRules, 7 days with data):
  # 200 "EC2 Instance Rebalance Recommendation" vs 121 "EC2 Spot Instance
  # Interruption Warning" account-wide -- 1.65 risk signals per real
  # interruption. Enabling capacity_rebalance converts each recommendation
  # into a proactive termination, so it would raise disruptive node deaths
  # ~65% to remove a replacement-latency gap that is NOT the dominant term in
  # queue time (that is FailedScheduling waiting for any node with room; see
  # helm/ci/README.md). Cross-check: AutoScalingManagedRule saw 321 = 200+121,
  # since it matches both detail-types. Do not flip this without re-measuring
  # BOTH counts; the ratio is the whole argument.
  capacity_rebalance  = false
  suspended_processes = ["AZRebalance"]

  # Without this the ASG emits NO group metrics at all, so there is no history
  # of desired/in-service/pending and you cannot tell whether the pool sat at
  # max_size (ci_max_nodes binding) or never needed to grow (burst shape) --
  # exactly the question every CI capacity decision turns on. Enabled by CLI
  # 2026-09-22 and declared here so an apply does not silently switch it back
  # off. ASG group metrics are not billed.
  metrics_granularity = "1Minute"
  enabled_metrics = [
    "GroupMinSize",
    "GroupMaxSize",
    "GroupDesiredCapacity",
    "GroupInServiceInstances",
    "GroupPendingInstances",
    "GroupStandbyInstances",
    "GroupTerminatingInstances",
    "GroupTotalInstances",
  ]

  mixed_instances_policy {
    instances_distribution {
      on_demand_base_capacity                  = 0
      on_demand_percentage_above_base_capacity = 0
      # The first scale-up (2026-09-17) got two interruption notices inside
      # 90 s and 13 UnfulfillableCapacity launch failures across four
      # m-family types in two AZs. Ten 4-vCPU types (m/r, 16-32 GiB) and the
      # price-capacity-optimized strategy give the pool somewhere to go.
      spot_allocation_strategy = "price-capacity-optimized"
    }
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.ci.id
        version            = "$Latest"
      }
      dynamic "override" {
        for_each = var.ci_instance_types
        content { instance_type = override.value }
      }
    }
  }

  dynamic "tag" {
    for_each = merge(local.tags, {
      Name                                                             = "${local.name}-ci"
      "k8s.io/cluster-autoscaler/enabled"                              = "true"
      "k8s.io/cluster-autoscaler/${var.env_name}"                      = "owned"
      "k8s.io/cluster-autoscaler/node-template/label/gardenio.io/role" = "ci"
      "k8s.io/cluster-autoscaler/node-template/taint/ci"               = "true:NoSchedule"
    })
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = true
    }
  }
  lifecycle { ignore_changes = [desired_capacity] }
}

# ── ziti public edge node ───────────────────────────────────────────────
# One on-demand node that exists only to host the public OpenZiti edge
# router (router-public-1, helm/ziti/templates/router-public.yaml) at a static address,
# ziti.clonk.tv (gd-dCvIWuT6t6). It is a plain aws_instance, not an ASG
# member, on purpose (operator ruling 2026-09-23, comment 3534):
#   - an ASG instance gets a fresh ENI on every replacement, so the EIP and
#     the separate SG (gd-oBrWeIVcsd static-eip, gd-II5iO7Nbbf separate-sg)
#     would have to be attached by the node itself at boot. That needs
#     ec2:AssociateAddress + ec2:ModifyNetworkInterfaceAttribute on the node
#     role, and every pod can reach that role over IMDS (gd-adirNxx3Qo).
#   - here terraform owns a standalone ENI carrying both SGs and the EIP; the
#     instance boots on that ENI, so the EIP is its public IP from the first
#     IMDS read, and nothing is granted to the node role.
# EC2 auto-recovery keeps the same instance id, ENI and EIP through a host
# failure. A deliberate replacement keeps the ENI and EIP too, since they are
# separate resources.
locals {
  ziti_edge_extra_args = "--node-label gardenio.io/role=ziti-edge --node-label gardenio.io/ziti-public-router=true --node-taint gardenio.io/ziti-edge=true:NoSchedule"
}

resource "aws_security_group" "ziti_edge" {
  count       = var.edge_node_enabled ? 1 : 0
  name        = "${local.name}-ziti-edge"
  description = "ziti public edge router node only"
  vpc_id      = aws_security_group.nodes.vpc_id
  tags        = local.tags
}

resource "aws_vpc_security_group_ingress_rule" "ziti_edge_public" {
  count             = var.ziti_public_router_enabled ? 1 : 0
  security_group_id = aws_security_group.ziti_edge[0].id
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 3022
  to_port           = 3022
  ip_protocol       = "tcp"
  description       = "ziti public edge router (ziti.clonk.tv) from the internet"
}

# Public web edge (decision gd-GbIgemW3dt, escalation gd-NoOR0riqwi): 443 from
# the internet on THIS node's own SG only, never on the shared nodes SG. The
# shared SG's 80/443 rules stay the GitHub-hooks allowlist for hooks.clonk.tv
# (gd-Bz9rkZg5Q0): ServiceLB SNATs, so Traefik cannot allowlist by source IP,
# and a world-open 443 there would publish every Traefik route. Here 443 is
# answered only by the public-edge nginx (helm/gardenio-ziti/public-edge.yaml),
# pinned to this node with hostPort 443; it serves exactly the hosts in
# public_edge_fqdns and an explicit path allowlist.
resource "aws_vpc_security_group_ingress_rule" "ziti_edge_web" {
  count             = var.edge_node_enabled && length(var.public_edge_fqdns) > 0 ? 1 : 0
  security_group_id = aws_security_group.ziti_edge[0].id
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
  description       = "public web edge (public-edge nginx) from the internet"
}

# Port 80 on the same node-only SG, for the public-edge nginx's redirect-only
# listener (hostPort 80): served hosts get a 301 to https on the same host,
# anything else is closed. Without it a bare or http:// link hung. Same scope
# as 443 above: this SG only, never the shared nodes SG, whose 80/443 rules
# stay the GitHub-hooks allowlist.
resource "aws_vpc_security_group_ingress_rule" "ziti_edge_http" {
  count             = var.edge_node_enabled && length(var.public_edge_fqdns) > 0 ? 1 : 0
  security_group_id = aws_security_group.ziti_edge[0].id
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 80
  to_port           = 80
  ip_protocol       = "tcp"
  description       = "public web edge http to https redirect (public-edge nginx) from the internet"
}

resource "aws_network_interface" "ziti_edge" {
  count           = var.edge_node_enabled ? 1 : 0
  subnet_id       = var.edge_subnet_id != "" ? var.edge_subnet_id : var.subnet_ids[0]
  security_groups = [aws_security_group.nodes.id, aws_security_group.ziti_edge[0].id]
  description     = "${local.name} ziti public edge node"
  tags            = merge(local.tags, { Name = "${local.name}-ziti-edge" })
}

# 32.199.110.32: ziti.clonk.tv and the public_edge records point here, and a
# released address cannot be got back, so destroying it is refused. Turning
# the edge off on purpose means deleting prevent_destroy first.
resource "aws_eip" "ziti_edge" {
  count  = var.edge_node_enabled ? 1 : 0
  domain = "vpc"
  tags   = merge(local.tags, { Name = "${local.name}-ziti-edge" })

  lifecycle { prevent_destroy = true }
}

# The association is its own resource, not aws_eip.network_interface. With
# the attribute on the EIP, replacing the ENI and instance (C8, gd-0F6aKIECTU)
# updated the EIP in place with the old instance id still in its computed
# `instance` attribute, and AssociateAddress failed on the terminated instance,
# leaving the EIP unattached (gd-nTBAOEoxqj). Here an ENI replacement replaces
# only this association, and the EIP is never touched.
resource "aws_eip_association" "ziti_edge" {
  count                = var.edge_node_enabled ? 1 : 0
  allocation_id        = aws_eip.ziti_edge[0].id
  network_interface_id = aws_network_interface.ziti_edge[0].id
}

resource "aws_instance" "ziti_edge" {
  count                = var.edge_node_enabled ? 1 : 0
  ami                  = var.ami_id
  instance_type        = var.ziti_edge_instance_type
  iam_instance_profile = aws_iam_instance_profile.node.name
  user_data            = templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "agent", extra_args = local.ziti_edge_extra_args }))
  ebs_optimized        = true

  network_interface {
    network_interface_id = aws_network_interface.ziti_edge[0].id
    device_index         = 0
  }
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }
  maintenance_options { auto_recovery = "default" }
  root_block_device {
    volume_size           = var.root_volume_gb
    volume_type           = "gp3"
    delete_on_termination = true
    encrypted             = true
    tags                  = local.tags
  }
  tags = merge(local.tags, { Name = "${local.name}-ziti-edge", "k3s:role" = "agent" })

  # ami comes from the SSM "current" Ubuntu parameter and user_data from the
  # shared node template: either changing would otherwise replace (or
  # stop/start) the node the public router lives on. Roll it deliberately
  # with -replace=module.k3s.aws_instance.ziti_edge[0]; the ENI and EIP stay.
  lifecycle { ignore_changes = [ami, user_data] }

  # The EIP must be on the ENI before first boot so IMDS public-ipv4 (and so
  # --node-external-ip) is the EIP, not a transient address.
  depends_on = [aws_eip_association.ziti_edge]
}

resource "aws_route53_record" "ziti_edge" {
  count   = var.ziti_public_router_enabled ? 1 : 0
  zone_id = var.zone_id
  name    = var.ziti_edge_fqdn
  type    = "A"
  ttl     = 300
  records = [aws_eip.ziti_edge[0].public_ip]
}

resource "aws_route53_record" "public_edge" {
  for_each = var.edge_node_enabled ? toset(var.public_edge_fqdns) : toset([])
  zone_id  = var.zone_id
  name     = each.value
  type     = "A"
  ttl      = 300
  records  = [aws_eip.ziti_edge[0].public_ip]
}
