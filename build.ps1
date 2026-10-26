# Top-level build: engram binary (from submodule) + engram-ui.

$ErrorActionPreference = 'Stop'

$Root = $PSScriptRoot

& (Join-Path $Root 'build-engram.ps1')
if ($LASTEXITCODE -ne 0) { throw "build-engram.ps1 failed: $LASTEXITCODE" }

Push-Location $Root
try {
    New-Item -ItemType Directory -Force -Path (Join-Path $Root 'bin') | Out-Null

    $Version = if ($env:VERSION) { $env:VERSION } else { 'dev' }
    $Commit  = try { (& git rev-parse --short HEAD 2>$null).Trim() } catch { 'unknown' }
    if (-not $Commit) { $Commit = 'unknown' }
    $Date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

    $LdFlags = "-s -w " +
        "-X github.com/edalandete/engram-ui/internal/version.Version=$Version " +
        "-X github.com/edalandete/engram-ui/internal/version.Commit=$Commit " +
        "-X github.com/edalandete/engram-ui/internal/version.Date=$Date"

    $Ext = if ($env:OS -eq 'Windows_NT') { '.exe' } else { '' }
    $Target = "bin/engram-ui$Ext"

    & go build -trimpath -ldflags $LdFlags -o $Target ./cmd/engram-ui
    if ($LASTEXITCODE -ne 0) { throw "go build failed: $LASTEXITCODE" }
    Write-Host "built $Target"
}
finally {
    Pop-Location
}
