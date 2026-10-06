package providers

import (
	"archive/zip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyRepoFile(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "a.jar")
	dst := filepath.Join(d, "files", "mods", "a.jar")
	if e := os.WriteFile(src, []byte("jar"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := Copy(src, dst); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(dst)
	if e != nil || string(b) != "jar" {
		t.Fatalf("copy failed: %q %v", b, e)
	}
}

func TestDetectCurseForgeAndResolveDownloadURL(t *testing.T) {
	var fingerprintCalled, downloadCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/version_file/"):
			http.NotFound(w, r)
		case r.URL.Path == "/v1/fingerprints/432":
			fingerprintCalled = true
			if r.Header.Get("x-api-key") != "key" {
				t.Error("missing API key")
			}
			fmt.Fprint(w, `{"data":{"exactMatches":[{"file":{"id":456,"gameId":432,"modId":123,"downloadUrl":""}}]}}`)
		case r.URL.Path == "/v1/mods/123/files/456/download-url":
			downloadCalled = true
			fmt.Fprint(w, `{"data":"https://example.test/mod.jar"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := filepath.Join(t.TempDir(), "mod.jar")
	if err := os.WriteFile(p, []byte("jar"), 0644); err != nil {
		t.Fatal(err)
	}
	d := &Detector{Client: server.Client(), CurseForgeKey: "key", ModrinthBase: server.URL, CurseForgeBase: server.URL}
	m, err := d.Detect(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !fingerprintCalled || !downloadCalled {
		t.Fatalf("expected both CurseForge requests")
	}
	if m.Source != "curseforge" || m.ModID != 123 || m.FileID != 456 || m.URL == "" {
		t.Fatalf("unexpected mod: %#v", m)
	}
}

func TestDetectUnknownHasNoImplicitRepoFallback(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := filepath.Join(t.TempDir(), "custom.jar")
	if err := os.WriteFile(p, []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	d := &Detector{Client: server.Client(), ModrinthBase: server.URL, CurseForgeBase: server.URL}
	m, err := d.Detect(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "" {
		t.Fatalf("unknown file was prematurely assigned source %q", m.Source)
	}
}

func TestCurseForgeStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/games/432" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "valid" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":{"id":432,"name":"Minecraft"}}`)
	}))
	defer server.Close()
	d := &Detector{Client: server.Client(), CurseForgeKey: "valid", CurseForgeBase: server.URL}
	status := d.CheckCurseForge(context.Background())
	if !status.KeyFound || !status.Available {
		t.Fatalf("unexpected status: %#v", status)
	}
	d.CurseForgeKey = "invalid"
	status = d.CheckCurseForge(context.Background())
	if status.Available || !status.KeyFound || !strings.Contains(status.Message, "401") {
		t.Fatalf("unexpected invalid-key status: %#v", status)
	}
}

func TestDownloadWithRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "jar-data")
	}))
	defer server.Close()
	dst := filepath.Join(t.TempDir(), "mod.jar")
	if err := DownloadWithRetry(context.Background(), server.Client(), server.URL, dst, 4, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("got %d attempts, want 3", calls)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "jar-data" {
		t.Fatalf("downloaded %q, %v", b, err)
	}
	if _, err = os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatal("partial file was left behind")
	}
}

func TestDownloadDoesNotRetryPermanentHTTPError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.NotFound(w, r) }))
	defer server.Close()
	err := DownloadWithRetry(context.Background(), server.Client(), server.URL, filepath.Join(t.TempDir(), "x.jar"), 4, nil)
	if err == nil || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestDetectsStableIdentityFromFabricJar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "example-2.0.jar")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	entry, err := zw.Create("fabric.mod.json")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(entry, `{"id":"example_mod","version":"2.0"}`)
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	d := &Detector{Client: server.Client(), ModrinthBase: server.URL, CurseForgeBase: server.URL}
	m, err := d.Detect(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "minecraft:example_mod" || m.Version != "2.0" || m.Source != "" {
		t.Fatalf("unexpected metadata: %#v", m)
	}
}
