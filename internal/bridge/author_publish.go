package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"modlock/internal/atomicfile"
	"modlock/internal/failure"
	"modlock/internal/lockfile"
	syncer "modlock/internal/sync"
)

type AuthorPublishResult struct {
	PreviewID string `json:"preview_id"`
	Commit    string `json:"commit"`
	Pushed    bool   `json:"pushed"`
	Branch    string `json:"branch"`
}

type savedFile struct {
	path   string
	data   []byte
	exists bool
}

func publishAuthor(ctx context.Context, root string, roots syncer.TargetRoots, previewID, message string) (*AuthorPublishResult, error) {
	if strings.TrimSpace(previewID) == "" {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("preview_id is required"))
	}
	preview, err := buildAuthorScan(root, roots)
	if err != nil {
		return nil, err
	}
	if preview.PreviewID != previewID {
		return nil, failure.WrapDetails(failure.Conflict, fmt.Errorf("publish preview is stale; scan and preview again"), map[string]any{"kind": "changed_during_apply", "expected_preview_id": previewID, "current_preview_id": preview.PreviewID})
	}
	if err := preview.PlannedLock.Validate(); err != nil {
		return nil, failure.Wrap(failure.InvalidRequest, err)
	}
	if err := requirePublishGitState(ctx, root, preview.Branch, preview.PlannedLock.Pack.Repository); err != nil {
		return nil, err
	}
	lockPath, _, err := authorLock(root)
	if err != nil {
		return nil, err
	}
	lockRel, err := filepath.Rel(root, lockPath)
	if err != nil {
		return nil, err
	}
	lockRel = filepath.ToSlash(lockRel)
	if err := lockfile.ValidateRelative(lockRel); err != nil {
		return nil, failure.Wrap(failure.InvalidRequest, err)
	}

	oldByPath := map[string]lockfile.ManagedFile{}
	newByPath := map[string]lockfile.ManagedFile{}
	for _, f := range preview.State.Lock.Files {
		oldByPath[f.Path] = f
	}
	for _, f := range preview.PlannedLock.Files {
		newByPath[f.Path] = f
	}
	paths := map[string]bool{lockRel: true}
	for rel, content := range preview.payloads {
		if err := lockfile.ValidateRelative(rel); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		if strings.HasPrefix(strings.ToLower(filepath.ToSlash(rel)), ".modlock/") || strings.EqualFold(rel, ".modlock") {
			return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("reserved publish path %q", rel))
		}
		dst, err := lockfile.ResolveWithin(root, rel)
		if err != nil {
			return nil, failure.Wrap(failure.Conflict, err)
		}
		if old, ok := oldByPath[rel]; ok {
			if err := verifyRepoSourceBeforeReplace(dst, old.SHA256); err != nil {
				return nil, err
			}
		} else if _, err := os.Lstat(dst); err == nil {
			return nil, failure.WrapDetails(failure.Conflict, fmt.Errorf("repository source already exists for new managed file %q", rel), map[string]any{"kind": "existing_unmanaged", "path": rel})
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		_ = content
		paths[rel] = true
	}
	for rel, old := range oldByPath {
		if _, ok := newByPath[rel]; ok {
			continue
		}
		dst, err := lockfile.ResolveWithin(root, rel)
		if err != nil {
			return nil, failure.Wrap(failure.Conflict, err)
		}
		if err := verifyRepoSourceBeforeReplace(dst, old.SHA256); err != nil {
			return nil, err
		}
		paths[rel] = true
	}

	var backups []savedFile
	rollback := func() {
		for i := len(backups) - 1; i >= 0; i-- {
			b := backups[i]
			if b.exists {
				_ = atomicfile.Write(b.path, b.data, 0644)
			} else {
				_ = os.Remove(b.path)
			}
		}
	}
	write := func(path string, data []byte) error {
		old, err := os.ReadFile(path)
		if err == nil {
			backups = append(backups, savedFile{path: path, data: old, exists: true})
		} else if os.IsNotExist(err) {
			backups = append(backups, savedFile{path: path})
		} else {
			return err
		}
		return atomicfile.Write(path, data, 0644)
	}
	for rel, data := range preview.payloads {
		dst, err := lockfile.ResolveWithin(root, rel)
		if err != nil {
			rollback()
			return nil, failure.Wrap(failure.Conflict, err)
		}
		if err = write(dst, data); err != nil {
			rollback()
			return nil, err
		}
	}
	for rel := range oldByPath {
		if _, ok := newByPath[rel]; ok {
			continue
		}
		dst, err := lockfile.ResolveWithin(root, rel)
		if err != nil {
			rollback()
			return nil, err
		}
		b, err := os.ReadFile(dst)
		if err != nil {
			rollback()
			return nil, err
		}
		backups = append(backups, savedFile{path: dst, data: b, exists: true})
		if err := os.Remove(dst); err != nil {
			rollback()
			return nil, err
		}
	}
	var lockBuffer bytes.Buffer
	if err := toml.NewEncoder(&lockBuffer).Encode(preview.PlannedLock); err != nil {
		rollback()
		return nil, err
	}
	lockBytes := lockBuffer.Bytes()
	if err := write(lockPath, lockBytes); err != nil {
		rollback()
		return nil, err
	}

	stageArgs := []string{"add", "--all", "--"}
	for rel := range paths {
		stageArgs = append(stageArgs, rel)
	}
	if _, err := runAuthorGit(ctx, root, stageArgs...); err != nil {
		rollback()
		unstage(ctx, root, stageArgs[3:])
		return nil, failure.Wrap(failure.Git, err)
	}
	if message == "" {
		message = preview.CommitMessage
	}
	if _, err := runAuthorGit(ctx, root, "-c", "user.name=ModLock", "-c", "user.email=modlock@local", "commit", "-m", message); err != nil {
		rollback()
		unstage(ctx, root, stageArgs[3:])
		return nil, failure.Wrap(failure.Git, err)
	}
	commit, err := runAuthorGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return nil, failure.Wrap(failure.Git, err)
	}
	if _, err := runAuthorGit(ctx, root, "push", "-u", "origin", preview.Branch); err != nil {
		return &AuthorPublishResult{PreviewID: previewID, Commit: strings.TrimSpace(commit), Pushed: false, Branch: preview.Branch}, failure.WrapDetails(failure.PushFailed, fmt.Errorf("commit created locally but push failed: %w", err), map[string]any{"commit": strings.TrimSpace(commit), "pushed": false, "branch": preview.Branch})
	}
	return &AuthorPublishResult{PreviewID: previewID, Commit: strings.TrimSpace(commit), Pushed: true, Branch: preview.Branch}, nil
}

