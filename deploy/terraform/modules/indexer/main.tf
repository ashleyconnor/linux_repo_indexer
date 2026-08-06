terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

locals {
  name = var.name_prefix

  # Prefixes that hold packages. The ingest Lambda classifies keys against
  # repos.yaml, but filtering here keeps index writes from generating events
  # that would only be classified and discarded.
  package_prefixes = var.package_prefixes
}

# --- storage -----------------------------------------------------------------

resource "aws_s3_bucket" "repo" {
  bucket = var.bucket_name
}

# Versioning is what makes an accidental delete or overwrite recoverable. The
# index can always be regenerated from the database, but a package object is
# the only copy of itself.
#
# The resource is omitted rather than set to Suspended when disabled, because
# LocalStack's community edition does not implement enough of the API for the
# provider to reconcile it.
resource "aws_s3_bucket_versioning" "repo" {
  count  = var.versioning_enabled ? 1 : 0
  bucket = aws_s3_bucket.repo.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_public_access_block" "repo" {
  bucket = aws_s3_bucket.repo.id

  block_public_acls       = true
  block_public_policy     = var.public_read_policy ? false : true
  ignore_public_acls      = true
  restrict_public_buckets = var.public_read_policy ? false : true
}

# A package repository is served over plain HTTP GET. In production this is
# normally fronted by CloudFront rather than exposed directly; the flag exists
# so a test stack can serve the bucket without one.
resource "aws_s3_bucket_policy" "public_read" {
  count  = var.public_read_policy ? 1 : 0
  bucket = aws_s3_bucket.repo.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "PublicRead"
      Effect    = "Allow"
      Principal = "*"
      Action    = "s3:GetObject"
      Resource  = "${aws_s3_bucket.repo.arn}/*"
    }]
  })

  depends_on = [aws_s3_bucket_public_access_block.repo]
}

resource "aws_dynamodb_table" "packages" {
  name         = "${local.name}-packages"
  billing_mode = "PAY_PER_REQUEST"

  # One row per object. The sort key is the path the index records, not the
  # package's name-version-architecture: an ObjectRemoved event carries only
  # the S3 key, and an RPM's epoch never appears in its filename.
  hash_key  = "scope"
  range_key = "filename"

  attribute {
    name = "scope"
    type = "S"
  }
  attribute {
    name = "filename"
    type = "S"
  }

  # Omitted entirely when disabled: the provider reconciles this by polling
  # DescribeContinuousBackups, which LocalStack's community edition does not
  # implement, and the apply then hangs until it gives up.
  dynamic "point_in_time_recovery" {
    for_each = var.point_in_time_recovery ? [1] : []
    content {
      enabled = true
    }
  }
}

resource "aws_dynamodb_table" "state" {
  name         = "${local.name}-state"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "scope"

  attribute {
    name = "scope"
    type = "S"
  }

  # Omitted entirely when disabled: the provider reconciles this by polling
  # DescribeContinuousBackups, which LocalStack's community edition does not
  # implement, and the apply then hangs until it gives up.
  dynamic "point_in_time_recovery" {
    for_each = var.point_in_time_recovery ? [1] : []
    content {
      enabled = true
    }
  }
}

# --- queues ------------------------------------------------------------------

resource "aws_sqs_queue" "ingest_dlq" {
  name                      = "${local.name}-ingest-dlq"
  message_retention_seconds = 1209600 # 14 days, the maximum
}

resource "aws_sqs_queue" "ingest" {
  name                       = "${local.name}-ingest"
  visibility_timeout_seconds = var.ingest_timeout * 6
  message_retention_seconds  = 345600

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.ingest_dlq.arn
    maxReceiveCount     = 5
  })
}

resource "aws_sqs_queue_policy" "ingest" {
  queue_url = aws_sqs_queue.ingest.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "s3.amazonaws.com" }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.ingest.arn
      Condition = {
        ArnLike = { "aws:SourceArn" = aws_s3_bucket.repo.arn }
      }
    }]
  })
}

resource "aws_sqs_queue" "publish_dlq" {
  name                      = "${local.name}-publish-dlq"
  message_retention_seconds = 1209600
}

resource "aws_sqs_queue" "publish" {
  name                       = "${local.name}-publish"
  visibility_timeout_seconds = var.publish_timeout * 6
  message_retention_seconds  = 345600

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.publish_dlq.arn
    maxReceiveCount     = 5
  })
}

resource "aws_s3_bucket_notification" "ingest" {
  bucket = aws_s3_bucket.repo.id

  dynamic "queue" {
    for_each = local.package_prefixes
    content {
      queue_arn     = aws_sqs_queue.ingest.arn
      events        = ["s3:ObjectCreated:*", "s3:ObjectRemoved:*"]
      filter_prefix = queue.value
    }
  }

  depends_on = [aws_sqs_queue_policy.ingest]
}
