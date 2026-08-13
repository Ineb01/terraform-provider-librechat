# By role name.
#
# Note that an imported resource carries no record of what these fields held before Terraform
# set them, so destroying it will warn rather than restore them.
terraform import librechat_role_permissions.user USER
