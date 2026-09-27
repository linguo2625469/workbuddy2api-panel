param(
    [ValidateSet("Console", "Tray", "All")]
    [string]$Mode = "Console"
)

$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $env:CGO_ENABLED = "0"
    if ($Mode -in @("Console", "All")) {
        go build -trimpath -ldflags "-s -w" -o wb2api.exe ./cmd/server
        if ($LASTEXITCODE -ne 0) { throw "console build failed" }
    }
    if ($Mode -in @("Tray", "All")) {
        go build -tags tray -trimpath -ldflags "-s -w -H=windowsgui" -o wb2api-tray.exe ./cmd/server
        if ($LASTEXITCODE -ne 0) { throw "tray build failed" }
    }
} finally {
    Pop-Location
}
