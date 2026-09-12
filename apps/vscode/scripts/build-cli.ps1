param(
  [string]$ExtensionRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Fail([string]$Message) {
  Write-Error "Yawr CLI debug build failed: $Message"
  exit 1
}

function Find-Go {
  $fromPath = Get-Command go -ErrorAction SilentlyContinue
  if ($fromPath) {
    return $fromPath.Source
  }

  $candidates = @(
    'C:\Program Files\Go\bin\go.exe',
    'C:\Go\bin\go.exe'
  )
  if ($env:LOCALAPPDATA) {
    $candidates += (Join-Path $env:LOCALAPPDATA 'Programs\Go\bin\go.exe')
  }

  foreach ($candidate in $candidates) {
    if ($candidate -and (Test-Path -LiteralPath $candidate -PathType Leaf)) {
      return $candidate
    }
  }

  return $null
}

$ExtensionRoot = [System.IO.Path]::GetFullPath($ExtensionRoot)
$yawrRepo = [System.IO.Path]::GetFullPath((Join-Path $ExtensionRoot '..\..\runtime'))

if (-not (Test-Path -LiteralPath $yawrRepo -PathType Container)) {
  Fail "expected the monorepo runtime at $yawrRepo, but it does not exist."
}

$goMod = Join-Path $yawrRepo 'go.mod'
$cmdYawr = Join-Path $yawrRepo 'cmd\yawr'
if (-not (Test-Path -LiteralPath $goMod -PathType Leaf) -or -not (Test-Path -LiteralPath $cmdYawr -PathType Container)) {
  Fail "$yawrRepo does not look like the Yawr runtime. Expected go.mod and cmd\yawr."
}

$go = Find-Go
if (-not $go) {
  Fail "Go was not found on PATH or in common Windows install locations. Install Go 1.25+ from https://go.dev/dl/ (or winget install GoLang.Go), then reopen VS Code. Checked PATH, C:\Program Files\Go\bin\go.exe, %LOCALAPPDATA%\Programs\Go\bin\go.exe, and C:\Go\bin\go.exe."
}

Write-Host "Building Yawr CLI using $go in $yawrRepo."
Push-Location $yawrRepo
try {
  & $go build -trimpath -buildvcs=false -o yawr.exe ./cmd/yawr
  if ($LASTEXITCODE -ne 0) {
    Fail "go build exited with code $LASTEXITCODE. Resolve the Go build error above, then press F5 again."
  }
} finally {
  Pop-Location
}

$output = Join-Path $yawrRepo 'yawr.exe'
if (-not (Test-Path -LiteralPath $output -PathType Leaf)) {
  Fail "go build reported success but $output was not created."
}

Write-Host "Yawr CLI debug build complete: $output"
