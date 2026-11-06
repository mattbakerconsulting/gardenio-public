# Bootstrap composition inputs.

variable "region" {
  description = "AWS region for the state bucket and lock table."
  type        = string
  default     = "us-east-1"
}

variable "state_bucket_name" {
  description = "Globally unique S3 bucket name for the Terraform state backend."
  type        = string
  default     = "gardenio-customer-poc-tfstate"
}

variable "lock_table_name" {
  description = "DynamoDB table name for the S3 backend lock handle."
  type        = string
  default     = "gardenio-customer-poc-tflock"
}

variable "common_tags" {
  description = "Tags applied to every taggable resource. The `gardenio:account` tag is NOT set here — it is derived from the live AWS caller identity in main.tf so a fresh account tags its own id automatically (no org-specific literal)."
  type        = map(string)
  default = {
    "gardenio:env"     = "customer-poc-poc"
    "gardenio:managed" = "terraform"
  }
}
