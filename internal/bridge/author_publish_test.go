package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modlock/internal/authorconfig"
	"modlock/internal/failure"
	"modlock/internal/lockfile"
	syncer "modlock/internal/sync"
)

// This integration test uses Git for Windows' local bare-remote transport.
// Keep it opt-in because restricted Windows sandboxes may deny its shell child.
func TestAuthorPublishBareOriginIntegration(t *testing.T) {
	if os.Getenv("MODLOCK_RUN_GIT_INTEGRATION") != "1" {
		t.Skip("set MODLOCK_RUN_GIT_INTEGRATION=1 to exercise a local bare Git remote")
	}
	root := filepath.Join(t.TempDir(), "workspace")
	remote := filepath.Join(t.TempDir(), "remote.git")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	gitIntegration(t, root, "init", "--bare", "-b", "main", remote)
	gitIntegration(t, root, "init", "-b", "main")
	gitIntegration(t, root, "config", "user.name", "test")
	gitIntegration(t, root, "config", "user.email", "test@example.test")
	gitIntegration(t, root, "remote", "add", "origin", remote)
	if err := os.MkdirAll(filepath.Join(root, ".modlock"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "kubejs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kubejs", "startup.js"), []byte("startup\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pack := lockfile.Pack{Repository: remote, Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 3, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	if err := authorconfig.Write(root, &authorconfig.Config{TrackedPaths: []authorconfig.TrackedPath{{Path: "kubejs", Targets: []string{"client"}, Policy: "replace"}}}); err != nil {
		t.Fatal(err)
	}
	gitIntegration(t, root, "add", "--all", "--", "mod.lock", ".modlock/author.toml")
	gitIntegration(t, root, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "initial")
	gitIntegration(t, root, "push", "-u", "origin", "main")
	roots := syncer.TargetRoots{"client": root}
	preview, err := buildAuthorScan(root, roots)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publishAuthor(context.Background(), root, roots, preview.PreviewID, "publish files")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Pushed || result.Commit == "" {
		t.Fatalf("publish result %#v", result)
	}
	if got := gitIntegration(t, root, "--git-dir", remote, "rev-parse", "refs/heads/main"); got != result.Commit {
		t.Fatalf("remote head %s != %s", got, result.Commit)
	}
}

func gitIntegration(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func TestAuthorPublishUsesExactPreviewAndPushesWithoutForce(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "kubejs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kubejs", "startup.js"), []byte("startup\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pack := lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 3, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	if err := authorconfig.Write(root, &authorconfig.Config{TrackedPaths: []authorconfig.TrackedPath{{Path: "kubejs", Targets: []string{"client"}, Policy: "replace"}}}); err != nil {
		t.Fatal(err)
	}
	roots := syncer.TargetRoots{"client": root}
	preview, err := buildAuthorScan(root, roots)
	if err != nil {
		t.Fatal(err)
	}
	oldRunner := runAuthorGit
	defer func() { runAuthorGit = oldRunner }()
	var calls [][]string
	runAuthorGit = func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch {
		case len(args) == 2 && args[0] == "branch":
			return "main\n", nil
		case len(args) == 3 && args[0] == "remote":
			return pack.Repository + "\n", nil
		case len(args) > 0 && args[0] == "rev-parse":
			return "0123456789abcdef\n", nil
		default:
			return "", nil
		}
	}
	result, err := publishAuthor(context.Background(), root, roots, preview.PreviewID, "publish files")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Pushed || result.Commit != "0123456789abcdef" || result.Branch != "main" {
		t.Fatalf("result: %#v", result)
	}
	if len(calls) < 5 {
		t.Fatalf("expected branch/origin/index/stage/commit/revision/push calls, got %#v", calls)
	}
	for _, args := range calls {
		for _, arg := range args {
			if arg == "--force" || arg == "-f" {
				t.Fatalf("force push is forbidden: %#v", args)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "files", "client", "kubejs", "startup.js")); err != nil || string(b) != "startup\n" {
		t.Fatalf("published payload=%q err=%v", b, err)
	}
}

func TestAuthorPublishKeepsLocalCommitWhenPushFails(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "kubejs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kubejs", "startup.js"), []byte("startup\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pack := lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), &lockfile.File{Schema: 3, Pack: pack}); err != nil {
		t.Fatal(err)
	}
	if err := authorconfig.Write(root, &authorconfig.Config{TrackedPaths: []authorconfig.TrackedPath{{Path: "kubejs", Targets: []string{"client"}, Policy: "replace"}}}); err != nil {
		t.Fatal(err)
	}
	roots := syncer.TargetRoots{"client": root}
	preview, err := buildAuthorScan(root, roots)
	if err != nil {
		t.Fatal(err)
	}
	oldRunner := runAuthorGit
	defer func() { runAuthorGit = oldRunner }()
	runAuthorGit = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "branch" {
			return "main\n", nil
		}
		if len(args) > 0 && args[0] == "remote" {
			return pack.Repository + "\n", nil
		}
		if len(args) > 0 && args[0] == "rev-parse" {
			return "deadbeef\n", nil
		}
		if len(args) > 0 && args[0] == "push" {
			return "", os.ErrPermission
		}
		return "", nil
	}
	result, err := publishAuthor(context.Background(), root, roots, preview.PreviewID, "")
	if err == nil || result == nil || result.Pushed || result.Commit != "deadbeef" {
		t.Fatalf("expected committed-but-unpushed outcome: result=%#v err=%v", result, err)
	}
	if failure.Code(err) != "push_failed" {
		t.Fatalf("push failure code: %q", failure.Code(err))
	}
}
