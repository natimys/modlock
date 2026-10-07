package authorconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSeparateSettingsRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := &Config{IncludeDirs: []string{"config", "scripts"}, ExcludePaths: []string{"logs/**", "saves/**"}, IgnoredModIDs: []string{"modrinth:abc"}}
	if err := Write(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch: got %#v, want %#v", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, RelativePath)); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsRejectEscapingPaths(t *testing.T) {
	if err := (&Config{IncludeDirs: []string{"../outside"}}).Validate(); err == nil {
		t.Fatal("expected include path to be rejected")
	}
	if err := (&Config{ExcludePaths: []string{"../outside/**"}}).Validate(); err == nil {
		t.Fatal("expected exclusion path to be rejected")
	}
}

func TestTrackedPathsRoundTripAndValidation(t *testing.T) {
	root := t.TempDir()
	want := &Config{TrackedPaths: []TrackedPath{{Path: "kubejs", Targets: []string{"server", "client"}, Policy: "replace"}}}
	if err := Write(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tracked paths round trip mismatch: got %#v, want %#v", got, want)
	}
	for _, entry := range []TrackedPath{{Path: "../outside", Targets: []string{"client"}, Policy: "replace"}, {Path: ".modlock/staging", Targets: []string{"client"}, Policy: "replace"}, {Path: "config", Targets: []string{"client", "client"}, Policy: "replace"}, {Path: "config", Targets: []string{"server"}, Policy: "overwrite"}} {
		if err := (&Config{TrackedPaths: []TrackedPath{entry}}).Validate(); err == nil {
			t.Errorf("invalid tracked path accepted: %#v", entry)
		}
	}
}
