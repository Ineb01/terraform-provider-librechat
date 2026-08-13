# librechat_grant resolves these itself, so this is not needed to make a grant. It is for
# answering "what does editor actually allow": perm_bits is the bitmask LibreChat's permission
# checks compare against, and reading it beats reasoning about which bits a release assigns.
data "librechat_access_role" "agent_editor" {
  resource_type = "agent"
  name          = "editor"
}
