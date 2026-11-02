# Builds app/build/bin/Immersive.exe. Needs Go 1.24+ (winget install GoLang.Go).
# No Node.js or Wails CLI required: the UI is plain HTML/CSS/JS embedded in the binary.
$ErrorActionPreference = "Stop"
Push-Location (Join-Path $PSScriptRoot "..\app")
try {
    go build -tags desktop,production -ldflags "-H windowsgui -s -w" -o build\bin\Immersive.exe .
    Write-Host "Đã build: app\build\bin\Immersive.exe" -ForegroundColor Green
} finally {
    Pop-Location
}
