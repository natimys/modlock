package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"modlock/internal/diff"
	ignorefile "modlock/internal/ignore"
	"modlock/internal/lockfile"
	"modlock/internal/providers"
	"modlock/internal/push"
	syncer "modlock/internal/sync"
)

var newDetector = providers.New

// ExplicitRootEnv is private process configuration supplied by the CLI. It is
// never persisted in the lock or in the user's environment.
const ExplicitRootEnv = "MODLOCK_INSTANCE_ROOT"

func FindRoot(requireLock bool) (string, error) {
	if root := os.Getenv(ExplicitRootEnv); root != "" {
		return ValidateRoot(root)
	}
	cwd, _ := os.Getwd()
	if _, e := lockfile.FindPath(cwd); e == nil {
		return cwd, nil
	}
	if launcherDir := os.Getenv("MODLOCK_LAUNCHER_DIR"); launcherDir != "" {
		if _, e := lockfile.FindPath(launcherDir); e == nil {
			return launcherDir, nil
		}
	}
	exe, _ := os.Executable()
	ed := filepath.Dir(exe)
	if _, e := lockfile.FindPath(ed); e == nil {
		return ed, nil
	}
	if !requireLock {
		if st, e := os.Stat(filepath.Join(cwd, "mods")); e == nil && st.IsDir() {
			return cwd, nil
		}
		if launcherDir := os.Getenv("MODLOCK_LAUNCHER_DIR"); launcherDir != "" {
			if st, e := os.Stat(filepath.Join(launcherDir, "mods")); e == nil && st.IsDir() {
				return launcherDir, nil
			}
		}
		if st, e := os.Stat(filepath.Join(ed, "mods")); e == nil && st.IsDir() {
			return ed, nil
		}
	}
	return "", errors.New("mod.lock not found; put it in the instance root or run modlock init")
}

// ValidateRoot accepts an existing, possibly empty Minecraft directory. An
// explicit root must never silently fall back to a different instance.
func ValidateRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("--root requires a directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve --root: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("open --root: %w", err)
	}
	if !st.IsDir() {
		return "", errors.New("--root must be a directory")
	}
	return lockfile.ResolveWithin(abs, ".")
}

func readProjectLock(root string) (*lockfile.File, string, error) {
	p, err := lockfile.FindPath(root)
	if err != nil {
		return nil, "", err
	}
	f, err := lockfile.Read(p)
	return f, p, err
}

func Sync(ctx context.Context, args ...string) error {
	target, err := parseSyncTarget(args)
	if err != nil {
		return err
	}
	root, e := FindRoot(true)
	if e != nil {
		return e
	}
	r, e := syncer.RunForTarget(ctx, root, target, func(s string) { fmt.Println(s) })
	if e != nil {
		return e
	}
	printDiff("Update applied:", r.Diff)
	fmt.Println("Сборка обновлена.")
	return nil
}

