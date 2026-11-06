# ses-identity — an SES sending domain with its DNS, plus the SMTP IAM user that
# Keycloak signs in with (gd-uBGCzwjlIh; deferred from gd-o4fbw9qUJc, needed by the
# realm SMTP rollout gd-ygZDTEs3Qt).
#
# WHAT IT CREATES, and why each piece:
#   - aws_sesv2_email_identity with Easy DKIM (2048-bit) and its three CNAMEs. A
#     DKIM-signed message on the From domain itself is what makes DMARC pass.
#   - A custom MAIL FROM, bounce.<domain>: an MX to SES's feedback endpoint plus an SPF
#     TXT. Without it the envelope sender is amazonses.com, so SPF never aligns with the
#     From domain and DMARC rests on DKIM alone.
#   - A DMARC TXT at _dmarc.<domain>.
#   - aws_iam_user with ONE inline policy: ses:SendRawEmail on this identity only,
#     conditioned on a single From address. SES SMTP auth takes an access key, never a
#     role, so this has to be a user.
#
# WHAT IT DELIBERATELY DOES NOT CREATE:
#   - aws_iam_access_key. Its secret (and ses_smtp_password_v4) would sit in terraform
#     state. The key is minted out of band and the SMTP password derived from it (README).
#   - SES production access. That is an account-level AWS support request made by the
#     owner, not an API terraform can drive. Until it is granted the account is in the
#     sandbox and mail reaches only addresses verified in SES.
#   - A configuration set or bounce/complaint SNS (gd-o4fbw9qUJc item 1). support@
#     inbound (item 3) is modules/ses-support-inbound. If a DEFAULT configuration set is later attached to this identity,
#     SendRawEmail is also authorized against the configuration-set ARN, so the policy
#     below must name it too or every send is AccessDenied.

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  region           = data.aws_region.current.name
  mail_from_domain = "${var.mail_from_subdomain}.${var.domain}"

  # Built rather than read from aws_sesv2_email_identity.this.arn so the policy is
  # known at plan time and can be reviewed (and tested) before apply. IAM does not
  # check that a Resource exists, so there is no ordering to preserve.
  identity_arn = "arn:${data.aws_partition.current.partition}:ses:${local.region}:${data.aws_caller_identity.current.account_id}:identity/${var.domain}"

  dmarc_record = join("; ", compact([
    "v=DMARC1",
    "p=${var.dmarc_policy}",
    var.dmarc_rua == "" ? "" : "rua=mailto:${var.dmarc_rua}",
  ]))
}

resource "aws_sesv2_email_identity" "this" {
  email_identity = var.domain

  dkim_signing_attributes {
    next_signing_key_length = "RSA_2048_BIT"
  }

  tags = var.tags
}

# Easy DKIM always issues exactly three tokens, so count is a constant and the plan
# does not depend on the not-yet-known token values.
resource "aws_route53_record" "dkim" {
  count = 3

  zone_id = var.zone_id
  name    = "${aws_sesv2_email_identity.this.dkim_signing_attributes[0].tokens[count.index]}._domainkey.${var.domain}"
  type    = "CNAME"
  ttl     = 1800
  records = ["${aws_sesv2_email_identity.this.dkim_signing_attributes[0].tokens[count.index]}.dkim.amazonses.com"]
}

resource "aws_sesv2_email_identity_mail_from_attributes" "this" {
  email_identity   = aws_sesv2_email_identity.this.email_identity
  mail_from_domain = local.mail_from_domain
  # If the MX ever goes missing, fall back to amazonses.com rather than reject sends.
  behavior_on_mx_failure = "USE_DEFAULT_VALUE"
}

resource "aws_route53_record" "mail_from_mx" {
  zone_id = var.zone_id
  name    = local.mail_from_domain
  type    = "MX"
  ttl     = 1800
  records = ["10 feedback-smtp.${local.region}.amazonses.com"]
}

resource "aws_route53_record" "mail_from_spf" {
  zone_id = var.zone_id
  name    = local.mail_from_domain
  type    = "TXT"
  ttl     = 1800
  records = ["v=spf1 include:amazonses.com ~all"]
}

resource "aws_route53_record" "dmarc" {
  zone_id = var.zone_id
  name    = "_dmarc.${var.domain}"
  type    = "TXT"
  ttl     = 1800
  records = [local.dmarc_record]
}

# No aws_iam_access_key: see the header.
resource "aws_iam_user" "smtp" {
  name = var.smtp_user_name
  path = "/gardenio/"
  tags = merge(var.tags, {
    "gardenio:purpose" = "ses-smtp ${var.smtp_from_address}"
  })
}

resource "aws_iam_user_policy" "smtp_send" {
  name = "ses-send-raw-${var.domain}"
  user = aws_iam_user.smtp.name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "SendAsOneAddressOnOneIdentity"
      Effect   = "Allow"
      Action   = "ses:SendRawEmail"
      Resource = local.identity_arn
      Condition = {
        StringEquals = { "ses:FromAddress" = var.smtp_from_address }
      }
    }]
  })

  lifecycle {
    precondition {
      condition     = endswith(var.smtp_from_address, "@${var.domain}")
      error_message = "smtp_from_address must be an address on domain, or every send is AccessDenied."
    }
  }
}
