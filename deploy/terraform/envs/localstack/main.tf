# The stack the end-to-end tests provision.
#
# It applies the same module as production, so the modules are genuinely
# exercised rather than approximated by a hand-built test fixture. Only the
# endpoints, the credentials and a handful of safety settings differ.

terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

variable "endpoint" {
  description = "LocalStack endpoint."
  type        = string
  default     = "http://localhost:4566"
}

variable "bucket_name" {
  type    = string
  default = "linux-repo-indexer-test"
}

variable "name_prefix" {
  type    = string
  default = "lri-test"
}

variable "ingest_zip" {
  type    = string
  default = "../../../../dist/ingest.zip"
}

variable "publish_zip" {
  type    = string
  default = "../../../../dist/publish.zip"
}

provider "aws" {
  region                      = "us-east-1"
  access_key                  = "test"
  secret_key                  = "test"
  s3_use_path_style           = true
  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true

  endpoints {
    s3             = var.endpoint
    dynamodb       = var.endpoint
    sqs            = var.endpoint
    lambda         = var.endpoint
    iam            = var.endpoint
    sts            = var.endpoint
    secretsmanager = var.endpoint
    cloudwatch     = var.endpoint
    events         = var.endpoint
    logs           = var.endpoint
  }
}

# The tests write the signing key into this secret before publishing.
resource "aws_secretsmanager_secret" "signing" {
  name = "${var.name_prefix}/gpg-signing-key"
}

module "indexer" {
  source = "../../modules/indexer"

  name_prefix = var.name_prefix
  bucket_name = var.bucket_name

  ingest_zip  = var.ingest_zip
  publish_zip = var.publish_zip

  signing_secret_arn = aws_secretsmanager_secret.signing.arn

  # No coalescing delay: the tests assert on the published output and should
  # not sit through the production window to do it.
  publish_delay_seconds = 0

  # The containers install packages straight from the bucket over HTTP.
  public_read_policy = true

  # LocalStack's community edition does not implement these.
  versioning_enabled     = false
  point_in_time_recovery = false

  log_level = "debug"
}

output "bucket" { value = module.indexer.bucket }
output "packages_table" { value = module.indexer.packages_table }
output "state_table" { value = module.indexer.state_table }
output "ingest_queue_url" { value = module.indexer.ingest_queue_url }
output "publish_queue_url" { value = module.indexer.publish_queue_url }
output "ingest_dlq_url" { value = module.indexer.ingest_dlq_url }
output "publish_dlq_url" { value = module.indexer.publish_dlq_url }
output "publish_function" { value = module.indexer.publish_function }
output "ingest_function" { value = module.indexer.ingest_function }
output "signing_secret_id" { value = aws_secretsmanager_secret.signing.name }
