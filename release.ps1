# Cuts a release from this machine: build + publish both binaries.
#
# Hosted Actions runners are disabled in this GitHub org, so .github/workflows/
# cannot do it. Requires the gh CLI, authenticated once with `gh auth login`.
#
# The commit must already be pushed — the tag is created on it remotely.

$ErrorActionPreference = 'Stop'

$Root = $PSScriptRoot
$Sha  = (& git -C $Root rev-parse HEAD).Trim()

# ponytail: commit count stands in for the CI run number — monotonic, needs no
# API call and no tag parsing. Pass a version explicitly if you outgrow v0.1.x.
$Tag = if ($args[0]) { $args[0] } else { "v0.1.$((& git -C $Root rev-list --count HEAD).Trim())" }

$env:VERSION = $Tag
& (Join-Path $Root 'build.ps1')
if ($LASTEXITCODE -ne 0) { throw "build.ps1 failed: $LASTEXITCODE" }

& gh release create $Tag `
    (Join-Path $Root 'bin\engram-ui.exe') `
    (Join-Path $Root 'internal\launcher\embedded\engram.exe') `
    --target $Sha `
    --generate-notes
if ($LASTEXITCODE -ne 0) { throw "gh release create failed: $LASTEXITCODE" }

Write-Host "released $Tag @ $Sha"
