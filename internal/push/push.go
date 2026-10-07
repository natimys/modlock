package push

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	gitlib "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"

	"modlock/internal/diff"
	ignorefile "modlock/internal/ignore"
	"modlock/internal/lockfile"
	"modlock/internal/providers"
)

type Commit struct{ Hash, Short, Subject string }

func RecentCommits(root string, limit int) ([]Commit, error) {
	if limit < 1 {
		limit = 15
	}
	cmd := exec.Command("git", "log", fmt.Sprintf("-n%d", limit), "--format=%H%x09%h%x09%s")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	var out []Commit
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 3)
		if len(parts) == 3 {
			out = append(out, Commit{Hash: parts[0], Short: parts[1], Subject: parts[2]})
		}
	}
	return out, nil
}

func ResolveCommit(root, ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", ref+"^{commit}")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("commit %q not found", ref)
	}
	return strings.TrimSpace(string(b)), nil
}

func RevertSummary(root, target string) (string, int, error) {
	countCmd := exec.Command("git", "rev-list", "--count", target+"..HEAD")
	countCmd.Dir = root
	countBytes, err := countCmd.Output()
	if err != nil {
		return "", 0, err
	}
	count, _ := strconv.Atoi(strings.TrimSpace(string(countBytes)))
	statCmd := exec.Command("git", "diff", "--stat", target, "HEAD")
	statCmd.Dir = root
	stat, err := statCmd.Output()
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(string(stat)), count, nil
}

func RevertTo(root, target, branch string) error {
	dirty := exec.Command("git", "status", "--porcelain", "--untracked-files=no")
	dirty.Dir = root
	b, err := dirty.Output()
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if len(bytes.TrimSpace(b)) > 0 {
		return fmt.Errorf("tracked files have uncommitted changes; commit or discard them before revert")
	}
	ancestor := exec.Command("git", "merge-base", "--is-ancestor", target, "HEAD")
	ancestor.Dir = root
	if err = ancestor.Run(); err != nil {
		return fmt.Errorf("selected commit is not an ancestor of HEAD")
	}
	diffCmd := exec.Command("git", "diff", "--name-only", "-z", target, "HEAD")
	diffCmd.Dir = root
	raw, err := diffCmd.Output()
	if err != nil {
		return err
	}
	raw = bytes.TrimSuffix(raw, []byte{0})
	if len(raw) == 0 {
		return fmt.Errorf("already at selected commit")
	}
	for _, item := range bytes.Split(raw, []byte{0}) {
		path := string(item)
		exists := exec.Command("git", "cat-file", "-e", target+":"+filepath.ToSlash(path))
		exists.Dir = root
		if exists.Run() == nil {
			if err = git(root, "restore", "--source="+target, "--staged", "--worktree", "--", path); err != nil {
				return err
			}
		} else {
			if err = git(root, "rm", "-f", "--ignore-unmatch", "--", path); err != nil {
				return err
			}
		}
	}
	short := target
	if len(short) > 12 {
		short = short[:12]
	}
	if err = git(root, "commit", "-m", "modlock: revert to "+short); err != nil {
		return err
	}
	return git(root, "push", "-u", "origin", branch)
}

type Summary struct {
	Added, Updated, Removed          int
	Modrinth, CurseForge, Repository int
}

func Prepare(ctx context.Context, root string, old *lockfile.File, ignored *ignorefile.File, progress func(int, int, string, string)) (*lockfile.File, map[string]string, []string, Summary, error) {
	return prepare(ctx, root, old, ignored, providers.New(), progress)
}

