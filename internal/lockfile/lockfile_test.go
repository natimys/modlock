package lockfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mod.lock")
	want := &File{Schema: 1, Pack: Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods"}, Mods: []ModEntry{{Filename: "a.jar", Source: "repo", Path: "files/mods/a.jar"}}}
	if err := Write(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mods) != 1 || got.Mods[0].Filename != "a.jar" {
		t.Fatalf("unexpected: %#v", got)
	}
}
func TestResolveWithinRejectsEscape(t *testing.T) {
	if _, e := ResolveWithin(t.TempDir(), "../outside"); e == nil {
		t.Fatal("expected path escape error")
	}
}
func TestReadRejectsProviderWithoutURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	os.WriteFile(p, []byte("schema=1\n[pack]\nrepository='x'\n[[mods]]\nfilename='a.jar'\nsource='modrinth'\n"), 0644)
	if _, e := Read(p); e == nil {
		t.Fatal("expected validation error")
	}
}

func TestDefaultLockName(t *testing.T) {
	f := &File{Schema: 1, Pack: Pack{Repository: "https://example.test/pack.git"}}
	f.Defaults()
	if f.Pack.LockPath != "mod.lock" {
		t.Fatalf("default lock path=%q", f.Pack.LockPath)
	}
}
