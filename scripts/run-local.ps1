<#
.SYNOPSIS
  Builds and runs Dropbox VFS on Windows.

.DESCRIPTION
  1. Downloads FFmpeg into tools\ffmpeg when ffmpeg is neither there nor on PATH
     (BtbN win64 GPL build from GitHub, SHA-256 verified).
  2. Builds the frontend (npm) and embeds it into the Go server.
  3. Builds bin\dvfs.exe and runs it with the data folder .\data
     (config.json is created there on first run).
  4. Opens the site in the default browser.

.PARAMETER Port      Listen port (default 8080). Also used in the localhost OAuth redirect URI.
.PARAMETER DataDir   Data folder (default <repo>\data).
.PARAMETER SkipBuild Run the existing bin\dvfs.exe without rebuilding.
.PARAMETER NoBrowser Do not open the browser.
.PARAMETER NoFFmpeg  Skip the FFmpeg download (original playback only).
#>
[CmdletBinding()]
param(
  [int]$Port = 8080,
  [string]$DataDir = "",
  [switch]$SkipBuild,
  [switch]$NoBrowser,
  [switch]$NoFFmpeg
)

# "Continue", not "Stop": Windows PowerShell 5.1 turns stderr lines of native tools (npm notices,
# go download logs) into errors when output is redirected, and "Stop" would abort the build.
# Failures are detected through $LASTEXITCODE and explicit throws instead.
$ErrorActionPreference = "Continue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$Root = Split-Path -Parent $PSScriptRoot
if (-not $DataDir) { $DataDir = Join-Path $Root "data" }
$Bin = Join-Path $Root "bin\dvfs.exe"

function Step($msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Need($cmd, $hint) {
  if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) {
    Write-Host "'$cmd'을(를) 찾을 수 없습니다. $hint" -ForegroundColor Red
    exit 1
  }
}

# ---------- FFmpeg ----------
function Ensure-FFmpeg {
  $toolDir = Join-Path $Root "tools\ffmpeg"
  $exe = Join-Path $toolDir "bin\ffmpeg.exe"
  if (Test-Path $exe) { return }
  if (Get-Command ffmpeg -ErrorAction SilentlyContinue) {
    Step "PATH의 ffmpeg를 사용합니다: $((Get-Command ffmpeg).Source)"
    return
  }
  Step "FFmpeg를 tools\ffmpeg에 내려받습니다 (약 150MB)"
  $base = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest"
  $name = "ffmpeg-master-latest-win64-gpl.zip"
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("dvfs-ffmpeg-" + [guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    $zip = Join-Path $tmp $name
    $sums = Join-Path $tmp "checksums.sha256"
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile $zip -ErrorAction Stop
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.sha256" -OutFile $sums -ErrorAction Stop
    $line = Get-Content $sums | Where-Object { $_ -match [regex]::Escape($name) + '$' } | Select-Object -First 1
    if (-not $line) { throw "checksums.sha256에 $name 항목이 없습니다" }
    $expected = ($line -split '\s+')[0].ToLower()
    $actual = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLower()
    if ($expected -ne $actual) { throw "FFmpeg 체크섬이 맞지 않습니다 (expected $expected, got $actual)" }
    Expand-Archive -Path $zip -DestinationPath $tmp -Force -ErrorAction Stop
    $inner = Get-ChildItem $tmp -Directory | Where-Object { Test-Path (Join-Path $_.FullName "bin\ffmpeg.exe") } | Select-Object -First 1
    if (-not $inner) { throw "압축 파일에서 bin\ffmpeg.exe를 찾지 못했습니다" }
    New-Item -ItemType Directory -Force -Path $toolDir | Out-Null
    Copy-Item -Recurse -Force (Join-Path $inner.FullName "bin") $toolDir -ErrorAction Stop
    Step "FFmpeg 준비 완료: $exe"
  } finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
}

# ---------- build ----------
function Build-All {
  Need "go" "Go 1.25 이상을 설치하세요: https://go.dev/dl/"
  Need "npm" "Node.js 20 이상을 설치하세요: https://nodejs.org/"
  $fe = Join-Path $Root "frontend"
  Push-Location $fe
  try {
    if (-not (Test-Path "node_modules")) {
      Step "프론트엔드 패키지 설치 (npm ci)"
      npm ci --no-audit --no-fund
      if ($LASTEXITCODE -ne 0) { throw "npm ci 실패" }
    }
    Step "프론트엔드 빌드"
    npm run build
    if ($LASTEXITCODE -ne 0) { throw "프론트엔드 빌드 실패" }
  } finally { Pop-Location }

  $embed = Join-Path $Root "backend\internal\web\dist"
  Get-ChildItem $embed -Force | Where-Object { $_.Name -ne ".keep" } | Remove-Item -Recurse -Force
  Copy-Item -Recurse -Force (Join-Path $fe "dist\*") $embed -ErrorAction Stop

  Step "Go 서버 빌드"
  Push-Location (Join-Path $Root "backend")
  try {
    $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags "-s -w" -o $Bin ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "go build 실패" }
  } finally { Pop-Location }
}

if (-not $NoFFmpeg) { Ensure-FFmpeg }
if (-not $SkipBuild -or -not (Test-Path $Bin)) { Build-All }

$env:DVFS_LISTEN = ":$Port"
$url = "http://localhost:$Port"
Step "실행: $url  (데이터: $DataDir, 첫 로그인 admin / 1234, 끝내려면 Ctrl+C)"
Write-Host "    Dropbox App Console에 등록할 Redirect URI: $url/api/oauth/callback" -ForegroundColor Yellow
if (-not $NoBrowser) {
  Start-Job -ScriptBlock { param($u) Start-Sleep -Seconds 2; Start-Process $u } -ArgumentList $url | Out-Null
}
Push-Location $Root
try {
  & $Bin -data $DataDir
} finally { Pop-Location }