func prepare(ctx context.Context, root string, old *lockfile.File, ignored *ignorefile.File, det *providers.Detector, progress func(int, int, string, string)) (*lockfile.File, map[string]string, []string, Summary, error) {
	modsDir, resolveErr := lockfile.ResolveWithin(root, old.Pack.ModsDir)
	if resolveErr != nil {
		return nil, nil, nil, Summary{}, resolveErr
	}
	d, e := diff.Local(modsDir, old)
	if e != nil {
		return nil, nil, nil, Summary{}, e
	}
	next := *old
	next.Mods = append([]lockfile.ModEntry{}, old.Mods...)
	removed := map[string]bool{}
	for _, n := range d.Removed {
		removed[n] = true
	}
	ignoredTracked := 0
	for _, m := range old.Mods {
		if ignored.Contains(m.Identity()) && !removed[m.Filename] {
			removed[m.Filename] = true
			ignoredTracked++
		}
	}
	kept := next.Mods[:0]
	var deleteRepo []string
	for _, m := range next.Mods {
		if removed[m.Filename] {
			if m.Source == "repo" {
				deleteRepo = append(deleteRepo, m.Path)
			}
		} else {
			kept = append(kept, m)
		}
	}
	next.Mods = kept
	oldByIdentity := map[string]lockfile.ModEntry{}
	for _, m := range old.Mods {
		oldByIdentity[m.Identity()] = m
	}
	copies := map[string]string{}
	s := Summary{Added: len(d.Added), Removed: len(d.Removed) + ignoredTracked}
	for i, n := range d.Added {
		m, e := det.Detect(ctx, filepath.Join(modsDir, n))
		if e != nil {
			return nil, nil, nil, s, e
		}
		if m.Source == "" {
			if m.ID != "" {
				m = providers.RepoFallbackWithIdentity(n, m.ID, m.Version)
			} else {
				m = providers.RepoFallback(n)
			}
		}
		if ignored.Contains(m.Identity()) {
			s.Added--
			progress(i+1, len(d.Added), n, "ignored")
			continue
		}
		m.SHA256, e = hashFile(filepath.Join(modsDir, n))
		if e != nil {
			return nil, nil, nil, s, fmt.Errorf("hash %s: %w", n, e)
		}
		if previous, ok := oldByIdentity[m.Identity()]; ok && removed[previous.Filename] {
			s.Updated++
			s.Added--
			s.Removed--
		}
		next.Mods = append(next.Mods, m)
		switch m.Source {
		case "modrinth":
			s.Modrinth++
		case "curseforge":
			s.CurseForge++
		case "repo":
			s.Repository++
			copies[filepath.Join(modsDir, n)] = m.Path
		}
		progress(i+1, len(d.Added), n, m.Source)
	}
	used := map[string]bool{}
	for _, m := range next.Mods {
		if m.Source == "repo" {
			used[filepath.ToSlash(m.Path)] = true
		}
	}
	filtered := deleteRepo[:0]
	for _, p := range deleteRepo {
		if !used[filepath.ToSlash(p)] {
			filtered = append(filtered, p)
		}
	}
	return &next, copies, filtered, s, nil
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

func Apply(root string, next *lockfile.File, copies map[string]string, deletes []string, message string) error {
	if err := EnsureOrigin(root, next.Pack.Repository); err != nil {
		return err
	}
	lockPath := filepath.Join(root, lockfile.DefaultFilename)
	if err := lockfile.Write(lockPath, next); err != nil {
		return err
	}
	var paths = []string{lockPath}
	ignorePath := filepath.Join(root, ignorefile.Filename)
	if _, err := os.Stat(ignorePath); err == nil {
		paths = append(paths, ignorePath)
	}
	for src, rel := range copies {
		dst, e := lockfile.ResolveWithin(root, rel)
		if e != nil {
			return e
		}
		if e = providers.Copy(src, dst); e != nil {
			return e
		}
		paths = append(paths, dst)
	}
	for _, m := range next.Mods {
		if m.Source != "repo" {
			continue
		}
		p, e := lockfile.ResolveWithin(root, m.Path)
		if e != nil {
			return e
		}
		paths = append(paths, p)
	}
	for _, rel := range deletes {
		p, e := lockfile.ResolveWithin(root, rel)
		if e != nil {
			return e
		}
		if e = os.Remove(p); e != nil && !os.IsNotExist(e) {
			return e
		}
		paths = append(paths, p)
	}
	for _, p := range paths {
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		if e = git(root, "add", "--", rel); e != nil {
			return e
		}
	}
	managedDir := filepath.Join("files", "mods")
	_, statErr := os.Stat(filepath.Join(root, managedDir))
	trackedCmd := exec.Command("git", "ls-files", "--", managedDir)
	trackedCmd.Dir = root
	trackedOut, _ := trackedCmd.Output()
	if statErr == nil || len(strings.TrimSpace(string(trackedOut))) > 0 {
		if e := git(root, "add", "-u", "--", managedDir); e != nil {
			return e
		}
	}
	if message == "" {
		message = "modlock: update pack"
	}
	if e := git(root, "commit", "-m", message); e != nil {
		return e
	}
	return git(root, "push", "-u", "origin", next.Pack.Branch)
}

func HasManagedChanges(root string) bool {
	cmd := exec.Command("git", "status", "--porcelain", "--", lockfile.DefaultFilename, ignorefile.Filename, filepath.Join("files", "mods"))
	cmd.Dir = root
	b, err := cmd.Output()
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}

func HasUnpushedCommits(root string) bool {
	cmd := exec.Command("git", "rev-list", "--count", "@{upstream}..HEAD")
	cmd.Dir = root
	b, err := cmd.Output()
	if err == nil {
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n > 0
	}
	head := exec.Command("git", "rev-parse", "--verify", "HEAD")
	head.Dir = root
	return head.Run() == nil
}

func PushBranch(root, branch string) error { return git(root, "push", "-u", "origin", branch) }

// EnsureOrigin makes the repository usable for push before we create a
// commit. A missing origin used to be discovered only after commit, leaving
// the author with a local commit that friends could never download.
//
// An existing, different origin is not silently replaced: that is usually a
// configuration mistake, and overwriting it could push a modpack to the
// wrong repository.
func EnsureOrigin(root, repository string) error {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return fmt.Errorf("pack.repository is empty; cannot configure Git origin")
	}
	r, err := gitlib.PlainOpen(root)
	if err != nil {
		return fmt.Errorf("open Git repository: %w", err)
	}
	remote, err := r.Remote("origin")
	if err == gitlib.ErrRemoteNotFound {
		if _, err = r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{repository}}); err != nil {
			return fmt.Errorf("create Git origin: %w", err)
		}
		fmt.Println("Git remote origin was missing; configured it from pack.repository:", repository)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Git origin: %w", err)
	}
	for _, url := range remote.Config().URLs {
		if url == repository {
			return nil
		}
	}
	return fmt.Errorf("Git remote origin points to %s, but pack.repository is %s; fix origin before pushing", strings.Join(remote.Config().URLs, ", "), repository)
}

func GitRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	b, e := cmd.Output()
	if e != nil {
		return "", fmt.Errorf("current directory is not a Git repository")
	}
	return strings.TrimSpace(string(b)), nil
}

func InitRepository(root, repository, branch string) error {
	r, err := gitlib.PlainOpen(root)
	if err == gitlib.ErrRepositoryNotExists {
		r, err = gitlib.PlainInit(root, false)
		if err != nil {
			return fmt.Errorf("git init: %w", err)
		}
		if branch == "" {
			branch = "main"
		}
		if err = r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch))); err != nil {
			return fmt.Errorf("set default branch: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("open Git repository: %w", err)
	}
	if repository == "" {
		return nil
	}
	remote, remoteErr := r.Remote("origin")
	if remoteErr == gitlib.ErrRemoteNotFound {
		_, err = r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{repository}})
		if err != nil {
			return fmt.Errorf("create Git origin: %w", err)
		}
		return nil
	}
	if remoteErr != nil {
		return remoteErr
	}
	for _, u := range remote.Config().URLs {
		if u == repository {
			return nil
		}
	}
	return fmt.Errorf("Git remote origin already points to %s", strings.Join(remote.Config().URLs, ", "))
}
func git(root string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = os.Environ()
	if len(args) > 0 && args[0] == "commit" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=ModLock", "GIT_AUTHOR_EMAIL=modlock@local", "GIT_COMMITTER_NAME=ModLock", "GIT_COMMITTER_EMAIL=modlock@local")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e := cmd.Run(); e != nil {
		return fmt.Errorf("git %s: %w", args[0], e)
	}
	return nil
}
func Confirm(r *bufio.Reader, prompt string, yesDefault bool) bool {
	fmt.Print(prompt)
	s, _ := r.ReadString('\n')
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return yesDefault
	}
	return s == "y" || s == "yes" || s == "д" || s == "да"
}
