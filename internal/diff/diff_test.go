package diff

import (
	"os"
	"path/filepath"
	"testing"

	"modlock/internal/lockfile"
)

func lf(names ...string) *lockfile.File {
	f := &lockfile.File{}
	for _, n := range names {
		f.Mods = append(f.Mods, lockfile.ModEntry{Filename: n})
	}
	return f
}
func TestLocks(t *testing.T) {
	d := Locks(lf("old.jar", "same.jar"), lf("new.jar", "same.jar"))
	if len(d.Added) != 1 || d.Added[0] != "new.jar" || len(d.Removed) != 1 || d.Removed[0] != "old.jar" || d.Unchanged != 1 {
		t.Fatalf("bad diff: %#v", d)
	}
}
func TestLocal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "new.jar"), nil, 0644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0644)
	d, e := Local(dir, lf("old.jar"))
	if e != nil {
		t.Fatal(e)
	}
	if len(d.Added) != 1 || d.Added[0] != "new.jar" || len(d.Removed) != 1 {
		t.Fatalf("bad diff: %#v", d)
	}
}

func TestLocksRecognizesVersionUpdate(t *testing.T) {
	old := &lockfile.File{Mods: []lockfile.ModEntry{{ID: "mod:create", Version: "1.0", Filename: "create-1.0.jar", Source: "repo", Path: "files/mods/create-1.0.jar"}}}
	next := &lockfile.File{Mods: []lockfile.ModEntry{{ID: "mod:create", Version: "2.0", Filename: "create-2.0.jar", Source: "repo", Path: "files/mods/create-2.0.jar"}}}
	d := Locks(old, next)
	if len(d.Updated) != 1 || len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Fatalf("expected update, got %#v", d)
	}
	if d.Updated[0].Old.DisplayVersion() != "1.0" || d.Updated[0].New.DisplayVersion() != "2.0" {
		t.Fatalf("bad versions: %#v", d.Updated[0])
	}
}
