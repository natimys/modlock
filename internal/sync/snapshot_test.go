package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"modlock/internal/lockfile"
)

func TestApplyUsesPreviewedCommitAfterBranchMoves(t *testing.T) {
	remote, root := t.TempDir(), t.TempDir()
	repo, err := git.PlainInit(remote, false)
	if err != nil {
		t.Fatal(err)
	}
	worktree, _ := repo.Worktree()
	source := lockfile.Pack{Repository: remote, Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}
	writeVersion := func(version string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(remote, "files", "mods"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(remote, "files", "mods", "same.jar"), []byte(version), 0644); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(version))
		lock := &lockfile.File{Schema: 1, Pack: source, Mods: []lockfile.ModEntry{{ID: "test:mod", Filename: "same.jar", Version: version, Source: "repo", Path: "files/mods/same.jar", SHA256: hex.EncodeToString(digest[:])}}}
		if err := lockfile.Write(filepath.Join(remote, "mod.lock"), lock); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"mod.lock", "files/mods/same.jar"} {
			if _, err := worktree.Add(path); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := worktree.Commit(version, &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.test", When: time.Now()}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 1, Pack: source}); err != nil {
		t.Fatal(err)
	}
	writeVersion("previewed")
	preview, err := Check(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Diff.Added) != 1 || len(preview.Revision) != 40 {
		t.Fatalf("invalid preview: %#v", preview)
	}
	if _, err = os.Stat(filepath.Join(root, "mods", "same.jar")); !os.IsNotExist(err) {
		t.Fatal("preview installed files")
	}
	writeVersion("later branch tip")
	result, err := RunRevision(context.Background(), root, preview.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "mods", "same.jar"))
	if err != nil || string(content) != "previewed" || result.Revision != preview.Revision {
		t.Fatalf("installed branch tip instead of preview: %q, %q, %v", content, result.Revision, err)
	}
	if err := os.Remove(filepath.Join(root, "mods", "same.jar")); err != nil {
		t.Fatal(err)
	}
	if _, err := RunRevision(context.Background(), root, preview.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mods", "same.jar"), []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	local, err := Verify(root)
	if err != nil || !local.NeedsRecovery || len(local.Damaged) != 1 {
		t.Fatalf("corruption was not reported: %#v, %v", local, err)
	}
	if _, err := RunRevision(context.Background(), root, preview.Revision, nil); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(filepath.Join(root, "mods", "same.jar"))
	if err != nil || string(content) != "previewed" {
		t.Fatalf("same-commit repair failed: %q, %v", content, err)
	}
}

func TestVerifyHashlessFilesArePresentButUnverified(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mods", "legacy.jar"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	lock := &lockfile.File{Schema: 1, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", ModsDir: "mods"}, Mods: []lockfile.ModEntry{{Filename: "legacy.jar", Source: "repo", Path: "files/legacy.jar"}}}
	result, err := VerifyLock(root, lock)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "unverified" || result.NeedsRecovery || len(result.Unverified) != 1 {
		t.Fatalf("hashless file was overclaimed: %#v", result)
	}
	if err := os.WriteFile(filepath.Join(root, "mods", "legacy.jar"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	result, err = VerifyLock(root, lock)
	if err != nil || !result.NeedsRecovery || len(result.Damaged) != 1 {
		t.Fatalf("empty file was not reported as damaged: %#v, %v", result, err)
	}
}

func TestSnapshotRejectsUnpinnedApplyAndInvalidHash(t *testing.T) {
	if _, err := RunRevision(context.Background(), t.TempDir(), "", nil); err == nil {
		t.Fatal("apply accepted missing commit")
	}
	if _, err := Fetch(context.Background(), lockfile.Pack{Repository: "https://example.test/pack.git"}, "main", nil); err == nil {
		t.Fatal("snapshot accepted moving revision name")
	}
}
