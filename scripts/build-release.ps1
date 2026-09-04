[CmdletBinding()]
param(
    [string]$Version = $(if ($env:CODEXMARATHON_VERSION) { $env:CODEXMARATHON_VERSION } else { '0.1.0' }),
    [string]$Platform = $(if ($env:CODEXMARATHON_PLATFORM) { $env:CODEXMARATHON_PLATFORM } else { 'windows-x86_64' }),
    [string]$Output = $(if ($env:CODEXMARATHON_OUTPUT) { $env:CODEXMARATHON_OUTPUT } else { 'dist/release' })
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$buildDir = Join-Path $repoRoot 'dist/build'
$outputDir = if ([IO.Path]::IsPathRooted($Output)) { $Output } else { Join-Path $repoRoot $Output }
New-Item -ItemType Directory -Force -Path $buildDir, $outputDir | Out-Null

Push-Location (Join-Path $repoRoot 'controller')
try {
    & go build -trimpath -o (Join-Path $buildDir 'codexmarathon.exe') ./cmd/codexmarathon
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
}
finally { Pop-Location }

Push-Location (Join-Path $repoRoot 'runtime/codex-rs')
try {
    & cargo build --locked --release -p codex-app-server
    if ($LASTEXITCODE -ne 0) { throw "cargo build failed with exit code $LASTEXITCODE" }
}
finally { Pop-Location }

& python (Join-Path $repoRoot 'scripts/package_release.py') `
    --root $repoRoot `
    --output $outputDir `
    --controller-binary (Join-Path $buildDir 'codexmarathon.exe') `
    --runtime-binary (Join-Path $repoRoot 'runtime/codex-rs/target/release/codex-app-server.exe') `
    --platform $Platform `
    --version $Version `
    --format zip `
    --windows-exe
if ($LASTEXITCODE -ne 0) { throw "release packaging failed with exit code $LASTEXITCODE" }

Write-Host "Release archive created under $outputDir. Run scripts/live_smoke.py separately to obtain live runtime evidence."
