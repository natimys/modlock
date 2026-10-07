package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"modlock/internal/failure"
	"modlock/internal/lockfile"
	syncer "modlock/internal/sync"
)

type AuthorResourceChange struct {
	Kind      string   `json:"kind,omitempty"`
	Action    string   `json:"action,omitempty"`
	Identity  string   `json:"identity,omitempty"`
	Path      string   `json:"path,omitempty"`
	Target    string   `json:"target,omitempty"`
	TargetID  string   `json:"target_id,omitempty"`
	Targets   []string `json:"targets,omitempty"`
	Filename  string   `json:"filename,omitempty"`
	OldSHA256 string   `json:"old_sha256,omitempty"`
	SHA256    string   `json:"sha256,omitempty"`
}

type AuthorChangeSet struct {
	Added   []AuthorResourceChange `json:"added"`
	Updated []AuthorResourceChange `json:"updated"`
	Removed []AuthorResourceChange `json:"removed"`
}

type AuthorScanResult struct {
	State         *AuthorState           `json:"state"`
	PlannedLock   *lockfile.File         `json:"planned_lock"`
	PreviewID     string                 `json:"preview_id"`
	Mods          AuthorChangeSet        `json:"mods"`
	Files         AuthorChangeSet        `json:"files"`
	TargetChanges []AuthorResourceChange `json:"target_changes"`
	Ignored       []AuthorResourceChange `json:"ignored"`
	Branch        string                 `json:"branch"`
	CommitMessage string                 `json:"commit_message"`
	payloads      map[string][]byte
}

type trackedInput struct {
	path    string
	targets []string
	policy  string
}
type trackedContent struct {
	targetID, path, hash, policy string
	bytes                        []byte
}

