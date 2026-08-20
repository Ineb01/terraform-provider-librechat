# terraform-provider-librechat

An OpenTofu/Terraform provider for LibreChat: users, agents, MCP servers, groups, roles and
permissions as declarative resources.

It exists because a LibreChat estate that matters cannot be built by clicking. Agents carry
system prompts that want reviewing, MCP servers carry bearer tokens, and "who can see this
agent" is an access-control decision — all of which belong in git and in a plan, not in
someone's browser session.

```hcl
resource "librechat_agent" "helpdesk" {
  agent_id       = "agent_helpdesk"
  name           = "Helpdesk Assistant"
  instructions   = file("${path.module}/files/agent-helpdesk.md")
  model_provider = "bedrock"
  model          = "claude-opus-5"

  tools            = ["add_mcp_dummy"]
  mcp_server_names = [librechat_mcp_server.dummy.server_name]

  author_id = librechat_user.admin.id
}

resource "librechat_grant" "service" {
  resource_type  = "agent"
  resource_id    = librechat_agent.helpdesk.id
  access_role    = "viewer"
  principal_type = "group"
  principal_id   = librechat_group.service.id
}
```

## Installing

```hcl
terraform {
  required_providers {
    librechat = {
      source  = "ineb01/librechat"
      version = "~> 0.1"
    }
  }
}
```

`source` is deliberately unqualified: it resolves against whichever registry the CLI defaults
to — `registry.terraform.io` for Terraform, `registry.opentofu.org` for OpenTofu — and both
serve the same GitHub release. Only the release is authoritative; there is no separate build per
registry.

