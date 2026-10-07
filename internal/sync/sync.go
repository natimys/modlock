package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
	return run(ctx, root, "", progress, nil)
}

// RunRevision applies the commit returned by Check, even if the branch has moved.
func RunRevision(ctx context.Context, root, revision string, progress Progress) (Result, error) {
	if revision == "" {
		return Result{}, fmt.Errorf("apply requires the previewed Git commit")
	}
	return run(ctx, root, revision, progress, nil)
}

type ConfirmedConflict struct {
	Target   string `json:"target"`
	TargetID string `json:"target_id,omitempty"`
	SHA256   string `json:"sha256"`
}

type targetState struct {
	exists  bool
	regular bool
	sha256  string
	name    string
}

func snapshotManagedTargets(root string, old, next *lockfile.File) (map[string]targetState, error) {
	targets := map[string]string{}
	if old != nil {
		for _, file := range old.Files {
			targets[strings.ToLower(filepath.ToSlash(file.Target))] = file.Target
		}
	}
	for _, file := range next.Files {
		targets[strings.ToLower(filepath.ToSlash(file.Target))] = file.Target
	}
	states := make(map[string]targetState, len(targets))
	for key, target := range targets {
		path, err := lockfile.ResolveWithin(root, target)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			states[key] = targetState{}
			continue
		}
		if err != nil {
			return nil, err
		}
		state := targetState{exists: true, regular: info.Mode().IsRegular()}
		if state.regular {
			state.sha256, err = hashFile(path)
			if err != nil {
				return nil, err
			}
		}
		states[key] = state
	}
	return states, nil
}

func changedManagedTarget(before, after map[string]targetState) string {
	for key, initial := range before {
		if current, ok := after[key]; !ok || current != initial {
			return key
		}
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			return key
		}
	}
	return ""
}