func verifyRepoSourceBeforeReplace(path, expected string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return failure.WrapDetails(failure.Conflict, fmt.Errorf("managed repository source is missing: %s", path), map[string]any{"kind": "locally_modified", "path": path})
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return failure.WrapDetails(failure.Conflict, fmt.Errorf("managed repository source is not a regular file: %s", path), map[string]any{"kind": "target_not_regular", "path": path})
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	actual := hex.EncodeToString(h[:])
	if expected != "" && !strings.EqualFold(actual, expected) {
		return failure.WrapDetails(failure.Conflict, fmt.Errorf("managed repository source was modified locally: %s", path), map[string]any{"kind": "locally_modified", "path": path, "sha256": actual})
	}
	return nil
}

func requirePublishGitState(ctx context.Context, root, branch, repository string) error {
	actualBranch, err := runAuthorGit(ctx, root, "branch", "--show-current")
	if err != nil {
		return failure.Wrap(failure.Git, fmt.Errorf("author workspace must be a Git repository on branch %q: %w", branch, err))
	}
	if strings.TrimSpace(actualBranch) != branch {
		return failure.WrapDetails(failure.Conflict, fmt.Errorf("current branch %q does not match pack branch %q", strings.TrimSpace(actualBranch), branch), map[string]any{"kind": "changed_during_apply", "branch": strings.TrimSpace(actualBranch)})
	}
	if _, err = runAuthorGit(ctx, root, "diff", "--cached", "--quiet"); err != nil {
		return failure.Wrap(failure.Conflict, fmt.Errorf("staged Git changes exist; commit or unstage them before publishing"))
	}
	remote, err := runAuthorGit(ctx, root, "remote", "get-url", "origin")
	if err != nil {
		return failure.Wrap(failure.Git, fmt.Errorf("Git origin is not configured"))
	}
	if strings.TrimSpace(remote) != repository {
		return failure.WrapDetails(failure.Conflict, fmt.Errorf("Git origin does not match pack.repository"), map[string]any{"kind": "changed_during_apply", "origin": strings.TrimSpace(remote)})
	}
	return nil
}

func unstage(ctx context.Context, root string, paths []string) {
	if len(paths) == 0 {
		return
	}
	args := append([]string{"reset", "--"}, paths...)
	_, _ = runAuthorGit(ctx, root, args...)
}

func runGit(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

var runAuthorGit = runGit
