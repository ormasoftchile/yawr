param(
  [string]$ExtensionRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Fail([string]$Message) {
  Write-Error "Yawr extension dependency bootstrap failed: $Message"
  exit 1
}

$ExtensionRoot = [System.IO.Path]::GetFullPath($ExtensionRoot)
$packageJson = Join-Path $ExtensionRoot 'package.json'
$packageLock = Join-Path $ExtensionRoot 'package-lock.json'
$nodeModules = Join-Path $ExtensionRoot 'node_modules'
$installedLock = Join-Path $nodeModules '.package-lock.json'

if (-not (Test-Path -LiteralPath $packageJson -PathType Leaf)) {
  Fail "package.json was not found at $packageJson. Open the Yawr repository root before pressing F5."
}

if (-not (Test-Path -LiteralPath $packageLock -PathType Leaf)) {
  Fail "package-lock.json was not found at $packageLock. This repository expects reproducible installs via npm ci."
}

$npm = Get-Command npm -ErrorAction SilentlyContinue
if (-not $npm) {
  Fail "npm was not found on PATH. Install Node.js 20+ from https://nodejs.org/ and reopen VS Code."
}

$reason = $null
if (-not (Test-Path -LiteralPath $nodeModules -PathType Container)) {
  $reason = 'node_modules is missing'
} elseif (-not (Test-Path -LiteralPath $installedLock -PathType Leaf)) {
  $reason = 'node_modules/.package-lock.json is missing'
} else {
  $installTime = (Get-Item -LiteralPath $installedLock).LastWriteTimeUtc
  $packageTime = (Get-Item -LiteralPath $packageJson).LastWriteTimeUtc
  $lockTime = (Get-Item -LiteralPath $packageLock).LastWriteTimeUtc

  if ($packageTime -gt $installTime -or $lockTime -gt $installTime) {
    $reason = 'package.json or package-lock.json is newer than the installed dependency lock'
  }
}

if (-not $reason) {
  Write-Host 'Yawr extension dependencies are already installed; skipping npm ci.'
  exit 0
}

Write-Host "Installing Yawr extension dependencies with npm ci because $reason."
Push-Location $ExtensionRoot
try {
  & npm ci
  if ($LASTEXITCODE -ne 0) {
    Fail "npm ci exited with code $LASTEXITCODE. Resolve the npm error above, then press F5 again."
  }
} finally {
  Pop-Location
}

Write-Host 'Yawr extension dependency bootstrap complete.'
