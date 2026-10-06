package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"modlock/internal/lockfile"
)

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
