# full-stack

A whole LibreChat deployment from nothing in **one apply**: the `docker` provider brings up
MongoDB and LibreChat, then this provider fills them with accounts, a group, a restricted role,
two MCP servers, an agent and the grants that decide who sees them. 22 resources.

```sh
tofu init
tofu apply
# http://localhost:3080 - log in as admin@example.test / change-me-please
tofu destroy
```

Nothing outside Docker and OpenTofu is needed. `../complete` is the same LibreChat content
against a stack you already have; this one owns the stack too.

## The ordering problem, and why provider-ordering.tf exists

A `provider` block cannot use `depends_on`. OpenTofu configures a provider when it first needs
it, and the only thing that can delay that is a dependency inside the provider's **own**
configuration expressions — so "connect only once LibreChat has started" has to be smuggled
into the value of `mongo_uri` itself. There is nowhere else to put it.

That is what the `?appName=tofu-${docker_container.wait_for_seed.id}` in `provider-ordering.tf`
is for. `appName` is a real connection-string option, the provider overwrites it anyway, and its
value is therefore inert — it is present only to put the readiness job into that expression's
dependency graph. It is a trick, and it is commented as one. If OpenTofu ever allows
`depends_on` in a provider block, that is the first thing to delete.

Two things had to be true for this to work at all, and both are worth knowing if you write a
configuration like this yourself:

- **Waiting for MongoDB is not enough.** LibreChat seeds the `roles` and `accessroles`
  collections during start-up. Until it has, there is no role for a user to hold and no
  permission template for a grant to point at. The readiness job therefore polls for a
  non-empty `accessroles` — the exact thing `librechat_grant` looks up — rather than for a
  healthy database. It is bounded, so a LibreChat crash-looping on a bad `CREDS_KEY` fails the
  apply instead of hanging it.
- **The provider must tolerate an unknown `mongo_uri` at plan time.** Because the URI derives
  from a container that does not exist yet, it is unknown during plan; the provider returns
  from `Configure` without a client and without an error, and OpenTofu configures it again
  during apply once the value is known. An earlier version raised an error there instead, and
  the symptom was a plan that silently refused to consider any `librechat_*` resource at all.

## Verifying

The `verify_access` output prints a command that asks **LibreChat's own permission code**
whether the grants really work — 15 assertions, including that an admin created after the
grant inherits it, and that a non-member gets nothing:

```sh
tofu output -raw verify_access   # then run it
```

## Notes

- The MCP servers point at hostnames that do not resolve, because this example brings up
  LibreChat and not an MCP server. The documents are what is being demonstrated; LibreChat
  reports the failed connection in its own interface, which is more honest than a fake endpoint
  that appears to work.
- `admin_password` has a default because the example is a throwaway. Anywhere real, drop the
  default and read it from a secret store — it lands in state either way.
- `mongo_host_port` defaults to **27018**, not 27017, so this does not collide with a MongoDB
  already running locally — including the one in `testing/docker-compose.yml`.
- MongoDB has no authentication and its port is published to the host. That is tolerable only
  because the whole stack is disposable; do not copy the pattern.
