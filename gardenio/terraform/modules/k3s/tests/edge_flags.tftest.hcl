# Native terraform tests for the edge-node / ziti flag split (gd-4o2FF4Wiec).
#
# One flag, ziti_edge_enabled, used to gate the public-edge node, its ENI/EIP/SG,
# the 443 rule and the auth/claimlayer records together with the ziti 3022 rules
# and ziti.clonk.tv, so switching ziti off would have destroyed the public edge.
# It is now three flags. The property locked down: turning the two ziti flags
# off leaves the edge node and everything the public web edge needs in place.
#
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
  env_name          = "test"
  vpc_id            = "vpc-0123456789abcdef0"
  vpc_cidr          = "10.0.0.0/16"
  admin_cidrs       = ["10.0.0.0/8"]
  subnet_ids        = ["subnet-0a", "subnet-0b"]
  zone_id           = "Z0123456789ABCDEFGHIJ"
  api_fqdn          = "k3s.example.invalid"
  join_fqdn         = "k3s-join.example.invalid"
  k3s_version       = "v1.31.0+k3s1"
  ami_id            = "ami-0123456789abcdef0"
  public_edge_fqdns = ["claimlayer.example.invalid", "auth.example.invalid"]
}

# --- (a) the live state: everything on ----------------------------------------
run "all_flags_on_creates_every_edge_and_ziti_resource" {
  command = plan

  variables {
    edge_node_enabled          = true
    ziti_public_router_enabled = true
    ziti_router_vpc_enabled    = true
  }

  assert {
    condition = (
      length(aws_instance.ziti_edge) == 1 &&
      length(aws_eip.ziti_edge) == 1 &&
      length(aws_eip_association.ziti_edge) == 1 &&
      length(aws_network_interface.ziti_edge) == 1 &&
      length(aws_security_group.ziti_edge) == 1
    )
    error_message = "edge node, EIP, its association, ENI and SG must all exist"
  }

  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.ziti_edge_web) == 1 && length(aws_route53_record.public_edge) == 2
    error_message = "443 rule and both public_edge records must exist"
  }

  assert {
    condition = (
      length(aws_vpc_security_group_ingress_rule.ziti_edge_public) == 1 &&
      length(aws_route53_record.ziti_edge) == 1 &&
      length(aws_vpc_security_group_ingress_rule.ziti_router) == 1
    )
    error_message = "world 3022 rule, ziti.clonk.tv record and VPC 3022 rule must exist"
  }
}

# --- (b) ziti off, edge on: the case that protects the public edge ------------
run "ziti_off_keeps_the_public_edge" {
  command = plan

  variables {
    edge_node_enabled          = true
    ziti_public_router_enabled = false
    ziti_router_vpc_enabled    = false
  }

  assert {
    condition = (
      length(aws_instance.ziti_edge) == 1 &&
      length(aws_eip.ziti_edge) == 1 &&
      length(aws_eip_association.ziti_edge) == 1 &&
      length(aws_network_interface.ziti_edge) == 1 &&
      length(aws_security_group.ziti_edge) == 1
    )
    error_message = "turning ziti off must not touch the edge node, EIP, its association, ENI or SG"
  }

  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.ziti_edge_web) == 1 && length(aws_route53_record.public_edge) == 2
    error_message = "turning ziti off must keep the 443 rule and both public_edge records"
  }

  assert {
    condition = (
      length(aws_vpc_security_group_ingress_rule.ziti_edge_public) == 0 &&
      length(aws_route53_record.ziti_edge) == 0 &&
      length(aws_vpc_security_group_ingress_rule.ziti_router) == 0
    )
    error_message = "world 3022 rule, ziti.clonk.tv record and VPC 3022 rule must be gone"
  }
}

# --- (c) everything off -------------------------------------------------------
run "all_flags_off_creates_none" {
  command = plan

  variables {
    edge_node_enabled          = false
    ziti_public_router_enabled = false
    ziti_router_vpc_enabled    = false
  }

  assert {
    condition = (
      length(aws_instance.ziti_edge) == 0 &&
      length(aws_eip.ziti_edge) == 0 &&
      length(aws_eip_association.ziti_edge) == 0 &&
      length(aws_network_interface.ziti_edge) == 0 &&
      length(aws_security_group.ziti_edge) == 0 &&
      length(aws_vpc_security_group_ingress_rule.ziti_edge_web) == 0 &&
      length(aws_route53_record.public_edge) == 0 &&
      length(aws_vpc_security_group_ingress_rule.ziti_edge_public) == 0 &&
      length(aws_route53_record.ziti_edge) == 0 &&
      length(aws_vpc_security_group_ingress_rule.ziti_router) == 0
    )
    error_message = "no edge or ziti resource may exist with every flag off"
  }
}

# --- (d) the public router lives on the edge node -----------------------------
run "public_router_without_edge_node_is_rejected" {
  command = plan

  variables {
    edge_node_enabled          = false
    ziti_public_router_enabled = true
  }

  expect_failures = [var.ziti_public_router_enabled]
}

# --- (e) every node registers its zone label (cost plan B4, gd-vMCMziHy7p) ----
# The AWS CCM runs node-lifecycle only, so nothing else sets
# topology.kubernetes.io/zone and zone affinity would match no node. Asserted
# on the rendered template, not the launch template's user_data, so it holds
# whether that is plain base64 or base64gzip.
run "nodes_label_their_zone" {
  command = plan

  assert {
    condition     = strcontains(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "agent", extra_args = "" })), "--node-label topology.kubernetes.io/zone=$AZ")
    error_message = "every node must register with its zone label (cost plan B4)"
  }
}