func parseSyncTarget(args []string) (string, error) {
	target := "client"
	seen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := ""
		switch {
		case arg == "--target":
			i++
			if i == len(args) {
				return "", fmt.Errorf("sync --target requires client or server")
			}
			value = args[i]
		case strings.HasPrefix(arg, "--target="):
			value = strings.TrimPrefix(arg, "--target=")
		default:
			return "", fmt.Errorf("unknown sync option %q; usage: modlock sync [--target client|server]", arg)
		}
		if seen {
			return "", fmt.Errorf("sync --target may be specified only once")
		}
		seen = true
		if value != "client" && value != "server" {
			return "", fmt.Errorf("sync target must be client or server")
		}
		target = value
	}
	return target, nil
}
func Diff() error {
	root, e := FindRoot(true)
	if e != nil {
		return e
	}
	l, _, e := readProjectLock(root)
	if e != nil {
		return e
	}
	md, e := lockfile.ResolveWithin(root, l.Pack.ModsDir)
	if e != nil {
		return e
	}
	d, e := diff.Local(md, l)
	if e != nil {
		return e
	}
	ignored, e := ignorefile.Read(root)
	if e != nil {
		return e
	}
	detected := map[string]lockfile.ModEntry{}
	ignoredAdded := map[string]bool{}
	if len(d.Added) > 0 {
		fmt.Println("Поиск источников добавленных модов...")
		det := newDetector()
		for i, name := range d.Added {
			m, detectErr := det.Detect(context.Background(), filepath.Join(md, name))
			if detectErr != nil {
				return fmt.Errorf("detect source for %s: %w", name, detectErr)
			}
			detected[name] = m
			identityEntry := m
			if identityEntry.Source == "" && identityEntry.ID == "" {
				identityEntry = providers.RepoFallback(name)
			}
			if ignored.Contains(identityEntry.Identity()) {
				ignoredAdded[name] = true
			}
			fmt.Printf("[%d/%d] %s: %s\n", i+1, len(d.Added), name, sourceName(m.Source))
		}
	}
	removedEntries := map[string]lockfile.ModEntry{}
	for _, m := range l.Mods {
		removedEntries[m.Filename] = m
	}
	removedUsed, addedUsed := map[string]bool{}, map[string]bool{}
	for _, newName := range d.Added {
		m := detected[newName]
		if m.ID == "" {
			continue
		}
		for _, oldName := range d.Removed {
			old := removedEntries[oldName]
			if !removedUsed[oldName] && old.Identity() == m.Identity() {
				d.Updated = append(d.Updated, diff.Update{Old: old, New: m})
				removedUsed[oldName] = true
				addedUsed[newName] = true
				break
			}
		}
	}
	var added, removed, unknown []string
	for _, n := range d.Added {
		if !addedUsed[n] && !ignoredAdded[n] {
			added = append(added, n)
			if detected[n].Source == "" {
				unknown = append(unknown, n)
			}
		}
	}
	for _, n := range d.Removed {
		if !removedUsed[n] {
			removed = append(removed, n)
		}
	}
	d.Added, d.Removed = added, removed
	printDiff("\nLocal changes:", d)
	printUnknown(unknown)
	return nil
}
func printDiff(title string, d diff.Result) {
	fmt.Println(title)
	fmt.Println()
	for _, n := range d.Added {
		fmt.Println("+", n)
	}
	for _, n := range d.Removed {
		fmt.Println("-", n)
	}
	for _, u := range d.Updated {
		fmt.Printf("~ %s: %s -> %s", u.New.Identity(), u.Old.DisplayVersion(), u.New.DisplayVersion())
		if u.Old.Filename != u.New.Filename {
			fmt.Printf(" (%s -> %s)", u.Old.Filename, u.New.Filename)
		}
		fmt.Println()
	}
	fmt.Printf("\n%d added\n%d updated\n%d removed\nUnchanged: %d\n", len(d.Added), len(d.Updated), len(d.Removed), d.Unchanged)
}

func Push(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	msg := fs.String("m", "modlock: update pack", "commit message")
	if e := fs.Parse(args); e != nil {
		return e
	}
	root, e := FindRoot(true)
	if e != nil {
		return e
	}
	gitRoot, e := push.GitRoot(root)
	if e != nil {
		return e
	}
	rootAbs, _ := filepath.Abs(root)
	gitAbs, _ := filepath.Abs(gitRoot)
	if rootAbs != gitAbs {
		return fmt.Errorf("instance root must be the Git repository root")
	}
	old, _, e := readProjectLock(root)
	if e != nil {
		return e
	}
	if e = push.EnsureOrigin(root, old.Pack.Repository); e != nil {
		return e
	}
	ignored, e := ignorefile.Read(root)
	if e != nil {
		return e
	}
	next, copies, deletes, s, e := push.Prepare(ctx, root, old, ignored, func(i, n int, name, source string) { fmt.Printf("[%d/%d] %s: %s\n", i, n, name, source) })
	if e != nil {
		return e
	}
	managedChanges := push.HasManagedChanges(root)
	unpushed := push.HasUnpushedCommits(root)
	if s.Added == 0 && s.Updated == 0 && s.Removed == 0 && !managedChanges {
		if unpushed {
			if !push.Confirm(bufio.NewReader(os.Stdin), "Есть незапушенные commits. Push now? [y/N] ", false) {
				return nil
			}
			return push.PushBranch(root, old.Pack.Branch)
		}
		fmt.Println("Nothing to push.")
		return nil
	}
	fmt.Printf("\nNew lock:\n+ %d mods\n~ %d updated\n- %d mods\n\nProviders:\nModrinth: %d\nCurseForge: %d\nRepository: %d\n\n", s.Added, s.Updated, s.Removed, s.Modrinth, s.CurseForge, s.Repository)
	if !push.Confirm(bufio.NewReader(os.Stdin), "Push these changes? [y/N] ", false) {
		fmt.Println("Cancelled.")
		return nil
	}
	return push.Apply(root, next, copies, deletes, *msg)
}

