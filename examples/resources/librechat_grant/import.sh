# A grant can be named by what identifies it - the resource and the principal - because an ACL
# row has no name of its own:
#
#   resource_type/resource_key/principal_type[/principal_id]
#
# The resource is an agent id or an MCP server name (those are the two this provider manages by
# name); any other resource type has to be given as an ObjectId. The principal is a group name,
# a user email, or a role name - ObjectIds work everywhere a name does.
terraform import librechat_grant.agent_owner   agent/agent_helpdesk/role/ADMIN
terraform import librechat_grant.agent_support agent/agent_helpdesk/group/Support
terraform import librechat_grant.agent_alice   agent/agent_helpdesk/user/alice@example.test
terraform import librechat_grant.agent_public  agent/agent_helpdesk/public
terraform import librechat_grant.mcp_owner     mcpServer/dummy/role/ADMIN

# The ACL row's ObjectId still works, and is the answer when the form above is ambiguous - a
# multi-tenant deployment separates otherwise identical rows by tenantId, which the lookup above
# does not filter on. It refuses to guess in that case rather than importing an arbitrary row.
terraform import librechat_grant.agent_owner 6a7d85f9807980fd965f4930

# Note `granted_by`: it is audit metadata the row may carry, and it is optional in the
# configuration. If the imported row has one and the configuration does not, the first plan
# after the import shows a change that clears it. Copy the value into the configuration, or
# accept that one-time diff.
