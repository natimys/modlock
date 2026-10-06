package updater

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{{"1.10.0", "1.9.9", 1}, {"1.2.3-alpha.2", "1.2.3-alpha.10", -1}, {"1.2.3-rc.1", "1.2.3", -1}, {"v1.2.3+build.9", "1.2.3+build.10", 0}}
	for _, tc := range cases {
		got, err := compareVersions(tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Errorf("compareVersions(%q,%q) = %d, %v; want %d", tc.a, tc.b, got, err, tc.want)
		}
	}
	for _, bad := range []string{"1.2", "01.2.3", "1.2.3-01", "1.2.3+bad..metadata"} {
		if _, err := parseVersion(bad); err == nil {
			t.Errorf("parseVersion(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestInvalidPointersAndVersionsNeverBecomePaths(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if err := SetActiveVersion("1.2.3"); err != nil {
		t.Fatal(err)
	}
	active, _ := ActivePath()
	before, _ := os.ReadFile(active)
	if err := SetActiveVersion("../outside"); err == nil {
		t.Fatal("accepted path-like version")
	}
	for _, version := range []string{" v1.2.3", "v1.2.3", "1.2.3 "} {
		if err := SetActiveVersion(version); err == nil {
			t.Errorf("accepted non-canonical pointer %q", version)
		}
	}
	if _, err := AppPath("../../outside"); err == nil {
		t.Fatal("accepted path-like app version")
	}
	if err := InitializeBootstrap(filepath.Join(t.TempDir(), "payload.exe"), "../outside"); err == nil {
		t.Fatal("accepted path-like bootstrap version")
	}
	if err := os.WriteFile(active, []byte(`{"version":"../../outside"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ActiveVersion(); got != "" {
		t.Fatalf("invalid active pointer returned %q", got)
	}
	if err := os.WriteFile(active, before, 0600); err != nil {
		t.Fatal(err)
	}
	if got := ActiveVersion(); got != "1.2.3" {
		t.Fatalf("valid pointer changed: %q", got)
	}
}

func TestAutomaticRollbackVerifiesFallbackAndRejectsFailedRelease(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	oldCheck := healthCheckPayload
	defer func() { healthCheckPayload = oldCheck }()
	healthCheckPayload = func(version string) error {
		if version == "1.0.0" {
			return errors.New("broken fallback")
		}
		return nil
	}
	for _, version := range []string{"1.0.0", "2.0.0"} {
		path, err := AppPath(version)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte("payload"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetActiveVersion("2.0.0"); err != nil {
		t.Fatal(err)
	}
	previous, _ := PreviousPath()
	if err := writePointer(previous, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	active, _ := ActivePath()
	activeBefore, _ := os.ReadFile(active)
	previousBefore, _ := os.ReadFile(previous)
	if err := AutomaticRollback("2.0.0"); err == nil {
		t.Fatal("expected unhealthy fallback rejection")
	}
	activeAfter, _ := os.ReadFile(active)
	previousAfter, _ := os.ReadFile(previous)
	if string(activeBefore) != string(activeAfter) || string(previousBefore) != string(previousAfter) {
		t.Fatal("pointers changed after fallback health check failed")
	}
	healthCheckPayload = func(string) error { return nil }
	if err := AutomaticRollback("2.0.0"); err != nil {
		t.Fatal(err)
	}
	if ActiveVersion() != "1.0.0" || PreviousVersion() != "2.0.0" {
		t.Fatalf("bad rollback pointers active=%q previous=%q", ActiveVersion(), PreviousVersion())
	}
	if rejectedVersion() != "2.0.0" {
		t.Fatalf("failed release was not recorded: %q", rejectedVersion())
	}
}

func makeZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, e := zw.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write([]byte(body)); e != nil {
			t.Fatal(e)
		}
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractApp(t *testing.T) {
	dest := t.TempDir()
	archive := makeZip(t, map[string]string{"modlock-app.exe": "binary"})
	if err := extractApp(archive, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "modlock-app.exe"))
	if err != nil || string(got) != "binary" {
		t.Fatalf("extracted payload %q, %v", got, err)
	}
}

func TestExtractRejectsUnsafeAndUnexpectedFiles(t *testing.T) {
	for _, entries := range []map[string]string{{"../escape": "x", "modlock-app.exe": "binary"}, {"other.txt": "x", "modlock-app.exe": "binary"}} {
		if err := extractApp(makeZip(t, entries), t.TempDir()); err == nil {
			t.Fatalf("extractApp(%v) unexpectedly succeeded", entries)
		}
	}
}

func TestBootstrapAndRollbackPointers(t *testing.T) {
	oldCheck := healthCheckPayload
	healthCheckPayload = func(version string) error { return nil }
	t.Cleanup(func() { healthCheckPayload = oldCheck })
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	bootstrap := filepath.Join(root, "modlock-app.exe")
	if err := os.WriteFile(bootstrap, []byte("v1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := InitializeBootstrap(bootstrap, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if got := ActiveVersion(); got != "1.0.0" {
		t.Fatalf("active version = %q", got)
	}
	if err := writePointer(mustPreviousPath(t), "0.9.0"); err != nil {
		t.Fatal(err)
	}
	previousPayload, err := AppPath("0.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(previousPayload), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(previousPayload, []byte("test"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := ActiveVersion(); got != "0.9.0" {
		t.Fatalf("rollback active version = %q", got)
	}
	if got := PreviousVersion(); got != "1.0.0" {
		t.Fatalf("rollback previous version = %q", got)
	}
}

func mustPreviousPath(t *testing.T) string {
	t.Helper()
	p, err := PreviousPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
