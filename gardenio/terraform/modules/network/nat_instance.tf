# modules/network — fck-nat / NAT-instance egress path (nat_mode="instance").
#
# A NAT *instance* is a like-for-like replacement for the managed NAT Gateway:
# a small ARM box (t4g.nano) with source/dest-check disabled running iptables
# MASQUERADE, carrying the private-app `0.0.0.0/0` egress for the ~78% of traffic
# that is IPv4-only public HTTPS with no PrivateLink (matrix ka-0000005663).
#
# Why this exists (ADR: docs/research/canonical-prod-nat-elimination.md):
#   - NAT-specific fixed cost ~$6.72/mo (t4g.nano + 1 IPv4) vs ~$36.50/mo for a
#     managed single NAT GW — a ~$57/mo saving at single-box parity.
#   - associate_public_ip_address=true => an *auto-assigned* public IPv4, so the
#     box consumes 0 Elastic IPs. This is the lever that unblocks the P4.1 prod
#     apply under the saturated EIP quota L-0263D0A3 (ka-0000000545 / ka-0000001677).
#
# Self-heal — ASG-of-1 + route-reclaim (ADR §9.1, this file's deliverable ka-0000006159):
#   Each box runs in an Auto Scaling Group of size 1. If the box dies (Spot
#   reclaim, hardware fault, failed status check) the ASG relaunches it in
#   ~1-3 min. Because the replacement gets a NEW auto-assigned IP and a NEW ENI,
#   the default route cannot be a static terraform `aws_route` pointed at an
#   instance ENI — it would dangle the moment the box is replaced. Instead the
#   box RECLAIMS the route itself on every boot: its user_data (a) disables its
#   own source/dest check and (b) rewrites the `0.0.0.0/0` route in each route
#   table it owns to point at its own ENI (the canonical fck-nat
#   `update_route_tables` pattern). 0 EIP, no Lambda, survives ASG replacement.
#   This is why main.tf does NOT create a static instance route for instance
#   mode — the route is runtime-owned by the box.
#
# Topology:
#   - nat_instance_per_az=false (default): ONE ASG/box in public[0] (or the first
#     edge subnet, when var.edge_subnets is set) that owns the
#     `0.0.0.0/0` route in ALL private-app route tables. Cost parity with
#     nat_mode="single". One box is a SPOF for IPv4 egress between failure and
#     ASG relaunch.
#   - nat_instance_per_az=true: one ASG/box per AZ in public[i] (or edge[AZ i]); box i owns ONLY
#     its own AZ's private-app route table (same-AZ default-route wiring), so one
#     AZ's box loss only degrades that AZ. Either way 0 EIPs.

# AWS account/region/partition for the IAM resource ARNs below. Cheap, always
# available — declared unconditionally even when nat_mode != instance.
data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}

# Latest Amazon Linux 2023 AMI for the NAT box CPU arch (arm64 for t4g). The
# STANDARD (not -minimal) AL2023 ships the AWS CLI v2 preinstalled, which the
# route-reclaim user_data needs (modify-instance-attribute + replace-route) — the
# minimal AMI would force a boot-time `dnf install` whose package name drifts
# across AL2023 minor versions. Still an Amazon-owned AMI: no marketplace
# subscription coupling, which is the property the upstream fck-nat AMI lacks.
data "aws_ssm_parameter" "nat_instance_ami" {
  count = local.nat_instance_count > 0 ? 1 : 0
  name  = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-${var.nat_instance_arch}"
}

locals {
  # A burstable T-family NAT box (t2/t3/t3a/t4g) accepts a credit_specification;
  # fixed-throughput families (m/c/r/…) reject it. Emit the block only for T
  # types so an operator can move to a non-burstable family (bead option (b))
  # without a launch-template validation error. Prefix match on the family token
  # before the first ".".
  nat_instance_is_burstable = (
    local.nat_instance_count == 0 ? false :
    can(regex("^t[0-9]", split(".", var.nat_instance_type)[0]))
  )

  # Route tables each box owns (and reclaims the default route in):
  #   per-az : box i owns private_app[i]            (same-AZ wiring)
  #   single : box 0 owns ALL private_app[*]        (one box for the VPC)
  # One inner list per box, indexed by the box's count.index in the ASG below.
  nat_instance_owned_rt_ids = (
    local.nat_instance_count == 0 ? [] :
    var.nat_instance_per_az
    ? [for i in range(local.az_count) : [aws_route_table.private_app[i].id]]
    : [aws_route_table.private_app[*].id]
  )
}

