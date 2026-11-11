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
$runStartedUtc = [DateTime]::UtcNow
$diagnosticsPath = Join-Path $ArtifactsDir "diagnostics.json"
# A failed run's snapshot must not survive into a later successful run in the same directory.
# A failed removal is reported, not silently treated as a clean start.
if (Test-Path -LiteralPath $diagnosticsPath -PathType Leaf) {
  try {
    Remove-Item -LiteralPath $diagnosticsPath -Force -ErrorAction Stop
  } catch {
    Write-Host "stale e2e diagnostics could not be removed: $($_.Exception.Message)"
  }
}
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

$originalCode = $code
if ($originalCode -ne 0) {
  # Failure-only snapshot. Every collector is bounded and isolated: a collector or write error is
  # recorded, never allowed to replace the E2E exit code. This runs after the suite has exited.
  try {
    $collectionErrors = New-Object System.Collections.Generic.List[string]
    $collectedAt = [DateTime]::UtcNow

    $processRows = New-Object System.Collections.Generic.List[object]
    $processSummary = New-Object System.Collections.Generic.List[object]
    foreach ($name in @("msedge", "node", "ffmpeg")) {
      try {
        $allProcs = @(Get-Process -Name $name -ErrorAction SilentlyContinue)
        $returned = [Math]::Min($allProcs.Count, 20)
        $processSummary.Add([pscustomobject]@{
          name = $name
          total = $allProcs.Count
          returned = $returned
          truncated = ($allProcs.Count -gt 20)
        })
        foreach ($process in @($allProcs | Select-Object -First 20)) {
          $processRows.Add([pscustomobject]@{
            name = $name
            pid = $process.Id
            cpuSeconds = if ($null -ne $process.CPU) { [math]::Round([double]$process.CPU, 3) } else { $null }
            workingSetMB = [math]::Round([double]$process.WorkingSet64 / 1MB, 1)
            privateMemoryMB = [math]::Round([double]$process.PrivateMemorySize64 / 1MB, 1)
            handles = $process.HandleCount
          })
        }
      } catch {
        $collectionErrors.Add("process " + $name + ": " + $_.Exception.Message)
      }
    }

    $tcpInfo = $null
    try {
      # One query; total and every state count come from this same snapshot.
      $connections = @(Get-CimInstance -Namespace "root/StandardCimv2" -ClassName "MSFT_NetTCPConnection" -ErrorAction Stop)
      $stateNames = @{
        1 = "Closed"; 2 = "Listen"; 3 = "SynSent"; 4 = "SynReceived"; 5 = "Established"
        6 = "FinWait1"; 7 = "FinWait2"; 8 = "CloseWait"; 9 = "Closing"; 10 = "LastAck"
        11 = "TimeWait"; 12 = "DeleteTCB"; 13 = "Bound"
      }
      $byState = New-Object System.Collections.Generic.List[object]
      foreach ($group in @($connections | Group-Object State)) {
        $stateNumber = [int]$group.Name
        $stateLabel = if ($stateNames.ContainsKey($stateNumber)) { $stateNames[$stateNumber] } else { "Unknown" + $stateNumber }
        $byState.Add([pscustomobject]@{ state = $stateLabel; count = $group.Count })
      }
      $browseRangeListen = @($connections | Where-Object {
        [int]$_.State -eq 2 -and [int]$_.LocalPort -ge 22000 -and [int]$_.LocalPort -le 31999
      }).Count
      $tcpInfo = [pscustomobject]@{
        total = $connections.Count
        byState = $byState.ToArray()
        browseRangeListen = $browseRangeListen
      }
    } catch {
      $collectionErrors.Add("tcp: " + $_.Exception.Message)
    }

    $memoryInfo = $null
    try {
      $os = Get-CimInstance Win32_OperatingSystem -ErrorAction Stop
      $drive = Get-PSDrive -Name C -ErrorAction SilentlyContinue
      $memoryInfo = [pscustomobject]@{
        freePhysicalMB = [math]::Round([double]$os.FreePhysicalMemory / 1024, 1)
        totalVisibleMB = [math]::Round([double]$os.TotalVisibleMemorySize / 1024, 1)
        freeVirtualMB = [math]::Round([double]$os.FreeVirtualMemory / 1024, 1)
        totalVirtualMB = [math]::Round([double]$os.TotalVirtualMemorySize / 1024, 1)
        cFreeGB = if ($drive) { [math]::Round([double]$drive.Free / 1GB, 1) } else { $null }
      }
    } catch {
      $collectionErrors.Add("memory: " + $_.Exception.Message)
    }

    $cleanupRows = New-Object System.Collections.Generic.List[object]
    try {
      $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
      $maxJsonBytes = 4096
      # This is candidate selection by time, not proof of ownership. Creation and write times both
      # have to fall after the run started so an old directory cannot be revived by an mtime touch.
      $knownDirs = @(Get-ChildItem -LiteralPath $tempRoot -Directory -Force -ErrorAction Stop |
        Where-Object {
          ($_.Name -like "dvfs-browse-e2e-*" -or $_.Name -like "dvfs-search-e2e-*") -and
          $_.CreationTimeUtc -ge $runStartedUtc -and
          $_.LastWriteTimeUtc -ge $runStartedUtc
        } |
        Sort-Object LastWriteTimeUtc -Descending |
        Select-Object -First 4)
      foreach ($dir in $knownDirs) {
        if ($dir.Attributes -band [IO.FileAttributes]::ReparsePoint) {
          $collectionErrors.Add("cleanup " + $dir.Name + ": reparse point")
          continue
        }
        $row = [ordered]@{ source = $dir.Name; cleanup = $null; runtime = $null }
        $files = @(
          @{ name = "cleanup.json"; target = "cleanup"; fields = @("portReleased", "serverListening", "openStreams") },
          @{ name = "runtime.json"; target = "runtime"; fields = @("port", "pid") }
        )
        foreach ($spec in $files) {
          $file = Join-Path $dir.FullName $spec.name
          if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { continue }
          try {
            $fileItem = Get-Item -LiteralPath $file -Force -ErrorAction Stop
            if ($fileItem.Attributes -band [IO.FileAttributes]::ReparsePoint) {
              $collectionErrors.Add("cleanup " + $dir.Name + " " + $spec.name + ": reparse point")
              continue
            }
            if ($fileItem.Length -gt $maxJsonBytes) {
              $collectionErrors.Add("cleanup " + $dir.Name + " " + $spec.name + ": over size limit")
              continue
            }
            $json = Get-Content -LiteralPath $file -Raw -ErrorAction Stop | ConvertFrom-Json
            $selected = [ordered]@{}
            foreach ($field in $spec.fields) {
              $property = $json.PSObject.Properties[$field]
              if ($null -eq $property) { continue }
              $value = $property.Value
              $validType = if ($field -in @("portReleased", "serverListening")) {
                $value -is [bool]
              } else {
                $value -is [int] -or $value -is [long]
              }
              if ($validType) {
                $selected[$field] = $value
              } else {
                $collectionErrors.Add("cleanup " + $dir.Name + " " + $spec.name + " " + $field + ": invalid type")
              }
            }
            $row[$spec.target] = $selected
          } catch {
            $collectionErrors.Add("cleanup " + $dir.Name + " " + $spec.name + ": " + $_.Exception.Message)
          }
        }
        $cleanupRows.Add([pscustomobject]$row)
      }
    } catch {
      $collectionErrors.Add("cleanup scan: " + $_.Exception.Message)
    }

    $diagnostics = [ordered]@{
      schemaVersion = 1
      collectedAtUtc = $collectedAt.ToString("o")
      originalExitCode = $originalCode
      collectionErrors = $collectionErrors.ToArray()
      processes = $processRows.ToArray()
      processSummary = $processSummary.ToArray()
      tcp = $tcpInfo
      memory = $memoryInfo
      cleanup = $cleanupRows.ToArray()
    }
    try {
      $jsonText = $diagnostics | ConvertTo-Json -Depth 6
      [IO.File]::WriteAllText($diagnosticsPath, $jsonText, (New-Object System.Text.UTF8Encoding($false)))
      Write-Host "e2e diagnostics: $diagnosticsPath"
    } catch {
      Write-Host "e2e diagnostics could not be written: $($_.Exception.Message)"
    }
  } catch {
    Write-Host "e2e diagnostics collection failed: $($_.Exception.Message)"
  }
}

Write-Host "e2e exit code: $originalCode"
Write-Host "e2e artifacts: $ArtifactsDir"
exit $originalCode
