package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteLeavesDestinationIntactWhenAtomicReplaceFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mod.lock")
	if err := os.WriteFile(path, []byte("old lock"), 0644); err != nil {
		t.Fatal(err)
	}
	oldReplace := replaceAtomic
	replaceAtomic = func(string, string) error { return errors.New("injected replace failure") }
	t.Cleanup(func() { replaceAtomic = oldReplace })
	if err := Write(path, []byte("new lock"), 0644); err == nil {
		t.Fatal("expected replace failure")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old lock" {
		t.Fatalf("destination = %q", got)
	}
}
