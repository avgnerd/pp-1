<#
.SYNOPSIS
  Installs a static Windows FFmpeg build that includes libx264.

.DESCRIPTION
  The GitHub-hosted Windows images do not ship ffmpeg. The Go media tests skip without it and the
  streaming E2E fixture encodes H.264, so CI must install it. The official BtbN gpl build bundles
  libx264 (the lgpl variant does not) and publishes checksums.sha256 next to each asset.

  The install lives in one fixed directory under the OS temp directory, tagged with a marker file.
  An existing directory without that marker is never modified; the script stops with a clear error.
  Reinstalls extract into a new stage directory instead of deleting the old tree, so no recursive
  delete ever runs against a path a caller supplied.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$asset = "ffmpeg-n9.0-latest-win64-gpl-9.0.zip"
$baseUrl = "https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/latest"

$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')
$installRoot = Join-Path $tempRoot "dvfs-ffmpeg"
$marker = Join-Path $installRoot ".dvfs-ci-ffmpeg"

function Assert-TempChild([string]$path, [string]$label) {
  $full = [IO.Path]::GetFullPath($path).TrimEnd('\')
  if (-not $full.StartsWith($tempRoot + '\', [StringComparison]::OrdinalIgnoreCase)) {
    throw "$label is outside the OS temp directory: $full"
  }
  return $full
}

# Returns the item, or $null when absent. Reparse points are rejected, never followed.
function Get-RealItem([string]$path, [string]$label) {
  $item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
  if (-not $item) { return $null }
  if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) {
    throw "$label is a reparse point; refusing to use it: $path"
  }
  return $item
}

function Test-FFmpeg([string]$path) {
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { return $false }
  $item = Get-RealItem $path "ffmpeg binary"
  if (-not $item) { return $false }
  try {
    $encoders = & $path -hide_banner -encoders 2>&1 | Out-String
  } catch {
    return $false
  }
  if ($LASTEXITCODE -ne 0) { return $false }
  return $encoders -match "\blibx264\b"
}

# Scans the layouts this script can produce: <root>\bin, <root>\<install>\bin, and
# <root>\<stage>\<install>\bin. Directory links are skipped, and every candidate must stay in root.
function Find-FFmpeg {
  $candidates = @(Join-Path $installRoot "bin\ffmpeg.exe")
  foreach ($dir in Get-ChildItem -LiteralPath $installRoot -Directory -Force -ErrorAction SilentlyContinue) {
    if ($dir.Attributes -band [IO.FileAttributes]::ReparsePoint) { continue }
    $candidates += Join-Path $dir.FullName "bin\ffmpeg.exe"
    foreach ($nested in Get-ChildItem -LiteralPath $dir.FullName -Directory -Force -ErrorAction SilentlyContinue) {
      if ($nested.Attributes -band [IO.FileAttributes]::ReparsePoint) { continue }
      $candidates += Join-Path $nested.FullName "bin\ffmpeg.exe"
    }
  }
  foreach ($candidate in $candidates) {
    $full = [IO.Path]::GetFullPath($candidate)
    if (-not $full.StartsWith($installRoot + '\', [StringComparison]::OrdinalIgnoreCase)) { continue }
    $binDir = Split-Path -Parent $full
    $binItem = Get-Item -LiteralPath $binDir -Force -ErrorAction SilentlyContinue
    if (-not $binItem -or -not $binItem.PSIsContainer) { continue }
    if ($binItem.Attributes -band [IO.FileAttributes]::ReparsePoint) { continue }
    if (Test-FFmpeg $full) { return $full }
  }
  return $null
}

function Add-ToPath([string]$dir) {
  $env:Path = "$dir;$env:Path"
  if ($env:GITHUB_PATH) { Add-Content -LiteralPath $env:GITHUB_PATH -Value $dir -Encoding utf8 }
}

function Initialize-InstallRoot {
  $full = Assert-TempChild $installRoot "ffmpeg install directory"
  $item = Get-RealItem $full "ffmpeg install directory"
  if ($item) {
    if (-not $item.PSIsContainer) { throw "ffmpeg install path is not a directory: $full" }
    if (-not (Test-Path -LiteralPath $marker -PathType Leaf)) {
      throw "refusing to modify ${full}: it exists but was not created by this script (missing $marker)"
    }
    return
  }
  New-Item -ItemType Directory -Path $full -ErrorAction Stop | Out-Null
  $created = Get-RealItem $full "ffmpeg install directory"
  if (-not $created.PSIsContainer) { throw "failed to create ffmpeg install directory: $full" }
  Set-Content -LiteralPath $marker -Value "created by scripts/ci/prepare-ffmpeg.ps1" -Encoding ascii
}

Initialize-InstallRoot

$existing = Find-FFmpeg
if ($existing) {
  Write-Host "ffmpeg with libx264 already present: $existing"
  Add-ToPath (Split-Path -Parent $existing)
  & $existing -hide_banner -version | Select-Object -First 1
  exit 0
}

$work = Assert-TempChild (Join-Path $tempRoot ("dvfs-ffmpeg-dl-" + [guid]::NewGuid().ToString("N"))) "download directory"
New-Item -ItemType Directory -Path $work -ErrorAction Stop | Out-Null
$workItem = Get-RealItem $work "download directory"
if (-not $workItem.PSIsContainer) { throw "failed to create download directory: $work" }
try {
  $zip = Join-Path $work $asset
  $sums = Join-Path $work "checksums.sha256"
  Write-Host "Downloading $asset"
  # The asset is about 190 MB, and the release CDN answers 504 on some paths, so allow a retry.
  foreach ($attempt in 1..2) {
    try {
      Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$asset" -OutFile $zip -TimeoutSec 900 -ErrorAction Stop
      Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/checksums.sha256" -OutFile $sums -TimeoutSec 120 -ErrorAction Stop
      break
    } catch {
      if ($attempt -eq 2) { throw }
      Write-Host "download failed, retrying: $($_.Exception.Message)"
      Start-Sleep -Seconds 5
    }
  }

  $line = Get-Content -LiteralPath $sums | Where-Object { $_ -match [regex]::Escape($asset) + '$' } | Select-Object -First 1
  if (-not $line) { throw "checksums.sha256 has no entry for $asset" }
  $expected = ($line -split '\s+')[0].ToLower()
  $actual = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLower()
  if ($expected -ne $actual) { throw "checksum mismatch for $asset (expected $expected, got $actual)" }

  $stage = Assert-TempChild (Join-Path $installRoot ("install-" + [guid]::NewGuid().ToString("N"))) "install stage"
  New-Item -ItemType Directory -Path $stage -ErrorAction Stop | Out-Null
  $stageItem = Get-RealItem $stage "install stage"
  if (-not $stageItem.PSIsContainer) { throw "failed to create install stage: $stage" }
  Expand-Archive -LiteralPath $zip -DestinationPath $stage -Force -ErrorAction Stop
} finally {
  Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

$ffmpeg = Find-FFmpeg
if (-not $ffmpeg) { throw "installed ffmpeg does not report the libx264 encoder under $installRoot" }
Add-ToPath (Split-Path -Parent $ffmpeg)
Write-Host "ffmpeg with libx264 ready: $ffmpeg"
& $ffmpeg -hide_banner -version | Select-Object -First 1
exit 0
