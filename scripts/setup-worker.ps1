# Creates worker/.venv with the ML dependencies.
# Uses uv (recommended: winget install astral-sh.uv). uv downloads an official
# CPython into worker/.python, so nothing is installed system-wide.
$ErrorActionPreference = "Stop"
$worker = Join-Path $PSScriptRoot "..\worker" | Resolve-Path

if (-not (Get-Command uv -ErrorAction SilentlyContinue)) {
    Write-Error "Chưa có uv. Cài bằng: winget install astral-sh.uv  (hoặc https://docs.astral.sh/uv/)"
}

Push-Location $worker
try {
    $env:UV_PYTHON_INSTALL_DIR = Join-Path $worker ".python"
    uv venv --python 3.12 --managed-python .venv
    uv pip install --python .venv\Scripts\python.exe -r requirements.txt
    Write-Host "`nWorker sẵn sàng: $worker\.venv" -ForegroundColor Green
} finally {
    Pop-Location
}
