# Show CLIProxyAPI (CPA) status on Windows.
$ErrorActionPreference = "Stop"

$OneClickDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$Root = if ($env:CPA_ROOT) { $env:CPA_ROOT } else { Resolve-Path (Join-Path $OneClickDir "..") }
$Config = if ($env:CPA_CONFIG) { $env:CPA_CONFIG } else { Join-Path $Root "config.yaml" }
$LogDir = Join-Path $OneClickDir "logs"
$PidFile = Join-Path $LogDir "cpa.pid"
$LogFile = Join-Path $LogDir "cpa.log"

function Get-Port {
  if ($env:CPA_PORT) { return $env:CPA_PORT }
  if (Test-Path $Config) {
    $m = Select-String -Path $Config -Pattern '^\s*port:\s*(\d+)' | Select-Object -First 1
    if ($m) { return $m.Matches[0].Groups[1].Value }
  }
  return "8317"
}

$port = Get-Port
if (Test-Path $PidFile) {
  $pid = (Get-Content $PidFile -Raw).Trim()
  $proc = if ($pid) { Get-Process -Id $pid -ErrorAction SilentlyContinue } else { $null }
  if ($proc) {
    Write-Host "[cpa-oneclick] status: running"
    Write-Host "[cpa-oneclick] pid:    $pid"
    Write-Host "[cpa-oneclick] port:   $port"
    Write-Host "[cpa-oneclick] log:    $LogFile"
    Write-Host "[cpa-oneclick] API:        http://127.0.0.1:$port"
    Write-Host "[cpa-oneclick] Management: http://127.0.0.1:$port/management.html"
    exit 0
  }
}

Write-Host "[cpa-oneclick] status: stopped"
Write-Host "[cpa-oneclick] port:   $port"
exit 1
