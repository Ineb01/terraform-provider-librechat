output "librechat_url" {
  description = "Where to log in. Use the admin account below."
  value       = "http://localhost:${var.librechat_host_port}"
}

output "admin_email" {
  value = librechat_user.admin.email
}

output "mongo_uri" {
  description = "Reaches every conversation in the deployment, hence sensitive."
  value       = "mongodb://127.0.0.1:${var.mongo_host_port}/${var.mongo_database}"
  sensitive   = true
}

output "agent_object_id" {
  description = "What an ACL row references - not the same as agent_id."
  value       = librechat_agent.helpdesk.id
}

output "verify_access" {
  description = "Asks LibreChat's own permission code whether the grants above actually work."
  value = join(" ", [
    "docker cp ../../testing/check-access.js ${docker_container.librechat.name}:/tmp/ &&",
    "docker exec -e MONGO_URI=mongodb://mongodb:27017/${var.mongo_database}",
    "${docker_container.librechat.name} node /tmp/check-access.js",
  ])
}
