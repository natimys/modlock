package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"modlock/internal/ignore"
	"modlock/internal/lockfile"
	"modlock/internal/providers"
)

func TestPrepareHashesAddedJarsForEverySourceAndKeepsExistingHashes(t *testing.T) {
	content := []byte("new jar content")
	digest := sha256.Sum256(content)
	wantHash := hex.EncodeToString(digest[:])
	tests := []struct {
		name    string
		source  string
		handler http.HandlerFunc
		key     string
	}{
		{
			name:   "Modrinth",
			source: "modrinth",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/v2/version_file/") {
					_, _ = w.Write([]byte(`{"id":"version-1","project_id":"project-1","version_number":"1.0","files":[{"url":"https://download.test/new.jar","filename":"new.jar"}]}`))
					return
				}
				http.NotFound(w, r)
			},
		},
		{
			name:   "CurseForge",
			source: "curseforge",
			key:    "test-key",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/fingerprints/432" {
					_, _ = w.Write([]byte(`{"data":{"exactMatches":[{"file":{"id":22,"gameId":432,"modId":11,"downloadUrl":"https://download.test/new.jar"}}]}}`))
					return
				}
				http.NotFound(w, r)
			},
		},
		{
			name:    "repository fallback",
			source:  "repo",
			handler: func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			root := t.TempDir()
			mods := filepath.Join(root, "mods")
			if err := os.MkdirAll(mods, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mods, "new.jar"), content, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mods, "old.jar"), []byte("unchanged old jar"), 0644); err != nil {
				t.Fatal(err)
			}
			old := &lockfile.File{
				Schema: 1,
				Pack:   lockfile.Pack{Repository: "https://example.test/pack.git", ModsDir: "mods"},
				Mods:   []lockfile.ModEntry{{ID: "old:mod", Filename: "old.jar", Source: "repo", Path: "files/mods/old.jar", SHA256: "preserve-this-hash"}},
			}
			detector := &providers.Detector{
				Client:         &http.Client{Timeout: time.Second},
				ModrinthBase:   server.URL,
				CurseForgeBase: server.URL,
				CurseForgeKey:  tc.key,
			}
			next, _, _, _, err := prepare(context.Background(), root, old, &ignore.File{}, detector, func(int, int, string, string) {})
			if err != nil {
				t.Fatal(err)
			}
			var added *lockfile.ModEntry
			for i := range next.Mods {
				if next.Mods[i].Filename == "new.jar" {
					added = &next.Mods[i]
				}
			}
			if added == nil || added.Source != tc.source || added.SHA256 != wantHash {
				t.Fatalf("added entry = %#v, want source %q and SHA-256 %s", added, tc.source, wantHash)
			}
			var existing *lockfile.ModEntry
			for i := range next.Mods {
				if next.Mods[i].Filename == "old.jar" {
					existing = &next.Mods[i]
				}
			}
			if existing == nil || existing.SHA256 != "preserve-this-hash" {
				t.Fatalf("existing entry/hash changed: %#v", existing)
			}
		})
	}
}

func TestInitRepository(t *testing.T) {
	root := t.TempDir()
	url := "https://example.test/pack.git"
	if err := InitRepository(root, url, "main"); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Reference(plumbing.HEAD, false)
	if err != nil {
		t.Fatal(err)
	}
	if head.Target() != plumbing.NewBranchReferenceName("main") {
		t.Fatalf("HEAD points to %s", head.Target())
	}
	remote, err := r.Remote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if got := remote.Config().URLs; len(got) != 1 || got[0] != url {
		t.Fatalf("origin=%v", got)
	}
	if _, err := filepath.Abs(root); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureOriginCreatesMissingRemote(t *testing.T) {
	root := t.TempDir()
	if _, err := gogit.PlainInit(root, false); err != nil {
		t.Fatal(err)
	}
	url := "https://example.test/pack.git"
	if err := EnsureOrigin(root, url); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := r.Remote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if got := remote.Config().URLs; len(got) != 1 || got[0] != url {
		t.Fatalf("origin=%v", got)
	}
}

func TestEnsureOriginRejectsDifferentRemote(t *testing.T) {
	root := t.TempDir()
	if _, err := gogit.PlainInit(root, false); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://example.test/other.git"}}); err != nil {
		t.Fatal(err)
	}
	err = EnsureOrigin(root, "https://example.test/pack.git")
	if err == nil || !strings.Contains(err.Error(), "fix origin before pushing") {
		t.Fatalf("expected actionable origin error, got %v", err)
	}
}

func TestRevertToCreatesRollbackCommit(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := gogit.PlainInit(remoteDir, true); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remoteDir}}); err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	file := filepath.Join(root, "mod.lock")
	commit := func(content, msg string) plumbing.Hash {
		if err := os.WriteFile(file, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Add("mod.lock"); err != nil {
			t.Fatal(err)
		}
		h, err := w.Commit(msg, &gogit.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	first := commit("one", "first")
	_ = commit("two", "second")
	if err = RevertTo(root, first.String(), "master"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "one" {
		t.Fatalf("content=%q err=%v", b, err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Message, "modlock: revert to ") {
		t.Fatalf("message=%q", c.Message)
	}
}
