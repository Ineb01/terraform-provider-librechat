# Unlike LibreChat's own `npm run create-user`, the password is declarative: the provider
# hashes it with bcrypt, so changing it here is an update rather than a delete-and-recreate.
resource "librechat_user" "admin" {
  email    = "admin@example.com"
  name     = "Platform Admin"
  password = var.admin_password
  role     = "ADMIN"
}

# An account that authenticates through an identity provider needs no local password at all.
resource "librechat_user" "sso" {
  email         = "someone@example.com"
  auth_provider = "openid"
}
