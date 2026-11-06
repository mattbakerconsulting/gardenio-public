# The ECR CI role must trust the OIDC subject GitHub really issues. This repo
# has use_immutable_subject: true, so every sub starts with the owner and repo
# IDs, not the names (gd-VAyugfDaI5; see local.github_oidc_sub_prefix in
# ci_ecr.tf). A name-based sub never matches and STS rejects the token.
# Run: terraform init -backend=false && terraform test   (from composition/gardenio/)

mock_provider "aws" {
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }
}
mock_provider "random" {}
mock_provider "archive" {}

# Every module call is overridden: this test is about ci_ecr.tf, the modules
# have their own tests, and a module planned under mocks breaks this file
# whenever it gains a count or an ARN check. Add a new module call here too.
override_module {
  target = module.network
}
override_module {
  target = module.finops
}
override_module {
  target = module.k3s
}
override_module {
  target = module.edge_probes
}
override_module {
  target = module.hq
}
override_module {
  target = module.infra_aws_readonly
}
override_module {
  target = module.ses_lands
}
override_module {
  target = module.ses_support_inbound
}
override_module {
  target = module.ecr_pull_through_cache
}

variables {
  edge_probe_alert_email = "alerts@example.invalid"
}

run "trust_policies_use_the_immutable_subject" {
  command = plan

  assert {
    condition = toset(flatten([
      for c in data.aws_iam_policy_document.ci_ecr_trust_release.statement[0].condition :
      c.values if c.variable == "token.actions.githubusercontent.com:sub"
      ])) == toset([
      "repo:mattbakerconsulting@334111061/gardenio@1360966138:ref:refs/tags/20*",
      "repo:mattbakerconsulting@334111061/gardenio@1360966138:ref:refs/heads/main",
      "repo:mattbakerconsulting@334111061/gardenio@1360966138:ref:refs/heads/agent/*",
    ])
    error_message = "gardenio-ci-ecr-release must trust exactly the immutable calver-tag, main and agent/* branch subjects"
  }
}
