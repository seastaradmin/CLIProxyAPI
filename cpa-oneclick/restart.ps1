# Restart CLIProxyAPI (CPA) on Windows.
$ErrorActionPreference = "Stop"
$OneClickDir = Split-Path -Parent $MyInvocation.MyCommand.Path
& (Join-Path $OneClickDir "stop.ps1")
Start-Sleep -Milliseconds 500
& (Join-Path $OneClickDir "start.ps1")
