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

type targetMod struct {
	id, filename string
	mod          lockfile.ModEntry
}

type targetFile struct {
	id   string
	file lockfile.ManagedFile
}

type stagedMutation struct {
	key, targetID, relative, final, staged string
	remove                                 bool
}

func rootForSchema3(root string, roots TargetRoots, id, relative string) (string, error) {
	return resolveTargetPath(root, 3, roots, id, relative)
}

func runSchema3(ctx context.Context, root, oldPath string, old, next *lockfile.File, repoDir, remoteLock, revision string, roots TargetRoots, progress Progress, confirmed []ConfirmedConflict) (Result, error) {
	if err := ValidateTargetRoots(root, old, roots); err != nil {
		return Result{}, err
	}
	if err := ValidateTargetRoots(root, next, roots); err != nil {
		return Result{}, err
	}
	if filepath.Clean(filepath.FromSlash(next.Pack.ModsDir)) != filepath.Clean(filepath.FromSlash(old.Pack.ModsDir)) {
		return Result{}, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("unsupported migration: remote pack.mods_dir changed from %q to %q", old.Pack.ModsDir, next.Pack.ModsDir))
	}
	newLockBytes, err := os.ReadFile(remoteLock)
	if err != nil {
		return Result{}, err
	}
	d := diff.Locks(old, next)
	changes, conflicts, err := inspectManagedFilesWithRoots(root, old, next, roots)
	if err != nil {
		return Result{}, err
	}
	confirmedByTarget := map[string]string{}
	for _, c := range confirmed {
		confirmedByTarget[conflictKey(c.TargetID, c.Target)] = strings.ToLower(c.SHA256)
	}
	for _, c := range conflicts {
		if c.Kind != "existing_unmanaged" && c.Kind != "locally_modified" {
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("%s: %s", c.Target, c.Message), map[string]any{"conflicts": conflicts})
		}
		if confirmedByTarget[conflictKey(c.TargetID, c.Target)] != strings.ToLower(c.SHA256) {
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("managed file conflict at %s (sha256 %s)", c.Target, c.SHA256), map[string]any{"conflicts": conflicts})
		}
	}

	oldMods := map[string]targetMod{}
	for _, mod := range old.Mods {
		ids := mod.Targets
		if old.Schema < 3 {
			ids = []string{"client"}
		}
		for _, id := range ids {
			oldMods[resourceKey(id, mod.Identity())] = targetMod{id: id, filename: mod.Filename, mod: mod}
		}
	}
	nextMods := map[string]targetMod{}
	for _, mod := range next.Mods {
		for _, id := range mod.Targets {
			nextMods[resourceKey(id, mod.Identity())] = targetMod{id: id, filename: mod.Filename, mod: mod}
		}
	}
	var mutations []stagedMutation
	stageMods := map[string]targetMod{}
	for key, desired := range nextMods {
		modsDir, e := rootForSchema3(root, roots, desired.id, next.Pack.ModsDir)
		if e != nil {
			return Result{}, e
		}
		final, e := lockfile.ResolveWithin(modsDir, desired.filename)
		if e != nil {
			return Result{}, e
		}
		current, e := fileState(final)
		if e != nil {
			return Result{}, e
		}
		prior, hadPrior := oldMods[key]
		if current.exists && !current.regular {
			c := fileConflict(desired.id, filepath.ToSlash(filepath.Join(next.Pack.ModsDir, desired.filename)), "target_not_regular", "target is not a regular file", current.sha256)
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("mod target is not a regular file: %s", c.Target), map[string]any{"conflicts": []FileConflict{c}})
		}
		needsInstall := !hadPrior || prior.filename != desired.filename || prior.mod.SHA256 != desired.mod.SHA256 || !current.exists || !strings.EqualFold(current.sha256, desired.mod.SHA256)
		if !needsInstall {
			continue
		}
		if current.exists && (!hadPrior || prior.filename != desired.filename) {
			c := fileConflict(desired.id, filepath.ToSlash(filepath.Join(next.Pack.ModsDir, desired.filename)), "existing_unmanaged", "existing file would be replaced", current.sha256)
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("cannot install %s: destination exists and is not managed", desired.filename), map[string]any{"conflicts": []FileConflict{c}})
		}
		mutations = append(mutations, stagedMutation{key: "mod:" + key, targetID: desired.id, relative: filepath.ToSlash(filepath.Join(next.Pack.ModsDir, desired.filename)), final: final})
		stageMods[key] = desired
	}
	for key, previous := range oldMods {
		if desired, ok := nextMods[key]; ok && desired.filename == previous.filename {
			continue
		}
		modsDir, e := rootForSchema3(root, roots, previous.id, old.Pack.ModsDir)
		if e != nil {
			return Result{}, e
		}
		final, e := lockfile.ResolveWithin(modsDir, previous.filename)
		if e != nil {
			return Result{}, e
		}
		current, e := fileState(final)
		if e != nil {
			return Result{}, e
		}
		if current.exists {
			mutations = append(mutations, stagedMutation{key: "mod:" + resourceKey(previous.id, previous.filename), targetID: previous.id, relative: filepath.ToSlash(filepath.Join(old.Pack.ModsDir, previous.filename)), final: final, remove: true})
		}
	}

	oldFiles := map[string]targetFile{}
	for _, f := range old.Files {
		ids := f.Targets
		if old.Schema < 3 {
			ids = []string{"client"}
		}
		for _, id := range ids {
			oldFiles[resourceKey(id, f.Target)] = targetFile{id: id, file: f}
		}
	}
	nextFiles := map[string]targetFile{}
	for _, f := range next.Files {
		for _, id := range f.Targets {
			nextFiles[resourceKey(id, f.Target)] = targetFile{id: id, file: f}
		}
	}
	for _, change := range changes {
		key := resourceKey(change.TargetID, change.Target)
		prev, hadPrev := oldFiles[key]
		if change.Action == "remove" {
			path, e := rootForSchema3(root, roots, change.TargetID, change.Target)
			if e != nil {
				return Result{}, e
			}
			mutations = append(mutations, stagedMutation{key: "file:" + key, targetID: change.TargetID, relative: change.Target, final: path, remove: true})
			continue
		}
		file := nextFiles[key].file
		_ = prev
		_ = hadPrev
		path, e := rootForSchema3(root, roots, change.TargetID, change.Target)
		if e != nil {
			return Result{}, e
		}
		mutations = append(mutations, stagedMutation{key: "file:" + key, targetID: change.TargetID, relative: change.Target, final: path})
		_ = file
	}
	for _, conflict := range conflicts {
		key := resourceKey(conflict.TargetID, conflict.Target)
		if confirmedByTarget[conflictKey(conflict.TargetID, conflict.Target)] != strings.ToLower(conflict.SHA256) {
			continue
		}
		already := false
		for _, op := range mutations {
			if op.key == "file:"+key {
				already = true
				break
			}
		}
		if already {
			continue
		}
		path, e := rootForSchema3(root, roots, conflict.TargetID, conflict.Target)
		if e != nil {
			return Result{}, e
		}
		_, remainsManaged := nextFiles[key]
		mutations = append(mutations, stagedMutation{key: "file:" + key, targetID: conflict.TargetID, relative: conflict.Target, final: path, remove: !remainsManaged})
	}
	sort.Slice(mutations, func(i, j int) bool {
		if mutations[i].targetID != mutations[j].targetID {
			return mutations[i].targetID < mutations[j].targetID
		}
		return mutations[i].relative < mutations[j].relative
	})

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
	modStage := filepath.Join(txnDir, "stage", "mods")
	fileStage := filepath.Join(txnDir, "stage", "files")
	if err = os.MkdirAll(modStage, 0755); err != nil {
		return Result{}, err
	}
	if err = os.MkdirAll(fileStage, 0755); err != nil {
		return Result{}, err
	}
	keys := make([]string, 0, len(stageMods))
	for k := range stageMods {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	client := &http.Client{Timeout: 2 * time.Minute}
	for i, key := range keys {
		mod := stageMods[key]
		staged := filepath.Join(modStage, fmt.Sprintf("%04d.jar", i))
		if mod.mod.Source == "repo" {
			source, e := lockfile.ResolveWithin(repoDir, mod.mod.Path)
			if e != nil {
				return Result{}, e
			}
			if err = providers.Copy(source, staged); err != nil {
				return Result{}, fmt.Errorf("copy %s: %w", mod.filename, err)
			}
		} else if err = providers.DownloadWithRetry(ctx, client, mod.mod.URL, staged, 4, nil); err != nil {
			return Result{}, failure.Wrap(failure.Network, fmt.Errorf("download %s: %w", mod.filename, err))
		}
		if err = verifyStagedMod(staged, mod.mod); err != nil {
			return Result{}, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("verify downloaded mod %s: %w", mod.filename, err))
		}
		for j := range mutations {
			if mutations[j].key == "mod:"+key {
				mutations[j].staged = staged
			}
		}
	}
	for i := range mutations {
		if !strings.HasPrefix(mutations[i].key, "file:") || mutations[i].remove {
			continue
		}
		item, ok := nextFiles[strings.TrimPrefix(mutations[i].key, "file:")]
		if !ok {
			return Result{}, fmt.Errorf("managed file disappeared from target lock")
		}
		source, e := lockfile.ResolveWithin(repoDir, item.file.Path)
		if e != nil {
			return Result{}, e
		}
		staged := filepath.Join(fileStage, fmt.Sprintf("%04d.payload", i))
		if err = providers.Copy(source, staged); err != nil {
			return Result{}, fmt.Errorf("copy managed file %s: %w", item.file.Path, err)
		}
		if err = verifyStagedManaged(staged, item.file); err != nil {
			return Result{}, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("verify managed file %s: %w", item.file.Path, err))
		}
		mutations[i].staged = staged
	}

	before := map[string]targetState{}
	for _, op := range mutations {
		state, e := fileState(op.final)
		if e != nil {
			return Result{}, e
		}
		before[op.key] = state
	}
	backupDir := filepath.Join(txnDir, "backup")
	if err = os.MkdirAll(backupDir, 0755); err != nil {
		return Result{}, err
	}
	type saved struct{ final, path string }
	var backups []saved
	for i, op := range mutations {
		if !before[op.key].exists {
			continue
		}
		backup := filepath.Join(backupDir, fmt.Sprintf("%04d.bak", i))
		if err = providers.Copy(op.final, backup); err != nil {
			return Result{}, fmt.Errorf("backup %s: %w", op.relative, err)
		}
		backups = append(backups, saved{final: op.final, path: backup})
	}
	lockTarget := filepath.Join(root, lockfile.DefaultFilename)
	lockTmp, err := atomicfile.Stage(lockTarget, newLockBytes, 0644)
	if err != nil {
		return Result{}, fmt.Errorf("prepare lock file: %w", err)
	}
	defer os.Remove(lockTmp)

	rollback := func(cause error) error {
		var restore []string
		for _, op := range mutations {
			if e := os.Remove(op.final); e != nil && !os.IsNotExist(e) {
				restore = append(restore, e.Error())
			}
		}
		for _, b := range backups {
			if e := restoreBackupFile(b.path, b.final); e != nil {
				restore = append(restore, e.Error())
			}
		}
		if len(restore) != 0 {
			keepTxn = true
			return failure.Wrap(failure.Recovery, fmt.Errorf("%w; rollback incomplete (%s); backups preserved at %s", cause, strings.Join(restore, "; "), backupDir))
		}
		return cause
	}
	if err = ctx.Err(); err != nil {
		return Result{}, err
	}
	for _, op := range mutations {
		current, e := fileState(op.final)
		if e != nil {
			return Result{}, e
		}
		if approved := confirmedByTarget[conflictKey(op.targetID, op.relative)]; approved != "" && !strings.EqualFold(current.sha256, approved) {
			conflict := fileConflict(op.targetID, op.relative, "changed_during_apply", "target changed after conflict confirmation", current.sha256)
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("target changed after preview: %s", op.relative), map[string]any{"conflicts": []FileConflict{conflict}})
		}
		if current != before[op.key] {
			conflict := fileConflict(op.targetID, op.relative, "changed_during_apply", "target changed during preparation", current.sha256)
			return Result{}, failure.WrapDetails(failure.Conflict, fmt.Errorf("target changed during preparation: %s", op.relative), map[string]any{"conflicts": []FileConflict{conflict}})
		}
		// Re-resolve using the trusted root mapping immediately before mutation.
		var resolved string
		var resolveErr error
		if strings.HasPrefix(op.key, "mod:") {
			modsRoot, e := rootForSchema3(root, roots, op.targetID, next.Pack.ModsDir)
			if e != nil {
				return Result{}, e
			}
			resolved, resolveErr = lockfile.ResolveWithin(modsRoot, filepath.Base(op.relative))
		} else {
			resolved, resolveErr = rootForSchema3(root, roots, op.targetID, op.relative)
		}
		if resolveErr != nil {
			return Result{}, resolveErr
		}
		if filepath.Clean(resolved) != filepath.Clean(op.final) {
			return Result{}, failure.Wrap(failure.InvalidRequest, fmt.Errorf("target root changed during apply"))
		}
	}
	for _, op := range mutations {
		if err = os.MkdirAll(filepath.Dir(op.final), 0755); err != nil {
			return Result{}, rollback(err)
		}
		if err = os.Remove(op.final); err != nil && !os.IsNotExist(err) {
			return Result{}, rollback(err)
		}
		if !op.remove {
			if err = renameInstalledFile(op.staged, op.final); err != nil {
				return Result{}, rollback(err)
			}
		}
	}
	if err = atomicfile.Commit(lockTmp, lockTarget); err != nil {
		return Result{}, rollback(fmt.Errorf("replace lock file: %w", err))
	}
	if oldPath != lockTarget {
		_ = os.Remove(oldPath)
	}
	return Result{Diff: d, Revision: revision}, nil
}

func conflictKey(targetID, target string) string {
	return strings.ToLower(targetID + "\x00" + filepath.ToSlash(target))
}
func resourceKey(targetID, identity string) string {
	return strings.ToLower(targetID + "\x00" + identity)
}

func fileState(path string) (targetState, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return targetState{}, nil
	}
	if err != nil {
		return targetState{}, err
	}
	state := targetState{exists: true, regular: info.Mode().IsRegular()}
	if state.regular {
		state.sha256, err = hashFile(path)
	}
	return state, err
}
