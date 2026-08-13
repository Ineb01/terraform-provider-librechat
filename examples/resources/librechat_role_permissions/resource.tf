# Patches named fields of a role LibreChat owns, leaving the rest of the document alone.
#
# This is what stops ordinary accounts building their own agents while leaving them able to use
# every agent granted to them. librechat.yaml cannot express it: its `interface` block writes
# only the USE bit of each permission type, so `agents: false` there would take away using them
# as well.
resource "librechat_role_permissions" "user" {
  role = "USER"

  permissions = {
    AGENTS      = { CREATE = false, SHARE = false }
    MCP_SERVERS = { CREATE = false }
  }
}
