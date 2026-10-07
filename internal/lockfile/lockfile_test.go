package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
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

func TestSchema2RoundTripAndValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mod.lock")
	hash := hex.EncodeToString(sha256.New().Sum(nil))
	want := &File{
		Schema: 2,
		Pack:   Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Example", Version: "1.2", Components: []Component{{ID: "net.minecraft", Version: "1.21.1"}, {ID: "fabric-loader", Version: "0.16.5"}}},
		Mods:   []ModEntry{{ID: "modrinth:abc", Filename: "example.jar", Source: "modrinth", URL: "https://example.test/example.jar", SHA256: hash}},
		Files:  []ManagedFile{{Path: "config/example.toml", Target: "config/example.toml", SHA256: hash, Policy: "replace"}, {Path: "scripts/start.ps1", Target: "scripts/start.ps1", SHA256: hash, Policy: "if_missing"}},
	}
	if err := Write(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != 2 || got.Pack.Name != "Example" || len(got.Pack.Components) != 2 || len(got.Files) != 2 || got.Mods[0].SHA256 != hash {
		t.Fatalf("unexpected schema 2 round trip: %#v", got)
	}
	got.Files[1].Target = "config/example.toml/child"
	if err := got.Validate(); err == nil {
		t.Fatal("expected overlapping destinations to be rejected")
	}
}
func TestResolveWithinRejectsEscape(t *testing.T) {
	if _, e := ResolveWithin(t.TempDir(), "../outside"); e == nil {
		t.Fatal("expected path escape error")
	}
}

func TestSchema2RejectsReservedTargets(t *testing.T) {
	hash := hex.EncodeToString(sha256.New().Sum(nil))
	for _, target := range []string{".git/config", ".modlock/author.toml", ".modlock-sync-a/stage/file", "mod.lock", "modlock.lock", "config/mod.lock.bak"} {
		lock := &File{Schema: 2, Pack: Pack{Repository: "https://example.test/pack.git", Name: "Example", Version: "1"}, Files: []ManagedFile{{Path: "files/a", Target: target, SHA256: hash, Policy: "replace"}}}
		if err := lock.Validate(); err == nil {
			t.Errorf("reserved target %q was accepted", target)
		}
	}
}

func TestSchema3RoundTripTargetsAndStableOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mod.lock")
	hash := hex.EncodeToString(sha256.New().Sum(nil))
	want := &File{Schema: 3,
		Pack:  Pack{Repository: "https://example.test/pack.git", Name: "Example", Version: "1", Components: []Component{{ID: "net.minecraft", Version: "1.21.1"}}},
		Mods:  []ModEntry{{ID: "shared", Filename: "shared.jar", Source: "repo", Path: "files/shared.jar", SHA256: hash, Targets: []string{"server", "client", "custom"}}},
		Files: []ManagedFile{{Path: "files/config/a", Target: "config/a", SHA256: hash, Policy: "replace", Targets: []string{"server", "client"}}},
	}
	if err := Write(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != 3 || len(got.Mods[0].Targets) != 3 || got.Mods[0].Targets[0] != "client" || got.Mods[0].Targets[1] != "server" || got.Mods[0].Targets[2] != "custom" {
		t.Fatalf("unexpected schema 3 round trip/order: %#v", got)
	}
}

func TestSchema3RejectsInvalidOrDuplicateTargets(t *testing.T) {
	hash := hex.EncodeToString(sha256.New().Sum(nil))
	base := func(ids []string) *File {
		return &File{Schema: 3, Pack: Pack{Repository: "https://example.test/pack.git", Name: "Example", Version: "1"}, Mods: []ModEntry{{Filename: "a.jar", Source: "repo", Path: "files/a.jar", SHA256: hash, Targets: ids}}}
	}
	for _, ids := range [][]string{{}, {""}, {"../client"}, {"a/b"}, {"client", "client"}, {"Client"}, {"bad.id"}} {
		if err := base(ids).Validate(); err == nil {
			t.Errorf("accepted targets %q", ids)
		}
	}
}

func TestSchema3AllowsSameDestinationForDisjointTargets(t *testing.T) {
	hash := hex.EncodeToString(sha256.New().Sum(nil))
	lock := &File{Schema: 3, Pack: Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Example", Version: "1"}, Mods: []ModEntry{
		{ID: "client-mod", Filename: "same.jar", Source: "repo", Path: "files/client.jar", SHA256: hash, Targets: []string{"client"}},
		{ID: "server-mod", Filename: "same.jar", Source: "repo", Path: "files/server.jar", SHA256: hash, Targets: []string{"server"}},
	}}
	if err := lock.Validate(); err != nil {
		t.Fatalf("disjoint target destinations should be valid: %v", err)
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
