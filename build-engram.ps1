# Builds the engram binary from third_party/engram/ and drops it where
# launcher's go:embed slot expects it.
#
# Run manually before `go build ./cmd/engram-ui` if you want the binary
# embedded in engram-ui. Otherwise, run engram serve externally.

$ErrorActionPreference = 'Stop'

$Root = $PSScriptRoot
$Sub  = Join-Path $Root 'third_party\engram'
$Out  = Join-Path $Root 'internal\launcher\embedded'

if (-not (Test-Path (Join-Path $Sub '.git'))) {
    Write-Error 'submodule not initialized; run: git submodule update --init --recursive'
}

New-Item -ItemType Directory -Force -Path $Out | Out-Null

Push-Location $Sub
try {
    $Ext = if ($env:OS -eq 'Windows_NT') { '.exe' } else { '' }
    $Target = Join-Path $Out "engram$Ext"

    # Derive version from the submodule's git tag so the embedded binary reports
    # a real release version (not "dev"). Without this, engram's update check
    # aborts with "development builds do not map to a release version".
    $Version = (& git describe --tags --always).Trim()
    Write-Host "engram version: $Version"

    & go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $Target ./cmd/engram
    if ($LASTEXITCODE -ne 0) { throw "go build failed: $LASTEXITCODE" }
    Write-Host "built engram -> $Target"

    # Windows cannot delete a binary that is running or that an antivirus still
    # holds open, so `go build` renames the old one to `<name>~` and only
    # best-effort deletes it. That leftover matters here: embed.go embeds this
    # whole directory with `all:embedded`, so a stale ~ file is ~20 MB of dead
    # weight inside every engram-ui binary. Drop it before engram-ui is built.
    Remove-Item "$Target~" -Force -ErrorAction SilentlyContinue
    $Commit = (& git rev-parse HEAD).Trim()
    Write-Host "engram commit: $Commit"
}
finally {
    Pop-Location
}
