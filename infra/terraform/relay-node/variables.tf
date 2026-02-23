variable "aws_region" {
  description = "AWS region for this relay deployment"
  type        = string
}

variable "environment" {
  description = "Deployment environment: prod | staging"
  type        = string
  default     = "prod"
}
