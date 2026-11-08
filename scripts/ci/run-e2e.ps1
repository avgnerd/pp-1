<#
.SYNOPSIS
  Runs the full Edge E2E suite and keeps its reports, traces, and log in one directory.

.DESCRIPTION
  frontend/scripts/run-e2e.mjs already stops at the first failing config and returns Playwright's
  exit code. This wrapper writes the same output to e2e.log and points DVFS_E2E_ARTIFACTS at a fixed
  directory, so a failing run still leaves evidence to upload. It exits with the runner's code.

  Artifact and log problems fail before the suite starts, or fail the step after it, so a run that
  could not record evidence never reports success. The temporary server data directory stays outside
  the artifact directory: it holds app.db and secret.key, which must never be uploaded.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)

$ArtifactsDir = $env:DVFS_E2E_ARTIFACTS
if (-not $ArtifactsDir) {
  $ArtifactsDir = Join-Path ([IO.Path]::GetTempPath()) ("dvfs-e2e-" + [guid]::NewGuid().ToString("N"))
}
$ArtifactsDir = [IO.Path]::GetFullPath($ArtifactsDir)
$log = Join-Path $ArtifactsDir "e2e.log"

try {
  $item = Get-Item -LiteralPath $ArtifactsDir -Force -ErrorAction SilentlyContinue
  if ($item -and -not $item.PSIsContainer) {
    Write-Host "artifacts path is not a directory: $ArtifactsDir"
    exit 1
  }
  if (-not $item) { New-Item -ItemType Directory -Path $ArtifactsDir -ErrorAction Stop | Out-Null }
  # Opening the log is the writability check. A read-only or unusable directory fails here.
  [IO.File]::WriteAllText($log, "")
} catch {
  Write-Host "cannot record E2E evidence in ${ArtifactsDir}: $($_.Exception.Message)"
  exit 1
}

$env:DVFS_E2E_ARTIFACTS = $ArtifactsDir
$env:PLAYWRIGHT_HTML_OPEN = "never"

if (-not (Get-Command npm -ErrorAction SilentlyContinue)) {
  Write-Host "npm was not found on PATH"
  exit 1
}

$code = 1
Push-Location (Join-Path $root "frontend")
try {
  # cmd merges npm's streams into stdout, so stderr warnings stay plain log lines. The exit code is
  # still npm's. A failure to write the log after this point fails the run instead of hiding it.
  & cmd /c "npm run e2e 2>&1" | Tee-Object -FilePath $log -ErrorAction Stop
  $code = $LASTEXITCODE
} catch {
  Write-Host "E2E output could not be recorded in ${log}: $($_.Exception.Message)"
  $code = 1
} finally {
  Pop-Location
}

if (-not (Test-Path -LiteralPath $log -PathType Leaf)) {
  Write-Host "E2E log is missing after the run: $log"
  $code = 1
}

Write-Host "e2e exit code: $code"
Write-Host "e2e artifacts: $ArtifactsDir"
exit $code
