package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	Revision          string         `json:"revision"`
	Lock              *lockfile.File `json:"lock"`
	Diff              diff.Result    `json:"diff"`
	LocalInstallation LocalCheck     `json:"local_installation"`
}

// LocalCheck describes only files managed by the selected lock. Files without
// a digest are checked for presence, regular-file type, and non-empty contents,
// but cannot be claimed as content-verified.
type LocalCheck struct {
	State         string   `json:"state"`
	NeedsRecovery bool     `json:"needs_recovery"`
	Missing       []string `json:"missing"`
	Damaged       []string `json:"damaged"`
	Unverified    []string `json:"unverified,omitempty"`
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
	if err := requireSupportedInstallSchema(old, snapshot.Lock); err != nil {
		return Preview{}, err
	}
	local, err := VerifyLock(root, snapshot.Lock)
	if err != nil {
		return Preview{}, err
	}
	result := diff.Locks(old, snapshot.Lock)
	needs := make(map[string]bool)
	for _, name := range local.Missing {
		needs[name] = true
	}
	for _, name := range local.Damaged {
		needs[name] = true
	}
	for _, mod := range snapshot.Lock.Mods {
		if !needs[mod.Filename] {
			continue
		}
		found := false
		for _, entry := range result.Added {
			if entry == mod.Filename {
				found = true
				break
			}
		}
		for _, entry := range result.Updated {
			if entry.New.Filename == mod.Filename {
				found = true
				break
			}
		}
		if !found {
			result.Added = append(result.Added, mod.Filename)
		}
	}
	sort.Strings(result.Added)
	result.Unchanged = len(snapshot.Lock.Mods) - len(result.Added) - len(result.Updated)
	return Preview{Revision: snapshot.Revision, Lock: snapshot.Lock, Diff: result, LocalInstallation: local}, nil
}

// VerifyLock checks local managed mod files without contacting the network.
func VerifyLock(root string, lock *lockfile.File) (LocalCheck, error) {
	modsDir, err := lockfile.ResolveWithin(root, lock.Pack.ModsDir)
	if err != nil {
		return LocalCheck{}, err
	}
	result := LocalCheck{State: "healthy", Missing: []string{}, Damaged: []string{}, Unverified: []string{}}
	for _, mod := range lock.Mods {
		path, err := lockfile.ResolveWithin(modsDir, mod.Filename)
		if err != nil {
			return LocalCheck{}, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			result.Missing = append(result.Missing, mod.Filename)
			continue
		}
		if err != nil {
			return LocalCheck{}, fmt.Errorf("check installed mod %s: %w", mod.Filename, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			result.Damaged = append(result.Damaged, mod.Filename)
			continue
		}
		if mod.SHA256 == "" {
			result.Unverified = append(result.Unverified, mod.Filename)
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return LocalCheck{}, fmt.Errorf("read installed mod %s: %w", mod.Filename, err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return LocalCheck{}, fmt.Errorf("hash installed mod %s: %w", mod.Filename, copyErr)
		}
		if closeErr != nil {
			return LocalCheck{}, closeErr
		}
		if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), mod.SHA256) {
			result.Damaged = append(result.Damaged, mod.Filename)
		}
	}
	sort.Strings(result.Missing)
	sort.Strings(result.Damaged)
	sort.Strings(result.Unverified)
	result.NeedsRecovery = len(result.Missing)+len(result.Damaged) > 0
	if result.NeedsRecovery {
		result.State = "incomplete"
	} else if len(result.Unverified) > 0 {
		result.State = "unverified"
	}
	return result, nil
}

// Verify checks the installed lock and managed files without network access.
// It shares the sync lock with Check and RunRevision.
func Verify(root string) (LocalCheck, error) {
	release, err := acquireSyncLock(root)
	if err != nil {
		return LocalCheck{}, err
	}
	defer release()
	path, err := lockfile.FindPath(root)
	if err != nil {
		return LocalCheck{}, err
	}
	lock, err := lockfile.Read(path)
	if err != nil {
		return LocalCheck{}, err
	}
	if err := requireSupportedInstallSchema(lock, nil); err != nil {
		return LocalCheck{}, err
	}
	return VerifyLock(root, lock)
}