# NAT-instance security group: allow all egress; allow all ingress FROM the VPC
# (so private-app pods can route through it). No public ingress.
resource "aws_security_group" "nat_instance" {
  count       = local.nat_instance_count > 0 ? 1 : 0
  name        = "${var.env_name}-nat-instance"
  description = "fck-nat / NAT instance: forward egress from the VPC, no public ingress"
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "Forwarded traffic from VPC private-app subnets"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = concat([aws_vpc.this.cidr_block], var.additional_cidr_blocks)
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-nat-instance-sg"
  })
}

# ---------------------------------------------------------------------------
# IAM: the box disables its own source/dest check + reclaims its own route.
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "nat_instance_assume" {
  count = local.nat_instance_count > 0 ? 1 : 0

  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "nat_instance" {
  count = local.nat_instance_count > 0 ? 1 : 0

  # Disable source/dest check on a NAT box, scoped by the nat-role tag the launch
  # template propagates, so this can only ever target a box this module launched.
  statement {
    sid       = "DisableSourceDestCheck"
    actions   = ["ec2:ModifyInstanceAttribute"]
    resources = ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:instance/*"]
    condition {
      test     = "StringEquals"
      variable = "ec2:ResourceTag/gardenio:nat-role"
      values   = ["instance"]
    }
  }

  # Rewrite the default route in exactly the private-app route tables this module
  # owns — never any other route table in the account.
  statement {
    sid     = "ReclaimDefaultRoute"
    actions = ["ec2:CreateRoute", "ec2:ReplaceRoute"]
    resources = [
      for rt in aws_route_table.private_app :
      "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:route-table/${rt.id}"
    ]
  }

  # Describe* has no resource-level scoping in the EC2 API, so "*" is required.
  statement {
    sid       = "DescribeForReclaim"
    actions   = ["ec2:DescribeRouteTables", "ec2:DescribeInstances", "ec2:DescribeNetworkInterfaces"]
    resources = ["*"]
  }
}

resource "aws_iam_role" "nat_instance" {
  count              = local.nat_instance_count > 0 ? 1 : 0
  name               = "${var.env_name}-nat-instance"
  assume_role_policy = data.aws_iam_policy_document.nat_instance_assume[0].json

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-nat-instance-role"
  })
}

resource "aws_iam_role_policy" "nat_instance" {
  count  = local.nat_instance_count > 0 ? 1 : 0
  name   = "${var.env_name}-nat-instance-reclaim"
  role   = aws_iam_role.nat_instance[0].id
  policy = data.aws_iam_policy_document.nat_instance[0].json
}

resource "aws_iam_instance_profile" "nat_instance" {
  count = local.nat_instance_count > 0 ? 1 : 0
  name  = "${var.env_name}-nat-instance"
  role  = aws_iam_role.nat_instance[0].name

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-nat-instance-profile"
  })
}

# SSM Session Manager on the box: a shell without SSH or a public ingress rule.
resource "aws_iam_role_policy_attachment" "nat_instance_ssm" {
  count      = local.nat_instance_count > 0 ? 1 : 0
  role       = aws_iam_role.nat_instance[0].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# ---------------------------------------------------------------------------
# Launch template + ASG-of-1 per box.
# ---------------------------------------------------------------------------
resource "aws_launch_template" "nat_instance" {
  count = local.nat_instance_count

  name_prefix   = "${var.env_name}-nat-instance-${count.index}-"
  image_id      = data.aws_ssm_parameter.nat_instance_ami[0].value
  instance_type = var.nat_instance_type

  iam_instance_profile {
    arn = aws_iam_instance_profile.nat_instance[0].arn
  }

  # Auto-assigned public IPv4 (0 EIP). SG + public-IP live on the primary ENI;
  # source/dest check is disabled by the box itself at boot (no launch-template
  # attribute exists for it).
  network_interfaces {
    device_index                = 0
    associate_public_ip_address = true
    security_groups             = [aws_security_group.nat_instance[0].id]
    delete_on_termination       = true
  }

  metadata_options {
    http_tokens   = "required" # IMDSv2-only
    http_endpoint = "enabled"
  }

  # CPU-credit mode for burstable NAT boxes. "unlimited" (default) keeps the box
  # from throttling to baseline CPU under a packet-forwarding spike — the CPU half
  # of the burst-credit hardening after the 2026-07-12 egress-saturation outage
  # (ka-0000007968). Omitted for fixed-throughput families, which reject it.
  dynamic "credit_specification" {
    for_each = local.nat_instance_is_burstable ? [1] : []
    content {
      cpu_credits = var.nat_instance_cpu_credits
    }
  }

  # Optional Spot capacity: cheaper, and the ASG-of-1 + route-reclaim already
  # tolerate the box vanishing — a Spot interruption is just another relaunch.
  dynamic "instance_market_options" {
    for_each = var.nat_instance_use_spot ? [1] : []
    content {
      market_type = "spot"
      spot_options {
        spot_instance_type             = "one-time"
        instance_interruption_behavior = "terminate"
      }
    }
  }

  # Cloud-init that turns the box into a self-healing NAT: enable IP forward +
  # MASQUERADE, persist iptables, disable own source/dest check, then claim the
  # default route in each owned route table. Re-runs on every ASG relaunch, so a
  # replacement box re-points the route at its own (new) ENI automatically.
  user_data = base64encode(<<-EOT
    #!/bin/bash
    set -euxo pipefail

    # --- IMDSv2: identity + primary ENI of this box ---
    TOKEN=$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
    imds() { curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" "http://169.254.169.254/latest/meta-data/$1"; }
    IID=$(imds instance-id)
    REGION=$(imds placement/region)
    MAC=$(imds network/interfaces/macs/ | head -n1 | tr -d /)
    ENI=$(imds "network/interfaces/macs/$MAC/interface-id")
    PRIMARY_IF=$(ip -o -4 route show to default | awk '{print $5}' | head -n1)

    # --- NAT datapath: forward + masquerade, persisted across reboot ---
    # AL2023 does NOT ship the iptables binary preinstalled, so the package
    # MUST be installed before the first iptables invocation. Under
    # `set -euo pipefail` an `iptables: command not found` aborts the whole
    # cloud-init script — which is exactly what left prod's private subnets
    # with no default route / zero egress (ka-0000000545). Install first.
    dnf install -y iptables iptables-services
    sysctl -w net.ipv4.ip_forward=1
    echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/99-nat.conf
    iptables -t nat -A POSTROUTING -o "$PRIMARY_IF" -j MASQUERADE
    iptables -A FORWARD -i "$PRIMARY_IF" -m state --state RELATED,ESTABLISHED -j ACCEPT
    iptables -A FORWARD -o "$PRIMARY_IF" -j ACCEPT
    iptables-save > /etc/sysconfig/iptables
    systemctl enable iptables

    # --- Link-local NTP (free win, §6): pin time sync at the AWS Amazon Time
    # Sync source 169.254.169.123 so the box's own NTP never leaves via the
    # public internet — the same win wired onto the EKS nodes. AL2023 ships
    # chrony; this drop-in makes the link-local source authoritative.
    cat > /etc/chrony.d/amazon-time-sync.conf <<'CHRONY'
    server 169.254.169.123 prefer iburst minpoll 4 maxpoll 4
    CHRONY
    systemctl restart chronyd || true

    # --- A NAT box must forward traffic not addressed to it ---
    aws ec2 modify-instance-attribute --region "$REGION" --instance-id "$IID" --no-source-dest-check

    # --- Reclaim the default route in each route table this box owns ---
    for RTB in ${join(" ", local.nat_instance_owned_rt_ids[count.index])}; do
      aws ec2 replace-route --region "$REGION" --route-table-id "$RTB" \
        --destination-cidr-block 0.0.0.0/0 --network-interface-id "$ENI" \
      || aws ec2 create-route --region "$REGION" --route-table-id "$RTB" \
        --destination-cidr-block 0.0.0.0/0 --network-interface-id "$ENI"
    done
  EOT
  )

  tag_specifications {
    resource_type = "instance"
    tags = merge(local.module_tags, {
      Name                   = "${var.env_name}-nat-instance-${local.azs[count.index]}"
      "gardenio:nat-role"    = "instance"
      "gardenio:subnet-tier" = "public"
    })
  }

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-nat-instance-${count.index}-lt"
  })
}

# ASG-of-1: keeps exactly one NAT box alive per launch template. A terminated box
# (Spot reclaim, fault, manual replace) is relaunched within ~1-3 min and
# reclaims its route via user_data — this is the §9.1 self-heal.
resource "aws_autoscaling_group" "nat_instance" {
  count = local.nat_instance_count

  name_prefix      = "${var.env_name}-nat-instance-${count.index}-"
  min_size         = 1
  max_size         = 1
  desired_capacity = 1
  # With edge_subnets set the box lives there, never in a subnet it may route
  # (nat_routed_public_azs): per-AZ, its own AZ's edge subnet; shared, the first.
  vpc_zone_identifier = [
    length(var.edge_subnets) == 0 ? aws_subnet.public[count.index].id
    : (var.nat_instance_per_az ? aws_subnet.edge[local.azs[count.index]].id : aws_subnet.edge[sort(keys(var.edge_subnets))[0]].id)
  ]

  # The unhealthy alarm below reads GroupInServiceInstances; an ASG publishes it
  # only with group metrics on (else the alarm's missing data is "breaching").
  metrics_granularity = "1Minute"
  enabled_metrics     = ["GroupInServiceInstances"]

  # An unhealthy box should be replaced, but give user_data time to finish the
  # MASQUERADE + route-reclaim before the EC2 health check can fault it.
  health_check_type         = "EC2"
  health_check_grace_period = 120

  launch_template {
    id      = aws_launch_template.nat_instance[count.index].id
    version = "$Latest"
  }

  # New box before old box is torn down on a launch-template change, to minimise
  # the egress gap during a rolling AMI/config refresh.
  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 0 # size-1 ASG: cannot keep >0% healthy mid-refresh
    }
  }

  tag {
    key                 = "Name"
    value               = "${var.env_name}-nat-instance-${local.azs[count.index]}"
    propagate_at_launch = true
  }

  lifecycle {
    create_before_destroy = true

    precondition {
      condition     = length(var.edge_subnets) == 0 || !var.nat_instance_per_az || contains(keys(var.edge_subnets), local.azs[count.index])
      error_message = "nat_instance_per_az with edge_subnets needs an edge subnet in every AZ that gets a NAT box (${local.azs[count.index]} has none)."
    }
  }
}

# Day-1 egress-down detection: alarm when the ASG has no in-service box. The
# self-heal relaunch is automatic; this alarm is the page for when relaunch
# itself is failing (capacity, AMI, quota) — which the ASG cannot fix.
resource "aws_cloudwatch_metric_alarm" "nat_instance_unhealthy" {
  count               = local.nat_instance_count
  alarm_name          = "${var.env_name}-nat-instance-${local.azs[count.index]}-unhealthy"
  alarm_description   = "NAT-instance ASG ${count.index} has no in-service box — IPv4 egress for ${var.nat_instance_per_az ? "AZ ${local.azs[count.index]}" : "the VPC"} is down."
  namespace           = "AWS/AutoScaling"
  metric_name         = "GroupInServiceInstances"
  statistic           = "Minimum"
  comparison_operator = "LessThanThreshold"
  threshold           = 1
  period              = 60
  evaluation_periods  = 3
  treat_missing_data  = "breaching"

  dimensions = {
    AutoScalingGroupName = aws_autoscaling_group.nat_instance[count.index].name
  }

  alarm_actions = var.nat_instance_alarm_sns_topic_arns
  ok_actions    = var.nat_instance_alarm_sns_topic_arns

  tags = local.module_tags
}