func Revert(args []string, in io.Reader) error {
	root, err := FindRoot(true)
	if err != nil {
		return err
	}
	gitRoot, err := push.GitRoot(root)
	if err != nil {
		return err
	}
	rootAbs, _ := filepath.Abs(root)
	gitAbs, _ := filepath.Abs(gitRoot)
	if rootAbs != gitAbs {
		return fmt.Errorf("instance root must be the Git repository root")
	}
	lf, _, err := readProjectLock(root)
	if err != nil {
		return err
	}
	if err = push.EnsureOrigin(root, lf.Pack.Repository); err != nil {
		return err
	}
	reader := bufio.NewReader(in)
	ref := ""
	if len(args) > 0 {
		ref = args[0]
	} else {
		commits, listErr := push.RecentCommits(root, 20)
		if listErr != nil {
			return listErr
		}
		if len(commits) < 2 {
			return errors.New("not enough commits to revert")
		}
		fmt.Println("Выберите состояние, к которому нужно вернуться:")
		for i, c := range commits {
			marker := ""
			if i == 0 {
				marker = " (текущий)"
			}
			fmt.Printf("%d. %s  %s%s\n", i+1, c.Short, c.Subject, marker)
		}
		n, parseErr := strconv.Atoi(ask(reader, "Номер commit: ", ""))
		if parseErr != nil || n < 2 || n > len(commits) {
			return errors.New("выберите предыдущий commit из списка")
		}
		ref = commits[n-1].Hash
	}
	target, err := push.ResolveCommit(root, ref)
	if err != nil {
		return err
	}
	stat, count, err := push.RevertSummary(root, target)
	if err != nil {
		return err
	}
	short := target
	if len(short) > 12 {
		short = short[:12]
	}
	fmt.Printf("\nБудут отменены изменения после %s (%d commits).\n", short, count)
	if stat != "" {
		fmt.Println("\n" + stat)
	}
	if !push.Confirm(reader, "\nСоздать и запушить rollback commit? [y/N] ", false) {
		fmt.Println("Revert cancelled.")
		return nil
	}
	return push.RevertTo(root, target, lf.Pack.Branch)
}

