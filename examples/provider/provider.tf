# The provider writes LibreChat's MongoDB directly, because LibreChat's REST API has no
# endpoints for roles, groups or role permissions.
#
# Prefer the LIBRECHAT_MONGO_URI environment variable over this attribute: the URI grants full
# read/write access to every conversation in the deployment.
provider "librechat" {
  mongo_uri = "mongodb://127.0.0.1:27017/LibreChat"
}
