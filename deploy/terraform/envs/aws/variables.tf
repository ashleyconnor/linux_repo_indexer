variable "region" {
  description = "AWS region to deploy into."
  type        = string
}

variable "profile" {
  description = "Shared-config profile to authenticate with. Empty uses the default credential chain."
  type        = string
  default     = null
}

variable "bucket_name" {
  description = "Repository bucket. S3 bucket names are globally unique, so this must not be taken."
  type        = string
}

variable "name_prefix" {
  description = "Prefix for every resource name, so several stacks can share an account."
  type        = string
  default     = "linux-repo-indexer"
}

variable "ingest_zip" {
  description = "Path to the built ingest Lambda. Run `make build` first."
  type        = string
  default     = "../../../../dist/ingest.zip"
}

variable "publish_zip" {
  description = "Path to the built publish Lambda. Run `make build` first."
  type        = string
  default     = "../../../../dist/publish.zip"
}

variable "public_read_policy" {
  description = "Serve the bucket directly over HTTP. This makes every object world-readable."
  type        = bool
  default     = false
}

variable "publish_delay_seconds" {
  description = "Coalescing window on publish messages."
  type        = number
  default     = 30
}

variable "sweep_schedule" {
  description = "Backstop sweep schedule."
  type        = string
  default     = "rate(1 hour)"
}

variable "log_level" {
  description = "Lambda log level."
  type        = string
  default     = "info"
}

variable "tags" {
  description = "Tags applied to every resource that supports them."
  type        = map(string)
  default     = {}
}
