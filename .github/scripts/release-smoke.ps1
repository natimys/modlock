param(
    [Parameter(Mandatory = $true)][string]$DistPath,
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$Commit,
    [Parameter(Mandatory = $true)][string]$Repository
)

$ErrorActionPreference = 'Stop'
$dist = (Resolve-Path -LiteralPath $DistPath).Path
$fullArchive = Join-Path $dist 'modlock-windows-amd64.zip'
$appArchive = Join-Path $dist 'modlock-app-windows-amd64.zip'
$manifestPath = Join-Path $dist 'manifest.json'
foreach ($path in @($fullArchive, $appArchive, $manifestPath)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Release artifact is missing: $path"
    }
}

$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
if ($manifest.version -ne $Version) { throw "Manifest version $($manifest.version) does not match $Version" }
if ($manifest.platform -ne 'windows-amd64') { throw "Unexpected manifest platform: $($manifest.platform)" }
if ($manifest.archive -ne 'modlock-app-windows-amd64.zip') { throw "Unexpected manifest archive: $($manifest.archive)" }
$archiveInfo = Get-Item -LiteralPath $appArchive
if ([long]$manifest.size -ne $archiveInfo.Length) { throw 'Manifest archive size does not match the application archive' }
$archiveHash = (Get-FileHash -LiteralPath $appArchive -Algorithm SHA256).Hash.ToLowerInvariant()
if ($manifest.sha256 -ne $archiveHash) { throw 'Manifest SHA-256 does not match the application archive' }

$scratch = Join-Path ([IO.Path]::GetTempPath()) ("modlock-release-smoke-" + [guid]::NewGuid().ToString('N'))
$fullExtract = Join-Path $scratch 'full'
$appExtract = Join-Path $scratch 'app'
$localAppData = Join-Path $scratch 'local-app-data'
New-Item -ItemType Directory -Force -Path $fullExtract, $appExtract, $localAppData | Out-Null
try {
    Expand-Archive -LiteralPath $fullArchive -DestinationPath $fullExtract
    Expand-Archive -LiteralPath $appArchive -DestinationPath $appExtract
    $launcher = Join-Path $fullExtract 'modlock.exe'
    $fullApp = Join-Path $fullExtract 'modlock-app.exe'
    $appOnly = Join-Path $appExtract 'modlock-app.exe'
    foreach ($exe in @($launcher, $fullApp, $appOnly)) {
        if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { throw "Expected executable missing after extraction: $exe" }
    }

    # Keep the launcher's updater storage isolated and remove Go from PATH. The
    # binaries must run as shipped, without a developer toolchain on PATH.
    $oldLocalAppData = $env:LOCALAPPDATA
    $oldPath = $env:PATH
    $oldSkipUpdate = $env:MODLOCK_SKIP_UPDATE
    try {
        $env:LOCALAPPDATA = $localAppData
        $env:PATH = Join-Path $env:WINDIR 'System32'
        $env:MODLOCK_SKIP_UPDATE = '1'

        $appVersionOutput = (& $fullApp version 2>&1 | Out-String)
        if ($LASTEXITCODE -ne 0) { throw "modlock-app.exe version failed: $appVersionOutput" }
        $launcherVersionOutput = (& $launcher version 2>&1 | Out-String)
        if ($LASTEXITCODE -ne 0) { throw "modlock.exe version failed: $launcherVersionOutput" }
        foreach ($output in @($appVersionOutput, $launcherVersionOutput)) {
            if ($output -notmatch "ModLock $([regex]::Escape($Version))\b") { throw "Version metadata missing from executable output: $output" }
            if ($output -notmatch "commit: $([regex]::Escape($Commit))\b") { throw "Commit metadata missing from executable output: $output" }
            if ($output -notmatch "releases: $([regex]::Escape($Repository))\b") { throw "Release repository metadata missing from executable output: $output" }
        }
        & $fullApp --modlock-healthcheck
        if ($LASTEXITCODE -ne 0) { throw 'modlock-app.exe health check failed' }
    }
    finally {
        $env:LOCALAPPDATA = $oldLocalAppData
        $env:PATH = $oldPath
        $env:MODLOCK_SKIP_UPDATE = $oldSkipUpdate
    }
}
finally {
    Remove-Item -LiteralPath $scratch -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host 'Release artifact smoke tests passed.'
