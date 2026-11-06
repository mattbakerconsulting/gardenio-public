# Gas City HQ host: one on-demand EC2 instance that runs the machine-wide
# `gc supervisor` (127.0.0.1:8372), the dogfood city and its tmux agent
# sessions. Placement A from hq/plans/hq-on-aws.md §2.2: public subnet, public
# IP, a security group with NO ingress -- ziti (outbound dial to the Vultr
# controller/router) and SSM Session Manager are the only doors.
#
# Scale-down: the host powers itself off after 30 idle minutes (hq-idle-stop
# timer, installed by user-data) and instance_initiated_shutdown_behavior=stop
# turns that into an EC2 stop, so only the EBS root bills until the next
# `scripts/hq start`. Resizing is a change to var.instance_type.

locals {
  name = "${var.env_name}-hq"
  tags = merge(var.common_tags, { "gardenio:component" = "hq", Project = "gascity-hq" })
}

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

# ── Security group: zero ingress, all egress ────────────────────────────
resource "aws_security_group" "hq" {
  name        = local.name
  description = "Gas City HQ host: no ingress; ziti and SSM are outbound dials"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { Name = local.name })
}

resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.hq.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
  description       = "apt, GitHub, Anthropic, SSM, ziti controller/router on Vultr"
}

# ── Instance role: SSM Session Manager + read of /<env>/hq/* parameters ──
# Deliberately not the k3s node profile (Route53 + ASG rights).
data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "hq" {
  name               = local.name
  assume_role_policy = data.aws_iam_policy_document.assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.hq.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# One-shot secrets (ziti enrolment JWT, Claude Code credentials) are staged as
# SecureStrings under /<env>/hq/ and deleted once the host has consumed them.
# The aws/ssm managed key needs no extra kms grant for same-account readers.
data "aws_iam_policy_document" "hq" {
  statement {
    sid     = "HQParams"
    actions = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
    resources = [
      "arn:aws:ssm:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:parameter/${var.env_name}/hq/*",
      # root password of the shared beads Dolt server on k3s (helm/beads);
      # the city reads it into .beads/.env at cutover.
      "arn:aws:ssm:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:parameter/${var.env_name}/beads/dolt-root",
      # k3s admin kubeconfig. The gc k8s session runtime drives the cluster
      # from this host through client-go, so it needs the API credentials the
      # same way it needs the Dolt password. Read-only; the parameter is
      # written by the k3s module's server bootstrap.
      "arn:aws:ssm:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:parameter/${var.env_name}/k3s/kubeconfig",
    ]
  }

  # gc-dolt-backup mirrors /home/gc/backups/dolt into s3://<bucket>/dolt/ with
  # `aws s3 sync --delete`. Delete here is a delete marker (versioning) that
  # the lifecycle keeps 30 days; no DeleteObjectVersion, no bucket admin.
  statement {
    sid       = "BackupBucketList"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.backups.arn]
  }
  statement {
    sid       = "BackupObjects"
    actions   = ["s3:PutObject", "s3:GetObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = ["${aws_s3_bucket.backups.arn}/dolt/*"]
  }
}

resource "aws_iam_role_policy" "hq" {
  name   = "${local.name}-params"
  role   = aws_iam_role.hq.id
  policy = data.aws_iam_policy_document.hq.json
}

resource "aws_iam_instance_profile" "hq" {
  name = local.name
  role = aws_iam_role.hq.name
  tags = local.tags
}

# ── The host ────────────────────────────────────────────────────────────
resource "aws_instance" "hq" {
  ami                         = var.ami_id
  instance_type               = var.instance_type
  subnet_id                   = var.subnet_id
  associate_public_ip_address = true
  vpc_security_group_ids      = [aws_security_group.hq.id]
  iam_instance_profile        = aws_iam_instance_profile.hq.name
  ebs_optimized               = true
  monitoring                  = false

  # An OS `systemctl poweroff` (hq-idle-stop) becomes an EC2 stop, not a
  # terminate; the root volume and everything on it persist.
  instance_initiated_shutdown_behavior = "stop"
  disable_api_termination              = true

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required" # IMDSv2 only
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.root_volume_gb
    encrypted             = true
    delete_on_termination = true
  }

  user_data_base64 = base64gzip(templatefile("${path.module}/templates/user-data.sh.tftpl", {
    env_name            = var.env_name
    gc_user             = var.gc_user
    go_version          = var.go_version
    node_major          = var.node_major
    dolt_version        = var.dolt_version
    bd_version          = var.bd_version
    gc_commit           = var.gc_commit
    gc_repo_url         = var.gc_repo_url
    ziti_tunnel_version = var.ziti_tunnel_version
    ziti_identity_name  = var.ziti_identity_name
    bootstrap_script    = file("${path.module}/files/hq-bootstrap.sh")
  }))

  tags = merge(local.tags, { Name = local.name })

  lifecycle {
    # The AMI parameter tracks "current" and user-data is first-boot only:
    # neither may replace a host whose root volume holds the city. Rebuild on
    # purpose with `terraform apply -replace=module.hq.aws_instance.hq` (after
    # removing prevent_destroy and disable_api_termination, and taking a
    # snapshot).
    #
    # associate_public_ip_address is here for a subtler reason: EC2 RELEASES
    # the public IP when an instance stops, so the attribute reads back false
    # on a stopped host while this config says true -- and that difference
    # "forces replacement". Since hq-idle-stop powers this host off after 30
    # idle minutes by design, ANY apply while it is stopped would otherwise
    # destroy and recreate it, taking the city, Dolt and beads with the root
    # volume. Observed for real on 2026-09-18, the first time the host stopped.
    ignore_changes = [ami, user_data_base64, associate_public_ip_address]

    # Last line of defence for the same reason: this host's root volume is the
    # city. disable_api_termination stops an AWS-side terminate; this stops a
    # terraform-side one. Remove deliberately to rebuild.
    prevent_destroy = true
  }
}

# ── Daily root-volume snapshots via DLM, retain 7 ───────────────────────
# Crash-consistent; the consistent copy of Dolt is gc-dolt-backup (below:
# DOLT_BACKUP via the running server, mirrored to the S3 bucket).
data "aws_iam_policy_document" "dlm_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["dlm.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "dlm" {
  name               = "${local.name}-dlm"
  assume_role_policy = data.aws_iam_policy_document.dlm_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "dlm" {
  role       = aws_iam_role.dlm.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSDataLifecycleManagerServiceRole"
}

resource "aws_dlm_lifecycle_policy" "daily" {
  description        = "${local.name} daily snapshot keep ${var.snapshot_retain_count}"
  execution_role_arn = aws_iam_role.dlm.arn
  state              = "ENABLED"
  tags               = local.tags

  policy_details {
    resource_types = ["INSTANCE"]
    target_tags    = { Name = local.name }

    schedule {
      name      = "daily"
      copy_tags = true
      create_rule {
        interval      = 24
        interval_unit = "HOURS"
        times         = ["03:00"] # UTC quiet hour
      }
      retain_rule { count = var.snapshot_retain_count }
      tags_to_add = { "gardenio:backup" = "dlm-daily" }
    }
  }
}

# ── Dolt backups: S3 mirror of /home/gc/backups/dolt ────────────────────
# gc-dolt-backup (host, daily 02:30 UTC + before every idle poweroff) runs
# `CALL DOLT_BACKUP('sync', ...)` per database against the city's managed
# Dolt server, then `aws s3 sync --delete` into dolt/. Versioned so a bad
# sync is recoverable for 30 days; dolt/ objects expire after 90 days (the
# daily sync re-uploads any chunk file the expiry removed).
resource "aws_s3_bucket" "backups" {
  bucket = "${local.name}-backups-${data.aws_caller_identity.current.account_id}"
  tags   = merge(local.tags, { Name = "${local.name}-backups", "gardenio:backup" = "dolt" })
}

resource "aws_s3_bucket_versioning" "backups" {
  bucket = aws_s3_bucket.backups.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "backups" {
  bucket                  = aws_s3_bucket.backups.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "backups" {
  bucket     = aws_s3_bucket.backups.id
  depends_on = [aws_s3_bucket_versioning.backups]

  rule {
    id     = "noncurrent-30d"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  rule {
    id     = "dolt-90d"
    status = "Enabled"
    filter {
      prefix = "dolt/"
    }
    expiration {
      days = 90
    }
  }

  # Same policy for the k3s beads server's backups (helm/beads dolt-backup).
  rule {
    id     = "k3s-dolt-90d"
    status = "Enabled"
    filter {
      prefix = "k3s-dolt/"
    }
    expiration {
      days = 90
    }
  }

  # The gardenio Postgres (beads store) off-node backup (gd-vekJWxnNGG).
  # 30d comfortably clears the task's ">= 7 days" retention floor.
  rule {
    id     = "k3s-postgres-30d"
    status = "Enabled"
    filter {
      prefix = "k3s-postgres/"
    }
    expiration {
      days = 30
    }
  }
}