To work on the provider itself instead, see [Building and using it](#building-and-using-it),
which bypasses the registry entirely.

## Importing an estate that already exists

Every resource imports by the name a person actually has: a user by email, a group or MCP server
by name, an agent by its `agent_id`, a role by its name. ACL rows have no name of their own, so a
grant is named by the two things that identify it — the resource and the principal:

```sh
tofu import 'librechat_grant.agent_owner["iqs"]' agent/agent_dev_iqs/role/ADMIN
tofu import 'librechat_grant.agent_view["iqs"]'  agent/agent_dev_iqs/group/onboarding
tofu import 'librechat_grant.agent_public["iqs"]' agent/agent_dev_iqs/public
```

That matters for exactly one job, and it is the job that decides whether this provider is worth
adopting: taking over an estate somebody else built. A LibreChat deployment with eight agents
carries something like 37 ACL rows, and importing them by ObjectId means querying MongoDB for
every one of them first. The ObjectId form still works, and is the fallback when the tuple is
ambiguous — a multi-tenant deployment separates otherwise identical rows by `tenantId`, and the
lookup refuses to guess rather than importing an arbitrary row.

## It talks to MongoDB, not to the REST API

This is the central design decision and it explains most of what follows.

LibreChat's HTTP API has **no endpoints at all** for roles, groups or role permissions, and
`librechat.yaml` can only write the `USE` bit of a permission type — so `agents: false` there
takes away *using* agents as well as creating them. The database is the only surface that
covers all six resource types this provider manages.

Two consequences worth knowing before you commit to it:

- **The provider needs network access to MongoDB.** In a normal deployment that port is only
  reachable from the Docker network LibreChat runs on, so applying from a workstation means an
  SSH tunnel to the daemon host. There is no way around this short of LibreChat growing an
  admin API.
- **It writes documents the application also writes.** Every resource here is careful about
  which fields it owns and which it leaves alone, and the field names were read out of
  `packages/data-schemas/src/schema/*.ts` inside the pinned image rather than guessed. When
  you bump LibreChat, re-read them — see [Checking a new LibreChat](#checking-a-new-librechat).

The one thing the database gives you that the application's own tooling does not: **passwords
are declarative.** LibreChat's `npm run create-user` cannot change a password, so a mongosh
seed job has to treat an existing account as untouchable. This provider hashes with bcrypt
itself, so a password change is an ordinary update. That interoperability is verified, not
assumed — see `TestBcryptFormatIsInteroperable`.

## Resources

| | Manages | Notes |
| --- | --- | --- |
| `librechat_user` | `users` | Hashes the password with bcrypt; detects a password changed in the interface as drift |
| `librechat_group` | `groups` | Local groups only; membership by account ObjectId |
| `librechat_agent` | `agents` | Creating one grants nobody access — pair with `librechat_grant` |
| `librechat_mcp_server` | `mcpservers` | Preserves LibreChat's cached introspection on update |
| `librechat_role` | `roles` | A **custom** role, owned outright |
| `librechat_role_permissions` | `roles.permissions.*` | Patches named fields of a role LibreChat owns (`USER`, `ADMIN`) |
| `librechat_grant` | `aclentries` | One "who may do what with this resource" row |

Data sources: `librechat_user`, `librechat_group`, `librechat_access_role`.

Every resource supports `import`, and by its natural name rather than only by ObjectId —
`tofu import librechat_user.admin admin@example.test`, `... librechat_agent.x agent_helpdesk`,
`... librechat_mcp_server.x dummy`. The id of an account somebody registered in the web
interface is not something they can easily find; the address they typed is.

### Two kinds of permission, and they are not the same question

This trips people up, so it is worth stating plainly:

- **`librechat_grant`** answers *who may use this particular agent*. It is an ACL row.
- **`librechat_role_permissions`** answers *may this account build an agent of its own at
  all*. It is a field on the role, enforced in LibreChat's middleware — `POST /api/agents`
  wants `AGENTS.USE` **and** `AGENTS.CREATE`, so clearing `CREATE` leaves an account able to
  use every agent shared with it and unable to make its own.

An estate whose agents come from git usually wants both: grants at `viewer`, and `CREATE`
cleared on `USER`.

### Grant ownership to the ADMIN role, not to a person

```hcl
principal_type = "role"
principal_id   = "ADMIN"
access_role    = "owner"
```

Every admin — including one created next year — then has full rights without anything being
re-granted, and deleting one admin account orphans nothing. `author_id` still has to name a
real account because the schema requires an ObjectId there, but it confers no permissions.

And grant no more than `viewer` to anyone who should not edit a Terraform-managed resource:
`editor` carries the EDIT bit, which is what LibreChat checks before allowing a PATCH from the
interface — and an edit made there is drift that the next apply silently overwrites.

### What Terraform does *not* reconcile

Terraform manages the grants it created. A grant added by hand in the sharing dialog is left
alone rather than revoked, so it will **not** show up in a plan. If exclusive control matters,
audit `aclentries` for the resource id directly. (A mongosh seed job that rewrites all grants
on every run does reconcile, and that is the one thing it does better than this.)

## Building and using it

Nothing needs to be installed but Docker; the toolchain runs in a container, the same way
OpenTofu itself is often run.

```sh
pwsh ./build.ps1 -Test          # test, build, write dev.tofurc
pwsh ./build.ps1 -Linux         # for an OpenTofu that itself runs in a container
```

The result is wired up with `dev_overrides`, not installed into a plugin directory — rebuild,
re-plan, no version bump and no `tofu init`. It is activated per-shell so it cannot quietly
affect an unrelated apply:

```powershell
$env:TF_CLI_CONFIG_FILE = "$PWD\dev.tofurc"
$env:LIBRECHAT_MONGO_URI = "mongodb://127.0.0.1:27017/LibreChat"
tofu plan
```

`tofu init` will warn that `dev_overrides` is in effect and that the lock file is being
ignored. That warning is the mechanism working. The flip side is that the provider is **not
pinned**: whatever binary is in `./bin` is what runs.

Prefer `LIBRECHAT_MONGO_URI` over the `mongo_uri` attribute. That URI grants full read/write
access to every conversation in the deployment.

## Examples

- **`examples/full-stack`** — a whole deployment from nothing in one apply: the `docker`
  provider creates MongoDB and LibreChat, then this provider creates the accounts, group, role,
  MCP servers, agent and grants. 22 resources, needs only Docker and OpenTofu. Its README
  explains the one genuinely awkward part, which is that a `provider` block cannot use
  `depends_on`.
- **`examples/complete`** — the same LibreChat content against a stack that already exists.
  This is what the provider's own smoke test applies.

## Tests

```sh
docker compose run --rm go test ./...     # ~1s, no MongoDB, no LibreChat, no network
```

The suite deliberately needs nothing running, because a suite that needs a live stack is a
suite nobody runs. It covers the decisions that are quietly wrong rather than loudly broken:
bcrypt format interoperability, the shape of each principal type, the restore-on-destroy
snapshot for role permissions, BSON's several numeric types, and — cheapest and most valuable
— `ValidateImplementation` on every schema, which catches at test time the things the
framework otherwise only reports in the middle of somebody's plan. (That is how `provider`
became `model_provider`: it is a reserved meta-argument in a resource block.)

## Releasing

Pushing a `v*` tag is the whole publishing mechanism. `.github/workflows/release.yml` builds
every platform with GoReleaser, signs the checksums with the release key, and creates the GitHub
release; both registries then pick it up from there. Nothing is uploaded by hand.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

Before the first tag, three things have to be in place — see
[PUBLISHING.md](PUBLISHING.md) for the full sequence:

- repository secrets `GPG_PRIVATE_KEY` and `PASSPHRASE`
- the matching **public** key uploaded to each registry
- the provider claimed on registry.terraform.io and submitted to the OpenTofu registry

Two rules that are easy to break and expensive to fix:

- **Never re-tag or replace a published version.** Both registries cache the checksums, so a
  replaced artifact fails verification for everyone who already downloaded it. Release
  `v0.1.1` instead.
- **Do not let the signing key expire.** Existing releases keep verifying, but new ones cannot
  be published under a key the registry has recorded as expired.

The docs under `docs/` are generated and must be regenerated whenever a schema description
changes, or the registry pages go stale:

```sh
docker compose run --rm go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@latest \
  generate --provider-name librechat
```

Validate the release build without publishing anything — worth doing before every tag, because a
tag cannot be taken back:

```sh
# Note the mount: this repo is a git submodule, so its .git is a *file* pointing at the
# superproject. Mounting only this directory makes GoReleaser report "not a git repository".
docker run --rm -v "$(git rev-parse --show-superproject-working-tree):/repo" \
  -w "/repo/$(git rev-parse --show-prefix)" goreleaser/goreleaser:latest \
  release --snapshot --clean --skip=sign,publish,validate
```

## Verifying

A green apply proves nothing about whether LibreChat agrees. `testing/` holds a throwaway
stack and a checker that calls **LibreChat's own permission code**:

```sh
docker compose -f testing/docker-compose.yml up -d
pwsh ./build.ps1
cd examples/complete && tofu apply           # 13 resources
cd ../.. && docker compose -f testing/docker-compose.yml cp testing/check-access.js librechat:/tmp/
docker compose -f testing/docker-compose.yml exec librechat node /tmp/check-access.js
```

LibreChat itself has to run, not just MongoDB: it seeds `roles` and `accessroles` on start-up,
and without those there are no permission templates for a grant to point at.

`check-access.js` goes through `getUserPrincipals` and `getEffectivePermissions` rather than
through HTTP. That is not a stylistic preference — LibreChat's REST API rejects a hand-made
request with `Illegal request` *before* any permission check runs, so an HTTP probe cannot
tell a working grant from a rejected request. Reading the collections with mongosh would be
worse still: it would only confirm that the documents match my reading of the schema, which is
the thing under test.

What it asserts, all currently passing against `v0.8.7`: a group member resolves to their
group and can VIEW but not EDIT the agent; an admin gets VIEW+EDIT+DELETE through the role
grant; **an admin created after the grant inherits it**; a non-member gets nothing; the MCP
server behaves the same; and the restricted role may USE agents but not CREATE them.

Also worth running by hand, because they are the claims most likely to rot:

- Log in through LibreChat's own form after a password change. The old password must stop
  working — that is the whole point of the bcrypt round trip.
- Seed `config.tools`/`config.capabilities` on an MCP server, then change its `title` and
  apply. The cache must survive; overwriting it forces a reconnect, and until that succeeds
  the agent's tool list is empty.
- `tofu destroy`, then check `roles`. A `librechat_role_permissions` restores each field to
  what it held *before* Terraform first set it — not to LibreChat's defaults, and not to the
  value the apply wrote.

## Checking a new LibreChat

The field names here were read out of the image, so that is also how to check a new one:

```sh
docker create --name probe librechat/librechat:v0.8.8
docker cp probe:/app/packages/data-schemas/src/schema ./schema
docker rm probe
```

Diff `user.ts`, `group.ts`, `agent.ts`, `mcpServer.ts`, `role.ts`, `aclEntry.ts` and
`accessRole.ts` against what the resources write. Then bump the image in
`testing/docker-compose.yml` and re-run the verification above — a schema change that this
provider does not know about is usually silent, not an error.

Permission bits are the exception and need no checking: `librechat_grant` reads `permBits`
out of the `accessroles` document at apply time instead of hardcoding it, so a release that
adds a bit does not leave stale bitmasks behind.

## Traps

- **`memberIds` are strings, not ObjectIds.** LibreChat's schema declares `[String]` and
  resolves membership with `Group.find({ memberIds: user.idOnTheSource || String(user._id) })`.
  An ObjectId there matches nothing: the user resolves to zero groups, every grant made to the
  group quietly stops applying, and nothing errors. The provider always writes the string form.
- **An agent has two identifiers.** `agent_id` (`agent_helpdesk`) is what LibreChat's API uses;
  `id` is the document's ObjectId, and that is what an ACL row references. Passing the wrong
  one to `librechat_grant.resource_id` is caught with a message saying so.
- **A role principal is identified by name.** `principal_id = "ADMIN"`, not the role
  document's ObjectId. Users and groups go the other way.
- **A `public` grant carries no `principalId` key at all**, which is why the provider omits it
  rather than writing null — a filter of `{principalId: null}` would not find the row.
- **`email` and `username` must be lowercase.** Mongoose lowercases those paths, so a
  mixed-case value written by the raw driver stops matching LibreChat's own queries. The
  provider rejects uppercase rather than normalising, because normalising silently would make
  state disagree with configuration.
- **Nested documents decode as `bson.D` by default.** The client sets `DefaultDocumentM`; the
  symptom without it was an MCP server's `headers` reading back as absent, so every plan wanted
  to write them again forever.
- **Destroying a `librechat_user` leaves their conversations.** Messages, files and uploads
  live in other collections and on disk; removing them is LibreChat's `npm run delete-user`.
  ACL grants made *to* the account do go with it.
- **An imported `librechat_role_permissions` cannot restore anything on destroy.** The
  pre-Terraform values are recorded in private state at create time, and an import has no such
  record. It warns rather than guessing.
- **A password shows up in state.** Unavoidable for a declarative password. Read it from
  a secret store rather than writing it in a committed `.tf`, and treat state as the secret
  store it already is. `password_hash` accepts a pre-computed hash if you would rather the
  plaintext never appeared at all.
