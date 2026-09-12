[CmdletBinding()]
param(
    [string]$Output = $(if ($env:CODEXMARATHON_OUTPUT) { $env:CODEXMARATHON_OUTPUT } else { 'dist/release' }),
    [string]$Runtime = $(if ($env:CODEXMARATHON_RUNTIME_DIR) { $env:CODEXMARATHON_RUNTIME_DIR } else { 'runtime/codex-rs' }),
    [string]$Target = $(if ($env:CODEXMARATHON_TARGET) { $env:CODEXMARATHON_TARGET } else { 'x86_64-pc-windows-msvc' }),
    [string]$Version = $(if ($env:CODEXMARATHON_VERSION) { $env:CODEXMARATHON_VERSION } else { 'codexmarathon-dev' })
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$runtimeDir = if ([IO.Path]::IsPathRooted($Runtime)) { $Runtime } else { Join-Path $repoRoot $Runtime }
$outputDir = if ([IO.Path]::IsPathRooted($Output)) { $Output } else { Join-Path $repoRoot $Output }
New-Item -ItemType Directory -Force -Path $outputDir | Out-Null

$binaries = @(
    'codex.exe',
    'codex-code-mode-host.exe',
    'codex-responses-api-proxy.exe',
    'codex-command-runner.exe',
    'codex-windows-sandbox-setup.exe'
)

Push-Location $runtimeDir
try {
    if ($env:CODEXMARATHON_UPSTREAM_COMMIT) {
        $env:STABLE_GIT_COMMIT = $env:CODEXMARATHON_UPSTREAM_COMMIT
    }
    & cargo build --locked --release --target $Target `
        --bin codex `
        --bin codex-code-mode-host `
        --bin codex-responses-api-proxy `
        --bin codex-command-runner `
        --bin codex-windows-sandbox-setup
    if ($LASTEXITCODE -ne 0) { throw "cargo build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

foreach ($binary in $binaries) {
    $source = Join-Path $runtimeDir "target/$Target/release/$binary"
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "Required release binary was not built: $source"
    }
    Copy-Item -LiteralPath $source -Destination (Join-Path $outputDir $binary) -Force
}

$archive = Join-Path $outputDir "$Version-$Target.zip"
Compress-Archive -LiteralPath ($binaries | ForEach-Object { Join-Path $outputDir $_ }) -DestinationPath $archive -Force
& (Join-Path $outputDir 'codex.exe') marathon --help | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Packaged codex.exe is missing the Marathon command surface" }
Write-Host "CodexMarathon package created at $archive ($($binaries -join ', '))."
