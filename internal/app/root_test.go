package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitRootWinsEvenWhenEmpty(t *testing.T) {
	instance := filepath.Join(t.TempDir(), "Сборка с пробелами")
	if err := os.Mkdir(instance, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ExplicitRootEnv, instance)
	t.Setenv("MODLOCK_LAUNCHER_DIR", t.TempDir())
	for _, requireLock := range []bool{false, true} {
		root, err := FindRoot(requireLock)
		if err != nil || root != instance {
			t.Fatalf("explicit root was not used: %q, %v", root, err)
		}
	}
}

func TestInvalidExplicitRootNeverFallsBack(t *testing.T) {
	for _, root := range []string{filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "file")} {
		if filepath.Base(root) == "file" {
			if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv(ExplicitRootEnv, root)
		if _, err := FindRoot(false); err == nil {
			t.Fatalf("accepted invalid root %q", root)
		}
	}
}
