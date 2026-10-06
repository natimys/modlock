package push

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

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
