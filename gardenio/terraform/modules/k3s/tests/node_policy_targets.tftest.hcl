# The node role policy must be appliable on its own with
# -target=module.k3s.aws_iam_role_policy.node, without pulling in a launch
# template (gd-VAyugfDaI5). A launch template in that closure rides along with
# whatever image_id drift the SSM AMI parameter has, which rolls an untested
# AMI onto every new node. So the autoscaler's scale grant names its ASGs by
# ARN pattern rather than by aws_autoscaling_group.*.arn: an ASG depends on
# its launch template. With every provider mocked, an ARN read from an ASG is
# unknown at plan time, so the assertion below also fails if a resource
# reference comes back.
# Run: terraform test   (from modules/k3s/)

mock_provider "aws" {
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }
  mock_data "aws_region" {
    defaults = { name = "us-east-1" }
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

run "autoscaler_scale_grant_names_its_asgs" {
  command = plan
  assert {
    condition = anytrue([
      for s in data.aws_iam_policy_document.node.statement :
      s.sid == "AutoscalerScale" && toset(s.resources) == toset([
        "arn:aws:autoscaling:us-east-1:111111111111:autoScalingGroup:*:autoScalingGroupName/test-k3s-ci",
        "arn:aws:autoscaling:us-east-1:111111111111:autoScalingGroup:*:autoScalingGroupName/test-k3s-agents-spot",
      ])
    ])
    error_message = "AutoscalerScale must name exactly the ci and agents-spot ASGs by ARN pattern, not by resource reference"
  }
}

