# Bootstrap composition.
#
# Creates the state-backend resources every other composition needs
# before it can run. Runs against a local backend on first apply; state
# is migrated into the bucket this composition provisions afterwards.
#
# Scope (POC, tier (a) per gardenio/plans/canonical-prod-aws-poc.md §5):
#   - S3 state bucket (versioned, SSE-S3, all public access blocked)
#   - DynamoDB lock table (PAY_PER_REQUEST, hash key LockID)
#
# Deferred to a follow-up MR (see README "Out of scope"):
#   - GitLab OIDC provider + IAM trust roles (need CI assume-role pattern
#     first)

# Live account id, so the `gardenio:account` tag reflects whatever account
# this bootstrap actually runs against instead of a hardcoded org literal.
data "aws_caller_identity" "current" {}

locals {
  module_tags = merge(
    var.common_tags,
    {
      "gardenio:module"  = "bootstrap"
      "gardenio:account" = data.aws_caller_identity.current.account_id
    },
  )
}

# -----------------------------------------------------------------------
# State bucket
# -----------------------------------------------------------------------

resource "aws_s3_bucket" "state" {
  bucket = var.state_bucket_name

  tags = local.module_tags
}

resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket = aws_s3_bucket.state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# -----------------------------------------------------------------------
# Lock table
# -----------------------------------------------------------------------

resource "aws_dynamodb_table" "lock" {
  name         = var.lock_table_name
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "LockID"

  attribute {
    name = "LockID"
    type = "S"
  }

  tags = local.module_tags
}
