# Cuts a release from this machine: build + publish both binaries.
#
# Hosted Actions runners are disabled in this GitHub org, so .github/workflows/
# cannot do it. Requires the gh CLI, authenticated once with `gh auth login`.
#
# The commit must already be pushed — the tag is created on it remotely.

$ErrorActionPreference = 'Stop'

$Root = $PSScriptRoot
$Sha  = (& git -C $Root rev-parse HEAD).Trim()

# Name the target repo from origin: gh guesses it from the remotes and skips hosts
# it does not know, so an origin behind an SSH alias (git@github-personal:...)
# made it publish to upstream instead.
$Origin = (& git -C $Root remote get-url origin).Trim()
if ($Origin -notmatch '[:/]([^/:]+)/([^/]+?)(\.git)?$') { throw "cannot read owner/repo from origin: $Origin" }
$Owner = $Matches[1]
$Repo  = "$Owner/$($Matches[2])"

# A GH_TOKEN in the environment overrides `gh auth switch`. When gh holds a login
# for the repo owner, publish with it (this process only); otherwise keep the active one.
$OwnerToken = try { & gh auth token --user $Owner 2>$null } catch { $null }
if ($OwnerToken) { $env:GH_TOKEN = "$OwnerToken".Trim() }

# ponytail: commit count stands in for the CI run number — monotonic, needs no
# API call and no tag parsing. Pass a version explicitly if you outgrow v0.1.x.
$Tag = if ($args[0]) { $args[0] } else { "v0.1.$((& git -C $Root rev-list --count HEAD).Trim())" }

$env:VERSION = $Tag
& (Join-Path $Root 'build.ps1')
if ($LASTEXITCODE -ne 0) { throw "build.ps1 failed: $LASTEXITCODE" }

& gh release create $Tag `
    (Join-Path $Root 'bin\engram-ui.exe') `
    (Join-Path $Root 'internal\launcher\embedded\engram.exe') `
    --repo $Repo `
    --target $Sha `
    --generate-notes
if ($LASTEXITCODE -ne 0) { throw "gh release create failed: $LASTEXITCODE" }

Write-Host "released $Tag @ $Sha to $Repo"
