[CmdletBinding()]
param(
    [string]$Source = (Join-Path $PSScriptRoot '..\..\FreesmLauncher'),
    [string]$Tools = (Join-Path $PSScriptRoot '..\.cache'),
    [string]$Python,
    [string]$JavaHome,
    [ValidateSet('Debug', 'Release')][string]$Configuration = 'Release',
    [ValidateRange(1, 64)][int]$Jobs = 4
)

$ErrorActionPreference = 'Stop'
$Source = (Resolve-Path -LiteralPath $Source).Path
$Tools = (Resolve-Path -LiteralPath $Tools).Path
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
$vs = & $vswhere -latest -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (-not $vs) { throw 'MSVC x64 tools are required.' }
$vcvars = Join-Path $vs 'VC\Auxiliary\Build\vcvars64.bat'
$PSNativeCommandArgumentPassing = 'Legacy'
& $env:ComSpec /d /c "call `"$vcvars`" >nul && set" | ForEach-Object {
    if ($_ -match '^([^=]+)=(.*)$') {
        [Environment]::SetEnvironmentVariable($matches[1], $matches[2], 'Process')
    }
}
if ($LASTEXITCODE -ne 0) { throw 'Could not initialize MSVC.' }

$cmake = Join-Path $Tools 'build-python\cmake\data\bin\cmake.exe'
$ctest = Join-Path $Tools 'build-python\cmake\data\bin\ctest.exe'
$ninja = Join-Path $Tools 'build-python\bin'
$qt = Join-Path $Tools 'Qt\6.10.2\msvc2022_64'
$env:Path = "$(Split-Path $cmake);$ninja;$qt\bin;$env:Path"
if ($Python) {
    $Python = (Resolve-Path -LiteralPath $Python).Path
    $env:Path = "$(Split-Path $Python);$env:Path"
} else {
    $Python = (Get-Command python.exe -ErrorAction Stop).Source
}
& $Python --version
if ($LASTEXITCODE -ne 0) { throw 'A working Python >= 3.7 is required by the vcpkg Meson overlay.' }
$env:CMAKE_PROGRAM_PATH = Split-Path $Python
$env:VCPKG_KEEP_ENV_VARS = "$env:VCPKG_KEEP_ENV_VARS;CMAKE_PROGRAM_PATH"
if (-not $JavaHome) { $JavaHome = $env:JAVA_HOME }
if (-not $JavaHome) { throw 'JDK 17 is required; pass -JavaHome with its installation directory.' }
$JavaHome = (Resolve-Path -LiteralPath $JavaHome).Path
$javac = Join-Path $JavaHome 'bin\javac.exe'
$javaVersion = & $javac -version 2>&1
if ($LASTEXITCODE -ne 0 -or "$javaVersion" -notmatch '^javac 17\.') { throw 'Use JDK 17, matching the upstream MSVC workflow.' }
$env:JAVA_HOME = $JavaHome
$env:Path = "$JavaHome\bin;$env:Path"
$env:CMAKE_PREFIX_PATH = $qt
$env:VCPKG_ROOT = Join-Path $Tools 'vcpkg'
$env:ARTIFACT_NAME = 'Windows-MSVC'
$env:BUILD_PLATFORM = 'Freesm ModLock Windows x64'
foreach ($required in @($cmake, $ctest, "$ninja\ninja.exe", "$qt\bin\qmake.exe", "$env:VCPKG_ROOT\vcpkg.exe")) {
    if (-not (Test-Path -LiteralPath $required)) { throw "Missing dependency: $required" }
}

Push-Location -LiteralPath $Source
try {
    $branch = & git branch --show-current
    if ($LASTEXITCODE -ne 0 -or $branch -ne 'feature/modlock') {
        throw 'Integrated builds must use the feature/modlock launcher branch.'
    }
    $gitlink = & git ls-tree HEAD libraries/modlock
    $expectedModLock = if ($gitlink -match '\s([0-9a-f]{40})\s+libraries/modlock$') { $matches[1] } else { $null }
    $actualModLock = & git -C libraries/modlock rev-parse HEAD
    if ($LASTEXITCODE -ne 0 -or -not $expectedModLock -or $actualModLock -ne $expectedModLock) {
        throw 'The ModLock submodule checkout does not match the launcher gitlink.'
    }
    & git rev-parse HEAD
    & git -C libraries/modlock rev-parse HEAD
    & $cmake --preset windows_msvc_modlock "-DJava_JAVA_EXECUTABLE=$JavaHome/bin/java.exe" "-DJava_JAVAC_EXECUTABLE=$JavaHome/bin/javac.exe" "-DJava_JAR_EXECUTABLE=$JavaHome/bin/jar.exe"
    if ($LASTEXITCODE -ne 0) { throw 'CMake configuration failed.' }
    & $cmake --build --preset windows_msvc_modlock --config $Configuration --parallel $Jobs
    if ($LASTEXITCODE -ne 0) { throw 'Launcher build failed.' }
    & $ctest --preset windows_msvc_modlock --build-config $Configuration
    if ($LASTEXITCODE -ne 0) { throw 'Launcher tests failed.' }
    Push-Location libraries/modlock
    try {
        go test ./...
        if ($LASTEXITCODE -ne 0) { throw 'ModLock Go tests failed.' }
    } finally {
        Pop-Location
    }
    & $cmake --install build-modlock --config $Configuration
    if ($LASTEXITCODE -ne 0) { throw 'Launcher packaging failed.' }
} finally {
    Pop-Location
}
