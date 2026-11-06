# modules/network — VPC + 3-tier subnets + NAT + IGW + route tables + VPC endpoints.
#
# Tier breakdown (one of each per AZ):
#   - public        — egress via IGW; map_public_ip_on_launch=true. Tagged for AWS LB Controller (`elb`).
#                     An AZ in nat_routed_public_azs egresses via the NAT instead, with no public IP on launch.
#   - edge          — optional (var.edge_subnets): small IGW-routed subnets for the NAT box and what keeps a public IP.
#   - private-app   — egress via NAT (per nat_mode); EKS node placement tier. Tagged for AWS LB Controller (`internal-elb`).
#   - private-data  — no NAT, no IGW route; RDS / stateful tier.
#
# VPC endpoints: `s3`/`dynamodb` from var.vpc_endpoint_services become Gateway endpoints
# (attached to all route tables). All others become Interface endpoints in the
# private-app subnets behind a shared SG. This matches plan-of-record §3.2 + D-4:
# POC composition passes `vpc_endpoint_services = ["s3"]` (gateway only); canonical-prod
# keeps the widened default set (S3 gateway + interface endpoints for the AWS
# control-plane data path — see variables.tf and the NAT-elimination ADR
# docs/research/canonical-prod-nat-elimination.md §3a).

locals {
  module_tags = merge(
    var.common_tags,
    {
      "gardenio:module" = "network"
      "gardenio:env"    = var.env_name
    },
  )

  azs      = length(var.availability_zones) > 0 ? var.availability_zones : slice(data.aws_availability_zones.available.names, 0, var.az_count)
  az_count = length(local.azs)

  public_subnet_cidrs       = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i)]
  private_app_subnet_cidrs  = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i + local.az_count)]
  private_data_subnet_cidrs = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 2 * local.az_count)]

  # Managed NAT Gateway count (nat_mode single|per-az). 0 for instance|none.
  nat_gateway_count = (
    var.nat_mode == "per-az" ? local.az_count :
    var.nat_mode == "single" ? 1 :
    0
  )

  # fck-nat / NAT-instance count (nat_mode=instance). Single box by default
  # (cost-parity with nat_mode=single); one-per-AZ when nat_instance_per_az=true.
  nat_instance_count = (
    var.nat_mode == "instance" ? (var.nat_instance_per_az ? local.az_count : 1) : 0
  )

  gateway_endpoint_services   = [for s in var.vpc_endpoint_services : s if contains(["s3", "dynamodb"], s)]
  interface_endpoint_services = [for s in var.vpc_endpoint_services : s if !contains(["s3", "dynamodb"], s)]
}

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-vpc"
  })
}

resource "aws_vpc_ipv4_cidr_block_association" "additional" {
  for_each   = toset(var.additional_cidr_blocks)
  vpc_id     = aws_vpc.this.id
  cidr_block = each.value
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-igw"
  })
}

resource "aws_subnet" "public" {
  count                   = local.az_count
  vpc_id                  = aws_vpc.this.id
  cidr_block              = local.public_subnet_cidrs[count.index]
  availability_zone       = local.azs[count.index]
  map_public_ip_on_launch = !contains(var.nat_routed_public_azs, local.azs[count.index])

  tags = merge(local.module_tags, {
    Name                     = "${var.env_name}-public-${local.azs[count.index]}"
    "gardenio:subnet-tier"   = "public"
    "kubernetes.io/role/elb" = "1"
  })
}

resource "aws_subnet" "private_app" {
  count             = local.az_count
  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_app_subnet_cidrs[count.index]
  availability_zone = local.azs[count.index]

  tags = merge(local.module_tags, {
    Name                              = "${var.env_name}-private-app-${local.azs[count.index]}"
    "gardenio:subnet-tier"            = "private-app"
    "kubernetes.io/role/internal-elb" = "1"
  })

  # karpenter.sh/discovery is stamped by aws_ec2_tag in the composition
  # (see composition/poc/karpenter_discovery.tf). Ignore that key here
  # so a full-state refresh does not fight the discovery wiring.
  lifecycle {
    ignore_changes = [tags["karpenter.sh/discovery"], tags_all["karpenter.sh/discovery"]]
  }
}

resource "aws_subnet" "private_data" {
  count             = local.az_count
  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_data_subnet_cidrs[count.index]
  availability_zone = local.azs[count.index]

  tags = merge(local.module_tags, {
    Name                   = "${var.env_name}-private-data-${local.azs[count.index]}"
    "gardenio:subnet-tier" = "private-data"
  })
}

resource "aws_eip" "nat" {
  count  = local.nat_gateway_count
  domain = "vpc"

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-nat-eip-${count.index}"
  })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_nat_gateway" "this" {
  count         = local.nat_gateway_count
  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-nat-${local.azs[count.index]}"
  })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-rt-public"
  })
}

resource "aws_route" "public_igw" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

# nat_routed_public_azs moves an AZ's public subnet onto its private-app route
# table, so it egresses through the NAT (cost plan 2026-10-21 C10). Changing
# route_table_id is an in-place ReplaceRouteTableAssociation, never a replace.
resource "aws_route_table_association" "public" {
  count     = local.az_count
  subnet_id = aws_subnet.public[count.index].id
  route_table_id = (
    contains(var.nat_routed_public_azs, local.azs[count.index])
    ? aws_route_table.private_app[count.index].id
    : aws_route_table.public.id
  )

  lifecycle {
    precondition {
      condition     = length(var.nat_routed_public_azs) == 0 || var.nat_mode != "none"
      error_message = "nat_routed_public_azs needs a NAT (nat_mode != \"none\"): with none, a flipped public subnet has no default route."
    }
  }
}

