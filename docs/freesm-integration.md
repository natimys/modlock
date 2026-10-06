# Freesm ModLock — integration checkpoint

The launcher and Go component remain separate repositories:

- Launcher fork: https://github.com/natimys/FreesmLauncher
- Upstream: https://github.com/FreesmTeam/FreesmLauncher
- Go component: https://github.com/natimys/modlock
- Launcher working copy: `C:\Users\natim\Projects\FreesmLauncher`
- Integration branch: `feature/modlock`
- Baseline upstream revision: `163424fc202e451f05ca360ef431209664691137`
- Submodule `libraries/libnbtplusplus`: `687e43031df0dc641984b4256bcca50d5b3f7de3`

The unmodified launcher has built locally and in Windows x64 CI. Local CTest
passed all 22 tests, CI uploaded the baseline artifact, and a hands-on check
confirmed instance creation and Minecraft launch. The fork now has a separate
Freesm ModLock application identity, data directory, and updater repository.
An integrated MSVC x64 build also packages the pinned Go payload; its bridge
smoke check passed with a root path containing spaces and Cyrillic characters.
The Go baseline and current bridge pass `go test ./...`.

The Go CLI now accepts an explicit `--root`, including empty instance directories,
while retaining automatic discovery for legacy calls. It is not yet connected to Qt.
Path resolution now rejects traversal, Windows alternate data streams and reserved
names, symlinks, and junctions. Errors are propagated before scanning or modifying
affected files. The symlink regression test ran successfully on this Windows host;
the complete Go test suite passes.
The synchronization service now separates `Check` from `RunRevision`. Preview
returns the Git commit and diff; applying requires the full commit hash and checks
out that exact object. A regression test advances the branch between preview and
apply and verifies that the previewed JAR content is installed.

Lock parsing accepts schema 1 and schema 2. Schema 2 adds pack name/version,
profile component IDs and versions, SHA-256 digests, and managed files with
checked instance targets and `replace`/`if_missing` policies. Schema 2 is not
advertised for player application until its transactional installer is ready.
Author scan preferences have a separate `.modlock/author.toml` file for include
directories, exclusions, and ignored mod IDs.

Protocol-1 bridge exposes `capabilities`, `read`, `check`, `apply`, and
`save-author-settings` for schema 1, with NDJSON progress, cancellation, and typed errors. The loader uses
its verified adjacent payload for bridge calls, bypassing shared CLI updates.
Tests cover fragmented JSON, progress, cancellation, incompatible protocol,
malformed/oversized messages, and branch movement between preview and apply.
Real loader/payload smoke checks passed with spaces and Cyrillic in the root.
The launcher now has an asynchronous `QProcess` adapter that passes an argument
array, parses fragmented NDJSON, preserves structured error codes, keeps stderr
separate, and sends cooperative cancellation. Startup checks bridge and loader
protocol compatibility and offers the fork's releases page on mismatch. Six Qt
tests cover compatibility, incompatible payloads, fragmented progress/results,
structured errors, cancellation, and CRLF stderr handling. See
[bridge-protocol.md](bridge-protocol.md). Import and update pages are not wired
to these operations yet.

## Local Windows builds

`scripts/build-freesm-baseline.ps1` uses the existing `windows_msvc` preset,
imports the MSVC x64 developer environment, builds, runs CTest, and installs
the launcher. It refuses changes to tracked launcher sources. The default
source directory is the sibling `FreesmLauncher` checkout.

The tools below are installed in the ignored `.cache` directory of ModLock:

- MSVC 2022: installed system tools, version 14.44.35207
- Python 3.12 (a working interpreter is required by the Meson overlay;
  the Windows Store execution alias is insufficient)
- CMake 3.31.6, Ninja 1.11.1.4
- Qt 6.10.2 `win64_msvc2022_64`, with `qtimageformats` and `qtnetworkauth`
- JDK 17 (upstream CI uses Zulu 17; JDK 25 cannot compile the Java 7 helpers)
- aqtinstall 3.1.21
- vcpkg checkout `434307da09bc05b2c86996dccc8b2351fc0d5d37`
- Dependency registry baseline comes from the unchanged launcher
  `vcpkg-configuration.json`: `2d6a6cf3ac9a7cc93942c3d289a2f9c661a6f4a7`

With Python and pip available, prepare the same tools as follows:

```powershell
python -m pip install --target .cache\build-python aqtinstall==3.1.21 cmake==3.31.6 ninja==1.11.1.4
$env:PYTHONPATH = (Resolve-Path .cache\build-python).Path
python -m aqt install-qt windows desktop 6.10.2 win64_msvc2022_64 -O .cache\Qt -m qtimageformats qtnetworkauth
git clone --filter=blob:none https://github.com/microsoft/vcpkg.git .cache\vcpkg
git -C .cache\vcpkg checkout 434307da09bc05b2c86996dccc8b2351fc0d5d37
& .cache\vcpkg\bootstrap-vcpkg.bat -disableMetrics
& .\scripts\build-freesm-baseline.ps1 -Configuration Debug -JavaHome 'C:\path\to\jdk-17'
```

If `python.exe` on PATH is an inactive Windows Store alias, pass
`-Python 'C:\path\to\python.exe'` to the build script. This prepends the real
interpreter to PATH without changing system settings.
The script also preserves `CMAKE_PROGRAM_PATH` in vcpkg's build environment so
its Meson overlay selects the same Python interpreter.

Once the launcher fork is checked out at `feature/modlock` with its submodules,
`scripts/build-freesm-modlock.ps1` uses the `windows_msvc_modlock` preset to
build and test the integrated launcher, run Go tests from the pinned submodule,
and install the player package. It verifies the submodule checkout against the
launcher gitlink before building.

The fork's `ModLock Windows baseline` workflow runs on Windows 2022 with
MSVC x64, Qt 6.10.2, and the existing dependency setup action. Its checkout
is pinned to the unchanged upstream revision. The output is an upstream
Freesm baseline, not an integrated ModLock release.
Initial CI run: https://github.com/natimys/FreesmLauncher/actions/runs/37504128242

## Integration constraints to retain

- Go owns synchronization, lock parsing, source detection, and Git publishing.
- Qt owns Minecraft components, instances, interface, and game launch.
- Go bridge stdout must contain only protocol-1 NDJSON; diagnostics use stderr.
- Check returns a Git commit and diff; apply must install that same commit.
- Cancel is a protocol message; file application must complete or roll back.
- Schema 2 includes profile component IDs/versions, file policies, and SHA-256;
  schema 1 remains readable and requires manual component selection.
- Instance creation needs a ModLock step after the standard game download and
  before registration. `InstanceCreationTask` currently downloads game files
  after `createInstance()` and must be extended carefully.
- Add a dedicated launch step; preserve the user's pre-launch commands.
- Component changes create a new instance rather than migrating the existing one.
- Author mode disables automatic local pack updates, uses installed Git
  credentials, and publishes only explicitly managed content.
- Preserve existing licensing and upstream attribution in every release.

The fork's `Freesm ModLock Windows x64` workflow builds the integrated launcher,
tests Qt and Go, smoke-tests the bundled bridge, and uploads the player package.
Initial run passed: https://github.com/natimys/FreesmLauncher/actions/runs/37516540359
Transactional schema 2 installation, the remaining bridge operations, Qt
import/update pages, author editor, and full release acceptance checks remain
pending implementation.
