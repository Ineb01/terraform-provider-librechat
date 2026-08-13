# Declaring an MCP server as a document rather than in librechat.yaml is the point: a server in
# the config file is global and ownerless, so every user gets it. A document is an ACL-managed
# resource and can be shared with a group.
resource "librechat_mcp_server" "dummy" {
  server_name = "dummy"
  title       = "Dummy Tools"
  url         = "http://dummy-mcp:8000/mcp"

  # Worth raising for a server that loads data at start-up.
  init_timeout = 120000

  # How a bearer token reaches an authenticated MCP server. Read it from a secret store: the
  # value lands in the document, readable by anyone with database access, and in state.
  headers = {
    Authorization = "Bearer ${var.mcp_token}"
  }

  author_id = librechat_user.admin.id
}
