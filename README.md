# ModLock

## Installing ModLock on Windows

Download `modlock-windows-amd64.zip` from a stable GitHub Release and extract both `modlock.exe` and `modlock-app.exe` into the Minecraft instance folder. The first installation requires both executables. Run `modlock.exe`; it is the stable launcher. The launcher stores shared application versions under `%LOCALAPPDATA%\ModLock`, while the small launcher stays in the instance folder. Users upgrading an older standalone ModLock executable need to install the launcher bundle once. The stable launcher does not update itself, so replace `modlock.exe` once to receive launcher fixes.

Release builds include their SemVer, commit, build date, and release repository. Local builds report version `dev` and do not self-update. The release repository is embedded by GitHub Actions from `GITHUB_REPOSITORY`, independently of the Minecraft pack repository in `mod.lock`.

The launcher checks the latest stable GitHub Release before opening the CLI or menu. Automatic checks are cached for 24 hours (or one hour after a failed check); explicit checks bypass the cache:

```powershell
modlock version
modlock self-update --check
modlock self-update
modlock self-update --rollback
```

The updater accepts only a compatible `windows-amd64` manifest and verifies archive size and SHA-256 before installing. Previous application versions are kept in the shared user data folder for rollback. Release tags must use `vX.Y.Z`; pushing one runs tests, builds the app and stable launcher, and publishes the Windows x64 installation bundle, app archive, and manifest.

ModLock keeps one Minecraft instance's `mods/*.jar` synchronized with a lock file published in a Git repository. Friends need only `modlock.exe` and `mod.lock`; Git and provider tokens are not required.

## Build

Go 1.25 or newer:

```powershell
go build -o modlock-app.exe ./cmd/modlock
go build -o modlock.exe ./cmd/launcher
```

## Friend setup

1. Put `modlock.exe` and `mod.lock` into the Minecraft instance.
2. Run `modlock.exe`.
3. Select **«Обновить сборку»**.

The command-line equivalent is `modlock sync`.

## Author workflow

For an existing pack, run `modlock init`, enter the public repository URL, and confirm the generated lock. Unknown/custom jars are copied to `files/mods/` automatically.

After changing `mods/`:

```powershell
modlock diff
modlock push
```

To restore the repository to an earlier commit without rewriting history:

```powershell
modlock revert
modlock revert <commit-or-tag>
```

`revert` refuses to overwrite uncommitted tracked changes, restores all Git-tracked files to the selected snapshot, creates a new rollback commit, and pushes it. Untracked Minecraft runtime files are left untouched.

To add one new jar and choose its source manually:

```powershell
modlock add
# or
modlock add custom-mod.jar
```

The command first tries automatic detection, then lets you keep the detected provider or choose Modrinth, CurseForge, or repository storage. Manual provider selection asks for a direct download URL. `add` updates local `mod.lock` but does not commit or push.

To ignore a mod as an entity:

```powershell
modlock ignore
modlock ignore mods/client-only-mod.jar
```

Ignored stable IDs are stored in `mod.lock.ignore`. An ignored entity is removed from `mod.lock`, excluded by `diff`, `init`, and `push`, and is not removed from the author's local `mods/` folder.

`diff` also checks newly added jars against Modrinth and, when `CURSEFORGE_API_KEY` is set, CurseForge. Files for which no provider is found are listed separately; `diff` does not copy or modify them.

`init` initializes the local Git repository, selects the configured branch, and creates the `origin` remote. `push` stages only `mod.lock`, `mod.lock.ignore`, and managed files below `files/mods/`, then commits and pushes. Before committing, `push` checks that `origin` exists and points to `pack.repository`; if it is missing, ModLock creates it automatically. If the URL is different, ModLock stops without creating a commit so the remote can be fixed safely. Use `modlock push -m "Add compat mod"` for a custom commit message.

During `sync`, mods that were present in the previous lock but are absent from the downloaded lock are removed from the configured `mods_dir`. Unmanaged JAR files are left untouched.

## Lock format

```toml
schema = 1

[pack]
repository = "https://github.com/example/techmagic-modpack.git"
branch = "main"
lock_path = "mod.lock"
mods_dir = "mods"

[[mods]]
id = "modrinth:create"
version = "6.0.10"
filename = "create-1.21.1.jar"
source = "modrinth"
project_id = "..."
version_id = "..."
url = "https://cdn.modrinth.com/..."

[[mods]]
id = "techmagic:custom-compat"
version = "0.2"
filename = "my-compat.jar"
source = "repo"
path = "files/mods/my-compat.jar"
```

`id` is the stable identity of a mod; it stays the same when the version, filename, or URL changes. `version` describes the installed release. A lock-to-lock diff reports matching IDs as `updated` instead of unrelated removal and addition. Older locks without these fields remain readable through provider-ID fallback.

## CurseForge

`CURSEFORGE_API_KEY` is optional and used by `init`, `diff`, and `push` for fingerprint detection. The official CurseForge API requires this key. Without it—or if detection fails—unknown mods are shown explicitly before `init`/`push` stores them in the Git repository as `source = "repo"`.

`modlock init` checks the key before asking for repository settings and reports whether the CurseForge API accepted it. Set it for the current PowerShell process with:

```powershell
$env:CURSEFORGE_API_KEY = "your-key"
modlock init
```
## License

ModLock is licensed under the GNU General Public License, version 3 or (at your option) any later version. See [LICENSE](LICENSE) for the full text.
