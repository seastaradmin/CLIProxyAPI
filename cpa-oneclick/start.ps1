# Start CLIProxyAPI (CPA) on Windows.
$ErrorActionPreference = "Stop"

$OneClickDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$Root = if ($env:CPA_ROOT) { $env:CPA_ROOT } else { Resolve-Path (Join-Path $OneClickDir "..") }
$Config = if ($env:CPA_CONFIG) { $env:CPA_CONFIG } else { Join-Path $Root "config.yaml" }
$Example = Join-Path $Root "config.example.yaml"
$Bin = if ($env:CPA_BIN) { $env:CPA_BIN } else {
  $exe = Join-Path $Root "cli-proxy-api.exe"
  $unix = Join-Path $Root "cli-proxy-api"
  if (Test-Path $exe) { $exe } else { $unix }
}
$LogDir = Join-Path $OneClickDir "logs"
$LogFile = Join-Path $LogDir "cpa.log"
$PidFile = Join-Path $LogDir "cpa.pid"

New-Item -ItemType Directory -Force -Path $LogDir | Out-Null

function Get-Port {
  if ($env:CPA_PORT) { return $env:CPA_PORT }
  if (Test-Path $Config) {
    $m = Select-String -Path $Config -Pattern '^\s*port:\s*(\d+)' | Select-Object -First 1
    if ($m) { return $m.Matches[0].Groups[1].Value }
  }
  return "8317"
}

if (Test-Path $PidFile) {
  $oldPid = (Get-Content $PidFile -Raw).Trim()
  if ($oldPid -and (Get-Process -Id $oldPid -ErrorAction SilentlyContinue)) {
    Write-Host "[cpa-oneclick] already running (pid=$oldPid)"
    $port = Get-Port
    Write-Host "[cpa-oneclick] API:        http://127.0.0.1:$port"
    Write-Host "[cpa-oneclick] Management: http://127.0.0.1:$port/management.html"
    exit 0
  }
  Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
}

if (-not (Test-Path $Config)) {
  if (-not (Test-Path $Example)) {
    throw "config not found: $Config"
  }
  Copy-Item $Example $Config
  Write-Host "[cpa-oneclick] created config from example: $Config"
}

if (-not (Test-Path $Bin)) {
  if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "binary not found: $Bin (and go is not installed)"
  }
  Write-Host "[cpa-oneclick] building binary..."
  Push-Location $Root
  try {
    go build -o $Bin ./cmd/server
  } finally {
    Pop-Location
  }
}

Write-Host "[cpa-oneclick] starting CPA from $Root"
$proc = Start-Process -FilePath $Bin `
  -ArgumentList @("--config", $Config) `
  -WorkingDirectory $Root `
  -RedirectStandardOutput $LogFile `
  -RedirectStandardError $LogFile `
  -PassThru `
  -WindowStyle Hidden

Set-Content -Path $PidFile -Value $proc.Id -NoNewline
$port = Get-Port
Write-Host "[cpa-oneclick] started (pid=$($proc.Id))"
Write-Host "[cpa-oneclick] API:        http://127.0.0.1:$port"
Write-Host "[cpa-oneclick] Management: http://127.0.0.1:$port/management.html"
Write-Host "[cpa-oneclick] Log file:   $LogFile"
