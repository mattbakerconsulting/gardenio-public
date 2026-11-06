# Native terraform tests for the SES sending domain and its SMTP user (gd-uBGCzwjlIh).
#
# Properties locked down:
#   - the SMTP user's ONE inline policy allows exactly ses:SendRawEmail, on exactly this
#     identity's ARN, only when the From address is the configured one (no wildcard
#     action, resource or From);
#   - DKIM is 2048-bit with three CNAMEs, MAIL FROM is bounce.<domain> with an MX to the
#     region's feedback endpoint and an SPF TXT, and DMARC is published;
#   - rua appears only when a mailbox is given, and a From address off the domain or a
#     bad DMARC policy is rejected.
#
# Run: terraform test   (from modules/ses-identity/)

mock_provider "aws" {
  override_data {
    target = data.aws_caller_identity.current
    values = { account_id = "123456789012" }
  }
  override_data {
    target = data.aws_region.current
    values = { name = "us-east-1" }
  }
  override_data {
    target = data.aws_partition.current
    values = { partition = "aws" }
  }
}

variables {
  domain            = "lands.clonk.tv"
  zone_id           = "Z0000000000000000000"
  smtp_user_name    = "gardenio-keycloak-smtp"
  smtp_from_address = "lands@lands.clonk.tv"
}

run "smtp_policy_is_one_action_one_identity_one_from" {
  command = plan

  assert {
    condition     = aws_iam_user.smtp.name == "gardenio-keycloak-smtp"
    error_message = "the SMTP user must be named gardenio-keycloak-smtp"
  }

  assert {
    condition     = length(jsondecode(aws_iam_user_policy.smtp_send.policy).Statement) == 1
    error_message = "the SMTP policy must have exactly one statement"
  }

  assert {
    condition     = jsondecode(aws_iam_user_policy.smtp_send.policy).Statement[0].Effect == "Allow" && jsondecode(aws_iam_user_policy.smtp_send.policy).Statement[0].Action == "ses:SendRawEmail"
    error_message = "the SMTP policy must allow ses:SendRawEmail and nothing else"
  }

  assert {
    condition     = jsondecode(aws_iam_user_policy.smtp_send.policy).Statement[0].Resource == "arn:aws:ses:us-east-1:123456789012:identity/lands.clonk.tv"
    error_message = "the SMTP policy must name exactly the lands.clonk.tv identity ARN"
  }

  assert {
    condition     = jsondecode(aws_iam_user_policy.smtp_send.policy).Statement[0].Condition == { StringEquals = { "ses:FromAddress" = "lands@lands.clonk.tv" } }
    error_message = "the SMTP policy must be conditioned on ses:FromAddress = lands@lands.clonk.tv, and only that"
  }

  assert {
    condition     = !strcontains(aws_iam_user_policy.smtp_send.policy, "*")
    error_message = "the SMTP policy must contain no wildcard"
  }
}

run "dns_records" {
  command = plan

  assert {
    condition     = aws_sesv2_email_identity.this.email_identity == "lands.clonk.tv" && aws_sesv2_email_identity.this.dkim_signing_attributes[0].next_signing_key_length == "RSA_2048_BIT"
    error_message = "the identity must be lands.clonk.tv with 2048-bit Easy DKIM"
  }

  assert {
    condition     = length(aws_route53_record.dkim) == 3 && alltrue([for r in aws_route53_record.dkim : r.type == "CNAME"])
    error_message = "Easy DKIM needs three CNAMEs"
  }

  assert {
    condition     = aws_sesv2_email_identity_mail_from_attributes.this.mail_from_domain == "bounce.lands.clonk.tv"
    error_message = "MAIL FROM must be bounce.lands.clonk.tv"
  }

  assert {
    condition     = aws_route53_record.mail_from_mx.name == "bounce.lands.clonk.tv" && aws_route53_record.mail_from_mx.records == toset(["10 feedback-smtp.us-east-1.amazonses.com"])
    error_message = "the MAIL FROM MX must point at the region's SES feedback endpoint"
  }

  assert {
    condition     = aws_route53_record.mail_from_spf.name == "bounce.lands.clonk.tv" && aws_route53_record.mail_from_spf.records == toset(["v=spf1 include:amazonses.com ~all"])
    error_message = "the MAIL FROM domain must publish the SES SPF record"
  }

  assert {
    condition     = aws_route53_record.dmarc.name == "_dmarc.lands.clonk.tv" && aws_route53_record.dmarc.records == toset(["v=DMARC1; p=quarantine"])
    error_message = "DMARC must default to p=quarantine with no rua"
  }
}

run "dmarc_rua_when_given" {
  command = plan

  variables {
    dmarc_rua = "dmarc@example.com"
  }

  assert {
    condition     = aws_route53_record.dmarc.records == toset(["v=DMARC1; p=quarantine; rua=mailto:dmarc@example.com"])
    error_message = "a dmarc_rua mailbox must appear as rua=mailto:"
  }
}

run "rejects_from_address_off_the_domain" {
  command = plan

  variables {
    smtp_from_address = "lands@clonk.tv"
  }

  expect_failures = [aws_iam_user_policy.smtp_send]
}

run "rejects_bad_dmarc_policy" {
  command = plan

  variables {
    dmarc_policy = "monitor"
  }

  expect_failures = [var.dmarc_policy]
}
