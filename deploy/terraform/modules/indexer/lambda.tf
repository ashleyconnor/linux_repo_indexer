# --- IAM ---------------------------------------------------------------------

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "indexer" {
  statement {
    sid = "ReadPackagesAndConfig"
    actions = [
      "s3:GetObject",
      "s3:GetObjectAttributes",
    ]
    resources = ["${aws_s3_bucket.repo.arn}/*"]
  }

  statement {
    sid = "WriteIndexes"
    actions = [
      "s3:PutObject",
      "s3:DeleteObject",
    ]
    resources = ["${aws_s3_bucket.repo.arn}/*"]
  }

  statement {
    sid       = "ListForPruning"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.repo.arn]
  }

  statement {
    sid = "Metadata"
    actions = [
      "dynamodb:PutItem",
      "dynamodb:GetItem",
      "dynamodb:DeleteItem",
      "dynamodb:UpdateItem",
      "dynamodb:Query",
    ]
    resources = [
      aws_dynamodb_table.packages.arn,
      aws_dynamodb_table.state.arn,
    ]
  }

  statement {
    sid = "Queues"
    actions = [
      "sqs:ReceiveMessage",
      "sqs:DeleteMessage",
      "sqs:GetQueueAttributes",
      "sqs:SendMessage",
    ]
    resources = [
      aws_sqs_queue.ingest.arn,
      aws_sqs_queue.publish.arn,
    ]
  }

  dynamic "statement" {
    for_each = var.signing_secret_arn == "" ? [] : [var.signing_secret_arn]
    content {
      sid       = "SigningKey"
      actions   = ["secretsmanager:GetSecretValue"]
      resources = [statement.value]
    }
  }
}

resource "aws_iam_role" "indexer" {
  name               = "${local.name}-lambda"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

resource "aws_iam_role_policy" "indexer" {
  name   = "${local.name}-lambda"
  role   = aws_iam_role.indexer.id
  policy = data.aws_iam_policy_document.indexer.json
}

resource "aws_iam_role_policy_attachment" "logs" {
  role       = aws_iam_role.indexer.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

# --- functions ---------------------------------------------------------------

locals {
  environment = {
    BUCKET                = aws_s3_bucket.repo.id
    CONFIG_KEY            = var.config_key
    PACKAGES_TABLE        = aws_dynamodb_table.packages.name
    STATE_TABLE           = aws_dynamodb_table.state.name
    PUBLISH_QUEUE_URL     = aws_sqs_queue.publish.id
    PUBLISH_DELAY_SECONDS = tostring(var.publish_delay_seconds)
    LEASE_TTL_SECONDS     = tostring(var.publish_timeout)
    LOG_LEVEL             = var.log_level
  }
}

resource "aws_lambda_function" "ingest" {
  function_name = "${local.name}-ingest"
  role          = aws_iam_role.indexer.arn

  filename         = var.ingest_zip
  source_code_hash = filebase64sha256(var.ingest_zip)

  runtime       = "provided.al2023"
  handler       = "bootstrap"
  architectures = ["arm64"]

  timeout = var.ingest_timeout

  # Ingest streams the package rather than buffering it, so memory buys CPU
  # for hashing rather than headroom.
  memory_size = var.ingest_memory

  environment {
    variables = local.environment
  }
}

resource "aws_lambda_function" "publish" {
  function_name = "${local.name}-publish"
  role          = aws_iam_role.indexer.arn

  filename         = var.publish_zip
  source_code_hash = filebase64sha256(var.publish_zip)

  runtime       = "provided.al2023"
  handler       = "bootstrap"
  architectures = ["arm64"]

  timeout = var.publish_timeout

  # The publisher holds a whole scope's metadata in memory: on the live
  # repository that is roughly 24 MB of file lists for 3400 packages, plus the
  # rendered and compressed output.
  memory_size = var.publish_memory

  environment {
    variables = local.environment
  }
}

resource "aws_lambda_event_source_mapping" "ingest" {
  event_source_arn = aws_sqs_queue.ingest.arn
  function_name    = aws_lambda_function.ingest.arn

  batch_size                         = 10
  maximum_batching_window_in_seconds = 5

  # Without this one bad package would drag its nine healthy neighbours
  # through the retry cycle and into the dead-letter queue.
  function_response_types = ["ReportBatchItemFailures"]
}

resource "aws_lambda_event_source_mapping" "publish" {
  event_source_arn = aws_sqs_queue.publish.arn
  function_name    = aws_lambda_function.publish.arn

  batch_size                         = 10
  maximum_batching_window_in_seconds = 5

  # One publisher at a time per scope is enforced by the lease, not here; this
  # cap just keeps a burst of messages from starting more work than the lease
  # will let through.
  scaling_config {
    maximum_concurrency = var.publish_max_concurrency
  }

  function_response_types = ["ReportBatchItemFailures"]
}

# --- scheduled sweep ---------------------------------------------------------

resource "aws_cloudwatch_event_rule" "sweep" {
  name = "${local.name}-sweep"

  # The backstop: republishes anything left dirty by a lost message, and picks
  # up a codename added to repos.yaml with no upload to trigger it.
  schedule_expression = var.sweep_schedule
}

resource "aws_cloudwatch_event_target" "sweep" {
  rule      = aws_cloudwatch_event_rule.sweep.name
  target_id = "publish"
  arn       = aws_lambda_function.publish.arn
}

resource "aws_lambda_permission" "sweep" {
  statement_id  = "AllowScheduledSweep"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.publish.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.sweep.arn
}

# --- alarms ------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "ingest_dlq" {
  count = var.alarm_actions == null ? 0 : 1

  alarm_name          = "${local.name}-ingest-dlq"
  alarm_description   = "Packages have failed to index and are parked in the dead-letter queue; the repository is missing them."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"

  dimensions    = { QueueName = aws_sqs_queue.ingest_dlq.name }
  alarm_actions = var.alarm_actions
}

resource "aws_cloudwatch_metric_alarm" "publish_dlq" {
  count = var.alarm_actions == null ? 0 : 1

  alarm_name          = "${local.name}-publish-dlq"
  alarm_description   = "Index rebuilds are failing; published metadata is going stale."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"

  dimensions    = { QueueName = aws_sqs_queue.publish_dlq.name }
  alarm_actions = var.alarm_actions
}