func Add(ctx context.Context, args []string, in io.Reader) error {
	root, err := FindRoot(true)
	if err != nil {
		return err
	}
	lf, _, err := readProjectLock(root)
	lockPath := filepath.Join(root, lockfile.DefaultFilename)
	if err != nil {
		return err
	}
	modsDir, err := lockfile.ResolveWithin(root, lf.Pack.ModsDir)
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, m := range lf.Mods {
		tracked[strings.ToLower(m.Filename)] = true
	}
	var available []string
	entries, err := os.ReadDir(modsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".jar") && !tracked[strings.ToLower(e.Name())] {
			available = append(available, e.Name())
		}
	}
	sort.Strings(available)
	if len(available) == 0 {
		fmt.Println("Новых модов нет.")
		return nil
	}
	reader := bufio.NewReader(in)
	name := ""
	if len(args) > 0 {
		name = filepath.Base(args[0])
	} else {
		fmt.Println("Новые моды:")
		for i, n := range available {
			fmt.Printf("%d. %s\n", i+1, n)
		}
		choice := ask(reader, "Выберите номер: ", "")
		n, parseErr := strconv.Atoi(choice)
		if parseErr != nil || n < 1 || n > len(available) {
			return errors.New("неверный номер мода")
		}
		name = available[n-1]
	}
	found := false
	for _, n := range available {
		if strings.EqualFold(n, name) {
			name = n
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%s is not a new jar in %s", name, lf.Pack.ModsDir)
	}
	path, err := lockfile.ResolveWithin(modsDir, name)
	if err != nil {
		return err
	}
	fmt.Println("Поиск источника", name+"...")
	detected, err := newDetector().Detect(ctx, path)
	if err != nil {
		return err
	}
	if detected.Source == "" && len(lf.Mods) > 0 {
		fmt.Println("\nЧтобы отметить файл как новую версию существующего мода, укажите тот же Stable mod ID:")
		for _, existing := range lf.Mods {
			fmt.Printf("  %s  version=%s  file=%s\n", existing.Identity(), existing.DisplayVersion(), existing.Filename)
		}
	}
	entry, cancelled, err := chooseSource(reader, name, detected)
	if err != nil {
		return err
	}
	if cancelled {
		fmt.Println("Добавление отменено.")
		return nil
	}
	ignored, err := ignorefile.Read(root)
	if err != nil {
		return err
	}
	if ignored.Contains(entry.Identity()) {
		return fmt.Errorf("mod %s is ignored in %s", entry.Identity(), ignorefile.Filename)
	}
	updateIndex := -1
	var previous lockfile.ModEntry
	for i, existing := range lf.Mods {
		if existing.Identity() == entry.Identity() {
			updateIndex = i
			previous = existing
			break
		}
	}
	if updateIndex >= 0 {
		lf.Mods[updateIndex] = entry
	} else {
		lf.Mods = append(lf.Mods, entry)
	}
	if err = lf.Validate(); err != nil {
		return err
	}
	if entry.Source == "repo" {
		dst, resolveErr := lockfile.ResolveWithin(root, entry.Path)
		if resolveErr != nil {
			return resolveErr
		}
		if err = providers.Copy(path, dst); err != nil {
			return err
		}
	}
	if err = lockfile.Write(lockPath, lf); err != nil {
		return err
	}
	if updateIndex >= 0 {
		if previous.Filename != entry.Filename {
			_ = os.Remove(filepath.Join(modsDir, previous.Filename))
		}
		if previous.Source == "repo" && previous.Path != entry.Path {
			if oldRepo, resolveErr := lockfile.ResolveWithin(root, previous.Path); resolveErr == nil {
				_ = os.Remove(oldRepo)
			}
		}
		fmt.Printf("Обновлён %s: %s -> %s.\n", entry.Identity(), previous.DisplayVersion(), entry.DisplayVersion())
	} else {
		fmt.Printf("Добавлен %s (%s).\n", name, sourceName(entry.Source))
	}
	return nil
}

