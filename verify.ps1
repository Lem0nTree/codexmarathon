[CmdletBinding()]
param(
    [switch]$SkipRust
)

# Windows-friendly verification entry point for the native Rust Codex CLI.
# Missing toolchains are reported as BLOCKED so local setup problems are not
# mistaken for source failures.
$ErrorActionPreference = 'Continue'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '.')).Path
$failures = [System.Collections.Generic.List[string]]::new()
$blocked = [System.Collections.Generic.List[string]]::new()

function Pass([string]$Name, [string]$Detail) {
    Write-Host "PASS: $Name - $Detail" -ForegroundColor Green
}

function Fail([string]$Name, [string]$Detail) {
    $script:failures.Add("FAIL: $Name - $Detail")
    Write-Host "FAIL: $Name - $Detail" -ForegroundColor Red
}

function Blocked([string]$Name, [string]$Detail) {
    $script:blocked.Add("BLOCKED: $Name - $Detail")
    Write-Host "BLOCKED: $Name - $Detail" -ForegroundColor Yellow
}

function Require-File([string]$RelativePath) {
    if (Test-Path -LiteralPath (Join-Path $repo $RelativePath) -PathType Leaf) {
        Pass "file $RelativePath" 'present'
    } else {
        Fail "file $RelativePath" 'missing'
    }
}

function Invoke-Tool([string]$Name, [string]$Command, [string]$WorkingDirectory, [string[]]$Arguments) {
    $commandInfo = Get-Command $Command -ErrorAction SilentlyContinue
    if ($null -eq $commandInfo) {
        Blocked $Name "$Command not found; install the Rust toolchain"
        return
    }
    Push-Location -LiteralPath $WorkingDirectory
    try {
        & $commandInfo.Source @Arguments 2>&1
        $exitCode = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    if ($exitCode -eq 0) {
        Pass $Name 'completed'
    } else {
        Fail $Name "exit code $exitCode"
    }
}

Write-Host "CodexMarathon verification: $repo" -ForegroundColor White

foreach ($required in @(
    'README.md',
    'runtime/PROVENANCE.md',
    'runtime/PATCH_LEDGER.md',
    'runtime/codex-rs/Cargo.toml',
    'runtime/codex-rs/Cargo.lock',
    'runtime/codex-rs/LICENSE',
    'runtime/codex-rs/NOTICE',
    'runtime/codex-rs/cli/Cargo.toml',
    'runtime/codex-rs/codexmarathon-runtime/Cargo.toml',
    'runtime/codexmarathon-adapter/Cargo.toml',
    'scripts/verify_provenance.py'
)) {
    Require-File $required
}

if (-not $SkipRust) {
    $runtimeDir = Join-Path $repo 'runtime/codex-rs'
    Invoke-Tool 'Rust formatting check' 'cargo' $runtimeDir @('fmt', '--all', '--', '--check')
    Invoke-Tool 'native Marathon runtime tests' 'cargo' $runtimeDir @('test', '--locked', '-p', 'codexmarathon-runtime')
    Invoke-Tool 'native Codex CLI build' 'cargo' $runtimeDir @('build', '--locked', '--release', '-p', 'codex-cli')
} else {
    Write-Host 'SKIP: Rust checks requested by -SkipRust' -ForegroundColor Gray
}

Write-Host ''
Write-Host 'Verification summary' -ForegroundColor White
Write-Host "  failures: $($failures.Count)" -ForegroundColor Red
Write-Host "  blocked: $($blocked.Count)" -ForegroundColor Yellow
if ($failures.Count -gt 0) {
    Write-Host 'Result: FAILED' -ForegroundColor Red
    exit 1
}
if ($blocked.Count -gt 0) {
    Write-Host 'Result: BLOCKED' -ForegroundColor Yellow
    exit 2
}
Write-Host 'Result: PASS' -ForegroundColor Green
exit 0
