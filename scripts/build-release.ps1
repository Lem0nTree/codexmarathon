[CmdletBinding()]
param(
    [string]$Output = $(if ($env:CODEXMARATHON_OUTPUT) { $env:CODEXMARATHON_OUTPUT } else { 'dist/release' })
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$runtimeDir = Join-Path $repoRoot 'runtime/codex-rs'
$outputDir = if ([IO.Path]::IsPathRooted($Output)) { $Output } else { Join-Path $repoRoot $Output }
New-Item -ItemType Directory -Force -Path $outputDir | Out-Null

Push-Location $runtimeDir
try {
    & cargo build --locked --release -p codex-cli
    if ($LASTEXITCODE -ne 0) { throw "cargo build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

Copy-Item -LiteralPath (Join-Path $runtimeDir 'target/release/codex.exe') -Destination (Join-Path $outputDir 'codex.exe') -Force
Write-Host "Native CodexMarathon CLI created at $(Join-Path $outputDir 'codex.exe')."