func Ignore(ctx context.Context, args []string, in io.Reader) error {
	root, err := FindRoot(true)
	if err != nil {
		return err
	}
	lf, _, err := readProjectLock(root)
	if err != nil {
		return err
	}
	modsDir, err := lockfile.ResolveWithin(root, lf.Pack.ModsDir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(modsDir)
	if err != nil {
		return err
	}
	var jars []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".jar") {
			jars = append(jars, e.Name())
		}
	}
	sort.Strings(jars)
	if len(jars) == 0 {
		fmt.Println("В папке mods нет JAR-файлов.")
		return nil
	}
	reader := bufio.NewReader(in)
	name := ""
	if len(args) > 0 {
		name = filepath.Base(filepath.Clean(args[0]))
	} else {
		fmt.Println("Выберите мод для ignore:")
		for i, n := range jars {
			label := ""
			for _, m := range lf.Mods {
				if strings.EqualFold(m.Filename, n) {
					label = " [" + m.Identity() + "]"
					break
				}
			}
			fmt.Printf("%d. %s%s\n", i+1, n, label)
		}
		n, parseErr := strconv.Atoi(ask(reader, "Номер: ", ""))
		if parseErr != nil || n < 1 || n > len(jars) {
			return errors.New("неверный номер мода")
		}
		name = jars[n-1]
	}
	found := false
	for _, n := range jars {
		if strings.EqualFold(n, name) {
			name = n
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%s not found in %s", name, lf.Pack.ModsDir)
	}
	var entity lockfile.ModEntry
	for _, m := range lf.Mods {
		if strings.EqualFold(m.Filename, name) {
			entity = m
			break
		}
	}
	if entity.Filename == "" {
		path, resolveErr := lockfile.ResolveWithin(modsDir, name)
		if resolveErr != nil {
			return resolveErr
		}
		entity, err = newDetector().Detect(ctx, path)
		if err != nil {
			return err
		}
		if entity.Source == "" {
			base := strings.TrimSuffix(name, filepath.Ext(name))
			id := ask(reader, "Stable mod ID [repo:"+base+"]: ", "repo:"+base)
			entity = providers.RepoFallbackWithIdentity(name, id, "ignored")
		}
	}
	ignored, err := ignorefile.Read(root)
	if err != nil {
		return err
	}
	ignored.Add(entity.Identity())
	kept := lf.Mods[:0]
	removed := 0
	for _, m := range lf.Mods {
		if strings.EqualFold(m.Identity(), entity.Identity()) {
			removed++
			if m.Source == "repo" {
				if p, e := lockfile.ResolveWithin(root, m.Path); e == nil {
					_ = os.Remove(p)
				}
			}
			continue
		}
		kept = append(kept, m)
	}
	lf.Mods = kept
	if err = ignorefile.Write(root, ignored); err != nil {
		return err
	}
	if err = lockfile.Write(filepath.Join(root, lockfile.DefaultFilename), lf); err != nil {
		return err
	}
	fmt.Printf("Ignored: %s (%s). Удалено записей из mod.lock: %d.\n", name, entity.Identity(), removed)
	return nil
}

