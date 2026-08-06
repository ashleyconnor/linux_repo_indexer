variable "name_prefix" {
  description = "Prefix for every resource name."
  type        = string
  default     = "linux-repo-indexer"
}

variable "bucket_name" {
  description = "Bucket holding both packages and published indexes."
  type        = string
}

variable "config_key" {
  description = "Key of repos.yaml within the bucket. It is read per invocation, so adding a codename takes effect without a deploy."
  type        = string
  default     = "repos.yaml"
}

variable "package_prefixes" {
  description = <<-EOT
    Key prefixes that hold packages. Each becomes an S3 notification filter.
    Index writes must not match any of them, or publishing would trigger
    ingest, which would trigger publishing.
  EOT
  type        = list(string)
  default     = ["pool/", "RHEL/", "AmazonLinux/", "fedora/"]
}

variable "ingest_zip" {
  description = "Path to the ingest Lambda package built by `make build`."
  type        = string
}

variable "publish_zip" {
  description = "Path to the publish Lambda package built by `make build`."
  type        = string
}

variable "signing_secret_arn" {
  description = "ARN of the Secrets Manager secret holding the OpenPGP signing key. Empty publishes an unsigned repository, which is only useful for a test stack."
  type        = string
  default     = ""
}

variable "publish_delay_seconds" {
  description = "Coalescing window on publish messages. Uploading a release's worth of packages should cause one index build, not one per package."
  type        = number
  default     = 30
}

variable "ingest_timeout" {
  description = "Seconds. Bounded by downloading and hashing the largest package."
  type        = number
  default     = 300
}

variable "ingest_memory" {
  description = "MB. Ingest streams rather than buffers, so this buys CPU for hashing."
  type        = number
  default     = 512
}

variable "publish_timeout" {
  description = "Seconds. Also used as the publish lease TTL, so a publisher that dies cannot block a scope for longer than one run."
  type        = number
  default     = 900
}

variable "publish_memory" {
  description = "MB. The publisher holds a whole scope's metadata in memory."
  type        = number
  default     = 2048
}

variable "publish_max_concurrency" {
  description = "Caps how many publish messages are worked at once. Correctness comes from the lease, not from this."
  type        = number
  default     = 10
}

variable "sweep_schedule" {
  description = "Schedule for the backstop sweep that republishes anything left dirty."
  type        = string
  default     = "rate(5 minutes)"
}

variable "versioning_enabled" {
  description = "The index can always be regenerated, but a package object is the only copy of itself."
  type        = bool
  default     = true
}

variable "point_in_time_recovery" {
  description = "Enable PITR on both tables."
  type        = bool
  default     = true
}

variable "public_read_policy" {
  description = "Serve the bucket directly over HTTP. Production normally fronts it with CloudFront instead."
  type        = bool
  default     = false
}

variable "log_level" {
  description = "info or debug."
  type        = string
  default     = "info"
}

variable "alarm_actions" {
  description = "SNS topic ARNs for the dead-letter queue alarms. Null disables the alarms."
  type        = list(string)
  default     = null
}