func buildAuthorScan(root string, roots syncer.TargetRoots) (*AuthorScanResult, error) {
	state, err := readAuthorState(root, roots)
	if err != nil {
		return nil, err
	}
	if state.Lock.Schema != 3 {
		return nil, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("author scan requires schema 3"))
	}
	config := state.Settings
	inputs := make([]trackedInput, 0, len(config.TrackedPaths)+len(config.IncludeDirs))
	for _, p := range config.TrackedPaths {
		inputs = append(inputs, trackedInput{path: p.Path, targets: p.Targets, policy: p.Policy})
	}
	for _, p := range config.IncludeDirs {
		inputs = append(inputs, trackedInput{path: p, targets: []string{"client"}, policy: "replace"})
	}
	for _, input := range inputs {
		for _, id := range input.targets {
			if roots[id] == "" {
				return nil, failure.WrapDetails(failure.InvalidRequest, fmt.Errorf("launcher did not provide a root for tracked target %q", id), map[string]any{"target": id, "path": input.path})
			}
		}
	}
	collected := map[string]trackedContent{}
	ignored := []AuthorResourceChange{}
	for _, input := range inputs {
		for _, id := range input.targets {
			rootPath := roots[id]
			full, err := lockfile.ResolveWithin(rootPath, input.path)
			if err != nil {
				return nil, failure.Wrap(failure.InvalidRequest, err)
			}
			info, err := os.Lstat(full)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			var paths []string
			if info.Mode().IsRegular() {
				paths = []string{filepath.Clean(input.path)}
			} else if info.IsDir() {
				err = filepath.WalkDir(full, func(p string, d fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					rel, err := filepath.Rel(rootPath, p)
					if err != nil {
						return err
					}
					rel = filepath.ToSlash(rel)
					if isAlwaysExcluded(rel) {
						if d.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
					if _, err = lockfile.ResolveWithin(rootPath, rel); err != nil {
						return failure.Wrap(failure.Conflict, err)
					}
					if excludedBy(rel, config.ExcludePaths) {
						ignored = append(ignored, AuthorResourceChange{TargetID: id, Target: rel})
						if d.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
					if d.Type()&os.ModeSymlink != 0 {
						return failure.Wrap(failure.Conflict, fmt.Errorf("tracked path contains a symlink or junction: %s", rel))
					}
					if d.IsDir() {
						return nil
					}
					if !d.Type().IsRegular() {
						return nil
					}
					if _, err = lockfile.ResolveWithin(rootPath, rel); err != nil {
						return failure.Wrap(failure.Conflict, err)
					}
					paths = append(paths, filepath.FromSlash(rel))
					return nil
				})
				if err != nil {
					return nil, err
				}
			} else {
				return nil, failure.Wrap(failure.Conflict, fmt.Errorf("tracked path is not a regular file or directory: %s", input.path))
			}
			for _, localRel := range paths {
				rel := filepath.ToSlash(localRel)
				if isAlwaysExcluded(rel) || excludedBy(rel, config.ExcludePaths) {
					continue
				}
				path, err := lockfile.ResolveWithin(rootPath, rel)
				if err != nil {
					return nil, failure.Wrap(failure.Conflict, err)
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return nil, err
				}
				sum := sha256.Sum256(b)
				hash := hex.EncodeToString(sum[:])
				key := strings.ToLower(id + "\x00" + rel)
				collected[key] = trackedContent{targetID: id, path: rel, hash: hash, policy: input.policy, bytes: b}
			}
		}
	}

	// Coalesce byte-identical paths into one shared lock entry. If target copies
	// differ, keep distinct repository sources under files/<target-id>/.
	byPath := map[string][]trackedContent{}
	for _, c := range collected {
		byPath[strings.ToLower(c.path)] = append(byPath[strings.ToLower(c.path)], c)
	}
	newFiles := []lockfile.ManagedFile{}
	payloads := map[string][]byte{}
	for _, group := range byPath {
		sort.Slice(group, func(i, j int) bool { return group[i].targetID < group[j].targetID })
		byHash := map[string][]trackedContent{}
		for _, c := range group {
			byHash[c.hash] = append(byHash[c.hash], c)
		}
		for hash, identical := range byHash {
			sort.Slice(identical, func(i, j int) bool { return identical[i].targetID < identical[j].targetID })
			source := filepath.ToSlash(filepath.Join("files", identical[0].path))
			if len(identical) == 1 {
				source = filepath.ToSlash(filepath.Join("files", identical[0].targetID, identical[0].path))
			}
			ids := make([]string, 0, len(identical))
			for _, c := range identical {
				ids = append(ids, c.targetID)
			}
			policy := identical[0].policy
			newFiles = append(newFiles, lockfile.ManagedFile{Path: source, Target: identical[0].path, SHA256: hash, Policy: policy, Targets: ids})
			payloads[source] = identical[0].bytes
		}
	}

	oldFiles := map[string]lockfile.ManagedFile{}
	for _, f := range state.Lock.Files {
		for _, id := range f.Targets {
			oldFiles[authorResourceKey(id, f.Target)] = f
		}
	}
	trackedScopes := func(targetID, target string) bool {
		for _, input := range inputs {
			for _, id := range input.targets {
				if id != targetID {
					continue
				}
				base := filepath.ToSlash(filepath.Clean(filepath.FromSlash(input.path)))
				target = filepath.ToSlash(filepath.Clean(filepath.FromSlash(target)))
				if base == "." || target == base || strings.HasPrefix(target, strings.TrimSuffix(base, "/")+"/") {
					return true
				}
			}
		}
		return false
	}
	newByTarget := map[string]lockfile.ManagedFile{}
	for _, f := range newFiles {
		for _, id := range f.Targets {
			newByTarget[authorResourceKey(id, f.Target)] = f
		}
	}
	planned := *state.Lock
	planned.Mods = append([]lockfile.ModEntry(nil), state.Lock.Mods...)
	planned.Files = nil
	for _, original := range state.Lock.Files { // retain memberships outside configured scan scopes
		retained := make([]string, 0, len(original.Targets))
		for _, id := range original.Targets {
			if !trackedScopes(id, original.Target) {
				retained = append(retained, id)
			}
		}
		if len(retained) > 0 {
			copy := original
			copy.Targets = retained
			planned.Files = append(planned.Files, copy)
		}
	}
	for _, f := range newFiles {
		planned.Files = append(planned.Files, f)
	}
	if err := planned.Validate(); err != nil {
		return nil, failure.Wrap(failure.InvalidRequest, err)
	}
	changeSet := AuthorChangeSet{Added: []AuthorResourceChange{}, Updated: []AuthorResourceChange{}, Removed: []AuthorResourceChange{}}
	for key, f := range newByTarget {
		old, ok := oldFiles[key]
		item := AuthorResourceChange{Path: f.Path, Target: f.Target, Targets: f.Targets}
		if !ok {
			changeSet.Added = append(changeSet.Added, item)
		} else if old.SHA256 != f.SHA256 || old.Policy != f.Policy || old.Path != f.Path {
			item.OldSHA256 = old.SHA256
			changeSet.Updated = append(changeSet.Updated, item)
		}
	}
	for key, f := range oldFiles {
		id := strings.SplitN(key, "\x00", 2)[0]
		if !trackedScopes(id, f.Target) {
			continue
		}
		if _, ok := newByTarget[key]; !ok {
			changeSet.Removed = append(changeSet.Removed, AuthorResourceChange{Path: f.Path, Target: f.Target, TargetID: id, OldSHA256: f.SHA256})
		}
	}
	sortChanges := func(items []AuthorResourceChange) {
		sort.Slice(items, func(i, j int) bool {
			return items[i].TargetID+items[i].Target+items[i].Path < items[j].TargetID+items[j].Target+items[j].Path
		})
	}
	sortChanges(changeSet.Added)
	sortChanges(changeSet.Updated)
	sortChanges(changeSet.Removed)
	mods := AuthorChangeSet{Added: []AuthorResourceChange{}, Updated: []AuthorResourceChange{}, Removed: []AuthorResourceChange{}}
	for _, m := range state.Mods {
		switch m.Status {
		case "unmanaged":
			mods.Added = append(mods.Added, AuthorResourceChange{Identity: m.Identity, Filename: m.Filename, Targets: m.Targets, SHA256: m.SHA256})
		case "modified":
			mods.Updated = append(mods.Updated, AuthorResourceChange{Identity: m.Identity, Filename: m.Filename, Targets: m.Targets, OldSHA256: m.SHA256})
		case "missing":
			mods.Removed = append(mods.Removed, AuthorResourceChange{Identity: m.Identity, Filename: m.Filename, Targets: m.Targets, OldSHA256: m.SHA256})
		}
	}
	targetChanges := targetMembershipChanges(state.Lock, &planned)
	result := &AuthorScanResult{State: state, PlannedLock: &planned, Mods: mods, Files: changeSet, TargetChanges: targetChanges, Ignored: ignored, Branch: state.Lock.Pack.Branch, CommitMessage: "modlock: update pack", payloads: payloads}
	encoded, _ := json.Marshal(struct {
		Lock     *lockfile.File
		Files    []lockfile.ManagedFile
		Payloads map[string]string
		Changes  any
	}{&planned, planned.Files, hashesOf(payloads), struct{ M, F AuthorChangeSet }{mods, changeSet}})
	sum := sha256.Sum256(encoded)
	result.PreviewID = hex.EncodeToString(sum[:])
	return result, nil
}

func targetMembershipChanges(old, next *lockfile.File) []AuthorResourceChange {
	changes := []AuthorResourceChange{}
	type membership struct {
		kind, identity, path, filename string
		targets                        []string
	}
	appendChanges := func(before, after membership) {
		oldSet, newSet := map[string]bool{}, map[string]bool{}
		for _, id := range before.targets {
			oldSet[id] = true
		}
		for _, id := range after.targets {
			newSet[id] = true
		}
		for id := range newSet {
			if !oldSet[id] {
				changes = append(changes, AuthorResourceChange{Kind: after.kind, Action: "added", Identity: after.identity, Path: after.path, Filename: after.filename, TargetID: id, Targets: []string{id}})
			}
		}
		for id := range oldSet {
			if !newSet[id] {
				changes = append(changes, AuthorResourceChange{Kind: before.kind, Action: "removed", Identity: before.identity, Path: before.path, Filename: before.filename, TargetID: id, Targets: []string{id}})
			}
		}
	}
	oldMods, nextMods := map[string]membership{}, map[string]membership{}
	for _, m := range old.Mods {
		oldMods[m.Identity()] = membership{kind: "mod", identity: m.Identity(), filename: m.Filename, targets: m.Targets}
	}
	for _, m := range next.Mods {
		nextMods[m.Identity()] = membership{kind: "mod", identity: m.Identity(), filename: m.Filename, targets: m.Targets}
	}
	for id, previous := range oldMods {
		appendChanges(previous, nextMods[id])
	}
	for id, current := range nextMods {
		if _, ok := oldMods[id]; !ok {
			appendChanges(membership{kind: "mod", identity: current.identity, filename: current.filename}, current)
		}
	}
	oldFiles, nextFiles := map[string]membership{}, map[string]membership{}
	putFile := func(into map[string]membership, f lockfile.ManagedFile) {
		key := strings.ToLower(f.Target)
		m := into[key]
		m.kind, m.identity, m.path = "file", f.Path, f.Target
		m.targets = append(m.targets, f.Targets...)
		into[key] = m
	}
	for _, f := range old.Files {
		putFile(oldFiles, f)
	}
	for _, f := range next.Files {
		putFile(nextFiles, f)
	}
	for key, previous := range oldFiles {
		appendChanges(previous, nextFiles[key])
	}
	for key, current := range nextFiles {
		if _, ok := oldFiles[key]; !ok {
			appendChanges(membership{kind: "file", identity: current.identity, path: current.path}, current)
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		return a.Kind+a.Identity+a.Path+a.TargetID+a.Action < b.Kind+b.Identity+b.Path+b.TargetID+b.Action
	})
	return changes
}

func hashesOf(payloads map[string][]byte) map[string]string {
	out := map[string]string{}
	for k, b := range payloads {
		h := sha256.Sum256(b)
		out[k] = hex.EncodeToString(h[:])
	}
	return out
}
func isAlwaysExcluded(rel string) bool {
	for _, part := range strings.Split(strings.ToLower(filepath.ToSlash(rel)), "/") {
		if part == ".modlock" || part == ".git" || part == "mod.lock" || part == "modlock.lock" || strings.HasPrefix(part, "mod.lock.") || strings.HasPrefix(part, "modlock.lock.") {
			return true
		}
	}
	return false
}
func excludedBy(rel string, patterns []string) bool {
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	for _, p := range patterns {
		p = filepath.ToSlash(p)
		if strings.HasSuffix(p, "/**") {
			base := strings.TrimSuffix(p, "/**")
			if rel == base || strings.HasPrefix(rel, base+"/") {
				return true
			}
		}
		if match, _ := filepath.Match(p, rel); match {
			return true
		}
	}
	return false
}