func Init(ctx context.Context, in io.Reader) error {
	root, e := FindRoot(false)
	if e != nil {
		root, _ = os.Getwd()
	}
	reader := bufio.NewReader(in)
	fmt.Println("ModLock init\n\nПроверка CurseForge...")
	cfStatus := newDetector().CheckCurseForge(ctx)
	if cfStatus.Available {
		fmt.Println("CurseForge: доступен —", cfStatus.Message)
	} else {
		fmt.Println("CurseForge: недоступен —", cfStatus.Message)
	}
	fmt.Println()
	lockPath := filepath.Join(root, lockfile.DefaultFilename)
	if existing, findErr := lockfile.FindPath(root); findErr == nil && !push.Confirm(reader, filepath.Base(existing)+" already exists. Overwrite it? [y/N] ", false) {
		fmt.Println("Initialization cancelled.")
		return nil
	}
	repo := ask(reader, "Repository URL: ", "")
	if repo == "" {
		return errors.New("repository URL is required")
	}
	branch := ask(reader, "Branch [main]: ", "main")
	lp := ask(reader, "Lock path [mod.lock]: ", lockfile.DefaultFilename)
	md := ask(reader, "Mods directory [mods]: ", "mods")
	modsDir, e := lockfile.ResolveWithin(root, md)
	if e != nil {
		return e
	}
	entries, e := os.ReadDir(modsDir)
	if e != nil {
		return e
	}
	var jars []string
	for _, x := range entries {
		if !x.IsDir() && strings.EqualFold(filepath.Ext(x.Name()), ".jar") {
			jars = append(jars, x.Name())
		}
	}
	f := &lockfile.File{Schema: 1, Pack: lockfile.Pack{Repository: repo, Branch: branch, LockPath: lp, ModsDir: md}}
	det := newDetector()
	ignored, e := ignorefile.Read(root)
	if e != nil {
		return e
	}
	tmp, e := os.MkdirTemp("", "modlock-init-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	counts := map[string]int{}
	var unknown []string
	fmt.Println("\nScanning mods...")
	for i, n := range jars {
		path, resolveErr := lockfile.ResolveWithin(modsDir, n)
		if resolveErr != nil {
			return resolveErr
		}
		m, e := det.Detect(ctx, path)
		if e != nil {
			return e
		}
		identityEntry := m
		if identityEntry.Source == "" && identityEntry.ID == "" {
			identityEntry = providers.RepoFallback(n)
		}
		if ignored.Contains(identityEntry.Identity()) {
			fmt.Printf("[%d/%d] %s\n        Ignored (%s)\n", i+1, len(jars), n, identityEntry.Identity())
			continue
		}
		f.Mods = append(f.Mods, m)
		counts[m.Source]++
		if m.Source == "" {
			unknown = append(unknown, n)
		}
		fmt.Printf("[%d/%d] %s\n        %s\n", i+1, len(jars), n, sourceName(m.Source))
		if m.Source == "" {
			if e = providers.Copy(path, filepath.Join(tmp, n)); e != nil {
				return e
			}
		}
	}
	fmt.Printf("\nModLock initialization complete.\n\nMods found: %d\n\nSources:\nModrinth: %d\nCurseForge: %d\nWithout source: %d\n", len(jars), counts["modrinth"], counts["curseforge"], len(unknown))
	printUnknown(unknown)
	if len(unknown) > 0 {
		fmt.Println("\nДля каждого такого мода можно вручную выбрать source и URL.")
		for i := range f.Mods {
			if f.Mods[i].Source != "" {
				continue
			}
			chosen, cancelled, chooseErr := chooseSource(reader, f.Mods[i].Filename, f.Mods[i])
			if chooseErr != nil {
				return chooseErr
			}
			if cancelled {
				chosen = providers.RepoFallback(f.Mods[i].Filename)
			}
			f.Mods[i] = chosen
		}
	}
	fmt.Println()
	if !push.Confirm(reader, "Create mod.lock? [Y/n] ", true) {
		fmt.Println("Initialization cancelled.")
		return nil
	}
	for _, m := range f.Mods {
		if m.Source == "repo" {
			dst, resolveErr := lockfile.ResolveWithin(root, m.Path)
			if resolveErr != nil {
				return resolveErr
			}
			if e = providers.Copy(filepath.Join(tmp, m.Filename), dst); e != nil {
				return e
			}
		}
	}
	if e = lockfile.Write(lockPath, f); e != nil {
		return e
	}
	if e = push.InitRepository(root, repo, branch); e != nil {
		return e
	}
	fmt.Println("Git repository initialized. Remote origin:", repo)
	return nil
}

func chooseSource(r *bufio.Reader, name string, detected lockfile.ModEntry) (lockfile.ModEntry, bool, error) {
	fmt.Printf("\n%s\n", name)
	if detected.Source != "" {
		fmt.Println("Найдено автоматически:", sourceName(detected.Source))
	} else {
		fmt.Println("Источник автоматически не найден.")
	}
	fmt.Println("Источник: [a] auto  [m] Modrinth  [c] CurseForge  [r] repository  [s] skip")
	def := "r"
	if detected.Source != "" {
		def = "a"
	}
	choice := strings.ToLower(ask(r, "Выбор ["+def+"]: ", def))
	switch choice {
	case "a", "auto":
		if detected.Source == "" {
			return providers.RepoFallback(name), false, nil
		}
		return detected, false, nil
	case "r", "repo", "repository", "":
		base := strings.TrimSuffix(name, filepath.Ext(name))
		defaultID, defaultVersion := "repo:"+base, "local"
		if detected.ID != "" {
			defaultID = detected.ID
		}
		if detected.Version != "" {
			defaultVersion = detected.Version
		}
		id := ask(r, "Stable mod ID ["+defaultID+"]: ", defaultID)
		version := ask(r, "Version ["+defaultVersion+"]: ", defaultVersion)
		return providers.RepoFallbackWithIdentity(name, id, version), false, nil
	case "m", "modrinth":
		u := ask(r, "Прямой URL файла: ", "")
		if u == "" {
			return lockfile.ModEntry{}, false, errors.New("URL обязателен")
		}
		projectID := ask(r, "Project ID: ", "")
		versionID := ask(r, "Version ID: ", "")
		defaultID := "modrinth:" + projectID
		if projectID == "" {
			defaultID = "mod:" + strings.TrimSuffix(name, filepath.Ext(name))
		}
		id := ask(r, "Stable mod ID ["+defaultID+"]: ", defaultID)
		version := ask(r, "Version ["+versionID+"]: ", versionID)
		return lockfile.ModEntry{ID: id, Version: version, Filename: name, Source: "modrinth", URL: u, ProjectID: projectID, VersionID: versionID}, false, nil
	case "c", "curseforge":
		u := ask(r, "Прямой URL файла: ", "")
		if u == "" {
			return lockfile.ModEntry{}, false, errors.New("URL обязателен")
		}
		modID, err := optionalInt64(ask(r, "Mod ID (необязательно): ", ""))
		if err != nil {
			return lockfile.ModEntry{}, false, fmt.Errorf("invalid Mod ID: %w", err)
		}
		fileID, err := optionalInt64(ask(r, "File ID (необязательно): ", ""))
		if err != nil {
			return lockfile.ModEntry{}, false, fmt.Errorf("invalid File ID: %w", err)
		}
		defaultID := fmt.Sprintf("curseforge:%d", modID)
		if modID == 0 {
			defaultID = "mod:" + strings.TrimSuffix(name, filepath.Ext(name))
		}
		id := ask(r, "Stable mod ID ["+defaultID+"]: ", defaultID)
		defaultVersion := ""
		if fileID != 0 {
			defaultVersion = fmt.Sprintf("%d", fileID)
		}
		version := ask(r, "Version ["+defaultVersion+"]: ", defaultVersion)
		return lockfile.ModEntry{ID: id, Version: version, Filename: name, Source: "curseforge", URL: u, ModID: modID, FileID: fileID}, false, nil
	case "s", "skip", "cancel":
		return lockfile.ModEntry{}, true, nil
	default:
		return lockfile.ModEntry{}, false, fmt.Errorf("неизвестный source %q", choice)
	}
}

func optionalInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}
func ask(r *bufio.Reader, p, def string) string {
	fmt.Print(p)
	s, _ := r.ReadString('\n')
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	return s
}
func sourceName(s string) string {
	switch s {
	case "modrinth":
		return "Modrinth"
	case "curseforge":
		return "CurseForge"
	default:
		return "Источник не найден"
	}
}

