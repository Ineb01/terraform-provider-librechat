# Making one apply work from nothing
#
# The problem: a provider block cannot use depends_on. OpenTofu configures a provider when it
# first needs it, and the only thing that can delay that is a dependency inside the provider's
# own configuration expressions. So "wait for LibreChat before connecting" has to be smuggled
# into the value of mongo_uri itself - there is nowhere else to put it.
#
# Without this the apply fails on a cold start, and confusingly: the librechat provider pings
# MongoDB during Configure, which happens before the containers exist.

# --- Readiness ----------------------------------------------------------------
#
# A one-shot container, not a healthcheck: what has to be true is not "MongoDB accepts
# connections" but "LibreChat has finished seeding", and only the second one makes a grant
# possible. LibreChat writes `accessroles` during start-up, so a non-empty accessroles is the
# signal - and it is the exact thing librechat_grant looks up.
resource "docker_container" "wait_for_seed" {
  name  = "${var.name_prefix}-wait-for-seed"
  image = docker_image.mongo.image_id

  # A job rather than a service. attach + logs makes OpenTofu wait for it to exit and keep its
  # output, so a timeout shows up in the apply instead of having to be dug out with docker logs.
  must_run = false
  attach   = true
  logs     = true

  command = ["sh", "-c", <<-EOT
    set -eu
    i=0
    until [ "$(mongosh --quiet "mongodb://mongodb:27017/${var.mongo_database}" \
                --eval 'db.accessroles.countDocuments({})')" -gt 0 ]; do
      i=$((i + 1))
      # Bounded on purpose. An unbounded wait turns a LibreChat that is crash-looping on a bad
      # CREDS_KEY into an apply that hangs forever with no indication why.
      if [ "$i" -gt ${var.seed_wait_attempts} ]; then
        echo "TIMED OUT after $i attempts: LibreChat has not seeded accessroles."
        echo "It is probably not running. Check: docker logs ${var.name_prefix}-librechat"
        exit 1
      fi
      echo "waiting for LibreChat to seed its roles ($i/${var.seed_wait_attempts})"
      sleep 2
    done
    echo "accessroles seeded after $i attempt(s); LibreChat is ready"
  EOT
  ]

  networks_advanced {
    name = docker_network.internal.name
  }

  depends_on = [docker_container.librechat]

  lifecycle {
    postcondition {
      condition     = self.exit_code == 0
      error_message = "LibreChat never seeded its roles. Read the job's output above, then: docker logs ${var.name_prefix}-librechat"
    }
  }
}

# --- The provider -------------------------------------------------------------

locals {
  # The dependency carrier. appName is a real MongoDB connection-string option and is
  # overwritten by the provider anyway, so its value is inert - which is exactly what is wanted
  # from something present only to put docker_container.wait_for_seed into this expression's
  # dependency graph.
  #
  # Yes, this is a trick. The alternatives are worse: two applies with -target, or a provider
  # that never checks its connection and so reports a wrong URI as seven timeouts instead of
  # one clear error. If OpenTofu ever allows depends_on in a provider block, this is the first
  # thing to delete.
  mongo_uri = join("", [
    "mongodb://127.0.0.1:${var.mongo_host_port}/${var.mongo_database}",
    "?appName=tofu-${docker_container.wait_for_seed.id}",
  ])
}

provider "librechat" {
  # 127.0.0.1 and not the container alias: this provider is a process running wherever tofu
  # runs, not inside the Docker network, so it reaches MongoDB through the published port.
  mongo_uri = local.mongo_uri
}
