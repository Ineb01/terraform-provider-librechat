# Creating an agent grants nobody access to it. Pair it with librechat_grant.
resource "librechat_agent" "helpdesk" {
  agent_id = "agent_helpdesk"
  name     = "Helpdesk Assistant"

  description  = "Answers questions about tickets and assets."
  instructions = file("${path.module}/files/agent-helpdesk.md")

  model_provider = "bedrock"
  model          = "claude-opus-5"

  model_parameters = jsonencode({
    temperature = 0.2
    max_tokens  = 4096
  })

  # An MCP server's name is a suffix on every tool it contributes, so a server named "dummy"
  # provides "add_mcp_dummy" and not "add".
  tools            = ["ping_mcp_dummy", "add_mcp_dummy"]
  mcp_server_names = [librechat_mcp_server.dummy.server_name]

  conversation_starters = ["What is the status of ticket 12345?"]

  # Required by LibreChat's schema; it grants nothing. Permissions come from librechat_grant.
  author_id = librechat_user.admin.id
}
