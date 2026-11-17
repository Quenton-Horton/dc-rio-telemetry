terraform {
  required_version = ">= 1.5.0"

  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
  }
}

provider "cloudflare" {
}

variable "cloudflare_account_id" {
  description = "Cloudflare Account ID"
  type        = string
  default     = "382f182b01308f42c7c91ea0c29939e3"
}

variable "allowed_emails" {
  description = "Email addresses allowed to receive One-Time PIN login codes"
  type        = list(string)
  default     = ["qahorton@gmail.com", "qhorton33@gmail.com"]
}

resource "cloudflare_zero_trust_access_identity_provider" "email_otp" {
  account_id = var.cloudflare_account_id
  name       = ""
  type       = "onetimepin"
  config     = {}
}

resource "cloudflare_zero_trust_access_policy" "owner_only" {
  account_id       = var.cloudflare_account_id
  name             = "Allow emails: 11/13/2026"
  decision         = "allow"
  session_duration = "24h"

  include = [
    for addr in var.allowed_emails : {
      email = {
        email = addr
      }
    }
  ]

  lifecycle {
    create_before_destroy = true
  }
}

resource "cloudflare_zero_trust_access_application" "telemetry" {
  account_id                = var.cloudflare_account_id
  name                      = "DC & Rio Telemetry"
  domain                    = "telemetry.quentonhorton.com"
  type                      = "self_hosted"
  session_duration          = "24h"
  app_launcher_visible      = true
  auto_redirect_to_identity = true

  allowed_idps = [
    cloudflare_zero_trust_access_identity_provider.email_otp.id
  ]

  policies = [
    {
      id         = cloudflare_zero_trust_access_policy.owner_only.id
      precedence = 1
    }
  ]
}

output "access_application_aud" {
  description = "Application Audience (AUD) tag for JWT verification"
  value       = cloudflare_zero_trust_access_application.telemetry.aud
}