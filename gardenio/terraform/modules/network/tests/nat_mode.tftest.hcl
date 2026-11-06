# Native terraform tests for the modules/network NAT-egress wiring.
#
# Mocks the AWS provider (no creds, no API calls) and plans the module under each
# nat_mode, asserting the right set of egress resources is created. This locks in
# the count math in main.tf + nat_instance.tf — especially the instance path
# (ka-0000006159): ASG-of-1 self-heal, 0 managed NAT GW, 0 EIP, and the route
# being runtime-owned (NO static aws_route for instance mode).
#
# Run: terraform test   (from modules/network/)

mock_provider "aws" {
  # Generate computed values (ids) at plan time too, so the C2 runs can compare
  # which subnet / route table a resource points at under `command = plan`.
  override_during = plan
  mock_resource "aws_subnet" {}
  mock_resource "aws_route_table" {}

  # aws_iam_policy_document.json is validated as JSON by aws_iam_role; the default
  # mock returns "" which fails that check. Feed both policy-doc data sources a
  # valid (empty-statement) JSON object so the instance-mode plan can proceed.
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  # The SSM policy attachment's policy_arn is validated as an ARN; the default
  # mock partition is a random string.
  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }
}

variables {
  env_name = "test"
  region   = "us-east-1"
  vpc_cidr = "10.0.0.0/16"
  az_count = 3
  # Pin AZs so the plan does not need the data.aws_availability_zones lookup.
  availability_zones = ["us-east-1a", "us-east-1b", "us-east-1c"]
}

# --- single: one managed NAT GW + one EIP, no NAT instance ---------------------
run "nat_mode_single" {
  command = plan

  variables {
    nat_mode = "single"
  }

  assert {
    condition     = length(aws_nat_gateway.this) == 1
    error_message = "single: expected exactly 1 managed NAT gateway"
  }
  assert {
    condition     = length(aws_eip.nat) == 1
    error_message = "single: expected exactly 1 NAT EIP"
  }
  assert {
    condition     = length(aws_route.private_app_nat) == 3
    error_message = "single: expected one managed-NAT default route per AZ"
  }
  assert {
    condition     = length(aws_autoscaling_group.nat_instance) == 0
    error_message = "single: expected no NAT-instance ASG"
  }
}

# --- per-az: one managed NAT GW + EIP per AZ -----------------------------------
run "nat_mode_per_az" {
  command = plan

  variables {
    nat_mode = "per-az"
  }

  assert {
    condition     = length(aws_nat_gateway.this) == 3
    error_message = "per-az: expected one managed NAT gateway per AZ"
  }
  assert {
    condition     = length(aws_eip.nat) == 3
    error_message = "per-az: expected one EIP per AZ"
  }
  assert {
    condition     = length(aws_autoscaling_group.nat_instance) == 0
    error_message = "per-az: expected no NAT-instance ASG"
  }
}

# --- instance (shared box): 1 ASG-of-1, 0 managed NAT, 0 EIP, no static route --
run "nat_mode_instance_shared" {
  command = plan

  variables {
    nat_mode            = "instance"
    nat_instance_per_az = false
  }

  assert {
    condition     = length(aws_autoscaling_group.nat_instance) == 1
    error_message = "instance/shared: expected exactly 1 NAT-instance ASG"
  }
  assert {
    condition     = length(aws_launch_template.nat_instance) == 1
    error_message = "instance/shared: expected exactly 1 launch template"
  }
  assert {
    condition     = aws_autoscaling_group.nat_instance[0].min_size == 1 && aws_autoscaling_group.nat_instance[0].max_size == 1 && aws_autoscaling_group.nat_instance[0].desired_capacity == 1
    error_message = "instance: ASG must be size-1 (min=max=desired=1) for self-heal"
  }
  assert {
    condition     = length(aws_nat_gateway.this) == 0 && length(aws_eip.nat) == 0
    error_message = "instance: must consume 0 managed NAT gateways and 0 EIPs"
  }
  assert {
    condition     = length(aws_route.private_app_nat) == 0
    error_message = "instance: no managed-NAT default route"
  }
  assert {
    condition     = length(aws_iam_instance_profile.nat_instance) == 1
    error_message = "instance: expected the route-reclaim instance profile"
  }
  # The shared box owns the default route in ALL three private-app route tables.
  assert {
    condition     = length(local.nat_instance_owned_rt_ids[0]) == 3
    error_message = "instance/shared: the single box must own all 3 private-app route tables"
  }
}

