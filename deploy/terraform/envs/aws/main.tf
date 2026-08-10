# A real AWS deployment of the indexer.
#
# It applies the same module as the LocalStack stack, so what is exercised in
# the end-to-end tests is what runs here. The differences are all in this file:
# real credentials, a real region, and a Secrets Manager entry holding the
# OpenPGP signing key.

terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source = "hashicorp/aws"
      # Unpinned, unlike the LocalStack stack: this talks to real AWS, which
      # does not have the DynamoDB polling quirk that forces v5 there.
      version = ">= 5.0"
    }
  }
}

provider "aws" {
  region  = var.region
  profile = var.profile

  default_tags {
    tags = var.tags
  }
}

# The signing key lives in Secrets Manager, not in Terraform state: the value is
# written out of band so the private key never passes through a plan, a state
# file, or a terminal that logs its scrollback.
#
# recovery_window_in_days is left at the default 30, so a destroy schedules the
# secret for deletion rather than removing it. Reusing the same name inside that
# window fails until it is force-deleted.
resource "aws_secretsmanager_secret" "signing" {
  name        = "${var.name_prefix}/gpg-signing-key"
  description = "OpenPGP private key the publisher signs Release and repomd.xml with."
}

module "indexer" {
  source = "../../modules/indexer"

  name_prefix = var.name_prefix
  bucket_name = var.bucket_name

  ingest_zip  = var.ingest_zip
  publish_zip = var.publish_zip

  signing_secret_arn = aws_secretsmanager_secret.signing.arn

  # Serving the bucket directly makes it world-readable, which is what a public
  # package repository is. Front it with CloudFront and turn this off for
  # anything that is not a test deployment.
  public_read_policy = var.public_read_policy

  publish_delay_seconds = var.publish_delay_seconds
  sweep_schedule        = var.sweep_schedule
  log_level             = var.log_level
}