func snapshotModTargets(modsDir string, names []string) (map[string]targetState, error) {
	entries, err := os.ReadDir(modsDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	actualNames := make(map[string]string, len(entries))
	for _, entry := range entries {
		actualNames[strings.ToLower(entry.Name())] = entry.Name()
	}
	states := make(map[string]targetState, len(names))
	for _, name := range names {
		key := strings.ToLower(name)
		actual := actualNames[key]
		if actual == "" {
			states[key] = targetState{}
			continue
		}
		path, err := lockfile.ResolveWithin(modsDir, actual)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		state := targetState{exists: true, regular: info.Mode().IsRegular(), name: actual}
		if state.regular {
			state.sha256, err = hashFile(path)
			if err != nil {
				return nil, err
			}
		}
		states[key] = state
	}
	return states, nil
}

func RunRevisionConfirmed(ctx context.Context, root, revision string, progress Progress, confirmed []ConfirmedConflict) (Result, error) {
	return RunRevisionConfirmedWithRoots(ctx, root, revision, progress, confirmed, nil)
}

func RunRevisionConfirmedWithRoots(ctx context.Context, root, revision string, progress Progress, confirmed []ConfirmedConflict, roots TargetRoots) (Result, error) {
	if revision == "" {
		return Result{}, fmt.Errorf("apply requires the previewed Git commit")
	}
	return runWithRoots(ctx, root, revision, progress, confirmed, roots)
}

func run(ctx context.Context, root, revision string, progress Progress, confirmed []ConfirmedConflict) (Result, error) {
	return runWithRoots(ctx, root, revision, progress, confirmed, nil)
}

func runWithRoots(ctx context.Context, root, revision string, progress Progress, confirmed []ConfirmedConflict, roots TargetRoots) (Result, error) {
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
	if old.Schema == 3 && next.Schema < 3 {
		return Result{}, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("schema 3 cannot be downgraded to schema %d during sync", next.Schema))
	}
	if next.Schema == 3 {
		return runSchema3(ctx, root, oldPath, old, next, repoDir, remoteLock, snapshot.Revision, roots, progress, confirmed)
	}
	if filepath.Clean(filepath.FromSlash(next.Pack.ModsDir)) != filepath.Clean(filepath.FromSlash(old.Pack.ModsDir)) {
		return Result{}, fmt.Errorf("unsupported migration: remote pack.mods_dir changed from %q to %q", old.Pack.ModsDir, next.Pack.ModsDir)
	}
	newLockBytes, err := os.ReadFile(remoteLock)
	if err != nil {
		return Result{}, err
	}
	d := diff.Locks(old, next)
	_, conflicts, err := inspectManagedFiles(root, old, next)
	if err != nil {
		return Result{}, err
	}
	confirmedByTarget := map[string]string{}
	for _, c := range confirmed {
		confirmedByTarget[strings.ToLower(filepath.ToSlash(c.Target))] = strings.ToLower(c.SHA256)
	}
	for _, c := range conflicts {
		if c.Kind != "existing_unmanaged" && c.Kind != "locally_modified" {
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("%s: %s", c.Target, c.Message), map[string]any{"conflicts": conflicts})
		}
		if confirmedByTarget[strings.ToLower(filepath.ToSlash(c.Target))] != strings.ToLower(c.SHA256) {
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("managed file conflict at %s (sha256 %s)", c.Target, c.SHA256), map[string]any{"conflicts": conflicts})
		}
	}
	managedBefore, err := snapshotManagedTargets(root, old, next)
	if err != nil {
		return Result{}, err
	}
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
	local, err := VerifyLock(root, next)
	if err != nil {
		return Result{}, err
	}
	for _, name := range append(append([]string{}, local.Missing...), local.Damaged...) {
		if !install[name] {
			d.Added = append(d.Added, name)
		}
		install[name] = true
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
	modTargets := append(append([]string{}, removeNames...), installNames...)
	modBefore, err := snapshotModTargets(modsDir, modTargets)
	if err != nil {
		return Result{}, err
	}
	for _, state := range modBefore {
		if state.exists && !state.regular {
			target := filepath.ToSlash(filepath.Join(old.Pack.ModsDir, state.name))
			conflict := fileConflict("", target, "target_not_regular", "mod target is not a regular file", "")
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("mod target %s is not a regular file", target), map[string]any{"conflicts": []FileConflict{conflict}})
		}
	}

	txnDir, err := os.MkdirTemp(root, ".modlock-sync-")
	if err != nil {
		return Result{}, err
	}
	keepTxn := false
	defer func() {
		if !keepTxn {
			_ = os.RemoveAll(txnDir)
		}
	}()
	stage := filepath.Join(txnDir, "stage", "mods")
	if err = os.MkdirAll(stage, 0755); err != nil {
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
		if err := verifyStagedMod(dst, m); err != nil {
			return Result{}, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("verify downloaded mod %s: %w", name, err))
		}
	}
	// Stage and hash every managed payload before touching the instance.
	managedStage := filepath.Join(txnDir, "stage", "files")
	type managedOp struct {
		target, staged string
		remove         bool
	}
	var managedOps []managedOp
	oldManaged := map[string]lockfile.ManagedFile{}
	for _, f := range old.Files {
		oldManaged[strings.ToLower(filepath.ToSlash(f.Target))] = f
	}
	nextManaged := map[string]lockfile.ManagedFile{}
	for _, f := range next.Files {
		nextManaged[strings.ToLower(filepath.ToSlash(f.Target))] = f
	}
	managedTargets := map[string]string{}
	for k, f := range oldManaged {
		managedTargets[k] = f.Target
	}
	for k, f := range nextManaged {
		managedTargets[k] = f.Target
	}
	var managedKeys []string
	for k := range managedTargets {
		managedKeys = append(managedKeys, k)
	}
	sort.Strings(managedKeys)
	for i, key := range managedKeys {
		target := managedTargets[key]
		finalPath, resolveErr := lockfile.ResolveWithin(root, target)
		if resolveErr != nil {
			return Result{}, resolveErr
		}
		info, statErr := os.Lstat(finalPath)
		exists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return Result{}, statErr
		}
		if exists && !info.Mode().IsRegular() {
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("managed target %s is not a regular file", target), map[string]any{"conflicts": []FileConflict{fileConflict("", target, "target_not_regular", "target is not a regular file", "")}})
		}
		oldFile, hadOld := oldManaged[key]
		newFile, hasNew := nextManaged[key]
		if !hasNew {
			if hadOld && oldFile.Policy == "replace" && exists {
				digest, hashErr := hashFile(finalPath)
				if hashErr != nil {
					return Result{}, hashErr
				}
				if !strings.EqualFold(digest, oldFile.SHA256) && confirmedByTarget[key] != strings.ToLower(digest) {
					return Result{}, failure.Wrap(failure.Conflict, fmt.Errorf("managed file conflict at %s (sha256 %s)", target, digest))
				}
				if strings.EqualFold(digest, oldFile.SHA256) || confirmedByTarget[key] == strings.ToLower(digest) {
					managedOps = append(managedOps, managedOp{target: finalPath, remove: true})
				}
			}
			continue
		}
		if newFile.Policy == "if_missing" && exists {
			continue
		}
		if newFile.Policy == "replace" && exists {
			digest, hashErr := hashFile(finalPath)
			if hashErr != nil {
				return Result{}, hashErr
			}
			if strings.EqualFold(digest, newFile.SHA256) {
				continue
			}
			if (!hadOld || oldFile.Policy == "if_missing" || (oldFile.Policy == "replace" && !strings.EqualFold(digest, oldFile.SHA256))) && confirmedByTarget[key] != strings.ToLower(digest) {
				kind := "existing_unmanaged"
				if hadOld && oldFile.Policy == "replace" {
					kind = "locally_modified"
				}
				conflict := fileConflict("", target, kind, "existing file would be replaced", digest)
				return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("managed file conflict at %s (sha256 %s)", target, digest), map[string]any{"conflicts": []FileConflict{conflict}})
			}
		}
		source, resolveErr := lockfile.ResolveWithin(repoDir, newFile.Path)
		if resolveErr != nil {
			return Result{}, resolveErr
		}
		staged := filepath.Join(managedStage, fmt.Sprintf("%04d", i))
		progress("Копирование " + target + "...")
		if err = providers.Copy(source, staged); err != nil {
			return Result{}, fmt.Errorf("stage managed file %s: %w", target, err)
		}
		if err = verifyStagedManaged(staged, newFile); err != nil {
			return Result{}, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("verify managed file %s: %w", target, err))
		}
		managedOps = append(managedOps, managedOp{target: finalPath, staged: staged})
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
	type managedBackup struct{ target, path string }
	var managedBackups []managedBackup
	managedBackupDir := filepath.Join(txnDir, "managed-backup")
	if err = os.Mkdir(managedBackupDir, 0755); err != nil {
		return Result{}, err
	}
	for i, op := range managedOps {
		info, statErr := os.Lstat(op.target)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return Result{}, statErr
		}
		if !info.Mode().IsRegular() {
			return Result{}, failure.Wrap(failure.Conflict, fmt.Errorf("managed target is not a regular file: %s", op.target))
		}
		backupPath := filepath.Join(managedBackupDir, fmt.Sprintf("%04d.bak", i))
		if err = providers.Copy(op.target, backupPath); err != nil {
			return Result{}, fmt.Errorf("backup managed file %s: %w", op.target, err)
		}
		managedBackups = append(managedBackups, managedBackup{target: op.target, path: backupPath})
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
		for _, op := range managedOps {
			if e := os.Remove(op.target); e != nil && !os.IsNotExist(e) {
				restoreErrs = append(restoreErrs, fmt.Sprintf("remove %s: %v", op.target, e))
			}
		}
		for _, b := range managedBackups {
			if e := restoreBackupFile(b.path, b.target); e != nil {
				restoreErrs = append(restoreErrs, fmt.Sprintf("restore %s: %v", b.target, e))
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
	// Hashes in the confirmation are authorization for one exact observed file.
	// Recheck immediately before mutation so a file changed during preparation
	// cannot be overwritten using an earlier preview.
	managedAfter, stateErr := snapshotManagedTargets(root, old, next)
	if stateErr != nil {
		return Result{}, stateErr
	}
	if changedManagedTarget(managedBefore, managedAfter) != "" {
		changed := changedManagedTarget(managedBefore, managedAfter)
		_, latestConflicts, inspectErr := inspectManagedFiles(root, old, next)
		if inspectErr != nil {
			return Result{}, inspectErr
		}
		state := managedAfter[changed]
		latestConflicts = append(latestConflicts, fileConflict("", filepath.ToSlash(changed), "changed_during_apply", "target changed during preparation", state.sha256))
		sort.Slice(latestConflicts, func(i, j int) bool { return latestConflicts[i].Target < latestConflicts[j].Target })
		return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("managed file changed during preparation: %s", changed), map[string]any{"conflicts": latestConflicts})
	}
	modAfter, stateErr := snapshotModTargets(modsDir, modTargets)
	if stateErr != nil {
		return Result{}, stateErr
	}
	if changed := changedManagedTarget(modBefore, modAfter); changed != "" {
		state := modAfter[changed]
		target := filepath.ToSlash(filepath.Join(old.Pack.ModsDir, state.name))
		if !state.exists {
			target = filepath.ToSlash(filepath.Join(old.Pack.ModsDir, filepath.Base(changed)))
		}
		conflict := fileConflict("", target, "changed_during_apply", "mod target changed during preparation", state.sha256)
		return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("mod target changed during preparation: %s", target), map[string]any{"conflicts": []FileConflict{conflict}})
	}
	for _, conflict := range conflicts {
		path, resolveErr := lockfile.ResolveWithin(root, conflict.Target)
		if resolveErr != nil {
			return Result{}, resolveErr
		}
		current, hashErr := hashFile(path)
		if hashErr != nil || !strings.EqualFold(current, conflict.SHA256) || confirmedByTarget[strings.ToLower(filepath.ToSlash(conflict.Target))] != strings.ToLower(current) {
			_, latestConflicts, _ := inspectManagedFiles(root, old, next)
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("managed file changed after preview: %s", conflict.Target), map[string]any{"conflicts": latestConflicts})
		}
	}
	if err = os.MkdirAll(modsDir, 0755); err != nil {
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
	for _, op := range managedOps {
		if err = os.MkdirAll(filepath.Dir(op.target), 0755); err != nil {
			return Result{}, rollback(fmt.Errorf("create directory for %s: %w", op.target, err))
		}
		if err = os.Remove(op.target); err != nil && !os.IsNotExist(err) {
			return Result{}, rollback(fmt.Errorf("replace managed file %s: %w", op.target, err))
		}
		if !op.remove {
			if err = renameInstalledFile(op.staged, op.target); err != nil {
				return Result{}, rollback(fmt.Errorf("install managed file %s: %w", op.target, err))
			}
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

func verifyStagedMod(path string, mod lockfile.ModEntry) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("file must be a non-empty regular file")
	}
	if mod.SHA256 == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), mod.SHA256) {
		return fmt.Errorf("SHA-256 does not match lock")
	}
	return nil
}

func verifyStagedManaged(path string, managed lockfile.ManagedFile) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("file must be regular")
	}
	digest, err := hashFile(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(digest, managed.SHA256) {
		return fmt.Errorf("SHA-256 does not match lock")
	}
	return nil
}

func requireSupportedInstallSchema(local, remote *lockfile.File) error {
	for _, file := range []*lockfile.File{local, remote} {
		if file == nil {
			continue
		}
		if file.Schema != 1 && file.Schema != 2 && file.Schema != 3 {
			return failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("unsupported schema %d", file.Schema))
		}
	}
	return nil
}

// ValidateInstallSchema rejects formats that the transactional installer cannot
// safely apply. It is shared by updates and first-time instance installation.
func ValidateInstallSchema(local, remote *lockfile.File) error {
	return requireSupportedInstallSchema(local, remote)
}
