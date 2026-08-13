# How a grant is made to a group synced from a directory: that group belongs to the sync and
# must not be managed with librechat_group, but its id is exactly what librechat_grant needs.
data "librechat_group" "engineering" {
  name = "Engineering"
}
