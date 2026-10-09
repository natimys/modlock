package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"modlock/internal/failure"
	"modlock/internal/lockfile"
)

func schema2Fixture(t *testing.T, oldFiles, nextFiles []lockfile.ManagedFile, oldData, repoData map[string][]byte) (string, string) {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote")
	pack := lockfile.Pack{Repository: remote, Branch: "master", LockPath: "mod.lock", ModsDir: "mods", Name: "Test pack", Version: "1"}
	for path, data := range repoData {
		full := filepath.Join(remote, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	next := &lockfile.File{Schema: 2, Pack: pack, Files: nextFiles}
	if err := lockfile.Write(filepath.Join(remote, "mod.lock"), next); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainInit(remote, false)
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Add("mod.lock"); err != nil {
		t.Fatal(err)
	}
	for path := range repoData {
		if _, err = w.Add(filepath.ToSlash(path)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = w.Commit("schema 2", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for target, data := range oldData {
		full := filepath.Join(root, filepath.FromSlash(target))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 2, Pack: pack, Files: oldFiles}); err != nil {
		t.Fatal(err)
	}
	return root, head.Hash().String()
}

func managedEntry(path, target, policy string, data []byte) lockfile.ManagedFile {
	digest := sha256.Sum256(data)
	return lockfile.ManagedFile{Path: path, Target: target, Policy: policy, SHA256: hex.EncodeToString(digest[:])}
}

func schema3Fixture(t *testing.T, roots TargetRoots, mods []lockfile.ModEntry, files []lockfile.ManagedFile, payloads map[string][]byte) (string, string) {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote")
	pack := lockfile.Pack{Repository: remote, Branch: "master", LockPath: "mod.lock", ModsDir: "mods", Name: "Target Pack", Version: "1"}
	for rel, data := range payloads {
		full := filepath.Join(remote, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := &lockfile.File{Schema: 3, Pack: pack, Mods: mods, Files: files}
	if err := lockfile.Write(filepath.Join(remote, "mod.lock"), manifest); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainInit(remote, false)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for rel := range payloads {
		if _, err = worktree.Add(filepath.ToSlash(rel)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = worktree.Add("mod.lock"); err != nil {
		t.Fatal(err)
	}
	if _, err = worktree.Commit("schema 3", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	processRoot := roots["client"]
	if err := os.MkdirAll(processRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.Write(filepath.Join(processRoot, "mod.lock"), &lockfile.File{Schema: 3, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	return processRoot, head.Hash().String()
}

func TestSchema3ApplyVerifyAndRepairAcrossTargets(t *testing.T) {
	client, server := filepath.Join(t.TempDir(), "minecraft"), filepath.Join(t.TempDir(), "server")
	roots := TargetRoots{"client": client, "server": server}
	clientBytes, serverBytes, sharedBytes := []byte("client jar"), []byte("server jar"), []byte("shared jar")
	entry := func(id, name, path string, data []byte, targets ...string) lockfile.ModEntry {
		h := sha256.Sum256(data)
		return lockfile.ModEntry{ID: id, Version: "1", Filename: name, Source: "repo", Path: path, SHA256: hex.EncodeToString(h[:]), Targets: targets}
	}
	mods := []lockfile.ModEntry{entry("client-only", "client.jar", "files/mods/client.jar", clientBytes, "client"), entry("server-only", "server.jar", "files/mods/server.jar", serverBytes, "server"), entry("shared", "shared.jar", "files/mods/shared.jar", sharedBytes, "client", "server")}
	clientCfg, serverCfg := []byte("client config"), []byte("server config")
	fileEntry := func(source, target string, data []byte, targets ...string) lockfile.ManagedFile {
		h := sha256.Sum256(data)
		return lockfile.ManagedFile{Path: source, Target: target, SHA256: hex.EncodeToString(h[:]), Policy: "replace", Targets: targets}
	}
	files := []lockfile.ManagedFile{fileEntry("files/config/client.cfg", "config/client.cfg", clientCfg, "client"), fileEntry("files/config/server.cfg", "config/server.cfg", serverCfg, "server")}
	root, revision := schema3Fixture(t, roots, mods, files, map[string][]byte{"files/mods/client.jar": clientBytes, "files/mods/server.jar": serverBytes, "files/mods/shared.jar": sharedBytes, "files/config/client.cfg": clientCfg, "files/config/server.cfg": serverCfg})
	preview, err := CheckWithRoots(context.Background(), root, roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Revision != revision {
		t.Fatalf("preview revision mismatch: %s != %s", preview.Revision, revision)
	}
	if _, err = RunRevisionConfirmedWithRoots(context.Background(), root, revision, nil, nil, roots); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{filepath.Join(client, "mods", "client.jar"): string(clientBytes), filepath.Join(server, "mods", "server.jar"): string(serverBytes), filepath.Join(client, "mods", "shared.jar"): string(sharedBytes), filepath.Join(server, "mods", "shared.jar"): string(sharedBytes), filepath.Join(client, "config", "client.cfg"): string(clientCfg), filepath.Join(server, "config", "server.cfg"): string(serverCfg)} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("materialized %s as %q, %v", path, got, err)
		}
	}
	check, err := VerifyWithRoots(root, roots)
	if err != nil || check.NeedsRecovery {
		t.Fatalf("healthy target install failed verification: %#v, %v", check, err)
	}
	if err := os.WriteFile(filepath.Join(server, "mods", "server.jar"), []byte("damaged"), 0644); err != nil {
		t.Fatal(err)
	}
	check, err = VerifyWithRoots(root, roots)
	if err != nil || !check.NeedsRecovery {
		t.Fatalf("damaged server target was not detected: %#v, %v", check, err)
	}
	if _, err = RunRevisionConfirmedWithRoots(context.Background(), root, revision, nil, nil, roots); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(server, "mods", "server.jar"))
	if err != nil || string(got) != string(serverBytes) {
		t.Fatalf("server target repair failed: %q, %v", got, err)
	}
}

func TestStandaloneSchema3SyncInstallsOnlyClientAndSharedResources(t *testing.T) {
	client := filepath.Join(t.TempDir(), "Minecraft Instance ü")
	server := filepath.Join(filepath.Dir(client), "server")
	if err := os.MkdirAll(client, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(server, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server, "keep.txt"), []byte("server data"), 0644); err != nil {
		t.Fatal(err)
	}
	digest := func(data []byte) string {
		h := sha256.Sum256(data)
		return hex.EncodeToString(h[:])
	}
	mods := []lockfile.ModEntry{
		{ID: "client", Filename: "client.jar", Source: "repo", Path: "files/mods/client.jar", SHA256: digest([]byte("client jar")), Targets: []string{"client"}},
		{ID: "server", Filename: "server.jar", Source: "repo", Path: "files/mods/server.jar", SHA256: digest([]byte("server jar")), Targets: []string{"server"}},
		{ID: "shared", Filename: "shared.jar", Source: "repo", Path: "files/mods/shared.jar", SHA256: digest([]byte("shared jar")), Targets: []string{"client", "server"}},
	}
	files := []lockfile.ManagedFile{
		{Path: "files/config/client.cfg", Target: "config/client.cfg", SHA256: digest([]byte("client config")), Policy: "replace", Targets: []string{"client"}},
		{Path: "files/config/server.cfg", Target: "config/server.cfg", SHA256: digest([]byte("server config")), Policy: "replace", Targets: []string{"server"}},
		{Path: "files/config/shared.cfg", Target: "config/shared.cfg", SHA256: digest([]byte("shared config")), Policy: "replace", Targets: []string{"client", "server"}},
	}
	payloads := map[string][]byte{
		"files/mods/client.jar": []byte("client jar"), "files/mods/server.jar": []byte("server jar"), "files/mods/shared.jar": []byte("shared jar"),
		"files/config/client.cfg": []byte("client config"), "files/config/server.cfg": []byte("server config"), "files/config/shared.cfg": []byte("shared config"),
	}
	root, _ := schema3Fixture(t, TargetRoots{"client": client}, mods, files, payloads)
	fixtureLock, err := lockfile.Read(filepath.Join(root, "mod.lock"))
	if err != nil {
		t.Fatal(err)
	}
	pack := fixtureLock.Pack
	oldMods := []lockfile.ModEntry{
		{ID: "obsolete", Filename: "obsolete.jar", Source: "repo", Path: "files/mods/obsolete.jar", SHA256: digest([]byte("obsolete")), Targets: []string{"client"}},
		{ID: "server", Filename: "server.jar", Source: "repo", Path: "files/mods/server.jar", SHA256: digest([]byte("server jar")), Targets: []string{"server"}},
	}
	oldFiles := []lockfile.ManagedFile{
		{Path: "files/config/obsolete.cfg", Target: "config/obsolete.cfg", SHA256: digest([]byte("old")), Policy: "replace", Targets: []string{"client"}},
		{Path: "files/config/server.cfg", Target: "config/server.cfg", SHA256: digest([]byte("old server")), Policy: "replace", Targets: []string{"server"}},
	}
	if err := os.MkdirAll(filepath.Join(client, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client, "mods", "obsolete.jar"), []byte("obsolete"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client, "mods", "local.jar"), []byte("unmanaged"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(client, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client, "config", "obsolete.cfg"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 3, Pack: pack, Mods: oldMods, Files: oldFiles}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunForTarget(context.Background(), root, "client", nil); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(client, "mods", "client.jar"):   "client jar",
		filepath.Join(client, "mods", "shared.jar"):   "shared jar",
		filepath.Join(client, "config", "client.cfg"): "client config",
		filepath.Join(client, "config", "shared.cfg"): "shared config",
		filepath.Join(client, "mods", "local.jar"):    "unmanaged",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{
		filepath.Join(client, "mods", "server.jar"), filepath.Join(client, "config", "server.cfg"),
		filepath.Join(server, "mods"), filepath.Join(server, "config"),
		filepath.Join(client, "mods", "obsolete.jar"), filepath.Join(client, "config", "obsolete.cfg"),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected path exists or could not be checked: %s (%v)", path, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(server, "keep.txt")); err != nil || string(got) != "server data" {
		t.Fatalf("server data changed: %q, %v", got, err)
	}
	// The canonical lock remains the full pack manifest after a client-only run.
	installed, err := lockfile.Read(filepath.Join(root, "mod.lock"))
	if err != nil || len(installed.Mods) != 3 || len(installed.Files) != 3 {
		t.Fatalf("full remote lock was not preserved: %#v, %v", installed, err)
	}
}

func TestSchema3TrustedWorkspaceRootsMapClientAndServerSubdirectories(t *testing.T) {
	workspace := t.TempDir()
	client := filepath.Join(workspace, "minecraft")
	server := filepath.Join(workspace, "server")
	for _, root := range []string{client, server} {
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatal(err)
		}
	}
	lock := &lockfile.File{Schema: 3, Mods: []lockfile.ModEntry{{Filename: "server.jar", Targets: []string{"server"}}}}
	if err := ValidateTargetRoots(workspace, lock, TargetRoots{"client": client, "server": server}); err != nil {
		t.Fatalf("workspace target map should be accepted: %v", err)
	}
}

func TestSchema3OperationsRequireLauncherTargetRoots(t *testing.T) {
	lock := &lockfile.File{Schema: 3, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "x", Version: "1"}, Mods: []lockfile.ModEntry{{ID: "server", Filename: "server.jar", Source: "repo", Path: "files/server.jar", SHA256: strings.Repeat("a", 64), Targets: []string{"server"}}}}
	if err := ValidateTargetRoots(t.TempDir(), lock, nil); failure.Code(err) != failure.InvalidRequest {
		t.Fatalf("missing trusted target root was not rejected: %v", err)
	}
}

func TestSchema3ApplyRollsBackAllRootsOnRenameFailure(t *testing.T) {
	client, server := filepath.Join(t.TempDir(), "client"), filepath.Join(t.TempDir(), "server")
	roots := TargetRoots{"client": client, "server": server}
	oldBytes, clientBytes, serverBytes := []byte("old client"), []byte("new client"), []byte("new server")
	hash := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	mods := []lockfile.ModEntry{
		{ID: "client", Version: "2", Filename: "client.jar", Source: "repo", Path: "files/mods/client.jar", SHA256: hash(clientBytes), Targets: []string{"client"}},
		{ID: "server", Version: "1", Filename: "server.jar", Source: "repo", Path: "files/mods/server.jar", SHA256: hash(serverBytes), Targets: []string{"server"}},
	}
	root, revision := schema3Fixture(t, roots, mods, nil, map[string][]byte{"files/mods/client.jar": clientBytes, "files/mods/server.jar": serverBytes})
	if err := os.MkdirAll(filepath.Join(client, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client, "mods", "client.jar"), oldBytes, 0644); err != nil {
		t.Fatal(err)
	}
	oldLock := &lockfile.File{Schema: 3, Mods: []lockfile.ModEntry{{ID: "client", Version: "1", Filename: "client.jar", Source: "repo", Path: "files/mods/client.jar", SHA256: hash(oldBytes), Targets: []string{"client"}}}}
	remoteLock, err := lockfile.Read(filepath.Join(root, "mod.lock"))
	if err != nil {
		t.Fatal(err)
	}
	oldLock.Pack = remoteLock.Pack
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), oldLock); err != nil {
		t.Fatal(err)
	}
	originalRename := renameInstalledFile
	defer func() { renameInstalledFile = originalRename }()
	failed := false
	renameInstalledFile = func(src, dst string) error {
		if !failed && strings.Contains(filepath.ToSlash(dst), "/server/mods/server.jar") {
			failed = true
			return os.ErrPermission
		}
		return os.Rename(src, dst)
	}
	if _, err := RunRevisionConfirmedWithRoots(context.Background(), root, revision, nil, nil, roots); err == nil {
		t.Fatal("injected mutation failure was not returned")
	}
	got, err := os.ReadFile(filepath.Join(client, "mods", "client.jar"))
	if err != nil || string(got) != string(oldBytes) {
		t.Fatalf("client target was not rolled back: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(server, "mods", "server.jar")); !os.IsNotExist(err) {
		t.Fatalf("server target survived rollback: %v", err)
	}
	installed, err := lockfile.Read(filepath.Join(root, "mod.lock"))
	if err != nil || installed.Mods[0].Version != "1" {
		t.Fatalf("lock was not rolled back: %#v, %v", installed, err)
	}
}

func TestSchema3ConflictKindAndStaleConfirmationAreTargetScoped(t *testing.T) {
	client := filepath.Join(t.TempDir(), "client")
	roots := TargetRoots{"client": client}
	oldBytes, desired, localEdit, laterEdit := []byte("old"), []byte("new"), []byte("local edit"), []byte("changed after preview")
	digest := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	file := managedEntry("files/config/a.cfg", "config/a.cfg", "replace", desired)
	file.Targets = []string{"client"}
	root, revision := schema3Fixture(t, roots, nil, []lockfile.ManagedFile{file}, map[string][]byte{"files/config/a.cfg": desired})
	remoteLock, err := lockfile.Read(filepath.Join(root, "mod.lock"))
	if err != nil {
		t.Fatal(err)
	}
	oldFile := managedEntry("files/config/a.cfg", "config/a.cfg", "replace", oldBytes)
	oldFile.Targets = []string{"client"}
	remoteLock.Files = []lockfile.ManagedFile{oldFile}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), remoteLock); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(client, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client, "config", "a.cfg"), localEdit, 0644); err != nil {
		t.Fatal(err)
	}
	preview, err := CheckWithRoots(context.Background(), root, roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Conflicts) != 1 || preview.Conflicts[0].Kind != "locally_modified" || preview.Conflicts[0].TargetID != "client" || preview.Conflicts[0].Message == "" {
		t.Fatalf("machine-readable target conflict missing: %#v", preview.Conflicts)
	}
	confirmed := ConfirmedConflict{Target: preview.Conflicts[0].Target, TargetID: preview.Conflicts[0].TargetID, SHA256: digest(localEdit)}
	if err := os.WriteFile(filepath.Join(client, "config", "a.cfg"), laterEdit, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = RunRevisionConfirmedWithRoots(context.Background(), root, revision, nil, []ConfirmedConflict{confirmed}, roots)
	if failure.Code(err) != failure.Conflict {
		t.Fatalf("stale target confirmation accepted: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(client, "config", "a.cfg"))
	if err != nil || string(got) != string(laterEdit) {
		t.Fatalf("stale apply changed user file: %q, %v", got, err)
	}
}

func TestSchema2ApplicationIsSupported(t *testing.T) {
	local := &lockfile.File{Schema: 1}
	remote := &lockfile.File{Schema: 2}
	if err := requireSupportedInstallSchema(local, remote); err != nil {
		t.Fatalf("schema 2 was rejected: %v", err)
	}
}

func TestSchema2ManagedFilesPoliciesRemovalAndVerify(t *testing.T) {
	old := []lockfile.ManagedFile{managedEntry("files/old.cfg", "config/old.cfg", "replace", []byte("old")), managedEntry("files/keep.cfg", "config/keep.cfg", "if_missing", []byte("repository"))}
	next := []lockfile.ManagedFile{managedEntry("files/empty.cfg", "config/empty.cfg", "replace", nil), managedEntry("files/keep.cfg", "config/keep.cfg", "if_missing", []byte("updated repository"))}
	root, revision := schema2Fixture(t, old, next, map[string][]byte{"config/old.cfg": []byte("old"), "config/keep.cfg": []byte("user value")}, map[string][]byte{"files/empty.cfg": nil, "files/keep.cfg": []byte("updated repository")})
	if _, err := RunRevision(context.Background(), root, revision, nil); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "config", "empty.cfg")); err != nil || len(data) != 0 {
		t.Fatalf("empty managed file: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "config", "keep.cfg")); err != nil || string(data) != "user value" {
		t.Fatalf("if_missing overwrote existing file: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "old.cfg")); !os.IsNotExist(err) {
		t.Fatalf("unchanged removed replace file remains: %v", err)
	}
	check, err := Verify(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(check.ManagedMissing)+len(check.ManagedChanged)+len(check.ManagedConflicts) != 0 {
		t.Fatalf("managed files failed verification: %#v", check)
	}
}

func TestSchema2ConflictNeedsMatchingConfirmation(t *testing.T) {
	old := []lockfile.ManagedFile{managedEntry("files/config.cfg", "config/config.cfg", "replace", []byte("old"))}
	next := []lockfile.ManagedFile{managedEntry("files/config.cfg", "config/config.cfg", "replace", []byte("new"))}
	root, revision := schema2Fixture(t, old, next, map[string][]byte{"config/config.cfg": []byte("local edit")}, map[string][]byte{"files/config.cfg": []byte("new")})
	localHash := sha256.Sum256([]byte("local edit"))
	confirmation := ConfirmedConflict{Target: "config/config.cfg", SHA256: hex.EncodeToString(localHash[:])}
	if _, err := RunRevision(context.Background(), root, revision, nil); failure.Code(err) != failure.Conflict {
		t.Fatalf("local edit was not reported as a conflict: %v", err)
	}
	if _, err := RunRevisionConfirmed(context.Background(), root, revision, nil, []ConfirmedConflict{{Target: confirmation.Target, SHA256: strings.Repeat("0", 64)}}); failure.Code(err) != failure.Conflict {
		t.Fatalf("stale confirmation was accepted: %v", err)
	}
	if _, err := RunRevisionConfirmed(context.Background(), root, revision, nil, []ConfirmedConflict{confirmation}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "config", "config.cfg")); err != nil || string(data) != "new" {
		t.Fatalf("confirmed build file not applied: %q, %v", data, err)
	}
}

func TestSchema2NewReplaceRequiresConfirmation(t *testing.T) {
	next := []lockfile.ManagedFile{managedEntry("files/config.cfg", "config/config.cfg", "replace", []byte("build"))}
	root, revision := schema2Fixture(t, nil, next, map[string][]byte{"config/config.cfg": []byte("local")}, map[string][]byte{"files/config.cfg": []byte("build")})
	preview, err := Check(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Conflicts) != 1 || preview.Conflicts[0].Target != "config/config.cfg" || preview.Conflicts[0].SHA256 == "" {
		t.Fatalf("new replace conflict missing from preview: %#v", preview.Conflicts)
	}
	if _, err = RunRevision(context.Background(), root, revision, nil); failure.Code(err) != failure.Conflict {
		t.Fatalf("new replace overwrote an unmanaged file: %v", err)
	}
	details, ok := failure.Details(err).(map[string]any)
	if !ok || details["conflicts"] == nil {
		t.Fatalf("conflict omitted structured details: %#v", failure.Details(err))
	}
	confirmation := ConfirmedConflict{Target: preview.Conflicts[0].Target, SHA256: preview.Conflicts[0].SHA256}
	if _, err = RunRevisionConfirmed(context.Background(), root, revision, nil, []ConfirmedConflict{confirmation}); err != nil {
		t.Fatal(err)
	}
}

func TestSchema2IfMissingToReplaceRequiresConfirmation(t *testing.T) {
	old := []lockfile.ManagedFile{managedEntry("files/config.cfg", "config/config.cfg", "if_missing", []byte("first"))}
	next := []lockfile.ManagedFile{managedEntry("files/config.cfg", "config/config.cfg", "replace", []byte("build"))}
	root, revision := schema2Fixture(t, old, next, map[string][]byte{"config/config.cfg": []byte("user")}, map[string][]byte{"files/config.cfg": []byte("build")})
	preview, err := Check(context.Background(), root, nil)
	if err != nil || len(preview.Conflicts) != 1 {
		t.Fatalf("policy transition conflict missing: %#v, %v", preview.Conflicts, err)
	}
	if _, err = RunRevision(context.Background(), root, revision, nil); failure.Code(err) != failure.Conflict {
		t.Fatalf("if_missing to replace overwrote local file: %v", err)
	}
	confirmation := ConfirmedConflict{Target: preview.Conflicts[0].Target, SHA256: preview.Conflicts[0].SHA256}
	if _, err = RunRevisionConfirmed(context.Background(), root, revision, nil, []ConfirmedConflict{confirmation}); err != nil {
		t.Fatal(err)
	}
}

func TestSchema2ChangedRemovedReplaceRequiresConfirmation(t *testing.T) {
	old := []lockfile.ManagedFile{managedEntry("files/config.cfg", "config/config.cfg", "replace", []byte("old"))}
	root, revision := schema2Fixture(t, old, nil, map[string][]byte{"config/config.cfg": []byte("user edit")}, nil)
	preview, err := Check(context.Background(), root, nil)
	if err != nil || len(preview.Conflicts) != 1 {
		t.Fatalf("removal conflict missing: %#v, %v", preview.Conflicts, err)
	}
	if _, err = RunRevision(context.Background(), root, revision, nil); failure.Code(err) != failure.Conflict {
		t.Fatalf("changed replace file was removed without confirmation: %v", err)
	}
	confirmation := ConfirmedConflict{Target: preview.Conflicts[0].Target, SHA256: preview.Conflicts[0].SHA256}
	if _, err = RunRevisionConfirmed(context.Background(), root, revision, nil, []ConfirmedConflict{confirmation}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "config", "config.cfg")); !os.IsNotExist(err) {
		t.Fatalf("confirmed removed file remains: %v", err)
	}
}

func TestSchema2MatchingReplaceNeedsNoConfirmation(t *testing.T) {
	data := []byte("already installed")
	next := []lockfile.ManagedFile{managedEntry("files/config.cfg", "config/config.cfg", "replace", data)}
	root, revision := schema2Fixture(t, nil, next, map[string][]byte{"config/config.cfg": data}, map[string][]byte{"files/config.cfg": data})
	preview, err := Check(context.Background(), root, nil)
	if err != nil || len(preview.Conflicts) != 0 {
		t.Fatalf("matching file requested confirmation: %#v, %v", preview.Conflicts, err)
	}
	if _, err = RunRevision(context.Background(), root, revision, nil); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRejectsManagedTargetThatAppearsDuringPreparation(t *testing.T) {
	next := []lockfile.ManagedFile{managedEntry("files/config.cfg", "config/config.cfg", "if_missing", []byte("build"))}
	root, revision := schema2Fixture(t, nil, next, nil, map[string][]byte{"files/config.cfg": []byte("build")})
	_, err := RunRevision(context.Background(), root, revision, func(message string) {
		if strings.Contains(message, "config/config.cfg") {
			if writeErr := os.MkdirAll(filepath.Join(root, "config"), 0755); writeErr != nil {
				t.Errorf("create target directory: %v", writeErr)
				return
			}
			if writeErr := os.WriteFile(filepath.Join(root, "config", "config.cfg"), []byte("appeared during prep"), 0644); writeErr != nil {
				t.Errorf("create raced target: %v", writeErr)
			}
		}
	})
	if failure.Code(err) != failure.Conflict {
		t.Fatalf("stale preparation overwrote newly created if_missing target: %v", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(root, "config", "config.cfg")); readErr != nil || string(got) != "appeared during prep" {
		t.Fatalf("raced file was overwritten: %q, %v", got, readErr)
	}
}

func TestMissingIfMissingRequiresRecovery(t *testing.T) {
	entry := managedEntry("files/config.cfg", "config/config.cfg", "if_missing", []byte("build"))
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	lock := &lockfile.File{Schema: 2, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", Name: "Test", Version: "1"}, Files: []lockfile.ManagedFile{entry}}
	if err := lockfile.Write(filepath.Join(root, lockfile.DefaultFilename), lock); err != nil {
		t.Fatal(err)
	}
	result, err := Verify(root)
	if err != nil || !result.NeedsRecovery || len(result.ManagedMissing) != 1 || result.ManagedMissing[0] != entry.Target {
		t.Fatalf("missing if_missing target was not marked for recovery: %#v, %v", result, err)
	}
}

func TestRepoSourceSync(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(filepath.Join(remote, "files", "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "files", "mods", "new.jar"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	next := &lockfile.File{Schema: 1, Pack: lockfile.Pack{Repository: remote, Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}, Mods: []lockfile.ModEntry{{ID: "test:mod", Version: "2", Filename: "new.jar", Source: "repo", Path: "files/mods/new.jar"}}}
	if err := lockfile.Write(filepath.Join(remote, "mod.lock"), next); err != nil {
		t.Fatal(err)
	}
	r, err := git.PlainInit(remote, false)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	if _, err = w.Add("mod.lock"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Add("files/mods/new.jar"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit("pack", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	mods := filepath.Join(root, "mods")
	if err = os.MkdirAll(mods, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(mods, "old.jar"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(mods, "unmanaged.jar"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	old := &lockfile.File{Schema: 1, Pack: next.Pack, Mods: []lockfile.ModEntry{{ID: "test:mod", Version: "1", Filename: "old.jar", Source: "repo", Path: "files/mods/old.jar"}}}
	if err = lockfile.Write(filepath.Join(root, "mod.lock"), old); err != nil {
		t.Fatal(err)
	}
	got, err := Run(context.Background(), root, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Diff.Added) != 0 || len(got.Diff.Removed) != 0 || len(got.Diff.Updated) != 1 {
		t.Fatalf("unexpected diff: %#v", got.Diff)
	}
	if _, err = os.Stat(filepath.Join(mods, "old.jar")); !os.IsNotExist(err) {
		t.Fatal("old tracked jar was not removed")
	}
	if b, err := os.ReadFile(filepath.Join(mods, "new.jar")); err != nil || string(b) != "new" {
		t.Fatalf("new jar: %q, %v", b, err)
	}
	if _, err = os.Stat(filepath.Join(mods, "unmanaged.jar")); err != nil {
		t.Fatal("unmanaged jar was removed")
	}

	// A bootstrap lock can already match the remote lock while mods/ is empty.
	// Sync must inspect the actual directory and restore every missing jar.
	if err = os.Remove(filepath.Join(mods, "new.jar")); err != nil {
		t.Fatal(err)
	}
	got, err = Run(context.Background(), root, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Diff.Added) != 1 || got.Diff.Added[0] != "new.jar" || got.Diff.Unchanged != 0 {
		t.Fatalf("missing installed jar was not added: %#v", got.Diff)
	}
	if b, readErr := os.ReadFile(filepath.Join(mods, "new.jar")); readErr != nil || string(b) != "new" {
		t.Fatalf("missing jar was not restored: %q, %v", b, readErr)
	}

	// Removing an entry from the published lock must remove the installed jar
	// on the next sync, even though the local lock currently matches the old
	// remote revision.
	if err = lockfile.Write(filepath.Join(remote, "mod.lock"), &lockfile.File{Schema: 1, Pack: next.Pack}); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Add("mod.lock"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit("remove mod", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	got, err = Run(context.Background(), root, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Diff.Removed) != 1 || got.Diff.Removed[0] != "new.jar" {
		t.Fatalf("removed jar was not reported: %#v", got.Diff)
	}
	if _, err = os.Stat(filepath.Join(mods, "new.jar")); !os.IsNotExist(err) {
		t.Fatalf("removed jar still exists, err=%v", err)
	}
}

func TestApplyRejectsRepositoryFileWithWrongHashBeforeChangingInstallation(t *testing.T) {
	pack := lockfile.Pack{Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}
	wrongHash := strings.Repeat("0", 64)
	root, remote, _ := syncFixture(t, pack, pack, nil, []lockfile.ModEntry{{ID: "test:mod", Filename: "new.jar", Source: "repo", Path: "files/mods/new.jar", SHA256: wrongHash}})
	pack.Repository = remote
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 1, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "mod.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), root, nil); err == nil || !strings.Contains(err.Error(), "SHA-256 does not match") {
		t.Fatalf("Run error = %v; want hash mismatch", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "mod.lock")); err != nil || string(got) != string(before) {
		t.Fatalf("lock changed after staged hash failure: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "mods", "new.jar")); !os.IsNotExist(err) {
		t.Fatalf("bad staged file reached instance: %v", err)
	}
}

func TestApplyRejectsModTargetThatChangesDuringPreparation(t *testing.T) {
	pack := lockfile.Pack{Repository: "", Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}
	oldMod := lockfile.ModEntry{ID: "test:mod", Version: "1", Filename: "same.jar", Source: "repo", Path: "files/mods/old.jar"}
	nextMod := lockfile.ModEntry{ID: "test:mod", Version: "2", Filename: "same.jar", Source: "repo", Path: "files/mods/new.jar"}
	root, remote, _ := syncFixture(t, pack, pack, []lockfile.ModEntry{oldMod}, []lockfile.ModEntry{nextMod})
	pack.Repository = remote
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 1, Pack: pack, Mods: []lockfile.ModEntry{oldMod}}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "mods", "same.jar")
	_, err := Run(context.Background(), root, func(message string) {
		if strings.Contains(message, "same.jar") {
			if writeErr := os.WriteFile(target, []byte("changed during preparation"), 0644); writeErr != nil {
				t.Errorf("change mod during preparation: %v", writeErr)
			}
		}
	})
	if failure.Code(err) != failure.Conflict {
		t.Fatalf("stale mod plan was applied: %v", err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "changed during preparation" {
		t.Fatalf("local mod was overwritten: %q, %v", got, readErr)
	}
}

func syncFixture(t *testing.T, oldPack, nextPack lockfile.Pack, oldMods, nextMods []lockfile.ModEntry) (string, string, *git.Worktree) {
	t.Helper()
	if oldPack.Repository == "" {
		oldPack.Repository = "fixture"
	}
	if nextPack.Repository == "" {
		nextPack.Repository = "fixture"
	}
	remote := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(filepath.Join(remote, "files", "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, m := range nextMods {
		if m.Source == "repo" {
			p := filepath.Join(remote, filepath.FromSlash(m.Path))
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("payload-"+m.Filename), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := lockfile.Write(filepath.Join(remote, "mod.lock"), &lockfile.File{Schema: 1, Pack: nextPack, Mods: nextMods}); err != nil {
		t.Fatal(err)
	}
	r, err := git.PlainInit(remote, false)
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Add("mod.lock"); err != nil {
		t.Fatal(err)
	}
	for _, m := range nextMods {
		if m.Source == "repo" {
			if _, err = w.Add(filepath.ToSlash(m.Path)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = w.Commit("pack", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err = os.MkdirAll(filepath.Join(root, oldPack.ModsDir), 0755); err != nil {
		t.Fatal(err)
	}
	for _, m := range oldMods {
		if err = os.WriteFile(filepath.Join(root, oldPack.ModsDir, m.Filename), []byte("old-"+m.Filename), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err = lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 1, Pack: oldPack, Mods: oldMods}); err != nil {
		t.Fatal(err)
	}
	return root, remote, w
}

func TestSyncRejectsModsDirMigrationBeforeTouchingFiles(t *testing.T) {
	oldPack := lockfile.Pack{Repository: "", Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}
	nextPack := oldPack
	nextPack.Repository = "x"
	nextPack.ModsDir = "other-mods"
	root, remote, _ := syncFixture(t, oldPack, nextPack, []lockfile.ModEntry{{ID: "x", Filename: "old.jar", Source: "repo", Path: "files/mods/old.jar"}}, nil)
	// The repository path is the absolute local path configured in the local lock.
	oldPack.Repository = remote
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 1, Pack: oldPack, Mods: []lockfile.ModEntry{{ID: "x", Filename: "old.jar", Source: "repo", Path: "files/mods/old.jar"}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "mod.lock"))
	if _, err := Run(context.Background(), root, func(string) {}); err == nil || !strings.Contains(err.Error(), "unsupported migration") {
		t.Fatalf("Run error = %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "mods", "old.jar")); err != nil || string(b) != "old-old.jar" {
		t.Fatalf("managed file changed: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "mod.lock")); string(b) != string(before) {
		t.Fatal("local lock changed")
	}
}

func TestSyncRejectsUnmanagedCollisionAndRestoresAfterLockReplaceFailure(t *testing.T) {
	pack := lockfile.Pack{Repository: "", Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}
	remoteNext := []lockfile.ModEntry{{ID: "x", Filename: "new.jar", Source: "repo", Path: "files/mods/new.jar"}}
	root, remote, _ := syncFixture(t, pack, pack, nil, remoteNext)
	pack.Repository = remote
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 1, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mods", "new.jar"), []byte("unmanaged"), 0644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "mod.lock"))
	if _, err := Run(context.Background(), root, func(string) {}); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("Run error = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "mods", "new.jar")); string(b) != "unmanaged" {
		t.Fatal("unmanaged file was overwritten")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "mod.lock")); string(b) != string(before) {
		t.Fatal("local lock changed")
	}

	// Now install to an empty target and fail only the atomic lock replacement.
	if err := os.Remove(filepath.Join(root, "mods", "new.jar")); err != nil {
		t.Fatal(err)
	}
	prior := commitLockFile
	commitLockFile = func(string, string) error { return errors.New("injected lock replacement failure") }
	t.Cleanup(func() { commitLockFile = prior })
	if _, err := Run(context.Background(), root, func(string) {}); err == nil {
		t.Fatal("expected lock replacement failure")
	}
	if _, err := os.Stat(filepath.Join(root, "mods", "new.jar")); !os.IsNotExist(err) {
		t.Fatal("installed mod was not rolled back")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "mod.lock")); string(b) != string(before) {
		t.Fatal("failed replacement changed the lock")
	}
}

func TestSyncPreservesBackupAndReportsLocationWhenRollbackFails(t *testing.T) {
	pack := lockfile.Pack{Repository: "", Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}
	oldMod := lockfile.ModEntry{ID: "x", Filename: "old.jar", Source: "repo", Path: "files/mods/old.jar"}
	newMod := lockfile.ModEntry{ID: "x", Filename: "new.jar", Source: "repo", Path: "files/mods/new.jar"}
	root, remote, _ := syncFixture(t, pack, pack, []lockfile.ModEntry{oldMod}, []lockfile.ModEntry{newMod})
	pack.Repository = remote
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 1, Pack: pack, Mods: []lockfile.ModEntry{oldMod}}); err != nil {
		t.Fatal(err)
	}
	oldRename, oldRestore := renameInstalledFile, restoreBackupFile
	renameInstalledFile = func(string, string) error { return errors.New("injected install failure") }
	restoreBackupFile = func(string, string) error { return errors.New("injected restore failure") }
	t.Cleanup(func() { renameInstalledFile = oldRename; restoreBackupFile = oldRestore })
	_, err := Run(context.Background(), root, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "backups preserved at ") {
		t.Fatalf("Run error = %v", err)
	}
	backupPath := strings.TrimPrefix(err.Error()[strings.Index(err.Error(), "backups preserved at "):], "backups preserved at ")
	entries, readErr := os.ReadDir(backupPath)
	if readErr != nil || len(entries) == 0 {
		t.Fatalf("rollback backup unavailable at %s: %v", backupPath, readErr)
	}
	if b, readErr := os.ReadFile(filepath.Join(backupPath, entries[0].Name())); readErr != nil || string(b) != "old-old.jar" {
		t.Fatalf("preserved backup = %q, %v", b, readErr)
	}
}

func TestSyncLockRejectsConcurrentRun(t *testing.T) {
	pack := lockfile.Pack{Repository: "", Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}
	root, remote, _ := syncFixture(t, pack, pack, nil, nil)
	pack.Repository = remote
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 1, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), root, func(string) {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-resume
		})
		firstDone <- err
	}()
	<-entered
	if _, err := Run(context.Background(), root, func(string) {}); err == nil || !strings.Contains(err.Error(), "another ModLock sync") {
		t.Fatalf("concurrent Run error = %v", err)
	}
	close(resume)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}
