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

## Local Windows baseline

`scripts/build-freesm-baseline.ps1` uses the existing `windows_msvc` preset,
imports the MSVC x64 developer environment, builds, runs CTest, and installs
the launcher. It refuses changes to tracked launcher sources. The default
source directory is the sibling `FreesmLauncher` checkout.

The tools below are installed in the ignored `.cache` directory of ModLock:

- MSVC 2022: installed system tools, version 14.44.35207
- CMake 3.31.6, Ninja 1.11.1.4
- Qt 6.10.2 `win64_msvc2022_64`, with `qtimageformats` and `qtnetworkauth`
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
& .\scripts\build-freesm-baseline.ps1 -Configuration Debug
```

The fork's `ModLock Windows baseline` workflow runs on Windows 2022 with
MSVC x64, Qt 6.10.2, and the existing dependency setup action. Its checkout
is pinned to the unchanged upstream revision. The output is an upstream
Freesm baseline, not an integrated ModLock release.

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

Bridge, schema 2, Qt import/update pages, author editor, bundled Go payload,
and release acceptance checks are still pending implementation.
