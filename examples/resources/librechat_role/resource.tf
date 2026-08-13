# A CUSTOM role, owned outright by Terraform.
#
# Do not use this for USER or ADMIN: LibreChat seeds those and re-reconciles them on every
# start-up, so managing one as a whole document means fighting the application. Use
# librechat_role_permissions for them instead.
resource "librechat_role" "managed_user" {
  name        = "MANAGED_USER"
  description = "Uses the agents Terraform owns; cannot create or share its own."

  permissions = {
    AGENTS      = { USE = true, CREATE = false, SHARE = false, SHARE_PUBLIC = false }
    MCP_SERVERS = { USE = true, CREATE = false, SHARE = false, SHARE_PUBLIC = false }
  }
}
