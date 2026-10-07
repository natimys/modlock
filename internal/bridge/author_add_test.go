package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"modlock/internal/lockfile"
	syncer "modlock/internal/sync"
)

func TestAddAuthorModStagesIntoSelectedTargetAndDesiredLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "client")
	server := filepath.Join(t.TempDir(), "server")
	for _, p := range []string{root, server, filepath.Join(root, ".modlock", "staging")} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	pack := lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 3, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	data := []byte("jar bytes")
	stageRel := ".modlock/staging/new.jar"
	if err := os.WriteFile(filepath.Join(root, ".modlock", "staging", "new.jar"), data, 0600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	result, err := addAuthorMod(root, addModRequest{Mod: lockfile.ModEntry{ID: "modrinth:example", Version: "1.0", Filename: "example.jar", Source: "modrinth", ProjectID: "example", VersionID: "v1", URL: "https://cdn.example.test/example.jar", SHA256: hex.EncodeToString(h[:])}, Targets: []string{"server"}, StagedFile: stageRel, TargetRoots: syncer.TargetRoots{"server": server}})
	if err != nil {
		t.Fatal(err)
	}
	_ = result
	if _, err := os.Stat(filepath.Join(root, "mods", "example.jar")); !os.IsNotExist(err) {
		t.Fatalf("client must stay unchanged, stat err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(server, "mods", "example.jar")); err != nil || string(got) != string(data) {
		t.Fatalf("server target=%q err=%v", got, err)
	}
	updated, err := lockfile.Read(filepath.Join(root, "mod.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Mods) != 1 || len(updated.Mods[0].Targets) != 1 || updated.Mods[0].Targets[0] != "server" || updated.Mods[0].SHA256 != hex.EncodeToString(h[:]) {
		t.Fatalf("desired lock: %#v", updated.Mods)
	}
}

func TestAddAuthorModRejectsAnyStageOutsideDedicatedSubtree(t *testing.T) {
	root := t.TempDir()
	pack := lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 3, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	_, err := addAuthorMod(root, addModRequest{Mod: lockfile.ModEntry{Filename: "x.jar", Source: "repo", Path: "files/x.jar"}, Targets: []string{"client"}, StagedFile: ".modlock/other/x.jar", TargetRoots: syncer.TargetRoots{"client": root}})
	if err == nil {
		t.Fatal("expected stage path rejection")
	}
}
