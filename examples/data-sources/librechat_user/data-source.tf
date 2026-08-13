# Usually needed for an agent's or MCP server's author_id: on a database where the first admin
# registered through the web interface, Terraform never created that account and has no
# reference to it.
data "librechat_user" "author" {
  email = "admin@example.com"
}
