package sync

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"modlock/internal/diff"
	"modlock/internal/failure"
	"modlock/internal/lockfile"
)

// Snapshot owns a checkout at exactly one commit. Close must be called after use.
type Snapshot struct {
	Revision string
	Lock     *lockfile.File
	LockPath string
	RepoDir  string
	tempDir  string
}

func (s *Snapshot) Close() error { return os.RemoveAll(s.tempDir) }

func Fetch(ctx context.Context, source lockfile.Pack, revision string, progress Progress) (*Snapshot, error) {
	config := &lockfile.File{Schema: 1, Pack: source}
	config.Defaults()
	if err := config.Validate(); err != nil {
		return nil, failure.Wrap(failure.InvalidRequest, err)
	}
	source = config.Pack
	if revision != "" {
		if _, err := hex.DecodeString(revision); err != nil || len(revision) != 40 {
			return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("revision must be a full 40-character Git commit hash"))
		}
	}
	temp, err := os.MkdirTemp("", "modlock-snapshot-")
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{tempDir: temp, RepoDir: filepath.Join(temp, "repo")}
	complete := false
	defer func() {
		if !complete {
			_ = snapshot.Close()
		}
	}()
	if progress != nil {
		progress("Получение ревизии сборки...")
	}
	depth := 1
	if revision != "" {
		// A branch can advance after preview. Fetch its history and check out
		// the previewed object, never substitute the current branch tip.
		depth = 0
	}
	repo, err := git.PlainCloneContext(ctx, snapshot.RepoDir, false, &git.CloneOptions{
		URL: source.Repository, ReferenceName: plumbing.NewBranchReferenceName(source.Branch), SingleBranch: true, Depth: depth,
	})
	if err != nil {
		code := failure.Network
		if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) || errors.Is(err, transport.ErrRepositoryNotFound) || errors.Is(err, plumbing.ErrReferenceNotFound) {
			code = failure.Git
		}
		return nil, failure.Wrap(code, fmt.Errorf("clone repository: %w", err))
	}
	if revision != "" {
		worktree, err := repo.Worktree()
		if err != nil {
			return nil, err
		}
		if err = worktree.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(revision)}); err != nil {
			return nil, failure.Wrap(failure.Git, fmt.Errorf("checkout previewed commit: %w", err))
		}
	}
	head, err := repo.Head()
	if err != nil {
		return nil, err
	}
	snapshot.Revision = head.Hash().String()
	if revision != "" && head.Hash() != plumbing.NewHash(revision) {
		return nil, fmt.Errorf("checkout does not match previewed commit")
	}
	snapshot.LockPath, err = lockfile.ResolveWithin(snapshot.RepoDir, source.LockPath)
	if err != nil {
		return nil, err
	}
	snapshot.Lock, err = lockfile.Read(snapshot.LockPath)
	if err != nil {
		return nil, fmt.Errorf("remote lock: %w", err)
	}
	complete = true
	return snapshot, nil
}

type Preview struct {
	Revision string         `json:"revision"`
	Lock     *lockfile.File `json:"lock"`
	Diff     diff.Result    `json:"diff"`
}

// Check does not install anything. Its commit can be supplied to RunRevision.
func Check(ctx context.Context, root string, progress Progress) (Preview, error) {
	release, err := acquireSyncLock(root)
	if err != nil {
		return Preview{}, err
	}
	defer release()
	path, err := lockfile.FindPath(root)
	if err != nil {
		return Preview{}, err
	}
	old, err := lockfile.Read(path)
	if err != nil {
		return Preview{}, err
	}
	snapshot, err := Fetch(ctx, old.Pack, "", progress)
	if err != nil {
		return Preview{}, err
	}
	defer snapshot.Close()
	return Preview{Revision: snapshot.Revision, Lock: snapshot.Lock, Diff: diff.Locks(old, snapshot.Lock)}, nil
}