# --- instance (per-AZ): one ASG-of-1 per AZ, each owning its own route table ----
run "nat_mode_instance_per_az" {
  command = plan

  variables {
    nat_mode            = "instance"
    nat_instance_per_az = true
  }

  assert {
    condition     = length(aws_autoscaling_group.nat_instance) == 3
    error_message = "instance/per-az: expected one NAT-instance ASG per AZ"
  }
  assert {
    condition     = length(aws_launch_template.nat_instance) == 3
    error_message = "instance/per-az: expected one launch template per AZ"
  }
  assert {
    condition     = length(aws_eip.nat) == 0
    error_message = "instance/per-az: must still consume 0 EIPs"
  }
  # Same-AZ wiring: each box owns exactly its own AZ's route table.
  assert {
    condition     = alltrue([for ids in local.nat_instance_owned_rt_ids : length(ids) == 1])
    error_message = "instance/per-az: each box must own exactly 1 (its own) route table"
  }
}

# --- none: no default route at all ---------------------------------------------
run "nat_mode_none" {
  command = plan

  variables {
    nat_mode = "none"
  }

  assert {
    condition     = length(aws_nat_gateway.this) == 0 && length(aws_autoscaling_group.nat_instance) == 0
    error_message = "none: expected no NAT gateways and no NAT instances"
  }
  assert {
    condition     = length(aws_route.private_app_nat) == 0
    error_message = "none: expected no default route"
  }
}

# --- sizing + credit spec: a burstable box carries the requested type + credit
# mode; the credit_specification block is emitted for T-family types (ka-0000007968).
run "nat_instance_sizing_burstable" {
  command = plan

  variables {
    nat_mode                 = "instance"
    nat_instance_per_az      = false
    nat_instance_type        = "t4g.medium"
    nat_instance_cpu_credits = "unlimited"
  }

  assert {
    condition     = aws_launch_template.nat_instance[0].instance_type == "t4g.medium"
    error_message = "sizing: launch template must carry the requested nat_instance_type"
  }
  assert {
    condition     = local.nat_instance_is_burstable
    error_message = "sizing: t4g.medium must be detected as a burstable (T-family) type"
  }
  assert {
    condition     = length(aws_launch_template.nat_instance[0].credit_specification) == 1
    error_message = "sizing: a T-family box must emit a credit_specification block"
  }
  assert {
    condition     = aws_launch_template.nat_instance[0].credit_specification[0].cpu_credits == "unlimited"
    error_message = "sizing: cpu_credits must follow nat_instance_cpu_credits"
  }
}

# --- fixed-throughput family: no credit_specification (would be rejected) -------
run "nat_instance_fixed_throughput_no_credit_spec" {
  command = plan

  variables {
    nat_mode          = "instance"
    nat_instance_type = "c6gn.medium"
    nat_instance_arch = "arm64"
  }

  assert {
    condition     = local.nat_instance_is_burstable == false
    error_message = "fixed-throughput: c6gn.medium must not be treated as burstable"
  }
  assert {
    condition     = length(aws_launch_template.nat_instance[0].credit_specification) == 0
    error_message = "fixed-throughput: must NOT emit a credit_specification block"
  }
}

# --- cost plan 2026-10-21 C2: edge subnets host the NAT box; per-AZ route flip ---
run "edge_subnets_host_the_nat_box" {
  command = plan

  variables {
    nat_mode     = "instance"
    edge_subnets = { "us-east-1a" = "10.0.160.0/24", "us-east-1b" = "10.0.161.0/24" }
  }

  assert {
    condition     = length(aws_subnet.edge) == 2 && alltrue([for s in aws_subnet.edge : s.map_public_ip_on_launch])
    error_message = "two public edge subnets"
  }
  assert {
    condition     = contains(aws_autoscaling_group.nat_instance[0].vpc_zone_identifier, aws_subnet.edge["us-east-1a"].id)
    error_message = "the NAT box must live in an edge subnet, never a subnet it routes"
  }
  assert {
    condition     = contains(aws_autoscaling_group.nat_instance[0].enabled_metrics, "GroupInServiceInstances")
    error_message = "the unhealthy alarm needs GroupInServiceInstances"
  }
  assert {
    condition     = length(aws_iam_role_policy_attachment.nat_instance_ssm) == 1
    error_message = "the NAT box must be reachable over SSM"
  }
  assert {
    condition     = alltrue([for a in aws_route_table_association.edge : a.route_table_id == aws_route_table.public.id])
    error_message = "edge subnets keep the IGW route table"
  }
}

