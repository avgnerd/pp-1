<#
.SYNOPSIS
  Copies the built frontend into the directory the Go server embeds.

.DESCRIPTION
  backend/internal/web embeds all:dist, so the E2E smoke server serves frontend/dist only after it
  is copied there. Both directories are fixed relative to this script's repository, so no caller can
  point the recursive delete at another directory. The embed directory keeps its .keep file so the
  embedded package is never empty.

  A reparse point (junction or symlink) anywhere in the embed directory stops the script instead of
  being followed, because deleting through one would reach outside the repository.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"

$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot "..\.."))
$source = [IO.Path]::GetFullPath((Join-Path $root "frontend\dist"))
$target = [IO.Path]::GetFullPath((Join-Path $root "backend\internal\web\dist"))

function Assert-UnderRoot([string]$path, [string]$label) {
  $prefix = $root.TrimEnd('\') + '\'
  if (-not $path.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "$label is outside the repository at $root : $path"
  }
}

# Both paths prove this is the expected repository layout, not a stray script copy.
Assert-UnderRoot $source "frontend build directory"
Assert-UnderRoot $target "embed directory"
foreach ($file in @("backend\go.mod", "frontend\package.json")) {
  if (-not (Test-Path -LiteralPath (Join-Path $root $file) -PathType Leaf)) {
    throw "$root does not look like the repository: $file is missing"
  }
}
if (-not (Test-Path -LiteralPath (Join-Path $target ".keep") -PathType Leaf)) {
  throw "$target is not the embed directory: .keep is missing"
}

function Get-RealItem([string]$path, [string]$label) {
  $item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
  if (-not $item) { throw "$label is missing: $path" }
  if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) {
    throw "$label is a reparse point; refusing to use it: $path"
  }
  return $item
}

$sourceItem = Get-RealItem $source "frontend build directory"
if (-not $sourceItem.PSIsContainer) { throw "$source is not a directory" }
if (-not (Test-Path -LiteralPath (Join-Path $source "index.html") -PathType Leaf)) {
  throw "$source\index.html is missing. Run npm run build in frontend first."
}

$targetItem = Get-RealItem $target "embed directory"
if (-not $targetItem.PSIsContainer) { throw "$target is not a directory" }

# Check every entry before deleting any, so a link or a locked file leaves the directory untouched.
$children = @(Get-ChildItem -LiteralPath $target -Force)
foreach ($child in $children) {
  if ($child.Attributes -band [IO.FileAttributes]::ReparsePoint) {
    throw "refusing to remove reparse point $($child.FullName); remove it manually if it is stale"
  }
}
foreach ($child in $children) {
  if ($child.Name -eq ".keep") { continue }
  Remove-Item -LiteralPath $child.FullName -Recurse -Force
}

Get-ChildItem -LiteralPath $source -Force | ForEach-Object {
  Copy-Item -LiteralPath $_.FullName -Destination $target -Recurse -Force
}

if (-not (Test-Path -LiteralPath (Join-Path $target "index.html") -PathType Leaf)) {
  throw "embed copy failed: $target\index.html is missing"
}
Write-Host "embedded $source into $target"
