<#
.SYNOPSIS
  Builds the provider and writes a CLI config file that points OpenTofu at it.

.DESCRIPTION
  Compiles in a golang container, so nothing but Docker has to be installed - the same
  reason it is often sensible to run OpenTofu itself that way.

  The result is wired up with dev_overrides rather than being installed into a plugin
  directory. That is what makes iterating bearable: rebuild, re-plan, no version bump, no
  checksum, no `tofu init`. It also means the provider is NOT pinned - whatever binary is in
  ./bin at the time is what runs.

  dev_overrides is written to ./dev.tofurc and activated with TF_CLI_CONFIG_FILE rather than
  by touching %APPDATA%\tofurc, so it applies only to shells that opt in and cannot quietly
  affect an unrelated apply.

.PARAMETER Linux
  Build for linux/amd64 instead of the host platform. Needed when OpenTofu itself runs in a
  container, because a plugin is executed as a process by the CLI and has to match the CLI's
  operating system, not the daemon's.

.PARAMETER Test
  Run the test suite first and stop if it fails.

.EXAMPLE
  pwsh ./build.ps1 -Test
  $env:TF_CLI_CONFIG_FILE = "$PWD/dev.tofurc"
  cd examples/complete
  tofu plan
#>
[CmdletBinding()]
param(
    [switch]$Linux,
    [switch]$Test
)

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

if ($Test) {
    Write-Host '--- go test ---' -ForegroundColor Cyan
    docker compose run --rm go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Tests failed; not building.' }
}

# Terraform recognises a plugin by filename, and dev_overrides is the one mechanism that does
# not care about the version in it - but the terraform-provider- prefix is still required.
$name = 'terraform-provider-librechat'

if ($Linux) {
    $goos = 'linux'
    $ext = ''
} else {
    $goos = 'windows'
    $ext = '.exe'
}

$binary = "bin/$name$ext"

New-Item -ItemType Directory -Force bin | Out-Null

Write-Host "--- go build ($goos/amd64) ---" -ForegroundColor Cyan
# The version is only cosmetic (it shows up in `tofu providers` and in the user agent), but a
# git describe makes a stale binary in ./bin obvious.
$version = (git describe --tags --always --dirty 2>$null)
if (-not $version) { $version = 'dev' }

docker compose run --rm `
    -e GOOS=$goos -e GOARCH=amd64 `
    go build -ldflags "-s -w -X main.version=$version" -o $binary .
if ($LASTEXITCODE -ne 0) { throw 'Build failed.' }

$absoluteBin = (Resolve-Path bin).Path

# The same binary is also placed in a filesystem mirror, because dev_overrides alone is not
# enough to run `tofu init`.
#
# dev_overrides takes effect at plan/apply time but NOT during init: init still tries to
# install every provider in required_providers, and ineb01/librechat is in no registry, so init
# fails. That does not matter for a configuration using only this provider - it needs no init -
# but examples/full-stack also uses kreuzwerker/docker and hashicorp/random, which do, and one
# failing provider fails the whole init.
#
# So: the mirror satisfies init, and dev_overrides still wins at run time, which keeps the fast
# rebuild-and-replan loop.
$mirrorVersion = '0.0.1'
$target = "${goos}_amd64"
$mirrorDir = "mirror/registry.opentofu.org/ineb01/librechat/$mirrorVersion/$target"
New-Item -ItemType Directory -Force $mirrorDir | Out-Null
Copy-Item $binary -Destination "$mirrorDir/$name$ext" -Force

$absoluteMirror = (Resolve-Path mirror).Path

# Forward slashes: this file is HCL, where a backslash starts an escape sequence, so a Windows
# path written literally produces "invalid escape sequence" or a silently wrong directory.
$hclPath = $absoluteBin -replace '\\', '/'
$hclMirror = $absoluteMirror -replace '\\', '/'

@"
# Written by build.ps1 - do not commit, and do not put it in %APPDATA%\tofurc.
#
# dev_overrides makes OpenTofu load the provider straight out of the build directory and skip
# the registry entirely. Two consequences worth knowing:
#
#   - `tofu init` will warn that dev_overrides is in effect, and any lock entry for this
#     provider is ignored. That warning is the mechanism working, not a problem.
#   - It applies to every configuration run with this file, so keep it opt-in through
#     TF_CLI_CONFIG_FILE instead of installing it globally.
provider_installation {
  dev_overrides {
    "ineb01/librechat" = "$hclPath"
  }

  # Only for `tofu init`, which ignores dev_overrides and would otherwise try to fetch
  # ineb01/librechat from a registry that has never heard of it. Pinned at $mirrorVersion; the
  # version is meaningless because dev_overrides supplies the actual binary at run time.
  filesystem_mirror {
    path    = "$hclMirror"
    include = ["registry.opentofu.org/ineb01/librechat"]
  }

  # Everything else still resolves normally. Without this block the two above would be the only
  # installation methods and kreuzwerker/docker would stop being found. The exclude keeps init
  # from also reaching out for the provider the mirror already answers for.
  direct {
    exclude = ["registry.opentofu.org/ineb01/librechat"]
  }
}
"@ | Set-Content -Path dev.tofurc -Encoding utf8

Write-Host ''
Write-Host "built    $absoluteBin\$name$ext  ($version)" -ForegroundColor Green
Write-Host "override $PSScriptRoot\dev.tofurc" -ForegroundColor Green
Write-Host ''
Write-Host 'To use it:' -ForegroundColor Cyan
Write-Host "  `$env:TF_CLI_CONFIG_FILE = '$PSScriptRoot\dev.tofurc'"
Write-Host "  `$env:LIBRECHAT_MONGO_URI = 'mongodb://127.0.0.1:27017/LibreChat'"
Write-Host '  tofu plan     # no init needed, and none wanted'