# The C1 topology: one box per AZ, each in its own AZ's edge subnet.
run "per_az_boxes_live_in_their_own_edge_subnet" {
  command = plan

  variables {
    nat_mode            = "instance"
    nat_instance_per_az = true
    az_count            = 2
    availability_zones  = ["us-east-1a", "us-east-1b"]
    edge_subnets        = { "us-east-1a" = "10.0.160.0/24", "us-east-1b" = "10.0.161.0/24" }
  }

  assert {
    condition     = aws_autoscaling_group.nat_instance[0].vpc_zone_identifier == toset([aws_subnet.edge["us-east-1a"].id])
    error_message = "box 0 must live in edge 1a"
  }
  assert {
    condition     = aws_autoscaling_group.nat_instance[1].vpc_zone_identifier == toset([aws_subnet.edge["us-east-1b"].id])
    error_message = "box 1 must live in edge 1b"
  }
}

run "per_az_boxes_need_an_edge_subnet_in_every_az" {
  command = plan

  variables {
    nat_mode            = "instance"
    nat_instance_per_az = true
    edge_subnets        = { "us-east-1a" = "10.0.160.0/24" }
  }

  expect_failures = [aws_autoscaling_group.nat_instance]
}

run "flipping_one_az_routes_its_public_subnet_through_the_nat" {
  command = plan

  variables {
    nat_mode              = "instance"
    edge_subnets          = { "us-east-1a" = "10.0.160.0/24" }
    nat_routed_public_azs = ["us-east-1b"]
  }

  assert {
    condition     = aws_route_table_association.public[1].route_table_id == aws_route_table.private_app[1].id
    error_message = "the flipped AZ's public subnet must use its private-app route table"
  }
  assert {
    condition     = aws_route_table_association.public[0].route_table_id == aws_route_table.public.id
    error_message = "an unflipped AZ must keep the IGW route table"
  }
  assert {
    condition     = !aws_subnet.public[1].map_public_ip_on_launch && aws_subnet.public[0].map_public_ip_on_launch
    error_message = "a flipped subnet stops handing out public IPs"
  }
}

# An extra public subnet's AZ has no private-app table of its own; it uses private_app[0].
run "flipping_an_extra_public_az_uses_the_first_private_app_table" {
  command = plan

  variables {
    nat_mode              = "instance"
    az_count              = 2
    availability_zones    = ["us-east-1a", "us-east-1b"]
    extra_public_subnets  = { "us-east-1c" = 6 }
    edge_subnets          = { "us-east-1a" = "10.0.160.0/24" }
    nat_routed_public_azs = ["us-east-1c"]
  }

  assert {
    condition     = aws_route_table_association.public_extra["us-east-1c"].route_table_id == aws_route_table.private_app[0].id
    error_message = "the flipped extra public subnet must use private_app[0]"
  }
  assert {
    condition     = !aws_subnet.public_extra["us-east-1c"].map_public_ip_on_launch
    error_message = "a flipped extra public subnet stops handing out public IPs"
  }
  assert {
    condition     = alltrue([for a in aws_route_table_association.public : a.route_table_id == aws_route_table.public.id])
    error_message = "the counted public subnets stay on the IGW route table"
  }
}

# Flipping with no NAT would leave the public subnet with no default route at all.
run "flipping_needs_a_nat" {
  command = plan

  variables {
    nat_mode              = "none"
    nat_routed_public_azs = ["us-east-1a"]
  }

  expect_failures = [aws_route_table_association.public]
}

run "defaults_change_nothing" {
  command = plan

  variables {
    nat_mode = "none"
  }

  assert {
    condition     = length(aws_subnet.edge) == 0 && alltrue([for a in aws_route_table_association.public : a.route_table_id == aws_route_table.public.id])
    error_message = "no edge subnets and no flipped associations by default"
  }
  assert {
    condition     = alltrue([for s in aws_subnet.public : s.map_public_ip_on_launch])
    error_message = "public subnets keep handing out public IPs by default"
  }
}
