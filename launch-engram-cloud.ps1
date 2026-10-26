# Launches the engram cloud stack (postgres + cloud) from
# third_party/engram/docker-compose.cloud.yml using podman.
#
# Why two steps instead of `podman compose up`:
#   podman-compose 1.5.0 on Windows mis-parses the absolute build context
#   "D:\..." as a git URL (drive letter looks like a URL scheme), so it drops
#   the "-f docker/cloud/Dockerfile" arg and the build fails. We build the
#   image directly (which works), tag it as the name compose expects
#   ({project}_{service} = engram_cloud), then bring the stack up with
#   --no-build so compose skips its broken build path.

$ErrorActionPreference = 'Stop'

$Root    = $PSScriptRoot
$Sub     = Join-Path $Root 'third_party\engram'
$Compose = Join-Path $Sub 'docker-compose.cloud.yml'

if (-not (Test-Path $Compose)) {
    Write-Error "compose file not found: $Compose (is the submodule initialized?)"
}

Push-Location $Sub
try {
    & podman build -t engram_cloud -f docker/cloud/Dockerfile .
    if ($LASTEXITCODE -ne 0) { throw "podman build failed: $LASTEXITCODE" }

    & podman compose -f docker-compose.cloud.yml up -d --no-build
    if ($LASTEXITCODE -ne 0) { throw "podman compose up failed: $LASTEXITCODE" }

    Write-Host ''
    Write-Host 'engram cloud stack up:'
    Write-Host '  cloud    -> http://127.0.0.1:18080/health'
    Write-Host '  postgres -> 127.0.0.1:5433'
}
finally {
    Pop-Location
}
