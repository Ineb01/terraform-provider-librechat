# Everything from here on is managed by this provider, against the containers main.tf just
# brought up. Nothing here needs a depends_on: each resource references the one before it, and
# the provider itself is ordered by provider-ordering.tf.

# --- Accounts -----------------------------------------------------------------

resource "librechat_user" "admin" {
  email    = "admin@example.test"
  name     = "Platform Admin"
  password = var.admin_password
  role     = "ADMIN"
}

resource "librechat_user" "member" {
  email    = "member@example.test"
  name     = "Support Agent"
  password = var.admin_password
  role     = librechat_role.managed_user.name
}

# --- A role that uses agents but cannot build them -----------------------------

resource "librechat_role" "managed_user" {
  name        = "MANAGED_USER"
  description = "Uses the agents Terraform owns; cannot create or share its own."

  permissions = {
    AGENTS      = { USE = true, CREATE = false, SHARE = false, SHARE_PUBLIC = false }
    MCP_SERVERS = { USE = true, CREATE = false, SHARE = false, SHARE_PUBLIC = false }
  }
}

# The same restriction on the role LibreChat ships. It cannot be managed as a whole document
# because LibreChat re-reconciles it on every start-up, so only the named fields are written.
resource "librechat_role_permissions" "user" {
  role = "USER"

  permissions = {
    AGENTS      = { CREATE = false, SHARE = false }
    MCP_SERVERS = { CREATE = false }
  }
}

# --- A group ------------------------------------------------------------------

resource "librechat_group" "support" {
  name        = "Support"
  description = "Everyone who answers tickets."

  member_ids = [librechat_user.member.id]
}

# --- An MCP server ------------------------------------------------------------
#
# Pointed at a name that does not resolve, because this example brings up LibreChat and not an
# MCP server. The document is what is being demonstrated; LibreChat will fail to connect and
# report so in its own interface, which is the honest outcome rather than a fake endpoint that
# appears to work.
resource "librechat_mcp_server" "dummy" {
  server_name = "dummy"
  title       = "Dummy Tools"
  url         = "http://dummy-mcp:8000/mcp"

  init_timeout = 120000

  headers = {
    Authorization = "Bearer replace-me-with-an-ssm-value"
  }

  author_id = librechat_user.admin.id
}

# --- An agent -----------------------------------------------------------------

resource "librechat_agent" "helpdesk" {
  agent_id = "agent_helpdesk"
  name     = "Helpdesk Assistant"

  description  = "Answers questions about tickets and assets using the tools it is given."
  instructions = file("${path.module}/files/agent-helpdesk.md")

  model_provider = "bedrock"
  model          = "claude-opus-5"

  model_parameters = jsonencode({
    temperature = 0.2
    max_tokens  = 4096
  })

  # The server name is a suffix on every tool LibreChat discovers, so a server named "dummy"
  # contributes "add_mcp_dummy" and not "add".
  tools            = ["ping_mcp_dummy", "add_mcp_dummy"]
  mcp_server_names = [librechat_mcp_server.dummy.server_name]

  conversation_starters = [
    "What is the status of ticket 12345?",
    "Which assets are assigned to me?",
  ]

  author_id = librechat_user.admin.id
}

# --- Who may see what ---------------------------------------------------------
#
# Ownership to the ADMIN role rather than to admin@example.test, so an admin created later
# inherits it and deleting one admin orphans nothing. Everyone else gets viewer: EDIT is the
# bit LibreChat checks before allowing a PATCH, and an edit in the interface is drift the next
# apply overwrites.

resource "librechat_grant" "agent_owner" {
  resource_type  = "agent"
  resource_id    = librechat_agent.helpdesk.id
  access_role    = "owner"
  principal_type = "role"
  principal_id   = "ADMIN"
  granted_by     = librechat_user.admin.id
}

resource "librechat_grant" "agent_support" {
  resource_type  = "agent"
  resource_id    = librechat_agent.helpdesk.id
  access_role    = "viewer"
  principal_type = "group"
  principal_id   = librechat_group.support.id
  granted_by     = librechat_user.admin.id
}

resource "librechat_grant" "mcp_owner" {
  resource_type  = "mcpServer"
  resource_id    = librechat_mcp_server.dummy.id
  access_role    = "owner"
  principal_type = "role"
  principal_id   = "ADMIN"
  granted_by     = librechat_user.admin.id
}

resource "librechat_grant" "mcp_support" {
  resource_type  = "mcpServer"
  resource_id    = librechat_mcp_server.dummy.id
  access_role    = "viewer"
  principal_type = "group"
  principal_id   = librechat_group.support.id
  granted_by     = librechat_user.admin.id
}
