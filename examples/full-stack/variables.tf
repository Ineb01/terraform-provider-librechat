variable "name_prefix" {
  description = "Prefixed onto every container, volume and network, so two copies of this example can coexist on one daemon."
  type        = string
  default     = "librechat-demo"
}

variable "librechat_image" {
  description = <<-EOT
    LibreChat image. Pinned rather than :latest because this provider writes documents against
    a known schema - see the README's "Checking a new LibreChat" before moving it.
  EOT
  type        = string
  default     = "librechat/librechat:v0.8.7"
}

variable "mongo_host_port" {
  description = <<-EOT
    Host port MongoDB is published on. It has to be published at all because the librechat
    provider runs wherever tofu runs, outside the Docker network.

    27018 rather than 27017 so this does not collide with a MongoDB already running locally -
    including the one in testing/docker-compose.yml.
  EOT
  type        = number
  default     = 27018
}

variable "librechat_host_port" {
  description = "Host port for the LibreChat interface."
  type        = number
  default     = 3080
}

variable "mongo_database" {
  description = "Database name. LibreChat's own default is LibreChat and there is rarely a reason to change it."
  type        = string
  default     = "LibreChat"
}

variable "seed_wait_attempts" {
  description = <<-EOT
    How many two-second attempts the readiness job makes before giving up. LibreChat needs
    roughly 15-25 seconds on a warm image; the default allows two minutes, which is generous
    for a first run that has to build indexes.
  EOT
  type        = number
  default     = 60
}

variable "admin_password" {
  description = <<-EOT
    Password for the admin account this example creates.

    A default is provided because the example is a throwaway. Anywhere real, drop the default
    and read the value from a secret store instead - AWS Parameter Store, Vault, or whatever
    the surrounding configuration already uses. It lands in Terraform state either way, so
    treat state accordingly.
  EOT
  type        = string
  sensitive   = true
  default     = "change-me-please"

  validation {
    # LibreChat's user schema sets minlength 8 on the password path, and the provider enforces
    # it - catching it here names the variable instead of the resource attribute.
    condition     = length(var.admin_password) >= 8
    error_message = "LibreChat requires at least 8 characters."
  }
}
