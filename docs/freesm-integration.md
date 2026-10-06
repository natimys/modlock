# Freesm ModLock — integration checkpoint

The launcher and Go component remain separate repositories:

- Launcher fork: https://github.com/natimys/FreesmLauncher
- Upstream: https://github.com/FreesmTeam/FreesmLauncher
- Go component: https://github.com/natimys/modlock
- Launcher working copy: `C:\Users\natim\Projects\FreesmLauncher`
- Integration branch: `feature/modlock`
- Baseline upstream revision: `163424fc202e451f05ca360ef431209664691137`
- Submodule `libraries/libnbtplusplus`: `687e43031df0dc641984b4256bcca50d5b3f7de3`

The baseline gate precedes application branding and functional integration:
build the unmodified launcher, run CTest, create a Minecraft instance, and
verify that Minecraft launches. These runtime checks are not yet complete.
The Go baseline passes `go test ./...`.

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

Protocol-1 bridge now exposes `capabilities`, `read`, `check`, and `apply` for
schema 1, with NDJSON progress, cancellation, and typed errors. The loader uses
its verified adjacent payload for bridge calls, bypassing shared CLI updates.
Tests cover fragmented JSON, progress, cancellation, incompatible protocol,
malformed/oversized messages, and branch movement between preview and apply.
Real loader/payload smoke checks passed with spaces and Cyrillic in the root.
See [bridge-protocol.md](bridge-protocol.md). Qt is not connected yet.

## Local Windows baseline

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
- Separate app/data identity and fork update source must precede distributing
  an integrated build. Preserve existing licensing and upstream attribution.

Schema 2, the remaining bridge operations, Qt import/update pages, author editor,
packaging a pinned Go payload with the launcher, and release acceptance checks
are still pending implementation.
