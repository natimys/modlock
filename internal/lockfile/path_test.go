package lockfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPortablePathRejections(t *testing.T) {
	for _, path := range []string{`..\outside`, `../outside`, `C:\outside`, `config/file:stream`, `/absolute`, `\\server\share`, `config/NUL.txt`, `config/COM1.jar`, `config/name.`, `config/name `, "config/line\nbreak", `config/*.json`} {
		if _, err := ResolveWithin(t.TempDir(), path); err == nil {
			t.Errorf("accepted unsafe path %q", path)
		}
	}
}

func TestResolveWithinRefusesLinkedParentAndRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	linked := filepath.Join(root, "config")
	if err := os.Symlink(outside, linked); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := ResolveWithin(root, "config/new.json"); err == nil {
		t.Fatal("resolved destination through linked parent")
	}
	if _, err := ResolveWithin(linked, "new.json"); err == nil {
		t.Fatal("accepted linked instance root")
	}
}

func TestResolveWithinAcceptsMissingDestinationAndUnicode(t *testing.T) {
	root := t.TempDir()
	got, err := ResolveWithin(root, `конфиги с пробелами\new.json`)
	want := filepath.Join(root, "конфиги с пробелами", "new.json")
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}
