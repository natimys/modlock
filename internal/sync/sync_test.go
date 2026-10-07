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
