# A complete estate against an existing LibreChat: accounts, a group, a restricted role, two
# MCP servers, an agent, and the grants that decide who sees them.
#
# This is the provider's own smoke test. Unlike ../full-stack it does not create the
# containers - it expects testing/docker-compose.yml to be up already:
#
#   docker compose -f ../../testing/docker-compose.yml up -d
#   pwsh ../../build.ps1
#   $env:TF_CLI_CONFIG_FILE = "<repo>/dev.tofurc"
#   tofu apply

terraform {
  required_providers {
    librechat = {
      source = "ineb01/librechat"
    }
  }
}

provider "librechat" {
  # Or leave it out and set LIBRECHAT_MONGO_URI, which is the better habit: the URI reaches a
  # database holding every conversation in the deployment.
  mongo_uri = "mongodb://127.0.0.1:27017/LibreChat"
}

# --- Accounts -----------------------------------------------------------------
#
# Unlike LibreChat's own create-user script, the password here is declarative: changing it is
# an update, not a delete-and-recreate. Read it from a secret store in anything real - it
# lands in state either way, but it should not also be in a committed file.

resource "librechat_user" "admin" {
  email    = "admin@example.test"
  name     = "Platform Admin"
  password = "a-throwaway-password"
  role     = "ADMIN"
}

resource "librechat_user" "member" {
  email    = "member@example.test"
  name     = "Support Agent"
  password = "another-throwaway-password"
  # Deliberately not ADMIN: this account should use the agent below and be unable to edit it.
  role = librechat_role.restricted.name
}

# --- A role that cannot build its own agents -----------------------------------
#
# The point of a Terraform-managed estate is that the agents in it come from git. An account
# that can create its own is an account that can work around that, so this role exists to take
# the ability away while leaving everything else intact.

resource "librechat_role" "restricted" {
  name        = "MANAGED_USER"
  description = "Uses agents that Terraform owns; cannot create or share its own."

  permissions = {
    AGENTS = {
      USE          = true
      CREATE       = false
      SHARE        = false
      SHARE_PUBLIC = false
    }
    MCP_SERVERS = {
      USE          = true
      CREATE       = false
      SHARE        = false
      SHARE_PUBLIC = false
    }
  }
}

# The same restriction applied to the role LibreChat ships, which cannot be managed as a whole
# document because LibreChat re-reconciles it on every start-up. Only the named fields are
# written; USE keeps whatever default the running version has.
resource "librechat_role_permissions" "user" {
  role = "USER"

  permissions = {
    AGENTS      = { CREATE = false, SHARE = false }
    MCP_SERVERS = { CREATE = false }
  }
}

# --- A group to share with ----------------------------------------------------

resource "librechat_group" "support" {
  name        = "Support"
  description = "Everyone who answers tickets."

  member_ids = [
    librechat_user.member.id,
  ]
}

# --- MCP servers --------------------------------------------------------------

resource "librechat_mcp_server" "dummy" {
  server_name = "dummy"
  title       = "Dummy Tools"
  url         = "http://dummy-mcp:8000/mcp"

  # A server that loads data at start-up needs longer than the default for its handshake.
  init_timeout = 120000

  headers = {
    Authorization = "Bearer not-a-real-token"
  }

  author_id = librechat_user.admin.id
}

# Everything optional left out: no title (it falls back to the server name), no headers, no
# timeouts. Worth having next to the one above - these are the paths where writing an explicit
# null instead of omitting the field would land a `null` in the document.
resource "librechat_mcp_server" "other" {
  server_name = "other"
  url         = "http://other-mcp:8000/mcp"
  author_id   = librechat_user.admin.id
}

# --- An agent -----------------------------------------------------------------

resource "librechat_agent" "helpdesk" {
  agent_id = "agent_helpdesk"
  name     = "Helpdesk Assistant"

  description  = "Answers questions about tickets and assets using the tools it is given."
  instructions = "You are a helpdesk assistant. Always use the tools; never guess a ticket number or a status."

  model_provider = "bedrock"
  model          = "claude-opus-5"

  model_parameters = jsonencode({
    temperature = 0.2
    max_tokens  = 4096
  })

  # The suffix is part of the tool name: a server named "dummy" produces ping_mcp_dummy and so
  # on, which is what has to appear here.
  tools = [
    "ping_mcp_dummy",
    "add_mcp_dummy",
  ]
  mcp_server_names = [librechat_mcp_server.dummy.server_name]

  conversation_starters = [
    "What is the status of ticket 12345?",
    "Which assets are assigned to me?",
  ]

  author_id = librechat_user.admin.id
}

# --- Who may see what ---------------------------------------------------------
#
# Ownership goes to the ADMIN role rather than to admin@example.test: every admin, including
# one created later, then has full rights without anything being re-granted, and deleting one
# admin account orphans nothing.

resource "librechat_grant" "agent_owner" {
  resource_type  = "agent"
  resource_id    = librechat_agent.helpdesk.id
  access_role    = "owner"
  principal_type = "role"
  principal_id   = "ADMIN"
  granted_by     = librechat_user.admin.id
}

# viewer, not editor: EDIT is the bit LibreChat checks before allowing a PATCH from the
# interface, and an edit made there is drift the next apply overwrites.
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

# A public grant: everyone with an account, named individually nowhere. principal_id is
# omitted, because a public grant applies to everybody and identifies nobody - the provider
# rejects it if given one.
resource "librechat_grant" "other_public" {
  resource_type  = "mcpServer"
  resource_id    = librechat_mcp_server.other.id
  access_role    = "viewer"
  principal_type = "public"
  granted_by     = librechat_user.admin.id
}

resource "librechat_grant" "other_owner" {
  resource_type  = "mcpServer"
  resource_id    = librechat_mcp_server.other.id
  access_role    = "owner"
  principal_type = "role"
  principal_id   = "ADMIN"
  granted_by     = librechat_user.admin.id
}

# --- What the grants actually mean --------------------------------------------

data "librechat_access_role" "agent_viewer" {
  resource_type = "agent"
  name          = "viewer"
}

output "agent_object_id" {
  description = "What an ACL row references - not the same as agent_id."
  value       = librechat_agent.helpdesk.id
}

output "viewer_perm_bits" {
  description = "The bitmask a viewer grant carries, read from LibreChat rather than assumed."
  value       = data.librechat_access_role.agent_viewer.perm_bits
}
