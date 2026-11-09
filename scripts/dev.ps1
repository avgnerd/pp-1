<#
.SYNOPSIS
  Development mode: Go server on :8080 plus the Vite dev server on :5173 (proxying /api).

.PARAMETER Fake  Also start the in-memory fake Dropbox on :9900 and use a separate data
                 folder (data-dev) whose config.json points at it. Storages with app keys
                 app1, app2, app3 (any secret) connect instantly with sample files.
#>
[CmdletBinding()]
param([switch]$Fake)

$ErrorActionPreference = "Continue"  # see run-local.ps1: native stderr must not abort
$Root = Split-Path -Parent $PSScriptRoot
$Data = Join-Path $Root ($(if ($Fake) { "data-dev" } else { "data" }))
$jobs = @()

if ($Fake) {
  New-Item -ItemType Directory -Force -Path $Data | Out-Null
  $cfgPath = Join-Path $Data "config.json"
  if (-not (Test-Path $cfgPath)) {
    @{
      listen = ":8080"; public_url = ""; trusted_proxies = @()
      ffmpeg_path = ""; ffprobe_path = ""
      dropbox_api_base = "http://127.0.0.1:9900"; dropbox_content_base = "http://127.0.0.1:9900"
      dropbox_authorize_url = "http://127.0.0.1:9900/oauth2/authorize"; dropbox_token_url = "http://127.0.0.1:9900/oauth2/token"
    } | ConvertTo-Json | Set-Content -Encoding utf8 $cfgPath
  }
  Write-Host "==> fake Dropbox :9900" -ForegroundColor Cyan
  $jobs += Start-Process -PassThru -NoNewWindow -WorkingDirectory (Join-Path $Root "backend") go -ArgumentList "run", "./cmd/fakedropbox", "-addr", "127.0.0.1:9900"
}

Write-Host "==> Go server :8080 (data: $Data)" -ForegroundColor Cyan
$jobs += Start-Process -PassThru -NoNewWindow -WorkingDirectory (Join-Path $Root "backend") go -ArgumentList "run", "./cmd/server", "-data", $Data

try {
  Write-Host "==> Vite :5173  ->  http://localhost:5173" -ForegroundColor Cyan
  Push-Location (Join-Path $Root "frontend")
  npm run dev
} finally {
  Pop-Location
  foreach ($j in $jobs) { if ($j -and -not $j.HasExited) { Stop-Process -Id $j.Id -Force -ErrorAction SilentlyContinue } }
}
