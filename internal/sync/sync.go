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

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"modlock/internal/diff"
	"modlock/internal/lockfile"
	"modlock/internal/providers"
)

type Progress func(string)
type Result struct{ Diff diff.Result }

func Run(ctx context.Context, root string, progress Progress) (Result, error) {
	oldPath, err := lockfile.FindPath(root)
	if err != nil {
		return Result{}, fmt.Errorf("find mod.lock: %w", err)
	}
	old, err := lockfile.Read(oldPath)
	if err != nil {
		return Result{}, err
	}
	tmp, err := os.MkdirTemp("", "modlock-sync-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(tmp)
	repoDir := filepath.Join(tmp, "repo")
	progress("Получение обновления...")
	_, err = git.PlainCloneContext(ctx, repoDir, false, &git.CloneOptions{URL: old.Pack.Repository, ReferenceName: plumbing.NewBranchReferenceName(old.Pack.Branch), SingleBranch: true, Depth: 1})
	if err != nil {
		return Result{}, fmt.Errorf("clone repository: %w", err)
	}
	remoteLock, err := lockfile.ResolveWithin(repoDir, old.Pack.LockPath)
	if err != nil {
		return Result{}, err
	}
	next, err := lockfile.Read(remoteLock)
	if err != nil {
		return Result{}, fmt.Errorf("remote lock: %w", err)
	}
	d := diff.Locks(old, next)
	modsDir, err := lockfile.ResolveWithin(root, old.Pack.ModsDir)
	if err != nil {
		return Result{}, err
	}
	if err = os.MkdirAll(modsDir, 0755); err != nil {
		return Result{}, err
	}
	// The lock describes the desired state, not proof that its files are installed.
	// A fresh instance may have the latest bootstrap lock and an empty mods folder.
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
	// Reconcile every entry that was managed by the previous lock against the
	// new lock as well. This keeps deletion tied to the desired lock state even
	// when two lock entries have different identities or legacy metadata.
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
	stage := filepath.Join(tmp, "stage")
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
				return Result{}, fmt.Errorf("download %s: %w", name, err)
			}
		}
	}
	backup := filepath.Join(tmp, "backup")
	var changed []string
	rollback := func() {
		for _, n := range changed {
			os.Remove(filepath.Join(modsDir, n))
			b := filepath.Join(backup, n)
			if _, e := os.Stat(b); e == nil {
				_ = providers.Copy(b, filepath.Join(modsDir, n))
			}
		}
	}
	for _, name := range append(append([]string{}, removeNames...), installNames...) {
		target := filepath.Join(modsDir, name)
		if _, e := os.Stat(target); e == nil {
			if e = providers.Copy(target, filepath.Join(backup, name)); e != nil {
				rollback()
				return Result{}, e
			}
		}
		changed = append(changed, name)
	}
	for _, name := range removeNames {
		if err = os.Remove(filepath.Join(modsDir, name)); err != nil && !os.IsNotExist(err) {
			rollback()
			return Result{}, err
		}
	}
	for _, name := range installNames {
		if err = os.Remove(filepath.Join(modsDir, name)); err != nil && !os.IsNotExist(err) {
			rollback()
			return Result{}, err
		}
	}
	for _, name := range installNames {
		if err = os.Rename(filepath.Join(stage, name), filepath.Join(modsDir, name)); err != nil {
			rollback()
			return Result{}, err
		}
	}
	newLockBytes, err := os.ReadFile(remoteLock)
	if err != nil {
		rollback()
		return Result{}, err
	}
	targetLock := filepath.Join(root, lockfile.DefaultFilename)
	lockTmp := targetLock + ".tmp"
	if err = os.WriteFile(lockTmp, newLockBytes, 0644); err != nil {
		rollback()
		return Result{}, err
	}
	if err = os.Rename(lockTmp, targetLock); err != nil {
		os.Remove(targetLock)
		err = os.Rename(lockTmp, targetLock)
	}
	if err != nil {
		rollback()
		return Result{}, err
	}
	if oldPath != targetLock {
		_ = os.Remove(oldPath)
	}
	return Result{Diff: d}, nil
}
