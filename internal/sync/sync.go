package sync

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modlock/internal/atomicfile"
	"modlock/internal/diff"
	"modlock/internal/failure"
	"modlock/internal/lockfile"
	"modlock/internal/providers"
)

type Progress func(string)
type Result struct {
	Diff     diff.Result `json:"diff"`
	Revision string      `json:"revision"`
}

var commitLockFile = atomicfile.Commit
var renameInstalledFile = os.Rename
var restoreBackupFile = providers.Copy

func Run(ctx context.Context, root string, progress Progress) (Result, error) {
	return run(ctx, root, "", progress)
}

// RunRevision applies the commit returned by Check, even if the branch has moved.
func RunRevision(ctx context.Context, root, revision string, progress Progress) (Result, error) {
	if revision == "" {
		return Result{}, fmt.Errorf("apply requires the previewed Git commit")
	}
	return run(ctx, root, revision, progress)
}

func run(ctx context.Context, root, revision string, progress Progress) (Result, error) {
	if progress == nil {
		progress = func(string) {}
	}
	release, err := acquireSyncLock(root)
	if err != nil {
		return Result{}, err
	}
	defer release()

	oldPath, err := lockfile.FindPath(root)
	if err != nil {
		return Result{}, fmt.Errorf("find mod.lock: %w", err)
	}
	old, err := lockfile.Read(oldPath)
	if err != nil {
		return Result{}, err
	}
	if err := requireSupportedInstallSchema(old, nil); err != nil {
		return Result{}, err
	}
	snapshot, err := Fetch(ctx, old.Pack, revision, progress)
	if err != nil {
		return Result{}, err
	}
	defer snapshot.Close()
	repoDir, remoteLock, next := snapshot.RepoDir, snapshot.LockPath, snapshot.Lock
	if err := requireSupportedInstallSchema(old, next); err != nil {
		return Result{}, err
	}
	if filepath.Clean(filepath.FromSlash(next.Pack.ModsDir)) != filepath.Clean(filepath.FromSlash(old.Pack.ModsDir)) {
		return Result{}, fmt.Errorf("unsupported migration: remote pack.mods_dir changed from %q to %q", old.Pack.ModsDir, next.Pack.ModsDir)
	}
	newLockBytes, err := os.ReadFile(remoteLock)
	if err != nil {
		return Result{}, err
	}
	d := diff.Locks(old, next)
	modsDir, err := lockfile.ResolveWithin(root, old.Pack.ModsDir)
	if err != nil {
		return Result{}, err
	}

	install := make(map[string]bool, len(d.Added)+len(d.Updated))
	for _, name := range d.Added {
		install[name] = true
	}
	for _, update := range d.Updated {
		install[update.New.Filename] = true
	}
	for _, m := range next.Mods {
		if install[m.Filename] {
			continue
		}
		_, statErr := os.Stat(filepath.Join(modsDir, m.Filename))
		if os.IsNotExist(statErr) {
			d.Added = append(d.Added, m.Filename)
			install[m.Filename] = true
		} else if statErr != nil {
			return Result{}, fmt.Errorf("check installed mod %s: %w", m.Filename, statErr)
		}
	}
	sort.Strings(d.Added)
	d.Unchanged = len(next.Mods) - len(d.Added) - len(d.Updated)
	var installNames []string
	for name := range install {
		installNames = append(installNames, name)
	}
	sort.Strings(installNames)
	removeSet := map[string]bool{}
	for _, name := range d.Removed {
		removeSet[name] = true
	}
	for _, update := range d.Updated {
		if update.Old.Filename != update.New.Filename {
			removeSet[update.Old.Filename] = true
		}
	}
	desired := make(map[string]bool, len(next.Mods))
	for _, m := range next.Mods {
		desired[strings.ToLower(m.Filename)] = true
	}
	for _, m := range old.Mods {
		if !desired[strings.ToLower(m.Filename)] {
			removeSet[m.Filename] = true
		}
	}
	var removeNames []string
	for name := range removeSet {
		removeNames = append(removeNames, name)
	}
	sort.Strings(removeNames)
	for _, name := range append(append([]string{}, removeNames...), installNames...) {
		if _, err := lockfile.ResolveWithin(modsDir, name); err != nil {
			return Result{}, err
		}
	}

	// Find existing names case-insensitively so Windows and Linux agree about
	// collisions and filename changes. Unmanaged files are never overwritten.
	existing := map[string]string{}
	entries, readErr := os.ReadDir(modsDir)
	if readErr != nil && !os.IsNotExist(readErr) {
		return Result{}, readErr
	}
	managed := map[string]bool{}
	for _, m := range old.Mods {
		managed[strings.ToLower(m.Filename)] = true
	}
	for _, entry := range entries {
		existing[strings.ToLower(entry.Name())] = entry.Name()
	}
	for _, name := range installNames {
		if actual, ok := existing[strings.ToLower(name)]; ok && !managed[strings.ToLower(name)] {
			return Result{}, failure.Wrap(failure.Conflict, fmt.Errorf("cannot install %s: it conflicts with unmanaged file %s", name, actual))
		}
	}

	if err = os.MkdirAll(modsDir, 0755); err != nil {
		return Result{}, err
	}
	txnDir, err := os.MkdirTemp(modsDir, ".modlock-sync-")
	if err != nil {
		return Result{}, err
	}
	keepTxn := false
	defer func() {
		if !keepTxn {
			_ = os.RemoveAll(txnDir)
		}
	}()
	stage := filepath.Join(txnDir, "stage")
	if err = os.Mkdir(stage, 0755); err != nil {
		return Result{}, err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	byName := map[string]lockfile.ModEntry{}
	for _, m := range next.Mods {
		byName[m.Filename] = m
	}
	for _, name := range installNames {
		m := byName[name]
		dst := filepath.Join(stage, name)
		switch m.Source {
		case "repo":
			progress("Копирование " + name + "...")
			src, resolveErr := lockfile.ResolveWithin(repoDir, m.Path)
			if resolveErr != nil {
				return Result{}, resolveErr
			}
			if err = providers.Copy(src, dst); err != nil {
				return Result{}, fmt.Errorf("copy %s: %w", name, err)
			}
		default:
			if err = providers.DownloadWithRetry(ctx, client, m.URL, dst, 4, func(attempt, total int, previous error, wait time.Duration) {
				if previous == nil {
					progress(fmt.Sprintf("Скачивание %s (попытка %d/%d)...", name, attempt, total))
				} else if wait > 0 {
					progress(fmt.Sprintf("Сетевая ошибка: %v\nПовтор через %s...", previous, wait))
				}
			}); err != nil {
				return Result{}, failure.Wrap(failure.Network, fmt.Errorf("download %s: %w", name, err))
			}
		}
	}

	// Build one case-insensitive list of affected paths and fully back it up.
	affected := map[string]string{}
	for _, name := range append(append([]string{}, removeNames...), installNames...) {
		key := strings.ToLower(name)
		if _, ok := affected[key]; !ok {
			affected[key] = name
		}
	}
	var keys []string
	for key := range affected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	backupDir := filepath.Join(txnDir, "backup")
	if err = os.Mkdir(backupDir, 0755); err != nil {
		return Result{}, err
	}
	type backup struct{ name, actual, path string }
	backups := make([]backup, 0, len(keys))
	for i, key := range keys {
		name := affected[key]
		actual, exists := existing[key]
		if !exists {
			continue
		}
		backupPath := filepath.Join(backupDir, fmt.Sprintf("%04d.bak", i))
		if err = providers.Copy(filepath.Join(modsDir, actual), backupPath); err != nil {
			return Result{}, fmt.Errorf("backup %s: %w", actual, err)
		}
		backups = append(backups, backup{name: name, actual: actual, path: backupPath})
	}
	targetLock := filepath.Join(root, lockfile.DefaultFilename)
	lockTmp, err := atomicfile.Stage(targetLock, newLockBytes, 0644)
	if err != nil {
		return Result{}, fmt.Errorf("prepare lock file: %w", err)
	}
	defer os.Remove(lockTmp)

	rollback := func(cause error) error {
		var restoreErrs []string
		removeTargets := map[string]bool{}
		for _, name := range append(append([]string{}, removeNames...), installNames...) {
			removeTargets[name] = true
		}
		for name := range removeTargets {
			if e := os.Remove(filepath.Join(modsDir, name)); e != nil && !os.IsNotExist(e) {
				restoreErrs = append(restoreErrs, fmt.Sprintf("remove %s: %v", name, e))
			}
		}
		for _, b := range backups {
			if e := restoreBackupFile(b.path, filepath.Join(modsDir, b.actual)); e != nil {
				restoreErrs = append(restoreErrs, fmt.Sprintf("restore %s: %v", b.actual, e))
			}
		}
		if len(restoreErrs) != 0 {
			keepTxn = true
			return failure.Wrap(failure.Recovery, fmt.Errorf("%w; rollback incomplete (%s); backups preserved at %s", cause, strings.Join(restoreErrs, "; "), backupDir))
		}
		return cause
	}

	// Honour cancellation before beginning file mutations. Once they begin,
	// complete the transaction or rollback instead of abandoning half an update.
	if err = ctx.Err(); err != nil {
		return Result{}, err
	}
	for _, name := range removeNames {
		actual := existing[strings.ToLower(name)]
		if actual == "" {
			continue
		}
		if err = os.Remove(filepath.Join(modsDir, actual)); err != nil && !os.IsNotExist(err) {
			return Result{}, rollback(fmt.Errorf("remove %s: %w", name, err))
		}
	}
	for _, name := range installNames {
		actual := existing[strings.ToLower(name)]
		if actual != "" {
			if err = os.Remove(filepath.Join(modsDir, actual)); err != nil && !os.IsNotExist(err) {
				return Result{}, rollback(fmt.Errorf("replace %s: %w", name, err))
			}
		}
		if err = renameInstalledFile(filepath.Join(stage, name), filepath.Join(modsDir, name)); err != nil {
			return Result{}, rollback(fmt.Errorf("install %s: %w", name, err))
		}
	}
	if err = commitLockFile(lockTmp, targetLock); err != nil {
		return Result{}, rollback(fmt.Errorf("replace lock file: %w", err))
	}
	if oldPath != targetLock {
		_ = os.Remove(oldPath)
	}
	return Result{Diff: d, Revision: snapshot.Revision}, nil
}

func requireSupportedInstallSchema(local, remote *lockfile.File) error {
	for _, file := range []*lockfile.File{local, remote} {
		if file == nil {
			continue
		}
		if file.Schema != 1 || len(file.Files) != 0 {
			return failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("schema %d managed-file packs require the transactional schema 2 installer", file.Schema))
		}
	}
	return nil
}

// ValidateInstallSchema rejects formats that the transactional installer cannot
// safely apply. It is shared by updates and first-time instance installation.
func ValidateInstallSchema(local, remote *lockfile.File) error {
	return requireSupportedInstallSchema(local, remote)
}
