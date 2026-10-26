# Updates the engram submodule to latest origin/main, rebuilds, runs tests.
# Use as the foundation for the weekly auto-bump CI workflow.

$ErrorActionPreference = 'Stop'

$Root = $PSScriptRoot

Push-Location (Join-Path $Root 'third_party\engram')
try {
    & git fetch origin
    if ($LASTEXITCODE -ne 0) { throw "git fetch failed: $LASTEXITCODE" }
    & git checkout origin/main
    if ($LASTEXITCODE -ne 0) { throw "git checkout failed: $LASTEXITCODE" }
    $New = (& git rev-parse --short HEAD).Trim()
}
finally {
    Pop-Location
}

& (Join-Path $Root 'build-engram.ps1')
if ($LASTEXITCODE -ne 0) { throw "build-engram.ps1 failed: $LASTEXITCODE" }

Push-Location $Root
try {
    & go test ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed: $LASTEXITCODE" }
    & git add third_party/engram
    Write-Host "engram bumped to $New; review and commit"
}
finally {
    Pop-Location
}
