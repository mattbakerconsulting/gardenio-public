# Pools move to their own subnets and servers/agents can drop public IPs
# (plan 2026-10-21-aws-cost-cuts Task C3); the defaults change nothing.
# Run: terraform test   (from modules/k3s/)

mock_provider "aws" {
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
}
mock_provider "random" {}

variables {
  env_name    = "test"
  vpc_id      = "vpc-0123456789abcdef0"
  vpc_cidr    = "10.0.0.0/16"
  admin_cidrs = []
  subnet_ids  = ["subnet-0a", "subnet-0b"]
  zone_id     = "Z0123456789ABCDEFGHIJ"
  api_fqdn    = "k3s.example.invalid"
  join_fqdn   = "k3s-join.example.invalid"
  k3s_version = "v1.31.0+k3s1"
  ami_id      = "ami-0123456789abcdef0"
}

run "defaults_keep_todays_wiring" {
  command = plan
  assert {
    condition = (
      aws_launch_template.server.network_interfaces[0].associate_public_ip_address == "true" &&
      aws_launch_template.agent.network_interfaces[0].associate_public_ip_address == "true" &&
      tolist(aws_autoscaling_group.ci.vpc_zone_identifier) == tolist(["subnet-0a", "subnet-0b"]) &&
      tolist(aws_autoscaling_group.agents_spot.vpc_zone_identifier) == tolist(["subnet-0a", "subnet-0b"]) &&
      length(aws_launch_template.agent_private) == 0
    )
    error_message = "defaults must not change any pool"
  }
}

run "pools_move_and_lose_public_ips" {
  command = plan
  variables {
    # the edge ENI exists only with the edge node (off by default)
    edge_node_enabled     = true
    ci_subnet_ids         = ["subnet-edge-a", "subnet-edge-b"]
    agent_spot_subnet_ids = ["subnet-priv-a", "subnet-priv-b"]
    edge_subnet_id        = "subnet-edge-a"
    server_public_ip      = false
    agent_public_ip       = false
  }
  assert {
    condition = (
      aws_launch_template.server.network_interfaces[0].associate_public_ip_address == "false" &&
      aws_launch_template.agent.network_interfaces[0].associate_public_ip_address == "false" &&
      aws_launch_template.agent_private[0].network_interfaces[0].associate_public_ip_address == "false" &&
      aws_launch_template.ci.network_interfaces[0].associate_public_ip_address == "true"
    )
    error_message = "servers and agents lose public IPs; CI keeps them"
  }
  assert {
    condition = (
      tolist(aws_autoscaling_group.ci.vpc_zone_identifier) == tolist(["subnet-edge-a", "subnet-edge-b"]) &&
      tolist(aws_autoscaling_group.agents_spot.vpc_zone_identifier) == tolist(["subnet-priv-a", "subnet-priv-b"]) &&
      aws_network_interface.ziti_edge[0].subnet_id == "subnet-edge-a"
    )
    error_message = "CI, spot and edge go to their own subnets"
  }
}

run "boot_script_handles_no_public_ip" {
  command = plan
  assert {
    condition = alltrue([
      for s in ["$${PUBLIC_IP:+--node-external-ip $PUBLIC_IP}", "$${PUBLIC_IP:+--tls-san $PUBLIC_IP}", "k3s-dns add \"$${PUBLIC_IP:-$PRIVATE_IP}\""] :
      strcontains(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "server", extra_args = "" })), s)
    ])
    error_message = "the boot script must not pass empty public-IP flags or publish an empty public IP"
  }
  # "H4sI" is base64 for the gzip magic. Measured 2026-10-21: plain base64 is
  # ~21.2K characters (~15.9 KB raw, ~450 bytes under the limit), gzipped ~9K.
  assert {
    condition = (
      startswith(aws_launch_template.agent.user_data, "H4sI") && length(aws_launch_template.agent.user_data) < 12000 &&
      startswith(aws_launch_template.ci.user_data, "H4sI") && length(aws_launch_template.ci.user_data) < 12000
    )
    error_message = "agent/ci user_data must be gzipped (16 KB EC2 limit)"
  }
  assert {
    condition     = length(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "agent", extra_args = local.ziti_edge_extra_args }))) < 16384
    error_message = "the edge instance's raw user_data must stay under 16 KB"
  }
}
