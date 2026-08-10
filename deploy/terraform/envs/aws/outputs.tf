output "bucket" {
  value = module.indexer.bucket
}

output "base_url" {
  description = "What apt and dnf point at when public_read_policy is on."
  value       = "https://${module.indexer.bucket}.s3.${var.region}.amazonaws.com"
}

output "packages_table" {
  value = module.indexer.packages_table
}

output "state_table" {
  value = module.indexer.state_table
}

output "publish_function" {
  description = "Invoke directly to force a rebuild without waiting for the sweep."
  value       = module.indexer.publish_function
}

output "ingest_function" {
  value = module.indexer.ingest_function
}

output "ingest_dlq_url" {
  description = "Messages here are packages that failed to index; the repository is missing them."
  value       = module.indexer.ingest_dlq_url
}

output "publish_dlq_url" {
  value = module.indexer.publish_dlq_url
}

output "signing_secret_id" {
  description = "Write the armored private key here before publishing."
  value       = aws_secretsmanager_secret.signing.name
}