# Extra public subnets, outside the per-AZ count: raising az_count would shift
# every private-app and private-data CIDR (they are indexed after the public
# ones) and force those subnets to be replaced.
resource "aws_subnet" "public_extra" {
  for_each                = var.extra_public_subnets
  vpc_id                  = aws_vpc.this.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 4, each.value)
  availability_zone       = each.key
  map_public_ip_on_launch = !contains(var.nat_routed_public_azs, each.key)

  tags = merge(local.module_tags, {
    Name                     = "${var.env_name}-public-${each.key}"
    "gardenio:subnet-tier"   = "public"
    "kubernetes.io/role/elb" = "1"
  })
}

# An extra public subnet's AZ has no private-app table of its own, so a flipped
# one uses private_app[0].
resource "aws_route_table_association" "public_extra" {
  for_each  = aws_subnet.public_extra
  subnet_id = each.value.id
  route_table_id = (
    contains(var.nat_routed_public_azs, each.key)
    ? aws_route_table.private_app[0].id
    : aws_route_table.public.id
  )
}

# Edge subnets: small public subnets (IGW route) for what must keep a public IP
# once the public subnets route through the NAT: the NAT box itself (it must
# never sit in a subnet it routes), the public-edge node and the CI pool.
resource "aws_subnet" "edge" {
  for_each                = var.edge_subnets
  vpc_id                  = aws_vpc.this.id
  cidr_block              = each.value
  availability_zone       = each.key
  map_public_ip_on_launch = true

  tags = merge(local.module_tags, {
    Name                   = "${var.env_name}-edge-${each.key}"
    "gardenio:subnet-tier" = "edge"
  })
}

resource "aws_route_table_association" "edge" {
  for_each       = aws_subnet.edge
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private_app" {
  count  = local.az_count
  vpc_id = aws_vpc.this.id

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-rt-private-app-${local.azs[count.index]}"
  })

  # nat_mode=instance reclaims the `0.0.0.0/0` route at the box's boot via the
  # EC2 API (see nat_instance.tf), so that route is runtime-owned, not
  # terraform-owned. Ignore the route set so terraform does not delete the
  # reclaimed route on the next apply. No-op for single|per-az (the route is a
  # standalone aws_route.private_app_nat resource) and for none (no default route).
  lifecycle {
    ignore_changes = [route]
  }
}

# Default route via a managed NAT Gateway (nat_mode single|per-az).
# per-az: each private-app RT -> the NAT in its own AZ. single: all -> the one NAT.
resource "aws_route" "private_app_nat" {
  count                  = contains(["single", "per-az"], var.nat_mode) ? local.az_count : 0
  route_table_id         = aws_route_table.private_app[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = var.nat_mode == "per-az" ? aws_nat_gateway.this[count.index].id : aws_nat_gateway.this[0].id
}

# nat_mode=instance has NO static aws_route here on purpose. Each NAT box runs in
# an ASG-of-1 with an auto-assigned public IP + ephemeral ENI, so a terraform
# route pointed at an instance ENI would dangle the instant the ASG replaces the
# box. The box instead reclaims the `0.0.0.0/0` route in each route table it owns
# at boot (the canonical fck-nat self-heal — see nat_instance.tf). The
# aws_route_table.private_app lifecycle ignores `route` so terraform leaves that
# runtime-owned route alone.

resource "aws_route_table_association" "private_app" {
  count          = local.az_count
  subnet_id      = aws_subnet.private_app[count.index].id
  route_table_id = aws_route_table.private_app[count.index].id
}

resource "aws_route_table" "private_data" {
  count  = local.az_count
  vpc_id = aws_vpc.this.id

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-rt-private-data-${local.azs[count.index]}"
  })
}

resource "aws_route_table_association" "private_data" {
  count          = local.az_count
  subnet_id      = aws_subnet.private_data[count.index].id
  route_table_id = aws_route_table.private_data[count.index].id
}

resource "aws_vpc_endpoint" "gateway" {
  for_each          = toset(local.gateway_endpoint_services)
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.region}.${each.value}"
  vpc_endpoint_type = "Gateway"

  route_table_ids = concat(
    [aws_route_table.public.id],
    aws_route_table.private_app[*].id,
    aws_route_table.private_data[*].id,
  )

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-vpce-${each.value}"
  })
}

resource "aws_security_group" "interface_endpoints" {
  count       = length(local.interface_endpoint_services) > 0 ? 1 : 0
  name        = "${var.env_name}-vpce-interface"
  description = "Allow HTTPS from VPC to interface VPC endpoints"
  vpc_id      = aws_vpc.this.id

  ingress {
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = concat([aws_vpc.this.cidr_block], var.additional_cidr_blocks)
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-vpce-interface-sg"
  })
}

resource "aws_vpc_endpoint" "interface" {
  for_each            = toset(local.interface_endpoint_services)
  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${var.region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.private_app[*].id
  security_group_ids  = [aws_security_group.interface_endpoints[0].id]
  private_dns_enabled = var.enable_private_dns_for_endpoints

  tags = merge(local.module_tags, {
    Name = "${var.env_name}-vpce-${replace(each.value, ".", "-")}"
  })
}
