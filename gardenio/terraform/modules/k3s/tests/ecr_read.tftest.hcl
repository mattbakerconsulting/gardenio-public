# The node role may PULL gardenio/* (images CI pushes to ECR, plan
# 2026-10-21-aws-cost-cuts Task A2) but never push, create or import there.
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

run "read_grant_is_pull_only" {
  command = plan
  variables {
    ecr_read_repo_arns = ["arn:aws:ecr:us-east-1:111111111111:repository/gardenio/*"]
  }
  assert {
    condition = anytrue([
      for s in data.aws_iam_policy_document.node.statement :
      s.sid == "EcrReadGardenio" &&
      toset(s.actions) == toset(["ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability"])
    ])
    error_message = "EcrReadGardenio must exist and carry exactly the three pull actions"
  }
  assert {
    condition = anytrue([
      for s in data.aws_iam_policy_document.node.statement : s.sid == "EcrAuth"
    ])
    error_message = "EcrAuth must exist when only ecr_read_repo_arns is set"
  }
}

run "no_read_grant_by_default" {
  command = plan
  assert {
    condition = !anytrue([
      for s in data.aws_iam_policy_document.node.statement : s.sid == "EcrReadGardenio"
    ])
    error_message = "EcrReadGardenio must not exist when ecr_read_repo_arns is empty"
  }
}
