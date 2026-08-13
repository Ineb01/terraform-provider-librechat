# A whole LibreChat deployment from nothing, in one apply: the docker provider brings up
# MongoDB and LibreChat, then this provider fills them with accounts, groups, roles, an MCP
# server and an agent.
#
#   tofu init
#   tofu apply
#   # http://localhost:3080 - log in as admin@example.test
#   tofu destroy
#
# The interesting part is the ordering, which is not obvious and is dealt with in
# provider-ordering.tf. Read that file before changing anything here.

terraform {
  required_version = ">= 1.10"

  required_providers {
    docker = {
      source  = "kreuzwerker/docker"
      version = "~> 4.5"
    }
    librechat = {
      source = "ineb01/librechat"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }
}

provider "docker" {
  # Left at the default, which is the local daemon - npipe on Windows, the unix socket
  # elsewhere. A remote daemon works the same way, but then `mongo_host_port` below is
  # published on that host and not on this one, and the librechat provider (which runs
  # wherever tofu runs) would need a tunnel to reach it.
}

# --- Images -------------------------------------------------------------------

resource "docker_image" "mongo" {
  name         = "mongo:7"
  keep_locally = true
}

resource "docker_image" "librechat" {
  # Pinned to the version this provider's field names were read out of. See the README's
  # "Checking a new LibreChat" before moving it.
  name         = var.librechat_image
  keep_locally = true
}

resource "docker_network" "internal" {
  name = "${var.name_prefix}-internal"
}

# --- MongoDB ------------------------------------------------------------------

resource "docker_volume" "mongo_data" {
  name = "${var.name_prefix}-mongo-data"
}

resource "docker_container" "mongodb" {
  name    = "${var.name_prefix}-mongodb"
  image   = docker_image.mongo.image_id
  restart = "unless-stopped"

  # --bind_ip_all because the port is published to the host: this provider runs wherever tofu
  # runs, which is outside the Docker network.
  command = ["mongod", "--bind_ip_all"]

  ports {
    internal = 27017
    external = var.mongo_host_port
  }

  volumes {
    volume_name    = docker_volume.mongo_data.name
    container_path = "/data/db"
  }

  networks_advanced {
    name = docker_network.internal.name
    # LibreChat and the readiness job reach it by this name.
    aliases = ["mongodb"]
  }

  healthcheck {
    test     = ["CMD", "mongosh", "--quiet", "--eval", "db.adminCommand('ping')"]
    interval = "3s"
    timeout  = "5s"
    retries  = 20
  }
}

# --- LibreChat ----------------------------------------------------------------
#
# LibreChat has to run before any of this provider's resources can be created, and not merely
# because it serves the interface: it SEEDS the `roles` and `accessroles` collections on
# start-up. Without those there is no role for a user to hold and no permission template for a
# grant to point at.

resource "docker_container" "librechat" {
  name    = "${var.name_prefix}-librechat"
  image   = docker_image.librechat.image_id
  restart = "unless-stopped"

  ports {
    internal = 3080
    external = var.librechat_host_port
  }

  env = [
    "HOST=0.0.0.0",
    "MONGO_URI=mongodb://mongodb:27017/${var.mongo_database}",
    # Meilisearch is not part of this example, and without this LibreChat retries the
    # connection noisily forever.
    "SEARCH=false",
    "ALLOW_REGISTRATION=true",
    "ALLOW_EMAIL_LOGIN=true",
    # LibreChat refuses to start without these. Generated rather than hardcoded so that two
    # copies of this example do not share them, and marked sensitive in outputs.
    "JWT_SECRET=${random_password.jwt.result}",
    "JWT_REFRESH_SECRET=${random_password.jwt_refresh.result}",
    "CREDS_KEY=${random_id.creds_key.hex}",
    "CREDS_IV=${random_id.creds_iv.hex}",
  ]

  networks_advanced {
    name = docker_network.internal.name
  }

  depends_on = [docker_container.mongodb]
}

# --- Secrets ------------------------------------------------------------------
#
# In a real deployment these belong in a secret store and should be read from it. random_password
# keeps the example self-contained at the cost of putting them in state, which is the trade being
# made here and should not be copied into anything long-lived.

resource "random_password" "jwt" {
  length  = 64
  special = false
}

resource "random_password" "jwt_refresh" {
  length  = 64
  special = false
}

# random_id, not random_password: LibreChat reads these with Buffer.from(value, "hex") and uses
# the result directly as an AES-256 key and IV, so they have to be valid hex of an exact
# length - 32 bytes (64 hex characters) and 16 bytes (32 hex characters). random_password would
# happily emit letters past f, and Buffer.from silently stops at the first non-hex character,
# producing a short key and a start-up crash rather than an error naming the variable.
resource "random_id" "creds_key" {
  byte_length = 32
}

resource "random_id" "creds_iv" {
  byte_length = 16
}
