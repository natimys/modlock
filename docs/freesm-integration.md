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
checked instance targets and `replace`/`if_missing` policies. The installer now
stages mods and managed files under the instance root, checks SHA-256 values,
backs up every affected file, applies removals and replacements, and writes the
lock last. Managed `replace` conflicts are reported with their current hash and
require matching confirmation at apply; `if_missing` preserves existing regular
files. Offline verify reports managed-file state separately from mod damage.
Author scan preferences have a separate `.modlock/author.toml` file for include
directories, exclusions, and ignored mod IDs.

Protocol-1 bridge exposes `capabilities`, `read`, `install`, `verify`, `check`,
`apply`, `scan`, and `save-author-settings` for schemas 1 and 2, with NDJSON progress,
cancellation, and typed errors. `verify` works offline, checks managed files for
safe paths, regular non-empty file type, and SHA-256 where available. Legacy
entries without hashes are reported as unverified rather than content-matched.
`check` reports the local installation state without modifying files; applying a
checked commit reinstalls missing or damaged mods and applies managed-file changes
from the exact previewed Git commit. The loader uses
its verified adjacent payload for bridge calls, bypassing shared CLI updates.
Tests cover fragmented JSON, progress, cancellation, incompatible protocol,
malformed/oversized messages, and branch movement between preview and apply.
Real loader/payload smoke checks passed with spaces and Cyrillic in the root.
The launcher has an asynchronous `QProcess` adapter that passes an argument
array, parses fragmented NDJSON, preserves structured error codes, keeps stderr
separate, and sends cooperative cancellation. It retains the bridge and window
until the process exits, even after cancellation or a terminal JSON event.
Instance creation treats Minecraft-download and ModLock post-install cancellation
as cancellation, and registers only successful installs. ModLock update and
launch paths verify the installed revision and restore missing or damaged mods;
offline launch is offered only after local verification passes. Startup checks
bridge and loader protocol compatibility and offers the fork's releases page on
mismatch. Qt tests cover compatibility, incompatible payloads, fragmented
progress/results, structured errors, cancellation with delayed process exit, and
CRLF stderr handling. See [bridge-protocol.md](bridge-protocol.md).

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
- Check returns a Git commit, diff, and local installation state; apply must
  install that same commit and repair missing or damaged managed mods.
- Verify is offline and read-only. Cancel is a protocol message; file application
  must complete or roll back before the process exits.
- Schema 2 includes profile component IDs/versions, file policies, and SHA-256;
  schema 1 remains readable and requires manual component selection.
- Instance creation runs ModLock install after the standard game download and
  before registration. ModLock download cancellation is terminal; ordinary
  instance creation retains its existing skip behavior.
- The dedicated launch step preserves the user's pre-launch commands and blocks
  launch when file recovery fails.
- Component changes create a new instance rather than migrating the existing one.
- Author mode disables automatic local pack updates, uses installed Git
  credentials, and publishes only explicitly managed content.
- Preserve existing licensing and upstream attribution in every release.

The fork's `Freesm ModLock Windows x64` workflow builds the integrated launcher,
tests Qt and Go, smoke-tests the bundled bridge, and uploads the player package.
Initial run passed: https://github.com/natimys/FreesmLauncher/actions/runs/37516540359
Transactional schema 2 installation in Go uses the same managed-file preview
rules during apply, returns structured conflict details, and accepts exact
SHA-256 confirmations for install and update. New `replace` files and
`if_missing` to `replace` transitions require user choice when local content
would be replaced; equal content needs no write. Missing `if_missing` entries
are included in offline recovery checks. The first-install bootstrap lock only
contains pack metadata.

The ModLock import page accepts schemas 1 and 2. Schema 2 resolves only the
exact Minecraft and optional Fabric, Quilt, Forge, or NeoForge versions from
Freesm metadata, creates the profile directly from that checked preview, and
shows the pack name/version, game components, and file count. Source edits
invalidate the preview and pending installation task. Schema 1 keeps manual
component selection.

Initial installation, manual update, and pre-launch update use one conflict
dialog with affected paths and replace/delete actions. Applying a build passes
the observed hashes back to Go; keeping local files defers the entire update.
A stale confirmation returns updated conflicts and prompts again. Managed-pack
properties show readable changes, build version, and Git revision separately.
The integrated Release workflow builds and installs the player package and
runs all Qt tests, Go tests, and the bundled bridge smoke check.
