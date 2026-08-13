# member_ids are ObjectId *strings*, which is what LibreChat's schema declares and how it
# resolves membership. Referencing librechat_user.x.id produces the right form.
resource "librechat_group" "support" {
  name        = "Support"
  description = "Everyone who answers tickets."

  member_ids = [librechat_user.agent.id]
}
