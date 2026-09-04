[CmdletBinding()]
param(
    [switch]$SkipGo,
    [switch]$SkipRust
)

# One Windows-friendly verification entry point. It deliberately keeps going
# after a missing toolchain so the report separates observed checks from
# checks blocked by the host environment.
$ErrorActionPreference = 'Continue'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '.')).Path
$failures = [System.Collections.Generic.List[string]]::new()
$blocked = [System.Collections.Generic.List[string]]::new()
$observed = [System.Collections.Generic.List[string]]::new()

function Pass([string]$Name, [string]$Detail) {
    $script:observed.Add("PASS: $Name - $Detail")
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
    $path = Join-Path $repo $RelativePath
    if (Test-Path -LiteralPath $path -PathType Leaf) {
        Pass "file $RelativePath" 'present'
        return $true
    }
    Fail "file $RelativePath" 'missing'
    return $false
}

function Invoke-Tool([string]$Name, [string]$Command, [string]$WorkingDirectory, [string[]]$Arguments) {
    $commandInfo = Get-Command $Command -ErrorAction SilentlyContinue
    if ($null -eq $commandInfo) {
        Blocked $Name "$Command not found; install the matching toolchain"
        return $false
    }
    Write-Host "RUN: $Name ($Command $($Arguments -join ' '))" -ForegroundColor Cyan
    Push-Location -LiteralPath $WorkingDirectory
    try {
        & $commandInfo.Source @Arguments 2>&1
        $exitCode = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    if ($exitCode -eq 0) {
        Pass $Name 'completed'
        return $true
    }
    Fail $Name "exit code $exitCode"
    return $false
}

Write-Host "CodexMarathon verification: $repo" -ForegroundColor White
Write-Host "Observed checks and toolchain-gated checks are reported separately." -ForegroundColor Gray

foreach ($required in @(
    'PLAN.md',
    'protocol/VERSION',
    'protocol/protocol.json',
    'protocol/commands.json',
    'protocol/events.json',
    'controller/go.mod',
    'controller/app/app.go',
    'controller/cmd/codexmarathon/main.go',
    'integration/go.mod',
    'runtime/PROVENANCE.md',
    'runtime/codex-rs/Cargo.toml',
    'runtime/codex-rs/Cargo.lock',
    'runtime/codex-rs/LICENSE',
    'runtime/codex-rs/NOTICE',
    'runtime/codex-rs/cli/Cargo.toml',
    'runtime/codex-rs/codexmarathon-runtime/Cargo.toml',
    'runtime/codexmarathon-adapter/Cargo.toml',
    'packaging/release-manifest.json',
    'packaging/README.md',
    'scripts/verify_provenance.py',
    'scripts/verify_package.py',
    'scripts/verify_protocol.py',
    'scripts/package_release.py',
    'scripts/live_smoke.py',
    'docs/release.md'
)) {
    [void](Require-File $required)
}

$embeddedManifest = Join-Path $repo 'runtime/codex-rs/Cargo.toml'
if (Test-Path -LiteralPath $embeddedManifest -PathType Leaf) {
    if ((Get-Content -LiteralPath $embeddedManifest -Raw) -match 'donor[\\/]') {
        Fail 'embedded workspace isolation' 'runtime/codex-rs/Cargo.toml references donor/'
    } else {
        Pass 'embedded workspace isolation' 'Cargo manifest has no donor dependency'
    }
}

foreach ($schema in @('protocol/protocol.json', 'protocol/commands.json', 'protocol/events.json', 'packaging/release-manifest.json')) {
    $schemaPath = Join-Path $repo $schema
    if (-not (Test-Path -LiteralPath $schemaPath -PathType Leaf)) {
        continue
    }
    try {
        $null = Get-Content -LiteralPath $schemaPath -Raw | ConvertFrom-Json
        Pass "json $schema" 'valid JSON'
    } catch {
        Fail "json $schema" $_.Exception.Message
    }
}

$versionPath = Join-Path $repo 'protocol/VERSION'
if (Test-Path -LiteralPath $versionPath -PathType Leaf) {
    $version = (Get-Content -LiteralPath $versionPath -Raw).Trim()
    if ($version -match '^[1-9][0-9]*$') {
        Pass 'protocol version' "v$version"
    } else {
        Fail 'protocol version' "expected a positive integer, got '$version'"
    }
}

if (-not $SkipGo) {
    $controllerDir = Join-Path $repo 'controller'
    $integrationDir = Join-Path $repo 'integration'
    if (Get-Command go -ErrorAction SilentlyContinue) {
        [void](Invoke-Tool 'controller Go tests' 'go' $controllerDir @('test', './...'))
        [void](Invoke-Tool 'integration Go tests' 'go' $integrationDir @('test', './...'))
    } else {
        Blocked 'Go tests' 'go.exe not found; install Go 1.22 or newer'
    }
} else {
    Write-Host 'SKIP: Go tests requested by -SkipGo' -ForegroundColor Gray
}

if (-not $SkipRust) {
    $runtimeDir = Join-Path $repo 'runtime/codexmarathon-adapter'
    $embeddedRuntimeDir = Join-Path $repo 'runtime/codex-rs'
    if (Get-Command cargo -ErrorAction SilentlyContinue) {
        [void](Invoke-Tool 'Rust adapter tests' 'cargo' $runtimeDir @('test'))
        [void](Invoke-Tool 'embedded runtime bridge tests' 'cargo' $embeddedRuntimeDir @('test', '-p', 'codexmarathon-runtime'))
        [void](Invoke-Tool 'embedded Codex CLI build' 'cargo' $embeddedRuntimeDir @('build', '-p', 'codex-cli'))
    } else {
        Blocked 'Rust runtime checks' 'cargo.exe not found; install Rust/Cargo'
    }
} else {
    Write-Host 'SKIP: Rust tests requested by -SkipRust' -ForegroundColor Gray
}

Write-Host ''
Write-Host 'Verification summary' -ForegroundColor White
Write-Host "  observed: $($observed.Count)" -ForegroundColor Green
Write-Host "  failures: $($failures.Count)" -ForegroundColor Red
Write-Host "  blocked:  $($blocked.Count)" -ForegroundColor Yellow
if ($failures.Count -gt 0) {
    Write-Host 'Result: FAILED (static or executed check failed).' -ForegroundColor Red
    exit 1
}
if ($blocked.Count -gt 0) {
    Write-Host 'Result: BLOCKED (required toolchain checks were unavailable).' -ForegroundColor Yellow
    exit 2
}
Write-Host 'Result: PASS' -ForegroundColor Green
exit 0
