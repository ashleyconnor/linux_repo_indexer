output "bucket" {
  description = "Name of the repository bucket."
  value       = aws_s3_bucket.repo.id
}

output "bucket_arn" {
  value = aws_s3_bucket.repo.arn
}

output "packages_table" {
  value = aws_dynamodb_table.packages.name
}

output "state_table" {
  value = aws_dynamodb_table.state.name
}

output "ingest_queue_url" {
  value = aws_sqs_queue.ingest.id
}

output "publish_queue_url" {
  value = aws_sqs_queue.publish.id
}

output "ingest_dlq_url" {
  description = "Messages here are packages that failed to index; the repository is missing them."
  value       = aws_sqs_queue.ingest_dlq.id
}

output "publish_dlq_url" {
  value = aws_sqs_queue.publish_dlq.id
}

output "ingest_function" {
  value = aws_lambda_function.ingest.function_name
}

output "publish_function" {
  description = "Invoke directly to force a rebuild without waiting for the sweep."
  value       = aws_lambda_function.publish.function_name
}
