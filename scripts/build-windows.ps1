$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags "-s -w -H=windowsgui" -o wb2api.exe ./cmd/server
} finally {
    Pop-Location
}
