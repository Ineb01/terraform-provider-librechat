# Publishing to the Terraform and OpenTofu registries

Everything in the repository is ready. What remains needs accounts and a private key, so it
cannot be automated from here.

Both registries read the **same GitHub release** — there is no separate build, and no artifact to
upload by hand. The release workflow produces exactly the asset layout they require.

## Already done

- Repository renamed to `terraform-provider-librechat`. Both registries require
  `NAMESPACE/terraform-provider-NAME`; the old URL still redirects.
- Repository is public.
- `LICENSE` — MPL-2.0, fetched verbatim from mozilla.org.
- `terraform-registry-manifest.json` — declares protocol 6.0, which is what
  terraform-plugin-framework speaks.
- `docs/` — an overview plus a page per resource and data source, generated from the schemas,
  each with an example and an import section.
- `.goreleaser.yml` and `.github/workflows/release.yml`, validated with `goreleaser check` and a
  full snapshot build: 14 platform zips, `SHA256SUMS` including the manifest entry, and the
  `_v`-prefixed binary inside each zip.
- A signing key, generated locally: RSA-4096, sign-only, no expiry.

  ```
  fingerprint  ABBA1CF0F1E47A610EE96F65169796855E550DDF
  uid          Ineb01 <ineb01@users.noreply.github.com>
  public key   ~/Desktop/librechat-provider-gpg-public.asc
  ```

  The UID uses GitHub's noreply address on purpose: it becomes public on both registries, and a
  work address there would tie the provider back to internal projects.

  It was generated **without a passphrase**, because batch mode cannot prompt.

## 1. Load the signing key into GitHub

Run these yourself; the private key should not pass through a transcript or a chat log:

```sh
gpg --armor --export-secret-keys ABBA1CF0F1E47A610EE96F65169796855E550DDF \
  | gh secret set GPG_PRIVATE_KEY --repo Ineb01/terraform-provider-librechat
```

Then back the private key up offline. Losing it does not break published versions, but you
cannot publish another under the same key, and a replacement key has to be re-uploaded to both
registries.

### On the passphrase

**Optional, and neither registry cares.** GoReleaser signs fine with an unprotected key; leave
the `PASSPHRASE` secret unset and the workflow works unchanged.

It is worth being clear about what it does and does not protect, because the obvious argument
for it is wrong. A passphrase does *not* harden CI: it would live in a secret in this same
repository, readable by this same workflow, so anyone who can read `GPG_PRIVATE_KEY` can read
`PASSPHRASE` beside it. The two travel together.

What it protects is the key **at rest** — in `~/.gnupg` and in backups. That is a real risk and
it is the only one in scope here: an unprotected signing key on a workstation can be used by
anything that can read the user profile.

So pick whichever fits:

- **Keep the key on this machine** → set a passphrase, and set the second secret.

  ```sh
  gpg --change-passphrase ABBA1CF0F1E47A610EE96F65169796855E550DDF
  gh secret set PASSPHRASE --repo Ineb01/terraform-provider-librechat   # prompts, not echoed
  ```

- **Do not keep it here** → skip the passphrase, and remove the local copy once the secret is
  set and an offline backup exists. Nothing then needs unlocking, because nothing is left to
  unlock.

  ```sh
  gpg --delete-secret-keys ABBA1CF0F1E47A610EE96F65169796855E550DDF
  ```

  Keep the *public* key: it is what the registries verify against, and deleting it would mean
  re-exporting it from a backup.

## 2. Terraform Registry

1. Sign in to <https://registry.terraform.io> with the GitHub account that owns the repo, and
   authorise the requested scopes.
2. **Settings → Signing Keys → add** the contents of
   `~/Desktop/librechat-provider-gpg-public.asc`. Do this *before* the first release, or the
   release imports with an unverifiable signature.
3. **Publish → Provider**, pick `Ineb01/terraform-provider-librechat`.

A webhook is created for you, so later tags publish themselves. If a release does not appear,
use the **Resync** button on the provider's settings page.

## 3. Tag the first release

Only after the secrets and the public key are in place:

```sh
git tag v0.1.0
git push origin v0.1.0
```

Watch it: `gh run watch --repo Ineb01/terraform-provider-librechat`

The workflow runs `go vet` and the tests before it signs anything, because a published version
can never be replaced.

Then confirm the release carries `SHA256SUMS`, `SHA256SUMS.sig` and `_manifest.json`:

```sh
gh release view v0.1.0 --repo Ineb01/terraform-provider-librechat --json assets \
  --jq '.assets[].name'
```

## 4. OpenTofu Registry

OpenTofu's registry is submission-based, and it is driven by GitHub issue **forms** — the
automation parses the structured fields, so a plain issue or a pull request will not work.

1. Submit the provider: open an issue at <https://github.com/opentofu/registry/issues/new/choose>
   using the **provider submission** template, giving `Ineb01/terraform-provider-librechat`.
2. Submit the GPG key with the **GPG key submission** template, pasting the same public key.

Automation validates the submission and opens a pull request; a maintainer approves or declines
it. Because the repository is owned by a personal account rather than an organisation, the
public-organisation-membership check that applies to org-owned providers does not apply here.

Anyone may submit a provider, not only its author — but only an author may submit a GPG key for
it, so step 2 has to come from your account.

## 5. Verify it resolves

The point of all of this. From an empty directory, with **no** `dev_overrides` in effect —
unset `TF_CLI_CONFIG_FILE` first, or the override will make this pass without proving anything:

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

```sh
tofu init          # must download and verify the signature
terraform init     # same, from the other registry
```

A successful `init` that reports the signature as verified is the real confirmation. Anything
less — a green plan, a release that looks right — is not.

## Notes

- **Namespace.** It is your GitHub account name, so the address is `ineb01/librechat` and cannot
  be chosen separately. Publishing under an organisation would require moving the repository.
- **Version numbers.** `v0.x` signals that the schema may still change; the registry does not
  treat it specially, but users' `~>` constraints do.
- **The submodule path still reads `projects/code/librechat-terraform-provider`** in the
  superproject, while the repository is now `terraform-provider-librechat`. Harmless — the URL in
  `.gitmodules` is what matters and it has been updated — but worth renaming the path if the
  mismatch is irritating.
