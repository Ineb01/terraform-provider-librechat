# Ownership goes to the ADMIN *role*, not to a person: every admin, including one created
# later, then has full rights without anything being re-granted, and deleting one admin account
# orphans nothing.
resource "librechat_grant" "agent_owner" {
  resource_type  = "agent"
  resource_id    = librechat_agent.helpdesk.id # the ObjectId, not agent_id
  access_role    = "owner"
  principal_type = "role"
  principal_id   = "ADMIN" # a role principal is identified by NAME, not by ObjectId
}

# viewer, not editor: EDIT is the bit LibreChat checks before allowing a PATCH from the
# interface, and an edit made there is drift the next apply overwrites.
resource "librechat_grant" "agent_support" {
  resource_type  = "agent"
  resource_id    = librechat_agent.helpdesk.id
  access_role    = "viewer"
  principal_type = "group"
  principal_id   = librechat_group.support.id
}

# A public grant applies to everybody and identifies nobody, so principal_id is omitted.
resource "librechat_grant" "agent_public" {
  resource_type  = "agent"
  resource_id    = librechat_agent.helpdesk.id
  access_role    = "viewer"
  principal_type = "public"
}