func printUnknown(names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Println("\nМоды без найденного источника:")
	for _, name := range names {
		fmt.Println("?", name)
	}
	if os.Getenv("CURSEFORGE_API_KEY") == "" {
		fmt.Println("\nCurseForge-поиск пропущен: переменная CURSEFORGE_API_KEY не задана.")
	}
}

func TUI(ctx context.Context) error {
	if _, e := FindRoot(true); e != nil {
		fmt.Println("ModLock\n\nModLock ещё не настроен.\n\n1. Создать lock из сборки\n2. Выход")
		fmt.Print("> ")
		var s string
		fmt.Scanln(&s)
		if s == "1" {
			return Init(ctx, os.Stdin)
		}
		return nil
	}
	for {
		fmt.Println("\nModLock\n\n1. Обновить сборку\n2. Показать изменения\n3. Выход")
		fmt.Print("> ")
		var s string
		fmt.Scanln(&s)
		switch s {
		case "1":
			if e := Sync(ctx); e != nil {
				fmt.Printf("\nНе удалось обновить сборку.\nПроверь подключение к интернету и попробуй ещё раз.\n\n%s\n", e)
			}
			wait()
		case "2":
			if e := Diff(); e != nil {
				fmt.Println("Ошибка:", e)
			}
			wait()
		case "3", "":
			return nil
		}
	}
}
func wait() { fmt.Print("\n[Enter] Назад"); bufio.NewReader(os.Stdin).ReadString('\n') }
