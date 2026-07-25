# Stop CLIProxyAPI (CPA) on Windows.
$ErrorActionPreference = "Stop"

$OneClickDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$LogDir = Join-Path $OneClickDir "logs"
$PidFile = Join-Path $LogDir "cpa.pid"

if (-not (Test-Path $PidFile)) {
  Write-Host "[cpa-oneclick] not running (no pid file)"
  exit 0
}

$pid = (Get-Content $PidFile -Raw).Trim()
if (-not $pid) {
  Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
  Write-Host "[cpa-oneclick] not running"
  exit 0
}

$proc = Get-Process -Id $pid -ErrorAction SilentlyContinue
if (-not $proc) {
  Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
  Write-Host "[cpa-oneclick] not running (stale pid=$pid)"
  exit 0
}

Write-Host "[cpa-oneclick] stopping pid=$pid"
Stop-Process -Id $pid -Force -ErrorAction SilentlyContinue
Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
Write-Host "[cpa-oneclick] stopped"
