# By email, which is what the account's owner actually typed - rather than an ObjectId nobody
# can easily find.
terraform import librechat_user.admin admin@example.com

# The document's ObjectId works too.
terraform import librechat_user.admin 6a7d85f81ac51c892da56853
