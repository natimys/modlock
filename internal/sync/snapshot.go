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
	Revision          string          `json:"revision"`
	Lock              *lockfile.File  `json:"lock"`
	Diff              diff.Result     `json:"diff"`
	LocalInstallation LocalCheck      `json:"local_installation"`
	ManagedFiles      []ManagedChange `json:"managed_files"`
	Conflicts         []FileConflict  `json:"conflicts"`
}

type ManagedChange struct {
	Target   string `json:"target"`
	TargetID string `json:"target_id,omitempty"`
	Action   string `json:"action"`
	Policy   string `json:"policy"`
}

type FileConflict struct {
	Target   string `json:"target"`
	TargetID string `json:"target_id,omitempty"`
	Kind     string `json:"kind"`
	SHA256   string `json:"sha256"`
	Message  string `json:"message"`
	Reason   string `json:"reason,omitempty"`
}

type ModCheck struct {
	Identity       string `json:"identity"`
	TargetID       string `json:"target_id,omitempty"`
	Filename       string `json:"filename"`
	State          string `json:"state"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	ActualSHA256   string `json:"actual_sha256,omitempty"`
}

// LocalCheck describes only files managed by the selected lock. Files without
// a digest are checked for presence, regular-file type, and non-empty contents,
// but cannot be claimed as content-verified.
type LocalCheck struct {
	State            string     `json:"state"`
	NeedsRecovery    bool       `json:"needs_recovery"`
	Missing          []string   `json:"missing"`
	Damaged          []string   `json:"damaged"`
	Unverified       []string   `json:"unverified,omitempty"`
	ManagedMissing   []string   `json:"managed_missing,omitempty"`
	ManagedChanged   []string   `json:"managed_changed,omitempty"`
	ManagedConflicts []string   `json:"managed_conflicts,omitempty"`
	ModStates        []ModCheck `json:"mod_states,omitempty"`
}

// Check does not install anything. Its commit can be supplied to RunRevision.
func Check(ctx context.Context, root string, progress Progress) (Preview, error) {
	return CheckWithRoots(ctx, root, nil, progress)
}

func CheckWithRoots(ctx context.Context, root string, roots TargetRoots, progress Progress) (Preview, error) {
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
	if err := ValidateTargetRoots(root, snapshot.Lock, roots); err != nil {
		return Preview{}, err
	}
	local, err := VerifyLockWithRoots(root, snapshot.Lock, roots)
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
	managed, conflicts, err := inspectManagedFilesWithRoots(root, old, snapshot.Lock, roots)
	if err != nil {
		return Preview{}, err
	}
	return Preview{Revision: snapshot.Revision, Lock: snapshot.Lock, Diff: result, LocalInstallation: local, ManagedFiles: managed, Conflicts: conflicts}, nil
}

// VerifyLock checks local managed mod files without contacting the network.
func VerifyLock(root string, lock *lockfile.File) (LocalCheck, error) {
	return VerifyLockWithRoots(root, lock, nil)
}

func VerifyLockWithRoots(root string, lock *lockfile.File, roots TargetRoots) (LocalCheck, error) {
	if err := ValidateTargetRoots(root, lock, roots); err != nil {
		return LocalCheck{}, err
	}
	result := LocalCheck{State: "healthy", Missing: []string{}, Damaged: []string{}, Unverified: []string{}, ManagedMissing: []string{}, ManagedChanged: []string{}, ManagedConflicts: []string{}}
	for _, mod := range lock.Mods {
		targets := mod.Targets
		if lock.Schema < 3 {
			targets = []string{""}
		}
		for _, targetID := range targets {
			modsDir, err := resolveTargetPath(root, lock.Schema, roots, targetID, lock.Pack.ModsDir)
			if err != nil {
				return LocalCheck{}, err
			}
			path, err := lockfile.ResolveWithin(modsDir, mod.Filename)
			if err != nil {
				return LocalCheck{}, err
			}
			state := ModCheck{Identity: mod.Identity(), TargetID: targetID, Filename: mod.Filename, State: "synced", ExpectedSHA256: mod.SHA256}
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				result.Missing = append(result.Missing, localModLabel(targetID, mod.Filename))
				state.State = "missing"
				result.ModStates = append(result.ModStates, state)
				continue
			}
			if err != nil {
				return LocalCheck{}, fmt.Errorf("check installed mod %s: %w", mod.Filename, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				result.Damaged = append(result.Damaged, localModLabel(targetID, mod.Filename))
				state.State = "damaged"
				result.ModStates = append(result.ModStates, state)
				continue
			}
			if mod.SHA256 == "" {
				result.Unverified = append(result.Unverified, localModLabel(targetID, mod.Filename))
				state.State = "unverified"
				result.ModStates = append(result.ModStates, state)
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
				result.Damaged = append(result.Damaged, localModLabel(targetID, mod.Filename))
				state.State = "damaged"
				state.ActualSHA256 = hex.EncodeToString(hash.Sum(nil))
			}
			result.ModStates = append(result.ModStates, state)
		}
	}
	for _, managed := range lock.Files {
		targets := managed.Targets
		if lock.Schema < 3 {
			targets = []string{""}
		}
		for _, targetID := range targets {
			path, err := resolveTargetPath(root, lock.Schema, roots, targetID, managed.Target)
			if err != nil {
				return LocalCheck{}, err
			}
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				result.ManagedMissing = append(result.ManagedMissing, localModLabel(targetID, managed.Target))
				continue
			}
			if err != nil {
				return LocalCheck{}, fmt.Errorf("check managed file %s: %w", managed.Target, err)
			}
			if !info.Mode().IsRegular() {
				result.ManagedConflicts = append(result.ManagedConflicts, localModLabel(targetID, managed.Target))
				continue
			}
			if managed.Policy == "replace" {
				digest, err := hashFile(path)
				if err != nil {
					return LocalCheck{}, err
				}
				if !strings.EqualFold(digest, managed.SHA256) {
					result.ManagedChanged = append(result.ManagedChanged, localModLabel(targetID, managed.Target))
				}
			}
		}
	}
	sort.Strings(result.Missing)
	sort.Strings(result.Damaged)
	sort.Strings(result.Unverified)
	sort.Strings(result.ManagedMissing)
	sort.Strings(result.ManagedChanged)
	sort.Strings(result.ManagedConflicts)
	result.NeedsRecovery = len(result.Missing)+len(result.Damaged)+len(result.ManagedMissing)+len(result.ManagedConflicts) > 0
	if result.NeedsRecovery {
		result.State = "incomplete"
	} else if len(result.Unverified) > 0 {
		result.State = "unverified"
	}
	return result, nil
}

func localModLabel(targetID, path string) string {
	if targetID == "" {
		return path
	}
	return targetID + ":" + filepath.ToSlash(path)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func inspectManagedFiles(root string, old, next *lockfile.File) ([]ManagedChange, []FileConflict, error) {
	return inspectManagedFilesWithRoots(root, old, next, nil)
}

func inspectManagedFilesWithRoots(root string, old, next *lockfile.File, roots TargetRoots) ([]ManagedChange, []FileConflict, error) {
	type targetFile struct {
		targetID string
		file     lockfile.ManagedFile
	}
	entries := func(lock *lockfile.File) map[string]targetFile {
		out := map[string]targetFile{}
		if lock == nil {
			return out
		}
		for _, file := range lock.Files {
			targetIDs := file.Targets
			if lock.Schema < 3 {
				targetIDs = []string{""}
			}
			for _, id := range targetIDs {
				key := strings.ToLower(id + "\x00" + filepath.ToSlash(file.Target))
				out[key] = targetFile{targetID: id, file: file}
			}
		}
		return out
	}
	oldFiles := map[string]targetFile{}
	if old != nil {
		oldFiles = entries(old)
	}
	nextFiles := entries(next)
	keys := map[string]bool{}
	for k := range oldFiles {
		keys[k] = true
	}
	for k := range nextFiles {
		keys[k] = true
	}
	var changes []ManagedChange
	var conflicts []FileConflict
	for key := range keys {
		prevEntry, hadPrev := oldFiles[key]
		itemEntry, hasNext := nextFiles[key]
		prev, item := prevEntry.file, itemEntry.file
		targetID := itemEntry.targetID
		if !hasNext {
			targetID = prevEntry.targetID
		}
		target := item.Target
		if !hasNext {
			target = prev.Target
		}
		fileSchema := next.Schema
		if !hasNext && old != nil {
			fileSchema = old.Schema
		}
		path, err := resolveTargetPath(root, fileSchema, roots, targetID, target)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Lstat(path)
		exists := err == nil
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, err
		}
		curHash := ""
		if exists {
			if !info.Mode().IsRegular() {
				conflicts = append(conflicts, fileConflict(targetID, target, "target_not_regular", "target is not a regular file", ""))
				continue
			}
			curHash, err = hashFile(path)
			if err != nil {
				return nil, nil, err
			}
		}
		action := ""
		if !hasNext {
			if prev.Policy == "replace" && exists {
				if strings.EqualFold(curHash, prev.SHA256) {
					action = "remove"
				} else {
					conflicts = append(conflicts, fileConflict(targetID, target, "locally_modified", "locally modified managed file", curHash))
				}
			}
		} else if item.Policy == "if_missing" {
			if !exists {
				action = "add"
			}
		} else if !exists || !strings.EqualFold(curHash, item.SHA256) {
			if exists && (!hadPrev || prev.Policy == "if_missing" || (prev.Policy == "replace" && !strings.EqualFold(curHash, prev.SHA256))) {
				kind := "existing_unmanaged"
				if hadPrev {
					kind = "locally_modified"
				}
				conflicts = append(conflicts, fileConflict(targetID, target, kind, "existing file would be replaced", curHash))
			} else {
				action = "add"
				if hadPrev {
					action = "update"
				}
			}
		}
		if action != "" {
			changes = append(changes, ManagedChange{Target: target, TargetID: targetID, Action: action, Policy: func() string {
				if hasNext {
					return item.Policy
				}
				return prev.Policy
			}()})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		return changes[i].TargetID+changes[i].Target < changes[j].TargetID+changes[j].Target
	})
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].TargetID+conflicts[i].Target < conflicts[j].TargetID+conflicts[j].Target
	})
	return changes, conflicts, nil
}

func fileConflict(targetID, target, kind, message, hash string) FileConflict {
	return FileConflict{Target: target, TargetID: targetID, Kind: kind, SHA256: hash, Message: message, Reason: message}
}

// Verify checks the installed lock and managed files without network access.
// It shares the sync lock with Check and RunRevision.
func Verify(root string) (LocalCheck, error) {
	return VerifyWithRoots(root, nil)
}

func VerifyWithRoots(root string, roots TargetRoots) (LocalCheck, error) {
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
	if err := ValidateTargetRoots(root, lock, roots); err != nil {
		return LocalCheck{}, err
	}
	return VerifyLockWithRoots(root, lock, roots)
}
