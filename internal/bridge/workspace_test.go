package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modlock/internal/lockfile"
	syncer "modlock/internal/sync"
)

func TestPromoteSchema3LockKeepsBackupAndWritesWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	client := filepath.Join(workspace, "minecraft")
	server := filepath.Join(workspace, "server")
	for _, root := range []string{client, server} {
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatal(err)
		}
	}
	legacyPath := filepath.Join(client, lockfile.DefaultFilename)
	lock := &lockfile.File{Schema: 3, Pack: lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Test", Version: "1"}, Mods: []lockfile.ModEntry{{ID: "modrinth:test", Filename: "test.jar", Source: "modrinth", URL: "https://example.test/test.jar", Targets: []string{"server"}, SHA256: strings.Repeat("a", 64)}}}
	if err := lockfile.Write(legacyPath, lock); err != nil {
		t.Fatal(err)
	}
	if _, err := promoteSchema3Lock(workspace, syncer.TargetRoots{"client": client, "server": server}); err != nil {
		t.Fatal(err)
	}
	if _, err := lockfile.Read(filepath.Join(workspace, lockfile.DefaultFilename)); err != nil {
		t.Fatalf("workspace lock missing or invalid: %v", err)
	}
	if _, err := os.Stat(legacyPath + ".workspace.bak"); err != nil {
		t.Fatalf("legacy lock backup missing: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy lock should be moved to backup, stat error=%v", err)
	}
}
