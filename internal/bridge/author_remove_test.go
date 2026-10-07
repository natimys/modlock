package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"modlock/internal/failure"
	"modlock/internal/lockfile"
	syncer "modlock/internal/sync"
)

func TestRemoveAuthorResourceDeletesOnlyMatchingSelectedTarget(t *testing.T) {
	client := filepath.Join(t.TempDir(), "client")
	server := filepath.Join(t.TempDir(), "server")
	for _, p := range []string{client, server} {
		if err := os.MkdirAll(filepath.Join(p, "mods"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("jar")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(server, "mods", "server.jar"), data, 0644); err != nil {
		t.Fatal(err)
	}
	pack := lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}
	manifest := &lockfile.File{Schema: 3, Pack: pack, Mods: []lockfile.ModEntry{{ID: "example:mod", Filename: "server.jar", Source: "repo", Path: "files/server.jar", SHA256: hash, Targets: []string{"server"}}}}
	lockPath := filepath.Join(client, "mod.lock")
	if err := lockfile.Write(lockPath, manifest); err != nil {
		t.Fatal(err)
	}
	_, err := removeAuthorResource(client, removeResourceRequest{Kind: "mod", Identity: "example:mod", Targets: []string{"server"}, ExpectedSHA256: hash, TargetRoots: syncer.TargetRoots{"client": client, "server": server}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(server, "mods", "server.jar")); !os.IsNotExist(err) {
		t.Fatalf("selected server bytes remain, err=%v", err)
	}
	updated, err := lockfile.Read(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Mods) != 0 {
		t.Fatalf("mod was not removed: %#v", updated.Mods)
	}
}

func TestRemoveAuthorResourceRejectsModifiedBytesWithoutDeleting(t *testing.T) {
	client := t.TempDir()
	server := t.TempDir()
	if err := os.MkdirAll(filepath.Join(server, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	expected := []byte("expected")
	actual := []byte("edited")
	sum := sha256.Sum256(expected)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(server, "mods", "server.jar")
	if err := os.WriteFile(path, actual, 0644); err != nil {
		t.Fatal(err)
	}
	manifest := &lockfile.File{Schema: 3, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}, Mods: []lockfile.ModEntry{{ID: "example:mod", Filename: "server.jar", Source: "repo", Path: "files/server.jar", SHA256: hash, Targets: []string{"server"}}}}
	if err := lockfile.Write(filepath.Join(client, "mod.lock"), manifest); err != nil {
		t.Fatal(err)
	}
	_, err := removeAuthorResource(client, removeResourceRequest{Kind: "mod", Identity: "example:mod", TargetRoots: syncer.TargetRoots{"client": client, "server": server}})
	if failure.Code(err) != failure.Conflict {
		t.Fatalf("expected conflict, got %v", err)
	}
	if details, _ := failure.Details(err).(map[string]any); details["kind"] != "locally_modified" {
		t.Fatalf("stable kind: %#v", details)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(actual) {
		t.Fatalf("modified bytes changed: %q err=%v", got, err)
	}
}
