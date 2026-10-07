package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ignorefile "modlock/internal/ignore"
	"modlock/internal/lockfile"
	"modlock/internal/providers"
)

func TestMigrateSchema2To3MakesEntriesClientOnlyAndKeepsBackup(t *testing.T) {
	root := t.TempDir()
	oldRoot := os.Getenv(ExplicitRootEnv)
	t.Cleanup(func() { _ = os.Setenv(ExplicitRootEnv, oldRoot) })
	if err := os.Setenv(ExplicitRootEnv, root); err != nil {
		t.Fatal(err)
	}
	data := []byte("content")
	h := sha256.Sum256(data)
	lock := &lockfile.File{Schema: 2, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}, Mods: []lockfile.ModEntry{{ID: "example:mod", Filename: "a.jar", Source: "repo", Path: "files/mods/a.jar", SHA256: hex.EncodeToString(h[:])}}, Files: []lockfile.ManagedFile{{Path: "files/config/a", Target: "config/a", SHA256: hex.EncodeToString(h[:]), Policy: "replace"}}}
	path := filepath.Join(root, "mod.lock")
	if err := lockfile.Write(path, lock); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateSchema3(nil); err != nil {
		t.Fatal(err)
	}
	got, err := lockfile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != 3 || len(got.Mods) != 1 || len(got.Mods[0].Targets) != 1 || got.Mods[0].Targets[0] != "client" || len(got.Files) != 1 || got.Files[0].Targets[0] != "client" {
		t.Fatalf("unexpected migrated lock: %#v", got)
	}
	backup, err := os.ReadFile(path + ".schema2.bak")
	if err != nil || string(backup) != string(before) {
		t.Fatalf("schema2 backup differs: %v", err)
	}
}

func TestMigrateSchema2To3RefusesExistingBackupWithoutChangingLock(t *testing.T) {
	root := t.TempDir()
	oldRoot := os.Getenv(ExplicitRootEnv)
	t.Cleanup(func() { _ = os.Setenv(ExplicitRootEnv, oldRoot) })
	if err := os.Setenv(ExplicitRootEnv, root); err != nil {
		t.Fatal(err)
	}
	lock := &lockfile.File{Schema: 2, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}}
	path := filepath.Join(root, "mod.lock")
	if err := lockfile.Write(path, lock); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".schema2.bak", []byte("do not overwrite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateSchema3(nil); err == nil {
		t.Fatal("expected existing-backup refusal")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".schema2.bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || string(backup) != "do not overwrite" {
		t.Fatalf("migration modified inputs: lock=%q backup=%q", after, backup)
	}
}

func TestAddRepoSource(t *testing.T) {
	root := t.TempDir()
	oldWD, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	if err := os.MkdirAll(filepath.Join(root, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mods", "custom.jar"), []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	lf := &lockfile.File{Schema: 1, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods"}}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), lf); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	oldFactory := newDetector
	newDetector = func() *providers.Detector {
		return &providers.Detector{Client: server.Client(), ModrinthBase: server.URL, CurseForgeBase: server.URL}
	}
	defer func() { newDetector = oldFactory }()
	if err := Add(context.Background(), []string{"custom.jar"}, bytes.NewBufferString("r\n")); err != nil {
		t.Fatal(err)
	}
	got, err := lockfile.Read(filepath.Join(root, lockfile.DefaultFilename))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mods) != 1 || got.Mods[0].Source != "repo" {
		t.Fatalf("unexpected lock: %#v", got.Mods)
	}
	if _, err = os.Stat(filepath.Join(root, "files", "mods", "custom.jar")); err != nil {
		t.Fatal(err)
	}
	if err = Ignore(context.Background(), []string{filepath.Join("mods", "custom.jar")}, bytes.NewBuffer(nil)); err != nil {
		t.Fatal(err)
	}
	got, err = lockfile.Read(filepath.Join(root, lockfile.DefaultFilename))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mods) != 0 {
		t.Fatalf("ignored mod remains in lock: %#v", got.Mods)
	}
	ignored, err := ignorefile.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if !ignored.Contains("repo:custom") {
		t.Fatalf("missing entity in ignore: %#v", ignored.IDs)
	}
	if _, err = os.Stat(filepath.Join(root, "files", "mods", "custom.jar")); !os.IsNotExist(err) {
		t.Fatal("ignored repo artifact was not removed")
	}
}

func TestFindRootUsesLauncherDirectoryBeforeCachedPayloadDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	instance := filepath.Join(t.TempDir(), "Minecraft сборка с пробелами")
	appCache := filepath.Join(t.TempDir(), "Local AppData", "ModLock", "versions", "1.2.3")
	if err = os.MkdirAll(instance, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(appCache, 0755); err != nil {
		t.Fatal(err)
	}
	lf := &lockfile.File{Schema: 1, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", ModsDir: "mods"}}
	if err = lockfile.Write(filepath.Join(instance, "mod.lock"), lf); err != nil {
		t.Fatal(err)
	}
	if err = lockfile.Write(filepath.Join(appCache, "mod.lock"), lf); err != nil {
		t.Fatal(err)
	}
	away := t.TempDir()
	if err = os.Chdir(away); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	t.Setenv("MODLOCK_LAUNCHER_DIR", instance)
	got, err := FindRoot(true)
	if err != nil {
		t.Fatal(err)
	}
	if got != instance {
		t.Fatalf("FindRoot() = %q, want launcher instance %q", got, instance)
	}
	if err = os.Chdir(instance); err != nil {
		t.Fatal(err)
	}
	if got, err = FindRoot(true); err != nil || got != instance {
		t.Fatalf("cwd priority: FindRoot() = %q, %v", got, err)
	}
}
